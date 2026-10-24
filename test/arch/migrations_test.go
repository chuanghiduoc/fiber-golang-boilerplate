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

// baselineVersion is the consolidated fresh-install schema. Every later
// migration uses a UTC timestamp version (make migrate-create).
const baselineVersion = 1

// timestampLayout is the golang-migrate -format used by make migrate-create.
const timestampLayout = "20060102150405"

// migrationOwners are the non-module owners allowed as a migration name
// prefix. Module owners come from internal/modules/<name>.
var migrationOwners = []string{"platform"}

// TestMigrationFilesArePairedAndUnique fails when two migrations share a
// version (golang-migrate refuses to start), when an up or down file is
// missing, or when a new migration does not follow
// <UTC timestamp>_<owner>_<purpose>. It needs no database.
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

// TestMigrationCheckRejectsBadSets keeps the checker honest: each broken set
// must produce at least one problem, and the good set none.
func TestMigrationCheckRejectsBadSets(t *testing.T) {
	owners := map[string]bool{"files": true, "platform": true, "api_keys": true}
	baseline := []string{"000001_init_schema.up.sql", "000001_init_schema.down.sql"}
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
		"unknown owner": {files: append([]string{
			"20261024101500_orders_add_total.up.sql",
			"20261024101500_orders_add_total.down.sql",
		}, baseline...)},
		"invalid timestamp": {files: append([]string{
			"20261399101500_files_add_checksum.up.sql",
			"20261399101500_files_add_checksum.down.sql",
		}, baseline...)},
		"bad file name": {files: append([]string{"20261024101500_files.sql"}, baseline...)},
		"no baseline":   {files: []string{"20261024101500_files_x.up.sql", "20261024101500_files_x.down.sql"}},
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
		name       string
		directions map[string]bool
	}
	var problems []string
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
			existing = &migration{name: name, directions: map[string]bool{}}
			byVersion[version] = existing
		}
		if existing.name != name {
			problems = append(problems, fmt.Sprintf(
				"version %d is used by two migrations (%q and %q); regenerate one with make migrate-create",
				version, existing.name, name))
			continue
		}
		existing.directions[direction] = true

		if version == baselineVersion {
			continue
		}
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

	if _, ok := byVersion[baselineVersion]; !ok {
		problems = append(problems, fmt.Sprintf("baseline migration %06d_*.sql is missing", baselineVersion))
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
	for _, owner := range migrationOwners {
		out[owner] = true
	}
	return out
}
