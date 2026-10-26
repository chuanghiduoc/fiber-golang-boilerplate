package repository

import (
	"context"
	"errors"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// TenantPurger implements tenancy.Purger for files. Each page's blobs are
// deleted before its rows, so a retry re-reads the rows, treats a missing blob
// as deleted, and finishes.
type TenantPurger struct {
	q       *filesqlc.Queries
	storage storage.Storage
}

var _ tenancycontracts.Purger = (*TenantPurger)(nil)

// NewTenantPurger binds the tenant-wide purger to db and the blob store.
func NewTenantPurger(db filesqlc.DBTX, store storage.Storage) *TenantPurger {
	return &TenantPurger{q: filesqlc.New(db), storage: store}
}

// PurgeTenant processes one bounded page at a time until no file row remains.
func (p *TenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	for {
		rows, err := p.q.ListTenantFilesForPurge(ctx, filesqlc.ListTenantFilesForPurgeParams{
			TenantID:  tenantID,
			BatchSize: tenancycontracts.PurgeBatchSize,
		})
		if err != nil {
			return fmt.Errorf("list tenant file blobs: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
			if err := p.storage.Delete(ctx, row.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("delete tenant file blob: %w", err)
			}
		}
		if _, err := p.q.DeleteTenantFilesForPurge(ctx, filesqlc.DeleteTenantFilesForPurgeParams{
			TenantID: tenantID,
			Ids:      ids,
		}); err != nil {
			return fmt.Errorf("delete tenant files: %w", err)
		}
	}
}
