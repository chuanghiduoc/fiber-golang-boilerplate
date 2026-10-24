// Package identity holds the stdlib-only contracts the identity module
// provides to other modules: the admin back-office surface, API-key
// principals, and notification recipient lookup. Implementations live in
// internal/modules/identity/adapter.
package identity

import (
	"context"
	"time"
)

// User is the identity module's cross-module view of an account. It
// deliberately omits credentials, token versions, and other internal fields.
type User struct {
	ID            int64
	Email         string
	Name          string
	Role          string
	EmailVerified bool
	HasPassword   bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
	BannedAt      *time.Time
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
// behalf of an API key's owner. It intentionally excludes credentials and
// session fields.
type Principal struct {
	ID       int64
	TenantID int64
	Email    string
	Name     string
	Role     string
	// TenantStatus is the owning tenant's current status ("active" or
	// "suspended"). API-key authentication rejects anything but
	// TenantStatusActive.
	TenantStatus string
	// CredentialsRevokedAt is set when the account was banned and survives an
	// unban. API keys created at or before it must be rejected, so a restored
	// account never revives credentials issued before the ban. Nil means no
	// revocation.
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
