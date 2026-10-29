// Package tenancy is the stdlib-only contract for tenant-wide data purging.
// Modules implement Purger in their own package and the composition root
// registers one Provider per module, so identity can erase a deleted tenant's
// remaining data without importing any module. Per-user data is erased through
// the privacy contract first; a Purger removes what is left and owned by the
// whole tenant (configuration, history, credentials).
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/google/uuid"
)

// PurgeBatchSize is the maximum number of rows one tenant-purge statement may
// touch. A purger loops until a batch comes back empty, so memory use and lock
// sets stay constant no matter how large the tenant is.
const PurgeBatchSize int32 = 500

// ErrPurgeBudgetExceeded tells the orchestrator that a purger stopped at a
// batch boundary because its context deadline ended. The rows already deleted
// stay deleted; the next run resumes from the remaining rows. Callers must not
// turn this into a job failure, because a retry would only repeat intentional
// deferral work.
var ErrPurgeBudgetExceeded = errors.New("tenant purge budget exceeded")

// Purger erases every row the module owns for one tenant. It never deletes the
// tenants row, the tenant's audit trail, or another tenant's data. It must be
// idempotent: a repeated call after a successful one succeeds.
type Purger interface {
	PurgeTenant(ctx context.Context, tenantID uuid.UUID) error
}

// CheckPurgeBudget returns ErrPurgeBudgetExceeded when the purge budget
// deadline has elapsed. It returns the original cancellation unchanged, so a
// shutting-down process is never mistaken for a resumable partial purge.
func CheckPurgeBudget(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrPurgeBudgetExceeded
	}
	return err
}

// Provider is one module's tenant purger.
type Provider struct {
	Name   string
	Purger Purger
}

var providerName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Registry is the process-wide set of tenant purgers, populated once at
// startup by the composition root. Register panics on a programming error
// (duplicate name or a nil purger), like a duplicate route.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds p. It preserves registration order, which callers use to run
// the owning module's own cleanup last.
func (r *Registry) Register(p Provider) {
	if !providerName.MatchString(p.Name) {
		panic(fmt.Sprintf("tenancy: invalid provider name %q", p.Name))
	}
	if p.Purger == nil {
		panic(fmt.Sprintf("tenancy: provider %q has a nil purger", p.Name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.providers {
		if existing.Name == p.Name {
			panic(fmt.Sprintf("tenancy: provider %q is registered twice", p.Name))
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
