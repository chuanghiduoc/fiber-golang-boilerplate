package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// TenantDeletionGrace is how long a tenant stays in pending_deletion (and
// can be restored) before it may be marked deleted.
const TenantDeletionGrace = 30 * 24 * time.Hour

// Default tenant settings for a new tenant.
const (
	defaultTenantLocale   = "en"
	defaultTenantTimezone = "UTC"
)

// TenantStatusTransitionAllowed is the tenant lifecycle state machine:
//
//	trialing         -> active | suspended | pending_deletion
//	active           -> suspended | pending_deletion
//	suspended        -> active | pending_deletion
//	pending_deletion -> active (deletion cancelled) | deleted
//	deleted          -> (terminal)
func TenantStatusTransitionAllowed(from, to TenantStatus) bool {
	switch from {
	case TenantStatusTrialing:
		return to == TenantStatusActive || to == TenantStatusSuspended || to == TenantStatusPendingDeletion
	case TenantStatusActive:
		return to == TenantStatusSuspended || to == TenantStatusPendingDeletion
	case TenantStatusSuspended:
		return to == TenantStatusActive || to == TenantStatusPendingDeletion
	case TenantStatusPendingDeletion:
		return to == TenantStatusActive || to == TenantStatusDeleted
	case TenantStatusDeleted:
		return false
	default:
		return false
	}
}

// TenantService owns the tenant lifecycle. Platform-admin HTTP endpoints
// arrive with platform roles (P2b-2); until then the methods are used by the
// composition root, operators' tooling, and tests. Every mutation writes a
// Tier 1 audit row in its transaction.
type TenantService struct {
	stores Stores
	tx     Transactor
}

// NewTenantService wires the tenant service.
func NewTenantService(stores Stores, tx Transactor) *TenantService {
	return &TenantService{stores: stores, tx: tx}
}

func tenantEvent(ctx context.Context, action string, t *Tenant, metadata map[string]any) audit.Event {
	event := audit.NewServiceEvent(ctx, action, "tenant")
	event.TenantID = t.ID
	event.ResourceID = strconv.FormatInt(t.ID, 10)
	event.Metadata = metadata
	return event
}

// Create creates a tenant (trialing with a trial end, or active) and its
// settings row.
func (s *TenantService) Create(ctx context.Context, in NewTenant) (*Tenant, error) {
	in.Slug = strings.TrimSpace(in.Slug)
	in.Name = strings.TrimSpace(in.Name)
	if !tenant.IsValidSlug(in.Slug) {
		return nil, apperror.NewBadRequest("invalid tenant slug")
	}
	if in.Name == "" {
		return nil, apperror.NewBadRequest("tenant name is required")
	}
	if err := normalizeNewTenant(&in); err != nil {
		return nil, err
	}

	var created *Tenant
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		t, err := tx.Tenants.Create(ctx, in)
		if err != nil {
			if errors.Is(err, ErrTenantSlugTaken) {
				return apperror.NewConflict("tenant slug already exists")
			}
			return apperror.NewInternal("failed to create tenant")
		}
		created = t
		return checkAudit(tx.Audit.RecordTx(ctx, tenantEvent(ctx, "tenant.created", t, map[string]any{"status": string(t.Status)})))
	})
	return created, err
}

// normalizeNewTenant validates the initial state and fills settings defaults.
func normalizeNewTenant(in *NewTenant) error {
	switch in.Status {
	case TenantStatusTrialing:
		if in.TrialEndsAt == nil || !in.TrialEndsAt.After(time.Now()) {
			return apperror.NewBadRequest("a trialing tenant needs a future trial end")
		}
	case TenantStatusActive:
	case TenantStatusSuspended, TenantStatusPendingDeletion, TenantStatusDeleted:
		return apperror.NewBadRequest("a new tenant must be trialing or active")
	default:
		return apperror.NewBadRequest("unknown tenant status")
	}
	if in.Locale == "" {
		in.Locale = defaultTenantLocale
	}
	if in.Timezone == "" {
		in.Timezone = defaultTenantTimezone
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return apperror.NewBadRequest("timezone must be an IANA time zone name")
	}
	return nil
}

// Transition moves a tenant to status along the lifecycle state machine.
// pending_deletion schedules deletion after TenantDeletionGrace; cancelling
// it (back to active) clears the schedule. The compatibility tenant (id 1)
// can never be deleted. Requests for a non-operational tenant return 404 from
// tenant resolution (after the resolver cache TTL) and 401 for existing
// sessions.
func (s *TenantService) Transition(ctx context.Context, id int64, to TenantStatus) (*Tenant, error) {
	var updated *Tenant
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		current, err := tx.Tenants.GetByIDForUpdate(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("tenant not found")
			}
			return apperror.NewInternal("failed to load tenant")
		}
		if !TenantStatusTransitionAllowed(current.Status, to) {
			return apperror.NewConflict(fmt.Sprintf("a %s tenant cannot become %s", current.Status, to))
		}
		if id == tenant.DefaultID && (to == TenantStatusPendingDeletion || to == TenantStatusDeleted) {
			return apperror.NewConflict("the default tenant cannot be deleted")
		}
		t, err := tx.Tenants.UpdateStatus(ctx, id, to, deletionSchedule(current, to))
		if err != nil {
			return apperror.NewInternal("failed to update tenant")
		}
		updated = t
		return checkAudit(tx.Audit.RecordTx(ctx, tenantEvent(ctx, "tenant.status_changed", t,
			map[string]any{"from": string(current.Status), "to": string(to)})))
	})
	return updated, err
}

// deletionSchedule returns deletion_scheduled_at for the new state: set when
// entering pending_deletion, kept when it is finally deleted, cleared
// otherwise.
func deletionSchedule(current *Tenant, to TenantStatus) *time.Time {
	switch to {
	case TenantStatusPendingDeletion:
		at := time.Now().Add(TenantDeletionGrace)
		return &at
	case TenantStatusDeleted:
		return current.DeletionScheduledAt
	case TenantStatusTrialing, TenantStatusActive, TenantStatusSuspended:
		return nil
	default:
		return nil
	}
}

// Settings returns a tenant's typed settings.
func (s *TenantService) Settings(ctx context.Context, id int64) (*TenantSettings, error) {
	settings, err := s.stores.Tenants.Settings(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("tenant not found")
		}
		return nil, apperror.NewInternal("failed to load tenant settings")
	}
	return settings, nil
}

// GetOperational returns an operational tenant by id (OAuth state checks).
func (s *TenantService) GetOperational(ctx context.Context, id int64) (*Tenant, error) {
	return s.stores.Tenants.GetOperationalByID(ctx, id)
}

// ResolveOperational returns an operational tenant by slug.
func (s *TenantService) ResolveOperational(ctx context.Context, slug string) (*Tenant, error) {
	return s.stores.Tenants.GetOperationalBySlug(ctx, slug)
}

// StartupTenants verifies the baseline compatibility tenant (id 1) and the
// configured default tenant exist, ignoring status. A non-operational tenant
// is a valid startup state: request resolution returns 404 until it is
// reactivated, but the process and seed command must still start.
func StartupTenants(ctx context.Context, repo TenantRepository, defaultSlug string) (baseline, configured *Tenant, err error) {
	baseline, err = repo.GetByID(ctx, tenant.DefaultID)
	if err != nil {
		return nil, nil, fmt.Errorf("baseline tenant (id %d) is missing; run cmd/migrate: %w", tenant.DefaultID, err)
	}
	configured, err = repo.GetBySlug(ctx, defaultSlug)
	if err != nil {
		return nil, nil, fmt.Errorf("configured default tenant %q is missing; run cmd/migrate or fix TENANT_DEFAULT_SLUG: %w", defaultSlug, err)
	}
	return baseline, configured, nil
}
