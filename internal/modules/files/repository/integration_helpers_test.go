//go:build integration

package repository_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// setupFilesDB provisions an isolated schema through pgtest directly. The
// files module tests do not need the identity-owned RBAC catalog sync that
// internal/testutil adds, so they stay independent of other modules.
func setupFilesDB(t *testing.T) *pgtest.Fixture {
	t.Helper()
	return pgtest.New(t)
}

// createFilesTenant inserts an operational tenant and returns its id.
func createFilesTenant(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&id))
	return id
}

// createFilesMember inserts a global user and an active membership in tenantID.
func createFilesMember(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	email = strings.ToLower(strings.TrimSpace(email))
	var userID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, name, email_verified_at) VALUES ($1, $1, NOW()) RETURNING id`,
		email).Scan(&userID))
	_, err := pool.Exec(ctx,
		`INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)`, tenantID, userID)
	require.NoError(t, err)
	return userID
}
