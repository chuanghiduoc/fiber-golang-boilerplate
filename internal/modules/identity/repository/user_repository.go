package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

type UserRepository interface {
	GetByID(ctx context.Context, id int64) (*identitysqlc.User, error)
	// GetSessionState returns the current authorization state the platform
	// session validator needs without exposing generated SQL types outside the
	// module. The lookup is by user ID only so the validator can detect a
	// token whose tenant claim no longer matches the stored row.
	GetSessionState(ctx context.Context, id int64) (role string, tokenVersion int32, tenantID int64, tenantStatus string, err error)
	GetByEmail(ctx context.Context, email string) (*identitysqlc.User, error)
	GetByGoogleID(ctx context.Context, googleID string) (*identitysqlc.User, error)
	GetBannedByEmail(ctx context.Context, email string) (*identitysqlc.User, error)
	List(ctx context.Context, limit, offset int32) ([]identitysqlc.User, error)
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]identitysqlc.User, error)
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, params identitysqlc.CreateUserParams) (*identitysqlc.User, error)
	CreateOAuthUser(ctx context.Context, params identitysqlc.CreateOAuthUserParams) (*identitysqlc.User, error)
	Update(ctx context.Context, params identitysqlc.UpdateUserParams) (*identitysqlc.User, error)
	UpdatePassword(ctx context.Context, params identitysqlc.UpdateUserPasswordParams) (*identitysqlc.User, error)
	UpdateRole(ctx context.Context, params identitysqlc.UpdateUserRoleParams) (*identitysqlc.User, error)
	RevokeSessions(ctx context.Context, id int64) (*identitysqlc.User, error)
	VerifyEmail(ctx context.Context, id int64, email string) (*identitysqlc.User, error)
	LinkGoogleAccount(ctx context.Context, params identitysqlc.LinkGoogleAccountParams) (*identitysqlc.User, error)
	Delete(ctx context.Context, id int64) (*identitysqlc.User, error)
	Ban(ctx context.Context, id int64) (*identitysqlc.User, error)
	Restore(ctx context.Context, id int64) (*identitysqlc.User, error)
	AdminList(ctx context.Context, limit, offset int32) ([]identitysqlc.User, error)
	AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]identitysqlc.User, error)
	AdminCount(ctx context.Context) (int64, error)
	CountActiveAdmins(ctx context.Context) (int64, error)
	AdminUserStats(ctx context.Context) (identitysqlc.AdminUserStatsRow, error)
	// GetPrincipal returns the active user's authorization view plus the
	// owning tenant's status, for API-key authentication.
	GetPrincipal(ctx context.Context, id int64) (*identitysqlc.GetUserPrincipalRow, error)
	// LockAdminChanges takes a transaction-scoped per-tenant lock that
	// serializes admin demotion/ban/delete. It is only meaningful on a
	// transaction-backed repository and must run before the target row is
	// updated.
	LockAdminChanges(ctx context.Context) error
}

type userRepository struct {
	q *identitysqlc.Queries
}

func NewUserRepository(db identitysqlc.DBTX) UserRepository {
	return &userRepository{
		q: identitysqlc.New(db),
	}
}

func (r *userRepository) GetByID(ctx context.Context, id int64) (*identitysqlc.User, error) {
	user, err := r.q.GetUserByID(ctx, identitysqlc.GetUserByIDParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) GetSessionState(ctx context.Context, id int64) (role string, tokenVersion int32, tenantID int64, tenantStatus string, err error) {
	state, err := r.q.GetUserSessionState(ctx, id)
	if err != nil {
		return "", 0, 0, "", wrapErr(err)
	}
	return state.Role, state.TokenVersion, state.TenantID, state.Status, nil
}

func (r *userRepository) GetByEmail(ctx context.Context, email string) (*identitysqlc.User, error) {
	user, err := r.q.GetUserByEmail(ctx, identitysqlc.GetUserByEmailParams{
		TenantID: tenantID(ctx),
		Lower:    email,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) GetByGoogleID(ctx context.Context, googleID string) (*identitysqlc.User, error) {
	user, err := r.q.GetUserByGoogleID(ctx, identitysqlc.GetUserByGoogleIDParams{
		TenantID: tenantID(ctx),
		GoogleID: pgtype.Text{String: googleID, Valid: true},
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) GetBannedByEmail(ctx context.Context, email string) (*identitysqlc.User, error) {
	user, err := r.q.GetBannedUserByEmail(ctx, identitysqlc.GetBannedUserByEmailParams{
		TenantID: tenantID(ctx),
		Lower:    email,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) List(ctx context.Context, limit, offset int32) ([]identitysqlc.User, error) {
	return r.q.ListUsers(ctx, identitysqlc.ListUsersParams{
		TenantID: tenantID(ctx),
		Limit:    limit,
		Offset:   offset,
	})
}

func (r *userRepository) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]identitysqlc.User, error) {
	return r.q.ListUsersCursor(ctx, identitysqlc.ListUsersCursorParams{
		TenantID:        tenantID(ctx),
		HasCursor:       cur.HasCursor,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.ID,
		RowLimit:        cur.Limit,
	})
}

func (r *userRepository) Count(ctx context.Context) (int64, error) {
	return r.q.CountUsers(ctx, tenantID(ctx))
}

func (r *userRepository) Create(ctx context.Context, params identitysqlc.CreateUserParams) (*identitysqlc.User, error) {
	params.TenantID = tenantID(ctx)
	user, err := r.q.CreateUser(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) CreateOAuthUser(ctx context.Context, params identitysqlc.CreateOAuthUserParams) (*identitysqlc.User, error) {
	params.TenantID = tenantID(ctx)
	user, err := r.q.CreateOAuthUser(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) Update(ctx context.Context, params identitysqlc.UpdateUserParams) (*identitysqlc.User, error) {
	params.TenantID = tenantID(ctx)
	user, err := r.q.UpdateUser(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) LinkGoogleAccount(ctx context.Context, params identitysqlc.LinkGoogleAccountParams) (*identitysqlc.User, error) {
	params.TenantID = tenantID(ctx)
	user, err := r.q.LinkGoogleAccount(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) UpdatePassword(ctx context.Context, params identitysqlc.UpdateUserPasswordParams) (*identitysqlc.User, error) {
	params.TenantID = tenantID(ctx)
	user, err := r.q.UpdateUserPassword(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) UpdateRole(ctx context.Context, params identitysqlc.UpdateUserRoleParams) (*identitysqlc.User, error) {
	params.TenantID = tenantID(ctx)
	user, err := r.q.UpdateUserRole(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) RevokeSessions(ctx context.Context, id int64) (*identitysqlc.User, error) {
	user, err := r.q.RevokeUserSessions(ctx, identitysqlc.RevokeUserSessionsParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) VerifyEmail(ctx context.Context, id int64, email string) (*identitysqlc.User, error) {
	user, err := r.q.VerifyUserEmail(ctx, identitysqlc.VerifyUserEmailParams{
		ID:       id,
		TenantID: tenantID(ctx),
		Email:    email,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) Delete(ctx context.Context, id int64) (*identitysqlc.User, error) {
	user, err := r.q.DeleteUser(ctx, identitysqlc.DeleteUserParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) Ban(ctx context.Context, id int64) (*identitysqlc.User, error) {
	user, err := r.q.BanUser(ctx, identitysqlc.BanUserParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) Restore(ctx context.Context, id int64) (*identitysqlc.User, error) {
	user, err := r.q.RestoreUser(ctx, identitysqlc.RestoreUserParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) AdminList(ctx context.Context, limit, offset int32) ([]identitysqlc.User, error) {
	return r.q.AdminListUsers(ctx, identitysqlc.AdminListUsersParams{
		TenantID: tenantID(ctx),
		Limit:    limit,
		Offset:   offset,
	})
}

func (r *userRepository) AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]identitysqlc.User, error) {
	return r.q.AdminListUsersCursor(ctx, identitysqlc.AdminListUsersCursorParams{
		TenantID:        tenantID(ctx),
		HasCursor:       cur.HasCursor,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.ID,
		RowLimit:        cur.Limit,
	})
}

func (r *userRepository) AdminCount(ctx context.Context) (int64, error) {
	return r.q.AdminCountUsers(ctx, tenantID(ctx))
}

func (r *userRepository) CountActiveAdmins(ctx context.Context) (int64, error) {
	return r.q.CountActiveAdmins(ctx, tenantID(ctx))
}

func (r *userRepository) AdminUserStats(ctx context.Context) (identitysqlc.AdminUserStatsRow, error) {
	return r.q.AdminUserStats(ctx, tenantID(ctx))
}

func (r *userRepository) GetPrincipal(ctx context.Context, id int64) (*identitysqlc.GetUserPrincipalRow, error) {
	row, err := r.q.GetUserPrincipal(ctx, identitysqlc.GetUserPrincipalParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &row, nil
}

func (r *userRepository) LockAdminChanges(ctx context.Context) error {
	if err := r.q.LockTenantAdminChanges(ctx, tenantID(ctx)); err != nil {
		return wrapErr(err)
	}
	return nil
}
