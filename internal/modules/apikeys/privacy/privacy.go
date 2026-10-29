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

const exportJSONOverhead int64 = 1024

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
	List(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]Key, bool, error)
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

type exportDocument struct {
	APIKeys []keyExport             `json:"apiKeys"`
	Budget  privacycontracts.Budget `json:"budget"`
}

func (p *Provider) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	keys, rowTruncated, err := p.store.List(ctx, tenantID, userID, p.rowLimit)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	out := exportDocument{APIKeys: make([]keyExport, 0, len(keys))}
	truncated := rowTruncated
	var used int64
	for _, key := range keys {
		scopes := key.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		item := keyExport{
			ID: key.ID, Name: key.Name, Prefix: key.Prefix, Scopes: scopes,
			ExpiresAt: key.ExpiresAt, RevokedAt: key.RevokedAt,
			RotatedAt: key.RotatedAt, RotationGraceUntil: key.RotationGraceUntil,
			LastUsedAt: key.LastUsedAt, CreatedAt: key.CreatedAt, UpdatedAt: key.UpdatedAt,
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		if !p.fitsBudget(used, len(encoded)) {
			truncated = true
			break
		}
		out.APIKeys = append(out.APIKeys, item)
		used += int64(len(encoded))
	}
	out.Budget = privacycontracts.Budget{
		RowLimit: p.rowLimit, ByteLimit: p.byteLimit,
		Returned: len(out.APIKeys), Truncated: truncated,
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "apikeys", Data: raw, Truncated: truncated}, nil
}

func (p *Provider) fitsBudget(used int64, itemBytes int) bool {
	if p.byteLimit <= 0 {
		return true
	}
	return used+int64(itemBytes)+exportJSONOverhead <= p.byteLimit
}

func (p *Provider) Erase(ctx context.Context, tenantID, userID uuid.UUID) error {
	return p.store.DeleteAll(ctx, tenantID, userID)
}
