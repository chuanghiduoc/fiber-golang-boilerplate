//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// A sign-in that upgrades an old hash must not overwrite a password change
// that committed after the password check verified: the update is conditional
// on the hash that was compared.
func TestUserRepository_RehashPasswordIsConditionalOnTheVerifiedHash(t *testing.T) {
	ctx := context.Background()
	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	oldPassword := strings.Join([]string{"first", "horse", "battery", "staple"}, "-")
	newPassword := strings.Join([]string{"second", "horse", "battery", "staple"}, "-")
	member := testutil.CreateMember(t, ctx, pool, tenant.DefaultID, "rehash-race@example.com",
		testutil.MemberOptions{Password: oldPassword})
	repo := identityrepo.NewUserRepository(fx.Scoped())

	verifiedHash, err := repo.PasswordHash(ctx, member.UserID)
	require.NoError(t, err)
	concurrentHash, err := identityservice.HashPassword(newPassword)
	require.NoError(t, err)
	_, err = repo.ReplacePassword(ctx, member.UserID, concurrentHash)
	require.NoError(t, err)

	upgradeHash, err := identityservice.HashPassword(oldPassword)
	require.NoError(t, err)
	require.NoError(t, repo.RehashPassword(ctx, member.UserID, verifiedHash, upgradeHash))

	stored, err := repo.PasswordHash(ctx, member.UserID)
	require.NoError(t, err)
	require.Equal(t, concurrentHash, stored, "the stale upgrade must not clobber the concurrent password change")

	// When the verified hash is still current, the upgrade is written.
	require.NoError(t, repo.RehashPassword(ctx, member.UserID, concurrentHash, upgradeHash))
	stored, err = repo.PasswordHash(ctx, member.UserID)
	require.NoError(t, err)
	require.Equal(t, upgradeHash, stored)
}
