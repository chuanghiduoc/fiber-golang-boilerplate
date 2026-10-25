package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// DataExportRepository implements service.DataExportRepository over sqlc.
type DataExportRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.DataExportRepository = (*DataExportRepository)(nil)

func NewDataExportRepository(db identitysqlc.DBTX) *DataExportRepository {
	return &DataExportRepository{q: identitysqlc.New(db)}
}

func toDataExport(row identitysqlc.DataExport) identityservice.DataExport {
	return identityservice.DataExport{
		ID:          row.ID,
		TenantID:    row.TenantID,
		UserID:      row.UserID,
		Status:      identityservice.DataExportStatus(row.Status),
		StoragePath: row.StoragePath.String,
		ExpiresAt:   timePtr(row.ExpiresAt),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
}

func (r *DataExportRepository) Create(ctx context.Context, userID int64) (*identityservice.DataExport, error) {
	row, err := r.q.CreateDataExport(ctx, identitysqlc.CreateDataExportParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		if _, ok := uniqueConstraint(err); ok {
			return nil, identityservice.ErrDataExportInProgress
		}
		return nil, wrapErr(err)
	}
	export := toDataExport(row)
	return &export, nil
}

func (r *DataExportRepository) Get(ctx context.Context, userID, id int64) (*identityservice.DataExport, error) {
	row, err := r.q.GetDataExport(ctx, identitysqlc.GetDataExportParams{
		ID:       id,
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	export := toDataExport(row)
	return &export, nil
}

func (r *DataExportRepository) ListForUser(ctx context.Context, userID int64) ([]identityservice.DataExport, error) {
	rows, err := r.q.ListDataExportsForUser(ctx, identitysqlc.ListDataExportsForUserParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]identityservice.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) ListCursor(ctx context.Context, userID int64, cur pagination.DBCursor) ([]identityservice.DataExport, error) {
	rows, err := r.q.ListDataExportsCursor(ctx, identitysqlc.ListDataExportsCursorParams{
		TenantID:        tenantID(ctx),
		UserID:          userID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]identityservice.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) MarkRunning(ctx context.Context, userID, id int64) (bool, error) {
	n, err := r.q.MarkDataExportRunning(ctx, identitysqlc.MarkDataExportRunningParams{
		ID:       id,
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	return n > 0, wrapErr(err)
}

func (r *DataExportRepository) MarkReady(ctx context.Context, userID, id int64, storagePath string, expiresAt time.Time) (bool, error) {
	n, err := r.q.MarkDataExportReady(ctx, identitysqlc.MarkDataExportReadyParams{
		StoragePath: storagePath,
		ExpiresAt:   timestamptz(expiresAt),
		ID:          id,
		TenantID:    tenantID(ctx),
		UserID:      userID,
	})
	return n > 0, wrapErr(err)
}

func (r *DataExportRepository) MarkFailed(ctx context.Context, userID, id int64) error {
	return wrapErr(r.q.MarkDataExportFailed(ctx, identitysqlc.MarkDataExportFailedParams{
		ID:       id,
		TenantID: tenantID(ctx),
		UserID:   userID,
	}))
}

func (r *DataExportRepository) ExpireForUser(ctx context.Context, userID int64) ([]identityservice.DataExport, error) {
	rows, err := r.q.ExpireDataExportsForUser(ctx, identitysqlc.ExpireDataExportsForUserParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]identityservice.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) DeleteForUser(ctx context.Context, userID int64) error {
	return wrapErr(r.q.DeleteDataExportsForUser(ctx, identitysqlc.DeleteDataExportsForUserParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	}))
}

func (r *DataExportRepository) Purgeable(ctx context.Context, limit int32) ([]identityservice.DataExport, error) {
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	rows, err := r.q.ListPurgeableDataExports(ctx, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	exports := make([]identityservice.DataExport, len(rows))
	for i := range rows {
		exports[i] = toDataExport(rows[i])
	}
	return exports, nil
}

func (r *DataExportRepository) Delete(ctx context.Context, id int64) error {
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return wrapErr(r.q.DeleteDataExport(ctx, id))
}

func (r *DataExportRepository) ListSessions(ctx context.Context, userID int64) ([]identityservice.Session, error) {
	rows, err := r.q.ListUserSessionsForPrivacy(ctx, identitysqlc.ListUserSessionsForPrivacyParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	sessions := make([]identityservice.Session, len(rows))
	for i := range rows {
		sessions[i] = toSession(rows[i])
	}
	return sessions, nil
}

func (r *DataExportRepository) AnonymizeUser(ctx context.Context, userID int64, email string) (*identityservice.User, error) {
	row, err := r.q.AnonymizeUser(ctx, identitysqlc.AnonymizeUserParams{Email: email, ID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperror.ErrNotFound
		}
		return nil, wrapErr(err)
	}
	return toUser(row, false), nil
}

func (r *DataExportRepository) DeleteCredentialsAndIdentities(ctx context.Context, userID int64) error {
	if err := r.q.DeleteUserPasswords(ctx, userID); err != nil {
		return wrapErr(err)
	}
	return wrapErr(r.q.DeleteUserIdentities(ctx, userID))
}

func (r *DataExportRepository) DeleteSessionsInTenant(ctx context.Context, userID int64) error {
	return wrapErr(r.q.DeleteTenantUserSessions(ctx, identitysqlc.DeleteTenantUserSessionsParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	}))
}

func (r *DataExportRepository) DeleteSessionsForUser(ctx context.Context, userID int64) error {
	ctx = database.WithRLSBypass(ctx) // The finalizer removes the user's sessions in every tenant.
	return wrapErr(r.q.DeleteUserSessions(ctx, userID))
}
