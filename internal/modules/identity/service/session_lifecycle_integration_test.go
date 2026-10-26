//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// Logout is possession-based: a tenant-B client that omits X-Tenant-Slug (so
// the request runs under the default tenant's RLS scope) must still revoke its
// session. The repository must bypass RLS for this one statement.
func TestLogout_RevokesTenantBSessionWithoutHeader(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
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
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
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

// Two tabs refreshing with the same token at once: exactly one rotates, the
// loser gets the dedicated concurrent code, and the shared session stays live.
func TestSessions_ConcurrentRefreshOneWinsSessionStaysLive(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "concurrent@example.com", testutil.MemberOptions{})

	pair, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	start := make(chan struct{})
	type outcome struct {
		result *service.RefreshResult
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := e.sessions.Refresh(ctx, pair.RefreshToken, nil)
			results <- outcome{result: result, err: err}
		}()
	}
	close(start)

	var succeeded *service.RefreshResult
	var concurrent int
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			succeeded = got.result
		case appCode(got.err) == string(identitycatalog.CodeAuthRefreshConcurrent):
			concurrent++
		default:
			t.Fatalf("unexpected refresh error: %v", got.err)
		}
	}
	require.NotNil(t, succeeded, "one request must rotate the token")
	assert.Equal(t, 1, concurrent, "the loser gets auth.refresh_concurrent")

	revokedAt := scalar[*time.Time](t, e.pool, `SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID)
	assert.Nil(t, revokedAt, "a concurrent refresh must not revoke the session")

	// The family is still usable: the winning token rotates again.
	_, err = e.sessions.Refresh(ctx, succeeded.Tokens.RefreshToken, nil)
	require.NoError(t, err)
}

// Reuse outside the 10-second grace still revokes the whole session.
func TestSessions_ReuseAfterGraceRevokesSession(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "old-reuse@example.com", testutil.MemberOptions{})

	pair, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)
	_, err = e.sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.NoError(t, err)

	_, err = e.pool.Exec(ctx,
		`UPDATE refresh_tokens SET used_at = NOW() - INTERVAL '11 seconds' WHERE session_id = $1 AND used_at IS NOT NULL`,
		pair.SessionID)
	require.NoError(t, err)

	_, err = e.sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, string(identitycatalog.CodeAuthRefreshInvalid), appCode(err))
	assert.Equal(t, "refresh_reuse", scalar[string](t, e.pool, `SELECT revoked_reason FROM sessions WHERE id = $1`, pair.SessionID))
}
