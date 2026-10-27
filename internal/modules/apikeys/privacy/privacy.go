// Package privacy implements the apikeys module's privacy contract. It only
// exposes key metadata; the key hash never leaves the module.
package privacy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
)

// Key is one API key's non-secret metadata.
type Key struct {
	ID                 uuid.UUID
	Name               string
	Prefix             string
	Scopes             []string
	ExpiresAt          time.Time
	RevokedAt          *time.Time
	RotatedAt          *time.Time
	RotationGraceUntil *time.Time
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Store is the module's privacy persistence port.
type Store interface {
	List(ctx context.Context, tenantID, userID uuid.UUID) ([]Key, error)
	DeleteAll(ctx context.Context, tenantID, userID uuid.UUID) error
}

// Provider implements privacy.Exporter and privacy.Eraser.
type Provider struct {
	store Store
}

func New(store Store) *Provider { return &Provider{store: store} }

func (p *Provider) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	keys, err := p.store.List(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	type keyExport struct {
		ID                 uuid.UUID  `json:"id"`
		Name               string     `json:"name"`
		Prefix             string     `json:"prefix"`
		Scopes             []string   `json:"scopes"`
		ExpiresAt          time.Time  `json:"expiresAt"`
		RevokedAt          *time.Time `json:"revokedAt,omitempty"`
		RotatedAt          *time.Time `json:"rotatedAt,omitempty"`
		RotationGraceUntil *time.Time `json:"rotationGraceUntil,omitempty"`
		LastUsedAt         *time.Time `json:"lastUsedAt,omitempty"`
		CreatedAt          time.Time  `json:"createdAt"`
		UpdatedAt          time.Time  `json:"updatedAt"`
	}
	out := struct {
		APIKeys []keyExport `json:"apiKeys"`
	}{APIKeys: make([]keyExport, len(keys))}
	for i, key := range keys {
		out.APIKeys[i] = keyExport(key)
		if out.APIKeys[i].Scopes == nil {
			out.APIKeys[i].Scopes = []string{}
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "apikeys", Data: raw}, nil
}

func (p *Provider) Erase(ctx context.Context, tenantID, userID uuid.UUID) error {
	return p.store.DeleteAll(ctx, tenantID, userID)
}
