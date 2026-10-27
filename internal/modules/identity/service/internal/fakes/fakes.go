package fakes

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/google/uuid"
)

// MemDB is an in-memory identity store for unit tests. It implements the
// ports the unit tests exercise with the same tenant-scoping rules as the
// repository (tenant from the context); the SQL-level guarantees (locks,
// triggers, unique indexes beyond email/membership) are covered by the
// integration tests.
type MemDB struct {
	Mu          sync.Mutex
	TxMu        sync.Mutex
	NextID      uint64
	Users       map[uuid.UUID]*User
	Passwords   map[uuid.UUID]string
	Identities  map[string]uuid.UUID // provider+"/"+subject -> user
	Memberships map[[2]uuid.UUID]*Membership
	Sessions    map[uuid.UUID]*Session
	Refresh     map[string]*RefreshToken // hash -> token
	Tenants     map[uuid.UUID]*Tenant
	Factors     map[uuid.UUID]*MFAFactor
	Recovery    map[uuid.UUID]map[string]bool // user -> code hash -> used
	Passkeys    map[uuid.UUID]*Passkey
	Invitations map[uuid.UUID]*Invitation
	Tokens      map[uuid.UUID]string // invitation id -> token hash
	Locale      string
	Mail        Mailer
	Bans        BanHasher
	ResetTokens map[string]*PasswordResetToken
	ResetOwner  map[string]uuid.UUID
	Audits      []audit.Event
	Published   []events.Event
}

var errFakeNotImplemented = errors.New("fake: not implemented")

func New() *MemDB {
	db := &MemDB{
		Users:       map[uuid.UUID]*User{},
		Passwords:   map[uuid.UUID]string{},
		Identities:  map[string]uuid.UUID{},
		Memberships: map[[2]uuid.UUID]*Membership{},
		Sessions:    map[uuid.UUID]*Session{},
		Refresh:     map[string]*RefreshToken{},
		Tenants:     map[uuid.UUID]*Tenant{},
		Factors:     map[uuid.UUID]*MFAFactor{},
		Recovery:    map[uuid.UUID]map[string]bool{},
		Passkeys:    map[uuid.UUID]*Passkey{},
		Invitations: map[uuid.UUID]*Invitation{},
		Tokens:      map[uuid.UUID]string{},
		Locale:      "en",
		ResetTokens: map[string]*PasswordResetToken{},
		ResetOwner:  map[string]uuid.UUID{},
	}
	hasher, err := banhash.New(strings.Join([]string{"unit", "test", "ban", "pepper"}, "-"))
	if err != nil {
		panic(err)
	}
	db.Bans = hasher
	db.Tenants[TestID(1)] = &Tenant{ID: TestID(1), Slug: "default", Name: "Default", Status: TenantStatusActive}
	db.Tenants[TestID(2)] = &Tenant{ID: TestID(2), Slug: "acme", Name: "Acme", Status: TenantStatusActive}
	return db
}

func (db *MemDB) ID() uuid.UUID { db.NextID++; return TestID(db.NextID) }

func (db *MemDB) Stores() Stores {
	return Stores{
		Users:         Users{db},
		Memberships:   Memberships{db},
		Sessions:      Sessions{db},
		RefreshTokens: Refresh{db},
		Resets:        Resets{db},
		Verifications: Verifications{},
		Invitations:   Invitations{db},
		Tenants:       Tenants{db},
		MFA:           MFA{db},
		Passkeys:      Passkeys{db},
		Privacy:       nil,
		Roles:         Roles{db},
		Bans:          db.Bans,
	}
}

// Tx runs fn against the same in-memory stores (no rollback) and records
// audit events. db.Mail, when set, is the transactional email outbox.
type Tx struct{ DB *MemDB }

func (t Tx) InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error {
	// Serialize transactions so a rotation's two writes (mark used + insert
	// the next token) are visible together, like one committed SQL transaction,
	// while individual repository calls keep their own short locks.
	// After-commit callbacks run once the "transaction" lock is released, so
	// a callback that opens another transaction cannot deadlock.
	t.DB.TxMu.Lock()
	var afterCommit []func()
	err := fn(ctx, TxRepos{
		Stores: t.DB.Stores(), Audit: Audit(t), Events: Events(t), Mail: t.DB.Mail,
		AfterCommit: func(f func()) {
			if f != nil {
				afterCommit = append(afterCommit, f)
			}
		},
	})
	t.DB.TxMu.Unlock()
	if err != nil {
		return err
	}
	for _, f := range afterCommit {
		f()
	}
	return nil
}

type Events struct{ DB *MemDB }

func (e Events) Publish(_ context.Context, event events.Event) error {
	e.DB.Mu.Lock()
	defer e.DB.Mu.Unlock()
	e.DB.Published = append(e.DB.Published, event)
	return nil
}

type Audit struct{ DB *MemDB }

func (a Audit) RecordTx(_ context.Context, event audit.Event) error {
	a.DB.Mu.Lock()
	defer a.DB.Mu.Unlock()
	a.DB.Audits = append(a.DB.Audits, event)
	return nil
}

func cloneUser(u *User, db *MemDB) *User {
	c := *u
	_, c.HasPassword = db.Passwords[u.ID]
	return &c
}

type Users struct{ DB *MemDB }

func (r Users) GetByID(_ context.Context, id uuid.UUID) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	u, ok := r.DB.Users[id]
	if !ok || u.Status == UserStatusDeleted {
		return nil, apperror.ErrNotFound
	}
	return cloneUser(u, r.DB), nil
}

func (r Users) GetByEmail(_ context.Context, email string) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for _, u := range r.DB.Users {
		if u.Email == email && u.Status != UserStatusDeleted {
			return cloneUser(u, r.DB), nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (r Users) GetByIdentity(_ context.Context, provider, subject string) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	id, ok := r.DB.Identities[provider+"/"+subject]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return cloneUser(r.DB.Users[id], r.DB), nil
}

func (r Users) Create(_ context.Context, in NewUser) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for _, u := range r.DB.Users {
		if u.Email == in.Email && u.Status != UserStatusDeleted {
			return nil, ErrEmailTaken
		}
	}
	now := time.Now()
	u := &User{ID: r.DB.ID(), Email: in.Email, Name: in.Name, Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if in.EmailVerified {
		u.EmailVerifiedAt = &now
	}
	r.DB.Users[u.ID] = u
	return cloneUser(u, r.DB), nil
}

func (r Users) CreatePassword(_ context.Context, userID uuid.UUID, hash string) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	r.DB.Passwords[userID] = hash
	return nil
}

func (r Users) PasswordHash(_ context.Context, userID uuid.UUID) (string, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	hash, ok := r.DB.Passwords[userID]
	if !ok {
		return "", apperror.ErrNotFound
	}
	return hash, nil
}

func (r Users) RehashPassword(_ context.Context, userID uuid.UUID, currentHash, newHash string) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	if _, ok := r.DB.Users[userID]; !ok {
		return apperror.ErrNotFound
	}
	// Mirror the conditional SQL update: a concurrent credential change wins.
	if r.DB.Passwords[userID] != currentHash {
		return nil
	}
	r.DB.Passwords[userID] = newHash
	return nil
}

func (r Users) ReplacePassword(_ context.Context, userID uuid.UUID, hash string) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	u, ok := r.DB.Users[userID]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	r.DB.Passwords[userID] = hash
	now := time.Now()
	u.TokenVersion++
	u.CredentialsChangedAt = &now
	return cloneUser(u, r.DB), nil
}

func (r Users) LinkIdentity(_ context.Context, in ExternalIdentity) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	if _, ok := r.DB.Identities[in.Provider+"/"+in.Subject]; ok {
		return ErrIdentityTaken
	}
	for key, uid := range r.DB.Identities {
		if uid == in.UserID && len(key) > len(in.Provider) && key[:len(in.Provider)] == in.Provider {
			return ErrIdentityConflict
		}
	}
	r.DB.Identities[in.Provider+"/"+in.Subject] = in.UserID
	return nil
}

func (r Users) UpdateProfile(_ context.Context, in ProfileUpdate) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	u, ok := r.DB.Users[in.ID]
	if !ok || u.Status == UserStatusDeleted {
		return nil, apperror.ErrNotFound
	}
	if in.UpdateEmail && in.Email != u.Email {
		for _, other := range r.DB.Users {
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
	return cloneUser(u, r.DB), nil
}

func (r Users) VerifyEmail(_ context.Context, id uuid.UUID, email string) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	u, ok := r.DB.Users[id]
	if !ok || u.Email != email {
		return nil, apperror.ErrNotFound
	}
	now := time.Now()
	u.EmailVerifiedAt = &now
	return cloneUser(u, r.DB), nil
}

func (r Users) RevokeTokens(_ context.Context, id uuid.UUID) (*User, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	u, ok := r.DB.Users[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	u.TokenVersion++
	return cloneUser(u, r.DB), nil
}

func (r Users) MarkDeleted(context.Context, uuid.UUID) (*User, error) {
	return nil, errFakeNotImplemented
}

type Memberships struct{ DB *MemDB }

func (r Memberships) member(tenantID, userID uuid.UUID) (*Member, bool) {
	m, ok := r.DB.Memberships[[2]uuid.UUID{tenantID, userID}]
	if !ok {
		return nil, false
	}
	return &Member{User: *cloneUser(r.DB.Users[userID], r.DB), Membership: *m}, true
}

func (r Memberships) Get(ctx context.Context, userID uuid.UUID) (*Member, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	m, ok := r.member(tenant.IDFromContext(ctx), userID)
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return m, nil
}

func (r Memberships) GetActive(ctx context.Context, userID uuid.UUID) (*Member, error) {
	m, err := r.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !m.Active() {
		return nil, apperror.ErrNotFound
	}
	return m, nil
}

func (r Memberships) list(ctx context.Context, activeOnly bool, cur pagination.DBCursor) []Member {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	var out []Member
	for key := range r.DB.Memberships {
		if key[0] != tenant.IDFromContext(ctx) {
			continue
		}
		m, _ := r.member(key[0], key[1])
		if !activeOnly || m.Active() {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User.ID.String() > out[j].User.ID.String() })
	if len(out) > int(cur.Limit) {
		out = out[:cur.Limit]
	}
	return out
}

func (r Memberships) ListActiveCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error) {
	return r.list(ctx, true, cur), nil
}

func (r Memberships) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error) {
	return r.list(ctx, false, cur), nil
}

func (r Memberships) ListFilteredCursor(ctx context.Context, cur pagination.DBCursor, status, role string) ([]Member, error) {
	members := r.list(ctx, false, cur)
	out := make([]Member, 0, len(members))
	for _, member := range members {
		if status != "" && string(member.Membership.Status) != status {
			continue
		}
		if role != "" && !containsString(member.Membership.Roles, role) && member.Membership.Role != role {
			continue
		}
		out = append(out, member)
	}
	return out, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r Memberships) ListForUser(_ context.Context, userID uuid.UUID) ([]Membership, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	var out []Membership
	for key, m := range r.DB.Memberships {
		if key[1] == userID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID.String() < out[j].TenantID.String() })
	return out, nil
}

func (r Memberships) Create(ctx context.Context, userID uuid.UUID, role string) (*Membership, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	key := [2]uuid.UUID{tenant.IDFromContext(ctx), userID}
	if _, ok := r.DB.Memberships[key]; ok {
		return nil, ErrAlreadyMember
	}
	now := time.Now()
	m := &Membership{ID: r.DB.ID(), TenantID: key[0], UserID: userID, Role: role, Roles: []string{role}, Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now}
	r.DB.Memberships[key] = m
	c := *m
	return &c, nil
}

// update applies fn to the membership when it is in status from.
func (r Memberships) update(ctx context.Context, userID uuid.UUID, from func(MembershipStatus) bool, fn func(*Membership)) (*Membership, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	m, ok := r.DB.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}]
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

func (r Memberships) Reactivate(ctx context.Context, userID uuid.UUID, role string) (*Membership, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	m, ok := r.DB.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}]
	if !ok || m.Status != MembershipStatusRemoved || m.BannedAt != nil {
		return nil, apperror.ErrNotFound
	}
	m.Status, m.RemovedAt, m.Role, m.Roles = MembershipStatusActive, nil, role, []string{role}
	m.ErasureRequestedAt = nil
	c := *m
	return &c, nil
}

func (r Memberships) UpdateRole(ctx context.Context, userID uuid.UUID, role string) (*Membership, error) {
	return r.update(ctx, userID, is(MembershipStatusActive), func(m *Membership) {
		m.Role, m.Roles = role, []string{role}
	})
}

func (r Memberships) Ban(ctx context.Context, userID uuid.UUID, marker BanMarker) (*Membership, error) {
	return r.update(ctx, userID, is(MembershipStatusActive), func(m *Membership) {
		now := time.Now()
		m.Status, m.BannedAt, m.CredentialsRevokedAt = MembershipStatusSuspended, &now, &now
		m.BannedEmailHash = marker.Hash
	})
}

func (r Memberships) Unban(ctx context.Context, userID uuid.UUID) (*Membership, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	m, ok := r.DB.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}]
	if !ok || m.Status != MembershipStatusSuspended || m.BannedAt == nil || m.ErasureRequestedAt != nil {
		return nil, apperror.ErrNotFound
	}
	m.Status, m.BannedAt, m.BannedEmailHash = MembershipStatusActive, nil, nil
	c := *m
	return &c, nil
}

func (r Memberships) Remove(ctx context.Context, userID uuid.UUID, marker BanMarker) (*Membership, error) {
	return r.update(ctx, userID, func(s MembershipStatus) bool { return s != MembershipStatusRemoved }, func(m *Membership) {
		now := time.Now()
		banned := m.BannedAt != nil
		m.Status, m.RemovedAt, m.CredentialsRevokedAt = MembershipStatusRemoved, &now, &now
		// A ban, and its keyed marker, survives removal and erasure; an
		// erasure reservation is not a ban and never derives one.
		if banned {
			if len(m.BannedEmailHash) == 0 {
				m.BannedEmailHash = marker.Hash
			}
		} else {
			m.BannedAt, m.BannedEmailHash = nil, nil
		}
	})
}

// SuspendForErasure reserves a membership without setting a ban marker.
func (r Memberships) SuspendForErasure(ctx context.Context, userID uuid.UUID) error {
	_, err := r.update(ctx, userID, func(s MembershipStatus) bool { return s != MembershipStatusRemoved }, func(m *Membership) {
		now := time.Now()
		m.Status, m.ErasureRequestedAt, m.RemovedAt = MembershipStatusSuspended, &now, nil
		m.CredentialsRevokedAt = &now
	})
	return err
}

func (r Memberships) CountActiveOwners(ctx context.Context) (int64, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	var n int64
	for key, m := range r.DB.Memberships {
		if key[0] == tenant.IDFromContext(ctx) && m.Role == RoleOwner && m.Status == MembershipStatusActive {
			n++
		}
	}
	return n, nil
}

func (r Memberships) LockOwnerChanges(context.Context) error { return nil }

func (r Memberships) TouchIfUnchanged(context.Context, uuid.UUID, time.Time) (bool, error) {
	return true, nil
}

func (r Memberships) LockMembership(ctx context.Context, userID uuid.UUID) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	if _, ok := r.DB.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}]; !ok {
		return apperror.ErrNotFound
	}
	return nil
}

func (r Memberships) Stats(context.Context) (MemberStats, error) {
	return MemberStats{}, errFakeNotImplemented
}

func (r Memberships) EmailBanned(ctx context.Context, hashes [][]byte) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for key, m := range r.DB.Memberships {
		if key[0] != tenant.IDFromContext(ctx) || len(m.BannedEmailHash) == 0 {
			continue
		}
		for _, hash := range hashes {
			if bytes.Equal(m.BannedEmailHash, hash) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r Memberships) BannedMarkerCounts(context.Context) ([]BanMarkerCount, error) {
	return nil, errFakeNotImplemented
}

func (r Memberships) GetPrincipal(ctx context.Context, userID uuid.UUID) (*Principal, error) {
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

func (r Memberships) LockPrincipal(ctx context.Context, userID uuid.UUID) (*Principal, error) {
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

type Sessions struct{ DB *MemDB }

func (r Sessions) Create(ctx context.Context, in NewSession) (*Session, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	tid := tenant.IDFromContext(ctx)
	m, ok := r.DB.Memberships[[2]uuid.UUID{tid, in.UserID}]
	u := r.DB.Users[in.UserID]
	if !ok || m.Status != MembershipStatusActive || u.Status != UserStatusActive || u.TokenVersion != in.TokenVersion {
		return nil, apperror.ErrNotFound
	}
	now := time.Now()
	s := &Session{ID: r.DB.ID(), TenantID: tid, UserID: in.UserID, TokenVersion: in.TokenVersion,
		UserAgent: in.UserAgent, IP: in.IP, AMR: in.AMR, MFAVerifiedAt: in.MFAVerifiedAt,
		ExpiresAt: in.ExpiresAt, CreatedAt: now, LastUsedAt: now}
	r.DB.Sessions[s.ID] = s
	c := *s
	return &c, nil
}

func (r Sessions) Get(ctx context.Context, id uuid.UUID) (*Session, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	s, ok := r.DB.Sessions[id]
	if !ok || s.TenantID != tenant.IDFromContext(ctx) {
		return nil, apperror.ErrNotFound
	}
	c := *s
	return &c, nil
}

func (r Sessions) ListLiveCursor(context.Context, uuid.UUID, pagination.DBCursor) ([]Session, error) {
	return nil, errFakeNotImplemented
}

func (r Sessions) revoke(match func(*Session) bool, why RevokeReason) (userID uuid.UUID, n int) {
	now := time.Now()
	for _, s := range r.DB.Sessions {
		if s.RevokedAt == nil && match(s) {
			s.RevokedAt, s.RevokedReason = &now, why
			userID = s.UserID
			n++
		}
	}
	return userID, n
}

func (r Sessions) Revoke(ctx context.Context, userID, id uuid.UUID, why RevokeReason) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	tid := tenant.IDFromContext(ctx)
	_, n := r.revoke(func(s *Session) bool { return s.ID == id && s.UserID == userID && s.TenantID == tid }, why)
	return n > 0, nil
}

func (r Sessions) RevokeAllInTenant(ctx context.Context, userID uuid.UUID, why RevokeReason) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	tid := tenant.IDFromContext(ctx)
	r.revoke(func(s *Session) bool { return s.UserID == userID && s.TenantID == tid }, why)
	return nil
}

func (r Sessions) RevokeAllForUser(_ context.Context, userID uuid.UUID, why RevokeReason) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	r.revoke(func(s *Session) bool { return s.UserID == userID }, why)
	return nil
}

func (r Sessions) RevokeStaleForUser(_ context.Context, userID uuid.UUID, why RevokeReason) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	u := r.DB.Users[userID]
	if u == nil {
		return nil
	}
	version := u.TokenVersion
	r.revoke(func(s *Session) bool { return s.UserID == userID && s.TokenVersion != version }, why)
	return nil
}

func (r Sessions) RevokeOtherUserSessions(_ context.Context, userID, keepID uuid.UUID, why RevokeReason) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	r.revoke(func(s *Session) bool { return s.UserID == userID && s.ID != keepID }, why)
	return nil
}

func (r Sessions) Restamp(ctx context.Context, userID, id uuid.UUID, tokenVersion int32, amr []string) (*Session, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	s, ok := r.DB.Sessions[id]
	if !ok || s.UserID != userID || s.TenantID != tenant.IDFromContext(ctx) || s.RevokedAt != nil {
		return nil, apperror.ErrNotFound
	}
	s.TokenVersion, s.AMR, s.LastUsedAt = tokenVersion, amr, time.Now()
	c := *s
	return &c, nil
}

func (r Sessions) DeleteRefreshTokens(_ context.Context, sessionID uuid.UUID) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for hash, token := range r.DB.Refresh {
		if token.SessionID == sessionID {
			delete(r.DB.Refresh, hash)
		}
	}
	return nil
}

func (r Sessions) RevokeByRefreshToken(_ context.Context, hash []byte, why RevokeReason) (userID uuid.UUID, revoked bool, err error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	rt, ok := r.DB.Refresh[string(hash)]
	if !ok {
		return uuid.Nil, false, nil
	}
	userID, n := r.revoke(func(s *Session) bool { return s.ID == rt.SessionID }, why)
	return userID, n > 0, nil
}

func (r Sessions) Touch(context.Context, uuid.UUID, string, string) error { return nil }

func (r Sessions) MarkMFAVerified(ctx context.Context, userID, sessionID uuid.UUID) (time.Time, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	s, ok := r.DB.Sessions[sessionID]
	if !ok || s.UserID != userID || s.TenantID != tenant.IDFromContext(ctx) || s.RevokedAt != nil {
		return time.Time{}, apperror.ErrNotFound
	}
	now := time.Now()
	s.MFAVerifiedAt = &now
	return now, nil
}

func (r Sessions) MarkMFAVerifiedWithMethod(ctx context.Context, userID, sessionID uuid.UUID, method string) (time.Time, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	s, ok := r.DB.Sessions[sessionID]
	if !ok || s.UserID != userID || s.TenantID != tenant.IDFromContext(ctx) || s.RevokedAt != nil {
		return time.Time{}, apperror.ErrNotFound
	}
	now := time.Now()
	s.MFAVerifiedAt = &now
	s.AMR = withAMR(s.AMR, method)
	return now, nil
}

func (r Sessions) State(context.Context, uuid.UUID, uuid.UUID) (*SessionState, error) {
	return nil, errFakeNotImplemented
}

type Refresh struct{ DB *MemDB }

func (r Refresh) Create(ctx context.Context, sessionID uuid.UUID, hash []byte, expiresAt time.Time) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	r.DB.Refresh[string(hash)] = &RefreshToken{ID: r.DB.ID(), TenantID: tenant.IDFromContext(ctx), SessionID: sessionID, ExpiresAt: expiresAt, CreatedAt: time.Now()}
	return nil
}

func (r Refresh) GetByHash(_ context.Context, hash []byte) (*RefreshToken, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	rt, ok := r.DB.Refresh[string(hash)]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	c := *rt
	return &c, nil
}

func (r Refresh) MarkUsed(_ context.Context, tenantID, id uuid.UUID) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for _, rt := range r.DB.Refresh {
		if rt.ID == id && rt.TenantID == tenantID && rt.UsedAt == nil {
			now := time.Now()
			rt.UsedAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (r Refresh) ClassifyRotation(_ context.Context, hash []byte, usedAfter time.Time) (RefreshRotationState, error) {
	// The real query reads committed rows only. Waiting for any in-flight
	// fake transaction models that: a concurrent rotation's "mark used" and
	// "insert next token" are seen together or not at all. The service calls
	// this outside any transaction, so taking TxMu cannot deadlock.
	r.DB.TxMu.Lock()
	defer r.DB.TxMu.Unlock()
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	rt, ok := r.DB.Refresh[string(hash)]
	if !ok {
		return RefreshRotationState{}, apperror.ErrNotFound
	}
	sess, ok := r.DB.Sessions[rt.SessionID]
	if !ok {
		return RefreshRotationState{}, apperror.ErrNotFound
	}
	state := RefreshRotationState{
		SessionLive:     sess.RevokedAt == nil && sess.ExpiresAt.After(time.Now()),
		UsedWithinGrace: rt.UsedAt != nil && rt.UsedAt.After(usedAfter),
	}
	for _, other := range r.DB.Refresh {
		if other.SessionID == rt.SessionID && other.ID.String() > rt.ID.String() {
			state.Rotated = true
			break
		}
	}
	return state, nil
}

type Resets struct{ DB *MemDB }

func (r Resets) Create(_ context.Context, userID uuid.UUID, email string, tokenHash []byte, expiresAt time.Time) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	u, ok := r.DB.Users[userID]
	if !ok || u.Status == UserStatusDeleted || u.Email != email {
		return false, nil
	}
	for hash, owner := range r.DB.ResetOwner {
		if owner == userID {
			delete(r.DB.ResetOwner, hash)
			delete(r.DB.ResetTokens, hash)
		}
	}
	r.DB.ResetOwner[string(tokenHash)] = userID
	r.DB.ResetTokens[string(tokenHash)] = &PasswordResetToken{ID: r.DB.ID(), UserID: userID, ExpiresAt: expiresAt, CreatedAt: time.Now()}
	return true, nil
}

func (r Resets) GetByHash(_ context.Context, tokenHash []byte) (*PasswordResetToken, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	token, ok := r.DB.ResetTokens[string(tokenHash)]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	c := *token
	return &c, nil
}

func (r Resets) Consume(_ context.Context, tokenHash []byte) (*PasswordResetToken, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	token, ok := r.DB.ResetTokens[string(tokenHash)]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	delete(r.DB.ResetTokens, string(tokenHash))
	delete(r.DB.ResetOwner, string(tokenHash))
	c := *token
	return &c, nil
}

func (r Resets) Delete(_ context.Context, tokenHash []byte) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	delete(r.DB.ResetTokens, string(tokenHash))
	delete(r.DB.ResetOwner, string(tokenHash))
	return nil
}

func (r Resets) DeleteByUser(_ context.Context, userID uuid.UUID) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for hash, owner := range r.DB.ResetOwner {
		if owner == userID {
			delete(r.DB.ResetOwner, hash)
			delete(r.DB.ResetTokens, hash)
		}
	}
	return nil
}

type Verifications struct{}

func (Verifications) Create(context.Context, uuid.UUID, string, []byte, time.Time) error {
	return nil
}
func (Verifications) Consume(context.Context, []byte) (*VerificationToken, error) {
	return nil, errFakeNotImplemented
}
func (Verifications) DeleteByUser(context.Context, uuid.UUID) error { return nil }

type Invitations struct{ DB *MemDB }

func (r Invitations) Upsert(ctx context.Context, in NewInvitation) (*Invitation, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	tid := tenant.IDFromContext(ctx)
	now := time.Now()
	for id, inv := range r.DB.Invitations {
		if inv.TenantID == tid && inv.Email == in.Email && inv.AcceptedAt == nil {
			inv.Role, inv.InvitedBy, inv.ExpiresAt, inv.UpdatedAt = in.Role, in.InvitedBy, in.ExpiresAt, now
			r.DB.Tokens[id] = string(in.TokenHash)
			c := *inv
			return &c, nil
		}
	}
	inv := &Invitation{
		ID: r.DB.ID(), TenantID: tid, Email: in.Email, Role: in.Role,
		InvitedBy: in.InvitedBy, ExpiresAt: in.ExpiresAt, CreatedAt: now, UpdatedAt: now,
	}
	r.DB.Invitations[inv.ID] = inv
	r.DB.Tokens[inv.ID] = string(in.TokenHash)
	c := *inv
	return &c, nil
}

func (r Invitations) ListPendingCursor(context.Context, pagination.DBCursor) ([]Invitation, error) {
	return nil, errFakeNotImplemented
}

func (r Invitations) Get(ctx context.Context, id uuid.UUID) (*Invitation, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	inv, ok := r.DB.Invitations[id]
	if !ok || inv.TenantID != tenant.IDFromContext(ctx) {
		return nil, apperror.ErrNotFound
	}
	c := *inv
	return &c, nil
}

func (r Invitations) DeletePending(ctx context.Context, id uuid.UUID) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	inv, ok := r.DB.Invitations[id]
	if !ok || inv.TenantID != tenant.IDFromContext(ctx) || inv.AcceptedAt != nil {
		return false, nil
	}
	delete(r.DB.Invitations, id)
	delete(r.DB.Tokens, id)
	return true, nil
}

func (r Invitations) TenantByTokenHash(_ context.Context, tokenHash []byte) (uuid.UUID, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for id, hash := range r.DB.Tokens {
		if hash == string(tokenHash) {
			return r.DB.Invitations[id].TenantID, nil
		}
	}
	return uuid.Nil, apperror.ErrNotFound
}

func (r Invitations) GetByTokenHashForUpdate(ctx context.Context, tenantID uuid.UUID, tokenHash []byte) (*Invitation, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for id, hash := range r.DB.Tokens {
		if hash != string(tokenHash) {
			continue
		}
		inv := r.DB.Invitations[id]
		if inv.TenantID != tenantID || inv.TenantID != tenant.IDFromContext(ctx) {
			return nil, apperror.ErrNotFound
		}
		c := *inv
		return &c, nil
	}
	return nil, apperror.ErrNotFound
}

func (r Invitations) MarkAccepted(ctx context.Context, tenantID, id, userID uuid.UUID) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	inv, ok := r.DB.Invitations[id]
	if !ok || inv.TenantID != tenantID || inv.AcceptedAt != nil {
		return false, nil
	}
	now := time.Now()
	inv.AcceptedAt, inv.AcceptedBy, inv.UpdatedAt = &now, uuid.NullUUID{UUID: userID, Valid: true}, now
	return true, nil
}

type Tenants struct{ DB *MemDB }

func (r Tenants) get(id uuid.UUID, operational bool) (*Tenant, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	t, ok := r.DB.Tenants[id]
	if !ok || (operational && !t.Status.Operational()) {
		return nil, apperror.ErrNotFound
	}
	c := *t
	return &c, nil
}

func (r Tenants) GetOperationalBySlug(context.Context, string) (*Tenant, error) {
	return nil, errFakeNotImplemented
}
func (r Tenants) GetOperationalByID(_ context.Context, id uuid.UUID) (*Tenant, error) {
	return r.get(id, true)
}
func (r Tenants) GetByID(_ context.Context, id uuid.UUID) (*Tenant, error) {
	return r.get(id, false)
}
func (r Tenants) GetBySlug(context.Context, string) (*Tenant, error) {
	return nil, errFakeNotImplemented
}
func (r Tenants) GetByIDForUpdate(_ context.Context, id uuid.UUID) (*Tenant, error) {
	return r.get(id, false)
}
func (r Tenants) ListCursor(_ context.Context, _ pagination.DBCursor) ([]Tenant, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	Tenants := make([]Tenant, 0, len(r.DB.Tenants))
	for _, t := range r.DB.Tenants {
		Tenants = append(Tenants, *t)
	}
	sort.Slice(Tenants, func(i, j int) bool { return Tenants[i].ID.String() > Tenants[j].ID.String() })
	return Tenants, nil
}
func (r Tenants) Create(context.Context, NewTenant) (*Tenant, error) {
	return nil, errFakeNotImplemented
}

func (r Tenants) TouchIfUnchanged(context.Context, uuid.UUID, time.Time) (bool, error) {
	return true, nil
}

func (r Tenants) UpdateName(_ context.Context, id uuid.UUID, name string) (*Tenant, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	t, ok := r.DB.Tenants[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	t.Name = name
	t.UpdatedAt = time.Now()
	c := *t
	return &c, nil
}
func (r Tenants) UpdateStatus(_ context.Context, id uuid.UUID, status TenantStatus, deletion *time.Time) (*Tenant, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	t, ok := r.DB.Tenants[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	t.Status, t.DeletionScheduledAt, t.StatusChangedAt = status, deletion, time.Now()
	c := *t
	return &c, nil
}
func (r Tenants) Settings(_ context.Context, id uuid.UUID) (*TenantSettings, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	if _, ok := r.DB.Tenants[id]; !ok {
		return nil, apperror.ErrNotFound
	}
	Locale := r.DB.Locale
	if Locale == "" {
		Locale = "en"
	}
	return &TenantSettings{TenantID: id, Locale: Locale, Timezone: "UTC"}, nil
}

type MFA struct{ DB *MemDB }

func (r MFA) GetTOTP(_ context.Context, userID uuid.UUID) (*MFAFactor, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	f, ok := r.DB.Factors[userID]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	c := *f
	return &c, nil
}

func (r MFA) GetTOTPForUpdate(ctx context.Context, userID uuid.UUID) (*MFAFactor, error) {
	return r.GetTOTP(ctx, userID)
}

func (r MFA) GetConfirmedTOTP(_ context.Context, userID uuid.UUID) (*MFAFactor, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	f, ok := r.DB.Factors[userID]
	if !ok || f.ConfirmedAt == nil {
		return nil, apperror.ErrNotFound
	}
	c := *f
	return &c, nil
}

func (r MFA) UpsertUnconfirmedTOTP(_ context.Context, userID uuid.UUID, sealed string) (*MFAFactor, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	now := time.Now()
	f, ok := r.DB.Factors[userID]
	if !ok {
		f = &MFAFactor{ID: r.DB.ID(), UserID: userID, Type: MFAFactorTOTP, CreatedAt: now}
		r.DB.Factors[userID] = f
	}
	if f.ConfirmedAt != nil {
		return nil, errors.New("fake: factor already confirmed")
	}
	f.SecretCiphertext = sealed
	f.UpdatedAt = now
	c := *f
	return &c, nil
}

func (r MFA) ConfirmTOTP(_ context.Context, id, userID uuid.UUID, step int64) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	f, ok := r.DB.Factors[userID]
	if !ok || f.ID != id || f.ConfirmedAt != nil {
		return false, nil
	}
	now := time.Now()
	f.ConfirmedAt = &now
	f.LastUsedStep = step
	return true, nil
}

func (r MFA) ConsumeTOTPStep(_ context.Context, userID uuid.UUID, step int64) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	f, ok := r.DB.Factors[userID]
	if !ok || f.ConfirmedAt == nil || f.LastUsedStep >= step {
		return false, nil
	}
	f.LastUsedStep = step
	return true, nil
}

func (r MFA) DeleteTOTP(_ context.Context, userID uuid.UUID) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	delete(r.DB.Factors, userID)
	return nil
}

func (r MFA) DeletePendingTOTP(_ context.Context, userID uuid.UUID) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	if f, ok := r.DB.Factors[userID]; ok && f.ConfirmedAt == nil {
		delete(r.DB.Factors, userID)
	}
	return nil
}

func (r MFA) DeleteRecoveryCodes(_ context.Context, userID uuid.UUID) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	delete(r.DB.Recovery, userID)
	return nil
}

func (r MFA) CreateRecoveryCodes(_ context.Context, userID uuid.UUID, hashes [][]byte) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	codes := r.DB.Recovery[userID]
	if codes == nil {
		codes = map[string]bool{}
		r.DB.Recovery[userID] = codes
	}
	for _, hash := range hashes {
		codes[string(hash)] = false
	}
	return nil
}

func (r MFA) UseRecoveryCode(_ context.Context, userID uuid.UUID, hash []byte) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	codes := r.DB.Recovery[userID]
	used, ok := codes[string(hash)]
	if !ok || used {
		return false, nil
	}
	codes[string(hash)] = true
	return true, nil
}

func (r MFA) CountUnusedRecoveryCodes(_ context.Context, userID uuid.UUID) (int64, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	var n int64
	for _, used := range r.DB.Recovery[userID] {
		if !used {
			n++
		}
	}
	return n, nil
}

// Roles resolves the built-in tenant roles (enough for inviter authority
// checks in unit tests); tenant-defined roles are covered by integration tests.
type Passkeys struct{ DB *MemDB }

func (r Passkeys) Create(_ context.Context, in Passkey) (*Passkey, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for _, existing := range r.DB.Passkeys {
		if bytes.Equal(existing.CredentialID, in.CredentialID) {
			return nil, ErrPasskeyAlreadyRegistered
		}
	}
	if in.ID == uuid.Nil {
		in.ID = r.DB.ID()
	}
	now := time.Unix(1_700_000_000, 0)
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	in.UpdatedAt = now
	created := in
	r.DB.Passkeys[created.ID] = &created
	copied := created
	return &copied, nil
}

func (r Passkeys) ListByUser(_ context.Context, userID uuid.UUID) ([]Passkey, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	out := make([]Passkey, 0)
	for _, passkey := range r.DB.Passkeys {
		if passkey.UserID == userID {
			out = append(out, *passkey)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (r Passkeys) GetByCredentialID(_ context.Context, credentialID []byte) (*Passkey, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for _, passkey := range r.DB.Passkeys {
		if bytes.Equal(passkey.CredentialID, credentialID) {
			copied := *passkey
			return &copied, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (r Passkeys) Get(_ context.Context, userID, id uuid.UUID) (*Passkey, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	passkey, ok := r.DB.Passkeys[id]
	if !ok || passkey.UserID != userID {
		return nil, apperror.ErrNotFound
	}
	copied := *passkey
	return &copied, nil
}

func (r Passkeys) Delete(_ context.Context, userID, id uuid.UUID) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	passkey, ok := r.DB.Passkeys[id]
	if !ok || passkey.UserID != userID {
		return false, nil
	}
	delete(r.DB.Passkeys, id)
	return true, nil
}

func (r Passkeys) DeleteByUser(_ context.Context, userID uuid.UUID) error {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	for id, passkey := range r.DB.Passkeys {
		if passkey.UserID == userID {
			delete(r.DB.Passkeys, id)
		}
	}
	return nil
}

func (r Passkeys) UpdateAfterAssertion(_ context.Context, userID, id uuid.UUID, signCount int64, backupState bool) (bool, error) {
	r.DB.Mu.Lock()
	defer r.DB.Mu.Unlock()
	passkey, ok := r.DB.Passkeys[id]
	if !ok || passkey.UserID != userID {
		return false, nil
	}
	if passkey.SignCount >= signCount && (signCount != 0 || passkey.SignCount != 0) {
		return false, nil
	}
	passkey.SignCount = signCount
	passkey.BackupState = backupState
	now := time.Unix(1_700_000_000, 0)
	passkey.LastUsedAt = &now
	return true, nil
}

type Roles struct{ DB *MemDB }

func (r Roles) GetByKey(_ context.Context, key string) (*Role, error) {
	if !ValidRole(key) {
		return nil, apperror.ErrNotFound
	}
	return &Role{Key: key, Name: key, Scope: "tenant"}, nil
}

func (r Roles) SyncCatalog(context.Context, []CatalogPermission, []CatalogRole) error {
	return errFakeNotImplemented
}
func (r Roles) ListCursor(context.Context, pagination.DBCursor) ([]Role, error) {
	return nil, errFakeNotImplemented
}
func (r Roles) Get(context.Context, uuid.UUID) (*Role, error) {
	return nil, errFakeNotImplemented
}
func (r Roles) Create(context.Context, NewRole) (*Role, error) {
	return nil, errFakeNotImplemented
}
func (r Roles) Update(context.Context, uuid.UUID, string, []string, *time.Time) (*Role, error) {
	return nil, errFakeNotImplemented
}
func (r Roles) Delete(context.Context, uuid.UUID) (bool, error) {
	return false, errFakeNotImplemented
}
func (r Roles) MemberIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, errFakeNotImplemented
}
func (r Roles) ReplaceMemberRoles(context.Context, uuid.UUID, []string) error {
	return errFakeNotImplemented
}
func (r Roles) AssignPlatformRole(context.Context, uuid.UUID, string) error {
	return errFakeNotImplemented
}

// withAMR appends method to amr unless it is already present.
func withAMR(amr []string, method string) []string {
	for _, value := range amr {
		if value == method {
			return append([]string(nil), amr...)
		}
	}
	return append(append([]string(nil), amr...), method)
}
