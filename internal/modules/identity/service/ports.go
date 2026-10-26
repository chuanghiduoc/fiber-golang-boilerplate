package service

import (
	"context"
	"errors"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// Persistence ports of the identity module. The repository package
// implements them over sqlc and maps rows to these types; services never see
// generated SQL types or pgx.
//
// Tenant scoping: methods of tenant-owned stores (memberships, sessions,
// refresh tokens, invitations) take the tenant from the context
// (tenant.EffectiveID). Users, passwords, external identities, and reset /
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

// NormalizeRole accepts the pre-RBAC "user" key as an alias for member.
func NormalizeRole(role string) string {
	if role == "user" {
		return RoleMember
	}
	return role
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
)

// MFA factor types (mfa_factors.type).
const MFAFactorTOTP = "totp"

// External identity providers (user_identities.provider).
const ProviderGoogle = "google"

// User is a global identity.
type User struct {
	ID                   int64
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
	ID       int64
	TenantID int64
	UserID   int64
	// Role is the highest system role key (owner > admin > member).
	Role string
	// Roles holds every assigned role key.
	Roles                []string
	Status               MembershipStatus
	BannedAt             *time.Time
	RemovedAt            *time.Time
	CredentialsRevokedAt *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
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
	ID          int64
	Name        string
	Email       string
	UpdateEmail bool
}

// ExternalIdentity links a user to a provider subject.
type ExternalIdentity struct {
	UserID   int64
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
	UserID       int64
	TenantID     int64
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
	ID            int64
	TenantID      int64
	UserID        int64
	TokenVersion  int32
	UserAgent     string
	IP            string
	AMR           []string
	LastUsedAt    time.Time
	CreatedAt     time.Time
	RevokedAt     *time.Time
	RevokedReason RevokeReason
}

// NewSession is the input for opening a session in the request tenant.
type NewSession struct {
	UserID int64
	// TokenVersion is the user's version the caller authenticated at; the
	// insert fails (apperror.ErrNotFound) when it is stale.
	TokenVersion int32
	UserAgent    string
	IP           string
	// AMR is the authentication-method reference set (pwd, google, otp, rc).
	AMR []string
}

// SessionState is everything the access-token validator compares.
type SessionState struct {
	TenantID         int64
	Revoked          bool
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
	ID        int64
	TenantID  int64
	SessionID int64
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// PasswordResetToken is a pending password reset (per user).
type PasswordResetToken struct {
	ID        int64
	UserID    int64
	ExpiresAt time.Time
	CreatedAt time.Time
}

// VerificationToken is a pending email verification bound to an address.
type VerificationToken struct {
	ID        int64
	UserID    int64
	Email     string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Invitation is a pending (or accepted) invitation into a tenant.
type Invitation struct {
	ID         int64
	TenantID   int64
	Email      string
	Role       string
	InvitedBy  int64
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	AcceptedBy *int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewInvitation is the input for creating (or refreshing) a pending
// invitation in the request tenant.
type NewInvitation struct {
	Email     string // normalized
	Role      string
	TokenHash string
	InvitedBy int64
	ExpiresAt time.Time
}

// Tenant is one tenant and its lifecycle state.
type Tenant struct {
	ID                  int64
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
	OwnerUserID int64
}

// TenantSettings are a tenant's typed settings.
type TenantSettings struct {
	TenantID int64
	Locale   string
	Timezone string
}

// MFAFactor is one MFA factor of a global user (mfa_factors). SecretCiphertext
// is sealed with the data encryption keyring; only the service opens it.
type MFAFactor struct {
	ID               int64
	UserID           int64
	Type             string
	SecretCiphertext string
	ConfirmedAt      *time.Time
	LastUsedStep     int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Confirmed reports whether the factor completed enrollment.
func (f *MFAFactor) Confirmed() bool { return f.ConfirmedAt != nil }

// MFAStatus is the public state of a user's MFA configuration.
type MFAStatus struct {
	Enabled                bool
	ConfirmedAt            *time.Time
	RecoveryCodesRemaining int64
}

// MFAEnrollment is a pending TOTP enrollment. The otpauth URI is the only time
// the secret is exposed.
type MFAEnrollment struct {
	OtpauthURI string
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
	GetTOTP(ctx context.Context, userID int64) (*MFAFactor, error)
	GetConfirmedTOTP(ctx context.Context, userID int64) (*MFAFactor, error)
	// UpsertUnconfirmedTOTP replaces a pending enrollment.
	UpsertUnconfirmedTOTP(ctx context.Context, userID int64, secretCiphertext string) (*MFAFactor, error)
	// ConfirmTOTP marks a pending factor confirmed and records step as used.
	ConfirmTOTP(ctx context.Context, id, userID, step int64) (bool, error)
	// ConsumeTOTPStep records step only when it is newer than last_used_step,
	// which makes concurrent uses of one code unable to both succeed.
	ConsumeTOTPStep(ctx context.Context, userID, step int64) (bool, error)
	DeleteTOTP(ctx context.Context, userID int64) error
	DeleteRecoveryCodes(ctx context.Context, userID int64) error
	CreateRecoveryCodes(ctx context.Context, userID int64, hashes []string) error
	// UseRecoveryCode claims an unused code; the first concurrent caller wins.
	UseRecoveryCode(ctx context.Context, userID int64, hash string) (bool, error)
	CountUnusedRecoveryCodes(ctx context.Context, userID int64) (int64, error)
}

// SecretSealer seals and opens MFA secrets and challenge tokens at rest. The
// composition root binds it to jobs.PayloadCodec (DATA_ENCRYPTION_KEY).
type SecretSealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// UserRepository stores global users and their credentials.
type UserRepository interface {
	// GetByID / GetByEmail return a non-deleted user (any other status).
	GetByID(ctx context.Context, id int64) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	GetByIdentity(ctx context.Context, provider, subject string) (*User, error)
	// Create returns ErrEmailTaken when a live user has the address.
	Create(ctx context.Context, user NewUser) (*User, error)
	CreatePassword(ctx context.Context, userID int64, hash string) error
	// PasswordHash returns apperror.ErrNotFound for a passwordless user.
	PasswordHash(ctx context.Context, userID int64) (string, error)
	// ReplacePassword stores hash and records a credential change (token
	// version bump), ending every token of the user.
	ReplacePassword(ctx context.Context, userID int64, hash string) (*User, error)
	// LinkIdentity returns ErrIdentityTaken or ErrIdentityConflict.
	LinkIdentity(ctx context.Context, identity ExternalIdentity) error
	// UpdateProfile returns ErrEmailTaken when the new address is in use.
	UpdateProfile(ctx context.Context, update ProfileUpdate) (*User, error)
	VerifyEmail(ctx context.Context, id int64, email string) (*User, error)
	RevokeTokens(ctx context.Context, id int64) (*User, error)
	MarkDeleted(ctx context.Context, id int64) (*User, error)
}

// MembershipRepository stores memberships in the request tenant.
type MembershipRepository interface {
	// Get returns the membership in any status; GetActive only an active
	// membership of an active user.
	Get(ctx context.Context, userID int64) (*Member, error)
	GetActive(ctx context.Context, userID int64) (*Member, error)
	ListActiveCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error)
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]Member, error)
	// ListForUser returns the user's memberships in every tenant
	// (deliberately cross-tenant: global account deletion).
	ListForUser(ctx context.Context, userID int64) ([]Membership, error)
	// Create returns ErrAlreadyMember when any membership exists.
	Create(ctx context.Context, userID int64, role string) (*Membership, error)
	Reactivate(ctx context.Context, userID int64, role string) (*Membership, error)
	UpdateRole(ctx context.Context, userID int64, role string) (*Membership, error)
	Ban(ctx context.Context, userID int64) (*Membership, error)
	Unban(ctx context.Context, userID int64) (*Membership, error)
	Remove(ctx context.Context, userID int64) (*Membership, error)
	CountActiveOwners(ctx context.Context) (int64, error)
	// LockOwnerChanges takes the per-tenant owner-change lock. It is only
	// meaningful inside a transaction and must run before the target row is
	// updated.
	LockOwnerChanges(ctx context.Context) error
	Stats(ctx context.Context) (MemberStats, error)
	// EmailBanned reports whether an account with email (live or deleted) is
	// banned in the request tenant.
	EmailBanned(ctx context.Context, email string) (bool, error)
	GetPrincipal(ctx context.Context, userID int64) (*Principal, error)
	// LockPrincipal is GetPrincipal holding the membership FOR SHARE until
	// the surrounding transaction ends.
	LockPrincipal(ctx context.Context, userID int64) (*Principal, error)
}

// SessionRepository stores sessions in the request tenant.
type SessionRepository interface {
	// Create returns apperror.ErrNotFound when the membership or user is not
	// active or NewSession.TokenVersion is stale.
	Create(ctx context.Context, session NewSession) (*Session, error)
	Get(ctx context.Context, id int64) (*Session, error)
	ListLiveCursor(ctx context.Context, userID int64, cur pagination.DBCursor) ([]Session, error)
	// Revoke reports whether a live session of userID was revoked.
	Revoke(ctx context.Context, userID, id int64, reason RevokeReason) (bool, error)
	RevokeAllInTenant(ctx context.Context, userID int64, reason RevokeReason) error
	// RevokeAllForUser revokes the user's sessions in every tenant
	// (deliberately cross-tenant: global credential events).
	RevokeAllForUser(ctx context.Context, userID int64, reason RevokeReason) error
	// RevokeOtherUserSessions revokes the user's sessions in every tenant
	// except keepID (an MFA change keeps the session that performed it).
	RevokeOtherUserSessions(ctx context.Context, userID, keepID int64, reason RevokeReason) error
	// RevokeByRefreshToken revokes the session owning a refresh-token hash,
	// by possession (no tenant predicate). revoked is false when the token is
	// unknown or its session was already revoked.
	RevokeByRefreshToken(ctx context.Context, tokenHash string, reason RevokeReason) (userID int64, revoked bool, err error)
	Touch(ctx context.Context, id int64, userAgent, ip string) error
	// State loads the validator view of a session of userID (any tenant; the
	// caller compares SessionState.TenantID with the token's tenant).
	State(ctx context.Context, sessionID, userID int64) (*SessionState, error)
}

// RefreshTokenRepository stores refresh-token hashes.
type RefreshTokenRepository interface {
	Create(ctx context.Context, sessionID int64, tokenHash string, expiresAt time.Time) error
	// GetByHash is possession-based (no tenant predicate); the caller compares
	// RefreshToken.TenantID with the request tenant.
	GetByHash(ctx context.Context, tokenHash string) (*RefreshToken, error)
	// MarkUsed reports whether this call consumed the token.
	MarkUsed(ctx context.Context, tenantID, id int64) (bool, error)
}

// PasswordResetRepository stores per-user reset tokens.
type PasswordResetRepository interface {
	// Create replaces the user's token. It reports false when the account no
	// longer has email (changed or deleted since the lookup).
	Create(ctx context.Context, userID int64, email, tokenHash string, expiresAt time.Time) (bool, error)
	GetByHash(ctx context.Context, tokenHash string) (*PasswordResetToken, error)
	Consume(ctx context.Context, tokenHash string) (*PasswordResetToken, error)
	Delete(ctx context.Context, tokenHash string) error
	DeleteByUser(ctx context.Context, userID int64) error
}

// VerificationRepository stores per-user email verification tokens.
type VerificationRepository interface {
	Create(ctx context.Context, userID int64, email, tokenHash string, expiresAt time.Time) error
	Consume(ctx context.Context, tokenHash string) (*VerificationToken, error)
	DeleteByUser(ctx context.Context, userID int64) error
}

// InvitationRepository stores invitations of the request tenant.
type InvitationRepository interface {
	Upsert(ctx context.Context, invitation NewInvitation) (*Invitation, error)
	ListPendingCursor(ctx context.Context, cur pagination.DBCursor) ([]Invitation, error)
	DeletePending(ctx context.Context, id int64) (bool, error)
	// GetByTokenHashForUpdate is possession-based (no tenant predicate) and
	// locks the row for the rest of the transaction.
	GetByTokenHashForUpdate(ctx context.Context, tokenHash string) (*Invitation, error)
	// MarkAccepted scopes to Invitation.TenantID (passed explicitly because
	// an authenticated invitee's request tenant is another tenant).
	MarkAccepted(ctx context.Context, tenantID, id, userID int64) (bool, error)
}

// Role is a tenant or platform role definition.
type Role struct {
	ID          int64
	TenantID    *int64
	Scope       string
	Key         string
	Name        string
	Permissions []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// System reports whether the role is a built-in (tenant_id IS NULL) role.
func (r *Role) System() bool { return r.TenantID == nil }

// NewRole is a tenant-defined role.
type NewRole struct {
	Key         string
	Name        string
	Permissions []string
}

// RoleUpdate replaces the supplied fields of a tenant role.
type RoleUpdate struct {
	ID          int64
	Name        *string
	Permissions *[]string
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
	Get(ctx context.Context, id int64) (*Role, error)
	// GetByKey returns a system role or a role of the request tenant.
	GetByKey(ctx context.Context, key string) (*Role, error)
	Create(ctx context.Context, in NewRole) (*Role, error)
	Update(ctx context.Context, id int64, name string, permissions []string) (*Role, error)
	// Delete removes a tenant role and reports whether it existed. A system
	// role or a role assigned to a membership is an error.
	Delete(ctx context.Context, id int64) (bool, error)
	// MemberIDs returns the users holding roleID in the request tenant.
	MemberIDs(ctx context.Context, roleID int64) ([]int64, error)
	ReplaceMemberRoles(ctx context.Context, userID int64, roleKeys []string) error
	// AssignPlatformRole grants a platform role to a user.
	AssignPlatformRole(ctx context.Context, userID int64, roleKey string) error
}

// TenantRepository stores tenants.
type TenantRepository interface {
	// GetOperationalBySlug / GetOperationalByID return only tenants that
	// serve requests (trialing or active).
	GetOperationalBySlug(ctx context.Context, slug string) (*Tenant, error)
	GetOperationalByID(ctx context.Context, id int64) (*Tenant, error)
	// GetByID / GetBySlug ignore the status (startup existence checks).
	GetByID(ctx context.Context, id int64) (*Tenant, error)
	GetBySlug(ctx context.Context, slug string) (*Tenant, error)
	GetByIDForUpdate(ctx context.Context, id int64) (*Tenant, error)
	// ListCursor lists every tenant (platform administration).
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]Tenant, error)
	// Create inserts the tenant and its settings row.
	Create(ctx context.Context, tenant NewTenant) (*Tenant, error)
	UpdateStatus(ctx context.Context, id int64, status TenantStatus, deletionScheduledAt *time.Time) (*Tenant, error)
	Settings(ctx context.Context, tenantID int64) (*TenantSettings, error)
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
	ID          int64
	TenantID    int64
	UserID      int64
	Status      DataExportStatus
	StoragePath string
	ExpiresAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErrDataExportInProgress means the membership already has a queued or running
// export (idx_data_exports_active).
var ErrDataExportInProgress = errors.New("identity: a data export is already in progress")

// DataExportRepository stores export requests, the export purge, and the
// cross-table reads/writes the identity eraser and finalizer need. Every
// method except ListPurgeableDataExports is tenant/user scoped.
type DataExportRepository interface {
	Create(ctx context.Context, userID int64) (*DataExport, error)
	Get(ctx context.Context, userID, id int64) (*DataExport, error)
	ListForUser(ctx context.Context, userID int64) ([]DataExport, error)
	ListCursor(ctx context.Context, userID int64, cur pagination.DBCursor) ([]DataExport, error)
	MarkRunning(ctx context.Context, userID, id int64) (bool, error)
	MarkReady(ctx context.Context, userID, id int64, storagePath string, expiresAt time.Time) (bool, error)
	MarkFailed(ctx context.Context, userID, id int64) error
	ExpireForUser(ctx context.Context, userID int64) ([]DataExport, error)
	DeleteForUser(ctx context.Context, userID int64) error
	Purgeable(ctx context.Context, limit int32) ([]DataExport, error)
	Delete(ctx context.Context, id int64) error
	ListSessions(ctx context.Context, userID int64) ([]Session, error)
	AnonymizeUser(ctx context.Context, userID int64, email string) (*User, error)
	DeleteCredentialsAndIdentities(ctx context.Context, userID int64) error
	DeleteSessionsInTenant(ctx context.Context, userID int64) error
	DeleteSessionsForUser(ctx context.Context, userID int64) error
}

// JobEnqueuer enqueues a job on the caller's transaction. The repository
// transactor binds it to the open pgx.Tx, so services never see pgx.
type JobEnqueuer interface {
	Enqueue(ctx context.Context, job jobs.Job) (int64, error)
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
	Privacy       DataExportRepository
	Roles         RoleRepository
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

// Mailer enqueues an email in the current transaction (the email.send job).
type Mailer interface {
	Send(ctx context.Context, msg email.Message) error
}

// TxRepos are the collaborators bound to one transaction.
type TxRepos struct {
	Stores
	Audit AuditWriter
	// Events is never nil; without a wired event bus it publishes nothing.
	Events EventPublisher
	// Mail is nil when the wired email sender cannot enqueue on the
	// transaction; callers then send after commit.
	Mail Mailer
	// Jobs is never nil in production; privacy requests enqueue their durable
	// job in the same transaction as the row.
	Jobs JobEnqueuer
}

// Transactor runs fn in one transaction: when fn returns an error every write
// made through tx rolls back, otherwise all of them commit together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error
}
