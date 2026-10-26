package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type fakeTenantPurgeRepo struct {
	tenants     []PurgeTenant
	members     []int64
	dataExports []DataExport
	invitations int64
	memberships int64
	roles       int64
	purged      []int64
	events      []audit.Event
	batchSizes  []int32
	marked      bool
	listErr     error
	deleteErr   error
}

func (r *fakeTenantPurgeRepo) ListDeletedTenants(context.Context, int32) ([]PurgeTenant, error) {
	return r.tenants, r.listErr
}

func (r *fakeTenantPurgeRepo) ListExpiredTrialTenantIDs(context.Context, int32) ([]int64, error) {
	return nil, nil
}

func (r *fakeTenantPurgeRepo) ListMembershipUserIDs(_ context.Context, _, after int64, _ int32) ([]int64, error) {
	var out []int64
	for _, id := range r.members {
		if id > after {
			out = append(out, id)
		}
	}
	return out, nil
}

func (r *fakeTenantPurgeRepo) ListDataExports(_ context.Context, _ int64, limit int32) ([]DataExport, error) {
	if len(r.dataExports) == 0 {
		return nil, nil
	}
	n := int(limit)
	if n > len(r.dataExports) {
		n = len(r.dataExports)
	}
	return r.dataExports[:n], nil
}

func (r *fakeTenantPurgeRepo) DeleteDataExports(_ context.Context, _ int64, ids []int64) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.batchSizes = append(r.batchSizes, int32(len(ids)))
	r.dataExports = r.dataExports[len(ids):]
	return nil
}

func (r *fakeTenantPurgeRepo) DeleteInvitationsBatch(_ context.Context, _ int64, limit int32) (int64, error) {
	return r.batchDelete(&r.invitations, limit)
}

func (r *fakeTenantPurgeRepo) DeleteMembershipsBatch(_ context.Context, _ int64, limit int32) (int64, error) {
	return r.batchDelete(&r.memberships, limit)
}

func (r *fakeTenantPurgeRepo) DeleteTenantRolesBatch(_ context.Context, _ int64, limit int32) (int64, error) {
	return r.batchDelete(&r.roles, limit)
}

func (r *fakeTenantPurgeRepo) DeleteTenantSettings(context.Context, int64) error {
	return nil
}

func (r *fakeTenantPurgeRepo) batchDelete(remaining *int64, limit int32) (int64, error) {
	if r.deleteErr != nil {
		return 0, r.deleteErr
	}
	r.batchSizes = append(r.batchSizes, limit)
	deleted := int64(limit)
	if *remaining < deleted {
		deleted = *remaining
	}
	*remaining -= deleted
	return deleted, nil
}

func (r *fakeTenantPurgeRepo) MarkPurged(_ context.Context, tenantID int64, event audit.Event) (bool, error) {
	if r.marked {
		return false, nil
	}
	r.marked = true
	r.purged = append(r.purged, tenantID)
	r.events = append(r.events, event)
	return true, nil
}

type fakePurger struct {
	name     string
	tenantID int64
	calls    int
	err      error
	log      *[]string
}

func (p *fakePurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	p.calls++
	p.tenantID = tenantID
	if p.log != nil {
		*p.log = append(*p.log, p.name)
	}
	return p.err
}

type fakeEraser struct {
	userID int64
	log    *[]string
	name   string
}

func (e *fakeEraser) Export(context.Context, int64, int64) (privacycontracts.Section, error) {
	return privacycontracts.Section{}, nil
}

func (e *fakeEraser) Erase(_ context.Context, _, userID int64) error {
	e.userID = userID
	if e.log != nil {
		*e.log = append(*e.log, "erase:"+e.name)
	}
	return nil
}

func newPurgeTestService(t *testing.T) (*TenantPurgeService, *fakeTenantPurgeRepo, *[]string) {
	t.Helper()
	var order []string
	privacyRegistry := privacycontracts.NewRegistry()
	privacyRegistry.Register(privacycontracts.Provider{
		Name: "files", Exporter: &fakeEraser{name: "files", log: &order}, Eraser: &fakeEraser{name: "files", log: &order},
	})
	privacyRegistry.Register(privacycontracts.Provider{
		Name: "identity", Exporter: &fakeEraser{name: "identity", log: &order}, Eraser: &fakeEraser{name: "identity", log: &order},
	})
	tenancyRegistry := tenancycontracts.NewRegistry()
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "files", Purger: &fakePurger{name: "files", log: &order}})
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "identity", Purger: &fakePurger{name: "identity", log: &order}})

	repo := &fakeTenantPurgeRepo{tenants: []PurgeTenant{{ID: 7, Slug: "gone"}}, members: []int64{10, 11}}
	svc := NewTenantPurgeService(TenantPurgeDeps{
		Repo:     repo,
		Privacy:  &PrivacyService{registry: privacyRegistry},
		Registry: tenancyRegistry,
	})
	return svc, repo, &order
}

func TestTenantPurgeErasesMembersThenModulesThenIdentity(t *testing.T) {
	svc, repo, order := newPurgeTestService(t)
	require.NoError(t, svc.HandlePurgeJob(context.Background(), nil))

	require.Len(t, *order, 6, "two members, files first, identity last")
	assert.Equal(t, []string{"erase:files", "erase:identity", "erase:files", "erase:identity", "files", "identity"}, *order)
	assert.Equal(t, []int64{7}, repo.purged)
	require.Len(t, repo.events, 1)
	assert.Equal(t, "tenant.purged", repo.events[0].Action)
	assert.Equal(t, audit.ActorTypeSystem, repo.events[0].ActorType)
	assert.Equal(t, int64(7), repo.events[0].TenantID)
}

// TestTenantPurgeDrainsTenantWideRowsInBoundedBatches proves identity's own
// purger never loads a whole table: every delete is bounded by PurgeBatchSize
// and repeats until the table is empty.
func TestTenantPurgeDrainsTenantWideRowsInBoundedBatches(t *testing.T) {
	const total = 1200
	repo := &fakeTenantPurgeRepo{
		invitations: total,
		memberships: total,
		roles:       total,
	}
	repo.dataExports = make([]DataExport, total)
	for i := range repo.dataExports {
		repo.dataExports[i] = DataExport{
			ID: int64(i + 1), TenantID: 9,
			StoragePath: fmt.Sprintf("9/exports/%d.json", i+1),
		}
	}
	store := &purgeBlobStore{}
	svc := NewTenantPurgeService(TenantPurgeDeps{Repo: repo, Storage: store})

	require.NoError(t, svc.PurgeTenant(context.Background(), 9))

	assert.Empty(t, repo.dataExports)
	assert.Zero(t, repo.invitations)
	assert.Zero(t, repo.memberships)
	assert.Zero(t, repo.roles)
	assert.Len(t, store.deleted, total, "every export blob is deleted before its row")
	require.NotEmpty(t, repo.batchSizes)
	for _, size := range repo.batchSizes {
		assert.LessOrEqual(t, size, tenancycontracts.PurgeBatchSize,
			"no delete page may exceed the shared batch bound")
	}
}

type purgeBlobStore struct {
	storage.Storage // unused methods fail loudly if called
	deleted         []string
}

func (s *purgeBlobStore) Delete(_ context.Context, path string) error {
	s.deleted = append(s.deleted, path)
	return nil
}

func TestTenantPurgeFailureLeavesTheTenantUnmarkedAndRetries(t *testing.T) {
	svc, repo, _ := newPurgeTestService(t)
	// Fail the first module purger so the completion marker is never written.
	providers := svc.registry.Providers()
	providers[0].Purger.(*fakePurger).err = errors.New("storage unavailable")
	svc.registry = tenancycontracts.NewRegistry()
	for _, provider := range providers {
		svc.registry.Register(provider)
	}

	err := svc.HandlePurgeJob(context.Background(), nil)
	require.Error(t, err)
	assert.Empty(t, repo.purged, "a failed run must not mark the tenant purged")

	// The next delivery sees no work after a successful retry.
	providers[0].Purger.(*fakePurger).err = nil
	svc.registry = tenancycontracts.NewRegistry()
	for _, provider := range providers {
		svc.registry.Register(provider)
	}
	require.NoError(t, svc.HandlePurgeJob(context.Background(), nil))
	assert.Equal(t, []int64{7}, repo.purged)
}

func TestTenantPurgeNeverTouchesAnotherTenant(t *testing.T) {
	privacyRegistry := privacycontracts.NewRegistry()
	eraser := &tenantRecorderEraser{}
	privacyRegistry.Register(privacycontracts.Provider{Name: "identity", Exporter: eraser, Eraser: eraser})
	tenancyRegistry := tenancycontracts.NewRegistry()
	purger := &tenantRecorderPurger{}
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "identity", Purger: purger})
	repo := &fakeTenantPurgeRepo{tenants: []PurgeTenant{{ID: 42, Slug: "gone"}}, members: []int64{5}}
	svc := NewTenantPurgeService(TenantPurgeDeps{
		Repo: repo, Privacy: &PrivacyService{registry: privacyRegistry}, Registry: tenancyRegistry,
	})

	require.NoError(t, svc.HandlePurgeJob(context.Background(), nil))
	assert.Equal(t, []int64{42}, eraser.tenants)
	assert.Equal(t, []int64{42}, purger.tenants)
}

type tenantRecorderEraser struct{ tenants []int64 }

func (e *tenantRecorderEraser) Export(context.Context, int64, int64) (privacycontracts.Section, error) {
	return privacycontracts.Section{}, nil
}

func (e *tenantRecorderEraser) Erase(ctx context.Context, tenantID, _ int64) error {
	e.tenants = append(e.tenants, tenant.EffectiveID(ctx))
	return nil
}

type tenantRecorderPurger struct{ tenants []int64 }

func (p *tenantRecorderPurger) PurgeTenant(ctx context.Context, _ int64) error {
	p.tenants = append(p.tenants, tenant.EffectiveID(ctx))
	return nil
}
