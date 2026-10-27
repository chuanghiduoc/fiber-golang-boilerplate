package banhash_test

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
)

func testKey(parts ...string) string {
	return strings.Join(parts, "-")
}

func TestMarkerIsKeyedAndNormalizesTheAddress(t *testing.T) {
	hasher, err := banhash.New(testKey("correct", "horse", "battery", "staple"))
	require.NoError(t, err)

	hash, kid := hasher.Marker("  Alice@Example.COM  ")
	assert.Len(t, hash, 32)
	assert.Len(t, kid, 8)
	assert.Regexp(t, "^[0-9a-f]{8}$", kid)

	same, sameKid := hasher.Marker("alice@example.com")
	assert.Equal(t, hash, same)
	assert.Equal(t, kid, sameKid)

	raw := sha256.Sum256([]byte("alice@example.com"))
	assert.False(t, bytes.Equal(raw[:], hash),
		"the ban marker must not be an unkeyed SHA-256 of the address")
}

func TestMarkerChangesWithTheKey(t *testing.T) {
	first, err := banhash.New(testKey("first", "pepper"))
	require.NoError(t, err)
	second, err := banhash.New(testKey("second", "pepper"))
	require.NoError(t, err)

	firstHash, firstKid := first.Marker("user@example.com")
	secondHash, secondKid := second.Marker("user@example.com")
	assert.NotEqual(t, firstHash, secondHash)
	assert.NotEqual(t, firstKid, secondKid)
}

func TestRotationKeepsPreviousMarkersMatchable(t *testing.T) {
	oldKey := testKey("old", "pepper")
	newKey := testKey("new", "pepper")
	oldHasher, err := banhash.New(oldKey)
	require.NoError(t, err)
	rotated, err := banhash.New(newKey, oldKey)
	require.NoError(t, err)

	oldHash, oldKid := oldHasher.Marker("banned@example.com")
	activeHash, activeKid := rotated.Marker("banned@example.com")

	assert.Equal(t, activeHash, rotated.Markers("banned@example.com")[0])
	assert.Contains(t, rotated.Markers("banned@example.com"), oldHash,
		"lookups must try every configured previous key")
	assert.Equal(t, []string{activeKid, oldKid}, rotated.KeyIDs())
	assert.NotEqual(t, oldHash, activeHash, "a new ban is written under the active key")
}

func TestEmptyActiveKeyIsRejected(t *testing.T) {
	_, err := banhash.New("   ")
	require.ErrorIs(t, err, banhash.ErrKeyRequired)
}

func TestNewBanUsesTheActiveKey(t *testing.T) {
	oldKey := testKey("old", "pepper")
	newKey := testKey("new", "pepper")
	rotated, err := banhash.New(newKey, oldKey)
	require.NoError(t, err)
	oldHasher, err := banhash.New(oldKey)
	require.NoError(t, err)

	hash, kid := rotated.Marker("banned@example.com")
	oldHash, oldKid := oldHasher.Marker("banned@example.com")
	assert.NotEqual(t, oldHash, hash)
	assert.NotEqual(t, oldKid, kid)
}
