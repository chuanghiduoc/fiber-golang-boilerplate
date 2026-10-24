package contracts

import (
	"context"
	"encoding/json"
	"time"
)

// Notification is the notifications module's cross-module view of a delivered
// in-app notification. Email delivery is a side effect of creation and has no
// contract representation.
type Notification struct {
	ID        int64
	TenantID  int64
	UserID    int64
	Type      string
	Title     string
	Body      string
	Data      json.RawMessage
	ReadAt    *time.Time
	CreatedAt time.Time
}

// CreateNotificationInput describes a notification a module wants to deliver.
// TenantID is optional inside a request: when zero the context tenant is used.
// A non-zero value that conflicts with the context tenant is rejected.
//
// Title and Body are user-visible. Data is optional JSON metadata (an object)
// stored with the in-app row and never included in the email body.
type CreateNotificationInput struct {
	TenantID int64
	UserID   int64
	Type     string
	Title    string
	Body     string
	Data     json.RawMessage
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

// Notifier is the service surface other modules use to create notifications.
// The owning module decides channel preferences and enqueues email jobs; a
// suppressed notification (all channels disabled) is not an error.
type Notifier interface {
	Notify(ctx context.Context, input CreateNotificationInput) error
}
