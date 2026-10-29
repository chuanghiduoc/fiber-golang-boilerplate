//go:build integration

package authn_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// Logout is possession-based: a tenant-B client that omits X-Tenant-Slug (so
// the request runs under the default tenant's RLS scope) must still revoke its
// session. The repository must bypass RLS for this one statement.
func TestLogout_RevokesTenantBSessionWithoutHeader(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "logout-acme")
	user := testutil.CreateMember(t, ctx, e.Pool, acmeID, "logout-b@example.com", testutil.MemberOptions{})

	pair, err := e.Sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)

	// No tenant header: the default tenant (1), not acme.
	require.NoError(t, e.Sessions.Logout(ctx, pair.RefreshToken))
	assert.NotNil(t, scalar[*time.Time](t, e.Pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID),
		"a tenant-B session must be revoked even without X-Tenant-Slug")
}

// A refresh token issued at sign-in records the session's absolute expiry,
// and refresh rejects the session once that instant has passed.
func TestSessions_AbsoluteLifetimeRejectsRefresh(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "lifetime@example.com", testutil.MemberOptions{})

	pair, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)

	expiresAt := scalar[time.Time](t, e.Pool, `SELECT expires_at FROM sessions WHERE id = $1`, pair.SessionID)
	assert.WithinDuration(t, time.Now().Add(720*time.Hour), expiresAt, 2*time.Minute)

	_, err = e.Pool.Exec(ctx, `UPDATE sessions SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, pair.SessionID)
	require.NoError(t, err)

	_, err = e.Sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, 401, pgfixtures.AppStatus(err))
}

// Two tabs refreshing with the same token at once: exactly one rotates, the
// loser gets the dedicated concurrent code, and the shared session stays live.
func TestSessions_ConcurrentRefreshOneWinsSessionStaysLive(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "concurrent@example.com", testutil.MemberOptions{})

	pair, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)

	start := make(chan struct{})
	type outcome struct {
		result *authn.RefreshResult
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := e.Sessions.Refresh(ctx, pair.RefreshToken, nil)
			results <- outcome{result: result, err: err}
		}()
	}
	close(start)

	var succeeded *authn.RefreshResult
	var concurrent int
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			succeeded = got.result
		case pgfixtures.AppCode(got.err) == string(identitycatalog.CodeAuthRefreshConcurrent):
			concurrent++
		default:
			t.Fatalf("unexpected refresh error: %v", got.err)
		}
	}
	require.NotNil(t, succeeded, "one request must rotate the token")
	assert.Equal(t, 1, concurrent, "the loser gets auth.refresh_concurrent")

	revokedAt := scalar[*time.Time](t, e.Pool, `SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID)
	assert.Nil(t, revokedAt, "a concurrent refresh must not revoke the session")

	// The family is still usable: the winning token rotates again.
	_, err = e.Sessions.Refresh(ctx, succeeded.Tokens.RefreshToken, nil)
	require.NoError(t, err)
}

// Reuse outside the 10-second grace still revokes the whole session.
func TestSessions_ReuseAfterGraceRevokesSession(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "old-reuse@example.com", testutil.MemberOptions{})

	pair, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)
	_, err = e.Sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.NoError(t, err)

	_, err = e.Pool.Exec(ctx,
		`UPDATE refresh_tokens SET used_at = NOW() - INTERVAL '11 seconds' WHERE session_id = $1 AND used_at IS NOT NULL`,
		pair.SessionID)
	require.NoError(t, err)

	_, err = e.Sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, string(identitycatalog.CodeAuthRefreshInvalid), pgfixtures.AppCode(err))
	assert.Equal(t, "refresh_reuse", scalar[string](t, e.Pool, `SELECT revoked_reason FROM sessions WHERE id = $1`, pair.SessionID))
}

// Two tokens rotated inside the same millisecond: uuidv7 ordering then falls
// back to random bits, so the newer row can carry the smaller id. The grace
// decision must compare creation time, not id, or a benign concurrent refresh
// revokes the shared session.
func TestSessions_ConcurrentRefreshGraceIgnoresUUIDOrder(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "uuid-order@example.com", testutil.MemberOptions{})

	pair, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)
	_, err = e.Sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.NoError(t, err)

	// The used token gets the larger id, the newer unused token the smaller.
	_, err = e.Pool.Exec(ctx, `
		UPDATE refresh_tokens
		SET id = CASE WHEN used_at IS NULL
		              THEN '00000000-0000-7000-8000-000000000001'::uuid
		              ELSE '00000000-0000-7000-8000-000000000002'::uuid END,
		    created_at = '2026-01-01 00:00:00+00'::timestamptz
		WHERE session_id = $1`, pair.SessionID)
	require.NoError(t, err)

	_, err = e.Sessions.Refresh(ctx, pair.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, string(identitycatalog.CodeAuthRefreshConcurrent), pgfixtures.AppCode(err))
	assert.Nil(t, scalar[*time.Time](t, e.Pool, `SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID),
		"a concurrent refresh must not revoke the session")
}
