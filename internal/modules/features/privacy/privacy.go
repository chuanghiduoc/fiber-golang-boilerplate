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

const exportJSONOverhead int64 = 1024

// Target is one user-specific feature-flag override.
type Target struct {
	FlagKey   string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the module's privacy persistence port.
type Store interface {
	List(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]Target, bool, error)
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

type targetExport struct {
	FlagKey   string    `json:"flagKey"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type exportDocument struct {
	FeatureTargets []targetExport          `json:"featureTargets"`
	Budget         privacycontracts.Budget `json:"budget"`
}

func (p *Provider) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	targets, rowTruncated, err := p.store.List(ctx, tenantID, userID, p.rowLimit)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	out := exportDocument{FeatureTargets: make([]targetExport, 0, len(targets))}
	truncated := rowTruncated
	var used int64
	for _, target := range targets {
		item := targetExport(target)
		encoded, err := json.Marshal(item)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		if !p.fitsBudget(used, len(encoded)) {
			truncated = true
			break
		}
		out.FeatureTargets = append(out.FeatureTargets, item)
		used += int64(len(encoded))
	}
	out.Budget = privacycontracts.Budget{
		RowLimit: p.rowLimit, ByteLimit: p.byteLimit,
		Returned: len(out.FeatureTargets), Truncated: truncated,
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "features", Data: raw, Truncated: truncated}, nil
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
