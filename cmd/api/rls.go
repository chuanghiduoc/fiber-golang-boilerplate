package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// checkRLSRole rejects a database role PostgreSQL exempts from row-level
// security. FORCE makes the table owner subject to its policies, so the owner
// role is fine; a superuser or a BYPASSRLS role is not. Production refuses to
// start; elsewhere the error is logged so local development can use postgres.
func checkRLSRole(ctx context.Context, pool *pgxpool.Pool, env string) error {
	var unsafe bool
	if err := pool.QueryRow(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&unsafe); err != nil {
		return fmt.Errorf("check database role for row-level security: %w", err)
	}
	if err := rlsRoleError(unsafe, env); err != nil {
		slog.Error("database role bypasses row-level security; connect the API and worker as a dedicated non-superuser role")
		return err
	}
	return nil
}

// rlsRoleError is the production gate for a role that bypasses RLS.
func rlsRoleError(unsafe bool, env string) error {
	if unsafe && env == "production" {
		return errors.New("database role is a superuser or has BYPASSRLS; use a dedicated non-superuser role")
	}
	return nil
}
