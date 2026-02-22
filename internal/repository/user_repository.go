package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

type UserRepository interface {
	GetByID(ctx context.Context, id int64) (*sqlc.User, error)
	GetByEmail(ctx context.Context, email string) (*sqlc.User, error)
	GetByGoogleID(ctx context.Context, googleID string) (*sqlc.User, error)
	List(ctx context.Context, limit, offset int32) ([]sqlc.User, error)
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, params sqlc.CreateUserParams) (*sqlc.User, error)
	CreateOAuthUser(ctx context.Context, params sqlc.CreateOAuthUserParams) (*sqlc.User, error)
	Update(ctx context.Context, params sqlc.UpdateUserParams) (*sqlc.User, error)
	UpdatePassword(ctx context.Context, params sqlc.UpdateUserPasswordParams) (*sqlc.User, error)
	UpdateRole(ctx context.Context, params sqlc.UpdateUserRoleParams) (*sqlc.User, error)
	VerifyEmail(ctx context.Context, id int64) (*sqlc.User, error)
	LinkGoogleAccount(ctx context.Context, params sqlc.LinkGoogleAccountParams) (*sqlc.User, error)
	Delete(ctx context.Context, id int64) (*sqlc.User, error)
	Restore(ctx context.Context, id int64) (*sqlc.User, error)
	AdminList(ctx context.Context, limit, offset int32) ([]sqlc.User, error)
	AdminCount(ctx context.Context) (int64, error)
	GetSystemStats(ctx context.Context) (sqlc.GetSystemStatsRow, error)
}

type userRepository struct {
	q *sqlc.Queries
}

func NewUserRepository(db sqlc.DBTX) UserRepository {
	return &userRepository{
		q: sqlc.New(db),
	}
}

func (r *userRepository) GetByID(ctx context.Context, id int64) (*sqlc.User, error) {
	user, err := r.q.GetUserByID(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) GetByEmail(ctx context.Context, email string) (*sqlc.User, error) {
	user, err := r.q.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) GetByGoogleID(ctx context.Context, googleID string) (*sqlc.User, error) {
	user, err := r.q.GetUserByGoogleID(ctx, pgtype.Text{String: googleID, Valid: true})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) List(ctx context.Context, limit, offset int32) ([]sqlc.User, error) {
	return r.q.ListUsers(ctx, sqlc.ListUsersParams{
		Limit:  limit,
		Offset: offset,
	})
}

func (r *userRepository) Count(ctx context.Context) (int64, error) {
	return r.q.CountUsers(ctx)
}

func (r *userRepository) Create(ctx context.Context, params sqlc.CreateUserParams) (*sqlc.User, error) {
	user, err := r.q.CreateUser(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) CreateOAuthUser(ctx context.Context, params sqlc.CreateOAuthUserParams) (*sqlc.User, error) {
	user, err := r.q.CreateOAuthUser(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) Update(ctx context.Context, params sqlc.UpdateUserParams) (*sqlc.User, error) {
	user, err := r.q.UpdateUser(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) LinkGoogleAccount(ctx context.Context, params sqlc.LinkGoogleAccountParams) (*sqlc.User, error) {
	user, err := r.q.LinkGoogleAccount(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) UpdatePassword(ctx context.Context, params sqlc.UpdateUserPasswordParams) (*sqlc.User, error) {
	user, err := r.q.UpdateUserPassword(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) UpdateRole(ctx context.Context, params sqlc.UpdateUserRoleParams) (*sqlc.User, error) {
	user, err := r.q.UpdateUserRole(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) VerifyEmail(ctx context.Context, id int64) (*sqlc.User, error) {
	user, err := r.q.VerifyUserEmail(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) Delete(ctx context.Context, id int64) (*sqlc.User, error) {
	user, err := r.q.DeleteUser(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) Restore(ctx context.Context, id int64) (*sqlc.User, error) {
	user, err := r.q.RestoreUser(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &user, nil
}

func (r *userRepository) AdminList(ctx context.Context, limit, offset int32) ([]sqlc.User, error) {
	return r.q.AdminListUsers(ctx, sqlc.AdminListUsersParams{
		Limit:  limit,
		Offset: offset,
	})
}

func (r *userRepository) AdminCount(ctx context.Context) (int64, error) {
	return r.q.AdminCountUsers(ctx)
}

func (r *userRepository) GetSystemStats(ctx context.Context) (sqlc.GetSystemStatsRow, error) {
	return r.q.GetSystemStats(ctx)
}
