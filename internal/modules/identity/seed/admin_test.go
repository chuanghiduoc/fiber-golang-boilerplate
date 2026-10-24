package seed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// fakeUsers implements only what the seed may call. The embedded nil
// interface makes any other call (UpdateRole, Restore, ...) panic, so a test
// fails loudly if the seed ever promotes or restores an account.
type fakeUsers struct {
	repository.UserRepository

	active    *identitysqlc.User
	deleted   *identitysqlc.User
	lookupErr error

	created  *identitysqlc.CreateAdminUserParams
	verified bool
}

func (f *fakeUsers) GetByEmail(context.Context, string) (*identitysqlc.User, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if f.active == nil {
		return nil, apperror.ErrNotFound
	}
	return f.active, nil
}

func (f *fakeUsers) GetDeletedByEmail(context.Context, string) (*identitysqlc.User, error) {
	if f.deleted == nil {
		return nil, apperror.ErrNotFound
	}
	return f.deleted, nil
}

func (f *fakeUsers) CreateAdmin(_ context.Context, params identitysqlc.CreateAdminUserParams) (*identitysqlc.User, error) {
	f.created = &params
	return &identitysqlc.User{ID: 1, Email: params.Email, Role: dto.RoleAdmin}, nil
}

func (f *fakeUsers) VerifyEmail(_ context.Context, id int64, email string) (*identitysqlc.User, error) {
	f.verified = true
	return &identitysqlc.User{ID: id, Email: email, Role: dto.RoleAdmin}, nil
}

const seedPassword = "Configured1!"

func seedConfig() config.AdminConfig {
	return config.AdminConfig{Email: "Admin@Example.com", Password: seedPassword, Name: "Admin"}
}

func passwordHash(t *testing.T, password string) pgtype.Text {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	return pgtype.Text{String: string(hash), Valid: true}
}

func TestAdminCreatesVerifiedAdminWhenAbsent(t *testing.T) {
	repo := &fakeUsers{}
	require.NoError(t, Admin(context.Background(), seedConfig(), repo, 7))

	require.NotNil(t, repo.created, "absent admin must be created")
	assert.Equal(t, "admin@example.com", repo.created.Email)
	assert.Equal(t, int64(7), repo.created.TenantID)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(repo.created.PasswordHash.String), []byte(seedPassword)))
}

func TestAdminVerifiesExistingUnverifiedAdmin(t *testing.T) {
	repo := &fakeUsers{active: &identitysqlc.User{ID: 3, Email: "admin@example.com", Role: dto.RoleAdmin}}
	require.NoError(t, Admin(context.Background(), seedConfig(), repo))
	assert.True(t, repo.verified)
	assert.Nil(t, repo.created)
}

// A demoted account must never crash startup and never be re-promoted,
// whether or not its password still matches ADMIN_PASSWORD.
func TestAdminSkipsDemotedAccount(t *testing.T) {
	for name, password := range map[string]string{
		"password changed":       "SomethingElse1!",
		"password still matches": seedPassword,
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeUsers{active: &identitysqlc.User{
				ID: 3, Email: "admin@example.com", Role: dto.RoleUser,
				PasswordHash:    passwordHash(t, password),
				EmailVerifiedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			}}
			require.NoError(t, Admin(context.Background(), seedConfig(), repo))
			assert.Nil(t, repo.created)
			assert.False(t, repo.verified)
		})
	}
}

func TestAdminSkipsBannedOrDeletedAccount(t *testing.T) {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	for name, user := range map[string]*identitysqlc.User{
		"banned":       {ID: 3, Email: "admin@example.com", Role: dto.RoleAdmin, DeletedAt: now, BannedAt: now},
		"self-deleted": {ID: 3, Email: "admin@example.com", Role: dto.RoleAdmin, DeletedAt: now},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeUsers{deleted: user}
			require.NoError(t, Admin(context.Background(), seedConfig(), repo))
			assert.Nil(t, repo.created, "a removed admin must not be resurrected")
		})
	}
}

func TestAdminReturnsLookupErrors(t *testing.T) {
	repo := &fakeUsers{lookupErr: errors.New("connection refused")}
	require.Error(t, Admin(context.Background(), seedConfig(), repo))
	assert.Nil(t, repo.created)
}
