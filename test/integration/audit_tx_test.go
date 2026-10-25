//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type auditTxHarness struct {
	pool     *pgxpool.Pool
	recorder *audit.PostgresRecorder
	tx       *database.TxManager
	users    identityrepo.UserRepository
	refresh  identityrepo.RefreshTokenRepository
	files    *filesrepo.FileRepository
	store    storage.Storage
}

func setupAuditTxHarness(t *testing.T) *auditTxHarness {
	t.Helper()
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	return &auditTxHarness{
		pool:     pool,
		recorder: audit.NewPostgresRecorder(pool),
		tx:       database.NewTxManager(pool),
		users:    identityrepo.NewUserRepository(pool),
		refresh:  identityrepo.NewRefreshTokenRepository(pool),
		files:    filesrepo.NewFileRepository(pool),
		store:    store,
	}
}

func (h *auditTxHarness) actorCtx(tenantID, actorID int64, role string) context.Context {
	ctx := tenant.WithID(context.Background(), tenantID)
	// Mirror JWTAuth: services authorize through the request subject.
	ctx = authz.WithSubject(ctx, authz.NewSubject(actorID, tenantID, role))
	return audit.WithActor(ctx, actorID, role)
}

func (h *auditTxHarness) insertUser(t *testing.T, tenantID int64, email, role string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`INSERT INTO users (tenant_id, email, name, role) VALUES ($1, $2, 'Audit User', $3) RETURNING id`,
		tenantID, email, role,
	).Scan(&id))
	return id
}

func (h *auditTxHarness) installInsertFailure(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := h.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_audit_insert_failure() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced audit insert failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_audit_insert_failure
			BEFORE INSERT ON audit_events
			FOR EACH ROW EXECUTE FUNCTION test_audit_insert_failure();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = h.pool.Exec(cleanupCtx,
			`DROP TRIGGER IF EXISTS test_audit_insert_failure ON audit_events`)
		_, _ = h.pool.Exec(cleanupCtx, `DROP FUNCTION IF EXISTS test_audit_insert_failure()`)
	})
}

func requireAuditUnavailable(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var appErr *apperror.AppError
	require.True(t, errors.As(err, &appErr), "expected an AppError, got %T: %v", err, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, appErr.Status)
	assert.Equal(t, string(apperror.CodeSystemAuditUnavailable), appErr.Code)
}

func TestAuditTx_UpdateRoleRollsBackWhenAuditFails(t *testing.T) {
	h := setupAuditTxHarness(t)
	adminID := h.insertUser(t, tenant.DefaultID, "audit-admin@test.com", "admin")
	targetID := h.insertUser(t, tenant.DefaultID, "audit-target@test.com", "user")
	h.installInsertFailure(t)

	svc := identityservice.NewAdminUserService(h.users, h.refresh, h.tx, h.recorder)
	_, err := svc.UpdateRole(h.actorCtx(tenant.DefaultID, adminID, "admin"), targetID, "admin")
	requireAuditUnavailable(t, err)

	var role string
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT role FROM users WHERE id = $1`, targetID).Scan(&role))
	assert.Equal(t, "user", role, "the role update must roll back with the failed audit row")
}

func TestAuditTx_BanRollsBackWhenAuditFails(t *testing.T) {
	h := setupAuditTxHarness(t)
	adminID := h.insertUser(t, tenant.DefaultID, "audit-admin-ban@test.com", "admin")
	targetID := h.insertUser(t, tenant.DefaultID, "audit-target-ban@test.com", "user")
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO refresh_tokens (tenant_id, user_id, token, expires_at)
		 VALUES ($1, $2, 'audit-refresh-token', NOW() + INTERVAL '1 day')`,
		tenant.DefaultID, targetID)
	require.NoError(t, err)
	h.installInsertFailure(t)

	svc := identityservice.NewAdminUserService(h.users, h.refresh, h.tx, h.recorder)
	err = svc.BanUser(h.actorCtx(tenant.DefaultID, adminID, "admin"), targetID)
	requireAuditUnavailable(t, err)

	var deleted, banned bool
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT deleted_at IS NOT NULL, banned_at IS NOT NULL FROM users WHERE id = $1`, targetID).
		Scan(&deleted, &banned))
	assert.False(t, deleted)
	assert.False(t, banned)

	var tokens int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM refresh_tokens WHERE user_id = $1`, targetID).Scan(&tokens))
	assert.Equal(t, int64(1), tokens, "refresh token revocation must roll back too")
}

func TestAuditTx_UnbanRollsBackWhenAuditFails(t *testing.T) {
	h := setupAuditTxHarness(t)
	adminID := h.insertUser(t, tenant.DefaultID, "audit-admin-unban@test.com", "admin")
	targetID := h.insertUser(t, tenant.DefaultID, "audit-target-unban@test.com", "user")
	_, err := h.pool.Exec(context.Background(),
		`UPDATE users SET deleted_at = NOW(), banned_at = NOW() WHERE id = $1`, targetID)
	require.NoError(t, err)
	h.installInsertFailure(t)

	svc := identityservice.NewAdminUserService(h.users, h.refresh, h.tx, h.recorder)
	_, err = svc.UnbanUser(h.actorCtx(tenant.DefaultID, adminID, "admin"), targetID)
	requireAuditUnavailable(t, err)

	var deleted bool
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, targetID).Scan(&deleted))
	assert.True(t, deleted, "restore must roll back with the failed audit row")
}

func TestAuditTx_FileDeleteAndRestoreRollBackWhenAuditFails(t *testing.T) {
	h := setupAuditTxHarness(t)
	adminID := h.insertUser(t, tenant.DefaultID, "audit-admin-file@test.com", "admin")
	var fileID int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES ($1, $2, 'audit.pdf', '1/1/audit.pdf', 'application/pdf', 10) RETURNING id`,
		tenant.DefaultID, adminID,
	).Scan(&fileID))

	svc := filesvc.NewAdminFileService(h.files, filesrepo.NewTransactor(h.tx, h.recorder, nil), h.store, nil)
	ctx := h.actorCtx(tenant.DefaultID, adminID, "admin")

	h.installInsertFailure(t)
	err := svc.DeleteFile(ctx, fileID)
	requireAuditUnavailable(t, err)

	var deletedAt *time.Time
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "file soft delete must roll back when audit fails")

	// Restore path: the row is already deleted, and the audit failure must
	// leave it deleted.
	_, err = h.pool.Exec(context.Background(),
		`UPDATE files SET deleted_at = NOW() WHERE id = $1`, fileID)
	require.NoError(t, err)
	_, err = svc.RestoreFile(ctx, fileID)
	requireAuditUnavailable(t, err)
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM files WHERE id = $1`, fileID).Scan(&deletedAt))
	require.NotNil(t, deletedAt, "file restore must roll back when audit fails")
}

// TestFilesWebhooks_AdminAndRestoreUseOutbox proves admin delete and both
// restores enqueue file.deleted/file.restored in the mutation transaction, and
// that an outbox failure rolls the mutation back (fail-closed).
func TestFilesWebhooks_AdminAndRestoreUseOutbox(t *testing.T) {
	h := setupAuditTxHarness(t)
	bg := context.Background()
	adminID := h.insertUser(t, tenant.DefaultID, "files-webhook-admin@test.com", "admin")
	ownerID := h.insertUser(t, tenant.DefaultID, "files-webhook-owner@test.com", "user")
	var fileID int64
	require.NoError(t, h.pool.QueryRow(bg,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES ($1, $2, 'hook.pdf', '1/2/hook.pdf', 'application/pdf', 10) RETURNING id`,
		tenant.DefaultID, ownerID,
	).Scan(&fileID))

	queue := jobs.NewQueue(h.pool)
	dispatcher := webhookservice.NewDispatcher(webhookrepo.New(h.pool, queue), queue)
	tx := filesrepo.NewTransactor(h.tx, h.recorder, dispatcher)
	adminSvc := filesvc.NewAdminFileService(h.files, tx, h.store, dispatcher)
	uploadSvc := filesvc.NewUploadService(h.files, tx, h.store, dispatcher)

	adminCtx := h.actorCtx(tenant.DefaultID, adminID, "admin")
	ownerCtx := h.actorCtx(tenant.DefaultID, ownerID, "user")
	require.NoError(t, adminSvc.DeleteFile(adminCtx, fileID))
	_, err := adminSvc.RestoreFile(adminCtx, fileID)
	require.NoError(t, err)
	_, err = h.pool.Exec(bg, `UPDATE files SET deleted_at = NOW() WHERE id = $1`, fileID)
	require.NoError(t, err)
	_, err = uploadSvc.Restore(ownerCtx, fileID, ownerID)
	require.NoError(t, err)

	rows, err := h.pool.Query(bg,
		`SELECT payload->>'type' FROM jobs WHERE kind = $1 AND tenant_id = $2 ORDER BY id`,
		webhookservice.DispatchJobKind, tenant.DefaultID)
	require.NoError(t, err)
	var types []string
	for rows.Next() {
		var eventType string
		require.NoError(t, rows.Scan(&eventType))
		types = append(types, eventType)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Equal(t, []string{
		webhookcontracts.EventFileDeleted,
		webhookcontracts.EventFileRestored,
		webhookcontracts.EventFileRestored,
	}, types)

	// A dispatcher whose outbox cannot enqueue fails the admin delete closed.
	broken := webhookservice.NewDispatcher(webhookrepo.New(h.pool, queue), nil)
	brokenSvc := filesvc.NewAdminFileService(h.files, filesrepo.NewTransactor(h.tx, h.recorder, broken), h.store, broken)
	require.Error(t, brokenSvc.DeleteFile(adminCtx, fileID))
	var deletedAt *time.Time
	require.NoError(t, h.pool.QueryRow(bg, `SELECT deleted_at FROM files WHERE id = $1`, fileID).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "the delete must roll back when the webhook outbox fails")
}

func TestAuditTx_RecordsTenantAndActor(t *testing.T) {
	h := setupAuditTxHarness(t)
	ctx := context.Background()
	var tenantID int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('audit-tx-tenant', 'Audit Tx') RETURNING id`).
		Scan(&tenantID))
	adminID := h.insertUser(t, tenantID, "tenant-audit-admin@test.com", "admin")
	targetID := h.insertUser(t, tenantID, "tenant-audit-target@test.com", "user")

	svc := identityservice.NewAdminUserService(h.users, h.refresh, h.tx, h.recorder)
	_, err := svc.UpdateRole(h.actorCtx(tenantID, adminID, "admin"), targetID, "admin")
	require.NoError(t, err)

	var (
		gotTenant   int64
		gotActor    *int64
		gotActorTyp string
		gotRole     *string
		gotSource   string
		gotOutcome  string
		parent      *int64
		completed   *time.Time
	)
	require.NoError(t, h.pool.QueryRow(ctx, `
		SELECT tenant_id, actor_id, actor_type, actor_role, source, outcome, parent_id, completed_at
		FROM audit_events
		WHERE action = 'admin.user.role_changed' AND resource_id = $1`,
		fmt.Sprint(targetID),
	).Scan(&gotTenant, &gotActor, &gotActorTyp, &gotRole, &gotSource, &gotOutcome, &parent, &completed))
	assert.Equal(t, tenantID, gotTenant)
	require.NotNil(t, gotActor)
	assert.Equal(t, adminID, *gotActor)
	assert.Equal(t, audit.ActorTypeAdmin, gotActorTyp)
	require.NotNil(t, gotRole)
	assert.Equal(t, "admin", *gotRole)
	assert.Equal(t, audit.SourceService, gotSource)
	assert.Equal(t, audit.OutcomeSucceeded, gotOutcome)
	assert.Nil(t, parent)
	assert.Nil(t, completed)
}

func TestAuditGuard_IntegrationIntentCompletionAndFailClosed(t *testing.T) {
	h := setupAuditTxHarness(t)
	adminID := h.insertUser(t, tenant.DefaultID, "guard-admin@test.com", "admin")

	buildApp := func() *fiber.App {
		app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
		app.Use(func(c fiber.Ctx) error {
			fiber.Locals[string](c, "request_id", fmt.Sprintf("guard-%d", time.Now().UnixNano()))
			fiber.Locals[int64](c, "user_id", adminID)
			fiber.Locals[string](c, "role", "admin")
			c.SetContext(h.actorCtx(tenant.DefaultID, adminID, "admin"))
			return c.Next()
		})
		app.Post("/mutate", audithttp.Guard(h.recorder, audithttp.Policy{
			Mode: audithttp.ModeRequired, Action: "test.guarded", Resource: "test",
		}), func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusNoContent)
		})
		return app
	}

	app := buildApp()
	req, _ := http.NewRequest(http.MethodPost, "/mutate", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

	var (
		outcome     string
		completedAt *time.Time
		actorID     *int64
		source      string
	)
	require.NoError(t, h.pool.QueryRow(context.Background(), `
		SELECT outcome, completed_at, actor_id, source
		FROM audit_events WHERE action = 'test.guarded'
		ORDER BY id DESC LIMIT 1`).
		Scan(&outcome, &completedAt, &actorID, &source))
	assert.Equal(t, audit.OutcomeSucceeded, outcome)
	assert.NotNil(t, completedAt)
	require.NotNil(t, actorID)
	assert.Equal(t, adminID, *actorID)
	assert.Equal(t, audit.SourceHTTP, source)

	// A forced intent failure must return 503 before the handler mutates.
	h.installInsertFailure(t)
	req, _ = http.NewRequest(http.MethodPost, "/mutate", http.NoBody)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}
