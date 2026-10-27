//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

type adminHarness struct {
	app    *fiber.App
	pool   *pgxpool.Pool
	secret string
	admin  testutil.Member
	user   testutil.Member
	token  string
}

func setupAdminHarness(t *testing.T) *adminHarness {
	t.Helper()
	full := newIntegrationApp(t, "test")
	admin := testutil.CreateMember(t, t.Context(), full.pool, uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001"), "admin-a@example.com", testutil.MemberOptions{Role: identitydto.RoleAdmin})
	user := testutil.CreateMember(t, t.Context(), full.pool, uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001"), "user-a@example.com", testutil.MemberOptions{Role: identitydto.RoleMember})
	token := testutil.SessionToken(t, t.Context(), full.pool, admin, full.config.JWT.Secret)
	return &adminHarness{app: full.app, pool: full.pool, secret: full.config.JWT.Secret, admin: admin, user: user, token: token}
}

func (h *adminHarness) request(t *testing.T, method, path, bearer, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	if body != "" {
		contentType := "application/json"
		if method == http.MethodPatch {
			contentType = "application/merge-patch+json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Tenant-Slug", "default")
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, payload
}

func TestIntegration_AdminOperations(t *testing.T) {
	h := setupAdminHarness(t)

	// The admin routes are the canonical /stats and /members collections.
	status, body := h.request(t, http.MethodGet, "/api/v1/stats", h.token, "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var stats struct {
		ActiveUsers int64 `json:"activeUsers"`
	}
	require.NoError(t, json.Unmarshal(body, &stats))
	assert.Equal(t, int64(2), stats.ActiveUsers)

	status, body = h.request(t, http.MethodGet, "/api/v1/members", h.token, "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var page struct {
		Data []identitydto.UserResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	assert.Len(t, page.Data, 2)

	// A banned member is suspended; unbanning is the same PATCH with status
	// active. PATCH is conditional through the member's ETag.
	status, body = h.request(t, http.MethodGet, "/api/v1/members/"+h.user.UserID.String(), h.token, "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var read struct {
		MembershipStatus string `json:"membershipStatus"`
	}
	require.NoError(t, json.Unmarshal(body, &read))
	status, body = h.request(t, http.MethodPatch, "/api/v1/members/"+h.user.UserID.String(), h.token, `{"status":"suspended"}`)
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	assert.Contains(t, string(body), `"membershipStatus":"suspended"`)

	// A removed membership is a ban plus removal; restoring follows the same
	// lifecycle.
	status, body = h.request(t, http.MethodPatch, "/api/v1/members/"+h.user.UserID.String(), h.token, `{"status":"active"}`)
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	assert.Contains(t, string(body), `"membershipStatus":"active"`)

	// A regular member cannot reach the admin surface.
	userToken := testutil.SessionToken(t, t.Context(), h.pool, h.user, h.secret)
	status, body = h.request(t, http.MethodGet, "/api/v1/stats", userToken, "")
	assert.Equal(t, fiber.StatusForbidden, status, "body: %s", body)

	// Profile reads are authenticated and tenant scoped.
	status, body = h.request(t, http.MethodGet, "/api/v1/me/profile", userToken, "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	assert.Contains(t, string(body), h.user.Email)

	status, _ = h.request(t, http.MethodGet, "/api/v1/me/profile", "", "")
	assert.Equal(t, fiber.StatusUnauthorized, status)
}
