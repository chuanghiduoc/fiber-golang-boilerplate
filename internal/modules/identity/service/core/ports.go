package core

import (
	"context"
	"errors"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/google/uuid"
)

// Persistence ports of the identity module. The repository package
// implements them over sqlc and maps rows to these types; services never see
// generated SQL types or pgx.
//
// Tenant scoping: methods of tenant-owned stores (memberships, sessions,
// refresh tokens, invitations) take the tenant from the context
// (tenant.Require(ctx)). Users, passwords, external identities, and reset /
// verification tokens are global and take no tenant. The few
// deliberately cross-tenant methods say so in their doc comment.

// Sentinel errors returned by the repositories for unique violations the
// services translate into public errors.
var (
	// ErrEmailTaken: a live user already has the address.
	ErrEmailTaken = errors.New("identity: email already registered")
	// ErrAlreadyMember: the user already has a membership in the tenant.
	ErrAlreadyMember = errors.New("identity: membership already exists")
	// ErrIdentityTaken: the external subject is linked to another user.
	ErrIdentityTaken = errors.New("identity: external identity linked to another user")
	// ErrIdentityConflict: the user is already linked to a different subject
	// of the same provider (a link is never replaced).
	ErrIdentityConflict = errors.New("identity: user already linked to another external identity")
	// ErrTenantSlugTaken: another tenant has the slug.
	ErrTenantSlugTaken = errors.New("identity: tenant slug already exists")
	// ErrRoleKeyTaken: the tenant already defines a role with the key.
	ErrRoleKeyTaken = errors.New("identity: role key already exists")
	// ErrRoleInUse: a membership still holds the role.
	ErrRoleInUse = errors.New("identity: role is still assigned")
	// ErrRoleNotFound: the role key does not resolve in this tenant.
	ErrRoleNotFound = errors.New("identity: role not found")
	// ErrPasskeyAlreadyRegistered: the WebAuthn credential id is already
	// registered to a user.
	ErrPasskeyAlreadyRegistered = errors.New("identity: passkey credential already registered")
)

// UserStatus is the global account state (users.status).
type UserStatus string

const (
	UserStatusActive    UserStatus = "active"
	UserStatusSuspended UserStatus = "suspended"
	UserStatusDeleted   UserStatus = "deleted"
)

// MembershipStatus is the per-tenant state (memberships.status).
type MembershipStatus string

const (
	MembershipStatusActive    MembershipStatus = "active"
	MembershipStatusSuspended MembershipStatus = "suspended" // banned by a tenant admin
	MembershipStatusRemoved   MembershipStatus = "removed"
)

// ValidMembershipStatus reports whether status is one of the three stored
// membership states.
func ValidMembershipStatus(status string) bool {
	switch MembershipStatus(status) {
	case MembershipStatusActive, MembershipStatusSuspended, MembershipStatusRemoved:
		return true
	}
	return false
}

// TenantStatus is the tenant lifecycle state (tenants.status).
type TenantStatus string

const (
	TenantStatusTrialing        TenantStatus = "trialing"
	TenantStatusActive          TenantStatus = "active"
	TenantStatusSuspended       TenantStatus = "suspended"
	TenantStatusPendingDeletion TenantStatus = "pending_deletion"
	TenantStatusDeleted         TenantStatus = "deleted"
)

// Operational reports whether a tenant in this state serves requests. An
// unknown state is not operational (fail closed).
func (s TenantStatus) Operational() bool {
	switch s {
	case TenantStatusTrialing, TenantStatusActive:
		return true
	case TenantStatusSuspended, TenantStatusPendingDeletion, TenantStatusDeleted:
		return false
	default:
		return false
	}
}

// System tenant role keys (roles.key where tenant_id IS NULL).
const (
	RoleMember = "member"
	RoleAdmin  = "admin"
	RoleOwner  = "owner"
)

// ValidRole reports whether role is a system tenant role.
func ValidRole(role string) bool {
	switch role {
	case RoleMember, RoleAdmin, RoleOwner:
		return true
	default:
		return false
	}
}

// RevokeReason records why a session ended (sessions.revoked_reason).
type RevokeReason string

const (
	RevokeLogout             RevokeReason = "logout"
	RevokeLogoutAll          RevokeReason = "logout_all"
	RevokeByUser             RevokeReason = "revoked"
	RevokeRefreshReuse       RevokeReason = "refresh_reuse"
	RevokeCredentialsChanged RevokeReason = "credentials_changed"
	RevokeMembershipChanged  RevokeReason = "membership_changed"
	RevokeAccountDeleted     RevokeReason = "account_deleted"
	// RevokeMFAChanged ends the user's other sessions after an MFA factor or
	// recovery-code set changes.
	RevokeMFAChanged RevokeReason = "mfa_changed"
)

// MFA method values recorded in access-token amr claims (RFC 8176).
const (
	AMRPassword     = "pwd"
	AMRGoogle       = "google"
	AMROTP          = "otp"
	AMRRecoveryCode = "rc"
	// AMRHardwareKey is RFC 8176 "hwk": a proof of possession of a
	// hardware-secured key. Passkeys that are not backed up (device-bound)
	// record this method.
	AMRHardwareKey = "hwk"
	// AMRSoftwareKey is RFC 8176 "swk": a proof of possession of a
	// software-secured key. Synced/backup-eligible passkeys record this
	// method.
	AMRSoftwareKey = "swk"
)

// MFA method values recorded by passkey ceremonies. Both are
// phishing-resistant AAL2 methods (RFC 8176).
const (
	PasskeyMethodHardware = AMRHardwareKey
	PasskeyMethodSoftware = AMRSoftwareKey
)

// MFA factor types (mfa_factors.type).
const MFAFactorTOTP = "totp"

// External identity providers (user_identities.provider).
const ProviderGoogle = "google"

// User is a global identity.
type User struct {
	ID                   uuid.UUID
	Email                string
	Name                 string
	EmailVerifiedAt      *time.Time
	Status               UserStatus
	TokenVersion         int32
	CredentialsChangedAt *time.Time
	HasPassword          bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time
}

// EmailVerified reports whether the current address was verified.
func (u *User) EmailVerified() bool { return u.EmailVerifiedAt != nil }

// Membership links one user to one tenant.
type Membership struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	UserID   uuid.UUID
	// Role is the highest system role key (owner > admin > member).
	Role string
	// Roles holds every assigned role key.
	Roles    []string
	Status   MembershipStatus
	BannedAt *time.Time
	// BannedEmailHash is the keyed pseudonymous marker (HMAC-SHA256, lowercase
	// hex) of the normalized address, written by a ban and kept after
	// removal/erasure.
	BannedEmailHash []byte
	// ErasureRequestedAt reserves the membership for erasure: it is suspended
	// and can no longer act until privacy.erase removes it. Unlike BannedAt it
	// carries no ban semantics.
	ErasureRequestedAt   *time.Time
	RemovedAt            *time.Time
	CredentialsRevokedAt *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// BanMarker is the keyed pseudonymous marker and the id of the key that wrote
// it, stored on a banned membership.
type BanMarker struct {
	Hash  []byte
	KeyID string
}

// BanHasher computes the keyed marker for an address. The composition root
// binds it to the data-encryption key set (platform/banhash).
type BanHasher interface {
	// Marker returns the marker and key id a new ban records.
	Marker(email string) (hash []byte, kid string)
	// Markers returns the marker under every configured key (active first),
	// so a lookup still matches a ban written before a key rotation.
	Markers(email string) [][]byte
	// KeyIDs returns the configured key ids, active first.
	KeyIDs() []string
}

// BanMarkerCount is the number of retained ban markers per key id, for the
// startup check that reports markers whose key is no longer configured.
type BanMarkerCount struct {
	KeyID string
	Bans  int64
}

// Member is a user as seen from one tenant.
type Member struct {
	User       User
	Membership Membership
}

// UpdatedAt is the later of the user's and the membership's last change.
func (m *Member) UpdatedAt() time.Time {
	if m.Membership.UpdatedAt.After(m.User.UpdatedAt) {
		return m.Membership.UpdatedAt
	}
	return m.User.UpdatedAt
}

// DeletedAt is when the member left the tenant, or else when the global
// account was deleted; nil while both exist.
func (m *Member) DeletedAt() *time.Time {
	if m.Membership.RemovedAt != nil {
		return m.Membership.RemovedAt
	}
	return m.User.DeletedAt
}

// Active reports whether the member may act in the tenant.
func (m *Member) Active() bool {
	return m.User.Status == UserStatusActive && m.Membership.Status == MembershipStatusActive
}

// NewUser is the input for creating a global user.
type NewUser struct {
	Email         string // normalized
	Name          string
	EmailVerified bool
}

// ProfileUpdate changes a user's global profile. Email is only applied when
// UpdateEmail is set.
type ProfileUpdate struct {
	ID          uuid.UUID
	Name        string
	Email       string
	UpdateEmail bool
}

// ExternalIdentity links a user to a provider subject.
type ExternalIdentity struct {
	UserID   uuid.UUID
	Provider string
	Subject  string
	Email    string
}

// MemberStats is the identity half of the admin dashboard for one tenant.
type MemberStats struct {
	Active   int64
	Inactive int64
}

// Principal is an API-key owner's authorization view (an active membership).
type Principal struct {
	UserID       uuid.UUID
	TenantID     uuid.UUID
	Email        string
	Name         string
	Role         string
	Permissions  []string
	Platform     bool
	TenantStatus TenantStatus
	// CredentialsRevokedAt is set by a ban or removal and never cleared.
	CredentialsRevokedAt *time.Time
}

// Session is one sign-in in one tenant: the refresh-token family.
type Session struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	UserID       uuid.UUID
	TokenVersion int32
	UserAgent    string
	IP           string
	AMR          []string
	// MFAVerifiedAt is when this session's second factor was last verified;
	// nil for a first-factor-only session.
	MFAVerifiedAt *time.Time
	LastUsedAt    time.Time
	CreatedAt     time.Time
	// ExpiresAt is the absolute session lifetime (SESSION_MAX_LIFETIME).
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	RevokedReason RevokeReason
}

// NewSession is the input for opening a session in the request tenant.
type NewSession struct {
	UserID uuid.UUID
	// TokenVersion is the user's version the caller authenticated at; the
	// insert fails (apperror.ErrNotFound) when it is stale.
	TokenVersion int32
	// ExpiresAt is now + SESSION_MAX_LIFETIME; refresh tokens never outlive it.
	ExpiresAt time.Time
	UserAgent string
	IP        string
	// AMR is the authentication-method reference set (pwd, google, otp, rc).
	AMR []string
	// MFAVerifiedAt is set when the session is created after a second factor
	// (otp/rc in AMR); nil leaves sessions.mfa_verified_at NULL.
	MFAVerifiedAt *time.Time
}

// SessionState is everything the access-token validator compares.
type SessionState struct {
	TenantID  uuid.UUID
	Revoked   bool
	ExpiresAt time.Time
	// MFAVerifiedAt is when the session's second factor was verified; the zero
	// time means never (a first-factor-only session).
	MFAVerifiedAt    time.Time
	AMR              []string
	Role             string
	Permissions      []string
	Platform         bool
	MembershipStatus MembershipStatus
	TokenVersion     int32
	UserStatus       UserStatus
	TenantStatus     TenantStatus
}

// RefreshToken is one rotation of a session's refresh token.
type RefreshToken struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	SessionID uuid.UUID
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// PasswordResetToken is a pending password reset (per user).
type PasswordResetToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
	CreatedAt time.Time
}

// VerificationToken is a pending email verification bound to an address.
type VerificationToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Email     string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Invitation is a pending (or accepted) invitation into a tenant.
type Invitation struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Email      string
	Role       string
	InvitedBy  uuid.UUID
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	AcceptedBy uuid.NullUUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewInvitation is the input for creating (or refreshing) a pending
// invitation in the request tenant.
type NewInvitation struct {
	Email     string // normalized
	Role      string
	TokenHash []byte
	InvitedBy uuid.UUID
	ExpiresAt time.Time
}

// Tenant is one tenant and its lifecycle state.
type Tenant struct {
	ID                  uuid.UUID
	Slug                string
	Name                string
	Status              TenantStatus
	StatusChangedAt     time.Time
	TrialEndsAt         *time.Time
	DeletionScheduledAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// NewTenant is the input for creating a tenant and its settings row.
type NewTenant struct {
	Slug        string
	Name        string
	Status      TenantStatus // TenantStatusTrialing or TenantStatusActive
	TrialEndsAt *time.Time
	Locale      string
	Timezone    string
	// OwnerUserID, when positive, becomes the tenant's first owner in the
	// same transaction (the platform create endpoint uses the caller).
	OwnerUserID uuid.UUID
}

// TenantUpdate is a merge-patched tenant change: a nil field is unchanged.
type TenantUpdate struct {
	Name   *string
	Status *TenantStatus
	// ExpectedUpdatedAt is the ETag validator the caller read. A mismatch is
	// 412 resource.precondition_failed.
	ExpectedUpdatedAt *time.Time
}

// TenantSettings are a tenant's typed settings.
type TenantSettings struct {
	TenantID uuid.UUID
	Locale   string
	Timezone string
}

// MFAFactor is one MFA factor of a global user (mfa_factors). SecretCiphertext
// is sealed with the data encryption keyring; only the service opens it.
type MFAFactor struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	Type             string
	SecretCiphertext string
	ConfirmedAt      *time.Time
	LastUsedStep     int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Confirmed reports whether the factor completed enrollment.
func (f *MFAFactor) Confirmed() bool { return f.ConfirmedAt != nil }

// MFAFactorStatus is the public, secret-free state of one MFA factor.
type MFAFactorStatus struct {
	ID          uuid.UUID
	Type        string
	Status      string
	ConfirmedAt *time.Time
	CreatedAt   time.Time
}

// MFA factor lifecycle values exposed by the status resource.
const (
	MFAFactorPending = "pending"
	MFAFactorActive  = "active"
)

// MFAStatus is the public state of a user's MFA configuration.
type MFAStatus struct {
	Enabled                bool
	ConfirmedAt            *time.Time
	RecoveryCodesRemaining int64
}

// MFAEnrollment is a pending TOTP enrollment. The otpauth URI is the only time
// the secret is exposed.
type MFAEnrollment struct {
	FactorID   uuid.UUID
	OtpauthURI string
}

// MFAConfirmResult is the one-time result of confirming a factor: the plaintext
// recovery codes and, when the caller confirmed from a session, the re-issued
// token pair for that session. Confirmation bumps the user's token version, so
// the caller's previous access token stops verifying; the new pair continues
// the same session row with the second factor in its amr.
type MFAConfirmResult struct {
	RecoveryCodes []string
	Tokens        TokenPair
}

// ReauthProof is the caller-supplied evidence for a sensitive account change:
// the interactive session that authorized it and, for password accounts, the
// current password. Passwordless accounts are checked by session age instead.
type ReauthProof struct {
	SessionID       uuid.UUID
	CurrentPassword string
}

// MFACodeChange identifies the TOTP factor and the proofs required to change
// it: re-authentication plus a current TOTP or recovery code.
type MFACodeChange struct {
	FactorID        uuid.UUID
	SessionID       uuid.UUID
	CurrentPassword string
	Code            string
}

// MFARecoveryChange is the proof for replacing recovery codes.
type MFARecoveryChange struct {
	SessionID       uuid.UUID
	CurrentPassword string
	Code            string
}

// MFALogin is a completed two-step sign-in.
type MFALogin struct {
	Tokens TokenPair
	Member *Member
}

// MFARepository stores per-user MFA factors and recovery codes.
type MFARepository interface {
	// GetTOTP returns the user's TOTP factor (confirmed or pending); the
	// service distinguishes them through ConfirmedAt.
	GetTOTP(ctx context.Context, userID uuid.UUID) (*MFAFactor, error)
	// GetTOTPForUpdate is GetTOTP holding the row until the surrounding
	// transaction ends, so confirmation verifies the secret stored at confirm
	// time.
	GetTOTPForUpdate(ctx context.Context, userID uuid.UUID) (*MFAFactor, error)
	GetConfirmedTOTP(ctx context.Context, userID uuid.UUID) (*MFAFactor, error)
	// UpsertUnconfirmedTOTP replaces a pending enrollment (last writer wins).
	UpsertUnconfirmedTOTP(ctx context.Context, userID uuid.UUID, secretCiphertext string) (*MFAFactor, error)
	// ConfirmTOTP marks a pending factor confirmed and records step as used.
	ConfirmTOTP(ctx context.Context, id, userID uuid.UUID, step int64) (bool, error)
	// ConsumeTOTPStep records step only when it is newer than last_used_step,
	// which makes concurrent uses of one code unable to both succeed.
	ConsumeTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (bool, error)
	DeleteTOTP(ctx context.Context, userID uuid.UUID) error
	// DeletePendingTOTP removes only an unconfirmed enrollment.
	DeletePendingTOTP(ctx context.Context, userID uuid.UUID) error
	DeleteRecoveryCodes(ctx context.Context, userID uuid.UUID) error
	CreateRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes [][]byte) error
	// UseRecoveryCode claims an unused code; the first concurrent caller wins.
	UseRecoveryCode(ctx context.Context, userID uuid.UUID, hash []byte) (bool, error)
	CountUnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) (int64, error)
}

// SecretSealer seals and opens MFA secrets and challenge tokens at rest. The
// composition root binds it to jobs.PayloadCodec (DATA_ENCRYPTION_KEY).
type SecretSealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// Passkey is one WebAuthn credential of a global user. PublicKey is the COSE
// credential public key exactly as the authenticator supplied it; the private
// key never leaves the authenticator. AAGUID is the authenticator model id
// (zero when the authenticator did not report one).
type Passkey struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	CredentialID   []byte
	PublicKey      []byte
	SignCount      int64
	AAGUID         uuid.NullUUID
	Transports     []string
	BackupEligible bool
	BackupState    bool
	Name           string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	UpdatedAt      time.Time
}

// Method returns the RFC 8176 amr value this credential proves: hwk for a
// device-bound key, swk for a synced/backup-eligible passkey.
func (p *Passkey) Method() string {
	if p.BackupEligible {
		return PasskeyMethodSoftware
	}
	return PasskeyMethodHardware
}

// PasskeyRepository stores WebAuthn credentials. Passkeys are per global user,
// so no query takes a tenant.
type PasskeyRepository interface {
	Create(ctx context.Context, passkey Passkey) (*Passkey, error)
	ListByUser(ctx context.Context, userID uuid.UUID) ([]Passkey, error)
	ListByUserLimited(ctx context.Context, userID uuid.UUID, limit int32) ([]Passkey, bool, error)
	GetByCredentialID(ctx context.Context, credentialID []byte) (*Passkey, error)
	Get(ctx context.Context, userID, id uuid.UUID) (*Passkey, error)
	// Delete reports whether a credential of userID was deleted.
	Delete(ctx context.Context, userID, id uuid.UUID) (bool, error)
	DeleteByUser(ctx context.Context, userID uuid.UUID) error
	// UpdateAfterAssertion advances the signature counter and records the last
	// use. It reports false when the counter did not advance (a replay or a
	// cloned authenticator), including a lost concurrent race.
	UpdateAfterAssertion(ctx context.Context, userID, id uuid.UUID, signCount int64, backupState bool) (bool, error)
}

// UserRepository stores global users and their credentials.
type UserRepository interface {
	// GetByID / GetByEmail return a non-deleted user (any other status).
	GetByID(ctx context.Context, id uuid.UUID) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByIdentity(ctx context.Context, provider, subject string) (*User, error)
	// Create returns ErrEmailTaken when a live user has the address.
	Create(ctx context.Context, user NewUser) (*User, error)
	CreatePassword(ctx context.Context, userID uuid.UUID, hash string) error
	// PasswordHash returns apperror.ErrNotFound for a passwordless user.
	PasswordHash(ctx context.Context, userID uuid.UUID) (string, error)
	// ReplacePassword stores hash and records a credential change (token
	// version bump), ending every token of the user.
	ReplacePassword(ctx context.Context, userID uuid.UUID, hash string) (*User, error)
	// RehashPassword upgrades a stored hash after a successful sign-in
	// without bumping the token version or ending any session. The update is
	// conditional on currentHash, the value that verified.
	RehashPassword(ctx context.Context, userID uuid.UUID, currentHash, newHash string) error
	// LinkIdentity returns ErrIdentityTaken or ErrIdentityConflict.
	LinkIdentity(ctx context.Context, identity ExternalIdentity) error
	// UpdateProfile returns ErrEmailTaken when the new address is in use.
	UpdateProfile(ctx context.Context, update ProfileUpdate) (*User, error)
	VerifyEmail(ctx context.Context, id uuid.UUID, email string) (*User, error)
	RevokeTokens(ctx context.Context, id uuid.UUID) (*User, error)
	MarkDeleted(ctx context.Context, id uuid.UUID) (*User, error)
}

// MembershipRepository stores memberships in the request tenant.
type MembershipRepository interface {
	// Get returns the membership in any status; GetActive only an active
	// membership of an active user.
	Get(ctx context.Context, userID uuid.UUID) (*Member, error)
	GetActive(ctx context.Context, userID uuid.UUID) (*Member, error)
	ListActiveCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error)
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error)
	// ListFilteredCursor lists members of the request tenant, optionally
	// filtered by membership status and role key.
	ListFilteredCursor(ctx context.Context, cur pagination.DBCursor, status, role string) ([]Member, error)
	// ListForUser returns the user's memberships in every tenant
	// (deliberately cross-tenant: global account deletion).
	ListForUser(ctx context.Context, userID uuid.UUID) ([]Membership, error)
	// Create returns ErrAlreadyMember when any membership exists.
	Create(ctx context.Context, userID uuid.UUID, role string) (*Membership, error)
	Reactivate(ctx context.Context, userID uuid.UUID, role string) (*Membership, error)
	UpdateRole(ctx context.Context, userID uuid.UUID, role string) (*Membership, error)
	Ban(ctx context.Context, userID uuid.UUID, marker BanMarker) (*Membership, error)
	Unban(ctx context.Context, userID uuid.UUID) (*Membership, error)
	// Remove keeps a ban and its marker; marker backfills one for a banned
	// membership whose row predates the keyed marker (zero otherwise).
	Remove(ctx context.Context, userID uuid.UUID, marker BanMarker) (*Membership, error)
	// SuspendForErasure reserves a non-removed membership for erasure: it
	// becomes suspended, records erasure_requested_at, and revokes its API
	// keys, without setting either ban marker.
	SuspendForErasure(ctx context.Context, userID uuid.UUID) error
	CountActiveOwners(ctx context.Context) (int64, error)
	// LockMembership takes the membership row FOR UPDATE until the
	// transaction ends. Rank checks and role changes use it to serialize a
	// concurrent role change against the target's current highest role.
	LockMembership(ctx context.Context, userID uuid.UUID) error
	// TouchIfUnchanged advances updated_at only while it still matches the
	// value the caller read, returning false when a concurrent writer won.
	TouchIfUnchanged(ctx context.Context, userID uuid.UUID, expected time.Time) (bool, error)
	// LockOwnerChanges takes the per-tenant owner-change lock. It is only
	// meaningful inside a transaction and must run before the target row is
	// updated.
	LockOwnerChanges(ctx context.Context) error
	Stats(ctx context.Context) (MemberStats, error)
	// EmailBanned reports whether any of hashes (the keyed markers computed
	// under every configured key) is retained by a membership of the request
	// tenant.
	EmailBanned(ctx context.Context, hashes [][]byte) (bool, error)
	// BannedMarkerCounts returns the retained ban markers per key id across
	// every tenant (deliberately cross-tenant: the startup warning check).
	BannedMarkerCounts(ctx context.Context) ([]BanMarkerCount, error)
	GetPrincipal(ctx context.Context, userID uuid.UUID) (*Principal, error)
	// LockPrincipal is GetPrincipal holding the membership FOR SHARE until
	// the surrounding transaction ends.
	LockPrincipal(ctx context.Context, userID uuid.UUID) (*Principal, error)
}

// SessionRepository stores sessions in the request tenant.
type SessionRepository interface {
	// Create returns apperror.ErrNotFound when the membership or user is not
	// active or NewSession.TokenVersion is stale.
	Create(ctx context.Context, session NewSession) (*Session, error)
	Get(ctx context.Context, id uuid.UUID) (*Session, error)
	ListLiveCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor) ([]Session, error)
	// Revoke reports whether a live session of userID was revoked.
	Revoke(ctx context.Context, userID, id uuid.UUID, reason RevokeReason) (bool, error)
	RevokeAllInTenant(ctx context.Context, userID uuid.UUID, reason RevokeReason) error
	// RevokeAllForUser revokes the user's sessions in every tenant
	// (deliberately cross-tenant: global credential events).
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason RevokeReason) error
	// RevokeStaleForUser records the revocation of the user's sessions in
	// every tenant whose token version no longer matches the current one
	// (deliberately cross-tenant: the after-commit cleanup of a credential or
	// MFA change, where the version bump already invalidated them). It is
	// idempotent.
	RevokeStaleForUser(ctx context.Context, userID uuid.UUID, reason RevokeReason) error
	// RevokeOtherUserSessions revokes the user's sessions in every tenant
	// except keepID (an MFA change keeps the session that performed it).
	RevokeOtherUserSessions(ctx context.Context, userID, keepID uuid.UUID, reason RevokeReason) error
	// Restamp re-issues one live session of the request tenant under
	// tokenVersion with amr, keeping the same session row. It returns
	// apperror.ErrNotFound when the session is not live in the request tenant.
	Restamp(ctx context.Context, userID, id uuid.UUID, tokenVersion int32, amr []string) (*Session, error)
	// DeleteRefreshTokens removes every refresh token of the session. A
	// re-issued session replaces its family: a token minted before the MFA
	// change must not refresh into the stronger amr.
	DeleteRefreshTokens(ctx context.Context, sessionID uuid.UUID) error
	// RevokeByRefreshToken revokes the session owning a refresh-token hash,
	// by possession (no tenant predicate). revoked is false when the token is
	// unknown or its session was already revoked.
	RevokeByRefreshToken(ctx context.Context, tokenHash []byte, reason RevokeReason) (userID uuid.UUID, revoked bool, err error)
	Touch(ctx context.Context, id uuid.UUID, userAgent, ip string) error
	// MarkMFAVerified records a fresh second-factor proof on the caller's own
	// live session and returns the new timestamp. A revoked or foreign session
	// returns apperror.ErrNotFound.
	MarkMFAVerified(ctx context.Context, userID, sessionID uuid.UUID) (time.Time, error)
	// MarkMFAVerifiedWithMethod records a fresh second-factor proof and adds
	// method to the session's amr set, so a passkey step-up makes later
	// RequireMFA checks accept the session without re-issuing its tokens.
	MarkMFAVerifiedWithMethod(ctx context.Context, userID, sessionID uuid.UUID, method string) (time.Time, error)
	// State loads the validator view of a session of userID in tenantID. The
	// tenant predicate is defense in depth; the validator also compares the
	// returned TenantID with the token's tenant.
	State(ctx context.Context, tenantID, sessionID, userID uuid.UUID) (*SessionState, error)
}

// RefreshRotationState is the one-statement view of a repeated refresh token
// used to decide between the concurrent-refresh grace and reuse revocation.
type RefreshRotationState struct {
	// SessionLive reports that the token's session is not revoked and not
	// expired.
	SessionLive bool
	// UsedWithinGrace reports that the token was consumed after usedAfter.
	UsedWithinGrace bool
	// Rotated reports that a newer token already exists in the same session,
	// so this token was consumed by a rotation (not by logout/revocation).
	Rotated bool
}

// RefreshTokenRepository stores refresh-token hashes.
type RefreshTokenRepository interface {
	Create(ctx context.Context, sessionID uuid.UUID, tokenHash []byte, expiresAt time.Time) error
	// GetByHash is tenant-scoped (RLS plus the caller's explicit tenant
	// comparison): a refresh token is only usable in the tenant that issued
	// it. The caller compares RefreshToken.TenantID with the request tenant.
	GetByHash(ctx context.Context, tokenHash []byte) (*RefreshToken, error)
	// MarkUsed reports whether this call consumed the token.
	MarkUsed(ctx context.Context, tenantID, id uuid.UUID) (bool, error)
	// ClassifyRotation reads the token and its session in one statement and
	// reports whether the token was consumed by a recent rotation of a still
	// live session. It is the race-safe input to the concurrent-refresh grace.
	ClassifyRotation(ctx context.Context, tokenHash []byte, usedAfter time.Time) (RefreshRotationState, error)
}

// PasswordResetRepository stores per-user reset tokens.
type PasswordResetRepository interface {
	// Create replaces the user's token. It reports false when the account no
	// longer has email (changed or deleted since the lookup).
	Create(ctx context.Context, userID uuid.UUID, email string, tokenHash []byte, expiresAt time.Time) (bool, error)
	GetByHash(ctx context.Context, tokenHash []byte) (*PasswordResetToken, error)
	Consume(ctx context.Context, tokenHash []byte) (*PasswordResetToken, error)
	Delete(ctx context.Context, tokenHash []byte) error
	DeleteByUser(ctx context.Context, userID uuid.UUID) error
}

// VerificationRepository stores per-user email verification tokens.
type VerificationRepository interface {
	Create(ctx context.Context, userID uuid.UUID, email string, tokenHash []byte, expiresAt time.Time) error
	Consume(ctx context.Context, tokenHash []byte) (*VerificationToken, error)
	DeleteByUser(ctx context.Context, userID uuid.UUID) error
}

// InvitationRepository stores invitations of the request tenant.
type InvitationRepository interface {
	Upsert(ctx context.Context, invitation NewInvitation) (*Invitation, error)
	Get(ctx context.Context, id uuid.UUID) (*Invitation, error)
	ListPendingCursor(ctx context.Context, cur pagination.DBCursor) ([]Invitation, error)
	DeletePending(ctx context.Context, id uuid.UUID) (bool, error)
	// TenantByTokenHash resolves the inviting tenant by token possession alone
	// (the caller is not a member of that tenant yet). It is the single
	// cross-tenant read of an accept/sign-up.
	TenantByTokenHash(ctx context.Context, tokenHash []byte) (uuid.UUID, error)
	// GetByTokenHashForUpdate is scoped to tenantID and locks the row for the
	// rest of the transaction.
	GetByTokenHashForUpdate(ctx context.Context, tenantID uuid.UUID, tokenHash []byte) (*Invitation, error)
	// MarkAccepted scopes to Invitation.TenantID (passed explicitly because
	// an authenticated invitee's request tenant is another tenant).
	MarkAccepted(ctx context.Context, tenantID, id, userID uuid.UUID) (bool, error)
}

// Role is a tenant or platform role definition.
type Role struct {
	ID          uuid.UUID
	TenantID    uuid.NullUUID
	Scope       string
	Key         string
	Name        string
	Permissions []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// System reports whether the role is a built-in (tenant_id IS NULL) role.
func (r *Role) System() bool { return !r.TenantID.Valid }

// NewRole is a tenant-defined role.
type NewRole struct {
	Key         string
	Name        string
	Permissions []string
}

// RoleUpdate replaces the supplied fields of a tenant role.
type RoleUpdate struct {
	ID          uuid.UUID
	Name        *string
	Permissions *[]string
	// ExpectedUpdatedAt is the ETag validator the caller read. A mismatch is
	// 412 resource.precondition_failed.
	ExpectedUpdatedAt *time.Time
}

// CatalogPermission is one permission the code catalog registers.
type CatalogPermission struct {
	Key      string
	Owner    string
	Platform bool
}

// CatalogRole is one system role and its permission set.
type CatalogRole struct {
	Key         string
	Name        string
	Scope       string
	Permissions []string
}

// RoleRepository stores RBAC roles and their assignments.
type RoleRepository interface {
	// SyncCatalog upserts permissions and system roles and prunes entries the
	// catalog no longer contains. It is safe to run concurrently.
	SyncCatalog(ctx context.Context, permissions []CatalogPermission, roles []CatalogRole) error
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]Role, error)
	// Get returns a system role or a role of the request tenant.
	Get(ctx context.Context, id uuid.UUID) (*Role, error)
	// GetByKey returns a system role or a role of the request tenant.
	GetByKey(ctx context.Context, key string) (*Role, error)
	Create(ctx context.Context, in NewRole) (*Role, error)
	Update(ctx context.Context, id uuid.UUID, name string, permissions []string, expected *time.Time) (*Role, error)
	// Delete removes a tenant role and reports whether it existed. A system
	// role or a role assigned to a membership is an error.
	Delete(ctx context.Context, id uuid.UUID) (bool, error)
	// MemberIDs returns the users holding roleID in the request tenant.
	MemberIDs(ctx context.Context, roleID uuid.UUID) ([]uuid.UUID, error)
	ReplaceMemberRoles(ctx context.Context, userID uuid.UUID, roleKeys []string) error
	// AssignPlatformRole grants a platform role to a user.
	AssignPlatformRole(ctx context.Context, userID uuid.UUID, roleKey string) error
}

// TenantRepository stores tenants.
type TenantRepository interface {
	// GetOperationalBySlug / GetOperationalByID return only tenants that
	// serve requests (trialing or active).
	GetOperationalBySlug(ctx context.Context, slug string) (*Tenant, error)
	GetOperationalByID(ctx context.Context, id uuid.UUID) (*Tenant, error)
	// GetByID / GetBySlug ignore the status (startup existence checks).
	GetByID(ctx context.Context, id uuid.UUID) (*Tenant, error)
	GetBySlug(ctx context.Context, slug string) (*Tenant, error)
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*Tenant, error)
	// ListCursor lists every tenant (platform administration).
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]Tenant, error)
	// Create inserts the tenant and its settings row.
	Create(ctx context.Context, tenant NewTenant) (*Tenant, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status TenantStatus, deletionScheduledAt *time.Time) (*Tenant, error)
	UpdateName(ctx context.Context, id uuid.UUID, name string) (*Tenant, error)
	// TouchIfUnchanged advances updated_at only while it still matches the
	// value the caller read, returning false when a concurrent writer won.
	TouchIfUnchanged(ctx context.Context, id uuid.UUID, expected time.Time) (bool, error)
	Settings(ctx context.Context, tenantID uuid.UUID) (*TenantSettings, error)
}

// TokenPurgeRepository deletes dead identity rows across all tenants (a
// maintenance job). Each call deletes at most batchSize rows.
type TokenPurgeRepository interface {
	PurgeExpiredRefreshTokens(ctx context.Context, batchSize int32) (int64, error)
	PurgeExpiredPasswordResetTokens(ctx context.Context, batchSize int32) (int64, error)
	PurgeExpiredEmailVerificationTokens(ctx context.Context, batchSize int32) (int64, error)
	PurgeExpiredInvitations(ctx context.Context, batchSize int32) (int64, error)
	PurgeStaleSessions(ctx context.Context, revokedBefore, idleBefore time.Time, batchSize int32) (int64, error)
}

// DataExportStatus is the lifecycle of a data-export request.
type DataExportStatus string

const (
	DataExportQueued  DataExportStatus = "queued"
	DataExportRunning DataExportStatus = "running"
	DataExportReady   DataExportStatus = "ready"
	DataExportFailed  DataExportStatus = "failed"
	DataExportExpired DataExportStatus = "expired"
)

// DataExport is one tenant/user export request and its stored document.
type DataExport struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	UserID      uuid.UUID
	Status      DataExportStatus
	StoragePath string
	ExpiresAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErasureScope says who requested an erasure.
type ErasureScope string

const (
	ErasureScopeSelf   ErasureScope = "self"
	ErasureScopeMember ErasureScope = "member"
)

// ErasureStatus is the lifecycle of an erasure request.
type ErasureStatus string

const (
	ErasureQueued     ErasureStatus = "queued"
	ErasureProcessing ErasureStatus = "processing"
	ErasureCompleted  ErasureStatus = "completed"
)

// ErasureRequest is the status resource for one erasure request. The request
// id is the target user id: identity keeps at most one self-erasure request
// per account, and the audit trail is the durable record.
type ErasureRequest struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	UserID      uuid.UUID
	Scope       ErasureScope
	Status      ErasureStatus
	RequestedAt time.Time
	CompletedAt *time.Time
}

// ErrDataExportInProgress means the membership already has a queued or running
// export (idx_data_exports_active).
var ErrDataExportInProgress = errors.New("identity: a data export is already in progress")

// DataExportRepository stores export requests, the export purge, and the
// cross-table reads/writes the identity eraser and finalizer need. Every
// method except the purge scan and the finalizer's all-tenants invitation
// sweep is tenant/user scoped.
type DataExportRepository interface {
	Create(ctx context.Context, userID uuid.UUID) (*DataExport, error)
	Get(ctx context.Context, userID, id uuid.UUID) (*DataExport, error)
	ListCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor) ([]DataExport, error)
	MarkRunning(ctx context.Context, userID, id uuid.UUID) (bool, error)
	MarkReady(ctx context.Context, userID, id uuid.UUID, storagePath string, expiresAt time.Time) (bool, error)
	MarkFailed(ctx context.Context, userID, id uuid.UUID) error
	ExpireForUser(ctx context.Context, userID uuid.UUID) error
	DeleteForUser(ctx context.Context, userID uuid.UUID) error
	Purgeable(ctx context.Context, limit int32) ([]DataExport, error)
	Delete(ctx context.Context, id uuid.UUID) error
	ListSessions(ctx context.Context, userID uuid.UUID, limit int32) ([]Session, bool, error)
	AnonymizeUser(ctx context.Context, userID uuid.UUID, email string) (*User, error)
	// AnonymizeErasedUserInvitations replaces the address on invitations the
	// user accepted or that were addressed to them, in the request tenant.
	AnonymizeErasedUserInvitations(ctx context.Context, userID uuid.UUID, replacement string) (int64, error)
	// AnonymizeErasedUserInvitationsAllTenants is the global finalizer's sweep:
	// it covers every tenant under the RLS bypass.
	AnonymizeErasedUserInvitationsAllTenants(ctx context.Context, userID uuid.UUID, replacement string) (int64, error)
	DeleteCredentialsAndIdentities(ctx context.Context, userID uuid.UUID) error
	DeleteSessionsInTenant(ctx context.Context, userID uuid.UUID) error
	DeleteSessionsForUser(ctx context.Context, userID uuid.UUID) error
}

// JobEnqueuer enqueues a job on the caller's transaction. The repository
// transactor binds it to the open pgx.Tx, so services never see pgx.
type JobEnqueuer interface {
	Enqueue(ctx context.Context, job jobs.Job) (uuid.UUID, error)
}

// Stores are the identity repositories bound to one database handle (the
// pool, or one transaction inside Transactor.InTx).
type Stores struct {
	Users         UserRepository
	Memberships   MembershipRepository
	Sessions      SessionRepository
	RefreshTokens RefreshTokenRepository
	Resets        PasswordResetRepository
	Verifications VerificationRepository
	Invitations   InvitationRepository
	Tenants       TenantRepository
	MFA           MFARepository
	Passkeys      PasskeyRepository
	Privacy       DataExportRepository
	Roles         RoleRepository
	// Bans computes the keyed ban marker. Stores that never touch a ban path
	// may leave it nil; those paths fail loudly instead of hashing unkeyed.
	Bans BanHasher
}

// AuditWriter records a Tier 1 audit event in the current transaction.
type AuditWriter interface {
	RecordTx(ctx context.Context, event audit.Event) error
}

// EventPublisher publishes a domain event in the current transaction
// (events.Bus.PublishTx): the event's durable subscriber jobs (webhook
// fan-out) commit or roll back with the mutation.
type EventPublisher interface {
	Publish(ctx context.Context, event events.Event) error
}

// Mailer enqueues an email in the current transaction (the email.send.encrypted job).
type Mailer interface {
	Send(ctx context.Context, msg email.Message) error
}

// TxRepos are the collaborators bound to one transaction.
type TxRepos struct {
	Stores
	Audit AuditWriter
	// Events is never nil; without a wired event bus it publishes nothing.
	Events EventPublisher
	// Mail is nil only in minimal test fixtures; production wiring always
	// provides the transactional outbox and services fail closed without it.
	Mail Mailer
	// Jobs enqueues a durable job on the caller's transaction.
	Jobs JobEnqueuer
	// AfterCommit registers a callback that runs only after the transaction
	// commits (never on rollback). It is nil outside a production transactor;
	// callers must treat a nil hook as "no post-commit channel".
	AfterCommit func(func())
}

// Transactor runs fn in one transaction: when fn returns an error every write
// made through tx rolls back, otherwise all of them commit together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error
}

// TokenPair is one issued access + refresh token for a session.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	SessionID    uuid.UUID
}
