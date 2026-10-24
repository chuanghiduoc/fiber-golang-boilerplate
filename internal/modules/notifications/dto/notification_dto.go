package dto

import "time"

// NotificationResponse is one in-app notification owned by the authenticated
// user in the request tenant.
type NotificationResponse struct {
	ID        int64          `json:"id"`
	TenantID  int64          `json:"tenantId"`
	UserID    int64          `json:"userId"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Data      map[string]any `json:"data" swaggertype:"object"`
	ReadAt    *time.Time     `json:"readAt,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// UnreadCountResponse is the unread badge count for the authenticated user.
type UnreadCountResponse struct {
	Count int64 `json:"count"`
}

// ReadAllResponse reports how many notifications were newly marked as read.
type ReadAllResponse struct {
	Updated int64 `json:"updated"`
}

// PreferenceResponse is one explicit channel preference. A missing row means
// the channel is enabled, so only rows the user changed are returned.
type PreferenceResponse struct {
	ID        int64     `json:"id"`
	TenantID  int64     `json:"tenantId"`
	UserID    int64     `json:"userId"`
	Type      string    `json:"type"`
	Channel   string    `json:"channel"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// UpdatePreferenceRequest upserts the enabled state for one notification type
// and channel. Types are lower-case identifiers such as "security.alert".
type UpdatePreferenceRequest struct {
	Type    string `json:"type" validate:"required,max=100"`
	Channel string `json:"channel" validate:"required,oneof=in_app email"`
	Enabled bool   `json:"enabled"`
}

// SendNotificationRequest is the back-office request for creating a
// notification for another user in the request tenant.
type SendNotificationRequest struct {
	UserID int64          `json:"userId" validate:"required,gt=0"`
	Type   string         `json:"type" validate:"required,max=100"`
	Title  string         `json:"title" validate:"required,max=255"`
	Body   string         `json:"body" validate:"required,max=10000"`
	Data   map[string]any `json:"data,omitempty" swaggertype:"object"`
}

// SendNotificationResponse reports which channels accepted the notification.
// Notification is null when the user disabled the in-app channel.
type SendNotificationResponse struct {
	Notification *NotificationResponse `json:"notification,omitempty"`
	EmailQueued  bool                  `json:"emailQueued"`
}
