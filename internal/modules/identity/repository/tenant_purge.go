package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// TenantPurgeRepository implements service.TenantPurgeRepository. The
// tenant-scoped deletes run through sqlc on the RLS-scoped handle in bounded
// pages; the completion marker and its audit row share one transaction.
type TenantPurgeRepository struct {
	db    identitysqlc.DBTX
	tx    *database.TxManager
	audit audit.TxRecorder
}

var _ identityservice.TenantPurgeRepository = (*TenantPurgeRepository)(nil)

func NewTenantPurgeRepository(db identitysqlc.DBTX, tx *database.TxManager, recorder audit.TxRecorder) *TenantPurgeRepository {
	return &TenantPurgeRepository{db: db, tx: tx, audit: recorder}
}

func (r *TenantPurgeRepository) ListDeletedTenants(ctx context.Context, limit int32) ([]identityservice.PurgeTenant, error) {
	rows, err := identitysqlc.New(r.db).ListDeletedUnpurgedTenants(ctx, limit)
	if err != nil {
		return nil, err
	}
	tenants := make([]identityservice.PurgeTenant, len(rows))
	for i := range rows {
		tenants[i] = identityservice.PurgeTenant{ID: rows[i].ID, Slug: rows[i].Slug}
	}
	return tenants, nil
}

func (r *TenantPurgeRepository) ListExpiredTrialTenantIDs(ctx context.Context, limit int32) ([]int64, error) {
	return identitysqlc.New(r.db).ListExpiredTrialTenantIDs(ctx, limit)
}

func (r *TenantPurgeRepository) ListMembershipUserIDs(ctx context.Context, tenantID, afterUserID int64, limit int32) ([]int64, error) {
	return identitysqlc.New(r.db).ListTenantMembershipUserIDs(ctx, identitysqlc.ListTenantMembershipUserIDsParams{
		TenantID:    tenantID,
		AfterUserID: afterUserID,
		RowLimit:    limit,
	})
}

func (r *TenantPurgeRepository) ListDataExports(ctx context.Context, tenantID int64, limit int32) ([]identityservice.DataExport, error) {
	rows, err := identitysqlc.New(r.db).ListTenantDataExportsForPurge(ctx, identitysqlc.ListTenantDataExportsForPurgeParams{
		TenantID:  tenantID,
		BatchSize: limit,
	})
	if err != nil {
		return nil, err
	}
	exports := make([]identityservice.DataExport, len(rows))
	for i := range rows {
		exports[i] = identityservice.DataExport{
			ID:          rows[i].ID,
			TenantID:    tenantID,
			StoragePath: rows[i].StoragePath,
		}
	}
	return exports, nil
}

func (r *TenantPurgeRepository) DeleteDataExports(ctx context.Context, tenantID int64, ids []int64) error {
	_, err := identitysqlc.New(r.db).DeleteTenantDataExportsForPurge(ctx, identitysqlc.DeleteTenantDataExportsForPurgeParams{
		TenantID: tenantID,
		Ids:      ids,
	})
	return err
}

func (r *TenantPurgeRepository) DeleteInvitationsBatch(ctx context.Context, tenantID int64, limit int32) (int64, error) {
	return identitysqlc.New(r.db).DeleteTenantInvitationsBatch(ctx, identitysqlc.DeleteTenantInvitationsBatchParams{
		TenantID:  tenantID,
		BatchSize: limit,
	})
}

func (r *TenantPurgeRepository) DeleteMembershipsBatch(ctx context.Context, tenantID int64, limit int32) (int64, error) {
	return identitysqlc.New(r.db).DeleteTenantMembershipsBatch(ctx, identitysqlc.DeleteTenantMembershipsBatchParams{
		TenantID:  tenantID,
		BatchSize: limit,
	})
}

func (r *TenantPurgeRepository) DeleteTenantRolesBatch(ctx context.Context, tenantID int64, limit int32) (int64, error) {
	return identitysqlc.New(r.db).DeleteTenantRolesBatch(ctx, identitysqlc.DeleteTenantRolesBatchParams{
		TenantID:  tenantID,
		BatchSize: limit,
	})
}

func (r *TenantPurgeRepository) DeleteTenantSettings(ctx context.Context, tenantID int64) error {
	_, err := identitysqlc.New(r.db).DeleteTenantSettings(ctx, tenantID)
	return err
}

// MarkPurged stamps purged_at and writes the completion audit row in one
// transaction. It reports false when another pass already marked the tenant.
func (r *TenantPurgeRepository) MarkPurged(ctx context.Context, tenantID int64, event audit.Event) (bool, error) {
	var marked bool
	err := r.tx.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := identitysqlc.New(tx).MarkTenantPurged(ctx, tenantID)
		if err != nil {
			return err
		}
		if rows == 0 {
			return nil
		}
		marked = true
		return audit.RecordInTx(ctx, r.audit, tx, event)
	})
	if err != nil {
		return false, err
	}
	return marked, nil
}
