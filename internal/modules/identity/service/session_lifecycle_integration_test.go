//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// Logout is possession-based: a tenant-B client that omits X-Tenant-Slug (so
// the request runs under the default tenant's RLS scope) must still revoke its
// session. The repository must bypass RLS for this one statement.
func TestLogout_RevokesTenantBSessionWithoutHeader(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "logout-acme")
	user := testutil.CreateMember(t, ctx, e.pool, acmeID, "logout-b@example.com", testutil.MemberOptions{})

	pair, err := e.sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	// No tenant header: the default tenant (1), not acme.
	require.NoError(t, e.sessions.Logout(ctx, pair.RefreshToken))
	assert.NotNil(t, scalar[*time.Time](t, e.pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID),
		"a tenant-B session must be revoked even without X-Tenant-Slug")
}

// A refresh token issued at sign-in records the session's absolute expiry,
// and refresh rejects the session once that instant has passed.
func TestSessions_AbsoluteLifetimeRejectsRefresh(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "lifetime@example.com", testutil.MemberOptions{})

	pair, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	expiresAt := scalar[time.Time](t, e.pool, `SELECT expires_at FROM sessions WHERE id = $1`, pair.SessionID)
	assert.WithinDuration(t, time.Now().Add(720*time.Hour), expiresAt, 2*time.Minute)

	_, err = e.pool.Exec(ctx, `UPDATE sessions SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, pair.SessionID)
	require.NoError(t, err)

	_, err = e.sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, 401, appStatus(err))
}
