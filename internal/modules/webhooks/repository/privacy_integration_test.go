//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// TestPrivacyStoreScopesAttributionAndDeletesDeliveriesFirst proves the SQL
// used by export/erasure lists only events attributed to the requested user,
// and that erasure removes those deliveries before their RESTRICT-referenced
// events while leaving another user's history untouched.
func TestPrivacyStoreScopesAttributionAndDeletesDeliveriesFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass
	ctx = tenant.WithID(ctx, tenant.DefaultID)

	codec, err := webhookservice.NewSecretCodec("privacy-integration-encryption-key")
	require.NoError(t, err)
	queue := &recordingQueue{}
	repo := webhookrepo.New(fx.Scoped(), fx.Tx(), queue)
	privacy := webhookrepo.NewPrivacyStore(fx.Scoped(), fx.Tx())
	endpointSecret := webhook.SecretPrefix + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

	endpoint, err := repo.CreateEndpoint(ctx, webhookservice.CreateEndpointDraft{
		URL:         "https://receiver.example/privacy",
		EventTypes:  []string{"file.*"},
		Status:      webhookservice.StatusActive,
		MaxAttempts: 3,
		TimeoutMS:   1000,
		SealSecret: func(endpointID uuid.UUID) (string, error) {
			return codec.Seal(webhookservice.EndpointSecretBinding(tenant.DefaultID, endpointID), []byte(endpointSecret))
		},
	})
	require.NoError(t, err)
	secretID, err := repo.ActiveSecretID(ctx, endpoint.ID)
	require.NoError(t, err)

	userID := testID(7001)
	otherID := testID(7002)
	now := time.Now().UTC().Add(-time.Minute)
	createDelivery := func(eventID uuid.UUID, subjects []uuid.UUID, payload string) uuid.UUID {
		t.Helper()
		id, created, err := repo.CreateDeliveryWithJob(ctx, webhookservice.DeliveryDraft{
			TenantID:       tenant.DefaultID,
			EndpointID:     endpoint.ID,
			EventID:        eventID,
			SecretID:       secretID,
			EventType:      "file.uploaded",
			DataVersion:    1,
			ActorType:      "system",
			EventCreatedAt: now,
			URL:            endpoint.URL,
			Payload:        json.RawMessage(payload),
			MaxAttempts:    3,
			TimeoutMS:      1000,
			RunAt:          now,
			SubjectUserIDs: subjects,
		})
		require.NoError(t, err)
		require.True(t, created)
		return id
	}

	matchedEventID := testID(7101)
	actorEventID := testID(7103)
	userPayloadEventID := testID(7104)
	ownerPayloadEventID := testID(7105)
	otherEventID := testID(7102)
	matchedDeliveryID := createDelivery(matchedEventID, []uuid.UUID{userID}, `{"fileId":1}`)
	actorDeliveryID := createDelivery(actorEventID, []uuid.UUID{otherID}, `{"fileId":3}`)
	userPayloadDeliveryID := createDelivery(userPayloadEventID, []uuid.UUID{otherID}, `{"userId":"`+userID.String()+`"}`)
	ownerPayloadDeliveryID := createDelivery(ownerPayloadEventID, []uuid.UUID{otherID}, `{"ownerId":"`+userID.String()+`"}`)
	otherDeliveryID := createDelivery(otherEventID, []uuid.UUID{otherID}, `{"fileId":2}`)

	// The actor branch is not covered by the producer helper: make this event
	// attributable through actor_id while its subject remains another user.
	_, err = pool.Exec(ctx, `UPDATE webhook_events
		SET actor_type = 'user', actor_id = $1
		WHERE tenant_id = $2 AND event_id = $3`, userID, tenant.DefaultID, actorEventID)
	require.NoError(t, err)

	events, err := privacy.ListEvents(ctx, tenant.DefaultID, userID, pagination.DBCursor{Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 4)
	eventIDs := make(map[uuid.UUID]bool, len(events))
	for _, event := range events {
		eventIDs[event.ID] = true
	}
	for _, id := range []uuid.UUID{matchedEventID, actorEventID, userPayloadEventID, ownerPayloadEventID} {
		assert.True(t, eventIDs[id], "event %s must match an attribution branch", id)
	}
	assert.False(t, eventIDs[otherEventID], "an unrelated event must not be exported")

	firstPage, err := privacy.ListDeliveries(ctx, tenant.DefaultID, userID, pagination.DBCursor{Limit: 2})
	require.NoError(t, err)
	require.Len(t, firstPage, 2)
	secondPage, err := privacy.ListDeliveries(ctx, tenant.DefaultID, userID, pagination.DBCursor{
		HasCursor: true,
		CreatedAt: firstPage[len(firstPage)-1].CreatedAt,
		ID:        firstPage[len(firstPage)-1].ID,
		Limit:     2,
	})
	require.NoError(t, err)
	require.Len(t, secondPage, 2)
	deliveryIDs := make(map[uuid.UUID]bool, 4)
	for _, delivery := range append(firstPage, secondPage...) {
		deliveryIDs[delivery.ID] = true
	}
	for _, id := range []uuid.UUID{matchedDeliveryID, actorDeliveryID, userPayloadDeliveryID, ownerPayloadDeliveryID} {
		assert.True(t, deliveryIDs[id], "delivery %s must match an attribution branch", id)
	}
	assert.False(t, deliveryIDs[otherDeliveryID], "an unrelated delivery must not be exported")

	require.NoError(t, privacy.DeleteEvents(ctx, tenant.DefaultID, userID))

	var remainingDeliveries int
	for _, id := range []uuid.UUID{matchedDeliveryID, actorDeliveryID, userPayloadDeliveryID, ownerPayloadDeliveryID} {
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM webhook_deliveries WHERE id = $1`, id,
		).Scan(&remainingDeliveries))
		assert.Zero(t, remainingDeliveries, "matched delivery %s must be erased", id)
	}
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE id = $1`, otherDeliveryID,
	).Scan(&remainingDeliveries))
	assert.Equal(t, 1, remainingDeliveries)

	var remainingEvents int
	for _, id := range []uuid.UUID{matchedEventID, actorEventID, userPayloadEventID, ownerPayloadEventID} {
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM webhook_events WHERE tenant_id = $1 AND event_id = $2`, tenant.DefaultID, id,
		).Scan(&remainingEvents))
		assert.Zero(t, remainingEvents, "matched event %s must be erased", id)
	}
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE tenant_id = $1 AND event_id = $2`, tenant.DefaultID, otherEventID,
	).Scan(&remainingEvents))
	assert.Equal(t, 1, remainingEvents)
}
