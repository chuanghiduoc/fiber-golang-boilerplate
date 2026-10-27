package handler

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

func TestOAuthState_BindsAndVerifiesTenant(t *testing.T) {
	key := oauthStateKey("unit-test-oauth-secret")
	tenantID := testID(2)
	state := encodeOAuthState(key, "nonce", tenantID)

	nonce, decodedTenant, ok := decodeOAuthState(key, state)
	require.True(t, ok)
	assert.Equal(t, "nonce", nonce)
	assert.Equal(t, tenantID, decodedTenant)

	// Changing the tenant without re-signing is a forgery.
	forged := strings.Replace(state, tenantID.String(), testID(3).String(), 1)
	_, _, ok = decodeOAuthState(key, forged)
	assert.False(t, ok)

	_, _, ok = decodeOAuthState(oauthStateKey("another-secret"), state)
	assert.False(t, ok, "a different key must not verify")
}

func TestOAuthFlow_CarriesPKCEVerifier(t *testing.T) {
	key := oauthStateKey("unit-test-oauth-secret")
	state := encodeOAuthState(key, "nonce", testID(2))
	flow := encodeOAuthFlow(key, state, "verifier-value")

	gotState, verifier, ok := decodeOAuthFlow(key, flow)
	require.True(t, ok)
	assert.Equal(t, state, gotState)
	assert.Equal(t, "verifier-value", verifier)

	_, _, ok = decodeOAuthFlow(key, flow+"x")
	assert.False(t, ok)
}

func TestCookieValue(t *testing.T) {
	header := "other=1; __Host-oauth_state=" + encodeOAuthFlow(oauthStateKey("s"), encodeOAuthState(oauthStateKey("s"), "n", testID(1)), "v") + "; last=2"
	value := cookieValue(header, oauthStateCookieName)
	require.NotEmpty(t, value)
	assert.Empty(t, cookieValue(header, "missing"))
}

func TestGoogleExchangeInvalidCode(t *testing.T) {
	err := googleExchangeInvalid()
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 401, resolvedStatus(appErr))
	assert.Equal(t, "auth.oauth_code_invalid", appErr.Code)
}

func TestSessionIDFromBearer(t *testing.T) {
	// The helper fails closed without a keyring or a bearer credential.
	assert.Equal(t, uuid.Nil, sessionIDFromBearer(nil, "Bearer token"))
}
