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
	ListNotifications(ctx context.Context, tenantID, userID uuid.UUID) ([]Notification, error)
	ListPreferences(ctx context.Context, tenantID, userID uuid.UUID) ([]Preference, error)
	DeleteAll(ctx context.Context, tenantID, userID uuid.UUID) error
}

// Provider implements privacy.Exporter and privacy.Eraser.
type Provider struct {
	store Store
}

func New(store Store) *Provider { return &Provider{store: store} }

func (p *Provider) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	notifications, err := p.store.ListNotifications(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	preferences, err := p.store.ListPreferences(ctx, tenantID, userID)
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
		Notifications []notificationExport `json:"notifications"`
		Preferences   []preferenceExport   `json:"preferences"`
	}{
		Notifications: make([]notificationExport, len(notifications)),
		Preferences:   make([]preferenceExport, len(preferences)),
	}
	for i, row := range notifications {
		data := row.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		out.Notifications[i] = notificationExport{
			ID: row.ID, Type: row.Type, Title: row.Title, Body: row.Body,
			Data: data, ReadAt: row.ReadAt, CreatedAt: row.CreatedAt,
		}
	}
	for i, row := range preferences {
		out.Preferences[i] = preferenceExport(row)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "notifications", Data: raw}, nil
}

func (p *Provider) Erase(ctx context.Context, tenantID, userID uuid.UUID) error {
	return p.store.DeleteAll(ctx, tenantID, userID)
}
