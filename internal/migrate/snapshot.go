package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// defaultSnapshotPath is the committed, normalized catalog dump every
// migration run is compared against. Regenerate it with
// `go run ./cmd/migrate -write-snapshot` after an intentional schema change.
const DefaultSnapshotPath = "migrations/schema.snapshot.sql"

// defaultTenantSlug identifies the seeded default tenant row the baseline
// creates. Its id is tenant.DefaultID in Go and the hard-coded constant in the
// seed. It is a real tenant, not a fallback.
const defaultTenantSlug = "default"

// ensureAuditPartitionsSQL creates the current and next UTC month partitions
// for the audit trail. It is idempotent and runs after every migration, so a
// long-lived deployment keeps writing into a matching partition. The dynamic
// identifiers live inside the DO block, so the statement this process sends is
// a compile-time constant. Retention is a reviewed maintenance path: detach an
// old partition (ALTER TABLE ... DETACH PARTITION), then drop it; audit rows
// are never DELETE'd.
const ensureAuditPartitionsSQL = `
DO $$
DECLARE
    target_schema TEXT := current_schema();
    offset_month INTEGER;
    partition_name TEXT;
    range_start DATE;
    range_end DATE;
BEGIN
    IF target_schema IS NULL THEN
        RAISE EXCEPTION 'cannot create audit partitions: current_schema() is NULL';
    END IF;
    FOR offset_month IN 0..1 LOOP
        range_start := (date_trunc('month', now() AT TIME ZONE 'utc')
            + make_interval(months => offset_month))::date;
        range_end := (range_start + make_interval(months => 1))::date;
        partition_name := 'audit_events_' || to_char(range_start, 'YYYYMM');
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I.%I PARTITION OF %I.audit_events FOR VALUES FROM (%L) TO (%L)',
            target_schema, partition_name, target_schema, range_start, range_end);
        -- A direct read of a partition skips the parent's policy, so every
        -- partition carries the same tenant isolation.
        EXECUTE format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', target_schema, partition_name);
        EXECUTE format('ALTER TABLE %I.%I FORCE ROW LEVEL SECURITY', target_schema, partition_name);
        IF NOT EXISTS (
            SELECT 1 FROM pg_policies
            WHERE schemaname = target_schema AND tablename = partition_name AND policyname = 'tenant_isolation'
        ) THEN
            EXECUTE format('CREATE POLICY tenant_isolation ON %I.%I USING (app_tenant_visible(tenant_id)) WITH CHECK (app_tenant_visible(tenant_id))',
                target_schema, partition_name);
        END IF;
    END LOOP;
END;
$$`

// ensureAuditPartitions creates this month's and next month's audit_events
// partitions. A failure is fatal: without the next month's partition, writes
// at the month boundary would land in the default partition instead.
func EnsureAuditPartitions(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, ensureAuditPartitionsSQL); err != nil {
		return fmt.Errorf("ensure audit_events partitions: %w", err)
	}
	return nil
}

// snapshotEntry is one normalized catalog object.
type snapshotEntry struct {
	Kind       string
	Name       string
	Definition string
}

// snapshotSQL dumps the app schema as deterministic (kind, name, definition)
// rows. It deliberately excludes schema_migrations (golang-migrate's ledger)
// and every partition child: monthly partitions are created by cmd/migrate and
// must not make the committed snapshot rot. Definition text is JSON-encoded by
// the writer so it can contain newlines.
const snapshotSQL = `
SELECT * FROM (
    SELECT 'table' AS kind, c.relname::text AS name,
           'relkind=' || c.relkind::text AS definition
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = $1
      AND c.relkind IN ('r', 'p')
      AND NOT c.relispartition
      AND c.relname <> 'schema_migrations'
    UNION ALL
    SELECT 'column', c.relname::text || '.' || a.attname::text,
           format_type(a.atttypid, a.atttypmod)
           || ' notnull=' || a.attnotnull::text
           || ' default=' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
           || ' identity=' || a.attidentity::text
    FROM pg_attribute a
    JOIN pg_class c ON c.oid = a.attrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
    WHERE n.nspname = $1
      AND c.relkind IN ('r', 'p')
      AND NOT c.relispartition
      AND c.relname <> 'schema_migrations'
      AND a.attnum > 0
      AND NOT a.attisdropped
    UNION ALL
    SELECT 'constraint', c.relname::text || '.' || con.conname::text,
           pg_get_constraintdef(con.oid)
    FROM pg_constraint con
    JOIN pg_class c ON c.oid = con.conrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = $1
      AND NOT c.relispartition
      AND c.relname <> 'schema_migrations'
    UNION ALL
    SELECT 'index', ic.relname, pg_get_indexdef(i.indexrelid)
    FROM pg_index i
    JOIN pg_class ic ON ic.oid = i.indexrelid
    JOIN pg_class tc ON tc.oid = i.indrelid
    JOIN pg_namespace n ON n.oid = tc.relnamespace
    WHERE n.nspname = $1
      AND NOT tc.relispartition
      AND tc.relname <> 'schema_migrations'
    UNION ALL
    SELECT 'trigger', c.relname::text || '.' || tg.tgname::text, pg_get_triggerdef(tg.oid)
    FROM pg_trigger tg
    JOIN pg_class c ON c.oid = tg.tgrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = $1
      AND NOT tg.tgisinternal
      AND tg.tgparentid = 0
      AND NOT c.relispartition
    UNION ALL
    SELECT 'policy', c.relname::text || '.' || p.polname::text,
           'cmd=' || p.polcmd::text
           || ' permissive=' || p.polpermissive::text
           || ' using=' || COALESCE(pg_get_expr(p.polqual, p.polrelid), '')
           || ' check=' || COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '')
    FROM pg_policy p
    JOIN pg_class c ON c.oid = p.polrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = $1
      AND NOT c.relispartition
    UNION ALL
    SELECT 'rls', c.relname,
           'enabled=' || c.relrowsecurity::text || ' forced=' || c.relforcerowsecurity::text
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = $1
      AND c.relkind IN ('r', 'p')
      AND NOT c.relispartition
      AND c.relname <> 'schema_migrations'
    UNION ALL
    SELECT 'function', p.proname::text || '(' || pg_get_function_identity_arguments(p.oid) || ')',
           pg_get_functiondef(p.oid)
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = $1
      AND NOT EXISTS (
          SELECT 1 FROM pg_depend d
          WHERE d.objid = p.oid AND d.deptype = 'e'
      )
) objects
ORDER BY kind, name`

// schemaSnapshot reads the normalized catalog dump.
// normalizeDefinition removes the database schema name from catalog text so a
// snapshot generated in one schema compares equal in another (tests run in a
// per-run schema). Schema qualification is irrelevant to the shape the guard
// verifies. Only whole identifiers are replaced: a column such as public_key
// must not be mangled because the schema happens to be named "public".
func normalizeDefinition(schema, definition string) string {
	if schema == "" {
		return definition
	}
	// Qualified names lose their schema qualifier entirely (the old snapshot
	// format), then any remaining standalone schema identifier (for example a
	// function's search_path) becomes the <schema> placeholder.
	definition = strings.ReplaceAll(definition, schema+".", "")
	var out strings.Builder
	for i := 0; i < len(definition); {
		index := strings.Index(definition[i:], schema)
		if index < 0 {
			out.WriteString(definition[i:])
			break
		}
		index += i
		out.WriteString(definition[i:index])
		end := index + len(schema)
		if isSchemaBoundary(definition, index, end) {
			out.WriteString("<schema>")
		} else {
			out.WriteString(schema)
		}
		i = end
	}
	return out.String()
}

// isSchemaBoundary reports whether definition[start:end] is a standalone
// identifier rather than a substring of a longer one.
func isSchemaBoundary(definition string, start, end int) bool {
	if start > 0 && isIdentifierByte(definition[start-1]) {
		return false
	}
	return end >= len(definition) || !isIdentifierByte(definition[end])
}

func isIdentifierByte(value byte) bool {
	return value == '_' ||
		(value >= 'a' && value <= 'z') ||
		(value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9')
}

func schemaSnapshot(ctx context.Context, pool *pgxpool.Pool, schema string) ([]snapshotEntry, error) {
	rows, err := pool.Query(ctx, snapshotSQL, schema)
	if err != nil {
		return nil, fmt.Errorf("read schema snapshot: %w", err)
	}
	defer rows.Close()

	var entries []snapshotEntry
	for rows.Next() {
		var entry snapshotEntry
		if err := rows.Scan(&entry.Kind, &entry.Name, &entry.Definition); err != nil {
			return nil, fmt.Errorf("read schema snapshot: scan row: %w", err)
		}
		entry.Definition = normalizeDefinition(schema, entry.Definition)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema snapshot: %w", err)
	}
	return entries, nil
}

// writeSchemaSnapshot writes the committed snapshot file.
func WriteSchemaSnapshot(ctx context.Context, pool *pgxpool.Pool, schema, path string) error {
	entries, err := schemaSnapshot(ctx, pool, schema)
	if err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString("-- Normalized PostgreSQL catalog snapshot of the migrated schema.\n")
	out.WriteString("-- Generated by `go run ./cmd/migrate -write-snapshot`; do not edit by hand.\n")
	out.WriteString("-- Format: <kind>\\t<name>\\t<json-encoded definition>\n")
	for _, entry := range entries {
		encoded, err := json.Marshal(entry.Definition)
		if err != nil {
			return fmt.Errorf("encode snapshot entry %s %s: %w", entry.Kind, entry.Name, err)
		}
		fmt.Fprintf(&out, "%s\t%s\t%s\n", entry.Kind, entry.Name, encoded)
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		return fmt.Errorf("write schema snapshot %s: %w", path, err)
	}
	return nil
}

// readSchemaSnapshot parses the committed snapshot file.
func readSchemaSnapshot(path string) (map[string]snapshotEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema snapshot %s: %w", path, err)
	}
	entries := make(map[string]snapshotEntry)
	for number, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("schema snapshot %s:%d is malformed", path, number+1)
		}
		var definition string
		if err := json.Unmarshal([]byte(fields[2]), &definition); err != nil {
			return nil, fmt.Errorf("schema snapshot %s:%d has an invalid definition: %w", path, number+1, err)
		}
		entry := snapshotEntry{Kind: fields[0], Name: fields[1], Definition: definition}
		entries[entry.Kind+"\x00"+entry.Name] = entry
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("schema snapshot %s is empty", path)
	}
	return entries, nil
}

// checkSnapshot diffs the live schema against the committed snapshot. The
// report is bounded so a badly broken database does not print thousands of
// lines; it names the first differences and the total count.
func checkSnapshot(ctx context.Context, pool *pgxpool.Pool, schema, path string) ([]string, error) {
	want, err := readSchemaSnapshot(path)
	if err != nil {
		return nil, err
	}
	live, err := schemaSnapshot(ctx, pool, schema)
	if err != nil {
		return nil, err
	}
	got := make(map[string]snapshotEntry, len(live))
	for _, entry := range live {
		got[entry.Kind+"\x00"+entry.Name] = entry
	}

	var gaps []string
	for key, expected := range want {
		actual, ok := got[key]
		switch {
		case !ok:
			gaps = append(gaps, "missing "+expected.Kind+" "+expected.Name)
		case actual.Definition != expected.Definition:
			gaps = append(gaps, "changed "+expected.Kind+" "+expected.Name)
		}
		delete(got, key)
	}
	for _, extra := range got {
		gaps = append(gaps, "unexpected "+extra.Kind+" "+extra.Name)
	}
	slices.Sort(gaps)
	return gaps, nil
}

// verifySchema fails loudly when the database drifted from the committed
// snapshot or lost a semantic guard. The snapshot comparison covers tables,
// columns, constraints, indexes, triggers, policies, RLS flags, and functions;
// the remaining checks are behavioral (the RLS predicate, trigger enablement,
// and the seeded default tenant).
func VerifySchema(ctx context.Context, pool *pgxpool.Pool, schema, snapshotPath string) error {
	checks := []func() ([]string, error){
		func() ([]string, error) { return checkSnapshot(ctx, pool, schema, snapshotPath) },
		func() ([]string, error) { return checkTenantColumnDefaults(ctx, pool, schema) },
		func() ([]string, error) { return checkRLS(ctx, pool, schema) },
		func() ([]string, error) { return checkRLSPredicate(ctx, pool, schema) },
		func() ([]string, error) { return checkTriggers(ctx, pool, schema) },
		func() ([]string, error) { return checkDefaultTenant(ctx, pool) },
	}
	var missing []string
	for _, check := range checks {
		gaps, err := check()
		if err != nil {
			return err
		}
		missing = append(missing, gaps...)
	}
	if len(missing) > 0 {
		const maxReported = 20
		total := len(missing)
		if len(missing) > maxReported {
			missing = missing[:maxReported]
			missing = append(missing, fmt.Sprintf("... and %d more", total-maxReported))
		}
		return fmt.Errorf(
			"schema does not match %s (%d difference(s): %s); regenerate the snapshot only for an intentional schema change",
			snapshotPath, total, strings.Join(missing, ", "),
		)
	}
	return nil
}

// checkDefaultTenant reports a missing or renumbered default tenant. The
// default tenant is a real seeded tenant (the default slug's tenant), not a
// fallback for rows that omit tenant_id.
func checkDefaultTenant(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var tenantID uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id FROM tenants WHERE slug = $1`, defaultTenantSlug).Scan(&tenantID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return []string{"default tenant row (slug default)"}, nil
	case err != nil:
		return nil, fmt.Errorf("verify default tenant: %w", err)
	case tenantID != tenant.DefaultID:
		return []string{"default tenant id " + tenant.DefaultID.String()}, nil
	}
	return nil, nil
}
