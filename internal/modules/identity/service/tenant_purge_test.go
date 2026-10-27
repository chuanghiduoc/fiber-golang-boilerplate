package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/google/uuid"
)

type fakeTenantPurgeRepo struct {
	tenants     []PurgeTenant
	members     []uuid.UUID
	dataExports []DataExport
	invitations int64
	memberships int64
	roles       int64
	purged      []uuid.UUID
	events      []audit.Event
	batchSizes  []int32
	marked      bool
	listErr     error
	deleteErr   error
	// Cursor tracking for the resumable purge.
	cursors       map[uuid.UUID]uuid.UUID
	deletedMember []uuid.UUID
	dueDeletions  []uuid.UUID
	// membersByTenant overrides members for a multi-tenant run.
	membersByTenant map[uuid.UUID][]uuid.UUID
}

func (r *fakeTenantPurgeRepo) ListDeletedTenants(context.Context, int32) ([]PurgeTenant, error) {
	return r.tenants, r.listErr
}

func (r *fakeTenantPurgeRepo) ListDueDeletionTenantIDs(context.Context, int32) ([]uuid.UUID, error) {
	return r.dueDeletions, nil
}

func (r *fakeTenantPurgeRepo) ListExpiredTrialTenantIDs(context.Context, int32) ([]uuid.UUID, error) {
	return nil, nil
}

func (r *fakeTenantPurgeRepo) ListMembershipUserIDs(_ context.Context, tenantID, after uuid.UUID, _ int32) ([]uuid.UUID, error) {
	members := r.members
	if r.membersByTenant != nil {
		members = r.membersByTenant[tenantID]
	}
	var out []uuid.UUID
	for _, id := range members {
		if id.String() > after.String() {
			out = append(out, id)
		}
	}
	return out, nil
}

func (r *fakeTenantPurgeRepo) DeleteMemberInvitations(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
	return 0, nil
}

func (r *fakeTenantPurgeRepo) DeleteMembership(_ context.Context, tenantID, userID uuid.UUID) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	if r.membersByTenant != nil {
		members := r.membersByTenant[tenantID]
		for i, id := range members {
			if id == userID {
				r.membersByTenant[tenantID] = append(members[:i], members[i+1:]...)
				break
			}
		}
	} else {
		for i, id := range r.members {
			if id == userID {
				r.members = append(r.members[:i], r.members[i+1:]...)
				break
			}
		}
	}
	r.deletedMember = append(r.deletedMember, userID)
	return nil
}

func (r *fakeTenantPurgeRepo) AdvancePurgeCursor(_ context.Context, tenantID, cursor uuid.UUID) error {
	if r.cursors == nil {
		r.cursors = map[uuid.UUID]uuid.UUID{}
	}
	r.cursors[tenantID] = cursor
	return nil
}

func (r *fakeTenantPurgeRepo) ListDataExports(_ context.Context, _ uuid.UUID, limit int32) ([]DataExport, error) {
	if len(r.dataExports) == 0 {
		return nil, nil
	}
	n := int(limit)
	if n > len(r.dataExports) {
		n = len(r.dataExports)
	}
	return r.dataExports[:n], nil
}

func (r *fakeTenantPurgeRepo) DeleteDataExports(_ context.Context, _ uuid.UUID, ids []uuid.UUID) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.batchSizes = append(r.batchSizes, int32(len(ids)))
	r.dataExports = r.dataExports[len(ids):]
	return nil
}

func (r *fakeTenantPurgeRepo) DeleteInvitationsBatch(_ context.Context, _ uuid.UUID, limit int32) (int64, error) {
	return r.batchDelete(&r.invitations, limit)
}

func (r *fakeTenantPurgeRepo) DeleteMembershipsBatch(_ context.Context, _ uuid.UUID, limit int32) (int64, error) {
	return r.batchDelete(&r.memberships, limit)
}

func (r *fakeTenantPurgeRepo) DeleteTenantRolesBatch(_ context.Context, _ uuid.UUID, limit int32) (int64, error) {
	return r.batchDelete(&r.roles, limit)
}

func (r *fakeTenantPurgeRepo) DeleteTenantSettings(context.Context, uuid.UUID) error {
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

func (r *fakeTenantPurgeRepo) MarkPurged(_ context.Context, tenantID uuid.UUID, event audit.Event) (bool, error) {
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
	tenantID uuid.UUID
	calls    int
	err      error
	log      *[]string
}

func (p *fakePurger) PurgeTenant(ctx context.Context, tenantID uuid.UUID) error {
	p.calls++
	p.tenantID = tenantID
	if p.log != nil {
		*p.log = append(*p.log, p.name)
	}
	return p.err
}

type fakeEraser struct {
	userID uuid.UUID
	log    *[]string
	name   string
}

func (e *fakeEraser) Export(context.Context, uuid.UUID, uuid.UUID) (privacycontracts.Section, error) {
	return privacycontracts.Section{}, nil
}

func (e *fakeEraser) Erase(_ context.Context, _, userID uuid.UUID) error {
	e.userID = userID
	if e.log != nil {
		*e.log = append(*e.log, "erase:"+e.name)
	}
	return nil
}

func newPurgeTestService(t *testing.T) (purgeSvc *TenantPurgeService, purgeRepo *fakeTenantPurgeRepo, purgeOrder *[]string) {
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

	repo := &fakeTenantPurgeRepo{tenants: []PurgeTenant{{ID: testID(7), Slug: "gone"}}, members: []uuid.UUID{testID(10), testID(11)}}
	svc := NewTenantPurgeService(TenantPurgeDeps{
		Repo:     repo,
		Privacy:  NewPrivacyServiceWithRegistry(privacyRegistry),
		Registry: tenancyRegistry,
	})
	return svc, repo, &order
}

func TestTenantPurgeErasesMembersThenModulesThenIdentity(t *testing.T) {
	svc, repo, order := newPurgeTestService(t)
	require.NoError(t, svc.HandlePurgeJob(testTenantCtx(), nil))

	require.Len(t, *order, 6, "two members, files first, identity last")
	assert.Equal(t, []string{"erase:files", "erase:identity", "erase:files", "erase:identity", "files", "identity"}, *order)
	assert.Equal(t, []uuid.UUID{testID(7)}, repo.purged)
	require.Len(t, repo.events, 1)
	assert.Equal(t, "tenant.purged", repo.events[0].Action)
	assert.Equal(t, audit.ActorTypeSystem, repo.events[0].ActorType)
	assert.Equal(t, testID(7), repo.events[0].TenantID)
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
			ID: testID(uint64(i + 1)), TenantID: testID(9),
			StoragePath: fmt.Sprintf("9/exports/%d.json", i+1),
		}
	}
	store := &purgeBlobStore{}
	svc := NewTenantPurgeService(TenantPurgeDeps{Repo: repo, Storage: store})

	require.NoError(t, svc.PurgeTenant(testTenantCtx(), testID(9)))

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

// fakeClock makes the purge budgets deterministic without sleeping.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// budgetEraser advances the fake clock while erasing one tenant, so that
// tenant exceeds its per-tenant budget and the next one still gets a turn.
type budgetEraser struct {
	clock    *fakeClock
	by       time.Duration
	tenantID uuid.UUID
}

func (e *budgetEraser) Export(context.Context, uuid.UUID, uuid.UUID) (privacycontracts.Section, error) {
	return privacycontracts.Section{}, nil
}

func (e *budgetEraser) Erase(_ context.Context, tenantID, _ uuid.UUID) error {
	if tenantID == e.tenantID {
		e.clock.Advance(e.by)
	}
	return nil
}

func TestTenantPurgeResumesFromThePersistedCursor(t *testing.T) {
	privacyRegistry := privacycontracts.NewRegistry()
	files := &fakeEraser{name: "files"}
	privacyRegistry.Register(privacycontracts.Provider{Name: "files", Exporter: files, Eraser: files})
	privacyRegistry.Register(privacycontracts.Provider{Name: "identity", Exporter: &fakeEraser{name: "identity"}, Eraser: &fakeEraser{name: "identity"}})
	tenancyRegistry := tenancycontracts.NewRegistry()
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "files", Purger: &fakePurger{name: "files"}})
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "identity", Purger: &fakePurger{name: "identity"}})
	repo := &fakeTenantPurgeRepo{
		tenants: []PurgeTenant{{ID: testID(7), Slug: "gone", Cursor: testID(5)}},
		members: []uuid.UUID{testID(4), testID(6), testID(7)},
	}
	svc := NewTenantPurgeService(TenantPurgeDeps{
		Repo: repo, Privacy: NewPrivacyServiceWithRegistry(privacyRegistry), Registry: tenancyRegistry,
	})

	require.NoError(t, svc.HandlePurgeJob(testTenantCtx(), nil))

	assert.Equal(t, []uuid.UUID{testID(6), testID(7)}, repo.deletedMember, "users at or below the cursor are not revisited")
	assert.Equal(t, testID(7), repo.cursors[testID(7)])
	assert.Equal(t, []uuid.UUID{testID(7)}, repo.purged)
}

func TestTenantPurgeTenantBudgetLetsTheNextTenantRun(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	privacyRegistry := privacycontracts.NewRegistry()
	slow := &budgetEraser{clock: clock, by: 3 * time.Minute, tenantID: testID(1)}
	privacyRegistry.Register(privacycontracts.Provider{Name: "files", Exporter: slow, Eraser: slow})
	privacyRegistry.Register(privacycontracts.Provider{Name: "identity", Exporter: &fakeEraser{name: "identity"}, Eraser: &fakeEraser{name: "identity"}})
	tenancyRegistry := tenancycontracts.NewRegistry()
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "files", Purger: &fakePurger{name: "files"}})
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "identity", Purger: &fakePurger{name: "identity"}})
	repo := &fakeTenantPurgeRepo{
		tenants:         []PurgeTenant{{ID: testID(1), Slug: "slow"}, {ID: testID(2), Slug: "fast"}},
		membersByTenant: map[uuid.UUID][]uuid.UUID{testID(1): {testID(10)}, testID(2): {testID(20)}},
	}
	svc := NewTenantPurgeService(TenantPurgeDeps{
		Repo: repo, Privacy: NewPrivacyServiceWithRegistry(privacyRegistry), Registry: tenancyRegistry,
	})
	svc.SetNow(clock.Now)

	require.NoError(t, svc.HandlePurgeJob(testTenantCtx(), nil))

	assert.Equal(t, []uuid.UUID{testID(2)}, repo.purged, "the fast tenant is not starved by the slow one")
	assert.Equal(t, testID(10), repo.cursors[testID(1)], "the slow tenant keeps the progress it made")
	assert.Equal(t, testID(20), repo.cursors[testID(2)])
	assert.Equal(t, []uuid.UUID{testID(10), testID(20)}, repo.deletedMember)
}

func TestTenantDeletionJobFinalizesDueTenantsThroughTheService(t *testing.T) {
	db := newMemDB()
	past := time.Now().Add(-time.Minute)
	db.tenants[testID(2)].Status = TenantStatusPendingDeletion
	db.tenants[testID(2)].DeletionScheduledAt = &past
	repo := &fakeTenantPurgeRepo{dueDeletions: []uuid.UUID{testID(2)}}
	svc := NewTenantPurgeService(TenantPurgeDeps{
		Repo:    repo,
		Tenants: NewTenantService(db.stores(), memTx{db}),
	})

	require.NoError(t, svc.HandleDeletionJob(testTenantCtx(), nil))
	assert.Equal(t, TenantStatusDeleted, db.tenants[testID(2)].Status)
	require.Len(t, db.audits, 1)
	assert.Equal(t, "tenant.status_changed", db.audits[0].Action)
	assert.Equal(t, audit.ActorTypeSystem, db.audits[0].ActorType)
	assert.Equal(t, "deletion_due", db.audits[0].Metadata["reason"])
}

func TestTenantPurgeFailureLeavesTheTenantUnmarkedAndRetries(t *testing.T) {
	svc, repo, _ := newPurgeTestService(t)
	// Fail the first module purger so the completion marker is never written.
	providers := svc.Registry().Providers()
	providers[0].Purger.(*fakePurger).err = errors.New("storage unavailable")
	svc.SetRegistry(tenancycontracts.NewRegistry())
	for _, provider := range providers {
		svc.Registry().Register(provider)
	}

	err := svc.HandlePurgeJob(testTenantCtx(), nil)
	require.Error(t, err)
	assert.Empty(t, repo.purged, "a failed run must not mark the tenant purged")

	// The next delivery sees no work after a successful retry.
	providers[0].Purger.(*fakePurger).err = nil
	svc.SetRegistry(tenancycontracts.NewRegistry())
	for _, provider := range providers {
		svc.Registry().Register(provider)
	}
	require.NoError(t, svc.HandlePurgeJob(testTenantCtx(), nil))
	assert.Equal(t, []uuid.UUID{testID(7)}, repo.purged)
}

func TestTenantPurgeNeverTouchesAnotherTenant(t *testing.T) {
	privacyRegistry := privacycontracts.NewRegistry()
	eraser := &tenantRecorderEraser{}
	privacyRegistry.Register(privacycontracts.Provider{Name: "identity", Exporter: eraser, Eraser: eraser})
	tenancyRegistry := tenancycontracts.NewRegistry()
	purger := &tenantRecorderPurger{}
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "identity", Purger: purger})
	repo := &fakeTenantPurgeRepo{tenants: []PurgeTenant{{ID: testID(42), Slug: "gone"}}, members: []uuid.UUID{testID(5)}}
	svc := NewTenantPurgeService(TenantPurgeDeps{
		Repo: repo, Privacy: NewPrivacyServiceWithRegistry(privacyRegistry), Registry: tenancyRegistry,
	})

	require.NoError(t, svc.HandlePurgeJob(testTenantCtx(), nil))
	assert.Equal(t, []uuid.UUID{testID(42)}, eraser.tenants)
	assert.Equal(t, []uuid.UUID{testID(42)}, purger.tenants)
}

type tenantRecorderEraser struct{ tenants []uuid.UUID }

func (e *tenantRecorderEraser) Export(context.Context, uuid.UUID, uuid.UUID) (privacycontracts.Section, error) {
	return privacycontracts.Section{}, nil
}

func (e *tenantRecorderEraser) Erase(ctx context.Context, tenantID, _ uuid.UUID) error {
	e.tenants = append(e.tenants, tenant.IDFromContext(ctx))
	return nil
}

type tenantRecorderPurger struct{ tenants []uuid.UUID }

func (p *tenantRecorderPurger) PurgeTenant(ctx context.Context, _ uuid.UUID) error {
	p.tenants = append(p.tenants, tenant.IDFromContext(ctx))
	return nil
}
