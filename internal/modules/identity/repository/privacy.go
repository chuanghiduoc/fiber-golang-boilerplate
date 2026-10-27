package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/google/uuid"
)

// DataExportRepository implements core.DataExportRepository over sqlc.
type DataExportRepository struct {
	q *identitysqlc.Queries
}

var _ core.DataExportRepository = (*DataExportRepository)(nil)

func NewDataExportRepository(db identitysqlc.DBTX) *DataExportRepository {
	return &DataExportRepository{q: identitysqlc.New(db)}
}

func toDataExport(row identitysqlc.DataExport) core.DataExport {
	return core.DataExport{
		ID:          row.ID,
		TenantID:    row.TenantID,
		UserID:      row.UserID,
		Status:      core.DataExportStatus(row.Status),
		StoragePath: row.StoragePath.String,
		ExpiresAt:   database.TimePtr(row.ExpiresAt),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
}

func (r *DataExportRepository) Create(ctx context.Context, userID uuid.UUID) (*core.DataExport, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.CreateDataExport(ctx, identitysqlc.CreateDataExportParams{
		TenantID: tenantID,
		UserID:   userID,
	})
	if err != nil {
		if _, ok := uniqueConstraint(err); ok {
			return nil, core.ErrDataExportInProgress
		}
		return nil, wrapErr(err)
	}
	export := toDataExport(row)
	return &export, nil
}

func (r *DataExportRepository) Get(ctx context.Context, userID, id uuid.UUID) (*core.DataExport, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetDataExport(ctx, identitysqlc.GetDataExportParams{
		ID:       id,
		TenantID: tenantID,
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	export := toDataExport(row)
	return &export, nil
}

func (r *DataExportRepository) ListForUser(ctx context.Context, userID uuid.UUID) ([]core.DataExport, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListDataExportsForUser(ctx, identitysqlc.ListDataExportsForUserParams{
		TenantID: tenantID,
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]core.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) ListCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor) ([]core.DataExport, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListDataExportsCursor(ctx, identitysqlc.ListDataExportsCursorParams{
		TenantID:        tenantID,
		UserID:          userID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]core.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) MarkRunning(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := r.q.MarkDataExportRunning(ctx, identitysqlc.MarkDataExportRunningParams{
		ID:       id,
		TenantID: tenantID,
		UserID:   userID,
	})
	return n > 0, wrapErr(err)
}

func (r *DataExportRepository) MarkReady(ctx context.Context, userID, id uuid.UUID, storagePath string, expiresAt time.Time) (bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := r.q.MarkDataExportReady(ctx, identitysqlc.MarkDataExportReadyParams{
		StoragePath: storagePath,
		ExpiresAt:   database.Timestamptz(expiresAt),
		ID:          id,
		TenantID:    tenantID,
		UserID:      userID,
	})
	return n > 0, wrapErr(err)
}

func (r *DataExportRepository) MarkFailed(ctx context.Context, userID, id uuid.UUID) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.q.MarkDataExportFailed(ctx, identitysqlc.MarkDataExportFailedParams{
		ID:       id,
		TenantID: tenantID,
		UserID:   userID,
	}))
}

func (r *DataExportRepository) ExpireForUser(ctx context.Context, userID uuid.UUID) ([]core.DataExport, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ExpireDataExportsForUser(ctx, identitysqlc.ExpireDataExportsForUserParams{
		TenantID: tenantID,
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]core.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) DeleteForUser(ctx context.Context, userID uuid.UUID) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.q.DeleteDataExportsForUser(ctx, identitysqlc.DeleteDataExportsForUserParams{
		TenantID: tenantID,
		UserID:   userID,
	}))
}

func (r *DataExportRepository) Purgeable(ctx context.Context, limit int32) ([]core.DataExport, error) {
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	rows, err := r.q.ListPurgeableDataExports(ctx, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]core.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) Delete(ctx context.Context, id uuid.UUID) error {
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return wrapErr(r.q.DeleteDataExport(ctx, id))
}

func (r *DataExportRepository) ListSessions(ctx context.Context, userID uuid.UUID) ([]core.Session, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListUserSessionsForPrivacy(ctx, identitysqlc.ListUserSessionsForPrivacyParams{
		TenantID: tenantID,
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	sessions := make([]core.Session, len(rows))
	for i := range rows {
		sessions[i] = toSession(rows[i])
	}
	return sessions, nil
}

func (r *DataExportRepository) AnonymizeUser(ctx context.Context, userID uuid.UUID, email string) (*core.User, error) {
	row, err := r.q.AnonymizeUser(ctx, identitysqlc.AnonymizeUserParams{Email: email, ID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperror.ErrNotFound
		}
		return nil, wrapErr(err)
	}
	return toUser(row, false), nil
}

func (r *DataExportRepository) AnonymizeErasedUserInvitations(ctx context.Context, userID uuid.UUID, replacement string) (int64, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := r.q.AnonymizeErasedUserInvitations(ctx, identitysqlc.AnonymizeErasedUserInvitationsParams{
		Replacement: replacement, TenantID: tenantID, UserID: userID,
	})
	if err != nil {
		return 0, wrapErr(err)
	}
	return rows, nil
}

// AnonymizeErasedUserInvitationsAllTenants sweeps every tenant; only the
// global finalizer calls it, under the RLS bypass, so an address invited to a
// tenant the user never joined is erased too.
func (r *DataExportRepository) AnonymizeErasedUserInvitationsAllTenants(ctx context.Context, userID uuid.UUID, replacement string) (int64, error) {
	rows, err := r.q.AnonymizeErasedUserInvitationsAllTenants(ctx,
		identitysqlc.AnonymizeErasedUserInvitationsAllTenantsParams{
			Replacement: replacement, UserID: userID,
		})
	if err != nil {
		return 0, wrapErr(err)
	}
	return rows, nil
}

func (r *DataExportRepository) DeleteCredentialsAndIdentities(ctx context.Context, userID uuid.UUID) error {
	if err := r.q.DeleteUserPasswords(ctx, userID); err != nil {
		return wrapErr(err)
	}
	return wrapErr(r.q.DeleteUserIdentities(ctx, userID))
}

func (r *DataExportRepository) DeleteSessionsInTenant(ctx context.Context, userID uuid.UUID) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	return wrapErr(r.q.DeleteTenantUserSessions(ctx, identitysqlc.DeleteTenantUserSessionsParams{
		TenantID: tenantID,
		UserID:   userID,
	}))
}

func (r *DataExportRepository) DeleteSessionsForUser(ctx context.Context, userID uuid.UUID) error {
	ctx = database.WithRLSBypass(ctx) // The finalizer removes the user's sessions in every tenant.
	return wrapErr(r.q.DeleteUserSessions(ctx, userID))
}
