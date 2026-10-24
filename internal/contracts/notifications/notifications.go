// Package notifications holds the stdlib-only contracts the notifications
// module provides to other modules. The recipient lookup it consumes is an
// identity contract (internal/contracts/identity).
package notifications

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

// Notifier is the service surface other modules use to create notifications.
// The owning module decides channel preferences and enqueues email jobs; a
// suppressed notification (all channels disabled) is not an error.
type Notifier interface {
	Notify(ctx context.Context, input CreateNotificationInput) error
}
