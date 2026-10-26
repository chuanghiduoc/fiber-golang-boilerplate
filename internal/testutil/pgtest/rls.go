package pgtest

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// Scoped returns the DBTX the application uses: every statement carries the
// RLS scope resolved from its context. The resolver is fail-closed exactly like
// app.RLSResolver: only a tenant stored with tenant.WithID is in scope, so a
// test that forgets one sees no rows instead of the compatibility tenant. The
// pool is the normal app role, except for a WithRLSBypass context, which uses
// the dedicated bypass pool.
func Scoped(pool *pgxpool.Pool) *database.Scoped {
	return database.NewScopedPools(poolsFor(pool), database.WithResolver(database.Resolver(tenant.IDFromContext)))
}

// NewTxManager returns a transaction manager with the application's resolver
// (fail-closed, like app.RLSResolver).
func NewTxManager(pool *pgxpool.Pool) *database.TxManager {
	return database.NewTxManagerPools(poolsFor(pool), database.WithResolver(database.Resolver(tenant.IDFromContext)))
}

// BypassScoped returns a DBTX for a deliberately cross-tenant store (the
// feature-flag snapshot reads every tenant's targets).
func BypassScoped(pool *pgxpool.Pool) *database.Scoped {
	return database.NewScopedPools(poolsFor(pool), database.WithResolver(func(context.Context) database.Scope {
		return database.Scope{Bypass: true}
	}))
}

// Bypass marks ctx as a deliberate cross-tenant operation.
func Bypass(ctx context.Context) context.Context {
	return database.WithRLSBypass(ctx)
}
