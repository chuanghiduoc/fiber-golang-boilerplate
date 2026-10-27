//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

type recordingQueue struct {
	enqueued []jobs.Job
}

func (q *recordingQueue) EnqueueTx(_ context.Context, _ pgx.Tx, job jobs.Job) (uuid.UUID, error) {
	q.enqueued = append(q.enqueued, job)
	return testID(1000 + len(q.enqueued)), nil
}

// TestRepositoryTracksVersionedSecrets proves a delivery snapshots the active
// endpoint secret by id and the worker can read that exact row, and that both
// endpoint and inbound-source rotations keep the immediately previous secret
// readable while it is inside the grace window.
func TestRepositoryTracksVersionedSecrets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass

	ctx = tenant.WithID(ctx, tenant.DefaultID)
	codec, err := webhookservice.NewSecretCodec("repository-versioned-secrets-test-key")
	require.NoError(t, err)
	queue := &recordingQueue{}
	repo := webhookrepo.New(fx.Scoped(), fx.Tx(), queue)

	endpointSecret := webhook.SecretPrefix + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	endpoint, err := repo.CreateEndpoint(ctx, webhookservice.CreateEndpointDraft{
		URL:         "https://receiver.example/hook",
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
	require.NotEqual(t, uuid.Nil, secretID)
	ciphertext, err := repo.SnapshotSecret(ctx, secretID)
	require.NoError(t, err)
	opened, err := codec.Open(webhookservice.EndpointSecretBinding(tenant.DefaultID, endpoint.ID), ciphertext)
	require.NoError(t, err)
	assert.Equal(t, endpointSecret, string(opened))

	eventID := testID(500)
	runAt := time.Now().UTC()
	deliveryID, created, err := repo.CreateDeliveryWithJob(ctx, webhookservice.DeliveryDraft{
		TenantID:       tenant.DefaultID,
		EndpointID:     endpoint.ID,
		EventID:        eventID,
		SecretID:       secretID,
		EventType:      "file.uploaded",
		DataVersion:    1,
		ActorType:      "user",
		ActorID:        testID(5).String(),
		EventCreatedAt: runAt,
		URL:            endpoint.URL,
		Payload:        json.RawMessage(`{"fileId":7}`),
		MaxAttempts:    3,
		TimeoutMS:      1000,
		RunAt:          runAt,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, uuid.Nil, deliveryID)
	require.Len(t, queue.enqueued, 1)
	assert.Equal(t, tenant.DefaultID, queue.enqueued[0].TenantID)
	assert.JSONEq(t, `{"deliveryId":"`+deliveryID.String()+`"}`, string(queue.enqueued[0].Payload))

	delivery, err := repo.GetDelivery(ctx, deliveryID)
	require.NoError(t, err)
	assert.Equal(t, secretID, delivery.SecretID)
	assert.Equal(t, eventID, delivery.EventID)
	assert.Equal(t, testID(5).String(), delivery.ActorID)
	assert.JSONEq(t, `{"fileId":7}`, string(delivery.Payload))

	// Rotation retires the first row with a grace window and inserts v2.
	newCiphertext, err := codec.Seal(webhookservice.EndpointSecretBinding(tenant.DefaultID, endpoint.ID), []byte(endpointSecret))
	require.NoError(t, err)
	_, err = repo.RotateEndpointSecret(ctx, endpoint.ID, newCiphertext, false)
	require.NoError(t, err)
	signing, err := repo.SigningSecrets(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, newCiphertext, signing.CurrentCiphertext)
	assert.Equal(t, ciphertext, signing.PreviousCiphertext)

	sourceSecret := webhook.SecretPrefix + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	source, err := repo.CreateInboundSource(ctx, webhookservice.InboundSourceDraft{
		Name: "Stripe",
		Slug: "stripe",
		SealSecret: func(sourceID uuid.UUID) (string, error) {
			return codec.Seal(webhookservice.InboundSourceSecretBinding(tenant.DefaultID, sourceID), []byte(sourceSecret))
		},
	})
	require.NoError(t, err)
	require.Equal(t, webhookservice.StatusActive, source.Status, "an omitted create status defaults to active")

	disabled, err := repo.CreateInboundSource(ctx, webhookservice.InboundSourceDraft{
		Name:   "GitHub",
		Slug:   "github",
		Status: webhookservice.StatusDisabled,
		SealSecret: func(sourceID uuid.UUID) (string, error) {
			return codec.Seal(webhookservice.InboundSourceSecretBinding(tenant.DefaultID, sourceID), []byte(sourceSecret))
		},
	})
	require.NoError(t, err)
	assert.Equal(t, webhookservice.StatusDisabled, disabled.Status, "create honours the requested status")

	inboundSecrets, err := repo.InboundSigningSecrets(ctx, source.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, inboundSecrets.CurrentCiphertext)
	assert.Empty(t, inboundSecrets.PreviousCiphertext)

	firstInboundCiphertext := inboundSecrets.CurrentCiphertext
	secondInboundCiphertext, err := codec.Seal(webhookservice.InboundSourceSecretBinding(tenant.DefaultID, source.ID), []byte(sourceSecret))
	require.NoError(t, err)
	_, err = repo.RotateInboundSourceSecret(ctx, source.ID, secondInboundCiphertext, false)
	require.NoError(t, err)
	inboundSecrets, err = repo.InboundSigningSecrets(ctx, source.ID)
	require.NoError(t, err)
	assert.Equal(t, secondInboundCiphertext, inboundSecrets.CurrentCiphertext)
	assert.Equal(t, firstInboundCiphertext, inboundSecrets.PreviousCiphertext)

	// expirePrevious closes the grace window immediately: only the newest
	// secret verifies after the second rotation commits.
	thirdInboundCiphertext, err := codec.Seal(webhookservice.InboundSourceSecretBinding(tenant.DefaultID, source.ID), []byte(sourceSecret))
	require.NoError(t, err)
	_, err = repo.RotateInboundSourceSecret(ctx, source.ID, thirdInboundCiphertext, true)
	require.NoError(t, err)
	inboundSecrets, err = repo.InboundSigningSecrets(ctx, source.ID)
	require.NoError(t, err)
	assert.Equal(t, thirdInboundCiphertext, inboundSecrets.CurrentCiphertext)
	assert.Empty(t, inboundSecrets.PreviousCiphertext)

	var versions int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_inbound_source_secrets WHERE source_id = $1`, source.ID).Scan(&versions))
	assert.Equal(t, 3, versions, "each rotation must add a secret version instead of overwriting the source")
}
