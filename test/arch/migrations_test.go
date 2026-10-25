package arch_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// migrationFile matches <version>_<name>.<up|down>.sql.
var migrationFile = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$`)

// migrationCreateTable finds the tables a migration creates. It tolerates any
// whitespace (including newlines) between keywords, TEMP/UNLOGGED modifiers,
// IF NOT EXISTS, quoted identifiers, and an optional schema qualifier, whose
// name is skipped so `public.users` yields `users`.
var migrationCreateTable = regexp.MustCompile(`(?is)\bCREATE\s+(?:(?:GLOBAL|LOCAL)\s+)?(?:(?:TEMP|TEMPORARY|UNLOGGED)\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:"?[a-z_][a-z0-9_]*"?\s*\.\s*)?"?([a-z_][a-z0-9_]*)"?`)

// createdTables returns the lower-cased table names a migration creates.
func createdTables(sql string) []string {
	matches := migrationCreateTable.FindAllStringSubmatch(sql, -1)
	tables := make([]string, 0, len(matches))
	for _, match := range matches {
		tables = append(tables, strings.ToLower(match[1]))
	}
	return tables
}

// timestampLayout is the golang-migrate -format used by make migrate-create
// and cmd/gen. Every migration, the per-owner baseline included (ADR 0005),
// uses it: there are no sequential versions.
const timestampLayout = "20060102150405"

// migrationOwners are the non-module owners allowed as a migration name
// prefix. Module owners come from internal/modules/<name>.
var migrationOwners = []string{"platform"}

// TestMigrationFilesArePairedAndUnique fails when two migrations share a
// version (golang-migrate refuses to start), when an up or down file is
// missing, or when a migration does not follow <UTC timestamp>_<owner>_<purpose>.
// It needs no database.
func TestMigrationFilesArePairedAndUnique(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			t.Errorf("migrations/%s: subdirectories are not migrations", entry.Name())
			continue
		}
		names = append(names, entry.Name())
	}
	for _, problem := range migrationProblems(names, migrationOwnerSet(t, root)) {
		t.Error(problem)
	}
}

// TestMigrationsCreateOnlyOwnedTables fails when a migration creates a table
// that belongs to another owner: a module's file may only create its own
// tables (tableOwners / naming convention), and platform files only platform
// tables. This keeps the per-owner split honest.
func TestMigrationsCreateOnlyOwnedTables(t *testing.T) {
	root := repoRoot(t)
	owners := migrationOwnerSet(t, root)
	modules := moduleNames(t, root)
	paths, err := filepath.Glob(filepath.Join(root, "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		match := migrationFile.FindStringSubmatch(name)
		if match == nil {
			continue // reported by TestMigrationFilesArePairedAndUnique
		}
		fileOwner := ownerPrefix(match[2], owners)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, tableName := range createdTables(string(raw)) {
			owner := tableOwner(tableName, modules)
			if strings.HasPrefix(owner, "platform/") {
				owner = "platform"
			}
			if owner != fileOwner {
				t.Errorf("migrations/%s creates %s, which is owned by %q, not %q", name, tableName, owner, fileOwner)
			}
		}
	}
}

// TestMigrationCheckRejectsBadSets keeps the checker honest: each broken set
// must produce at least one problem, and the good set none.
func TestMigrationCheckRejectsBadSets(t *testing.T) {
	owners := map[string]bool{"files": true, "platform": true, "api_keys": true}
	baseline := []string{
		"20260101000000_platform_functions.up.sql", "20260101000000_platform_functions.down.sql",
		"20260101000100_files_init.up.sql", "20260101000100_files_init.down.sql",
	}
	cases := map[string]struct {
		files []string
		ok    bool
	}{
		"baseline only": {files: baseline, ok: true},
		"timestamped pair": {ok: true, files: append([]string{
			"20261024101500_files_add_checksum.up.sql",
			"20261024101500_files_add_checksum.down.sql",
			"20261024101501_api_keys_add_label.up.sql",
			"20261024101501_api_keys_add_label.down.sql",
		}, baseline...)},
		"empty":        {files: nil},
		"missing down": {files: append([]string{"20261024101500_files_add_checksum.up.sql"}, baseline...)},
		"duplicate version": {files: append([]string{
			"20261024101500_files_add_checksum.up.sql",
			"20261024101500_files_add_checksum.down.sql",
			"20261024101500_platform_add_index.up.sql",
			"20261024101500_platform_add_index.down.sql",
		}, baseline...)},
		"sequential version": {files: append([]string{
			"000002_files_add_checksum.up.sql",
			"000002_files_add_checksum.down.sql",
		}, baseline...)},
		"old sequential baseline": {files: []string{
			"000001_init_schema.up.sql",
			"000001_init_schema.down.sql",
		}},
		"unknown owner": {files: append([]string{
			"20261024101500_orders_add_total.up.sql",
			"20261024101500_orders_add_total.down.sql",
		}, baseline...)},
		"owner without purpose": {files: append([]string{
			"20261024101500_files.up.sql",
			"20261024101500_files.down.sql",
		}, baseline...)},
		"invalid timestamp": {files: append([]string{
			"20261399101500_files_add_checksum.up.sql",
			"20261399101500_files_add_checksum.down.sql",
		}, baseline...)},
		"bad file name": {files: append([]string{"20261024101500_files.sql"}, baseline...)},
		"timestamp spelled twice": {files: append([]string{
			"20261024101500_files_add_checksum.up.sql",
			"020261024101500_files_add_checksum.down.sql",
		}, baseline...)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			problems := migrationProblems(tc.files, owners)
			if tc.ok && len(problems) > 0 {
				t.Fatalf("expected no problems, got %v", problems)
			}
			if !tc.ok && len(problems) == 0 {
				t.Fatal("expected at least one problem")
			}
		})
	}
}

func migrationProblems(files []string, owners map[string]bool) []string {
	type migration struct {
		spelling   string // the version digits exactly as written
		name       string
		directions map[string]bool
	}
	var problems []string
	if len(files) == 0 {
		return []string{"migrations/ contains no migrations"}
	}
	byVersion := map[uint64]*migration{}
	for _, file := range files {
		match := migrationFile.FindStringSubmatch(file)
		if match == nil {
			problems = append(problems, fmt.Sprintf("migrations/%s: name must be <version>_<name>.<up|down>.sql", file))
			continue
		}
		version, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("migrations/%s: invalid version: %v", file, err))
			continue
		}
		name, direction := match[2], match[3]

		existing, ok := byVersion[version]
		if !ok {
			existing = &migration{spelling: match[1], name: name, directions: map[string]bool{}}
			byVersion[version] = existing
		}
		// golang-migrate compares versions numerically, so a zero-padded
		// second spelling of a version is a duplicate.
		if existing.spelling != match[1] {
			problems = append(problems, fmt.Sprintf(
				"migrations/%s: version %d is also spelled %q; golang-migrate sees a duplicate version",
				file, version, existing.spelling))
			continue
		}
		if existing.name != name {
			problems = append(problems, fmt.Sprintf(
				"version %d is used by two migrations (%q and %q); regenerate one with make migrate-create",
				version, existing.name, name))
			continue
		}
		existing.directions[direction] = true

		if len(match[1]) != len(timestampLayout) {
			problems = append(problems, fmt.Sprintf(
				"migrations/%s: version must be a %d-digit UTC timestamp (make migrate-create)",
				file, len(timestampLayout)))
			continue
		}
		if _, err := time.Parse(timestampLayout, match[1]); err != nil {
			problems = append(problems, fmt.Sprintf(
				"migrations/%s: version is not a valid %s timestamp: %v", file, timestampLayout, err))
		}
		if ownerPrefix(name, owners) == "" {
			problems = append(problems, fmt.Sprintf(
				"migrations/%s: name must start with an owning module or platform (<ts>_<owner>_<purpose>)", file))
		}
	}

	versions := make([]uint64, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for _, version := range versions {
		m := byVersion[version]
		for _, direction := range []string{"up", "down"} {
			if !m.directions[direction] {
				problems = append(problems, fmt.Sprintf(
					"migration %d_%s is missing its .%s.sql file", version, m.name, direction))
			}
		}
	}
	return problems
}

// ownerPrefix returns the owner whose name followed by "_" prefixes the
// migration name. Module names may contain underscores, so the longest
// matching owner wins.
func ownerPrefix(name string, owners map[string]bool) string {
	best := ""
	for owner := range owners {
		prefix := owner + "_"
		if len(name) > len(prefix) && strings.HasPrefix(name, prefix) && len(owner) > len(best) {
			best = owner
		}
	}
	return best
}

// migrationOwnerSet is every internal/modules/<name> directory plus the
// non-module owners.
func migrationOwnerSet(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := moduleNames(t, root)
	for _, owner := range migrationOwners {
		out[owner] = true
	}
	return out
}

// moduleNames is every internal/modules/<name> directory.
func moduleNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "internal", "modules"))
	if err != nil {
		t.Fatalf("read modules: %v", err)
	}
	out := make(map[string]bool, len(entries)+len(migrationOwners))
	for _, entry := range entries {
		if entry.IsDir() {
			out[entry.Name()] = true
		}
	}
	return out
}

// TestCreatedTablesParser pins the CREATE TABLE forms the ownership check must
// recognize, so a migration cannot hide another owner's table behind a schema
// qualifier, quotes, modifiers, or line breaks.
func TestCreatedTablesParser(t *testing.T) {
	cases := map[string][]string{
		"CREATE TABLE files (id BIGINT);":                            {"files"},
		"create table if not exists files (id bigint);":              {"files"},
		"CREATE TABLE public.users (id BIGINT);":                     {"users"},
		"CREATE TABLE \"public\".\"users\" (id BIGINT);":             {"users"},
		"CREATE\nTABLE\n  IF NOT EXISTS\n  public . users (id INT);": {"users"},
		"CREATE UNLOGGED TABLE scratch (id INT);":                    {"scratch"},
		"CREATE TEMP TABLE tmp_rows (id INT);":                       {"tmp_rows"},
		"CREATE INDEX idx ON users (id);":                            nil,
	}
	for sql, want := range cases {
		got := createdTables(sql)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("createdTables(%q) = %v, want %v", sql, got, want)
		}
	}
}
