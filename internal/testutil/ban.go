//go:build integration

package testutil

import (
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
)

// BanHasher returns a keyed ban-marker hasher for tests that wire identity
// repositories directly. Build the key at runtime so it is not a
// password-like literal.
func BanHasher(t testing.TB) *banhash.Hasher {
	t.Helper()
	return MustBanHasher()
}

// MustBanHasher is BanHasher for helpers that have no *testing.T. The key is a
// constant, so failure is a programming error.
func MustBanHasher() *banhash.Hasher {
	hasher, err := banhash.New(strings.Join([]string{"integration", "test", "ban", "pepper"}, "-"))
	if err != nil {
		panic("testutil: create ban hasher: " + err.Error())
	}
	return hasher
}
