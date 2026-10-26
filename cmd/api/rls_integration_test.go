//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// TestCheckRLSRoleIntegration proves the real catalog queries: the pgtest app
// role is a dedicated non-superuser that owns nothing, while the schema owner,
// a table owner, and a member of a BYPASSRLS role are all unsafe.
func TestCheckRLSRoleIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	appPool, ownerPool, cleanup, err := pgtest.SetupTestDBWithOwner(ctx)
	if err != nil {
		t.Fatalf("setup test db: %v", err)
	}
	t.Cleanup(cleanup)

	var schema string
	if err := ownerPool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}

	if err := checkRLSRole(ctx, appPool, schema, "production"); err != nil {
		t.Fatalf("the app role must be accepted: %v", err)
	}
	ownerErr := checkRLSRole(ctx, ownerPool, schema, "production")
	if ownerErr == nil {
		t.Fatal("the schema owner role must be refused in production")
	}
	if !strings.Contains(ownerErr.Error(), "superuser") {
		t.Fatalf("owner error = %v, want the superuser reason", ownerErr)
	}
	if err := checkRLSRole(ctx, ownerPool, schema, "staging"); err != nil {
		t.Fatalf("a non-production environment must only log the risk: %v", err)
	}

	suffix := strings.TrimPrefix(schema, "test_")
	exec := func(query string) {
		t.Helper()
		if _, err := ownerPool.Exec(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	ident := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	rolePool := func(role string) *pgxpool.Pool {
		t.Helper()
		cfg := ownerPool.Config()
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET ROLE "+ident(role))
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("open %s pool: %v", role, err)
		}
		t.Cleanup(pool.Close)
		return pool
	}

	// A non-superuser table owner can run ALTER TABLE ... NO FORCE ROW LEVEL
	// SECURITY, so ownership alone is unsafe.
	ownerRole := "rls_owner_" + suffix
	exec(`CREATE ROLE ` + ident(ownerRole) + ` NOLOGIN NOSUPERUSER NOBYPASSRLS`)
	exec(`CREATE TABLE ` + pgx.Identifier{schema}.Sanitize() + `.rls_owner_probe (id INT)`)
	exec(`ALTER TABLE ` + pgx.Identifier{schema}.Sanitize() + `.rls_owner_probe OWNER TO ` + ident(ownerRole))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = ownerPool.Exec(cleanupCtx, `DROP TABLE IF EXISTS `+pgx.Identifier{schema}.Sanitize()+`.rls_owner_probe`)
		_, _ = ownerPool.Exec(cleanupCtx, `DROP ROLE IF EXISTS `+ident(ownerRole))
	})
	ownerErr = checkRLSRole(ctx, rolePool(ownerRole), schema, "production")
	if ownerErr == nil || !strings.Contains(ownerErr.Error(), "owns tables") {
		t.Fatalf("table owner checkRLSRole = %v, want an owns-tables refusal", ownerErr)
	}

	// A member of a BYPASSRLS role can SET ROLE to it, so membership is unsafe.
	bypassRole := "rls_bypass_" + suffix
	memberRole := "rls_member_" + suffix
	exec(`CREATE ROLE ` + ident(bypassRole) + ` NOLOGIN BYPASSRLS`)
	exec(`CREATE ROLE ` + ident(memberRole) + ` NOLOGIN NOSUPERUSER NOBYPASSRLS`)
	exec(`GRANT ` + ident(bypassRole) + ` TO ` + ident(memberRole))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = ownerPool.Exec(cleanupCtx, `DROP ROLE IF EXISTS `+ident(memberRole))
		_, _ = ownerPool.Exec(cleanupCtx, `DROP ROLE IF EXISTS `+ident(bypassRole))
	})
	memberErr := checkRLSRole(ctx, rolePool(memberRole), schema, "production")
	if memberErr == nil || !strings.Contains(memberErr.Error(), "member") {
		t.Fatalf("member checkRLSRole = %v, want a membership refusal", memberErr)
	}
}
