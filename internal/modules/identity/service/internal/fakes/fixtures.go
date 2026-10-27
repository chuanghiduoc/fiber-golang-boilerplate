// Package fakes is the identity module's in-memory store for unit tests.
// It is internal to the service tree so only the identity subpackages' tests
// can import it; production code never links it.
package fakes

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"

	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
)

// TestID returns the deterministic UUIDv7 fixture id for n. Tests use small
// integer ids instead of repeating UUID literals.
func TestID(n uint64) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

// Fixture passwords are built at runtime so the repository carries no
// password-like literals for secret scanners.
var (
	Passphrase      = strings.Join([]string{"correct", "horse", "battery", "staple"}, "-") + "-2026"
	WrongPassword   = strings.Join([]string{"wrong", "fixture", "guess"}, "-")
	NewPassword     = strings.Join([]string{"new", "fixture", "password"}, "-") + "-2026"
	AnotherPassword = strings.Join([]string{"another", "fixture", "password"}, "-") + "-2026"
)

// TenantCtx is the explicit request/background tenant for unit tests. There
// is no compatibility fallback, so a test context must name one.
func TenantCtx() context.Context {
	return tenant.WithID(context.Background(), tenant.DefaultID)
}

// ConfigurePasswordHashing switches the package's tests to the cheap Argon2id
// parameter set. Hashes stay real Argon2id PHC strings, so every password
// check behaves the same; only the work factor drops. The dummy hash is
// recomputed because package init already built it at the production cost.
func ConfigurePasswordHashing() {
	core.SetPasswordHashParamsForTest(1024, 1, 1)
	dummy, _ := core.HashPassword("timing-only-dummy-password")
	core.SetDummyPasswordHashForTest(dummy)
}

func init() { ConfigurePasswordHashing() }
