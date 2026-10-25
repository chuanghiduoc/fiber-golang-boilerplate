//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	notificationservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

func integrationTenant(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	return testutil.CreateTenant(t, context.Background(), pool, slug)
}

func integrationUser(t *testing.T, pool *pgxpool.Pool, tenantID int64, address string) int64 {
	t.Helper()
	return testutil.CreateMember(t, context.Background(), pool, tenantID, address, testutil.MemberOptions{}).UserID
}

func newIntegrationService(t *testing.T, pool *pgxpool.Pool, recipients identitycontracts.NotificationRecipientLookup) notificationservice.NotificationService {
	t.Helper()
	sender := jobs.NewQueuedSender(jobs.NewQueue(testutil.Scoped(pool)))
	return notificationservice.NewNotificationService(
		repository.NewNotificationRepository(testutil.Scoped(pool)),
		recipients,
		sender,
		repository.NewTransactor(testutil.NewTxManager(pool), sender),
	)
}

// failingSender returns an error while the notification transaction is open.
type failingSender struct{}

func (failingSender) Send(context.Context, email.Message) error {
	return errors.New("queue unavailable")
}

type staticRecipients map[int64]identitycontracts.NotificationRecipient

func (s staticRecipients) NotificationRecipient(_ context.Context, userID int64) (identitycontracts.NotificationRecipient, error) {
	recipient, ok := s[userID]
	if !ok {
		return identitycontracts.NotificationRecipient{}, apperror.ErrNotFound
	}
	return recipient, nil
}

func TestCreateIntegrationPersistsRowAndEmailJob(t *testing.T) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	tenantTwo := integrationTenant(t, pool, "notifications-service-two")
	userOne := integrationUser(t, pool, tenant.DefaultID, "notifications-service-one@example.com")
	userTwo := integrationUser(t, pool, tenantTwo, "notifications-service-two@example.com")
	recipients := staticRecipients{
		userOne: {UserID: userOne, Email: "notifications-service-one@example.com", Name: "One"},
		userTwo: {UserID: userTwo, Email: "notifications-service-two@example.com", Name: "Two"},
	}
	svc := newIntegrationService(t, pool, recipients)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	result, err := svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: userOne,
		Type:   "security.alert",
		Title:  "Sign-in from a new device",
		Body:   "A new device signed in to your account.",
		Data:   json.RawMessage(`{"device":"laptop"}`),
	})
	require.NoError(t, err)
	require.NotNil(t, result.Notification)
	assert.True(t, result.EmailQueued)

	var storedTenant, storedUser int64
	var storedTitle, storedType string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id, user_id, type, title FROM notifications WHERE id = $1`, result.Notification.ID,
	).Scan(&storedTenant, &storedUser, &storedType, &storedTitle))
	assert.Equal(t, tenant.DefaultID, storedTenant)
	assert.Equal(t, userOne, storedUser)
	assert.Equal(t, "security.alert", storedType)
	assert.Equal(t, "Sign-in from a new device", storedTitle)

	var jobTenant int64
	var payload []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id, payload FROM jobs WHERE kind = $1 ORDER BY id DESC LIMIT 1`, jobs.EmailJobKind,
	).Scan(&jobTenant, &payload))
	assert.Equal(t, tenant.DefaultID, jobTenant, "the email job must inherit the notification tenant")

	var msg email.Message
	require.NoError(t, json.Unmarshal(payload, &msg))
	assert.Equal(t, []string{"notifications-service-one@example.com"}, msg.To)
	assert.Equal(t, "Sign-in from a new device", msg.Subject)
	assert.Contains(t, msg.Body, "A new device signed in")
	assert.Contains(t, msg.HTML, "Sign-in from a new device")

	// The same service called in the second tenant writes only tenant-two data.
	ctxTwo := tenant.WithID(context.Background(), tenantTwo)
	resultTwo, err := svc.Create(ctxTwo, notificationcontracts.CreateNotificationInput{
		UserID: userTwo, Type: "system.info", Title: "Tenant two", Body: "Body",
	})
	require.NoError(t, err)
	require.NotNil(t, resultTwo.Notification)
	assert.Equal(t, tenantTwo, resultTwo.Notification.TenantID)

	items, _, err := svc.List(ctx, userOne, 20, "", false)
	require.NoError(t, err)
	require.Len(t, items, 1, "tenant one must not see tenant two's notification")
	assert.Equal(t, result.Notification.ID, items[0].ID)

	_, err = svc.Get(ctx, userOne, resultTwo.Notification.ID)
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, appErr.Status)

	count, err := svc.UnreadCount(ctx, userOne)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestCreateIntegrationHonorsPreferencesAndReadState(t *testing.T) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userID := integrationUser(t, pool, tenant.DefaultID, "notifications-prefs@example.com")
	recipients := staticRecipients{userID: {UserID: userID, Email: "notifications-prefs@example.com"}}
	svc := newIntegrationService(t, pool, recipients)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	_, err = svc.SetPreference(ctx, userID, notificationservice.PreferenceUpdate{
		Type: "marketing.promo", Channel: notificationservice.ChannelEmail, Enabled: new(false),
	})
	require.NoError(t, err)
	result, err := svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: userID, Type: "marketing.promo", Title: "In-app only", Body: "Body",
	})
	require.NoError(t, err)
	assert.False(t, result.EmailQueued)

	var jobsForType int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE payload->'To' @> jsonb_build_array($1::text)`,
		"notifications-prefs@example.com",
	).Scan(&jobsForType))
	assert.Zero(t, jobsForType, "a disabled email channel must not enqueue a job")

	_, err = svc.SetPreference(ctx, userID, notificationservice.PreferenceUpdate{
		Type: "marketing.promo", Channel: notificationservice.ChannelInApp, Enabled: new(false),
	})
	require.NoError(t, err)
	result, err = svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: userID, Type: "marketing.promo", Title: "Suppressed", Body: "Body",
	})
	require.NoError(t, err)
	assert.Nil(t, result.Notification)
	assert.False(t, result.EmailQueued)

	// Re-enable in-app and exercise unread/read/read-all against real SQL.
	_, err = svc.SetPreference(ctx, userID, notificationservice.PreferenceUpdate{
		Type: "marketing.promo", Channel: notificationservice.ChannelInApp, Enabled: new(true),
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: userID, Type: "marketing.promo", Title: "Read me", Body: "Body",
	})
	require.NoError(t, err)

	unread, hasMore, err := svc.List(ctx, userID, 20, "", true)
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, unread, 2)
	assert.Equal(t, "Read me", unread[0].Title)

	count, err := svc.UnreadCount(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	read, err := svc.MarkRead(ctx, userID, unread[0].ID)
	require.NoError(t, err)
	require.NotNil(t, read.ReadAt)

	updated, err := svc.MarkAllRead(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), updated)

	count, err = svc.UnreadCount(ctx, userID)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestCreateIntegrationRollsBackNotificationWhenQueueFails(t *testing.T) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userID := integrationUser(t, pool, tenant.DefaultID, "notifications-queue-fail@example.com")
	recipients := staticRecipients{userID: {UserID: userID, Email: "notifications-queue-fail@example.com"}}
	// The sender stands in for a full job queue: it fails while the row's
	// transaction is open, like an INSERT into jobs would.
	svc := notificationservice.NewNotificationService(
		repository.NewNotificationRepository(testutil.Scoped(pool)),
		recipients,
		failingSender{},
		repository.NewTransactor(testutil.NewTxManager(pool), failingSender{}),
	)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	_, err = svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: userID, Type: "system.info", Title: "Atomic", Body: "Body",
	})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, 500, appErr.Status)

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1`, userID,
	).Scan(&count))
	assert.Zero(t, count, "an enqueue failure must roll the in-app row back")
}
