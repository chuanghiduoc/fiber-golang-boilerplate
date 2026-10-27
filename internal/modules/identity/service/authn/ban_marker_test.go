package authn

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
)

func testBanHasher(t *testing.T) *banhash.Hasher {
	t.Helper()
	hasher, err := banhash.New(strings.Join([]string{"unit", "test", "ban", "marker"}, "-"))
	require.NoError(t, err)
	return hasher
}

func TestBanMarkerNormalizesTheAddressLikeIdentity(t *testing.T) {
	hasher := testBanHasher(t)
	marker, err := newBanMarker(hasher, "  Alice@Example.COM  ")
	require.NoError(t, err)

	normalized, err := newBanMarker(hasher, core.NormalizeEmail("  Alice@Example.COM  "))
	require.NoError(t, err)
	assert.Equal(t, normalized, marker)
	assert.Len(t, marker.Hash, 32)
	assert.Len(t, marker.KeyID, 8)
}

func TestBanMarkerFailsLoudlyWithoutAKey(t *testing.T) {
	_, err := newBanMarker(nil, "user@example.com")
	require.Error(t, err)

	stores := fakes.New().Stores()
	stores.Bans = nil
	_, err = stores.EmailBanned(t.Context(), "user@example.com")
	require.Error(t, err)
}

func TestRemovalOnlyBackfillsAMissingBanMarker(t *testing.T) {
	hasher := testBanHasher(t)
	now := time.Now()
	active := &core.Membership{Status: core.MembershipStatusActive}
	marker, err := removalBanMarker(hasher, active, "user@example.com")
	require.NoError(t, err)
	assert.Empty(t, marker, "an active membership has no ban to preserve")

	banned := &core.Membership{BannedAt: &now, BannedEmailHash: []byte("existing")}
	marker, err = removalBanMarker(hasher, banned, "user@example.com")
	require.NoError(t, err)
	assert.Empty(t, marker, "an existing marker is preserved by the SQL update")

	legacy := &core.Membership{BannedAt: &now}
	marker, err = removalBanMarker(hasher, legacy, "User@Example.com ")
	require.NoError(t, err)
	expected, kid := hasher.Marker("user@example.com")
	assert.Equal(t, core.BanMarker{Hash: expected, KeyID: kid}, marker)
}
