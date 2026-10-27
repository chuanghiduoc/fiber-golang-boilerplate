// Package privacy is the stdlib-only contract for per-user data export and
// erasure. Modules implement Exporter and Eraser in their own package and
// register a Provider through app.PrivacyProvider; identity orchestrates the
// calls, so no module imports another.
package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"

	"github.com/google/uuid"
)

// Section is one module's contribution to a data export. Name is the module
// name; Data must be a JSON value.
type Section struct {
	Name string
	Data json.RawMessage
}

// Exporter returns the module's data for one user in one tenant. Implementations
// must scope every lookup to (tenantID, userID) and never return credentials,
// hashes, or another tenant's rows.
type Exporter interface {
	Export(ctx context.Context, tenantID, userID uuid.UUID) (Section, error)
}

// Eraser deletes or anonymizes the module's data for one user in one tenant.
// It is idempotent: a repeated call after a successful one succeeds.
type Eraser interface {
	Erase(ctx context.Context, tenantID, userID uuid.UUID) error
}

// Provider is one module's privacy implementation. Both interfaces are
// required: every module that registers holds per-user data in both
// directions.
type Provider struct {
	Name     string
	Exporter Exporter
	Eraser   Eraser
}

var providerName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Registry is the process-wide set of privacy providers, populated once at
// startup through app.PrivacyProvider. Register panics on a programming error
// (duplicate name or missing side), like a duplicate route.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds p. It preserves registration order and returns a snapshot from
// Providers.
func (r *Registry) Register(p Provider) {
	if !providerName.MatchString(p.Name) {
		panic(fmt.Sprintf("privacy: invalid provider name %q", p.Name))
	}
	if p.Exporter == nil || p.Eraser == nil {
		panic(fmt.Sprintf("privacy: provider %q must implement both Exporter and Eraser", p.Name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.providers {
		if existing.Name == p.Name {
			panic(fmt.Sprintf("privacy: provider %q is registered twice", p.Name))
		}
	}
	r.providers = append(r.providers, p)
}

// Providers returns a snapshot in registration order.
func (r *Registry) Providers() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}
