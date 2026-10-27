package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// Notification is one in-app notification row.
type Notification struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	UserID    uuid.UUID
	Type      string
	Title     string
	Body      string
	Data      map[string]any
	ReadAt    *time.Time
	CreatedAt time.Time
}

// Preference is one explicit channel preference row. It has no surrogate id:
// the natural key is (tenant, user, type, channel).
type Preference struct {
	TenantID  uuid.UUID
	UserID    uuid.UUID
	Type      string
	Channel   string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewNotification is the input for creating an in-app row. The tenant comes
// from the request context.
type NewNotification struct {
	UserID uuid.UUID
	Type   string
	Title  string
	Body   string
	Data   []byte
}

// NewPreference is a validated channel preference write.
type NewPreference struct {
	UserID  uuid.UUID
	Type    string
	Channel string
	Enabled bool
}

// PreferenceUpdate is the service input for one channel preference. Enabled
// is a pointer so an omitted field is rejected instead of disabling a channel.
type PreferenceUpdate struct {
	Type    string
	Channel string
	Enabled *bool
}

// CreateResult reports which channels accepted a notification. Notification
// is nil when the in-app channel is disabled.
type CreateResult struct {
	Notification *Notification
	EmailQueued  bool
}

// NotificationRepository is the persistence port. Implementations scope every
// query to the request tenant and, for user-facing reads and writes, also
// filter by user ID; no row match is apperror.ErrNotFound.
type NotificationRepository interface {
	Create(ctx context.Context, in NewNotification) (*Notification, error)
	GetByID(ctx context.Context, id, userID uuid.UUID) (*Notification, error)
	ListCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor, unreadOnly bool) ([]Notification, error)
	CountUnread(ctx context.Context, userID uuid.UUID) (int64, error)
	MarkRead(ctx context.Context, id, userID uuid.UUID) (*Notification, error)
	MarkUnread(ctx context.Context, id, userID uuid.UUID) (*Notification, error)
	MarkAllRead(ctx context.Context, userID uuid.UUID) (int64, error)
	GetPreference(ctx context.Context, userID uuid.UUID, notificationType, channel string) (*Preference, error)
	ListPreferences(ctx context.Context, userID uuid.UUID) ([]Preference, error)
	UpsertPreference(ctx context.Context, in NewPreference) (*Preference, error)
}

// Mailer sends an email. Bound to a transaction it enqueues the email.send.encrypted job
// so the job commits with the caller's writes.
type Mailer interface {
	Send(ctx context.Context, msg email.Message) error
}

// TxRepos are the collaborators bound to one transaction.
type TxRepos struct {
	Notifications NotificationRepository
	Mail          Mailer
}

// Transactor runs fn in one transaction: when fn returns an error every write
// made through tx rolls back, otherwise all of them commit together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error
}

// RetentionRepository is the retention persistence port. It is not
// tenant-scoped: retention is a cross-tenant maintenance pass.
type RetentionRepository interface {
	// PurgeRead deletes at most limit notifications read more than
	// retentionDays ago and returns how many it deleted.
	PurgeRead(ctx context.Context, retentionDays, limit int32) (int64, error)
}
