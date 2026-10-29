package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// tenantPurgeBlobConcurrency bounds parallel object-store deletes for one
// tenant page. The row delete still happens only after every blob in the page
// succeeded, so a failure leaves the rows for an idempotent retry.
const tenantPurgeBlobConcurrency = 4

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
func (p *TenantPurger) PurgeTenant(ctx context.Context, tenantID uuid.UUID) error {
	ctx = tenant.WithID(ctx, tenantID)
	for {
		if err := tenancycontracts.CheckPurgeBudget(ctx); err != nil {
			return err
		}
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
		ids := make([]uuid.UUID, len(rows))
		group, groupCtx := errgroup.WithContext(ctx)
		group.SetLimit(tenantPurgeBlobConcurrency)
		for i, row := range rows {
			ids[i] = row.ID
			path := row.StoragePath
			group.Go(func() error {
				if err := p.storage.Delete(groupCtx, path); err != nil && !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("delete tenant file blob: %w", err)
				}
				return nil
			})
		}
		if err := group.Wait(); err != nil {
			// A deadline reached during concurrent deletes is a resumable
			// budget stop, not a storage failure. The rows are untouched.
			if budgetErr := tenancycontracts.CheckPurgeBudget(ctx); budgetErr != nil {
				return budgetErr
			}
			return err
		}
		if err := tenancycontracts.CheckPurgeBudget(ctx); err != nil {
			return err
		}
		if _, err := p.q.DeleteTenantFilesForPurge(ctx, filesqlc.DeleteTenantFilesForPurgeParams{
			TenantID: tenantID,
			Ids:      ids,
		}); err != nil {
			return fmt.Errorf("delete tenant files: %w", err)
		}
	}
}
