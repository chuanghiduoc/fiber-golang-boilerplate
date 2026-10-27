package pgtest

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// Scoped returns the DBTX the application uses: every statement carries the
// RLS scope resolved from its context. The resolver is fail-closed exactly like
// app.RLSResolver: only a tenant stored with tenant.WithID is in scope, so a
// test that forgets one sees no rows instead of borrowing a tenant. The
// pool is the normal app role, except for a WithRLSBypass context, which uses
// the dedicated bypass pool.
func Scoped(pools database.Pools) *database.Scoped {
	return database.NewScopedPools(pools, database.WithResolver(database.Resolver(tenant.IDFromContext)))
}

// NewTxManager returns a transaction manager with the application's resolver
// (fail-closed, like app.RLSResolver).
func NewTxManager(pools database.Pools) *database.TxManager {
	return database.NewTxManagerPools(pools, database.WithResolver(database.Resolver(tenant.IDFromContext)))
}

// BypassScoped returns a DBTX for a deliberately cross-tenant store (the
// feature-flag snapshot reads every tenant's targets).
func BypassScoped(pools database.Pools) *database.Scoped {
	return database.NewScopedPools(pools, database.WithResolver(func(context.Context) database.Scope {
		return database.Scope{Bypass: true}
	}))
}

// Bypass marks ctx as a deliberate cross-tenant operation.
func Bypass(ctx context.Context) context.Context {
	return database.WithRLSBypass(ctx)
}
