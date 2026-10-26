package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// memDB is an in-memory identity store for unit tests. It implements the
// ports the unit tests exercise with the same tenant-scoping rules as the
// repository (tenant from the context); the SQL-level guarantees (locks,
// triggers, unique indexes beyond email/membership) are covered by the
// integration tests.
type memDB struct {
	mu          sync.Mutex
	txMu        sync.Mutex
	nextID      int64
	users       map[int64]*User
	passwords   map[int64]string
	identities  map[string]int64 // provider+"/"+subject -> user
	memberships map[[2]int64]*Membership
	sessions    map[int64]*Session
	refresh     map[string]*RefreshToken // hash -> token
	tenants     map[int64]*Tenant
	factors     map[int64]*MFAFactor
	recovery    map[int64]map[string]bool // user -> code hash -> used
	invitations map[int64]*Invitation
	tokens      map[int64]string // invitation id -> token hash
	locale      string
	mail        Mailer
	resetTokens map[string]*PasswordResetToken
	resetOwner  map[string]int64
	audits      []audit.Event
	published   []events.Event
}

var errFakeNotImplemented = errors.New("fake: not implemented")

func newMemDB() *memDB {
	db := &memDB{
		users:       map[int64]*User{},
		passwords:   map[int64]string{},
		identities:  map[string]int64{},
		memberships: map[[2]int64]*Membership{},
		sessions:    map[int64]*Session{},
		refresh:     map[string]*RefreshToken{},
		tenants:     map[int64]*Tenant{},
		factors:     map[int64]*MFAFactor{},
		recovery:    map[int64]map[string]bool{},
		invitations: map[int64]*Invitation{},
		tokens:      map[int64]string{},
		locale:      "en",
		resetTokens: map[string]*PasswordResetToken{},
		resetOwner:  map[string]int64{},
	}
	db.tenants[1] = &Tenant{ID: 1, Slug: "default", Name: "Default", Status: TenantStatusActive}
	db.tenants[2] = &Tenant{ID: 2, Slug: "acme", Name: "Acme", Status: TenantStatusActive}
	return db
}

func (db *memDB) id() int64 { db.nextID++; return db.nextID }

func (db *memDB) stores() Stores {
	return Stores{
		Users:         memUsers{db},
		Memberships:   memMemberships{db},
		Sessions:      memSessions{db},
		RefreshTokens: memRefresh{db},
		Resets:        memResets{db},
		Verifications: memVerifications{},
		Invitations:   memInvitations{db},
		Tenants:       memTenants{db},
		MFA:           memMFA{db},
		Privacy:       nil,
		Roles:         memRoles{db},
	}
}

// memTx runs fn against the same in-memory stores (no rollback) and records
// audit events. db.mail, when set, is the transactional email outbox.
type memTx struct{ db *memDB }

func (t memTx) InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error {
	// Serialize transactions so a rotation's two writes (mark used + insert
	// the next token) are visible together, like one committed SQL transaction,
	// while individual repository calls keep their own short locks.
	// After-commit callbacks run once the "transaction" lock is released, so
	// a callback that opens another transaction cannot deadlock.
	t.db.txMu.Lock()
	var afterCommit []func()
	err := fn(ctx, TxRepos{
		Stores: t.db.stores(), Audit: memAudit(t), Events: memEvents(t), Mail: t.db.mail,
		AfterCommit: func(f func()) {
			if f != nil {
				afterCommit = append(afterCommit, f)
			}
		},
	})
	t.db.txMu.Unlock()
	if err != nil {
		return err
	}
	for _, f := range afterCommit {
		f()
	}
	return nil
}

type memEvents struct{ db *memDB }

func (e memEvents) Publish(_ context.Context, event events.Event) error {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	e.db.published = append(e.db.published, event)
	return nil
}

type memAudit struct{ db *memDB }

func (a memAudit) RecordTx(_ context.Context, event audit.Event) error {
	a.db.mu.Lock()
	defer a.db.mu.Unlock()
	a.db.audits = append(a.db.audits, event)
	return nil
}

func cloneUser(u *User, db *memDB) *User {
	c := *u
	_, c.HasPassword = db.passwords[u.ID]
	return &c
}

type memUsers struct{ db *memDB }

func (r memUsers) GetByID(_ context.Context, id int64) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	u, ok := r.db.users[id]
	if !ok || u.Status == UserStatusDeleted {
		return nil, apperror.ErrNotFound
	}
	return cloneUser(u, r.db), nil
}

func (r memUsers) GetByEmail(_ context.Context, email string) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	for _, u := range r.db.users {
		if u.Email == email && u.Status != UserStatusDeleted {
			return cloneUser(u, r.db), nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (r memUsers) GetByIdentity(_ context.Context, provider, subject string) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	id, ok := r.db.identities[provider+"/"+subject]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return cloneUser(r.db.users[id], r.db), nil
}

func (r memUsers) Create(_ context.Context, in NewUser) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	for _, u := range r.db.users {
		if u.Email == in.Email && u.Status != UserStatusDeleted {
			return nil, ErrEmailTaken
		}
	}
	now := time.Now()
	u := &User{ID: r.db.id(), Email: in.Email, Name: in.Name, Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if in.EmailVerified {
		u.EmailVerifiedAt = &now
	}
	r.db.users[u.ID] = u
	return cloneUser(u, r.db), nil
}

func (r memUsers) CreatePassword(_ context.Context, userID int64, hash string) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	r.db.passwords[userID] = hash
	return nil
}

func (r memUsers) PasswordHash(_ context.Context, userID int64) (string, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	hash, ok := r.db.passwords[userID]
	if !ok {
		return "", apperror.ErrNotFound
	}
	return hash, nil
}

func (r memUsers) ReplacePassword(_ context.Context, userID int64, hash string) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	u, ok := r.db.users[userID]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	r.db.passwords[userID] = hash
	now := time.Now()
	u.TokenVersion++
	u.CredentialsChangedAt = &now
	return cloneUser(u, r.db), nil
}

func (r memUsers) LinkIdentity(_ context.Context, in ExternalIdentity) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if _, ok := r.db.identities[in.Provider+"/"+in.Subject]; ok {
		return ErrIdentityTaken
	}
	for key, uid := range r.db.identities {
		if uid == in.UserID && len(key) > len(in.Provider) && key[:len(in.Provider)] == in.Provider {
			return ErrIdentityConflict
		}
	}
	r.db.identities[in.Provider+"/"+in.Subject] = in.UserID
	return nil
}

func (r memUsers) UpdateProfile(_ context.Context, in ProfileUpdate) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	u, ok := r.db.users[in.ID]
	if !ok || u.Status == UserStatusDeleted {
		return nil, apperror.ErrNotFound
	}
	if in.UpdateEmail && in.Email != u.Email {
		for _, other := range r.db.users {
			if other.ID != u.ID && other.Email == in.Email && other.Status != UserStatusDeleted {
				return nil, ErrEmailTaken
			}
		}
		now := time.Now()
		u.Email = in.Email
		u.EmailVerifiedAt = nil
		u.TokenVersion++
		u.CredentialsChangedAt = &now
	}
	u.Name = in.Name
	u.UpdatedAt = time.Now()
	return cloneUser(u, r.db), nil
}

func (r memUsers) VerifyEmail(_ context.Context, id int64, email string) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	u, ok := r.db.users[id]
	if !ok || u.Email != email {
		return nil, apperror.ErrNotFound
	}
	now := time.Now()
	u.EmailVerifiedAt = &now
	return cloneUser(u, r.db), nil
}

func (r memUsers) RevokeTokens(_ context.Context, id int64) (*User, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	u, ok := r.db.users[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	u.TokenVersion++
	return cloneUser(u, r.db), nil
}

func (r memUsers) MarkDeleted(context.Context, int64) (*User, error) {
	return nil, errFakeNotImplemented
}

type memMemberships struct{ db *memDB }

func (r memMemberships) member(tenantID, userID int64) (*Member, bool) {
	m, ok := r.db.memberships[[2]int64{tenantID, userID}]
	if !ok {
		return nil, false
	}
	return &Member{User: *cloneUser(r.db.users[userID], r.db), Membership: *m}, true
}

func (r memMemberships) Get(ctx context.Context, userID int64) (*Member, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	m, ok := r.member(tenant.EffectiveID(ctx), userID)
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return m, nil
}

func (r memMemberships) GetActive(ctx context.Context, userID int64) (*Member, error) {
	m, err := r.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !m.Active() {
		return nil, apperror.ErrNotFound
	}
	return m, nil
}

func (r memMemberships) list(ctx context.Context, activeOnly bool, cur pagination.DBCursor) []Member {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	var out []Member
	for key := range r.db.memberships {
		if key[0] != tenant.EffectiveID(ctx) {
			continue
		}
		m, _ := r.member(key[0], key[1])
		if !activeOnly || m.Active() {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User.ID > out[j].User.ID })
	if len(out) > int(cur.Limit) {
		out = out[:cur.Limit]
	}
	return out
}

func (r memMemberships) ListActiveCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error) {
	return r.list(ctx, true, cur), nil
}

func (r memMemberships) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error) {
	return r.list(ctx, false, cur), nil
}

func (r memMemberships) ListForUser(_ context.Context, userID int64) ([]Membership, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	var out []Membership
	for key, m := range r.db.memberships {
		if key[1] == userID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out, nil
}

func (r memMemberships) Create(ctx context.Context, userID int64, role string) (*Membership, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	key := [2]int64{tenant.EffectiveID(ctx), userID}
	if _, ok := r.db.memberships[key]; ok {
		return nil, ErrAlreadyMember
	}
	now := time.Now()
	m := &Membership{ID: r.db.id(), TenantID: key[0], UserID: userID, Role: role, Roles: []string{role}, Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now}
	r.db.memberships[key] = m
	c := *m
	return &c, nil
}

// update applies fn to the membership when it is in status from.
func (r memMemberships) update(ctx context.Context, userID int64, from func(MembershipStatus) bool, fn func(*Membership)) (*Membership, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	m, ok := r.db.memberships[[2]int64{tenant.EffectiveID(ctx), userID}]
	if !ok || !from(m.Status) {
		return nil, apperror.ErrNotFound
	}
	fn(m)
	c := *m
	return &c, nil
}

func is(s MembershipStatus) func(MembershipStatus) bool {
	return func(got MembershipStatus) bool { return got == s }
}

func (r memMemberships) Reactivate(ctx context.Context, userID int64, role string) (*Membership, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	m, ok := r.db.memberships[[2]int64{tenant.EffectiveID(ctx), userID}]
	if !ok || m.Status != MembershipStatusRemoved || m.BannedAt != nil {
		return nil, apperror.ErrNotFound
	}
	m.Status, m.RemovedAt, m.Role, m.Roles = MembershipStatusActive, nil, role, []string{role}
	c := *m
	return &c, nil
}

func (r memMemberships) UpdateRole(ctx context.Context, userID int64, role string) (*Membership, error) {
	return r.update(ctx, userID, is(MembershipStatusActive), func(m *Membership) {
		m.Role, m.Roles = role, []string{role}
	})
}

func (r memMemberships) Ban(ctx context.Context, userID int64) (*Membership, error) {
	return r.update(ctx, userID, is(MembershipStatusActive), func(m *Membership) {
		now := time.Now()
		m.Status, m.BannedAt, m.CredentialsRevokedAt = MembershipStatusSuspended, &now, &now
		if u := r.db.users[userID]; u != nil {
			m.BannedEmailHash = bannedEmailHash(u.Email)
		}
	})
}

func (r memMemberships) Unban(ctx context.Context, userID int64) (*Membership, error) {
	return r.update(ctx, userID, is(MembershipStatusSuspended), func(m *Membership) {
		m.Status, m.BannedAt, m.BannedEmailHash = MembershipStatusActive, nil, ""
	})
}

func (r memMemberships) Remove(ctx context.Context, userID int64) (*Membership, error) {
	return r.update(ctx, userID, func(s MembershipStatus) bool { return s != MembershipStatusRemoved }, func(m *Membership) {
		now := time.Now()
		banned := m.Status == MembershipStatusSuspended
		m.Status, m.RemovedAt, m.CredentialsRevokedAt = MembershipStatusRemoved, &now, &now
		// A ban, and its pseudonymous email marker, survives removal and
		// erasure; an active member's removal clears the (already empty) fields.
		if banned {
			if m.BannedEmailHash == "" {
				if u := r.db.users[userID]; u != nil {
					m.BannedEmailHash = bannedEmailHash(u.Email)
				}
			}
		} else {
			m.BannedAt, m.BannedEmailHash = nil, ""
		}
	})
}

func (r memMemberships) CountActiveOwners(ctx context.Context) (int64, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	var n int64
	for key, m := range r.db.memberships {
		if key[0] == tenant.EffectiveID(ctx) && m.Role == RoleOwner && m.Status == MembershipStatusActive {
			n++
		}
	}
	return n, nil
}

func (r memMemberships) LockOwnerChanges(context.Context) error { return nil }

func (r memMemberships) LockMembership(ctx context.Context, userID int64) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if _, ok := r.db.memberships[[2]int64{tenant.EffectiveID(ctx), userID}]; !ok {
		return apperror.ErrNotFound
	}
	return nil
}

func (r memMemberships) Stats(context.Context) (MemberStats, error) {
	return MemberStats{}, errFakeNotImplemented
}

func (r memMemberships) EmailBanned(ctx context.Context, email string) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	hash := bannedEmailHash(email)
	normalized := NormalizeEmail(email)
	for key, m := range r.db.memberships {
		if key[0] != tenant.EffectiveID(ctx) {
			continue
		}
		if m.BannedEmailHash != "" && m.BannedEmailHash == hash {
			return true, nil
		}
		if u := r.db.users[key[1]]; u != nil && m.Status == MembershipStatusSuspended && u.Email == normalized {
			return true, nil
		}
	}
	return false, nil
}

func (r memMemberships) GetPrincipal(ctx context.Context, userID int64) (*Principal, error) {
	m, err := r.GetActive(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &Principal{
		UserID: m.User.ID, TenantID: m.Membership.TenantID,
		Email: m.User.Email, Name: m.User.Name, Role: m.Membership.Role,
		TenantStatus: TenantStatusActive,
	}, nil
}

func (r memMemberships) LockPrincipal(ctx context.Context, userID int64) (*Principal, error) {
	m, err := r.GetActive(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &Principal{
		UserID: m.User.ID, TenantID: m.Membership.TenantID,
		Email: m.User.Email, Name: m.User.Name, Role: m.Membership.Role,
		TenantStatus: TenantStatusActive,
	}, nil
}

type memSessions struct{ db *memDB }

func (r memSessions) Create(ctx context.Context, in NewSession) (*Session, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	tid := tenant.EffectiveID(ctx)
	m, ok := r.db.memberships[[2]int64{tid, in.UserID}]
	u := r.db.users[in.UserID]
	if !ok || m.Status != MembershipStatusActive || u.Status != UserStatusActive || u.TokenVersion != in.TokenVersion {
		return nil, apperror.ErrNotFound
	}
	now := time.Now()
	s := &Session{ID: r.db.id(), TenantID: tid, UserID: in.UserID, TokenVersion: in.TokenVersion,
		UserAgent: in.UserAgent, IP: in.IP, AMR: in.AMR, ExpiresAt: in.ExpiresAt, CreatedAt: now, LastUsedAt: now}
	r.db.sessions[s.ID] = s
	c := *s
	return &c, nil
}

func (r memSessions) Get(ctx context.Context, id int64) (*Session, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	s, ok := r.db.sessions[id]
	if !ok || s.TenantID != tenant.EffectiveID(ctx) {
		return nil, apperror.ErrNotFound
	}
	c := *s
	return &c, nil
}

func (r memSessions) ListLiveCursor(context.Context, int64, pagination.DBCursor) ([]Session, error) {
	return nil, errFakeNotImplemented
}

func (r memSessions) revoke(match func(*Session) bool, why RevokeReason) (userID int64, n int) {
	now := time.Now()
	for _, s := range r.db.sessions {
		if s.RevokedAt == nil && match(s) {
			s.RevokedAt, s.RevokedReason = &now, why
			userID = s.UserID
			n++
		}
	}
	return userID, n
}

func (r memSessions) Revoke(ctx context.Context, userID, id int64, why RevokeReason) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	tid := tenant.EffectiveID(ctx)
	_, n := r.revoke(func(s *Session) bool { return s.ID == id && s.UserID == userID && s.TenantID == tid }, why)
	return n > 0, nil
}

func (r memSessions) RevokeAllInTenant(ctx context.Context, userID int64, why RevokeReason) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	tid := tenant.EffectiveID(ctx)
	r.revoke(func(s *Session) bool { return s.UserID == userID && s.TenantID == tid }, why)
	return nil
}

func (r memSessions) RevokeAllForUser(_ context.Context, userID int64, why RevokeReason) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	r.revoke(func(s *Session) bool { return s.UserID == userID }, why)
	return nil
}

func (r memSessions) RevokeOtherUserSessions(_ context.Context, userID, keepID int64, why RevokeReason) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	r.revoke(func(s *Session) bool { return s.UserID == userID && s.ID != keepID }, why)
	return nil
}

func (r memSessions) RevokeByRefreshToken(_ context.Context, hash string, why RevokeReason) (userID int64, revoked bool, err error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	rt, ok := r.db.refresh[hash]
	if !ok {
		return 0, false, nil
	}
	userID, n := r.revoke(func(s *Session) bool { return s.ID == rt.SessionID }, why)
	return userID, n > 0, nil
}

func (r memSessions) Touch(context.Context, int64, string, string) error { return nil }

func (r memSessions) State(context.Context, int64, int64) (*SessionState, error) {
	return nil, errFakeNotImplemented
}

type memRefresh struct{ db *memDB }

func (r memRefresh) Create(ctx context.Context, sessionID int64, hash string, expiresAt time.Time) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	r.db.refresh[hash] = &RefreshToken{ID: r.db.id(), TenantID: tenant.EffectiveID(ctx), SessionID: sessionID, ExpiresAt: expiresAt, CreatedAt: time.Now()}
	return nil
}

func (r memRefresh) GetByHash(_ context.Context, hash string) (*RefreshToken, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	rt, ok := r.db.refresh[hash]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	c := *rt
	return &c, nil
}

func (r memRefresh) MarkUsed(_ context.Context, tenantID, id int64) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	for _, rt := range r.db.refresh {
		if rt.ID == id && rt.TenantID == tenantID && rt.UsedAt == nil {
			now := time.Now()
			rt.UsedAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (r memRefresh) ClassifyRotation(_ context.Context, hash string, usedAfter time.Time) (RefreshRotationState, error) {
	// The real query reads committed rows only. Waiting for any in-flight
	// fake transaction models that: a concurrent rotation's "mark used" and
	// "insert next token" are seen together or not at all. The service calls
	// this outside any transaction, so taking txMu cannot deadlock.
	r.db.txMu.Lock()
	defer r.db.txMu.Unlock()
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	rt, ok := r.db.refresh[hash]
	if !ok {
		return RefreshRotationState{}, apperror.ErrNotFound
	}
	sess, ok := r.db.sessions[rt.SessionID]
	if !ok {
		return RefreshRotationState{}, apperror.ErrNotFound
	}
	state := RefreshRotationState{
		SessionLive:     sess.RevokedAt == nil && sess.ExpiresAt.After(time.Now()),
		UsedWithinGrace: rt.UsedAt != nil && rt.UsedAt.After(usedAfter),
	}
	for _, other := range r.db.refresh {
		if other.SessionID == rt.SessionID && other.ID > rt.ID {
			state.Rotated = true
			break
		}
	}
	return state, nil
}

type memResets struct{ db *memDB }

func (r memResets) Create(_ context.Context, userID int64, email, tokenHash string, expiresAt time.Time) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	u, ok := r.db.users[userID]
	if !ok || u.Status == UserStatusDeleted || u.Email != email {
		return false, nil
	}
	for hash, owner := range r.db.resetOwner {
		if owner == userID {
			delete(r.db.resetOwner, hash)
			delete(r.db.resetTokens, hash)
		}
	}
	r.db.resetOwner[tokenHash] = userID
	r.db.resetTokens[tokenHash] = &PasswordResetToken{ID: r.db.id(), UserID: userID, ExpiresAt: expiresAt, CreatedAt: time.Now()}
	return true, nil
}

func (r memResets) GetByHash(_ context.Context, tokenHash string) (*PasswordResetToken, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	token, ok := r.db.resetTokens[tokenHash]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	c := *token
	return &c, nil
}

func (r memResets) Consume(_ context.Context, tokenHash string) (*PasswordResetToken, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	token, ok := r.db.resetTokens[tokenHash]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	delete(r.db.resetTokens, tokenHash)
	delete(r.db.resetOwner, tokenHash)
	c := *token
	return &c, nil
}

func (r memResets) Delete(_ context.Context, tokenHash string) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	delete(r.db.resetTokens, tokenHash)
	delete(r.db.resetOwner, tokenHash)
	return nil
}

func (r memResets) DeleteByUser(_ context.Context, userID int64) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	for hash, owner := range r.db.resetOwner {
		if owner == userID {
			delete(r.db.resetOwner, hash)
			delete(r.db.resetTokens, hash)
		}
	}
	return nil
}

type memVerifications struct{}

func (memVerifications) Create(context.Context, int64, string, string, time.Time) error { return nil }
func (memVerifications) Consume(context.Context, string) (*VerificationToken, error) {
	return nil, errFakeNotImplemented
}
func (memVerifications) DeleteByUser(context.Context, int64) error { return nil }

type memInvitations struct{ db *memDB }

func (r memInvitations) Upsert(ctx context.Context, in NewInvitation) (*Invitation, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	tid := tenant.EffectiveID(ctx)
	now := time.Now()
	for id, inv := range r.db.invitations {
		if inv.TenantID == tid && inv.Email == in.Email && inv.AcceptedAt == nil {
			inv.Role, inv.InvitedBy, inv.ExpiresAt, inv.UpdatedAt = in.Role, in.InvitedBy, in.ExpiresAt, now
			r.db.tokens[id] = in.TokenHash
			c := *inv
			return &c, nil
		}
	}
	inv := &Invitation{
		ID: r.db.id(), TenantID: tid, Email: in.Email, Role: in.Role,
		InvitedBy: in.InvitedBy, ExpiresAt: in.ExpiresAt, CreatedAt: now, UpdatedAt: now,
	}
	r.db.invitations[inv.ID] = inv
	r.db.tokens[inv.ID] = in.TokenHash
	c := *inv
	return &c, nil
}

func (r memInvitations) ListPendingCursor(context.Context, pagination.DBCursor) ([]Invitation, error) {
	return nil, errFakeNotImplemented
}

func (r memInvitations) DeletePending(ctx context.Context, id int64) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	inv, ok := r.db.invitations[id]
	if !ok || inv.TenantID != tenant.EffectiveID(ctx) || inv.AcceptedAt != nil {
		return false, nil
	}
	delete(r.db.invitations, id)
	delete(r.db.tokens, id)
	return true, nil
}

func (r memInvitations) TenantByTokenHash(_ context.Context, tokenHash string) (int64, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	for id, hash := range r.db.tokens {
		if hash == tokenHash {
			return r.db.invitations[id].TenantID, nil
		}
	}
	return 0, apperror.ErrNotFound
}

func (r memInvitations) GetByTokenHashForUpdate(ctx context.Context, tenantID int64, tokenHash string) (*Invitation, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	for id, hash := range r.db.tokens {
		if hash != tokenHash {
			continue
		}
		inv := r.db.invitations[id]
		if inv.TenantID != tenantID || inv.TenantID != tenant.EffectiveID(ctx) {
			return nil, apperror.ErrNotFound
		}
		c := *inv
		return &c, nil
	}
	return nil, apperror.ErrNotFound
}

func (r memInvitations) MarkAccepted(ctx context.Context, tenantID, id, userID int64) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	inv, ok := r.db.invitations[id]
	if !ok || inv.TenantID != tenantID || inv.AcceptedAt != nil {
		return false, nil
	}
	now := time.Now()
	inv.AcceptedAt, inv.AcceptedBy, inv.UpdatedAt = &now, &userID, now
	return true, nil
}

type memTenants struct{ db *memDB }

func (r memTenants) get(id int64, operational bool) (*Tenant, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	t, ok := r.db.tenants[id]
	if !ok || (operational && !t.Status.Operational()) {
		return nil, apperror.ErrNotFound
	}
	c := *t
	return &c, nil
}

func (r memTenants) GetOperationalBySlug(context.Context, string) (*Tenant, error) {
	return nil, errFakeNotImplemented
}
func (r memTenants) GetOperationalByID(_ context.Context, id int64) (*Tenant, error) {
	return r.get(id, true)
}
func (r memTenants) GetByID(_ context.Context, id int64) (*Tenant, error) { return r.get(id, false) }
func (r memTenants) GetBySlug(context.Context, string) (*Tenant, error) {
	return nil, errFakeNotImplemented
}
func (r memTenants) GetByIDForUpdate(_ context.Context, id int64) (*Tenant, error) {
	return r.get(id, false)
}
func (r memTenants) ListCursor(_ context.Context, _ pagination.DBCursor) ([]Tenant, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	tenants := make([]Tenant, 0, len(r.db.tenants))
	for _, t := range r.db.tenants {
		tenants = append(tenants, *t)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID > tenants[j].ID })
	return tenants, nil
}
func (r memTenants) Create(context.Context, NewTenant) (*Tenant, error) {
	return nil, errFakeNotImplemented
}
func (r memTenants) UpdateStatus(_ context.Context, id int64, status TenantStatus, deletion *time.Time) (*Tenant, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	t, ok := r.db.tenants[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	t.Status, t.DeletionScheduledAt, t.StatusChangedAt = status, deletion, time.Now()
	c := *t
	return &c, nil
}
func (r memTenants) Settings(_ context.Context, id int64) (*TenantSettings, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if _, ok := r.db.tenants[id]; !ok {
		return nil, apperror.ErrNotFound
	}
	locale := r.db.locale
	if locale == "" {
		locale = "en"
	}
	return &TenantSettings{TenantID: id, Locale: locale, Timezone: "UTC"}, nil
}

type memMFA struct{ db *memDB }

func (r memMFA) GetTOTP(_ context.Context, userID int64) (*MFAFactor, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	f, ok := r.db.factors[userID]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	c := *f
	return &c, nil
}

func (r memMFA) GetTOTPForUpdate(ctx context.Context, userID int64) (*MFAFactor, error) {
	return r.GetTOTP(ctx, userID)
}

func (r memMFA) GetConfirmedTOTP(_ context.Context, userID int64) (*MFAFactor, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	f, ok := r.db.factors[userID]
	if !ok || f.ConfirmedAt == nil {
		return nil, apperror.ErrNotFound
	}
	c := *f
	return &c, nil
}

func (r memMFA) UpsertUnconfirmedTOTP(_ context.Context, userID int64, sealed string) (*MFAFactor, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	now := time.Now()
	f, ok := r.db.factors[userID]
	if !ok {
		f = &MFAFactor{ID: r.db.id(), UserID: userID, Type: MFAFactorTOTP, CreatedAt: now}
		r.db.factors[userID] = f
	}
	if f.ConfirmedAt != nil {
		return nil, errors.New("fake: factor already confirmed")
	}
	f.SecretCiphertext = sealed
	f.UpdatedAt = now
	c := *f
	return &c, nil
}

func (r memMFA) ConfirmTOTP(_ context.Context, id, userID, step int64) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	f, ok := r.db.factors[userID]
	if !ok || f.ID != id || f.ConfirmedAt != nil {
		return false, nil
	}
	now := time.Now()
	f.ConfirmedAt = &now
	f.LastUsedStep = step
	return true, nil
}

func (r memMFA) ConsumeTOTPStep(_ context.Context, userID, step int64) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	f, ok := r.db.factors[userID]
	if !ok || f.ConfirmedAt == nil || f.LastUsedStep >= step {
		return false, nil
	}
	f.LastUsedStep = step
	return true, nil
}

func (r memMFA) DeleteTOTP(_ context.Context, userID int64) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	delete(r.db.factors, userID)
	return nil
}

func (r memMFA) DeletePendingTOTP(_ context.Context, userID int64) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if f, ok := r.db.factors[userID]; ok && f.ConfirmedAt == nil {
		delete(r.db.factors, userID)
	}
	return nil
}

func (r memMFA) DeleteRecoveryCodes(_ context.Context, userID int64) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	delete(r.db.recovery, userID)
	return nil
}

func (r memMFA) CreateRecoveryCodes(_ context.Context, userID int64, hashes []string) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	codes := r.db.recovery[userID]
	if codes == nil {
		codes = map[string]bool{}
		r.db.recovery[userID] = codes
	}
	for _, hash := range hashes {
		codes[hash] = false
	}
	return nil
}

func (r memMFA) UseRecoveryCode(_ context.Context, userID int64, hash string) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	codes := r.db.recovery[userID]
	used, ok := codes[hash]
	if !ok || used {
		return false, nil
	}
	codes[hash] = true
	return true, nil
}

func (r memMFA) CountUnusedRecoveryCodes(_ context.Context, userID int64) (int64, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	var n int64
	for _, used := range r.db.recovery[userID] {
		if !used {
			n++
		}
	}
	return n, nil
}

// memRoles resolves the built-in tenant roles (enough for inviter authority
// checks in unit tests); tenant-defined roles are covered by integration tests.
type memRoles struct{ db *memDB }

func (r memRoles) GetByKey(_ context.Context, key string) (*Role, error) {
	if !ValidRole(key) {
		return nil, apperror.ErrNotFound
	}
	return &Role{Key: key, Name: key, Scope: "tenant"}, nil
}

func (r memRoles) SyncCatalog(context.Context, []CatalogPermission, []CatalogRole) error {
	return errFakeNotImplemented
}
func (r memRoles) ListCursor(context.Context, pagination.DBCursor) ([]Role, error) {
	return nil, errFakeNotImplemented
}
func (r memRoles) Get(context.Context, int64) (*Role, error) {
	return nil, errFakeNotImplemented
}
func (r memRoles) Create(context.Context, NewRole) (*Role, error) {
	return nil, errFakeNotImplemented
}
func (r memRoles) Update(context.Context, int64, string, []string) (*Role, error) {
	return nil, errFakeNotImplemented
}
func (r memRoles) Delete(context.Context, int64) (bool, error) {
	return false, errFakeNotImplemented
}
func (r memRoles) MemberIDs(context.Context, int64) ([]int64, error) {
	return nil, errFakeNotImplemented
}
func (r memRoles) ReplaceMemberRoles(context.Context, int64, []string) error {
	return errFakeNotImplemented
}
func (r memRoles) AssignPlatformRole(context.Context, int64, string) error {
	return errFakeNotImplemented
}
