//go:build integration

package testutil

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// SetupTestDB is pgtest.SetupTestDB plus the RBAC catalog sync the API runs at
// startup: tests call services and routes directly, so the code-owned catalog
// must reach the database before any session loads its snapshot. Platform
// packages that identity imports use pgtest directly to avoid an import cycle.
func SetupTestDB(ctx context.Context) (*pgxpool.Pool, func(), error) {
	pool, cleanup, err := pgtest.SetupTestDB(ctx)
	if err != nil {
		return nil, nil, err
	}
	stores := identityrepo.NewStores(pool)
	tx := identityrepo.NewTransactor(pgtest.NewTxManager(pool), nil, nil, nil)
	if err := identityservice.SyncPermissions(ctx, tx, stores.Roles); err != nil {
		cleanup()
		return nil, nil, err
	}
	return pool, cleanup, nil
}

// The pgtest helpers, re-exported for module and end-to-end tests.
var (
	OpenAppPool  = pgtest.OpenAppPool
	Scoped       = pgtest.Scoped
	NewTxManager = pgtest.NewTxManager
	BypassScoped = pgtest.BypassScoped
	Bypass       = pgtest.Bypass
)
