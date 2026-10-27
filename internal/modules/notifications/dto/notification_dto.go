// Package dto contains the notifications module's HTTP request and response
// bodies. The route contract lives in docs/api-routes.md.
package dto

import (
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

// NotificationResponse is one in-app notification owned by the authenticated
// member in the request tenant.
type NotificationResponse struct {
	ID        uuid.UUID      `json:"id" format:"uuid"`
	TenantID  uuid.UUID      `json:"tenantId" format:"uuid"`
	UserID    uuid.UUID      `json:"userId" format:"uuid"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Data      map[string]any `json:"data"`
	ReadAt    *time.Time     `json:"readAt,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// UnreadCountResponse is the unread badge count for the authenticated member.
type UnreadCountResponse struct {
	Count int64 `json:"count"`
}

// UpdatedCountResponse reports how many notifications a bulk update changed.
type UpdatedCountResponse struct {
	Updated int64 `json:"updated"`
}

// NotificationPreferenceResponse is one explicit channel preference. A missing
// row means the channel is enabled, so only rows the member changed are
// returned.
type NotificationPreferenceResponse struct {
	Type      string    `json:"type"`
	Channel   string    `json:"channel" enum:"in_app,email"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NotificationPreferencesResponse is the member's explicit preference set. An
// empty set is {} preferences with every channel enabled.
type NotificationPreferencesResponse struct {
	Preferences []NotificationPreferenceResponse `json:"preferences"`
}

// SetPreferenceRequest replaces the enabled state of one type/channel pair.
// The pair is identified by the item URI, so the body only carries the state.
type SetPreferenceRequest struct {
	Enabled *bool `json:"enabled" required:"true"`
}

// MergeRead is one RFC 7396 member for the notification read state. It records
// presence and null separately from the value; the schema documents the
// member's own type rather than the presence carrier.
type MergeRead struct{ patch.Field[bool] }

// Schema implements huma.SchemaProvider.
func (MergeRead) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeBoolean, Nullable: true}
}

// UpdateNotificationRequest is a merge patch on one notification. An absent
// read member leaves the state unchanged and an explicit null clears it
// (marks the notification unread).
type UpdateNotificationRequest struct {
	Read MergeRead `json:"read" required:"false"`
}

// UpdateNotificationsRequest marks the caller's whole feed read. The target
// map's only collection update is {read:true}; the merge-patch shape keeps the
// one patch syntax on both routes.
type UpdateNotificationsRequest struct {
	Read MergeRead `json:"read" required:"false"`
}

// CreateMemberNotificationRequest creates a notification for another member of
// the request tenant. The recipient comes from the path, never the body.
type CreateMemberNotificationRequest struct {
	Type  string         `json:"type" required:"true" maxLength:"100"`
	Title string         `json:"title" required:"true" maxLength:"255"`
	Body  string         `json:"body" required:"true" maxLength:"10000"`
	Data  map[string]any `json:"data,omitempty"`
}

// CreateNotificationResponse reports which channels accepted the notification.
// Notification is null when the recipient disabled the in-app channel.
type CreateNotificationResponse struct {
	Notification *NotificationResponse `json:"notification"`
	EmailQueued  bool                  `json:"emailQueued"`
}
