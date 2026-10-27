//go:build integration

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// TestListFilesCursorScopesToTenantAndHonoursIncludeDeleted covers the
// tenant-wide page the collection route uses for an admin caller. It runs
// through the application (RLS-scoped) role, so a repository regression that
// dropped the tenant predicate would leak another tenant's row here.
func TestListFilesCursorScopesToTenantAndHonoursIncludeDeleted(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	tenantB := createFilesTenant(t, pool, "list-files-b")
	userA := createFilesMember(t, pool, tenant.DefaultID, "list-files-a@example.com")
	userB := createFilesMember(t, pool, tenantB, "list-files-b@example.com")

	liveA, _ := insertFile(t, pool, nil, tenant.DefaultID, userA, "live-a", 0, false)
	trashA, _ := insertFile(t, pool, nil, tenant.DefaultID, userA, "trash-a", time.Hour, false)
	liveB, _ := insertFile(t, pool, nil, tenantB, userB, "live-b", 0, false)

	repo := filesrepo.NewFileRepository(fx.Scoped())
	cur := pagination.DBCursor{Limit: 10}

	ctxA := tenant.WithID(context.Background(), tenant.DefaultID)
	files, err := repo.ListCursor(ctxA, cur, false)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{liveA}, fileIDsOf(files), "live page excludes trash and other tenants")

	files, err = repo.ListCursor(ctxA, cur, true)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{liveA, trashA}, fileIDsOf(files), "includeDeleted adds the tenant's trash")

	ctxB := tenant.WithID(context.Background(), tenantB)
	files, err = repo.ListCursor(ctxB, cur, true)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{liveB}, fileIDsOf(files), "the tenant boundary still applies")
}

func fileIDsOf(files []filesvc.File) []uuid.UUID {
	out := make([]uuid.UUID, len(files))
	for i := range files {
		out[i] = files[i].ID
	}
	return out
}
