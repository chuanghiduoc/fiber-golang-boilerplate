//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// TestVerifySchemaIntegrationAcceptsBaseline keeps the guard lists honest: a
// fresh baseline must satisfy every required table, column, unique index, and
// tenant-leading index.
func TestVerifySchemaIntegrationAcceptsBaseline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	if err != nil {
		t.Fatalf("setup test db: %v", err)
	}
	t.Cleanup(cleanup)

	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}
	if err := verifySchema(ctx, pool, schema); err != nil {
		t.Fatalf("verifySchema on a fresh baseline: %v", err)
	}
}
