//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func createUser(t *testing.T, pool *pgxpool.Pool, tenantID int64, email string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (tenant_id, email, name) VALUES ($1, $2, $2) RETURNING id`,
		tenantID, email,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func createTenant(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id`, slug,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func TestRepositoryUserAndTenantIsolation(t *testing.T) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx := context.Background()
	tenantTwo := createTenant(t, pool, "notifications-two")
	userOne := createUser(t, pool, tenant.DefaultID, "notifications-one@example.com")
	userTwo := createUser(t, pool, tenantTwo, "notifications-two@example.com")

	repo := NewNotificationRepository(pool)
	ctxOne := tenant.WithID(ctx, tenant.DefaultID)
	ctxTwo := tenant.WithID(ctx, tenantTwo)

	first, err := repo.Create(ctxOne, notificationsqlc.CreateNotificationParams{
		TenantID: 999, // must be overridden by the context tenant
		UserID:   userOne,
		Type:     "security.alert",
		Title:    "Tenant one",
		Body:     "Body one",
		Data:     []byte(`{"source":"test"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, tenant.DefaultID, first.TenantID)
	assert.Equal(t, userOne, first.UserID)

	second, err := repo.Create(ctxTwo, notificationsqlc.CreateNotificationParams{
		UserID: userTwo,
		Type:   "security.alert",
		Title:  "Tenant two",
		Body:   "Body two",
		Data:   []byte(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, tenantTwo, second.TenantID)

	// An inconsistent row (tenant two owning a notification for a tenant-one
	// user id) is rejected by the tenant-composite foreign key.
	_, err = pool.Exec(ctx,
		`INSERT INTO notifications (tenant_id, user_id, type, title, body)
		 VALUES ($1, $2, 'system.info', 'Cross tenant', 'Body')`,
		tenantTwo, userOne,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fk_notifications_tenant_user")

	rows, err := repo.ListCursor(ctxOne, userOne, pagination.DBCursor{Limit: 10}, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, first.ID, rows[0].ID)

	// Queries are scoped by tenant, not only by user id.
	rows, err = repo.ListCursor(ctxTwo, userOne, pagination.DBCursor{Limit: 10}, false)
	require.NoError(t, err)
	assert.Empty(t, rows, "a tenant-one user id must not expose rows in tenant two")

	rows, err = repo.ListCursor(ctxOne, userTwo, pagination.DBCursor{Limit: 10}, false)
	require.NoError(t, err)
	assert.Empty(t, rows, "a user id from another tenant must not expose rows")

	_, err = repo.GetByID(ctxOne, second.ID, userTwo)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	_, err = repo.GetByID(ctxTwo, first.ID, userOne)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	_, err = repo.MarkRead(ctxTwo, first.ID, userOne)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	read, err := repo.MarkRead(ctxOne, first.ID, userOne)
	require.NoError(t, err)
	require.True(t, read.ReadAt.Valid)
	readAgain, err := repo.MarkRead(ctxOne, first.ID, userOne)
	require.NoError(t, err)
	assert.Equal(t, read.ReadAt.Time, readAgain.ReadAt.Time)

	count, err := repo.CountUnread(ctxOne, userOne)
	require.NoError(t, err)
	assert.Zero(t, count)

	updated, err := repo.MarkAllRead(ctxOne, userOne)
	require.NoError(t, err)
	assert.Zero(t, updated)
}

func TestRepositoryUnreadListAndCursor(t *testing.T) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	userID := createUser(t, pool, tenant.DefaultID, "notifications-cursor@example.com")
	repo := NewNotificationRepository(pool)

	base := time.Now().UTC().Add(-time.Hour)
	for i, title := range []string{"one", "two", "three"} {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO notifications (tenant_id, user_id, type, title, body, created_at)
			 VALUES ($1, $2, 'system.info', $3, 'Body', $4)`,
			tenant.DefaultID, userID, title, base.Add(time.Duration(i)*time.Minute),
		)
		require.NoError(t, err)
	}

	count, err := repo.CountUnread(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), count)

	page, err := repo.ListCursor(ctx, userID, pagination.DBCursor{Limit: 3}, true)
	require.NoError(t, err)
	require.Len(t, page, 3)
	assert.Equal(t, "three", page[0].Title)
	assert.Equal(t, "one", page[2].Title)

	_, err = repo.MarkRead(ctx, page[1].ID, userID)
	require.NoError(t, err)
	unread, err := repo.ListCursor(ctx, userID, pagination.DBCursor{Limit: 10}, true)
	require.NoError(t, err)
	assert.Len(t, unread, 2)

	updated, err := repo.MarkAllRead(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated)
	unread, err = repo.ListCursor(ctx, userID, pagination.DBCursor{Limit: 10}, true)
	require.NoError(t, err)
	assert.Empty(t, unread)
}

func TestRepositoryPreferencesAreUniquelyScoped(t *testing.T) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx := context.Background()
	tenantTwo := createTenant(t, pool, "notifications-prefs-two")
	userOne := createUser(t, pool, tenant.DefaultID, "notifications-prefs-one@example.com")
	userTwo := createUser(t, pool, tenantTwo, "notifications-prefs-two@example.com")
	repo := NewNotificationRepository(pool)
	ctxOne := tenant.WithID(ctx, tenant.DefaultID)
	ctxTwo := tenant.WithID(ctx, tenantTwo)

	first, err := repo.UpsertPreference(ctxOne, notificationsqlc.UpsertNotificationPreferenceParams{
		UserID: userOne, Type: "security.alert", Channel: "email", Enabled: false,
	})
	require.NoError(t, err)
	assert.False(t, first.Enabled)

	same, err := repo.UpsertPreference(ctxOne, notificationsqlc.UpsertNotificationPreferenceParams{
		UserID: userOne, Type: "security.alert", Channel: "email", Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, first.ID, same.ID, "the unique index must make the upsert idempotent")
	assert.True(t, same.Enabled)

	_, err = repo.GetPreference(ctxOne, userOne, "security.alert", "in_app")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	_, err = repo.GetPreference(ctxTwo, userOne, "security.alert", "email")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	_, err = repo.GetPreference(ctxOne, userTwo, "security.alert", "email")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	list, err := repo.ListPreferences(ctxOne, userOne)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, first.ID, list[0].ID)
}
