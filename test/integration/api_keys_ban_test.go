//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TestIntegration_APIKeyBanRevokesAndUnbanDoesNotRevive proves a key issued
// before a ban never authenticates again, even after the owner is unbanned,
// while a key issued after the unban works.
func TestIntegration_APIKeyBanRevokesAndUnbanDoesNotRevive(t *testing.T) {
	h := setupAPIKeyHarness(t)
	ctx := tenant.WithID(context.Background(), h.tenant)

	var ownerID int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, name) VALUES ($1, 'apikey-ban@test.com', 'Ban Me') RETURNING id`,
		h.tenant,
	).Scan(&ownerID))

	before, err := h.svc.Create(ctx, ownerID, apikeydto.CreateRequest{Name: "before-ban", Scopes: []string{"files:read"}})
	require.NoError(t, err)
	resp, body := h.request(t, before.Key, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)

	users := identityrepo.NewUserRepository(h.pool)
	_, err = users.Ban(ctx, ownerID)
	require.NoError(t, err)
	resp, _ = h.request(t, before.Key, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, "banned owner")

	_, err = users.Restore(ctx, ownerID)
	require.NoError(t, err)
	resp, _ = h.request(t, before.Key, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, "unban must not revive a key issued before the ban")

	after, err := h.svc.Create(ctx, ownerID, apikeydto.CreateRequest{Name: "after-unban", Scopes: []string{"files:read"}})
	require.NoError(t, err)
	resp, body = h.request(t, after.Key, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "a key issued after the unban works; body: %s", body)
}
