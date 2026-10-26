package files

import (
	"context"
	"errors"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const (
	listTenantFilePathsSQL = `SELECT storage_path FROM files WHERE tenant_id = $1`
	deleteTenantFilesSQL   = `DELETE FROM files WHERE tenant_id = $1`
)

// tenantPurger removes a deleted tenant's blobs, then its file rows. A missing
// blob counts as deleted, so a retried purge can finish after a partial run.
type tenantPurger struct {
	db      database.DBTX
	storage storage.Storage
}

// RegisterTenantPurge contributes files' tenant-wide purger to the tenancy
// registry. The composition root calls it once after Build.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry, db database.DBTX, store storage.Storage) {
	registry.Register(tenancycontracts.Provider{Name: "files", Purger: &tenantPurger{db: db, storage: store}})
}

func (p *tenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	rows, err := p.db.Query(ctx, listTenantFilePathsSQL, tenantID)
	if err != nil {
		return fmt.Errorf("list tenant file blobs: %w", err)
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return fmt.Errorf("scan tenant file blob: %w", err)
		}
		paths = append(paths, path)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list tenant file blobs: %w", err)
	}
	for _, path := range paths {
		if err := p.storage.Delete(ctx, path); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("delete tenant file blob: %w", err)
		}
	}
	if _, err := p.db.Exec(ctx, deleteTenantFilesSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant files: %w", err)
	}
	return nil
}

var _ tenancycontracts.Purger = (*tenantPurger)(nil)
