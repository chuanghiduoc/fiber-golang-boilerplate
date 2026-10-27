// Package privacy implements the features module's privacy contract for
// per-user flag targets. Tenant-wide targets and flag definitions are not a
// user's personal data.
package privacy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
)

// Target is one user-specific feature-flag override.
type Target struct {
	FlagKey   string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the module's privacy persistence port.
type Store interface {
	List(ctx context.Context, tenantID, userID uuid.UUID) ([]Target, error)
	DeleteAll(ctx context.Context, tenantID, userID uuid.UUID) error
}

// Provider implements privacy.Exporter and privacy.Eraser.
type Provider struct {
	store Store
}

func New(store Store) *Provider { return &Provider{store: store} }

func (p *Provider) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	targets, err := p.store.List(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	type targetExport struct {
		FlagKey   string    `json:"flagKey"`
		Enabled   bool      `json:"enabled"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	out := struct {
		FeatureTargets []targetExport `json:"featureTargets"`
	}{FeatureTargets: make([]targetExport, len(targets))}
	for i, target := range targets {
		out.FeatureTargets[i] = targetExport(target)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "features", Data: raw}, nil
}

func (p *Provider) Erase(ctx context.Context, tenantID, userID uuid.UUID) error {
	return p.store.DeleteAll(ctx, tenantID, userID)
}
