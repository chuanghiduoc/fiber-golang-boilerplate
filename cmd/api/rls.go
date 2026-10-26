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
//   - a table owner (or a member of the owner role, which can SET ROLE to it)
//     may ALTER TABLE ... NO FORCE ROW LEVEL SECURITY, which removes the last
//     guard FORCE adds;
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
		reasons = append(reasons, "owns tables in the configured schema (directly or as a member of their owner)")
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
          AND pg_has_role(current_user, c.relowner, 'MEMBER')
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

// bypassRoleState is what PostgreSQL reports about the dedicated cross-tenant
// role. It must be the inverse of the normal role: BYPASSRLS yes, superuser
// no, owns nothing, and unreachable through SET ROLE from the normal role.
type bypassRoleState struct {
	Role           string
	BypassRLS      bool
	Superuser      bool
	OwnsTables     bool
	NormalIsMember bool
}

// unsafe reports whether the bypass role is misconfigured.
func (s bypassRoleState) unsafe() bool {
	return !s.BypassRLS || s.Superuser || s.OwnsTables || s.NormalIsMember
}

// reasons describes every detected problem, so an operator can fix the role.
func (s bypassRoleState) reasons() []string {
	reasons := make([]string, 0, 4)
	if !s.BypassRLS {
		reasons = append(reasons, "does not have BYPASSRLS")
	}
	if s.Superuser {
		reasons = append(reasons, "is a superuser")
	}
	if s.OwnsTables {
		reasons = append(reasons, "owns tables in the configured schema (directly or as a member of their owner)")
	}
	if s.NormalIsMember {
		reasons = append(reasons, "is reachable with SET ROLE from the normal role")
	}
	return reasons
}

// bypassRoleQuery evaluates the bypass role's own attributes in one round
// trip. Membership is checked separately, as the normal role, below.
const bypassRoleQuery = `
WITH me AS (
    SELECT oid, rolsuper, rolbypassrls
    FROM pg_roles
    WHERE rolname = current_user
)
SELECT
    current_user,
    me.rolbypassrls,
    me.rolsuper,
    EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = $1
          AND pg_has_role(current_user, c.relowner, 'MEMBER')
          AND c.relkind IN ('r', 'p')
    )
FROM me`

// normalIsBypassMember reports whether the normal role can SET ROLE to the
// bypass role.
const normalIsBypassMember = `SELECT pg_has_role(current_user, $1, 'MEMBER')`

// checkBypassRLSRole verifies the dedicated cross-tenant role can bypass RLS
// but cannot do more than that, and that the normal role cannot become it.
// Production refuses on any violation; elsewhere the risk is logged.
func checkBypassRLSRole(ctx context.Context, bypass, normal *pgxpool.Pool, schema, env string) error {
	if bypass == nil {
		message := "DB_BYPASS_USERNAME is not configured: cross-tenant paths (job claim, retention, catalog sync) run on the normal role and only work if it bypasses RLS"
		slog.Warn(message)
		if env == "production" {
			return errors.New("DB_BYPASS_USERNAME is required in production")
		}
		return nil
	}

	var state bypassRoleState
	if err := bypass.QueryRow(ctx, bypassRoleQuery, schema).Scan(
		&state.Role, &state.BypassRLS, &state.Superuser, &state.OwnsTables,
	); err != nil {
		return fmt.Errorf("check database bypass role: %w", err)
	}
	if err := normal.QueryRow(ctx, normalIsBypassMember, state.Role).Scan(&state.NormalIsMember); err != nil {
		return fmt.Errorf("check bypass role membership: %w", err)
	}
	if !state.unsafe() {
		return nil
	}
	slog.Error("database bypass role is misconfigured; use a dedicated BYPASSRLS role that owns no tables and is not reachable from the normal role",
		slog.String("role", state.Role),
		slog.String("reasons", strings.Join(state.reasons(), ", ")),
	)
	if env == "production" {
		return errors.New("database bypass role is misconfigured (" +
			strings.Join(state.reasons(), "; ") +
			"); use a dedicated BYPASSRLS role that is not a superuser, owns no tables, and is not reachable with SET ROLE from the normal role")
	}
	return nil
}
