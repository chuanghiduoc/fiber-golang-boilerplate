package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
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

	// tenantPurgeBatchSize bounds how many deleted tenants one run starts.
	tenantPurgeBatchSize = 25
	// tenantPurgeMembershipBatch bounds one membership page of a tenant.
	tenantPurgeMembershipBatch = 100
	// tenantPurgeTrialBatch bounds one expired-trial page.
	tenantPurgeTrialBatch = 100
)

// tenantPurgerName is identity's own tenant-wide purger. It runs last so the
// memberships and roles still exist while every other module cleans up.
const tenantPurgerName = "identity"

// PurgeTenant is one tenant waiting for its deleted data to be erased.
type PurgeTenant struct {
	ID   int64
	Slug string
}

// TenantPurgeRepository is the identity persistence the purge and hourly trial
// expiry jobs need. Every tenant-scoped method runs under the tenant context
// and therefore RLS; only the platform-wide tenant list is unscoped.
type TenantPurgeRepository interface {
	ListDeletedTenants(ctx context.Context, limit int32) ([]PurgeTenant, error)
	ListExpiredTrialTenantIDs(ctx context.Context, limit int32) ([]int64, error)
	ListMembershipUserIDs(ctx context.Context, tenantID, afterUserID int64, limit int32) ([]int64, error)
	ListDataExports(ctx context.Context, tenantID int64) ([]DataExport, error)
	DeleteDataExports(ctx context.Context, tenantID int64) error
	DeleteInvitations(ctx context.Context, tenantID int64) error
	DeleteMemberships(ctx context.Context, tenantID int64) error
	DeleteTenantRoles(ctx context.Context, tenantID int64) error
	DeleteTenantSettings(ctx context.Context, tenantID int64) error
	MarkPurged(ctx context.Context, tenantID int64, event audit.Event) (bool, error)
}

// TenantPurgeDeps are the purge service's collaborators.
type TenantPurgeDeps struct {
	Repo     TenantPurgeRepository
	Privacy  *PrivacyService
	Tenants  *TenantService
	Registry *tenancycontracts.Registry
	Storage  storage.Storage
}

// TenantPurgeService orchestrates the deleted-tenant purge and the hourly
// trial expiry. It also implements tenancy.Purger for identity's own
// tenant-wide rows.
type TenantPurgeService struct {
	repo     TenantPurgeRepository
	privacy  *PrivacyService
	tenants  *TenantService
	registry *tenancycontracts.Registry
	storage  storage.Storage
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
	}
}

// HandlePurgeJob purges every deleted tenant that has not completed yet. It is
// resumable: each phase is idempotent and purged_at is written only after every
// module succeeded, so a failed run retries the same tenant. One failing tenant
// does not stop the others; the joined error makes the job retry.
func (s *TenantPurgeService) HandlePurgeJob(ctx context.Context, _ json.RawMessage) error {
	ctx = authz.SystemContext(ctx)
	tenants, err := s.repo.ListDeletedTenants(ctx, tenantPurgeBatchSize)
	if err != nil {
		return fmt.Errorf("list deleted tenants: %w", err)
	}
	var errs []error
	for _, t := range tenants {
		if err := s.purgeDeletedTenant(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("tenant %d: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *TenantPurgeService) purgeDeletedTenant(ctx context.Context, t PurgeTenant) error {
	tctx := tenant.WithID(ctx, t.ID)
	if err := s.eraseMemberships(tctx, t.ID); err != nil {
		return err
	}
	if s.registry == nil {
		return fmt.Errorf("tenant purge registry is not configured")
	}
	providers := s.registry.Providers()
	identitySeen := false
	for _, provider := range providers {
		if provider.Name == tenantPurgerName {
			continue
		}
		if err := provider.Purger.PurgeTenant(tctx, t.ID); err != nil {
			return fmt.Errorf("purge %s: %w", provider.Name, err)
		}
	}
	for _, provider := range providers {
		if provider.Name != tenantPurgerName {
			continue
		}
		identitySeen = true
		if err := provider.Purger.PurgeTenant(tctx, t.ID); err != nil {
			return fmt.Errorf("purge %s: %w", provider.Name, err)
		}
	}
	if !identitySeen {
		return fmt.Errorf("identity tenant purger is not registered")
	}

	event := audit.NewServiceEvent(tctx, "tenant.purged", "tenant")
	event.TenantID = t.ID
	event.ResourceID = strconv.FormatInt(t.ID, 10)
	event.ActorType = audit.ActorTypeSystem
	event.Source = audit.SourceSystem
	marked, err := s.repo.MarkPurged(tctx, t.ID, event)
	if err != nil {
		return fmt.Errorf("mark tenant purged: %w", err)
	}
	if marked {
		slog.Info("deleted tenant purged", slog.Int64("tenant_id", t.ID), slog.String("tenant", t.Slug))
	}
	return nil
}

// eraseMemberships runs the privacy erasers for every membership of the
// tenant, including removed ones, in user-id pages. Identity's eraser only
// marks the membership removed; the final membership delete happens after the
// tenant-wide module purgers.
func (s *TenantPurgeService) eraseMemberships(ctx context.Context, tenantID int64) error {
	if s.privacy == nil {
		return fmt.Errorf("privacy service is not configured")
	}
	after := int64(0)
	for {
		userIDs, err := s.repo.ListMembershipUserIDs(ctx, tenantID, after, tenantPurgeMembershipBatch)
		if err != nil {
			return fmt.Errorf("list tenant memberships: %w", err)
		}
		for _, userID := range userIDs {
			if err := s.privacy.eraseUserInTenant(ctx, tenantID, userID); err != nil {
				return fmt.Errorf("erase member %d: %w", userID, err)
			}
		}
		if len(userIDs) < tenantPurgeMembershipBatch {
			return nil
		}
		after = userIDs[len(userIDs)-1]
	}
}

// PurgeTenant implements tenancy.Purger for identity's own tenant-wide rows:
// export documents, invitations, memberships (with their sessions, refresh
// tokens, and role assignments), and custom roles.
func (s *TenantPurgeService) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	exports, err := s.repo.ListDataExports(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("list tenant data exports: %w", err)
	}
	for _, export := range exports {
		if export.StoragePath == "" {
			continue
		}
		if err := s.storage.Delete(ctx, export.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("delete tenant data export %d: %w", export.ID, err)
		}
	}
	if err := s.repo.DeleteDataExports(ctx, tenantID); err != nil {
		return fmt.Errorf("delete tenant data exports: %w", err)
	}
	if err := s.repo.DeleteInvitations(ctx, tenantID); err != nil {
		return fmt.Errorf("delete tenant invitations: %w", err)
	}
	if err := s.repo.DeleteMemberships(ctx, tenantID); err != nil {
		return fmt.Errorf("delete tenant memberships: %w", err)
	}
	if err := s.repo.DeleteTenantRoles(ctx, tenantID); err != nil {
		return fmt.Errorf("delete tenant roles: %w", err)
	}
	if err := s.repo.DeleteTenantSettings(ctx, tenantID); err != nil {
		return fmt.Errorf("delete tenant settings: %w", err)
	}
	return nil
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
			errs = append(errs, fmt.Errorf("expire tenant %d trial: %w", id, err))
			continue
		}
		if changed {
			slog.Info("tenant trial expired", slog.Int64("tenant_id", id))
		}
	}
	return errors.Join(errs...)
}
