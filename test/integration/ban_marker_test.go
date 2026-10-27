//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

func banKey(parts ...string) string {
	return strings.Join(parts, "-")
}

// A ban written under the previous key keeps blocking the address while that
// key is configured, and stops matching once it is removed. The latter is
// documented and expected: the marker cannot be re-hashed after erasure.
func TestBanMarker_OldKeyMatchesOnlyWhileItIsAPreviousKey(t *testing.T) {
	ctx := context.Background()
	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	member := testutil.CreateMember(t, ctx, pool, tenant.DefaultID, "ban-rotation@example.com", testutil.MemberOptions{})
	tctx := tenant.WithID(ctx, tenant.DefaultID)
	repo := identityrepo.NewMembershipRepository(fx.Scoped())

	oldKey := banKey("old", "ban", "pepper")
	newKey := banKey("new", "ban", "pepper")
	oldHasher, err := banhash.New(oldKey)
	require.NoError(t, err)
	oldHash, oldKid := oldHasher.Marker(member.Email)

	_, err = repo.Ban(tctx, member.UserID, core.BanMarker{Hash: oldHash, KeyID: oldKid})
	require.NoError(t, err, "ban under the old key")

	var storedHash []byte
	var storedKid string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT banned_email_hash, banned_email_hash_kid FROM memberships
		 WHERE tenant_id = $1 AND user_id = $2`, tenant.DefaultID, member.UserID).Scan(&storedHash, &storedKid))
	assert.Equal(t, oldHash, storedHash)
	assert.Equal(t, oldKid, storedKid)

	rotated, err := banhash.New(newKey, oldKey)
	require.NoError(t, err)
	banned, err := repo.EmailBanned(tctx, rotated.Markers(member.Email))
	require.NoError(t, err)
	assert.True(t, banned, "a ban written with a previous key must still match")

	withoutOld, err := banhash.New(newKey)
	require.NoError(t, err)
	banned, err = repo.EmailBanned(tctx, withoutOld.Markers(member.Email))
	require.NoError(t, err)
	assert.False(t, banned, "removing the previous key makes its bans ineffective")

	counts, err := repo.BannedMarkerCounts(ctx)
	require.NoError(t, err)
	assert.Contains(t, counts, core.BanMarkerCount{KeyID: oldKid, Bans: 1})
}

// A ban written after the rotation records the active key, and the startup
// check counts it under that key.
func TestBanMarker_NewBanRecordsTheActiveKey(t *testing.T) {
	ctx := context.Background()
	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	member := testutil.CreateMember(t, ctx, pool, tenant.DefaultID, "ban-active@example.com", testutil.MemberOptions{})
	tctx := tenant.WithID(ctx, tenant.DefaultID)
	repo := identityrepo.NewMembershipRepository(fx.Scoped())

	oldKey := banKey("previous", "ban", "pepper")
	activeKey := banKey("active", "ban", "pepper")
	rotated, err := banhash.New(activeKey, oldKey)
	require.NoError(t, err)
	hash, kid := rotated.Marker(member.Email)

	_, err = repo.Ban(tctx, member.UserID, core.BanMarker{Hash: hash, KeyID: kid})
	require.NoError(t, err)

	var storedKid string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT banned_email_hash_kid FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenant.DefaultID, member.UserID).Scan(&storedKid))
	assert.Equal(t, rotated.KeyIDs()[0], storedKid)
	assert.NotEqual(t, rotated.KeyIDs()[1], storedKid)
}
