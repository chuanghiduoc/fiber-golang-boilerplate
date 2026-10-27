//go:build integration

package testutil

import (
	"context"
	"testing"

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// NewFixture is pgtest.New plus the RBAC catalog sync the API runs at startup:
// tests call services and routes directly, so the code-owned catalog must reach
// the database before any session loads its snapshot. Platform packages that
// identity imports use pgtest.New directly to avoid an import cycle.
func NewFixture(t testing.TB) *pgtest.Fixture {
	t.Helper()
	fixture := pgtest.New(t)
	stores := identityrepo.NewStores(fixture.Bypass)
	tx := identityrepo.NewTransactor(fixture.Tx(), nil, nil, nil)
	if err := identityservice.SyncPermissions(context.Background(), tx, stores.Roles, Catalogs().Permissions); err != nil {
		t.Fatalf("sync permission catalog: %v", err)
	}
	return fixture
}

// The pgtest helpers, re-exported for module and end-to-end tests.
var (
	OpenAppPools = pgtest.OpenAppPools
	Scoped       = pgtest.Scoped
	NewTxManager = pgtest.NewTxManager
	BypassScoped = pgtest.BypassScoped
	Bypass       = pgtest.Bypass
)
