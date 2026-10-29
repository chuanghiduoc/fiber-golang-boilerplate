// Package privacy implements the notifications module's data-export and
// erasure contract over a narrow store, so the rules are unit-testable without
// sqlc.
package privacy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
)

const exportJSONOverhead int64 = 1024

// Notification is one in-app feed row.
type Notification struct {
	ID        uuid.UUID
	Type      string
	Title     string
	Body      string
	Data      json.RawMessage
	ReadAt    *time.Time
	CreatedAt time.Time
}

// Preference is one per-type channel preference.
type Preference struct {
	Type      string
	Channel   string
	Enabled   bool
	UpdatedAt time.Time
}

// Store is the module's privacy persistence port.
type Store interface {
	ListNotifications(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]Notification, bool, error)
	ListPreferences(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]Preference, bool, error)
	DeleteAll(ctx context.Context, tenantID, userID uuid.UUID) error
}

// Provider implements privacy.Exporter and privacy.Eraser.
type Provider struct {
	store     Store
	rowLimit  int32
	byteLimit int64
}

func New(store Store) *Provider {
	return &Provider{
		store:     store,
		rowLimit:  privacycontracts.DefaultRowLimit,
		byteLimit: privacycontracts.DefaultByteLimit,
	}
}

func (p *Provider) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	notifications, notificationsTruncated, err := p.store.ListNotifications(ctx, tenantID, userID, p.rowLimit)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	preferences, preferencesTruncated, err := p.store.ListPreferences(ctx, tenantID, userID, p.rowLimit)
	if err != nil {
		return privacycontracts.Section{}, err
	}

	type notificationExport struct {
		ID        uuid.UUID       `json:"id"`
		Type      string          `json:"type"`
		Title     string          `json:"title"`
		Body      string          `json:"body"`
		Data      json.RawMessage `json:"data"`
		ReadAt    *time.Time      `json:"readAt,omitempty"`
		CreatedAt time.Time       `json:"createdAt"`
	}
	type preferenceExport struct {
		Type      string    `json:"type"`
		Channel   string    `json:"channel"`
		Enabled   bool      `json:"enabled"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	out := struct {
		Notifications []notificationExport    `json:"notifications"`
		Preferences   []preferenceExport      `json:"preferences"`
		Budget        privacycontracts.Budget `json:"budget"`
	}{
		Notifications: make([]notificationExport, 0, len(notifications)),
		Preferences:   make([]preferenceExport, 0, len(preferences)),
	}
	truncated := notificationsTruncated || preferencesTruncated
	var used int64
	for _, row := range notifications {
		data := row.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		item := notificationExport{
			ID: row.ID, Type: row.Type, Title: row.Title, Body: row.Body,
			Data: data, ReadAt: row.ReadAt, CreatedAt: row.CreatedAt,
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		if p.byteLimit > 0 && used+int64(len(encoded))+exportJSONOverhead > p.byteLimit {
			truncated = true
			break
		}
		out.Notifications = append(out.Notifications, item)
		used += int64(len(encoded))
	}
	for _, row := range preferences {
		item := preferenceExport(row)
		encoded, err := json.Marshal(item)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		if p.byteLimit > 0 && used+int64(len(encoded))+exportJSONOverhead > p.byteLimit {
			truncated = true
			break
		}
		out.Preferences = append(out.Preferences, item)
		used += int64(len(encoded))
	}
	out.Budget = privacycontracts.Budget{
		RowLimit: p.rowLimit, ByteLimit: p.byteLimit,
		Returned: len(out.Notifications) + len(out.Preferences), Truncated: truncated,
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "notifications", Data: raw, Truncated: truncated}, nil
}

func (p *Provider) Erase(ctx context.Context, tenantID, userID uuid.UUID) error {
	return p.store.DeleteAll(ctx, tenantID, userID)
}
