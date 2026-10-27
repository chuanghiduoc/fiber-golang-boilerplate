package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/google/uuid"
)

const (
	// TenantPurgeJobKind erases a deleted tenant's remaining data.
	TenantPurgeJobKind = "identity.tenants.purge_deleted"
	// TenantPurgeInterval is the hourly retention cadence.
	TenantPurgeInterval = time.Hour
	// TrialExpiryJobKind suspends trialing tenants whose trial ended.
	TrialExpiryJobKind = "identity.tenants.expire_trials"
	// TrialExpiryInterval is the hourly cadence that closes expired trials.
	TrialExpiryInterval = time.Hour
	// TenantDeletionJobKind moves pending_deletion tenants past their grace
	// period to deleted.
	TenantDeletionJobKind = "identity.tenants.finalize_deletions"
	// TenantDeletionInterval is the hourly cadence that finalizes due
	// deletions.
	TenantDeletionInterval = time.Hour

	// tenantPurgeBatchSize bounds how many deleted tenants one run starts.
	tenantPurgeBatchSize = 25
	// tenantPurgeMembershipBatch bounds one membership page of a tenant.
	tenantPurgeMembershipBatch = 100
	// tenantPurgeTrialBatch bounds one expired-trial page.
	tenantPurgeTrialBatch = 100

	// tenantPurgeTenantBudget is how long one run may spend on a single
	// tenant. Progress is persisted per member, so the next run resumes it.
	tenantPurgeTenantBudget = 2 * time.Minute
	// tenantPurgeRunBudget leaves headroom under the 10-minute job timeout so
	// one slow tenant cannot consume the whole pass.
	tenantPurgeRunBudget = 8 * time.Minute
)

// tenantPurgerName is identity's own tenant-wide purger. It runs last so the
// per-member erasers have already removed every module's user rows.
const tenantPurgerName = "identity"

// PurgeTenant is one tenant waiting for its deleted data to be erased.
type PurgeTenant struct {
	ID   uuid.UUID
	Slug string
	// Cursor is the highest membership user id whose erasers already
	// succeeded in an earlier run.
	Cursor uuid.UUID
}

// TenantPurgeRepository is the identity persistence the purge, hourly trial
// expiry, and deletion-finalizer jobs need. Every tenant-scoped method runs
// under the tenant context and therefore RLS; only the platform-wide tenant
// lists are unscoped.
type TenantPurgeRepository interface {
	ListDeletedTenants(ctx context.Context, limit int32) ([]PurgeTenant, error)
	ListDueDeletionTenantIDs(ctx context.Context, limit int32) ([]uuid.UUID, error)
	ListExpiredTrialTenantIDs(ctx context.Context, limit int32) ([]uuid.UUID, error)
	ListMembershipUserIDs(ctx context.Context, tenantID, afterUserID uuid.UUID, limit int32) ([]uuid.UUID, error)
	ListDataExports(ctx context.Context, tenantID uuid.UUID, limit int32) ([]DataExport, error)
	DeleteDataExports(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) error
	DeleteMemberInvitations(ctx context.Context, tenantID, userID uuid.UUID) (int64, error)
	DeleteMembership(ctx context.Context, tenantID, userID uuid.UUID) error
	AdvancePurgeCursor(ctx context.Context, tenantID, cursor uuid.UUID) error
	DeleteInvitationsBatch(ctx context.Context, tenantID uuid.UUID, limit int32) (int64, error)
	DeleteMembershipsBatch(ctx context.Context, tenantID uuid.UUID, limit int32) (int64, error)
	DeleteTenantRolesBatch(ctx context.Context, tenantID uuid.UUID, limit int32) (int64, error)
	DeleteTenantSettings(ctx context.Context, tenantID uuid.UUID) error
	MarkPurged(ctx context.Context, tenantID uuid.UUID, event audit.Event) (bool, error)
}

// TenantPurgeDeps are the purge service's collaborators.
type TenantPurgeDeps struct {
	Repo     TenantPurgeRepository
	Privacy  *PrivacyService
	Tenants  *TenantService
	Registry *tenancycontracts.Registry
	Storage  storage.Storage
}

// TenantPurgeService orchestrates the deleted-tenant purge, the hourly trial
// expiry, and the pending-deletion finalizer. It also implements
// tenancy.Purger for identity's own tenant-wide rows.
type TenantPurgeService struct {
	repo     TenantPurgeRepository
	privacy  *PrivacyService
	tenants  *TenantService
	registry *tenancycontracts.Registry
	storage  storage.Storage
	// now is the clock used for the per-tenant and per-run budgets. Tests set
	// it to advance time without sleeping.
	now func() time.Time
}

var _ tenancycontracts.Purger = (*TenantPurgeService)(nil)

// NewTenantPurgeService wires the purge service.
func NewTenantPurgeService(deps TenantPurgeDeps) *TenantPurgeService {
	return &TenantPurgeService{
		repo:     deps.Repo,
		privacy:  deps.Privacy,
		tenants:  deps.Tenants,
		registry: deps.Registry,
		storage:  deps.Storage,
		now:      time.Now,
	}
}

// HandlePurgeJob purges every deleted tenant that has not completed yet,
// least recently advanced first. It is resumable: progress is persisted per
// member, each phase is idempotent, and purged_at is written only after every
// module succeeded. One failing tenant does not stop the others; the joined
// error makes the job retry. Per-tenant and per-run budgets keep a large
// tenant from starving the rest.
func (s *TenantPurgeService) HandlePurgeJob(ctx context.Context, _ json.RawMessage) error {
	ctx = authz.SystemContext(ctx)
	tenants, err := s.repo.ListDeletedTenants(ctx, tenantPurgeBatchSize)
	if err != nil {
		return fmt.Errorf("list deleted tenants: %w", err)
	}
	runStart := s.clock()
	var errs []error
	for _, t := range tenants {
		if s.clock().Sub(runStart) >= tenantPurgeRunBudget {
			slog.Info("tenant purge run budget exhausted; remaining tenants continue next run",
				slog.String("next_tenant_id", t.ID.String()))
			break
		}
		completed, err := s.purgeDeletedTenant(ctx, t)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t.ID, err))
			continue
		}
		if !completed {
			slog.Info("tenant purge deferred by its per-tenant time budget",
				slog.String("tenant_id", t.ID.String()))
		}
	}
	return errors.Join(errs...)
}

// purgeDeletedTenant reports completed=false when its per-tenant budget ran
// out before the tenant was finished; the persisted cursor lets the next run
// resume after the last finished member.
func (s *TenantPurgeService) purgeDeletedTenant(ctx context.Context, t PurgeTenant) (bool, error) {
	start := s.clock()
	tctx := tenant.WithID(ctx, t.ID)
	completed, err := s.eraseMemberships(tctx, start, t.ID, t.Cursor)
	if err != nil {
		return false, err
	}
	if !completed {
		return false, nil
	}
	if s.registry == nil {
		return false, fmt.Errorf("tenant purge registry is not configured")
	}
	providers := s.registry.Providers()
	identitySeen := false
	for _, provider := range providers {
		if provider.Name == tenantPurgerName {
			continue
		}
		if s.budgetExhausted(start) {
			return false, nil
		}
		if err := provider.Purger.PurgeTenant(tctx, t.ID); err != nil {
			return false, fmt.Errorf("purge %s: %w", provider.Name, err)
		}
	}
	for _, provider := range providers {
		if provider.Name != tenantPurgerName {
			continue
		}
		if s.budgetExhausted(start) {
			return false, nil
		}
		identitySeen = true
		if err := provider.Purger.PurgeTenant(tctx, t.ID); err != nil {
			return false, fmt.Errorf("purge %s: %w", provider.Name, err)
		}
	}
	if !identitySeen {
		return false, fmt.Errorf("identity tenant purger is not registered")
	}
	if s.budgetExhausted(start) {
		return false, nil
	}

	event := audit.NewServiceEvent(tctx, "tenant.purged", "tenant")
	event.TenantID = t.ID
	event.ResourceID = t.ID.String()
	event.ActorType = audit.ActorTypeSystem
	event.Source = audit.SourceSystem
	marked, err := s.repo.MarkPurged(tctx, t.ID, event)
	if err != nil {
		return false, fmt.Errorf("mark tenant purged: %w", err)
	}
	if marked {
		slog.Info("deleted tenant purged", slog.String("tenant_id", t.ID.String()), slog.String("tenant", t.Slug))
	}
	return true, nil
}

// eraseMemberships runs the privacy erasers for every membership of the
// tenant, including removed ones, in user-id pages from cursor. Each member's
// membership is deleted as soon as every eraser succeeded, so a resumed run
// never revisits finished work. Identity's own tenant-wide purger still runs
// afterwards for anything left.
func (s *TenantPurgeService) eraseMemberships(ctx context.Context, start time.Time, tenantID, cursor uuid.UUID) (bool, error) {
	if s.privacy == nil {
		return false, fmt.Errorf("privacy service is not configured")
	}
	after := cursor
	for {
		if s.budgetExhausted(start) {
			return false, nil
		}
		userIDs, err := s.repo.ListMembershipUserIDs(ctx, tenantID, after, tenantPurgeMembershipBatch)
		if err != nil {
			return false, fmt.Errorf("list tenant memberships: %w", err)
		}
		for _, userID := range userIDs {
			if s.budgetExhausted(start) {
				return false, nil
			}
			if err := s.privacy.EraseUserInTenant(ctx, tenantID, userID); err != nil {
				return false, fmt.Errorf("erase member %s: %w", userID, err)
			}
			// Invitations reference the membership through two RESTRICT FKs
			// (invited_by and accepted_by), so release them before the delete.
			if _, err := s.repo.DeleteMemberInvitations(ctx, tenantID, userID); err != nil {
				return false, fmt.Errorf("delete member %s invitations: %w", userID, err)
			}
			if err := s.repo.DeleteMembership(ctx, tenantID, userID); err != nil {
				return false, fmt.Errorf("delete member %s membership: %w", userID, err)
			}
			if err := s.repo.AdvancePurgeCursor(ctx, tenantID, userID); err != nil {
				return false, fmt.Errorf("persist tenant purge progress: %w", err)
			}
			after = userID
		}
		if len(userIDs) < tenantPurgeMembershipBatch {
			return true, nil
		}
	}
}

func (s *TenantPurgeService) budgetExhausted(start time.Time) bool {
	return s.clock().Sub(start) >= tenantPurgeTenantBudget
}

// clock is the budget clock; the constructor sets time.Now.
func (s *TenantPurgeService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// PurgeTenant implements tenancy.Purger for identity's own tenant-wide rows:
// export documents, invitations, memberships (with their sessions, refresh
// tokens, and role assignments), custom roles, and tenant settings. Every
// table is drained in bounded pages so a large tenant never loads in one
// statement; a failure leaves the deleted pages gone and the rest resumable.
func (s *TenantPurgeService) PurgeTenant(ctx context.Context, tenantID uuid.UUID) error {
	ctx = tenant.WithID(ctx, tenantID)
	if err := s.purgeDataExports(ctx, tenantID); err != nil {
		return err
	}
	if err := purgeInBatches(func() (int64, error) {
		return s.repo.DeleteInvitationsBatch(ctx, tenantID, tenancycontracts.PurgeBatchSize)
	}); err != nil {
		return fmt.Errorf("delete tenant invitations: %w", err)
	}
	if err := purgeInBatches(func() (int64, error) {
		return s.repo.DeleteMembershipsBatch(ctx, tenantID, tenancycontracts.PurgeBatchSize)
	}); err != nil {
		return fmt.Errorf("delete tenant memberships: %w", err)
	}
	if err := purgeInBatches(func() (int64, error) {
		return s.repo.DeleteTenantRolesBatch(ctx, tenantID, tenancycontracts.PurgeBatchSize)
	}); err != nil {
		return fmt.Errorf("delete tenant roles: %w", err)
	}
	// tenant_settings is keyed by tenant_id, so this affects at most one row.
	if err := s.repo.DeleteTenantSettings(ctx, tenantID); err != nil {
		return fmt.Errorf("delete tenant settings: %w", err)
	}
	return nil
}

// purgeDataExports deletes each page's blobs before its rows. A missing blob
// counts as deleted, so a retry after a partial run finishes the same page.
func (s *TenantPurgeService) purgeDataExports(ctx context.Context, tenantID uuid.UUID) error {
	for {
		exports, err := s.repo.ListDataExports(ctx, tenantID, tenancycontracts.PurgeBatchSize)
		if err != nil {
			return fmt.Errorf("list tenant data exports: %w", err)
		}
		if len(exports) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, len(exports))
		for i, export := range exports {
			ids[i] = export.ID
			if export.StoragePath == "" {
				continue
			}
			if err := s.storage.Delete(ctx, export.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("delete tenant data export %s: %w", export.ID, err)
			}
		}
		if err := s.repo.DeleteDataExports(ctx, tenantID, ids); err != nil {
			return fmt.Errorf("delete tenant data exports: %w", err)
		}
	}
}

// purgeInBatches repeats one bounded delete until it reports no rows.
func purgeInBatches(batch func() (int64, error)) error {
	for {
		deleted, err := batch()
		if err != nil {
			return err
		}
		if deleted == 0 {
			return nil
		}
	}
}

// HandleTrialExpiryJob suspends trialing tenants whose trial has ended. The
// write goes through TenantService.ExpireTrial so it is audited as a system
// status change and the state machine is re-checked under the row lock.
func (s *TenantPurgeService) HandleTrialExpiryJob(ctx context.Context, _ json.RawMessage) error {
	ctx = authz.SystemContext(ctx)
	ids, err := s.repo.ListExpiredTrialTenantIDs(ctx, tenantPurgeTrialBatch)
	if err != nil {
		return fmt.Errorf("list expired trials: %w", err)
	}
	var errs []error
	for _, id := range ids {
		changed, err := s.tenants.ExpireTrial(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("expire tenant %s trial: %w", id, err))
			continue
		}
		if changed {
			slog.Info("tenant trial expired", slog.String("tenant_id", id.String()))
		}
	}
	return errors.Join(errs...)
}

// HandleDeletionJob moves pending_deletion tenants whose grace period ended to
// deleted through the audited TenantService, so the hourly purge job can pick
// them up. Each transition is re-checked under the tenant row lock.
func (s *TenantPurgeService) HandleDeletionJob(ctx context.Context, _ json.RawMessage) error {
	ctx = authz.SystemContext(ctx)
	ids, err := s.repo.ListDueDeletionTenantIDs(ctx, tenantPurgeTrialBatch)
	if err != nil {
		return fmt.Errorf("list due deletions: %w", err)
	}
	var errs []error
	for _, id := range ids {
		changed, err := s.tenants.FinalizeDeletion(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("finalize tenant %s deletion: %w", id, err))
			continue
		}
		if changed {
			slog.Info("tenant deletion finalized", slog.String("tenant_id", id.String()))
		}
	}
	return errors.Join(errs...)
}

// SetNow replaces the clock used by the purge budgets. It is a test seam.
func (s *TenantPurgeService) SetNow(now func() time.Time) { s.now = now }

// Registry returns the module purger registry. It is a test seam.
func (s *TenantPurgeService) Registry() *tenancycontracts.Registry { return s.registry }

// SetRegistry replaces the module purger registry. It is a test seam.
func (s *TenantPurgeService) SetRegistry(registry *tenancycontracts.Registry) { s.registry = registry }
