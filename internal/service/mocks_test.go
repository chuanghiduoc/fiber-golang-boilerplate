package service

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// cursorLess reports whether (ts1,id1) sorts before (ts2,id2) in the
// (created_at DESC, id DESC) order used by cursor pagination.
func cursorLess(ts1 time.Time, id1 int64, ts2 time.Time, id2 int64) bool {
	if ts1.Equal(ts2) {
		return id1 < id2
	}
	return ts1.Before(ts2)
}

// ---------------------------------------------------------------------------
// mockUserRepo
// ---------------------------------------------------------------------------

type mockUserRepo struct {
	users  map[int64]*sqlc.User
	nextID int64
}

func newMockUserRepo() *mockUserRepo {
	return &mockUserRepo{users: make(map[int64]*sqlc.User), nextID: 1}
}

func (m *mockUserRepo) GetByID(_ context.Context, id int64) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	return u, nil
}

func (m *mockUserRepo) GetByEmail(_ context.Context, addr string) (*sqlc.User, error) {
	for _, u := range m.users {
		if !u.DeletedAt.Valid && strings.EqualFold(u.Email, addr) {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) GetByGoogleID(_ context.Context, googleID string) (*sqlc.User, error) {
	for _, u := range m.users {
		if !u.DeletedAt.Valid && u.GoogleID.Valid && u.GoogleID.String == googleID {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) GetBannedByEmail(_ context.Context, addr string) (*sqlc.User, error) {
	for _, u := range m.users {
		if u.BannedAt.Valid && strings.EqualFold(u.Email, addr) {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) List(_ context.Context, limit, offset int32) ([]sqlc.User, error) {
	all := make([]sqlc.User, 0, len(m.users))
	for _, u := range m.users {
		if u.DeletedAt.Valid {
			continue
		}
		all = append(all, *u)
	}
	start := int(offset)
	if start > len(all) {
		return nil, nil
	}
	end := start + int(limit)
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], nil
}

func (m *mockUserRepo) ListCursor(_ context.Context, cur pagination.DBCursor) ([]sqlc.User, error) {
	return m.listCursor(cur, false), nil
}

func (m *mockUserRepo) AdminListCursor(_ context.Context, cur pagination.DBCursor) ([]sqlc.User, error) {
	return m.listCursor(cur, true), nil
}

func (m *mockUserRepo) listCursor(cur pagination.DBCursor, includeDeleted bool) []sqlc.User {
	all := make([]sqlc.User, 0, len(m.users))
	for _, u := range m.users {
		if u.DeletedAt.Valid && !includeDeleted {
			continue
		}
		all = append(all, *u)
	}
	// (created_at DESC, id DESC)
	sort.Slice(all, func(i, j int) bool {
		return cursorLess(all[j].CreatedAt.Time, all[j].ID, all[i].CreatedAt.Time, all[i].ID)
	})
	out := make([]sqlc.User, 0, len(all))
	for _, u := range all {
		if cur.HasCursor && !cursorLess(u.CreatedAt.Time, u.ID, cur.CreatedAt, cur.ID) {
			continue
		}
		out = append(out, u)
		if len(out) >= int(cur.Limit) {
			break
		}
	}
	return out
}

func (m *mockUserRepo) Count(_ context.Context) (int64, error) {
	var count int64
	for _, u := range m.users {
		if !u.DeletedAt.Valid {
			count++
		}
	}
	return count, nil
}

func (m *mockUserRepo) Create(_ context.Context, params sqlc.CreateUserParams) (*sqlc.User, error) {
	// Enforce active, case-insensitive email uniqueness like the real schema.
	for _, u := range m.users {
		if !u.DeletedAt.Valid && strings.EqualFold(u.Email, params.Email) {
			return nil, fmt.Errorf("duplicate key value violates unique constraint \"idx_users_email_lower_active\"")
		}
	}
	u := &sqlc.User{
		ID:           m.nextID,
		Email:        params.Email,
		PasswordHash: params.PasswordHash,
		Name:         params.Name,
		AuthProvider: "local",
		Role:         "user",
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	m.users[m.nextID] = u
	m.nextID++
	return u, nil
}

func (m *mockUserRepo) CreateOAuthUser(_ context.Context, params sqlc.CreateOAuthUserParams) (*sqlc.User, error) {
	// Enforce active, case-insensitive email uniqueness like the real schema.
	for _, u := range m.users {
		if !u.DeletedAt.Valid && strings.EqualFold(u.Email, params.Email) {
			return nil, fmt.Errorf("duplicate key value violates unique constraint \"idx_users_email_lower_active\"")
		}
	}
	u := &sqlc.User{
		ID:           m.nextID,
		Email:        params.Email,
		Name:         params.Name,
		GoogleID:     params.GoogleID,
		AuthProvider: params.AuthProvider,
		Role:         "user",
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	m.users[m.nextID] = u
	m.nextID++
	return u, nil
}

func (m *mockUserRepo) Update(_ context.Context, params sqlc.UpdateUserParams) (*sqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok || u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	u.Name = params.Name
	if params.UpdateEmail {
		if !strings.EqualFold(u.Email, params.Email) {
			u.EmailVerifiedAt = pgtype.Timestamptz{}
			u.TokenVersion++
			u.PasswordChangedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}
		u.Email = params.Email
	}
	return u, nil
}

func (m *mockUserRepo) UpdatePassword(_ context.Context, params sqlc.UpdateUserPasswordParams) (*sqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok || u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	u.PasswordHash = params.PasswordHash
	u.TokenVersion++
	u.PasswordChangedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) RevokeSessions(_ context.Context, id int64) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	u.TokenVersion++
	return u, nil
}

func (m *mockUserRepo) UpdateRole(_ context.Context, params sqlc.UpdateUserRoleParams) (*sqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok || u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	u.Role = params.Role
	u.TokenVersion++
	return u, nil
}

func (m *mockUserRepo) VerifyEmail(_ context.Context, id int64, addr string) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok || !strings.EqualFold(u.Email, addr) {
		return nil, apperror.ErrNotFound
	}
	u.EmailVerifiedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) LinkGoogleAccount(_ context.Context, params sqlc.LinkGoogleAccountParams) (*sqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	u.GoogleID = params.GoogleID
	u.AuthProvider = "google"
	return u, nil
}

func (m *mockUserRepo) Delete(_ context.Context, id int64) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	u.DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	u.TokenVersion++
	u.PasswordChangedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) Ban(_ context.Context, id int64) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	u.DeletedAt = now
	u.BannedAt = now
	u.TokenVersion++
	u.PasswordChangedAt = now
	return u, nil
}

func (m *mockUserRepo) Restore(_ context.Context, id int64) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok || !u.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	u.DeletedAt = pgtype.Timestamptz{}
	u.BannedAt = pgtype.Timestamptz{}
	u.TokenVersion++
	u.PasswordChangedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) AdminList(ctx context.Context, limit, offset int32) ([]sqlc.User, error) {
	return m.List(ctx, limit, offset)
}

func (m *mockUserRepo) AdminCount(_ context.Context) (int64, error) {
	return int64(len(m.users)), nil
}

func (m *mockUserRepo) CountActiveAdmins(_ context.Context) (int64, error) {
	var count int64
	for _, u := range m.users {
		if !u.DeletedAt.Valid && u.Role == "admin" {
			count++
		}
	}
	return count, nil
}

func (m *mockUserRepo) GetSystemStats(_ context.Context) (sqlc.GetSystemStatsRow, error) {
	return sqlc.GetSystemStatsRow{ActiveUsers: int64(len(m.users))}, nil
}

// ---------------------------------------------------------------------------
// mockRefreshTokenRepo
// ---------------------------------------------------------------------------

type mockRefreshTokenRepo struct {
	tokens         map[string]*sqlc.RefreshToken
	deletedUserIDs []int64
}

func newMockRefreshTokenRepo() *mockRefreshTokenRepo {
	return &mockRefreshTokenRepo{tokens: make(map[string]*sqlc.RefreshToken)}
}

func (m *mockRefreshTokenRepo) Create(_ context.Context, params sqlc.CreateRefreshTokenParams) (*sqlc.RefreshToken, error) {
	rt := &sqlc.RefreshToken{
		UserID:       params.UserID,
		Token:        params.Token,
		ExpiresAt:    params.ExpiresAt,
		TokenVersion: params.TokenVersion,
	}
	m.tokens[params.Token] = rt
	return rt, nil
}

func (m *mockRefreshTokenRepo) GetByToken(_ context.Context, token string) (*sqlc.RefreshToken, error) {
	rt, ok := m.tokens[token]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return rt, nil
}

func (m *mockRefreshTokenRepo) Consume(_ context.Context, token string) (*sqlc.RefreshToken, error) {
	rt, ok := m.tokens[token]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	delete(m.tokens, token)
	return rt, nil
}

func (m *mockRefreshTokenRepo) Delete(_ context.Context, token string) error {
	delete(m.tokens, token)
	return nil
}

func (m *mockRefreshTokenRepo) DeleteByUserID(_ context.Context, userID int64) error {
	m.deletedUserIDs = append(m.deletedUserIDs, userID)
	for k, v := range m.tokens {
		if v.UserID == userID {
			delete(m.tokens, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// mockFileRepo
// ---------------------------------------------------------------------------

type mockFileRepo struct {
	files  map[int64]*sqlc.File
	nextID int64
}

var _ filesrepo.FileRepository = (*mockFileRepo)(nil)

func newMockFileRepo() *mockFileRepo {
	return &mockFileRepo{files: make(map[int64]*sqlc.File), nextID: 1}
}

func (m *mockFileRepo) Create(_ context.Context, params sqlc.CreateFileParams) (*sqlc.File, error) {
	f := &sqlc.File{
		ID:           m.nextID,
		UserID:       params.UserID,
		OriginalName: params.OriginalName,
		StoragePath:  params.StoragePath,
		MimeType:     params.MimeType,
		Size:         params.Size,
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	m.files[m.nextID] = f
	m.nextID++
	return f, nil
}

func (m *mockFileRepo) GetByID(_ context.Context, id int64) (*sqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *mockFileRepo) GetByIDIncludingDeleted(_ context.Context, id int64) (*sqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *mockFileRepo) ListByUserID(_ context.Context, userID int64, limit, offset int32) ([]sqlc.File, error) {
	var result []sqlc.File
	for _, f := range m.files {
		if f.UserID == userID {
			result = append(result, *f)
		}
	}
	start := int(offset)
	if start > len(result) {
		return nil, nil
	}
	end := start + int(limit)
	if end > len(result) {
		end = len(result)
	}
	return result[start:end], nil
}

func (m *mockFileRepo) ListByUserCursor(_ context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]sqlc.File, error) {
	return m.listCursor(&userID, cur, includeDeleted), nil
}

func (m *mockFileRepo) AdminListCursor(_ context.Context, cur pagination.DBCursor) ([]sqlc.File, error) {
	return m.listCursor(nil, cur, true), nil
}

func (m *mockFileRepo) listCursor(userID *int64, cur pagination.DBCursor, includeDeleted bool) []sqlc.File {
	all := make([]sqlc.File, 0, len(m.files))
	for _, f := range m.files {
		if userID != nil && f.UserID != *userID {
			continue
		}
		if !includeDeleted && f.DeletedAt.Valid {
			continue
		}
		all = append(all, *f)
	}
	sort.Slice(all, func(i, j int) bool {
		return cursorLess(all[j].CreatedAt.Time, all[j].ID, all[i].CreatedAt.Time, all[i].ID)
	})
	out := make([]sqlc.File, 0, len(all))
	for _, f := range all {
		if cur.HasCursor && !cursorLess(f.CreatedAt.Time, f.ID, cur.CreatedAt, cur.ID) {
			continue
		}
		out = append(out, f)
		if len(out) >= int(cur.Limit) {
			break
		}
	}
	return out
}

func (m *mockFileRepo) CountByUserID(_ context.Context, userID int64) (int64, error) {
	var count int64
	for _, f := range m.files {
		if f.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (m *mockFileRepo) Delete(_ context.Context, id int64) (*sqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return f, nil
}

func (m *mockFileRepo) Restore(_ context.Context, id int64) (*sqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{}
	return f, nil
}

func (m *mockFileRepo) AdminList(_ context.Context, limit, offset int32) ([]sqlc.File, error) {
	all := make([]sqlc.File, 0, len(m.files))
	for _, f := range m.files {
		all = append(all, *f)
	}
	start := int(offset)
	if start > len(all) {
		return nil, nil
	}
	end := start + int(limit)
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], nil
}

func (m *mockFileRepo) AdminCount(_ context.Context) (int64, error) {
	return int64(len(m.files)), nil
}

// ---------------------------------------------------------------------------
// mockStorage
// ---------------------------------------------------------------------------

type mockStorage struct {
	files        map[string][]byte
	deletedPaths []string
	putErr       error
	getErr       error
	delErr       error
	baseURL      string
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		files:   make(map[string][]byte),
		baseURL: "http://localhost:8080/files",
	}
}

func (m *mockStorage) Put(_ context.Context, path string, reader io.Reader, _ int64, _ string) error {
	if m.putErr != nil {
		return m.putErr
	}
	data, _ := io.ReadAll(reader)
	m.files[path] = data
	return nil
}

func (m *mockStorage) Get(_ context.Context, path string) (io.ReadCloser, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	data, ok := m.files[path]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return io.NopCloser(io.NewSectionReader(readerAt(data), 0, int64(len(data)))), nil
}

func (m *mockStorage) Delete(_ context.Context, path string) error {
	if m.delErr != nil {
		return m.delErr
	}
	m.deletedPaths = append(m.deletedPaths, path)
	delete(m.files, path)
	return nil
}

func (m *mockStorage) URL(path string) string {
	return m.baseURL + "/" + path
}

// readerAt wraps []byte to implement io.ReaderAt
type readerAt []byte

func (r readerAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(r)) {
		return 0, io.EOF
	}
	n = copy(p, r[off:])
	if n < len(p) {
		err = io.EOF
	}
	return
}
