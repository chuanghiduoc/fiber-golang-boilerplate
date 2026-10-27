package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TestFileRepositoryRequiresExplicitTenant proves a repository never falls
// back: without a tenant in the context the write fails before reaching SQL.
func TestFileRepositoryRequiresExplicitTenant(t *testing.T) {
	repo := NewFileRepository(nil)

	_, err := repo.Create(context.Background(), filesvc.NewFile{
		UserID:       testID(1),
		OriginalName: "photo.jpg",
		StoragePath:  "1/1/photo.jpg",
		MimeType:     "image/jpeg",
		Size:         4,
	})
	require.ErrorIs(t, err, tenant.ErrMissingTenant)

	_, err = repo.GetByID(context.Background(), testID(1))
	require.ErrorIs(t, err, tenant.ErrMissingTenant)
}
