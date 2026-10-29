package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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

// auditPartition is one monthly range of the audit_events partitioned table.
type auditPartition struct {
	name  string
	start time.Time
	end   time.Time
}

// auditPartitionsAt returns the current and next UTC month. Creating the next
// month ahead of time keeps writes at the boundary out of the default
// partition.
func auditPartitionsAt(now time.Time) []auditPartition {
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return []auditPartition{
		newAuditPartition(month),
		newAuditPartition(month.AddDate(0, 1, 0)),
	}
}

func newAuditPartition(start time.Time) auditPartition {
	return auditPartition{
		name:  "audit_events_" + start.Format("200601"),
		start: start,
		end:   start.AddDate(0, 1, 0),
	}
}

// EnsureAuditPartitions creates this month's and next month's audit_events
// partitions. A failure is fatal: without the next month's partition, writes
// at the month boundary would land in the default partition instead.
//
// The whole operation is transactional and serialized with an advisory lock so
// concurrent deploy jobs cannot race. If a downtime skipped a month, the
// default partition can already contain rows for the range a new partition
// needs. PostgreSQL refuses to create that partition while those rows remain,
// so the default is rebuilt: detach it, keep its rows in a temp table, create
// the monthly partitions and a fresh default, then copy the rows back through
// the parent so each lands in the right partition. Retention remains a
// reviewed maintenance path: detach an old partition, then drop it; audit rows
// are never DELETE'd.
func EnsureAuditPartitions(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ensure audit_events partitions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('migrate.audit_partitions', 0))`); err != nil {
		return fmt.Errorf("ensure audit_events partitions: lock: %w", err)
	}

	var schema string
	if err := tx.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return fmt.Errorf("ensure audit_events partitions: current schema: %w", err)
	}
	if schema == "" {
		return errors.New("ensure audit_events partitions: current_schema() is NULL")
	}

	if err := ensureAuditPartitionsInTx(ctx, tx, schema, auditPartitionsAt(time.Now().UTC())); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ensure audit_events partitions: commit: %w", err)
	}
	return nil
}

func ensureAuditPartitionsInTx(ctx context.Context, tx pgx.Tx, schema string, partitions []auditPartition) error {
	conflict, err := auditDefaultPartitionHasRows(ctx, tx, schema, partitions)
	if err != nil {
		return err
	}
	if conflict {
		return rebuildAuditDefaultPartition(ctx, tx, schema, partitions)
	}
	for _, partition := range partitions {
		if err := createAuditPartition(ctx, tx, schema, partition); err != nil {
			return err
		}
	}
	return secureAuditPartition(ctx, tx, schema, "audit_events_default")
}

// auditDefaultPartitionHasRows reports whether a missing monthly partition's
// range already has rows in the default partition. FORCE RLS is cleared for
// the check because the migration role owns the table and must see every
// tenant's rows; the transaction restores it before commit.
func auditDefaultPartitionHasRows(ctx context.Context, tx pgx.Tx, schema string, partitions []auditPartition) (bool, error) {
	if err := setAuditPartitionForcedRLS(ctx, tx, schema, "audit_events_default", false); err != nil {
		return false, err
	}
	for _, partition := range partitions {
		exists, err := auditPartitionExists(ctx, tx, schema, partition.name)
		if err != nil {
			return false, err
		}
		if exists {
			continue
		}
		var hasRows bool
		query := fmt.Sprintf(
			`SELECT EXISTS (SELECT 1 FROM %s WHERE created_at >= $1 AND created_at < $2)`,
			qualifiedAuditTable(schema, "audit_events_default"),
		)
		if err := tx.QueryRow(ctx, query, partition.start, partition.end).Scan(&hasRows); err != nil {
			return false, fmt.Errorf("check audit_events_default for %s: %w", partition.name, err)
		}
		if hasRows {
			return true, nil
		}
	}
	return false, nil
}

// rebuildAuditDefaultPartition moves the default partition's rows aside while
// the missing monthly partitions are created. The old partition is dropped as
// a standalone table, so its index names do not collide with the new default's
// indexes. Rows are copied back through the parent, which routes them to the
// monthly partitions or the fresh default.
func rebuildAuditDefaultPartition(ctx context.Context, tx pgx.Tx, schema string, partitions []auditPartition) error {
	parent := qualifiedAuditTable(schema, "audit_events")
	defaultPartition := qualifiedAuditTable(schema, "audit_events_default")

	if err := setAuditPartitionForcedRLS(ctx, tx, schema, "audit_events_default", false); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`CREATE TEMP TABLE audit_events_default_backup ON COMMIT DROP AS SELECT * FROM %s`,
		defaultPartition,
	)); err != nil {
		return fmt.Errorf("back up audit_events_default: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, defaultPartition)); err != nil {
		return fmt.Errorf("drop audit_events_default: %w", err)
	}
	for _, partition := range partitions {
		if err := createAuditPartition(ctx, tx, schema, partition); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF %s DEFAULT`, defaultPartition, parent)); err != nil {
		return fmt.Errorf("recreate audit_events_default: %w", err)
	}
	if err := secureAuditPartition(ctx, tx, schema, "audit_events_default"); err != nil {
		return err
	}

	// Copying through the parent exercises its FORCE RLS policy. The migration
	// role owns it, so clearing FORCE (only inside this transaction) lets every
	// tenant's rows be restored. Tuple routing may also check the destination
	// partition's policy, so clear FORCE on every possible target, not just the
	// parent, and restore it afterwards.
	targets := append([]auditPartition(nil), partitions...)
	for _, partition := range targets {
		if err := setAuditPartitionForcedRLS(ctx, tx, schema, partition.name, false); err != nil {
			return err
		}
	}
	if err := setAuditPartitionForcedRLS(ctx, tx, schema, "audit_events_default", false); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s NO FORCE ROW LEVEL SECURITY`, parent)); err != nil {
		return fmt.Errorf("open audit_events for default-partition restore: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s SELECT * FROM audit_events_default_backup`, parent)); err != nil {
		return fmt.Errorf("restore audit_events_default rows: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, parent)); err != nil {
		return fmt.Errorf("restore audit_events row-level security: %w", err)
	}
	for _, partition := range targets {
		if err := secureAuditPartition(ctx, tx, schema, partition.name); err != nil {
			return err
		}
	}
	if err := secureAuditPartition(ctx, tx, schema, "audit_events_default"); err != nil {
		return err
	}
	return nil
}

func createAuditPartition(ctx context.Context, tx pgx.Tx, schema string, partition auditPartition) error {
	parent := qualifiedAuditTable(schema, "audit_events")
	target := qualifiedAuditTable(schema, partition.name)
	statement := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		target,
		parent,
		partition.start.Format("2006-01-02"),
		partition.end.Format("2006-01-02"),
	)
	if _, err := tx.Exec(ctx, statement); err != nil {
		return fmt.Errorf("create audit partition %s: %w", partition.name, err)
	}
	attached, err := auditPartitionAttached(ctx, tx, schema, partition.name)
	if err != nil {
		return err
	}
	if !attached {
		return fmt.Errorf("audit partition %s exists but is not attached to audit_events", partition.name)
	}
	return secureAuditPartition(ctx, tx, schema, partition.name)
}

func secureAuditPartition(ctx context.Context, tx pgx.Tx, schema, table string) error {
	target := qualifiedAuditTable(schema, table)
	for _, statement := range []string{
		fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, target),
		fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, target),
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("secure audit partition %s: %w", table, err)
		}
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_policies
		WHERE schemaname = $1 AND tablename = $2 AND policyname = 'tenant_isolation'
	)`, schema, table).Scan(&exists); err != nil {
		return fmt.Errorf("inspect audit partition policy %s: %w", table, err)
	}
	if exists {
		return nil
	}
	statement := fmt.Sprintf(
		`CREATE POLICY tenant_isolation ON %s USING (app_tenant_visible(tenant_id)) WITH CHECK (app_tenant_visible(tenant_id))`,
		target,
	)
	if _, err := tx.Exec(ctx, statement); err != nil {
		return fmt.Errorf("create audit partition policy %s: %w", table, err)
	}
	return nil
}

func setAuditPartitionForcedRLS(ctx context.Context, tx pgx.Tx, schema, table string, forced bool) error {
	action := "NO FORCE"
	if forced {
		action = "FORCE"
	}
	statement := fmt.Sprintf(`ALTER TABLE %s %s ROW LEVEL SECURITY`, qualifiedAuditTable(schema, table), action)
	if _, err := tx.Exec(ctx, statement); err != nil {
		return fmt.Errorf("set audit partition RLS %s: %w", table, err)
	}
	return nil
}

func auditPartitionExists(ctx context.Context, tx pgx.Tx, schema, table string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
	)`, schema, table).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspect audit partition %s: %w", table, err)
	}
	return exists, nil
}

func auditPartitionAttached(ctx context.Context, tx pgx.Tx, schema, table string) (bool, error) {
	var attached bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM pg_inherits i
		JOIN pg_class child ON child.oid = i.inhrelid
		JOIN pg_class parent ON parent.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = child.relnamespace
		JOIN pg_namespace pn ON pn.oid = parent.relnamespace
		WHERE n.nspname = $1 AND child.relname = $2
		  AND pn.nspname = $1 AND parent.relname = 'audit_events'
	)`, schema, table).Scan(&attached); err != nil {
		return false, fmt.Errorf("inspect audit partition attachment %s: %w", table, err)
	}
	return attached, nil
}

func qualifiedAuditTable(schema, table string) string {
	return pgx.Identifier{schema, table}.Sanitize()
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
	if err := writeSnapshotFile(path, []byte(out.String())); err != nil {
		return fmt.Errorf("write schema snapshot %s: %w", path, err)
	}
	return nil
}

// writeSnapshotFile replaces the snapshot atomically so an interrupted
// regeneration cannot leave a truncated file that the migration guard reads as
// schema drift. The temporary file must share the destination directory for
// rename to be atomic on the same filesystem.
func writeSnapshotFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTmp = false
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
		key := entry.Kind + "\x00" + entry.Name
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("schema snapshot %s:%d duplicates %s %s", path, number+1, entry.Kind, entry.Name)
		}
		entries[key] = entry
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
