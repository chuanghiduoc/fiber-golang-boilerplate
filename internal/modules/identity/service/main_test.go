package service

import (
	"context"

	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// testTenantCtx is the explicit request/background tenant for unit tests.
// There is no compatibility fallback, so a test context must name one.
func testTenantCtx() context.Context {
	return tenant.WithID(context.Background(), tenant.DefaultID)
}

// init switches this package's tests to the cheap Argon2id parameter set.
// Hashes stay real Argon2id PHC strings, so every password check behaves the
// same; only the work factor drops. The dummy hash is recomputed because
// package init already built it at the production cost.
func init() {
	core.SetPasswordHashParamsForTest(1024, 1, 1)
	dummy, _ := hashPassword("timing-only-dummy-password")
	core.SetDummyPasswordHashForTest(dummy)
}
