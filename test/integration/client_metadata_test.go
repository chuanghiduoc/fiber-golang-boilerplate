//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// TestClientMetadata_LoginLockoutIsPerIP drives the real HTTP stack: the
// platform middleware records the trusted-proxy-aware client IP once, the
// identity service keys its per-(email, IP) budget on it, and the session row
// keeps the same IP plus the User-Agent.
func TestClientMetadata_LoginLockoutIsPerIP(t *testing.T) {
	full := newIntegrationApp(t, "test")
	ctx := context.Background()
	member := testutil.CreateMember(t, ctx, full.pool, tenant.DefaultID, "lockout-ip@example.com",
		testutil.MemberOptions{Password: testPassphrase})

	login := func(t *testing.T, ip, userAgent, password string) (int, []byte) {
		t.Helper()
		body, err := json.Marshal(identitydto.LoginRequest{Email: member.Email, Password: password})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-Slug", "default")
		req.Header.Set("X-Forwarded-For", ip)
		req.Header.Set("User-Agent", userAgent)
		resp, err := full.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		payload, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, payload
	}

	const wrong = "not-the-fixture-password-2026"
	for range 5 {
		status, body := login(t, "198.51.100.1", "Browser/1.0", wrong)
		require.Equal(t, fiber.StatusUnauthorized, status, "body: %s", body)
	}
	status, body := login(t, "198.51.100.1", "Browser/1.0", wrong)
	assert.Equal(t, fiber.StatusTooManyRequests, status, "the sixth failure from one IP is locked: %s", body)

	// A different IP has its own budget: one failure, then a correct password.
	status, body = login(t, "198.51.100.2", "OtherBrowser/2.0", wrong)
	require.Equal(t, fiber.StatusUnauthorized, status, "body: %s", body)
	status, body = login(t, "198.51.100.2", "OtherBrowser/2.0", testPassphrase)
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var loginResponse identitydto.LoginResponse
	require.NoError(t, json.Unmarshal(body, &loginResponse))

	var ip string
	var userAgent *string
	require.NoError(t, full.pool.QueryRow(ctx, `
		SELECT host(s.ip), s.user_agent
		FROM sessions s
		JOIN refresh_tokens rt ON rt.session_id = s.id
		WHERE s.user_id = $1
		ORDER BY s.created_at DESC LIMIT 1`, member.UserID).Scan(&ip, &userAgent))
	assert.Equal(t, "198.51.100.2", ip, "the session records the trusted client IP")
	require.NotNil(t, userAgent)
	assert.Equal(t, "OtherBrowser/2.0", *userAgent)
}
