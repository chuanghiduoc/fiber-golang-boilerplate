//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	// `go test` runs each package's tests in its own process; the shared
	// fixture creates and drops a per-schema copy of the application schema.
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// TestRevokeMigrationTableAccessIntegration proves the runtime roles lose every
// privilege on golang-migrate's ledger. pgtest's app/bypass roles receive the
// same "GRANT ... ON ALL TABLES" the production role script applies, so this
// exercises the real default-privilege leak.
func TestRevokeMigrationTableAccessIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, owner, cleanup, err := pgtest.SetupTestDBWithOwner(ctx)
	if err != nil {
		t.Fatalf("setup test db: %v", err)
	}
	t.Cleanup(cleanup)

	privilege := func(role, verb string) bool {
		t.Helper()
		var granted bool
		if err := owner.QueryRow(ctx,
			`SELECT has_table_privilege($1, to_regclass('schema_migrations'), $2)`,
			role, verb,
		).Scan(&granted); err != nil {
			t.Fatalf("read %s privilege for %s: %v", verb, role, err)
		}
		return granted
	}

	for _, role := range []string{pgtest.AppRole, pgtest.BypassRole} {
		if !privilege(role, "UPDATE") {
			t.Fatalf("%s must start with UPDATE on schema_migrations (the leak this fix closes)", role)
		}
	}

	if err := revokeMigrationTableAccess(ctx, owner); err != nil {
		t.Fatalf("revoke migration table access: %v", err)
	}

	for _, role := range []string{pgtest.AppRole, pgtest.BypassRole} {
		for _, verb := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			if privilege(role, verb) {
				t.Fatalf("%s still has %s on schema_migrations after the revoke", role, verb)
			}
		}
	}

	// Idempotent: the second run is a no-op, not an error.
	if err := revokeMigrationTableAccess(ctx, owner); err != nil {
		t.Fatalf("second revoke must succeed: %v", err)
	}
}
