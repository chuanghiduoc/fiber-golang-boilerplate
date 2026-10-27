//go:build integration

package migrate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// TestVerifySchemaIntegrationAcceptsBaseline keeps the guard lists honest: a
// fresh baseline must satisfy every required table, column, unique index, and
// tenant-leading index.
func snapshotPathForTest() string {
	return filepath.Join("..", "..", "migrations", "schema.snapshot.sql")
}

func TestVerifySchemaIntegrationAcceptsBaseline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass

	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}
	if err := VerifySchema(ctx, pool, schema, snapshotPathForTest()); err != nil {
		t.Fatalf("verifySchema on a fresh baseline: %v", err)
	}
}

// TestVerifySchemaIntegrationRejectsTenantDefault proves the guard catches a
// compatibility default: tenant-owned rows must always state their tenant.
func TestVerifySchemaIntegrationRejectsTenantDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := pgtest.New(t)
	pool, owner := fx.Bypass, fx.Owner

	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}
	if _, err := owner.Exec(ctx, `ALTER TABLE jobs ALTER COLUMN tenant_id SET DEFAULT '00000000-0000-0000-0000-000000000001'`); err != nil {
		t.Fatalf("install a tenant_id default: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `ALTER TABLE jobs ALTER COLUMN tenant_id DROP DEFAULT`)
	})

	gaps, err := checkTenantColumnDefaults(ctx, pool, schema)
	if err != nil {
		t.Fatalf("checkTenantColumnDefaults: %v", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("gaps = %v, want one jobs tenant_id default", gaps)
	}
}
