package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// mockTenant mirrors repository tenant scoping so service tests can prove that
// two tenants never see each other's rows.
func mockTenant(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

func inMockTenant(ctx context.Context, tenantID int64) bool {
	return tenantID == mockTenant(ctx)
}

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
	users  map[int64]*identitysqlc.User
	nextID int64
}

func newMockUserRepo() *mockUserRepo {
	return &mockUserRepo{users: make(map[int64]*identitysqlc.User), nextID: 1}
}

func (m *mockUserRepo) GetByID(ctx context.Context, id int64) (*identitysqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	return u, nil
}

func (m *mockUserRepo) GetSessionState(ctx context.Context, id int64) (role string, tokenVersion int32, tenantID int64, tenantStatus string, err error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid {
		return "", 0, 0, "", apperror.ErrNotFound
	}
	return u.Role, u.TokenVersion, u.TenantID, "active", nil
}

func (m *mockUserRepo) GetByEmail(ctx context.Context, addr string) (*identitysqlc.User, error) {
	for _, u := range m.users {
		if !u.DeletedAt.Valid && inMockTenant(ctx, u.TenantID) && strings.EqualFold(u.Email, addr) {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) GetByGoogleID(ctx context.Context, googleID string) (*identitysqlc.User, error) {
	for _, u := range m.users {
		if !u.DeletedAt.Valid && inMockTenant(ctx, u.TenantID) && u.GoogleID.Valid && u.GoogleID.String == googleID {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) GetBannedByEmail(ctx context.Context, addr string) (*identitysqlc.User, error) {
	for _, u := range m.users {
		if u.BannedAt.Valid && inMockTenant(ctx, u.TenantID) && strings.EqualFold(u.Email, addr) {
			return u, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (m *mockUserRepo) List(ctx context.Context, limit, offset int32) ([]identitysqlc.User, error) {
	all := make([]identitysqlc.User, 0, len(m.users))
	for _, u := range m.users {
		if u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
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

func (m *mockUserRepo) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]identitysqlc.User, error) {
	return m.listCursor(ctx, cur, false), nil
}

func (m *mockUserRepo) AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]identitysqlc.User, error) {
	return m.listCursor(ctx, cur, true), nil
}

func (m *mockUserRepo) listCursor(ctx context.Context, cur pagination.DBCursor, includeDeleted bool) []identitysqlc.User {
	all := make([]identitysqlc.User, 0, len(m.users))
	for _, u := range m.users {
		if !inMockTenant(ctx, u.TenantID) {
			continue
		}
		if u.DeletedAt.Valid && !includeDeleted {
			continue
		}
		all = append(all, *u)
	}
	// (created_at DESC, id DESC)
	sort.Slice(all, func(i, j int) bool {
		return cursorLess(all[j].CreatedAt.Time, all[j].ID, all[i].CreatedAt.Time, all[i].ID)
	})
	out := make([]identitysqlc.User, 0, len(all))
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

func (m *mockUserRepo) Count(ctx context.Context) (int64, error) {
	var count int64
	for _, u := range m.users {
		if !u.DeletedAt.Valid && inMockTenant(ctx, u.TenantID) {
			count++
		}
	}
	return count, nil
}

func (m *mockUserRepo) Create(ctx context.Context, params identitysqlc.CreateUserParams) (*identitysqlc.User, error) {
	tid := mockTenant(ctx)
	// Enforce active, case-insensitive email uniqueness inside the tenant.
	for _, u := range m.users {
		if !u.DeletedAt.Valid && inMockTenant(ctx, u.TenantID) && strings.EqualFold(u.Email, params.Email) {
			return nil, fmt.Errorf("duplicate key value violates unique constraint \"idx_users_tenant_email_lower_active\"")
		}
	}
	u := &identitysqlc.User{
		TenantID:     tid,
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

func (m *mockUserRepo) CreateOAuthUser(ctx context.Context, params identitysqlc.CreateOAuthUserParams) (*identitysqlc.User, error) {
	tid := mockTenant(ctx)
	// Enforce active, case-insensitive email uniqueness inside the tenant.
	for _, u := range m.users {
		if !u.DeletedAt.Valid && inMockTenant(ctx, u.TenantID) && strings.EqualFold(u.Email, params.Email) {
			return nil, fmt.Errorf("duplicate key value violates unique constraint \"idx_users_tenant_email_lower_active\"")
		}
	}
	u := &identitysqlc.User{
		TenantID:     tid,
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

func (m *mockUserRepo) Update(ctx context.Context, params identitysqlc.UpdateUserParams) (*identitysqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok || u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
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

func (m *mockUserRepo) UpdatePassword(ctx context.Context, params identitysqlc.UpdateUserPasswordParams) (*identitysqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok || u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	u.PasswordHash = params.PasswordHash
	u.TokenVersion++
	u.PasswordChangedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) RevokeSessions(ctx context.Context, id int64) (*identitysqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	u.TokenVersion++
	return u, nil
}

func (m *mockUserRepo) UpdateRole(ctx context.Context, params identitysqlc.UpdateUserRoleParams) (*identitysqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok || u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	u.Role = params.Role
	u.TokenVersion++
	return u, nil
}

func (m *mockUserRepo) VerifyEmail(ctx context.Context, id int64, addr string) (*identitysqlc.User, error) {
	u, ok := m.users[id]
	if !ok || !inMockTenant(ctx, u.TenantID) || !strings.EqualFold(u.Email, addr) {
		return nil, apperror.ErrNotFound
	}
	u.EmailVerifiedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) LinkGoogleAccount(ctx context.Context, params identitysqlc.LinkGoogleAccountParams) (*identitysqlc.User, error) {
	u, ok := m.users[params.ID]
	if !ok || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	u.GoogleID = params.GoogleID
	u.AuthProvider = "google"
	return u, nil
}

func (m *mockUserRepo) Delete(ctx context.Context, id int64) (*identitysqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	u.DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	u.TokenVersion++
	u.PasswordChangedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) Ban(ctx context.Context, id int64) (*identitysqlc.User, error) {
	u, ok := m.users[id]
	if !ok || u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	u.DeletedAt = now
	u.BannedAt = now
	u.TokenVersion++
	u.PasswordChangedAt = now
	return u, nil
}

func (m *mockUserRepo) Restore(ctx context.Context, id int64) (*identitysqlc.User, error) {
	u, ok := m.users[id]
	if !ok || !u.DeletedAt.Valid || !inMockTenant(ctx, u.TenantID) {
		return nil, apperror.ErrNotFound
	}
	u.DeletedAt = pgtype.Timestamptz{}
	u.BannedAt = pgtype.Timestamptz{}
	u.TokenVersion++
	u.PasswordChangedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return u, nil
}

func (m *mockUserRepo) AdminList(ctx context.Context, limit, offset int32) ([]identitysqlc.User, error) {
	return m.List(ctx, limit, offset)
}

func (m *mockUserRepo) AdminCount(ctx context.Context) (int64, error) {
	var count int64
	for _, u := range m.users {
		if inMockTenant(ctx, u.TenantID) {
			count++
		}
	}
	return count, nil
}

func (m *mockUserRepo) CountActiveAdmins(ctx context.Context) (int64, error) {
	var count int64
	for _, u := range m.users {
		if !u.DeletedAt.Valid && u.Role == "admin" && inMockTenant(ctx, u.TenantID) {
			count++
		}
	}
	return count, nil
}

func (m *mockUserRepo) AdminUserStats(ctx context.Context) (identitysqlc.AdminUserStatsRow, error) {
	var active, deleted int64
	for _, u := range m.users {
		if !inMockTenant(ctx, u.TenantID) {
			continue
		}
		if u.DeletedAt.Valid {
			deleted++
		} else {
			active++
		}
	}
	return identitysqlc.AdminUserStatsRow{ActiveUsers: active, DeletedUsers: deleted}, nil
}

// ---------------------------------------------------------------------------
// mockRefreshTokenRepo
// ---------------------------------------------------------------------------

type mockRefreshTokenRepo struct {
	tokens         map[string]*identitysqlc.RefreshToken
	deletedUserIDs []int64
}

func newMockRefreshTokenRepo() *mockRefreshTokenRepo {
	return &mockRefreshTokenRepo{tokens: make(map[string]*identitysqlc.RefreshToken)}
}

func (m *mockRefreshTokenRepo) Create(ctx context.Context, params identitysqlc.CreateRefreshTokenParams) (*identitysqlc.RefreshToken, error) {
	rt := &identitysqlc.RefreshToken{
		TenantID:     mockTenant(ctx),
		UserID:       params.UserID,
		Token:        params.Token,
		ExpiresAt:    params.ExpiresAt,
		TokenVersion: params.TokenVersion,
	}
	m.tokens[params.Token] = rt
	return rt, nil
}

func (m *mockRefreshTokenRepo) GetByToken(ctx context.Context, token string) (*identitysqlc.RefreshToken, error) {
	rt, ok := m.tokens[token]
	if !ok || !inMockTenant(ctx, rt.TenantID) {
		return nil, apperror.ErrNotFound
	}
	return rt, nil
}

func (m *mockRefreshTokenRepo) Consume(ctx context.Context, token string) (*identitysqlc.RefreshToken, error) {
	rt, ok := m.tokens[token]
	if !ok || !inMockTenant(ctx, rt.TenantID) {
		return nil, apperror.ErrNotFound
	}
	delete(m.tokens, token)
	return rt, nil
}

func (m *mockRefreshTokenRepo) Delete(ctx context.Context, token string) error {
	if rt, ok := m.tokens[token]; ok && inMockTenant(ctx, rt.TenantID) {
		delete(m.tokens, token)
	}
	return nil
}

func (m *mockRefreshTokenRepo) DeleteByUserID(ctx context.Context, userID int64) error {
	m.deletedUserIDs = append(m.deletedUserIDs, userID)
	for k, v := range m.tokens {
		if v.UserID == userID && inMockTenant(ctx, v.TenantID) {
			delete(m.tokens, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// mockEmailVerificationRepo
// ---------------------------------------------------------------------------

type mockEmailVerificationRepo struct {
	tokens map[string]*identitysqlc.EmailVerificationToken
	nextID int64
}

func newMockEmailVerificationRepo() *mockEmailVerificationRepo {
	return &mockEmailVerificationRepo{tokens: make(map[string]*identitysqlc.EmailVerificationToken), nextID: 1}
}

func (m *mockEmailVerificationRepo) Create(ctx context.Context, params identitysqlc.CreateEmailVerificationTokenParams) (*identitysqlc.EmailVerificationToken, error) {
	t := &identitysqlc.EmailVerificationToken{
		TenantID:  mockTenant(ctx),
		ID:        m.nextID,
		UserID:    params.UserID,
		Email:     params.Email,
		Token:     params.Token,
		ExpiresAt: params.ExpiresAt,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	m.tokens[params.Token] = t
	m.nextID++
	return t, nil
}

func (m *mockEmailVerificationRepo) GetByToken(ctx context.Context, token string) (*identitysqlc.EmailVerificationToken, error) {
	t, ok := m.tokens[token]
	if !ok || !inMockTenant(ctx, t.TenantID) {
		return nil, apperror.ErrNotFound
	}
	return t, nil
}

func (m *mockEmailVerificationRepo) Consume(ctx context.Context, token string) (*identitysqlc.EmailVerificationToken, error) {
	t, ok := m.tokens[token]
	if !ok || !inMockTenant(ctx, t.TenantID) {
		return nil, apperror.ErrNotFound
	}
	delete(m.tokens, token)
	return t, nil
}

func (m *mockEmailVerificationRepo) Delete(ctx context.Context, token string) error {
	if t, ok := m.tokens[token]; ok && inMockTenant(ctx, t.TenantID) {
		delete(m.tokens, token)
	}
	return nil
}

func (m *mockEmailVerificationRepo) DeleteByUserID(ctx context.Context, userID int64) error {
	for k, v := range m.tokens {
		if v.UserID == userID && inMockTenant(ctx, v.TenantID) {
			delete(m.tokens, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// mockPasswordResetRepo
// ---------------------------------------------------------------------------

type mockPasswordResetRepo struct {
	tokens map[string]*identitysqlc.PasswordResetToken
	nextID int64
}

func newMockPasswordResetRepo() *mockPasswordResetRepo {
	return &mockPasswordResetRepo{tokens: make(map[string]*identitysqlc.PasswordResetToken), nextID: 1}
}

func (m *mockPasswordResetRepo) Create(ctx context.Context, params identitysqlc.CreatePasswordResetTokenParams) (*identitysqlc.PasswordResetToken, error) {
	t := &identitysqlc.PasswordResetToken{
		TenantID:  mockTenant(ctx),
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

func (m *mockPasswordResetRepo) GetByToken(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error) {
	t, ok := m.tokens[token]
	if !ok || !inMockTenant(ctx, t.TenantID) {
		return nil, apperror.ErrNotFound
	}
	return t, nil
}

func (m *mockPasswordResetRepo) GetByTokenForUpdate(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error) {
	return m.GetByToken(ctx, token)
}

func (m *mockPasswordResetRepo) Consume(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error) {
	t, ok := m.tokens[token]
	if !ok || !inMockTenant(ctx, t.TenantID) {
		return nil, apperror.ErrNotFound
	}
	delete(m.tokens, token)
	return t, nil
}

func (m *mockPasswordResetRepo) Delete(ctx context.Context, token string) error {
	if t, ok := m.tokens[token]; ok && inMockTenant(ctx, t.TenantID) {
		delete(m.tokens, token)
	}
	return nil
}

func (m *mockPasswordResetRepo) DeleteByUserID(ctx context.Context, userID int64) error {
	for k, v := range m.tokens {
		if v.UserID == userID && inMockTenant(ctx, v.TenantID) {
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
	ttls  map[string]time.Duration
}

func newMockCache() *mockCache {
	return &mockCache{items: make(map[string][]byte), ttls: make(map[string]time.Duration)}
}

func (m *mockCache) Get(_ context.Context, key string) ([]byte, error) {
	v, ok := m.items[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (m *mockCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.items[key] = value
	m.ttls[key] = ttl
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

func (m *mockCache) Increment(_ context.Context, key string, ttl time.Duration) (int64, error) {
	var n int64
	if v, ok := m.items[key]; ok {
		n, _ = strconv.ParseInt(string(v), 10, 64)
	}
	n++
	m.items[key] = []byte(strconv.FormatInt(n, 10))
	m.ttls[key] = ttl
	return n, nil
}

func (m *mockCache) Close() error                 { return nil }
func (m *mockCache) Ping(_ context.Context) error { return nil }

// ---------------------------------------------------------------------------
// mockEmailSender implements email.Sender
// ---------------------------------------------------------------------------

type mockEmailSender struct {
	mu       sync.Mutex
	sendErr  error
	sent     int
	messages []email.Message
}

func newMockEmailSender() *mockEmailSender {
	return &mockEmailSender{}
}

func (m *mockEmailSender) Send(_ context.Context, msg email.Message) error {
	if m.sendErr != nil {
		return m.sendErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent++
	m.messages = append(m.messages, msg)
	return nil
}

func (m *mockEmailSender) Sent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent
}

// Messages returns a copy of the delivered messages under the sender lock so
// tests can inspect bodies without racing an asynchronous send.
func (m *mockEmailSender) Messages() []email.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]email.Message, len(m.messages))
	copy(out, m.messages)
	return out
}

// WaitFor waits for asynchronously sent mail to arrive without sleeping in the
// happy path. Used by tests that exercise fire-and-forget email delivery.
func (m *mockEmailSender) WaitFor(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.Sent() >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return m.Sent() >= n
}

// mockTransactionalEmailSender is both an email.Sender and a
// transactionalEmailSender. Tests use it to prove that services only call
// SendTx when they actually have a transaction; otherwise they must fall back
// to exactly one Send.
type mockTransactionalEmailSender struct {
	*mockEmailSender
	mu          sync.Mutex
	sendTxCalls int
}

func newMockTransactionalEmailSender() *mockTransactionalEmailSender {
	return &mockTransactionalEmailSender{mockEmailSender: newMockEmailSender()}
}

func (m *mockTransactionalEmailSender) SendTx(ctx context.Context, _ pgx.Tx, msg email.Message) error {
	m.mu.Lock()
	m.sendTxCalls++
	m.mu.Unlock()
	return m.Send(ctx, msg)
}

func (m *mockTransactionalEmailSender) SendTxCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sendTxCalls
}
