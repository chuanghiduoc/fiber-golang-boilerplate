//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func createUser(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, name, email_verified_at) VALUES ($1, $1, NOW()) RETURNING id`,
		email).Scan(&id))
	_, err := pool.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)`, tenantID, id)
	require.NoError(t, err)
	return id
}

func createTenant(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&id))
	_, err := pool.Exec(context.Background(), `INSERT INTO tenant_settings (tenant_id) VALUES ($1)`, id)
	require.NoError(t, err)
	return id
}

func TestRepositoryFlagLifecycle(t *testing.T) {
	_, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)

	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	repo := New(fx.Scoped())
	key := "file_uploads"
	enabled := true

	created, err := repo.CreateFlag(ctx, key, featuressvc.FlagWrite{Enabled: &enabled, KillSwitch: false, Description: "created"})
	require.NoError(t, err)
	assert.True(t, created.Configured)
	assert.True(t, created.Enabled)
	assert.Equal(t, "created", created.Description)

	_, err = repo.CreateFlag(ctx, key, featuressvc.FlagWrite{KillSwitch: false})
	require.Error(t, err)
	var conflict *apperror.AppError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 409, conflict.Status)

	disabled := false
	updated, err := repo.UpsertFlag(ctx, key, featuressvc.FlagWrite{Enabled: &disabled, KillSwitch: true, Description: "updated"})
	require.NoError(t, err)
	assert.True(t, updated.KillSwitch)
	assert.False(t, updated.Enabled)
	assert.Equal(t, "updated", updated.Description)

	got, err := repo.GetFlag(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "updated", got.Description)

	// Conditional writes enforce the validator in the statement itself.
	require.NotNil(t, updated.UpdatedAt)
	stale := updated.UpdatedAt.Add(-time.Second)
	_, err = repo.UpdateFlagIfUnchanged(ctx, key, featuressvc.FlagWrite{KillSwitch: true, Description: "raced"}, stale)
	var precondition *apperror.AppError
	require.ErrorAs(t, err, &precondition)
	assert.Equal(t, 412, precondition.Status)
	err = repo.DeleteFlagIfUnchanged(ctx, key, stale)
	require.ErrorAs(t, err, &precondition)
	assert.Equal(t, 412, precondition.Status)
	_, err = repo.CreateFlagIfAbsent(ctx, key, featuressvc.FlagWrite{KillSwitch: false})
	require.ErrorAs(t, err, &precondition)
	assert.Equal(t, 412, precondition.Status)

	fresh, err := repo.UpdateFlagIfUnchanged(ctx, key, featuressvc.FlagWrite{KillSwitch: true, Description: "checked"}, *updated.UpdatedAt)
	require.NoError(t, err)
	assert.Equal(t, "checked", fresh.Description)
	require.NotNil(t, fresh.UpdatedAt)
	require.NoError(t, repo.DeleteFlagIfUnchanged(ctx, key, *fresh.UpdatedAt))

	_, err = repo.GetFlag(ctx, key)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))
}

func TestRepositoryTargetsStayTenantScopedExceptPlatformView(t *testing.T) {
	_, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass

	tenantTwo := createTenant(t, pool, "features-two")
	userOne := createUser(t, pool, tenant.DefaultID, "features-one@example.com")
	userTwo := createUser(t, pool, tenantTwo, "features-two@example.com")

	repo := New(fx.Scoped())
	ctxOne := tenant.WithID(context.Background(), tenant.DefaultID)
	ctxTwo := tenant.WithID(context.Background(), tenantTwo)
	key := "file_uploads"

	tenantWide, err := repo.UpsertTarget(ctxOne, key, tenant.DefaultID, nil, true)
	require.NoError(t, err)
	assert.Equal(t, tenant.DefaultID, tenantWide.TenantID)

	userTarget, err := repo.UpsertTarget(ctxOne, key, tenant.DefaultID, &userOne, false)
	require.NoError(t, err)
	assert.Equal(t, userOne, *userTarget.UserID)

	// A statement scoped to tenant two cannot see tenant one's targets.
	twoTargets, err := repo.ListTargets(ctxTwo, key)
	require.NoError(t, err)
	assert.Empty(t, twoTargets)

	// The platform view spans every tenant.
	all, err := repo.ListAllTargets(ctxOne, key)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	// A platform write for another tenant uses the bypass path.
	crossTarget, err := repo.UpsertTarget(ctxOne, key, tenantTwo, &userTwo, true)
	require.NoError(t, err)
	assert.Equal(t, tenantTwo, crossTarget.TenantID)
	assert.Equal(t, userTwo, *crossTarget.UserID)

	all, err = repo.ListAllTargets(ctxOne, key)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// Deleting by canonical scope only removes the addressed target.
	require.NoError(t, repo.DeleteTarget(ctxOne, key, tenant.DefaultID, &userOne))
	all, err = repo.ListAllTargets(ctxOne, key)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	require.NoError(t, repo.DeleteTarget(ctxOne, key, tenantTwo, &userTwo))
	require.NoError(t, repo.DeleteTarget(ctxOne, key, tenant.DefaultID, nil))

	err = repo.DeleteTarget(ctxOne, key, tenant.DefaultID, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	// The tenant-wide target upsert created the parent flag row with no
	// database default, so it anchors targets without changing other tenants.
	flag, err := repo.GetFlag(ctxOne, key)
	require.NoError(t, err)
	assert.False(t, flag.Configured)
}
