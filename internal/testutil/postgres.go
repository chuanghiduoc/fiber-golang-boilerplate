package testutil

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// AppRole is the non-superuser role integration tests run as, matching the
	// role the API uses in production. FORCE RLS subjects it to every tenant
	// policy.
	AppRole     = "app"
	appPassword = "app"
)

// OpenAppPool creates (or refreshes) the non-superuser app role, grants it
// access to schema, and returns a pool connected as that role. adminDSN is the
// privileged DSN that owns the schema.
//
// The pool's sessions default app.rls_bypass to on so fixtures and assertions
// that use the raw pool see every tenant. Application code always overrides
// both settings per statement (database.Scoped) or transaction (TxManager), so
// its queries stay isolated; TestRLS proves it.
func OpenAppPool(ctx context.Context, adminDSN, schema string) (*pgxpool.Pool, error) {
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parse admin pool config: %w", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		return nil, fmt.Errorf("connect as schema owner: %w", err)
	}
	defer admin.Close()

	role := pgx.Identifier{AppRole}.Sanitize()
	if _, err := admin.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+AppRole+`') THEN
			CREATE ROLE `+role+` LOGIN;
		END IF;
	END $$;`); err != nil {
		return nil, fmt.Errorf("create app role: %w", err)
	}
	if _, err := admin.Exec(ctx, `ALTER ROLE `+role+` WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '`+appPassword+`'`); err != nil {
		return nil, fmt.Errorf("configure app role: %w", err)
	}

	ident := pgx.Identifier{schema}.Sanitize()
	for _, statement := range []string{
		"GRANT USAGE ON SCHEMA " + ident + " TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA " + ident + " TO " + role,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + ident + " TO " + role,
		"GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA " + ident + " TO " + role,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			return nil, fmt.Errorf("grant app role: %w", err)
		}
	}

	u, err := url.Parse(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parse app DSN: %w", err)
	}
	u.User = url.UserPassword(AppRole, appPassword)
	poolCfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("parse app pool config: %w", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SELECT set_config('app.rls_bypass', 'on', false)`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect as app role: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping as app role: %w", err)
	}
	return pool, nil
}
