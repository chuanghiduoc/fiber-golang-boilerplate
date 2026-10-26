package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rlsRoleState is what PostgreSQL reports about the connected role. Any one of
// these exempts or lets the role exempt itself from row-level security:
//
//   - a superuser or BYPASSRLS role ignores every policy;
//   - a table owner may ALTER TABLE ... NO FORCE ROW LEVEL SECURITY, which
//     removes the last guard FORCE adds;
//   - a member of a superuser/BYPASSRLS role can SET ROLE to it.
type rlsRoleState struct {
	Superuser      bool
	BypassRLS      bool
	OwnsTables     bool
	MemberOfExempt bool
}

// unsafe reports whether the role can read across tenants.
func (s rlsRoleState) unsafe() bool {
	return s.Superuser || s.BypassRLS || s.OwnsTables || s.MemberOfExempt
}

// reasons describes every detected risk, so an operator can fix the role
// instead of guessing which of the four checks fired.
func (s rlsRoleState) reasons() []string {
	reasons := make([]string, 0, 4)
	if s.Superuser {
		reasons = append(reasons, "is a superuser")
	}
	if s.BypassRLS {
		reasons = append(reasons, "has BYPASSRLS")
	}
	if s.OwnsTables {
		reasons = append(reasons, "owns tables in the configured schema")
	}
	if s.MemberOfExempt {
		reasons = append(reasons, "is a member of a superuser or BYPASSRLS role")
	}
	return reasons
}

// rlsRoleQuery evaluates the four risks in one round trip. The configured
// schema is a parameter because current_schema() returns NULL for a role that
// lacks USAGE on its search_path schemas, which would hide table ownership.
const rlsRoleQuery = `
WITH me AS (
    SELECT oid, rolsuper, rolbypassrls
    FROM pg_roles
    WHERE rolname = current_user
)
SELECT
    current_user,
    me.rolsuper,
    me.rolbypassrls,
    EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = $1
          AND c.relowner = me.oid
          AND c.relkind IN ('r', 'p')
    ),
    EXISTS (
        SELECT 1
        FROM pg_roles r
        WHERE r.oid <> me.oid
          AND NOT me.rolsuper
          AND (r.rolsuper OR r.rolbypassrls)
          AND pg_has_role(current_user, r.oid, 'MEMBER')
    )
FROM me`

// checkRLSRole rejects a database role PostgreSQL exempts from row-level
// security. FORCE makes the table owner subject to its policies, but the owner
// can turn FORCE off, and a member of a superuser/BYPASSRLS role can SET ROLE.
// Production refuses to start; elsewhere the risk is logged so local
// development can still use postgres.
func checkRLSRole(ctx context.Context, pool *pgxpool.Pool, schema, env string) error {
	var (
		role  string
		state rlsRoleState
	)
	if err := pool.QueryRow(ctx, rlsRoleQuery, schema).Scan(
		&role, &state.Superuser, &state.BypassRLS, &state.OwnsTables, &state.MemberOfExempt,
	); err != nil {
		return fmt.Errorf("check database role for row-level security: %w", err)
	}
	if !state.unsafe() {
		return nil
	}
	slog.Error("database role bypasses row-level security; connect the API and worker as a dedicated non-superuser role",
		slog.String("role", role),
		slog.String("reasons", strings.Join(state.reasons(), ", ")),
	)
	return rlsRoleError(state, env)
}

// rlsRoleError is the production gate for a role that bypasses RLS.
func rlsRoleError(state rlsRoleState, env string) error {
	if env == "production" && state.unsafe() {
		return errors.New("database role bypasses row-level security (" +
			strings.Join(state.reasons(), "; ") +
			"); use a dedicated non-superuser role that owns no tables and is not a member of a role that bypasses RLS")
	}
	return nil
}
