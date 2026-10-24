//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestTenantRepository_StatusAwareAndExistingLookups(t *testing.T) {
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name, status) VALUES ('suspended-acme', 'Suspended Acme', 'suspended') RETURNING id`,
	).Scan(&id))

	repo := NewTenantRepository(pool)

	// Request resolution only sees active tenants.
	_, err = repo.ResolveBySlug(ctx, "suspended-acme")
	require.ErrorIs(t, err, apperror.ErrNotFound)
	_, err = repo.GetByID(ctx, id)
	require.ErrorIs(t, err, apperror.ErrNotFound)

	// Startup/seed existence checks see the row and its status.
	bySlug, err := repo.GetBySlugIncludingSuspended(ctx, "suspended-acme")
	require.NoError(t, err)
	require.Equal(t, id, bySlug.ID)
	require.Equal(t, "suspended", bySlug.Status)

	byID, err := repo.GetByIDIncludingSuspended(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "suspended-acme", byID.Slug)

	baseline, configured, err := StartupTenants(ctx, repo, "suspended-acme")
	require.NoError(t, err, "a suspended configured default must not fail startup")
	require.Equal(t, int64(1), baseline.ID)
	require.Equal(t, id, configured.ID)

	_, _, err = StartupTenants(ctx, repo, "missing")
	require.Error(t, err)
	require.True(t, errors.Is(err, apperror.ErrNotFound), "missing slug error = %v", err)
}
