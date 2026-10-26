package jobs

import (
	"context"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// purgeTenantJobsSQL deletes one bounded page of a deleted tenant's jobs. A
// running row is left alone: its worker owns the lease and will finish (or
// fail) it, and deleting it mid-flight would strand the handler's bookkeeping
// and could re-run an already-started side effect. Every other status
// (queued, succeeded, failed, dead) is removed, so queued encrypted email
// payloads and other PII cannot run after the tenant is gone.
const purgeTenantJobsSQL = `
DELETE FROM jobs
WHERE id IN (
    SELECT id
    FROM jobs
    WHERE tenant_id = $1
      AND status <> 'running'
    ORDER BY id
    LIMIT $2
)`

// PurgeTenant erases a deleted tenant's jobs in bounded batches. The caller
// supplies the tenant purger's db (the RLS-scoped handle); the context is
// scoped to tenantID explicitly so the tenant_isolation policy admits exactly
// that tenant's rows. Running rows are retained. It is idempotent: a repeated
// call finds nothing to delete.
func PurgeTenant(ctx context.Context, db database.DBTX, tenantID int64) error {
	if db == nil {
		return ErrNoPool
	}
	if tenantID <= 0 {
		return fmt.Errorf("jobs: purge tenant %d: %w", tenantID, ErrInvalidTenant)
	}
	ctx = tenant.WithID(ctx, tenantID)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("jobs: purge tenant jobs: %w", err)
		}
		tag, err := db.Exec(ctx, purgeTenantJobsSQL, tenantID, purgeBatchSize)
		if err != nil {
			return fmt.Errorf("jobs: purge tenant %d jobs: %w", tenantID, err)
		}
		if tag.RowsAffected() < int64(purgeBatchSize) {
			return nil
		}
	}
}

// TenantPurger adapts PurgeTenant to the tenancy.Purger contract so the
// composition root can register the jobs table's owner alongside the module
// purgers. It is registered last: jobs references no other module's rows, but
// the ordering keeps the module-owner convention (identity last among
// modules) intact.
type TenantPurger struct {
	db database.DBTX
}

var _ tenancycontracts.Purger = (*TenantPurger)(nil)

// NewTenantPurger binds the tenant-wide jobs purge to db (the RLS-scoped
// handle the application uses for every statement).
func NewTenantPurger(db database.DBTX) *TenantPurger {
	return &TenantPurger{db: db}
}

// PurgeTenant implements tenancy.Purger.
func (p *TenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	return PurgeTenant(ctx, p.db, tenantID)
}
