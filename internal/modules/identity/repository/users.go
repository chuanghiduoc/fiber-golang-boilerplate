package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/google/uuid"
)

// Unique constraints/indexes the user repository maps to service sentinels.
const (
	uniqueUsersEmailLive          = "idx_users_email_live"
	uniqueIdentityProviderSubject = "uq_user_identities_provider_subject"
	uniqueIdentityUserProvider    = "uq_user_identities_user_provider"
)

// UserRepository implements service.UserRepository. Users are global
// identities, so no query takes a tenant.
type UserRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.UserRepository = (*UserRepository)(nil)

func NewUserRepository(db identitysqlc.DBTX) *UserRepository {
	return &UserRepository{q: identitysqlc.New(db)}
}

func toUser(row identitysqlc.User, hasPassword bool) *identityservice.User {
	return &identityservice.User{
		ID:                   row.ID,
		Email:                row.Email,
		Name:                 row.Name,
		EmailVerifiedAt:      database.TimePtr(row.EmailVerifiedAt),
		Status:               identityservice.UserStatus(row.Status),
		TokenVersion:         row.TokenVersion,
		CredentialsChangedAt: database.TimePtr(row.CredentialsChangedAt),
		HasPassword:          hasPassword,
		CreatedAt:            row.CreatedAt.Time,
		UpdatedAt:            row.UpdatedAt.Time,
		DeletedAt:            database.TimePtr(row.DeletedAt),
	}
}

func (r *UserRepository) GetByID(ctx context.Context, id uuid.UUID) (*identityservice.User, error) {
	row, err := r.q.GetUserByID(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toUser(row.User, row.HasPassword), nil
}

func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*identityservice.User, error) {
	row, err := r.q.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toUser(row.User, row.HasPassword), nil
}

func (r *UserRepository) GetByIdentity(ctx context.Context, provider, subject string) (*identityservice.User, error) {
	row, err := r.q.GetUserByIdentity(ctx, identitysqlc.GetUserByIdentityParams{Provider: provider, Subject: subject})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toUser(row.User, row.HasPassword), nil
}

func (r *UserRepository) Create(ctx context.Context, user identityservice.NewUser) (*identityservice.User, error) {
	row, err := r.q.CreateUser(ctx, identitysqlc.CreateUserParams{
		Email:         user.Email,
		Name:          user.Name,
		EmailVerified: user.EmailVerified,
	})
	if err != nil {
		if name, ok := uniqueConstraint(err); ok && name == uniqueUsersEmailLive {
			return nil, identityservice.ErrEmailTaken
		}
		return nil, wrapErr(err)
	}
	return toUser(row, false), nil
}

func (r *UserRepository) CreatePassword(ctx context.Context, userID uuid.UUID, hash string) error {
	if err := r.q.CreateUserPassword(ctx, identitysqlc.CreateUserPasswordParams{UserID: userID, PasswordHash: hash}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *UserRepository) PasswordHash(ctx context.Context, userID uuid.UUID) (string, error) {
	hash, err := r.q.GetUserPasswordHash(ctx, userID)
	if err != nil {
		return "", wrapErr(err)
	}
	return hash, nil
}

// RehashPassword replaces the stored hash without a credential change, so a
// sign-in that upgrades an old hash does not end the user's sessions. The
// update is conditional on the hash that verified: a concurrent password
// change wins and the stale upgrade is dropped instead of overwriting it.
func (r *UserRepository) RehashPassword(ctx context.Context, userID uuid.UUID, currentHash, newHash string) error {
	if _, err := r.q.RehashUserPassword(ctx, identitysqlc.RehashUserPasswordParams{
		UserID: userID, CurrentHash: currentHash, NewHash: newHash,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *UserRepository) ReplacePassword(ctx context.Context, userID uuid.UUID, hash string) (*identityservice.User, error) {
	if err := r.q.UpsertUserPassword(ctx, identitysqlc.UpsertUserPasswordParams{UserID: userID, PasswordHash: hash}); err != nil {
		return nil, wrapErr(err)
	}
	row, err := r.q.RecordUserCredentialChange(ctx, userID)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toUser(row, true), nil
}

func (r *UserRepository) LinkIdentity(ctx context.Context, identity identityservice.ExternalIdentity) error {
	err := r.q.CreateUserIdentity(ctx, identitysqlc.CreateUserIdentityParams{
		UserID:   identity.UserID,
		Provider: identity.Provider,
		Subject:  identity.Subject,
		Email:    identity.Email,
	})
	if err == nil {
		return nil
	}
	switch name, _ := uniqueConstraint(err); name {
	case uniqueIdentityProviderSubject:
		return identityservice.ErrIdentityTaken
	case uniqueIdentityUserProvider:
		return identityservice.ErrIdentityConflict
	default:
		return wrapErr(err)
	}
}

func (r *UserRepository) UpdateProfile(ctx context.Context, update identityservice.ProfileUpdate) (*identityservice.User, error) {
	row, err := r.q.UpdateUserProfile(ctx, identitysqlc.UpdateUserProfileParams{
		ID:          update.ID,
		Name:        update.Name,
		Email:       update.Email,
		UpdateEmail: update.UpdateEmail,
	})
	if err != nil {
		if name, ok := uniqueConstraint(err); ok && name == uniqueUsersEmailLive {
			return nil, identityservice.ErrEmailTaken
		}
		return nil, wrapErr(err)
	}
	return r.withPasswordFlag(ctx, row)
}

func (r *UserRepository) VerifyEmail(ctx context.Context, id uuid.UUID, email string) (*identityservice.User, error) {
	row, err := r.q.VerifyUserEmail(ctx, identitysqlc.VerifyUserEmailParams{ID: id, Email: email})
	if err != nil {
		return nil, wrapErr(err)
	}
	return r.withPasswordFlag(ctx, row)
}

func (r *UserRepository) RevokeTokens(ctx context.Context, id uuid.UUID) (*identityservice.User, error) {
	row, err := r.q.RevokeUserTokens(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return r.withPasswordFlag(ctx, row)
}

func (r *UserRepository) MarkDeleted(ctx context.Context, id uuid.UUID) (*identityservice.User, error) {
	row, err := r.q.MarkUserDeleted(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toUser(row, false), nil
}

// withPasswordFlag completes an UPDATE ... RETURNING row with HasPassword,
// which the users row does not carry.
func (r *UserRepository) withPasswordFlag(ctx context.Context, row identitysqlc.User) (*identityservice.User, error) {
	user := toUser(row, false)
	if _, err := r.q.GetUserPasswordHash(ctx, row.ID); err == nil {
		user.HasPassword = true
	} else if wrapped := wrapErr(err); wrapped != nil && !isNotFound(wrapped) {
		return nil, wrapped
	}
	return user, nil
}
