//go:build integration

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
)

// secretFromOtpURI extracts the shared secret from the enrollment URI; the
// secret intentionally never appears anywhere else in the API.
func secretFromOtpURI(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := url.Parse(uri)
	require.NoError(t, err)
	secret := parsed.Query().Get("secret")
	require.NotEmpty(t, secret)
	return secret
}

// TestIntegration_MFAStepUpRefreshesTheSessionColumn covers both writes of the
// new column end to end: an MFA login stamps it at session creation, and the
// step-up route rewrites it on the caller's current session. It also proves the
// freshness gate: a session whose second factor is older than the harness's
// one-hour MFA_MAX_AGE is refused by a platform route until the step-up.
func TestIntegration_MFAStepUpRefreshesTheSessionColumn(t *testing.T) {
	h := newIdentityHarness(t, nil)
	app := identityApp(h)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	pw := testPassphrase

	status, body := do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/register",
		body: dto.RegisterRequest{Email: "step-up@example.com", Password: pw, Name: "Step Up"}})
	require.Equal(t, http.StatusCreated, status, string(body))

	var userID, passwordSessionID int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE email = 'step-up@example.com'`).Scan(&userID))
	mustLogin(t, app, "", "step-up@example.com", pw)
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT id FROM sessions WHERE user_id = $1 ORDER BY id LIMIT 1`, userID).Scan(&passwordSessionID))
	var passwordSessionMFA *time.Time
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT mfa_verified_at FROM sessions WHERE id = $1`, passwordSessionID).Scan(&passwordSessionMFA))
	assert.Nil(t, passwordSessionMFA, "a password-only session starts without an AAL2 timestamp")

	// Enable a factor on the password session, then sign in through the
	// two-step flow so the new session is created with an otp method.
	enrollment, err := h.mfa.EnrollTOTP(ctx, userID, passwordSessionID, pw)
	require.NoError(t, err)
	secret := secretFromOtpURI(t, enrollment.OtpauthURI)
	enrollCode, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	confirmed, err := h.mfa.ConfirmTOTP(ctx, userID, passwordSessionID, pw, enrollCode)
	require.NoError(t, err)
	recoveryCodes := confirmed.RecoveryCodes
	require.Len(t, recoveryCodes, 10)

	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/login",
		body: dto.LoginRequest{Email: "step-up@example.com", Password: pw}})
	require.Equal(t, http.StatusOK, status, string(body))
	var challenged dto.LoginResponse
	require.NoError(t, json.Unmarshal(body, &challenged))
	require.True(t, challenged.MFARequired)
	require.NotEmpty(t, challenged.MFAChallengeToken)

	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/mfa/verify",
		body: dto.MFAVerifyRequest{Challenge: challenged.MFAChallengeToken, Code: recoveryCodes[0]}})
	require.Equal(t, http.StatusOK, status, string(body))
	var mfaLogin dto.LoginResponse
	require.NoError(t, json.Unmarshal(body, &mfaLogin))
	require.NotEmpty(t, mfaLogin.AccessToken)

	var mfaSessionID int64
	var createdMFA *time.Time
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT id, mfa_verified_at FROM sessions
		 WHERE user_id = $1 AND revoked_at IS NULL ORDER BY id DESC LIMIT 1`, userID).
		Scan(&mfaSessionID, &createdMFA))
	require.NotNil(t, createdMFA, "an MFA login stamps mfa_verified_at at session creation")

	// Age the proof past the harness's one-hour MFA_MAX_AGE. The platform route
	// now refuses with the MFA code, not the permission code.
	stale := time.Now().Add(-13 * time.Hour)
	_, err = h.pool.Exec(ctx, `UPDATE sessions SET mfa_verified_at = $2 WHERE id = $1`, mfaSessionID, stale)
	require.NoError(t, err)
	require.NoError(t, h.sessionStore.Invalidate(ctx, userID))

	status, body = do(t, app, call{method: http.MethodGet, path: "/api/v1/platform/tenants", bearer: mfaLogin.AccessToken})
	assert.Equal(t, http.StatusForbidden, status, string(body))
	assert.Contains(t, string(body), "auth.mfa_required")

	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/mfa/step-up",
		bearer: mfaLogin.AccessToken, body: dto.MFAStepUpRequest{Code: recoveryCodes[1]}})
	require.Equal(t, http.StatusOK, status, string(body))
	var stepped dto.MFAStepUpResponse
	require.NoError(t, json.Unmarshal(body, &stepped))
	assert.True(t, stepped.VerifiedAt.After(stale))

	var refreshed time.Time
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT mfa_verified_at FROM sessions WHERE id = $1`, mfaSessionID).Scan(&refreshed))
	assert.True(t, refreshed.After(time.Now().Add(-time.Minute)), "step-up must rewrite the session column")

	// The freshness gate now passes; the member role still lacks the platform
	// permission, so the same route reaches authorization and returns a
	// different code.
	status, body = do(t, app, call{method: http.MethodGet, path: "/api/v1/platform/tenants", bearer: mfaLogin.AccessToken})
	assert.Equal(t, http.StatusForbidden, status, string(body))
	assert.Contains(t, string(body), "permission.denied")
	assert.NotContains(t, string(body), "auth.mfa_required")
}
