package tenancy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// TenantDeletionGrace is how long a tenant stays in pending_deletion (and
// can be restored) before it may be marked deleted.
const TenantDeletionGrace = 30 * 24 * time.Hour

// Default tenant settings for a new tenant.
const (
	defaultTenantLocale   = "en"
	defaultTenantTimezone = "UTC"
)

// tenantLocalePattern mirrors the tenant_settings.locale CHECK.
var tenantLocalePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// TenantStatusTransitionAllowed is the tenant lifecycle state machine:
//
//	trialing         -> active | suspended | pending_deletion
//	active           -> suspended | pending_deletion
//	suspended        -> active | pending_deletion
//	pending_deletion -> suspended (deletion cancelled) | deleted
//	deleted          -> (terminal)
//
// Cancelling a deletion lands on suspended, never directly on active: the
// tenant was not necessarily operational before deletion, so a platform admin
// must activate it explicitly afterwards.
func TenantStatusTransitionAllowed(from, to TenantStatus) bool {
	switch from {
	case TenantStatusTrialing:
		return to == TenantStatusActive || to == TenantStatusSuspended || to == TenantStatusPendingDeletion
	case TenantStatusActive:
		return to == TenantStatusSuspended || to == TenantStatusPendingDeletion
	case TenantStatusSuspended:
		return to == TenantStatusActive || to == TenantStatusPendingDeletion
	case TenantStatusPendingDeletion:
		return to == TenantStatusSuspended || to == TenantStatusDeleted
	case TenantStatusDeleted:
		return false
	default:
		return false
	}
}

// TenantService owns the tenant lifecycle. The platform tenant endpoints and
// the composition root use it. Every mutation writes a Tier 1 audit row in its
// transaction.
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
	event.ResourceID = t.ID.String()
	event.Metadata = metadata
	return event
}

// List returns one cursor page of every tenant (platform administration).

// Get returns a tenant in any lifecycle state.
func (s *TenantService) List(ctx context.Context, limit int, startingAfter string) (listed []Tenant, more bool, listErr error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	tenants, err := s.stores.Tenants.ListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list tenants")
	}
	hasMore := len(tenants) > pageSize
	if hasMore {
		tenants = tenants[:pageSize]
	}
	return tenants, hasMore, nil
}

func (s *TenantService) Get(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	t, err := s.stores.Tenants.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("tenant not found")
		}
		return nil, apperror.NewInternal("failed to load tenant")
	}
	return t, nil
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
	// Deliberate RLS bypass: the new tenant and its settings/audit row are not
	// the caller's request tenant, so no single app.tenant_id can cover them.
	ctx = database.WithRLSBypass(ctx)
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		t, err := tx.Tenants.Create(ctx, in)
		if err != nil {
			if errors.Is(err, ErrTenantSlugTaken) {
				return apperror.NewConflict("tenant slug already exists")
			}
			return apperror.NewInternal("failed to create tenant")
		}
		if in.OwnerUserID != uuid.Nil {
			ownerCtx := tenant.WithID(ctx, t.ID)
			if _, err := tx.Memberships.Create(ownerCtx, in.OwnerUserID, RoleOwner); err != nil {
				return apperror.NewInternal("failed to assign the tenant owner")
			}
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
	if !tenantLocalePattern.MatchString(in.Locale) {
		return apperror.NewBadRequest("locale must be a BCP 47 language tag")
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
// pending_deletion schedules deletion after TenantDeletionGrace; cancelling it
// lands on suspended and clears the schedule (a platform admin activates it
// explicitly afterwards). The default tenant (id 1) can never be
// deleted. Requests for a non-operational tenant return 404 from tenant
// resolution (after the resolver cache TTL) and 401 for existing sessions.
func (s *TenantService) Transition(ctx context.Context, id uuid.UUID, to TenantStatus) (*Tenant, error) {
	var updated *Tenant
	// Deliberate RLS bypass: the platform transition targets a tenant other
	// than the caller's (the tenants table is platform-wide lifecycle data).
	ctx = database.WithRLSBypass(ctx)
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
		if to == TenantStatusDeleted && (current.DeletionScheduledAt == nil ||
			time.Now().Before(*current.DeletionScheduledAt)) {
			return apperror.NewCode(identitycatalog.CodeTenantDeletionNotDue,
				"the tenant deletion grace period has not ended")
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

// Update applies a merge patch to a tenant's name and/or status. It shares
// the transition rules with Transition and records one tenant.status_changed
// audit row in the mutation transaction.
func (s *TenantService) Update(ctx context.Context, id uuid.UUID, update TenantUpdate) (*Tenant, error) {
	var updated *Tenant
	// Deliberate RLS bypass: platform administration targets a tenant other
	// than the caller's.
	ctx = database.WithRLSBypass(ctx)
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		current, err := tx.Tenants.GetByIDForUpdate(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("tenant not found")
			}
			return apperror.Internal(err, "failed to load tenant")
		}
		// The conditional UPDATE is the compare-and-swap: a second writer
		// holding the same ETag gets 412 without applying its patch or audit
		// row.
		if update.ExpectedUpdatedAt != nil {
			won, err := tx.Tenants.TouchIfUnchanged(ctx, id, *update.ExpectedUpdatedAt)
			if err != nil {
				return apperror.Internal(err, "failed to update tenant")
			}
			if !won {
				return apperror.NewPreconditionFailed("the tenant has changed since it was read; refresh and retry")
			}
			current, err = tx.Tenants.GetByIDForUpdate(ctx, id)
			if err != nil {
				return apperror.Internal(err, "failed to reload tenant")
			}
		}
		metadata := map[string]any{"from": string(current.Status), "to": string(current.Status)}
		result, err := applyTenantName(ctx, tx, current, id, update.Name, metadata)
		if err != nil {
			return err
		}
		result, err = applyTenantStatus(ctx, tx, result, id, update.Status, metadata)
		if err != nil {
			return err
		}
		updated = result
		return checkAudit(tx.Audit.RecordTx(ctx, tenantEvent(ctx, "tenant.status_changed", result, metadata)))
	})
	return updated, err
}

// applyTenantName renames a tenant when the patch changes it.
func applyTenantName(ctx context.Context, tx TxRepos, current *Tenant, id uuid.UUID, raw *string, metadata map[string]any) (*Tenant, error) {
	if raw == nil {
		return current, nil
	}
	name := strings.TrimSpace(*raw)
	if name == "" {
		return nil, apperror.NewBadRequest("tenant name is required")
	}
	if name == current.Name {
		return current, nil
	}
	renamed, err := tx.Tenants.UpdateName(ctx, id, name)
	if err != nil {
		return nil, apperror.Internal(err, "failed to update tenant")
	}
	metadata["name_changed"] = true
	return renamed, nil
}

// applyTenantStatus validates and applies a lifecycle transition.
func applyTenantStatus(ctx context.Context, tx TxRepos, current *Tenant, id uuid.UUID, raw *TenantStatus, metadata map[string]any) (*Tenant, error) {
	if raw == nil {
		return current, nil
	}
	to := *raw
	if !TenantStatusTransitionAllowed(current.Status, to) {
		return nil, apperror.NewConflict(fmt.Sprintf("a %s tenant cannot become %s", current.Status, to))
	}
	if id == tenant.DefaultID && (to == TenantStatusPendingDeletion || to == TenantStatusDeleted) {
		return nil, apperror.NewConflict("the default tenant cannot be deleted")
	}
	if to == TenantStatusDeleted && (current.DeletionScheduledAt == nil ||
		time.Now().Before(*current.DeletionScheduledAt)) {
		return nil, apperror.NewCode(identitycatalog.CodeTenantDeletionNotDue,
			"the tenant deletion grace period has not ended")
	}
	transitioned, err := tx.Tenants.UpdateStatus(ctx, id, to, deletionSchedule(current, to))
	if err != nil {
		return nil, apperror.Internal(err, "failed to update tenant")
	}
	metadata["from"] = string(current.Status)
	metadata["to"] = string(to)
	return transitioned, nil
}

// ExpireTrial moves a trialing tenant whose trial_ends_at has passed to
// suspended. It is the system-actor path used by the hourly schedule: the
// status is re-checked under the row lock, a concurrent admin transition is a
// no-op, and the change is audited as tenant.status_changed with a
// reason=trial_expired metadata entry.
func (s *TenantService) ExpireTrial(ctx context.Context, id uuid.UUID) (bool, error) {
	ctx = tenant.WithID(ctx, id)
	var changed bool
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		current, err := tx.Tenants.GetByIDForUpdate(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return nil
			}
			return apperror.NewInternal("failed to load tenant")
		}
		if current.Status != TenantStatusTrialing || current.TrialEndsAt == nil ||
			current.TrialEndsAt.After(time.Now()) {
			return nil
		}
		t, err := tx.Tenants.UpdateStatus(ctx, id, TenantStatusSuspended, nil)
		if err != nil {
			return apperror.NewInternal("failed to suspend expired trial")
		}
		changed = true
		event := tenantEvent(ctx, "tenant.status_changed", t,
			map[string]any{"from": string(TenantStatusTrialing), "to": string(TenantStatusSuspended), "reason": "trial_expired"})
		event.ActorType = audit.ActorTypeSystem
		event.Source = audit.SourceSystem
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	return changed, err
}

// FinalizeDeletion moves a pending_deletion tenant whose grace period has
// ended to deleted. It is the system-actor path used by the hourly schedule:
// the status and schedule are re-checked under the row lock, a concurrent
// admin transition is a no-op, and the change is audited as
// tenant.status_changed with a reason=deletion_due metadata entry.
func (s *TenantService) FinalizeDeletion(ctx context.Context, id uuid.UUID) (bool, error) {
	ctx = tenant.WithID(ctx, id)
	var changed bool
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		current, err := tx.Tenants.GetByIDForUpdate(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return nil
			}
			return apperror.NewInternal("failed to load tenant")
		}
		if current.Status != TenantStatusPendingDeletion || current.DeletionScheduledAt == nil ||
			current.DeletionScheduledAt.After(time.Now()) || id == tenant.DefaultID {
			return nil
		}
		t, err := tx.Tenants.UpdateStatus(ctx, id, TenantStatusDeleted, current.DeletionScheduledAt)
		if err != nil {
			return apperror.NewInternal("failed to finalize tenant deletion")
		}
		changed = true
		event := tenantEvent(ctx, "tenant.status_changed", t,
			map[string]any{"from": string(TenantStatusPendingDeletion), "to": string(TenantStatusDeleted), "reason": "deletion_due"})
		event.ActorType = audit.ActorTypeSystem
		event.Source = audit.SourceSystem
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	return changed, err
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
func (s *TenantService) Settings(ctx context.Context, id uuid.UUID) (*TenantSettings, error) {
	// Deliberate RLS bypass: platform administration reads another tenant's
	// settings, which the tenant's own scope would hide.
	ctx = database.WithRLSBypass(ctx)
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
func (s *TenantService) GetOperational(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	return s.stores.Tenants.GetOperationalByID(ctx, id)
}

// ResolveOperational returns an operational tenant by slug.
func (s *TenantService) ResolveOperational(ctx context.Context, slug string) (*Tenant, error) {
	return s.stores.Tenants.GetOperationalBySlug(ctx, slug)
}

// StartupTenants verifies the seeded default tenant (id 1) and the
// configured default tenant exist, ignoring status. A non-operational tenant
// is a valid startup state: request resolution returns 404 until it is
// reactivated, but the process and seed command must still start.
func StartupTenants(ctx context.Context, repo TenantRepository, defaultSlug string) (baseline, configured *Tenant, err error) {
	baseline, err = repo.GetByID(ctx, tenant.DefaultID)
	if err != nil {
		return nil, nil, fmt.Errorf("baseline tenant (id %s) is missing; run cmd/migrate: %w", tenant.DefaultID, err)
	}
	configured, err = repo.GetBySlug(ctx, defaultSlug)
	if err != nil {
		return nil, nil, fmt.Errorf("configured default tenant %q is missing; run cmd/migrate or fix TENANT_DEFAULT_SLUG: %w", defaultSlug, err)
	}
	return baseline, configured, nil
}
