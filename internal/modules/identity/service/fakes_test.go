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
		Resets:        memResets{},
		Verifications: memVerifications{},
		Invitations:   memInvitations{},
		Tenants:       memTenants{db},
		MFA:           memMFA{db},
	}
}

// memTx runs fn against the same in-memory stores (no rollback) and records
// audit events.
type memTx struct{ db *memDB }

func (t memTx) InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error {
	return fn(ctx, TxRepos{Stores: t.db.stores(), Audit: memAudit(t), Events: memEvents(t)})
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

func (r memUsers) UpdateProfile(context.Context, ProfileUpdate) (*User, error) {
	return nil, errFakeNotImplemented
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

func (r memMemberships) GetPrincipal(context.Context, int64) (*Principal, error) {
	return nil, errFakeNotImplemented
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
		UserAgent: in.UserAgent, IP: in.IP, AMR: in.AMR, CreatedAt: now, LastUsedAt: now}
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

type memResets struct{}

func (memResets) Create(context.Context, int64, string, string, time.Time) (bool, error) {
	return false, errFakeNotImplemented
}
func (memResets) GetByHash(context.Context, string) (*PasswordResetToken, error) {
	return nil, errFakeNotImplemented
}
func (memResets) Consume(context.Context, string) (*PasswordResetToken, error) {
	return nil, errFakeNotImplemented
}
func (memResets) Delete(context.Context, string) error      { return nil }
func (memResets) DeleteByUser(context.Context, int64) error { return nil }

type memVerifications struct{}

func (memVerifications) Create(context.Context, int64, string, string, time.Time) error { return nil }
func (memVerifications) Consume(context.Context, string) (*VerificationToken, error) {
	return nil, errFakeNotImplemented
}
func (memVerifications) DeleteByUser(context.Context, int64) error { return nil }

type memInvitations struct{}

func (memInvitations) Upsert(context.Context, NewInvitation) (*Invitation, error) {
	return nil, errFakeNotImplemented
}
func (memInvitations) ListPendingCursor(context.Context, pagination.DBCursor) ([]Invitation, error) {
	return nil, errFakeNotImplemented
}
func (memInvitations) DeletePending(context.Context, int64) (bool, error) {
	return false, errFakeNotImplemented
}
func (memInvitations) GetByTokenHashForUpdate(context.Context, string) (*Invitation, error) {
	return nil, errFakeNotImplemented
}
func (memInvitations) MarkAccepted(context.Context, int64, int64, int64) (bool, error) {
	return false, errFakeNotImplemented
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
func (r memTenants) Settings(context.Context, int64) (*TenantSettings, error) {
	return nil, errFakeNotImplemented
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
