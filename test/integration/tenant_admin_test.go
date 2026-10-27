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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

type tenantAdminHarness struct {
	app        *fiber.App
	pool       *pgxpool.Pool
	adminToken string
	tenantB    uuid.UUID
	adminAID   uuid.UUID
	userBID    uuid.UUID
	bannedBID  uuid.UUID
}

func setupTenantAdminHarness(t *testing.T) *tenantAdminHarness {
	t.Helper()
	ctx := t.Context()
	full := newIntegrationApp(t, "test")
	tenantB := testutil.CreateTenant(t, ctx, full.pool, "admin-acme")

	member := func(tenantID uuid.UUID, email, role string) testutil.Member {
		t.Helper()
		return testutil.CreateMember(t, ctx, full.pool, tenantID, email, testutil.MemberOptions{Role: role})
	}
	adminA := member(uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001"), "admin-a@example.com", identitydto.RoleAdmin)
	member(uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001"), "user-a@example.com", identitydto.RoleMember)
	userB := member(tenantB, "user-b@example.com", identitydto.RoleMember)
	bannedB := member(tenantB, "banned-b@example.com", identitydto.RoleMember)
	_, err := full.pool.Exec(ctx,
		`UPDATE memberships SET status = 'suspended', banned_at = NOW() WHERE tenant_id = $1 AND user_id = $2`,
		tenantB, bannedB.UserID)
	require.NoError(t, err)

	return &tenantAdminHarness{
		app:        full.app,
		pool:       full.pool,
		adminToken: testutil.SessionToken(t, ctx, full.pool, adminA, full.config.JWT.Secret),
		tenantB:    tenantB,
		adminAID:   adminA.UserID,
		userBID:    userB.UserID,
		bannedBID:  bannedB.UserID,
	}
}

func (h *tenantAdminHarness) request(t *testing.T, method, path, body string) (status int, respBody []byte) {
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
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	req.Header.Set("X-Tenant-Slug", "default")
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, respBody
}

func (h *tenantAdminHarness) membership(t *testing.T, tenantID, userID uuid.UUID) (role, status string) {
	t.Helper()
	require.NoError(t, h.pool.QueryRow(context.Background(), `
		SELECT COALESCE((
			SELECT r.key FROM membership_roles mr JOIN roles r ON r.id = mr.role_id
			WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
			ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END
			LIMIT 1
		), 'member'), m.status
		FROM memberships m WHERE m.tenant_id = $1 AND m.user_id = $2`, tenantID, userID,
	).Scan(&role, &status))
	return role, status
}

func TestTenantAdminModule_CrossTenantOperationsAreNotFound(t *testing.T) {
	h := setupTenantAdminHarness(t)
	defaultTenant := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001")

	// List: only tenant A members are visible.
	status, body := h.request(t, http.MethodGet, "/api/v1/members", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var page struct {
		Data []identitydto.UserResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Data, 2)
	for _, user := range page.Data {
		assert.NotEqual(t, h.userBID, user.ID)
		assert.NotEqual(t, h.bannedBID, user.ID)
	}

	// Stats: tenant A's active members only.
	status, body = h.request(t, http.MethodGet, "/api/v1/stats", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var stats identitydto.StatsResponse
	require.NoError(t, json.Unmarshal(body, &stats))
	assert.Equal(t, int64(2), stats.ActiveUsers)
	assert.Zero(t, stats.DeletedUsers)

	// Role update, ban, and unban of a user without a tenant A membership:
	// not found, not forbidden, and the other tenant's rows are untouched.
	status, body = h.request(t, http.MethodPatch,
		"/api/v1/members/"+h.userBID.String(), `{"status":"suspended"}`)
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	status, body = h.request(t, http.MethodPatch,
		"/api/v1/members/"+h.bannedBID.String(), `{"status":"active"}`)
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	role, memberStatus := h.membership(t, h.tenantB, h.userBID)
	assert.Equal(t, identitydto.RoleMember, role)
	assert.Equal(t, "active", memberStatus, "tenant B active member must not be banned")

	role, memberStatus = h.membership(t, h.tenantB, h.bannedBID)
	assert.Equal(t, identitydto.RoleMember, role)
	assert.Equal(t, "suspended", memberStatus, "tenant B banned member must stay banned")

	role, memberStatus = h.membership(t, defaultTenant, h.adminAID)
	assert.Equal(t, identitydto.RoleAdmin, role)
	assert.Equal(t, "active", memberStatus)
}
