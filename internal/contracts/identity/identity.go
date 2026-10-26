// Package identity holds the stdlib-only contracts the identity module
// provides to other modules: the admin back-office surface, API-key
// principals, notification recipient lookup, and the member role hierarchy.
// Implementations live in internal/modules/identity/adapter.
package identity

import (
	"context"
	"time"
)

// User is the identity module's cross-module view of a global user as a
// member of the context tenant. Role, MembershipStatus, JoinedAt,
// BannedAt, and DeletedAt (membership removal or account deletion) describe
// the membership; the rest is the global identity. It deliberately omits
// credentials, token versions, and other internal fields.
type User struct {
	ID    int64
	Email string
	Name  string
	Role  string
	// Roles lists every role assigned in the tenant.
	Roles []string
	// MembershipStatus is "active", "suspended" (banned), or "removed".
	MembershipStatus string
	EmailVerified    bool
	HasPassword      bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// JoinedAt orders member lists (their cursor is (JoinedAt, ID)).
	JoinedAt  time.Time
	DeletedAt *time.Time
	BannedAt  *time.Time
}

// UserPage is one cursor page of users plus the next-page indicator.
type UserPage struct {
	Items   []User
	HasMore bool
}

// UserStats is the identity-owned half of the admin dashboard.
type UserStats struct {
	ActiveUsers  int64
	DeletedUsers int64
}

// Principal is the identity-owned view needed to authenticate a request on
// behalf of an API key's owner: an active membership with its effective
// permissions. It intentionally excludes credentials and session fields.
type Principal struct {
	ID       int64
	TenantID int64
	Email    string
	Name     string
	Role     string
	// Permissions is the owner's effective permission set in TenantID.
	Permissions []string
	// Platform is true when the owner holds a platform role assignment.
	Platform bool
	// TenantStatus is TenantStatusActive for every tenant state that serves
	// requests (trialing or active) and the raw lifecycle state otherwise.
	// API-key authentication rejects anything but TenantStatusActive.
	TenantStatus string
	// CredentialsRevokedAt is set when the membership was banned or removed
	// and survives an unban/re-join. API keys created at or before it must be
	// rejected, so a restored member never revives credentials issued before
	// the ban. Nil means no revocation.
	CredentialsRevokedAt *time.Time
}

// TenantStatusActive is the only tenant status that may authenticate.
const TenantStatusActive = "active"

// PrincipalReader loads the current principal for a tenant/user pair. It is
// used by API-key authentication so bans, deletions, and role changes take
// effect immediately. Implementations must return apperror.ErrNotFound (or a
// wrapped error) when the account is not active.
type PrincipalReader interface {
	GetPrincipal(ctx context.Context, tenantID, userID int64) (Principal, error)
}

// MemberRankChecker applies identity's tenant role hierarchy to a caller acting
// on another member. It returns a permission/rank error when the caller does
// not outrank targetUserID in the request tenant, and apperror.ErrNotFound when
// the target is not an active member of that tenant. A caller without a
// subject is allowed only under authz.SystemContext.
type MemberRankChecker interface {
	CanManageMember(ctx context.Context, targetUserID int64) error
}

// IdentityAdmin is the back-office surface identity exposes to admin.
type IdentityAdmin interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) (UserPage, error)
	UpdateRole(ctx context.Context, id int64, role string) (User, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (User, error)
	GetUserStats(ctx context.Context) (UserStats, error)
}

// NotificationRecipient is the delivery address resolved for a user. The
// notifications module never reads the users table directly; identity owns
// that data and implements NotificationRecipientLookup.
type NotificationRecipient struct {
	UserID int64
	Email  string
	Name   string
}

// NotificationRecipientLookup resolves the email address used by the email
// channel. Implementations must scope the lookup to the context tenant and
// return apperror.ErrNotFound for a deleted or cross-tenant user.
type NotificationRecipientLookup interface {
	NotificationRecipient(ctx context.Context, userID int64) (NotificationRecipient, error)
}
