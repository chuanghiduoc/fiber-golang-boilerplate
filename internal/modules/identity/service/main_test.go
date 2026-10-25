package service

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestMain lowers the bcrypt cost for this package's unit and integration
// tests. Hashes stay valid bcrypt, so every password check behaves the same;
// only the work factor drops. The dummy hash is recomputed because package
// init already built it at the production cost.
func TestMain(m *testing.M) {
	bcryptCost = bcrypt.MinCost
	dummyPasswordHash, _ = bcrypt.GenerateFromPassword([]byte("timing-only-dummy-password"), bcryptCost)
	os.Exit(m.Run())
}
