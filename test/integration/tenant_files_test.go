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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

type tenantFilesHarness struct {
	app      *fiber.App
	pool     *pgxpool.Pool
	fileRepo *filesrepo.FileRepository
	tenantA  uuid.UUID
	tenantB  uuid.UUID
	userA    uuid.UUID
	userB    uuid.UUID
	tokenA   string
	tokenB   string
}

func setupTenantFilesHarness(t *testing.T) *tenantFilesHarness {
	t.Helper()
	ctx := context.Background()
	full := newIntegrationApp(t, "test")
	tenantB := testutil.CreateTenant(t, ctx, full.pool, "files-acme")

	insertUser := func(tenantID uuid.UUID, email string) testutil.Member {
		t.Helper()
		return testutil.CreateMember(t, ctx, full.pool, tenantID, email, testutil.MemberOptions{Role: "admin"})
	}
	userA := insertUser(tenant.DefaultID, "files-a@test.com")
	userB := insertUser(tenantB, "files-b@test.com")
	tokenA := testutil.SessionToken(t, ctx, full.pool, userA, full.config.JWT.Secret)
	tokenB := testutil.SessionToken(t, ctx, full.pool, userB, full.config.JWT.Secret)

	return &tenantFilesHarness{
		app:      full.app,
		pool:     full.pool,
		fileRepo: filesrepo.NewFileRepository(full.fx.Scoped()),
		tenantA:  tenant.DefaultID,
		tenantB:  tenantB,
		userA:    userA.UserID,
		userB:    userB.UserID,
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
	slug := "default"
	if bearer == h.tokenB {
		slug = "files-acme"
	}
	req.Header.Set("X-Tenant-Slug", slug)
	if method != http.MethodGet && method != http.MethodDelete {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	resp, err = h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, respBody
}

func (h *tenantFilesHarness) upload(t *testing.T, bearer, filename, content string) filesdto.FileResponse {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	resp, respBody := h.request(t, http.MethodPost, "/api/v1/files", bearer, body.Bytes(), writer.FormDataContentType())
	require.Equal(t, fiber.StatusCreated, resp.StatusCode, "body: %s", respBody)
	var file filesdto.FileResponse
	require.NoError(t, json.Unmarshal(respBody, &file))
	return file
}

func (h *tenantFilesHarness) listFiles(t *testing.T, bearer string) []filesdto.FileResponse {
	t.Helper()
	resp, respBody := h.request(t, http.MethodGet, "/api/v1/files", bearer, nil, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", respBody)
	var page struct {
		Data []filesdto.FileResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(respBody, &page))
	return page.Data
}

func tenantFilesCtx(id uuid.UUID) context.Context {
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
	assert.True(t, strings.HasPrefix(pathA, fmt.Sprintf("%s/%s/", h.tenantA, h.userA)), "pathA = %q", pathA)
	assert.True(t, strings.HasPrefix(blobPathB, fmt.Sprintf("%s/%s/", h.tenantB, h.userB)), "blobPathB = %q", blobPathB)
	assert.NotEqual(t, pathA, blobPathB)

	// Tenant admins see every row in their tenant; a cross-tenant id is 404.
	filesA := h.listFiles(t, h.tokenA)
	require.Len(t, filesA, 1)
	assert.Equal(t, fileA.ID, filesA[0].ID)

	pathB := "/api/v1/files/" + fileB.ID.String()
	resp, _ := h.request(t, http.MethodGet, pathB, h.tokenA, nil, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "cross-tenant metadata read")
	resp, _ = h.request(t, http.MethodGet, pathB+"/content", h.tokenA, nil, "")
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

	// Direct repository assertions keep the SQL predicates under test even if a
	// future service check masks a missing filter.
	ctxA := tenantFilesCtx(h.tenantA)
	rowsA, err := h.fileRepo.AdminListCursor(ctxA, pagination.DBCursor{Limit: 50})
	require.NoError(t, err)
	for _, row := range rowsA {
		assert.Equal(t, h.tenantA, row.TenantID)
	}
}
