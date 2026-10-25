//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	adminservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/adapter"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const tenantFilesSecret = "tenant-files-integration-secret"

type tenantFilesHarness struct {
	app      *fiber.App
	pool     *pgxpool.Pool
	fileRepo *filesrepo.FileRepository
	store    storage.Storage
	tenantA  int64
	tenantB  int64
	userA    int64
	userB    int64
	tokenA   string
	tokenB   string
}

// zeroIdentityAdmin keeps the admin composition focused on the files half of
// the dashboard; identity is covered by its own tenant integration test.
type zeroIdentityAdmin struct{}

func (zeroIdentityAdmin) ListUsers(context.Context, int, string) (identitycontracts.UserPage, error) {
	return identitycontracts.UserPage{}, nil
}

func (zeroIdentityAdmin) UpdateRole(context.Context, int64, string) (identitycontracts.User, error) {
	return identitycontracts.User{}, nil
}

func (zeroIdentityAdmin) BanUser(context.Context, int64) error { return nil }

func (zeroIdentityAdmin) UnbanUser(context.Context, int64) (identitycontracts.User, error) {
	return identitycontracts.User{}, nil
}

func (zeroIdentityAdmin) GetUserStats(context.Context) (identitycontracts.UserStats, error) {
	return identitycontracts.UserStats{}, nil
}

func setupTenantFilesHarness(t *testing.T) *tenantFilesHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var tenantB int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('files-acme', 'Files Acme') RETURNING id`,
	).Scan(&tenantB))

	insertUser := func(tenantID int64, email string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO users (tenant_id, email, name, role)
			 VALUES ($1, $2, $3, 'admin') RETURNING id`,
			tenantID, email, strings.Split(email, "@")[0],
		).Scan(&id))
		return id
	}
	userA := insertUser(tenant.DefaultID, "files-a@test.com")
	userB := insertUser(tenantB, "files-b@test.com")

	tokenA, err := token.GenerateWithVersionAndTenant(userA, tenant.DefaultID, "files-a@test.com", "admin", 0, tenantFilesSecret, 24)
	require.NoError(t, err)
	tokenB, err := token.GenerateWithVersionAndTenant(userB, tenantB, "files-b@test.com", "admin", 0, tenantFilesSecret, 24)
	require.NoError(t, err)

	fileRepo := filesrepo.NewFileRepository(pool)
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	filesTx := filesrepo.NewTransactor(database.NewTxManager(pool), audit.NewPostgresRecorder(pool), nil)
	uploadSvc := filesvc.NewUploadService(fileRepo, filesTx, store, nil)
	uploadHandler := fileshandler.NewUploadHandler(uploadSvc, 10<<20, []string{"application/pdf"})
	filesAdmin := adapter.NewAdminFiles(filesvc.NewAdminFileService(fileRepo, filesTx, store, nil))
	adminHandler := adminhandler.NewHandler(adminservice.NewService(zeroIdentityAdmin{}, filesAdmin))

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	api := app.Group("/api/v1", tenanthttp.Middleware(
		identityadapter.NewTenantResolver(identityrepo.NewTenantRepository(pool)),
		tenant.DefaultSlug,
	))

	files := api.Group("/files", middleware.JWTAuth(tenantFilesSecret))
	files.Post("/upload", uploadHandler.Upload)
	files.Get("/", uploadHandler.List)
	files.Get("/:id/download", uploadHandler.Download)
	files.Get("/:id", uploadHandler.GetInfo)
	files.Delete("/:id", uploadHandler.Delete)
	files.Post("/:id/restore", uploadHandler.Restore)

	admin := api.Group("/admin", middleware.JWTAuth(tenantFilesSecret), middleware.RequireRole("admin"))
	admin.Get("/stats", adminHandler.GetStats)
	admin.Get("/files", adminHandler.ListFiles)
	admin.Get("/files/:id/download", adminHandler.DownloadFile)
	admin.Delete("/files/:id", adminHandler.DeleteFile)
	admin.Post("/files/:id/restore", adminHandler.RestoreFile)

	return &tenantFilesHarness{
		app:      app,
		pool:     pool,
		fileRepo: fileRepo,
		store:    store,
		tenantA:  tenant.DefaultID,
		tenantB:  tenantB,
		userA:    userA,
		userB:    userB,
		tokenA:   tokenA,
		tokenB:   tokenB,
	}
}

func (h *tenantFilesHarness) request(t *testing.T, method, path, bearer string, body []byte, contentType string) (resp *http.Response, respBody []byte) {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err = h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, respBody
}

func (h *tenantFilesHarness) upload(t *testing.T, bearer, filename, content string) dto.FileResponse {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	resp, respBody := h.request(t, http.MethodPost, "/api/v1/files/upload", bearer, body.Bytes(), writer.FormDataContentType())
	require.Equal(t, fiber.StatusCreated, resp.StatusCode, "body: %s", respBody)
	var file dto.FileResponse
	require.NoError(t, json.Unmarshal(respBody, &file))
	return file
}

func (h *tenantFilesHarness) listFiles(t *testing.T, bearer string) []dto.FileResponse {
	t.Helper()
	resp, respBody := h.request(t, http.MethodGet, "/api/v1/files/", bearer, nil, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", respBody)
	var page struct {
		Data []dto.FileResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(respBody, &page))
	return page.Data
}

func (h *tenantFilesHarness) adminFiles(t *testing.T, bearer string) []dto.FileResponse {
	t.Helper()
	resp, respBody := h.request(t, http.MethodGet, "/api/v1/admin/files", bearer, nil, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", respBody)
	var page struct {
		Data []dto.FileResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(respBody, &page))
	return page.Data
}

func tenantFilesCtx(id int64) context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), id))
}

func TestTenantFiles_Isolation(t *testing.T) {
	h := setupTenantFilesHarness(t)

	fileA := h.upload(t, h.tokenA, "a.pdf", "%PDF-1.4 tenant one")
	fileB := h.upload(t, h.tokenB, "b.pdf", "%PDF-1.4 tenant two")

	assert.Equal(t, h.tenantA, fileA.TenantID)
	assert.Equal(t, h.tenantB, fileB.TenantID)

	// Blob keys are prefixed by tenant and owner so equal names cannot collide.
	var pathA, blobPathB string
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT storage_path FROM files WHERE id = $1 AND tenant_id = $2`, fileA.ID, h.tenantA,
	).Scan(&pathA))
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT storage_path FROM files WHERE id = $1 AND tenant_id = $2`, fileB.ID, h.tenantB,
	).Scan(&blobPathB))
	assert.True(t, strings.HasPrefix(pathA, fmt.Sprintf("%d/%d/", h.tenantA, h.userA)), "pathA = %q", pathA)
	assert.True(t, strings.HasPrefix(blobPathB, fmt.Sprintf("%d/%d/", h.tenantB, h.userB)), "blobPathB = %q", blobPathB)
	assert.NotEqual(t, pathA, blobPathB)

	// User-facing reads and writes are tenant-scoped.
	filesA := h.listFiles(t, h.tokenA)
	require.Len(t, filesA, 1)
	assert.Equal(t, fileA.ID, filesA[0].ID)

	pathB := fmt.Sprintf("/api/v1/files/%d", fileB.ID)
	resp, _ := h.request(t, http.MethodGet, pathB, h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant metadata read")
	resp, _ = h.request(t, http.MethodGet, pathB+"/download", h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant download")
	resp, _ = h.request(t, http.MethodDelete, pathB, h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant delete")
	resp, _ = h.request(t, http.MethodGet, pathB, h.tokenB, nil, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "tenant two file must survive tenant one's delete attempt")

	resp, _ = h.request(t, http.MethodDelete, pathB, h.tokenB, nil, "")
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	resp, _ = h.request(t, http.MethodPost, pathB+"/restore", h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant restore")
	var deleted bool
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT deleted_at IS NOT NULL FROM files WHERE id = $1 AND tenant_id = $2`, fileB.ID, h.tenantB,
	).Scan(&deleted))
	assert.True(t, deleted, "cross-tenant restore must not revive the file")
	resp, _ = h.request(t, http.MethodPost, pathB+"/restore", h.tokenB, nil, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Admin listing, downloads, deletes, restores, and stats are scoped too.
	adminA := h.adminFiles(t, h.tokenA)
	require.Len(t, adminA, 1)
	assert.Equal(t, fileA.ID, adminA[0].ID)
	adminB := h.adminFiles(t, h.tokenB)
	require.Len(t, adminB, 1)
	assert.Equal(t, fileB.ID, adminB[0].ID)

	adminPathB := fmt.Sprintf("/api/v1/admin/files/%d", fileB.ID)
	resp, _ = h.request(t, http.MethodGet, adminPathB+"/download", h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant admin download")
	resp, _ = h.request(t, http.MethodDelete, adminPathB, h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant admin delete")
	resp, _ = h.request(t, http.MethodGet, pathB, h.tokenB, nil, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "tenant two file must survive tenant one admin delete")

	resp, _ = h.request(t, http.MethodDelete, adminPathB, h.tokenB, nil, "")
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	resp, _ = h.request(t, http.MethodPost, adminPathB+"/restore", h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant admin restore")
	resp, _ = h.request(t, http.MethodPost, adminPathB+"/restore", h.tokenB, nil, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	statsA := struct {
		TotalFiles    int64 `json:"totalFiles"`
		TotalFileSize int64 `json:"totalFileSize"`
	}{}
	resp, respBody := h.request(t, http.MethodGet, "/api/v1/admin/stats", h.tokenA, nil, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", respBody)
	require.NoError(t, json.Unmarshal(respBody, &statsA))
	assert.Equal(t, int64(1), statsA.TotalFiles)
	assert.Equal(t, int64(len("%PDF-1.4 tenant one")), statsA.TotalFileSize)

	statsB := statsA
	resp, respBody = h.request(t, http.MethodGet, "/api/v1/admin/stats", h.tokenB, nil, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", respBody)
	require.NoError(t, json.Unmarshal(respBody, &statsB))
	assert.Equal(t, int64(1), statsB.TotalFiles)
	assert.Equal(t, int64(len("%PDF-1.4 tenant two")), statsB.TotalFileSize)

	// Direct repository assertions keep the SQL predicates under test even if a
	// future service check masks a missing filter.
	ctxA := tenantFilesCtx(h.tenantA)
	ctxB := tenantFilesCtx(h.tenantB)
	_, err := h.fileRepo.GetByID(ctxA, fileB.ID)
	require.ErrorIs(t, err, apperror.ErrNotFound)
	_, err = h.fileRepo.GetByIDIncludingDeleted(ctxA, fileB.ID)
	require.ErrorIs(t, err, apperror.ErrNotFound)
	_, err = h.fileRepo.GetByID(ctxB, fileA.ID)
	require.ErrorIs(t, err, apperror.ErrNotFound)
	_, err = h.fileRepo.Delete(ctxA, fileB.ID)
	require.ErrorIs(t, err, apperror.ErrNotFound)

	// Delete tenant two's file directly and prove tenant one's restore cannot
	// see or revive it.
	_, err = h.fileRepo.Delete(ctxB, fileB.ID)
	require.NoError(t, err)
	_, err = h.fileRepo.Restore(ctxA, fileB.ID)
	require.ErrorIs(t, err, apperror.ErrNotFound)
	_, err = h.fileRepo.GetByIDIncludingDeleted(ctxA, fileB.ID)
	require.ErrorIs(t, err, apperror.ErrNotFound)
	_, err = h.fileRepo.Restore(ctxB, fileB.ID)
	require.NoError(t, err)

	cur := pagination.DBCursor{Limit: 10}
	repoFilesA, err := h.fileRepo.ListByUserCursor(ctxA, h.userA, cur, true)
	require.NoError(t, err)
	require.Len(t, repoFilesA, 1)
	assert.Equal(t, fileA.ID, repoFilesA[0].ID)
	adminRepoFilesA, err := h.fileRepo.AdminListCursor(ctxA, cur)
	require.NoError(t, err)
	require.Len(t, adminRepoFilesA, 1)
	assert.Equal(t, fileA.ID, adminRepoFilesA[0].ID)
	repoStatsA, err := h.fileRepo.AdminFileStats(ctxA)
	require.NoError(t, err)
	assert.Equal(t, int64(1), repoStatsA.TotalFiles)
	assert.Equal(t, int64(len("%PDF-1.4 tenant one")), repoStatsA.TotalFileSize)

	// The admin adapter must carry the tenant id into contract types.
	adminPage, err := adapter.NewAdminFiles(filesvc.NewAdminFileService(
		h.fileRepo,
		filesrepo.NewTransactor(database.NewTxManager(h.pool), audit.NewPostgresRecorder(h.pool), nil),
		h.store, nil,
	)).ListFiles(ctxA, 10, "")
	require.NoError(t, err)
	require.Len(t, adminPage.Items, 1)
	assert.Equal(t, h.tenantA, adminPage.Items[0].TenantID)
}
