package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDatabaseRolesScriptRevokesEscalationPaths guards the two deployment-only
// hardening steps that no Go code can enforce on an existing database: the
// runtime roles must not be able to create temp tables (a trigger function's
// search_path could otherwise be shadowed per statement) and they must not
// keep DML on golang-migrate's ledger (rewriting it replays or skips DDL).
func TestDatabaseRolesScriptRevokesEscalationPaths(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "postgres", "database-roles.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	script := string(raw)

	for _, want := range []string{
		"REVOKE TEMP ON DATABASE %I FROM PUBLIC",
		"REVOKE TEMP ON DATABASE %I FROM %I, %I",
		"REVOKE ALL ON TABLE %I.schema_migrations FROM %I, %I",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s is missing %q", path, want)
		}
	}
}
