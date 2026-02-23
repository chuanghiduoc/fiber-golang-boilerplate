package service

import (
	"context"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

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
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return u, nil
}

func (m *mockUserRepo) GetByEmail(_ context.Context, addr string) (*sqlc.User, error) {
	for _, u := range m.users {
		if u.Email == addr {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) GetByGoogleID(_ context.Context, googleID string) (*sqlc.User, error) {
	for _, u := range m.users {
		if u.GoogleID.Valid && u.GoogleID.String == googleID {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) List(_ context.Context, limit, offset int32) ([]sqlc.User, error) {
	all := make([]sqlc.User, 0, len(m.users))
	for _, u := range m.users {
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

func (m *mockUserRepo) Count(_ context.Context) (int64, error) {
	return int64(len(m.users)), nil
}

func (m *mockUserRepo) Create(_ context.Context, params sqlc.CreateUserParams) (*sqlc.User, error) {
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
	if !ok {
		return nil, apperror.ErrNotFound
	}
	u.Name = params.Name
	u.Email = params.Email
	return u, nil
}

func (m *mockUserRepo) UpdatePassword(_ context.Context, params sqlc.UpdateUserPasswordParams) (*sqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	u.PasswordHash = params.PasswordHash
	return u, nil
}

func (m *mockUserRepo) UpdateRole(_ context.Context, params sqlc.UpdateUserRoleParams) (*sqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	u.Role = params.Role
	return u, nil
}

func (m *mockUserRepo) VerifyEmail(_ context.Context, id int64) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok {
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
	if !ok {
		return nil, apperror.ErrNotFound
	}
	delete(m.users, id)
	return u, nil
}

func (m *mockUserRepo) Restore(_ context.Context, id int64) (*sqlc.User, error) {
	u, ok := m.users[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	u.DeletedAt = pgtype.Timestamptz{}
	return u, nil
}

func (m *mockUserRepo) AdminList(ctx context.Context, limit, offset int32) ([]sqlc.User, error) {
	return m.List(ctx, limit, offset)
}

func (m *mockUserRepo) AdminCount(_ context.Context) (int64, error) {
	return int64(len(m.users)), nil
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
		UserID:    params.UserID,
		Token:     params.Token,
		ExpiresAt: params.ExpiresAt,
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
// mockEmailVerificationRepo
// ---------------------------------------------------------------------------

type mockEmailVerificationRepo struct {
	tokens map[string]*sqlc.EmailVerificationToken
	nextID int64
}

func newMockEmailVerificationRepo() *mockEmailVerificationRepo {
	return &mockEmailVerificationRepo{tokens: make(map[string]*sqlc.EmailVerificationToken), nextID: 1}
}

func (m *mockEmailVerificationRepo) Create(_ context.Context, params sqlc.CreateEmailVerificationTokenParams) (*sqlc.EmailVerificationToken, error) {
	t := &sqlc.EmailVerificationToken{
		ID:        m.nextID,
		UserID:    params.UserID,
		Token:     params.Token,
		ExpiresAt: params.ExpiresAt,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	m.tokens[params.Token] = t
	m.nextID++
	return t, nil
}

func (m *mockEmailVerificationRepo) GetByToken(_ context.Context, token string) (*sqlc.EmailVerificationToken, error) {
	t, ok := m.tokens[token]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return t, nil
}

func (m *mockEmailVerificationRepo) Delete(_ context.Context, token string) error {
	delete(m.tokens, token)
	return nil
}

func (m *mockEmailVerificationRepo) DeleteByUserID(_ context.Context, userID int64) error {
	for k, v := range m.tokens {
		if v.UserID == userID {
			delete(m.tokens, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// mockPasswordResetRepo
// ---------------------------------------------------------------------------

type mockPasswordResetRepo struct {
	tokens map[string]*sqlc.PasswordResetToken
	nextID int64
}

func newMockPasswordResetRepo() *mockPasswordResetRepo {
	return &mockPasswordResetRepo{tokens: make(map[string]*sqlc.PasswordResetToken), nextID: 1}
}

func (m *mockPasswordResetRepo) Create(_ context.Context, params sqlc.CreatePasswordResetTokenParams) (*sqlc.PasswordResetToken, error) {
	t := &sqlc.PasswordResetToken{
		ID:        m.nextID,
		UserID:    params.UserID,
		Token:     params.Token,
		ExpiresAt: params.ExpiresAt,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	m.tokens[params.Token] = t
	m.nextID++
	return t, nil
}

func (m *mockPasswordResetRepo) GetByToken(_ context.Context, token string) (*sqlc.PasswordResetToken, error) {
	t, ok := m.tokens[token]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return t, nil
}

func (m *mockPasswordResetRepo) GetByTokenForUpdate(ctx context.Context, token string) (*sqlc.PasswordResetToken, error) {
	return m.GetByToken(ctx, token)
}

func (m *mockPasswordResetRepo) Delete(_ context.Context, token string) error {
	delete(m.tokens, token)
	return nil
}

func (m *mockPasswordResetRepo) DeleteByUserID(_ context.Context, userID int64) error {
	for k, v := range m.tokens {
		if v.UserID == userID {
			delete(m.tokens, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// mockCache
// ---------------------------------------------------------------------------

type mockCache struct {
	items map[string][]byte
}

func newMockCache() *mockCache {
	return &mockCache{items: make(map[string][]byte)}
}

func (m *mockCache) Get(_ context.Context, key string) ([]byte, error) {
	v, ok := m.items[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (m *mockCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	m.items[key] = value
	return nil
}

func (m *mockCache) Delete(_ context.Context, key string) error {
	delete(m.items, key)
	return nil
}

func (m *mockCache) Exists(_ context.Context, key string) (bool, error) {
	_, ok := m.items[key]
	return ok, nil
}

func (m *mockCache) Close() error                 { return nil }
func (m *mockCache) Ping(_ context.Context) error { return nil }

// ---------------------------------------------------------------------------
// mockEmailSender implements email.Sender
// ---------------------------------------------------------------------------

type mockEmailSender struct {
	sendErr error
	sent    int
}

func newMockEmailSender() *mockEmailSender {
	return &mockEmailSender{}
}

func (m *mockEmailSender) Send(_ context.Context, _ email.Message) error {
	if m.sendErr != nil {
		return m.sendErr
	}
	m.sent++
	return nil
}

// ---------------------------------------------------------------------------
// mockStorage
// ---------------------------------------------------------------------------

type mockStorage struct {
	files   map[string][]byte
	putErr  error
	getErr  error
	delErr  error
	baseURL string
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
