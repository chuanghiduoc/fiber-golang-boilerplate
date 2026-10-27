//go:build integration

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apikeyrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func TestRepositoryAPIKeyLifecycleAndTenantIsolation(t *testing.T) {
	_, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass

	ctx := context.Background()
	tenantA := testID(100)
	var err error
	tenantB := testID(101)
	userA := testID(200)
	userB := testID(201)
	userC := testID(202)

	for _, fixture := range []struct {
		tenantID uuid.UUID
		slug     string
	}{
		{tenantA, "apikey-repo-a"},
		{tenantB, "apikey-repo-b"},
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $2)`,
			fixture.tenantID, fixture.slug)
		require.NoError(t, err)
	}
	for _, membership := range []struct {
		tenantID uuid.UUID
		userID   uuid.UUID
		email    string
	}{
		{tenantA, userA, "apikey-repo-a@example.com"},
		{tenantA, userC, "apikey-repo-c@example.com"},
		{tenantB, userB, "apikey-repo-b@example.com"},
	} {
		_, err = pool.Exec(ctx,
			`INSERT INTO users (id, email, name) VALUES ($1, $2, $2)`,
			membership.userID, membership.email)
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)`,
			membership.tenantID, membership.userID)
		require.NoError(t, err)
	}
	for _, scope := range []string{"files:read", "files:write"} {
		_, err = pool.Exec(ctx,
			`INSERT INTO permissions (key, owner) VALUES ($1, 'files')`,
			scope)
		require.NoError(t, err)
	}

	repo := apikeyrepo.NewAPIKeyRepository(fx.Scoped())
	ctxA := tenant.WithID(ctx, tenantA)
	ctxB := tenant.WithID(ctx, tenantB)

	credentials, err := apikey.Generate()
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(2 * time.Hour)
	created, err := repo.Create(ctxA, apikeysvc.NewKey{
		UserID:    userA,
		Name:      "integration",
		Prefix:    credentials.Prefix,
		Hash:      credentials.Hash,
		Scopes:    []authz.Permission{"files:write", "files:read"},
		ExpiresAt: expiresAt,
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, created.ID)
	assert.Equal(t, tenantA, created.TenantID)
	assert.Equal(t, userA, created.UserID)
	assert.Equal(t, credentials.Hash, created.Hash)
	assert.WithinDuration(t, expiresAt, created.ExpiresAt, time.Microsecond)

	// Scopes come back sorted, and every read path maps the same row.
	got, err := repo.GetByID(ctxA, created.ID)
	require.NoError(t, err)
	assert.Equal(t, []authz.Permission{"files:read", "files:write"}, got.Scopes)

	// The possession lookup is the one cross-tenant statement.
	byHash, err := repo.GetByHash(ctxB, credentials.Hash)
	require.NoError(t, err)
	assert.Equal(t, created.ID, byHash.ID)

	// Tenant-facing reads and writes are scoped by the request tenant.
	_, err = repo.GetByID(ctxB, created.ID)
	require.ErrorIs(t, err, apperror.ErrNotFound)
	_, err = repo.Update(ctxB, created.ID, "hijack", []authz.Permission{"files:read"})
	require.ErrorIs(t, err, apperror.ErrNotFound)

	updated, err := repo.Update(ctxA, created.ID, "renamed", []authz.Permission{"files:read"})
	require.NoError(t, err)
	assert.Equal(t, "renamed", updated.Name)
	assert.Equal(t, []authz.Permission{"files:read"}, updated.Scopes)

	listed, err := repo.ListCursor(ctxA, pagination.DBCursor{Limit: 10})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, created.ID, listed[0].ID)

	// The member-scoped page is filtered by user_id but keeps the tenant scope.
	credentialsC, err := apikey.Generate()
	require.NoError(t, err)
	other, err := repo.Create(ctxA, apikeysvc.NewKey{
		UserID:    userC,
		Name:      "other-member",
		Prefix:    credentialsC.Prefix,
		Hash:      credentialsC.Hash,
		Scopes:    []authz.Permission{"files:read"},
		ExpiresAt: time.Now().UTC().Add(2 * time.Hour),
	})
	require.NoError(t, err)

	mine, err := repo.ListByUserCursor(ctxA, userA, pagination.DBCursor{Limit: 10})
	require.NoError(t, err)
	require.Len(t, mine, 1)
	assert.Equal(t, created.ID, mine[0].ID)

	theirs, err := repo.ListByUserCursor(ctxA, userC, pagination.DBCursor{Limit: 10})
	require.NoError(t, err)
	require.Len(t, theirs, 1)
	assert.Equal(t, other.ID, theirs[0].ID)

	crossTenant, err := repo.ListByUserCursor(ctxB, userA, pagination.DBCursor{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, crossTenant, "another tenant sees an empty page, never these rows")

	_, err = repo.Revoke(ctxA, other.ID)
	require.NoError(t, err)

	touched := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, repo.TouchLastUsed(ctxA, created.ID, touched))
	var rawLastUsed *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_used_at FROM api_keys WHERE id = $1`, created.ID).Scan(&rawLastUsed))
	require.NotNil(t, rawLastUsed, "the UPDATE must persist last_used_at")
	got, err = repo.GetByID(ctxA, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt)
	assert.WithinDuration(t, touched, *got.LastUsedAt, time.Microsecond)

	rotated, err := repo.MarkRotated(ctxA, created.ID, 60)
	require.NoError(t, err)
	require.NotNil(t, rotated.RotatedAt)
	require.NotNil(t, rotated.RotationGraceUntil)
	assert.WithinDuration(t, time.Now().Add(time.Minute), *rotated.RotationGraceUntil, 2*time.Second)

	revoked, err := repo.Revoke(ctxA, created.ID)
	require.NoError(t, err)
	require.NotNil(t, revoked.RevokedAt)
}

// TestRepositoryHashesAreBytes proves key hashes stay binary: a text column
// would corrupt the 32-byte SHA-256 digest.
func TestRepositoryHashesAreBytes(t *testing.T) {
	_, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass

	ctx := context.Background()
	tenantID := testID(110)
	userID := testID(210)
	var err error
	_, err = pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, 'apikey-hash', 'apikey-hash')`, tenantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO users (id, email, name) VALUES ($1, 'apikey-hash@example.com', 'apikey-hash')`, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)`, tenantID, userID)
	require.NoError(t, err)

	repo := apikeyrepo.NewAPIKeyRepository(fx.Scoped())
	ctx = tenant.WithID(ctx, tenantID)
	credentials, err := apikey.Generate()
	require.NoError(t, err)
	created, err := repo.Create(ctx, apikeysvc.NewKey{
		UserID:    userID,
		Name:      "hash",
		Prefix:    credentials.Prefix,
		Hash:      credentials.Hash,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Len(t, created.Hash, apikey.KeyHashBytes)
	assert.Equal(t, credentials.Hash, created.Hash)

	byHash, err := repo.GetByHash(ctx, credentials.Hash)
	require.NoError(t, err)
	assert.Equal(t, created.ID, byHash.ID)

	var stored []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT key_hash FROM api_keys WHERE id = $1`, created.ID).Scan(&stored))
	assert.Equal(t, credentials.Hash, stored)
}

// TestTenantPurgerDeletesOnlyTheTargetTenant runs the real purge SQL against
// PostgreSQL and proves RLS plus the tenant predicate keep the other tenant.
func TestTenantPurgerDeletesOnlyTheTargetTenant(t *testing.T) {
	_, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass

	ctx := context.Background()
	tenantA := testID(120)
	tenantB := testID(121)
	userA := testID(220)
	userB := testID(221)
	var err error
	for _, fixture := range []struct {
		tenantID uuid.UUID
		userID   uuid.UUID
		slug     string
	}{
		{tenantA, userA, "apikey-purge-a"},
		{tenantB, userB, "apikey-purge-b"},
	} {
		_, err = pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $2)`, fixture.tenantID, fixture.slug)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO users (id, email, name) VALUES ($1, $2, $2)`, fixture.userID, fixture.slug+"@example.com")
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)`, fixture.tenantID, fixture.userID)
		require.NoError(t, err)
	}

	repo := apikeyrepo.NewAPIKeyRepository(fx.Scoped())
	for _, key := range []struct {
		tenantID uuid.UUID
		userID   uuid.UUID
	}{
		{tenantA, userA},
		{tenantB, userB},
	} {
		credentials, genErr := apikey.Generate()
		require.NoError(t, genErr)
		_, err = repo.Create(tenant.WithID(ctx, key.tenantID), apikeysvc.NewKey{
			UserID:    key.userID,
			Name:      "purge",
			Prefix:    credentials.Prefix,
			Hash:      credentials.Hash,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		})
		require.NoError(t, err)
	}

	require.NoError(t, apikeyrepo.NewTenantPurger(fx.Scoped()).PurgeTenant(ctx, tenantA))

	var countA, countB int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE tenant_id = $1`, tenantA).Scan(&countA))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE tenant_id = $1`, tenantB).Scan(&countB))
	assert.Zero(t, countA)
	assert.Equal(t, 1, countB, "the purge must not reach another tenant")
}
