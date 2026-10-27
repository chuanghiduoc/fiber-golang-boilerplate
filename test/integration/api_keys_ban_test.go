//go:build integration

package integration_test

import (
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apikeyservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// TestIntegration_APIKeyBanRevokesAndUnbanDoesNotRevive proves a key issued
// before a membership ban never authenticates again, even after the member
// is unbanned, while a key issued after the unban works.
func TestIntegration_APIKeyBanRevokesAndUnbanDoesNotRevive(t *testing.T) {
	h := setupAPIKeyHarness(t)
	ctx := systemTenantCtx(h.tenant)

	member := testutil.CreateMember(t, ctx, h.pool, h.tenant, "apikey-ban@test.com", testutil.MemberOptions{})
	ownerID := member.UserID

	before, err := h.svc.Create(ctx, ownerID, apikeyservice.CreateInput{Name: "before-ban", Scopes: []string{"files:read"}})
	require.NoError(t, err)
	resp, body := h.request(t, before.Secret, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)

	hasher, err := banhash.New(strings.Join([]string{"api", "key", "ban", "pepper"}, "-"))
	require.NoError(t, err)
	hash, kid := hasher.Marker(member.Email)
	memberships := identityrepo.NewMembershipRepository(h.pool)
	_, err = memberships.Ban(ctx, ownerID, core.BanMarker{Hash: hash, KeyID: kid})
	require.NoError(t, err)
	resp, _ = h.request(t, before.Secret, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, "banned owner")

	_, err = memberships.Unban(ctx, ownerID)
	require.NoError(t, err)
	resp, _ = h.request(t, before.Secret, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, "unban must not revive a key issued before the ban")

	after, err := h.svc.Create(ctx, ownerID, apikeyservice.CreateInput{Name: "after-unban", Scopes: []string{"files:read"}})
	require.NoError(t, err)
	resp, body = h.request(t, after.Secret, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "a key issued after the unban works; body: %s", body)
}
