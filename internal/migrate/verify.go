// Package migrate owns the read-only schema verifier that cmd/migrate runs
// after applying migrations: a normalized catalog snapshot diff plus the
// behavioral guards the snapshot cannot express (RLS, triggers, defaults).
package migrate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var requiredTriggers = map[string][]string{
	"memberships":               {"trigger_memberships_last_owner"},
	"membership_roles":          {"trigger_membership_roles_scope", "trigger_protect_last_owner"},
	"platform_role_assignments": {"trigger_platform_role_scope"},
	"role_permissions":          {"trigger_role_permissions_scope"},
	"invitations":               {"trigger_invitations_role_scope"},
	"data_exports":              {"trigger_data_exports_updated_at"},
	"webauthn_credentials":      {"trigger_webauthn_credentials_updated_at"},
	"audit_events": {
		"trigger_audit_events_completion",
		"trigger_audit_events_no_delete",
		"trigger_audit_events_no_truncate",
	},
}

// requiredRLS is the expected set of tenant-owned tables. checkRLS discovers
// every table in the schema with a tenant_id column dynamically; this list
// additionally catches an expected table whose tenant_id column disappeared
// (for example a hand-rebuilt schema) and keeps the baseline drift test honest.
var requiredRLS = []string{
	"tenant_settings",
	"memberships",
	"membership_roles",
	"roles",
	"invitations",
	"sessions",
	"refresh_tokens",
	"data_exports",
	"api_keys",
	"files",
	"feature_flag_targets",
	"notifications",
	"notification_preferences",
	"jobs",
	"audit_events",
	"webhook_endpoints",
	"webhook_endpoint_subscriptions",
	"webhook_endpoint_secrets",
	"webhook_events",
	"webhook_deliveries",
	"webhook_delivery_attempts",
	"webhook_inbound_sources",
	"webhook_inbound_source_secrets",
	"webhook_inbound_events",
}

type rlsPolicy struct {
	Name  string
	Using string
	Check string
}

// rlsTable is the catalog state of one table with a tenant_id column.
type rlsTable struct {
	Table    string
	Enabled  bool
	Forced   bool
	Policies []rlsPolicy
}

// checkRLS reports every table with a tenant_id column whose row-level
// security is not enabled and forced, or whose policies are not exactly one
// tenant_isolation policy calling app_tenant_visible. Partition children are
// excluded: a partition inherits its parent's RLS and policy, and enabling RLS
// on the parent is what protects every partition. The table set is
// discovered from the catalog rather than a static list, so a later migration
// cannot add a tenant-owned table that this guard silently ignores: TRUNCATE
// ignores RLS, but SELECT/INSERT/UPDATE/DELETE do not.
func checkRLS(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	var tables []rlsTable
	current := -1
	err := scanRows(ctx, pool, "verify row-level security", "verify schema: scan row-level security",
		func(rows pgx.Rows) error {
			var (
				table           string
				enabled, forced bool
				policy          string
				qual, withCheck string
			)
			if err := rows.Scan(&table, &enabled, &forced, &policy, &qual, &withCheck); err != nil {
				return err
			}
			if current < 0 || tables[current].Table != table {
				tables = append(tables, rlsTable{Table: table, Enabled: enabled, Forced: forced})
				current = len(tables) - 1
			}
			if policy != "" {
				tables[current].Policies = append(tables[current].Policies, rlsPolicy{
					Name:  policy,
					Using: qual,
					Check: withCheck,
				})
			}
			return nil
		},
		`SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		        COALESCE(p.polname, ''),
		        COALESCE(pg_get_expr(p.polqual, p.polrelid), ''),
		        COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '')
		 FROM pg_class c
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 LEFT JOIN pg_policy p ON p.polrelid = c.oid
		 WHERE n.nspname = $1
		   AND c.relkind IN ('r', 'p')
		   AND EXISTS (
		       SELECT 1 FROM pg_attribute a
		       WHERE a.attrelid = c.oid
		         AND a.attname = 'tenant_id'
		         AND a.attnum > 0
		         AND NOT a.attisdropped
		   )
		 ORDER BY c.relname, p.polname`,
		schema,
	)
	if err != nil {
		return nil, err
	}
	return rlsGaps(tables), nil
}

// rlsGaps turns the catalog state into operator-facing gaps. One problem per
// table keeps the report readable when a table is missing several guards.
func rlsGaps(tables []rlsTable) []string {
	var gaps []string
	seen := make(map[string]struct{}, len(tables))
	for _, table := range tables {
		seen[table.Table] = struct{}{}
		switch {
		case !table.Enabled:
			gaps = append(gaps, "row-level security on "+table.Table)
		case !table.Forced:
			gaps = append(gaps, "forced row-level security on "+table.Table)
		default:
			if gap := policyGap(table); gap != "" {
				gaps = append(gaps, gap)
			}
		}
	}
	for _, table := range requiredRLS {
		if _, ok := seen[table]; !ok {
			gaps = append(gaps, "row-level security on "+table)
		}
	}
	return gaps
}

// policyGap checks that the table has exactly one tenant_isolation policy and
// that both its USING and (when present) WITH CHECK clauses call
// app_tenant_visible. Any second policy (for example a USING (true) escape
// hatch) makes the table readable across tenants.
func policyGap(table rlsTable) string {
	switch len(table.Policies) {
	case 0:
		return "tenant_isolation policy on " + table.Table
	case 1:
	default:
		names := make([]string, len(table.Policies))
		for i, policy := range table.Policies {
			names[i] = policy.Name
		}
		slices.Sort(names)
		return "single tenant_isolation policy on " + table.Table +
			" (found " + strings.Join(names, ", ") + ")"
	}
	policy := table.Policies[0]
	if policy.Name != "tenant_isolation" {
		return "tenant_isolation policy on " + table.Table + " (found " + policy.Name + ")"
	}
	if !strings.Contains(policy.Using, "app_tenant_visible") {
		return "tenant_isolation policy on " + table.Table + " USING clause must reference app_tenant_visible"
	}
	if policy.Check != "" && !strings.Contains(policy.Check, "app_tenant_visible") {
		return "tenant_isolation policy on " + table.Table + " WITH CHECK clause must reference app_tenant_visible"
	}
	return ""
}

// checkRLSPredicate reads the installed app_tenant_visible definition. The
// function is the single policy predicate for every tenant-owned table, so a
// setting it reads is an isolation escape hatch visible to every role. The
// bypass role (BYPASSRLS) is the only cross-tenant mechanism; the predicate
// must not consult app.rls_bypass or any other application-writable setting.
func checkRLSPredicate(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	var definition string
	err := pool.QueryRow(ctx, `
		SELECT pg_get_functiondef(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.proname = 'app_tenant_visible' AND p.pronargs = 1`,
		schema).Scan(&definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{"function app_tenant_visible(UUID)"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("verify app_tenant_visible: %w", err)
	}
	return rlsPredicateGaps(definition), nil
}

// rlsPredicateGaps is the pure check behind checkRLSPredicate, so its cases
// are unit-testable without DDL.
func rlsPredicateGaps(definition string) []string {
	lowered := strings.ToLower(definition)
	var gaps []string
	if strings.Contains(lowered, "rls_bypass") {
		gaps = append(gaps, "app_tenant_visible must not read app.rls_bypass (use the dedicated BYPASSRLS role)")
	}
	if !strings.Contains(lowered, "app.tenant_id") {
		gaps = append(gaps, "app_tenant_visible must compare the row tenant with app.tenant_id")
	}
	if strings.Contains(lowered, "set search_path") {
		gaps = append(gaps, "app_tenant_visible must not SET search_path (it must stay inlinable into every policy)")
	}
	return gaps
}

// checkTriggers reports required triggers that are absent or disabled.
// Triggers enforce invariants the application does not re-check, so a
// disabled trigger is as bad as a missing one.
func checkTriggers(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	tables := sortedKeys(requiredTriggers)
	names := make([]string, 0, len(requiredTriggers))
	for _, table := range tables {
		names = append(names, requiredTriggers[table]...)
	}

	enabled := make(map[string]bool, len(names))
	err := scanRows(ctx, pool, "verify schema triggers", "verify schema: scan trigger",
		func(rows pgx.Rows) error {
			var table, trigger string
			var isEnabled bool
			if err := rows.Scan(&table, &trigger, &isEnabled); err != nil {
				return err
			}
			enabled[table+"."+trigger] = isEnabled
			return nil
		},
		`SELECT c.relname, tg.tgname, tg.tgenabled <> 'D'
		 FROM pg_trigger tg
		 JOIN pg_class c ON c.oid = tg.tgrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND NOT tg.tgisinternal AND tg.tgname = ANY($2)`,
		schema, names,
	)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, table := range tables {
		for _, trigger := range requiredTriggers[table] {
			isEnabled, ok := enabled[table+"."+trigger]
			switch {
			case !ok:
				missing = append(missing, "trigger "+table+"."+trigger)
			case !isEnabled:
				missing = append(missing, "enabled trigger "+table+"."+trigger)
			}
		}
	}
	return missing, nil
}

// checkTenantColumnDefaults reports tenant_id columns that still carry a
// default. A tenant-owned row must always state its tenant: a default would
// silently attribute a write that forgot one, and RLS WITH CHECK cannot catch
// a value the database supplied itself.
func checkTenantColumnDefaults(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT table_name, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = $1
		  AND column_name = 'tenant_id'
		  AND column_default IS NOT NULL
		ORDER BY table_name`, schema)
	if err != nil {
		return nil, fmt.Errorf("verify tenant_id defaults: %w", err)
	}
	defer rows.Close()
	var gaps []string
	for rows.Next() {
		var table, columnDefault string
		if err := rows.Scan(&table, &columnDefault); err != nil {
			return nil, fmt.Errorf("verify schema: scan tenant_id default: %w", err)
		}
		gaps = append(gaps, "tenant_id default on "+table+" ("+columnDefault+")")
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("verify tenant_id defaults: %w", err)
	}
	return gaps, nil
}

// checkDefaultTenant reports a missing or renumbered default tenant. The
func scanRows(ctx context.Context, pool *pgxpool.Pool, queryErr, scanErr string,
	scan func(pgx.Rows) error, sql string, args ...any,
) error {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", queryErr, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("%s: %w", scanErr, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: %w", queryErr, err)
	}
	return nil
}

// sortedKeys returns m's keys in order so the missing-object report is stable
// across runs.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
