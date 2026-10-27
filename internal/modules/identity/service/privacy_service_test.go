package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/google/uuid"
)

type fakePrivacyRepo struct {
	exports           map[uuid.UUID]*DataExport
	nextID            int64
	sessions          []Session
	anonymized        []uuid.UUID
	deleted           []uuid.UUID
	anonymizeNotFound bool
	// invitationAnonymizations records each (user, replacement) call the
	// identity eraser and finalizer make.
	invitationAnonymizations []string
}

func newFakePrivacyRepo() *fakePrivacyRepo {
	return &fakePrivacyRepo{exports: map[uuid.UUID]*DataExport{}, nextID: 1}
}

func (r *fakePrivacyRepo) Create(_ context.Context, userID uuid.UUID) (*DataExport, error) {
	for _, e := range r.exports {
		if e.UserID == userID && (e.Status == DataExportQueued || e.Status == DataExportRunning) {
			return nil, ErrDataExportInProgress
		}
	}
	e := &DataExport{ID: testID(uint64(r.nextID)), TenantID: testID(1), UserID: userID, Status: DataExportQueued, CreatedAt: time.Now()}
	r.nextID++
	r.exports[e.ID] = e
	c := *e
	return &c, nil
}

func (r *fakePrivacyRepo) Get(_ context.Context, userID, id uuid.UUID) (*DataExport, error) {
	e, ok := r.exports[id]
	if !ok || e.UserID != userID {
		return nil, apperror.ErrNotFound
	}
	c := *e
	return &c, nil
}

func (r *fakePrivacyRepo) ListForUser(_ context.Context, userID uuid.UUID) ([]DataExport, error) {
	var out []DataExport
	for _, e := range r.exports {
		if e.UserID == userID {
			out = append(out, *e)
		}
	}
	return out, nil
}

func (r *fakePrivacyRepo) ListCursor(context.Context, uuid.UUID, pagination.DBCursor) ([]DataExport, error) {
	return nil, nil
}

func (r *fakePrivacyRepo) MarkRunning(_ context.Context, userID, id uuid.UUID) (bool, error) {
	e, ok := r.exports[id]
	if !ok || e.UserID != userID {
		return false, nil
	}
	if e.Status != DataExportQueued && e.Status != DataExportFailed && e.Status != DataExportRunning {
		return false, nil
	}
	e.Status = DataExportRunning
	return true, nil
}

func (r *fakePrivacyRepo) MarkReady(_ context.Context, userID, id uuid.UUID, path string, expires time.Time) (bool, error) {
	e, ok := r.exports[id]
	if !ok || e.UserID != userID || e.Status != DataExportRunning {
		return false, nil
	}
	e.Status, e.StoragePath, e.ExpiresAt = DataExportReady, path, &expires
	return true, nil
}

func (r *fakePrivacyRepo) MarkFailed(_ context.Context, userID, id uuid.UUID) error {
	if e, ok := r.exports[id]; ok && e.UserID == userID {
		e.Status = DataExportFailed
	}
	return nil
}

func (r *fakePrivacyRepo) ExpireForUser(_ context.Context, userID uuid.UUID) ([]DataExport, error) {
	var out []DataExport
	for _, e := range r.exports {
		if e.UserID == userID && e.Status != DataExportExpired {
			e.Status = DataExportExpired
			now := time.Now()
			e.ExpiresAt = &now
			out = append(out, *e)
		}
	}
	return out, nil
}

func (r *fakePrivacyRepo) DeleteForUser(_ context.Context, userID uuid.UUID) error {
	for id, e := range r.exports {
		if e.UserID == userID {
			delete(r.exports, id)
		}
	}
	return nil
}

func (r *fakePrivacyRepo) Purgeable(context.Context, int32) ([]DataExport, error) { return nil, nil }
func (r *fakePrivacyRepo) Delete(_ context.Context, id uuid.UUID) error {
	delete(r.exports, id)
	return nil
}

func (r *fakePrivacyRepo) ListSessions(context.Context, uuid.UUID) ([]Session, error) {
	return r.sessions, nil
}

func (r *fakePrivacyRepo) AnonymizeUser(_ context.Context, userID uuid.UUID, _ string) (*User, error) {
	if r.anonymizeNotFound {
		return nil, apperror.ErrNotFound
	}
	r.anonymized = append(r.anonymized, userID)
	return &User{ID: userID, Status: UserStatusDeleted}, nil
}

func (r *fakePrivacyRepo) DeleteCredentialsAndIdentities(_ context.Context, userID uuid.UUID) error {
	r.deleted = append(r.deleted, userID)
	return nil
}

func (r *fakePrivacyRepo) AnonymizeErasedUserInvitations(_ context.Context, userID uuid.UUID, replacement string) (int64, error) {
	r.invitationAnonymizations = append(r.invitationAnonymizations, fmt.Sprintf("%s:%s", userID, replacement))
	return 0, nil
}

func (r *fakePrivacyRepo) AnonymizeErasedUserInvitationsAllTenants(_ context.Context, userID uuid.UUID, replacement string) (int64, error) {
	r.invitationAnonymizations = append(r.invitationAnonymizations, fmt.Sprintf("all:%s:%s", userID, replacement))
	return 0, nil
}

func (r *fakePrivacyRepo) DeleteSessionsInTenant(context.Context, uuid.UUID) error { return nil }
func (r *fakePrivacyRepo) DeleteSessionsForUser(context.Context, uuid.UUID) error  { return nil }

type fakeJobs struct {
	mu   sync.Mutex
	jobs []jobs.Job
}

func (j *fakeJobs) Enqueue(_ context.Context, job jobs.Job) (uuid.UUID, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jobs = append(j.jobs, job)
	return testID(uint64(len(j.jobs))), nil
}

type fakePrivateTx struct {
	stores Stores
	jobs   *fakeJobs
	audits []audit.Event
}

func (t *fakePrivateTx) InTx(ctx context.Context, fn func(context.Context, TxRepos) error) error {
	return fn(ctx, TxRepos{Stores: t.stores, Audit: fakePrivateAudit{tx: t}, Jobs: t.jobs})
}

type fakePrivateAudit struct{ tx *fakePrivateTx }

func (a fakePrivateAudit) RecordTx(_ context.Context, event audit.Event) error {
	a.tx.audits = append(a.tx.audits, event)
	return nil
}

type fakeEvents struct {
	got []events.Event
}

func (e *fakeEvents) Publish(_ context.Context, event events.Event) error {
	e.got = append(e.got, event)
	return nil
}

type fakeExporter struct{ section privacycontracts.Section }

func (e fakeExporter) Export(context.Context, uuid.UUID, uuid.UUID) (privacycontracts.Section, error) {
	return e.section, nil
}
func (e fakeExporter) Erase(context.Context, uuid.UUID, uuid.UUID) error { return nil }

func privacyTestStores(repo *fakePrivacyRepo) (Stores, *memDB) {
	db := newMemDB()
	db.users[testID(10)] = &User{ID: testID(10), Email: "privacy@example.com", Name: "Privacy", Status: UserStatusActive}
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}] = &Membership{
		ID: testID(100), TenantID: tenant.DefaultID, UserID: testID(10), Role: RoleMember,
		Status: MembershipStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	stores := db.stores()
	stores.Privacy = repo
	return stores, db
}

// adminCallerCtx adds an active admin (user 1) to the request tenant and
// returns a context carrying that admin as the subject: admin erasure follows
// the role hierarchy, so the caller must outrank the target.
func adminCallerCtx(db *memDB) context.Context {
	db.users[testID(1)] = &User{ID: testID(1), Email: "admin@example.com", Name: "Admin", Status: UserStatusActive}
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(1)}] = &Membership{
		ID: testID(1), TenantID: tenant.DefaultID, UserID: testID(1), Role: RoleAdmin, Roles: []string{RoleAdmin},
		Status: MembershipStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	return authz.WithSubject(ctx, roleSubject(testID(1), tenant.DefaultID, RoleAdmin))
}

func newPrivacyTestService(t *testing.T, repo *fakePrivacyRepo, registry *privacycontracts.Registry) (resultSvc *PrivacyService, resultJobs *fakeJobs, resultEvents *fakeEvents, resultStore storage.Storage, resultStores Stores) {
	t.Helper()
	stores, _ := privacyTestStores(repo)
	tx := &fakePrivateTx{stores: stores, jobs: &fakeJobs{}}
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	notifier := &fakeEvents{}
	svc := NewPrivacyService(PrivacyDeps{
		Stores: stores, Tx: tx, Storage: store, Registry: registry, Events: notifier,
	})
	return svc, tx.jobs, notifier, store, stores
}

func TestIdentityExportListsPasskeysWithoutKeyMaterial(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	lastUsed := time.Now().Add(-time.Hour)
	db.passkeys[testID(55)] = &Passkey{
		ID: testID(55), UserID: testID(10), Name: "Laptop",
		CredentialID: []byte("credential-id-secret"), PublicKey: []byte("public-key-material"),
		CreatedAt: time.Now().Add(-24 * time.Hour), LastUsedAt: &lastUsed,
	}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores})
	section, err := svc.Export(testTenantCtx(), tenant.DefaultID, testID(10))
	require.NoError(t, err)

	var out struct {
		Passkeys []struct {
			Name       string     `json:"name"`
			CreatedAt  time.Time  `json:"createdAt"`
			LastUsedAt *time.Time `json:"lastUsedAt"`
		} `json:"passkeys"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.Passkeys, 1)
	assert.Equal(t, "Laptop", out.Passkeys[0].Name)
	assert.Equal(t, lastUsed.Unix(), out.Passkeys[0].LastUsedAt.Unix())
	lowered := strings.ToLower(string(section.Data))
	assert.NotContains(t, lowered, "publickey")
	assert.NotContains(t, lowered, "credentialid")
}

func TestCleanupUserDeletesPasskeys(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	db.passkeys[testID(56)] = &Passkey{ID: testID(56), UserID: testID(10), Name: "Phone"}
	tx := &fakePrivateTx{stores: stores, jobs: &fakeJobs{}}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx})
	require.NoError(t, tx.InTx(testTenantCtx(), func(ctx context.Context, tx TxRepos) error {
		_, err := svc.CleanupUser(ctx, tx, testID(10))
		return err
	}))
	assert.Empty(t, db.passkeys)
}

func TestRequestExportCreatesRowAndJob(t *testing.T) {
	repo := newFakePrivacyRepo()
	svc, jobQueue, _, _, _ := newPrivacyTestService(t, repo, privacycontracts.NewRegistry())
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	export, err := svc.RequestExport(ctx, testID(10))
	require.NoError(t, err)
	assert.Equal(t, DataExportQueued, export.Status)
	require.Len(t, jobQueue.jobs, 1)
	assert.Equal(t, JobKindDataExport, jobQueue.jobs[0].Kind)
	var payload ExportJob
	require.NoError(t, json.Unmarshal(jobQueue.jobs[0].Payload, &payload))
	assert.Equal(t, export.ID, payload.ExportID)
	assert.Equal(t, testID(10), payload.UserID)
}

func TestRequestExportConflictIs409(t *testing.T) {
	repo := newFakePrivacyRepo()
	svc, _, _, _, _ := newPrivacyTestService(t, repo, privacycontracts.NewRegistry())
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	_, err := svc.RequestExport(ctx, testID(10))
	require.NoError(t, err)
	_, err = svc.RequestExport(ctx, testID(10))
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 409, resolvedStatus(appErr))
	assert.Equal(t, string(identitycatalog.CodeDataExportInProgress), appErr.Code)
}

func TestExportJobWritesBlobMarksReadyAndNotifies(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(testTenantCtx(), testID(10))
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{
		Name:     "files",
		Exporter: fakeExporter{privacycontracts.Section{Name: "files", Data: json.RawMessage(`{"n":1}`)}},
		Eraser:   fakeExporter{},
	})
	svc, _, notifier, store, _ := newPrivacyTestService(t, repo, registry)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	require.NoError(t, svc.HandleExportJob(ctx, mustJSON(ExportJob{ExportID: e.ID, UserID: testID(10)})))

	got, _ := repo.Get(testTenantCtx(), testID(10), e.ID)
	require.Equal(t, DataExportReady, got.Status)
	require.NotEmpty(t, got.StoragePath)
	require.NotNil(t, got.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(DataExportTTL), *got.ExpiresAt, time.Minute)
	reader, err := store.Get(testTenantCtx(), got.StoragePath)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	var doc ExportDocument
	require.NoError(t, json.NewDecoder(reader).Decode(&doc))
	var section map[string]int
	require.NoError(t, json.Unmarshal(doc.Sections["files"], &section))
	assert.Equal(t, 1, section["n"])
	require.Len(t, notifier.got, 1)
	assert.Equal(t, identitycontracts.EventDataExportReady, notifier.got[0].Type)
	assert.Equal(t, tenant.DefaultID, notifier.got[0].TenantID)
	assert.Equal(t, events.UserActor(testID(10)), notifier.got[0].Actor)
	var ready identitycontracts.DataExportReady
	require.NoError(t, json.Unmarshal(notifier.got[0].Data, &ready))
	assert.Equal(t, e.ID, ready.ExportID)
	assert.Equal(t, testID(10), ready.UserID)

	// At-least-once redelivery is a no-op and does not notify twice.
	require.NoError(t, svc.HandleExportJob(ctx, mustJSON(ExportJob{ExportID: e.ID, UserID: testID(10)})))
	assert.Len(t, notifier.got, 1)
}

func TestExportJobSkipsAnErasedExport(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(testTenantCtx(), testID(10))
	svc, _, notifier, _, _ := newPrivacyTestService(t, repo, privacycontracts.NewRegistry())
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	_, err := repo.ExpireForUser(ctx, testID(10))
	require.NoError(t, err)

	require.NoError(t, svc.HandleExportJob(ctx, mustJSON(ExportJob{ExportID: e.ID, UserID: testID(10)})))
	got, _ := repo.Get(testTenantCtx(), testID(10), e.ID)
	assert.Equal(t, DataExportExpired, got.Status)
	assert.Empty(t, got.StoragePath)
	assert.Empty(t, notifier.got)
}

func TestIdentityExportIncludesProfileMembershipAndSessions(t *testing.T) {
	repo := newFakePrivacyRepo()
	repo.sessions = []Session{{ID: testID(7), TenantID: tenant.DefaultID, UserID: testID(10), UserAgent: "agent", CreatedAt: time.Now()}}
	stores, _ := privacyTestStores(repo)
	svc := NewPrivacyService(PrivacyDeps{Stores: stores})
	section, err := svc.Export(testTenantCtx(), tenant.DefaultID, testID(10))
	require.NoError(t, err)
	assert.Equal(t, "identity", section.Name)

	var out struct {
		Profile struct {
			Email string `json:"email"`
		} `json:"profile"`
		Membership struct {
			Role string `json:"role"`
		} `json:"membership"`
		Sessions []struct {
			ID uuid.UUID `json:"id"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	assert.Equal(t, "privacy@example.com", out.Profile.Email)
	assert.Equal(t, RoleMember, out.Membership.Role)
	require.Len(t, out.Sessions, 1)
	assert.Equal(t, testID(7), out.Sessions[0].ID)
	assert.NotContains(t, strings.ToLower(string(section.Data)), "password")
}

// An admin erasure is a per-tenant operation: it touches only the request
// tenant, never the global account or another membership.
func TestRequestAdminErasureStaysInTheRequestTenant(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	acme := testID(2)
	db.memberships[[2]uuid.UUID{acme, testID(10)}] = &Membership{
		ID: testID(101), TenantID: acme, UserID: testID(10), Role: RoleMember,
		Status: MembershipStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	registry := privacycontracts.NewRegistry()
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: store, Registry: registry})
	registry.Register(privacycontracts.Provider{Name: "identity", Exporter: svc, Eraser: svc})
	registry.Register(privacycontracts.Provider{Name: "files", Exporter: fakeExporter{}, Eraser: fakeExporter{}})
	ctx := adminCallerCtx(db)

	require.NoError(t, svc.RequestAdminErasure(ctx, testID(10)))
	require.Len(t, jobsQueue.jobs, 1, "exactly one tenant erase job")
	assert.Equal(t, JobKindErase, jobsQueue.jobs[0].Kind)
	assert.Equal(t, tenant.DefaultID, jobsQueue.jobs[0].TenantID)

	require.NoError(t, svc.HandleEraseJob(ctx, mustJSON(EraseJob{UserID: testID(10)})))
	assert.Equal(t, MembershipStatusRemoved, db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Status)
	assert.Equal(t, MembershipStatusActive, db.memberships[[2]uuid.UUID{acme, testID(10)}].Status,
		"the other tenant's membership must stay intact")
	assert.Empty(t, repo.anonymized, "admin erasure must not anonymize the global user")
	require.Len(t, tx.audits, 1)
	assert.Equal(t, "admin.user.erasure_requested", tx.audits[0].Action)
}

func TestRequestAdminErasureRejectsACrossTenantID(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	delete(db.memberships, [2]uuid.UUID{tenant.DefaultID, testID(10)})
	db.memberships[[2]uuid.UUID{testID(2), testID(10)}] = &Membership{
		ID: testID(101), TenantID: testID(2), UserID: testID(10), Role: RoleMember,
		Status: MembershipStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: failingPutStorage{}})

	err := svc.RequestAdminErasure(adminCallerCtx(db), testID(10))
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, resolvedStatus(appErr))
	assert.Empty(t, jobsQueue.jobs)
}

func TestRequestAdminErasureRejectsARemovedMember(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Status = MembershipStatusRemoved
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: failingPutStorage{}})

	err := svc.RequestAdminErasure(adminCallerCtx(db), testID(10))
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, resolvedStatus(appErr))
	assert.Empty(t, jobsQueue.jobs)
}

// Admin erasure follows the role hierarchy: an admin cannot erase an owner.
func TestRequestAdminErasureRequiresAHigherRank(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Role = RoleOwner
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Roles = []string{RoleOwner}
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: failingPutStorage{}})

	err := svc.RequestAdminErasure(adminCallerCtx(db), testID(10))
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appErr.Code)
	assert.Empty(t, jobsQueue.jobs)
}

func TestRequestAdminErasureRefusesTheLastOwner(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Role = RoleOwner
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Roles = []string{RoleOwner}
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: failingPutStorage{}})

	// The sole owner targets themselves through the admin route: an owner may
	// manage an owner, so only the last-owner rule can refuse it.
	ownerCtx := authz.WithSubject(tenant.WithID(context.Background(), tenant.DefaultID),
		roleSubject(testID(10), tenant.DefaultID, RoleOwner))
	err := svc.RequestAdminErasure(ownerCtx, testID(10))
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(identitycatalog.CodeUserLastAdmin), appErr.Code)
	assert.Empty(t, jobsQueue.jobs)
}

// A failing module eraser must leave identity's membership in place: the
// finalizer then keeps refusing to anonymize the global user.
func TestEraseJobRunsIdentityLastSoAFailureKeepsTheMembership(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	registry := privacycontracts.NewRegistry()
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: store, Registry: registry})
	registry.Register(privacycontracts.Provider{Name: "identity", Exporter: svc, Eraser: svc})
	registry.Register(privacycontracts.Provider{
		Name: "files", Exporter: fakeExporter{},
		Eraser: failingEraser{err: errors.New("storage outage")},
	})
	markReserved(db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}])
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	err = svc.HandleEraseJob(ctx, mustJSON(EraseJob{UserID: testID(10)}))
	require.Error(t, err)
	assert.Equal(t, MembershipStatusSuspended, db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Status,
		"identity must not remove the membership when another module failed")

	err = svc.HandleFinalizeJob(testTenantCtx(), mustJSON(FinalizeJob{UserID: testID(10)}))
	assert.ErrorIs(t, err, ErrErasurePending)
	assert.Empty(t, repo.anonymized)
}

// A reserved membership can be reactivated only outside the normal flow (the
// unban query refuses an erasure reservation); the job re-checks the last
// owner under the same per-tenant lock before running any eraser.
func TestEraseJobRechecksTheLastOwnerForAReactivatedMember(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	m := db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}]
	m.Role = RoleOwner
	m.Roles = []string{RoleOwner}
	markReserved(m)
	m.Status = MembershipStatusActive
	state := &txState{}
	stores.Memberships = lockRecordingMemberships{memMemberships: memMemberships{db}, state: state}
	tx := txFlaggingTransactor{state: state, inner: &fakePrivateTx{stores: stores, jobs: &fakeJobs{}}}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: failingPutStorage{}})

	err := svc.HandleEraseJob(tenant.WithID(context.Background(), tenant.DefaultID), mustJSON(EraseJob{UserID: testID(10)}))
	require.Error(t, err)
	assert.True(t, jobs.IsPermanent(err), "a job-time last-owner refusal is permanent")
	assert.True(t, state.lockedInTx, "the last-owner lock must be taken inside the check transaction")
}

// The request transaction reserves each membership (suspended, no ban
// markers) and revokes its sessions before it commits, so the account cannot
// act while the erase jobs are pending.
func TestRequestErasureReservesMembershipsAndRevokesSessions(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	acme := testID(2)
	db.memberships[[2]uuid.UUID{acme, testID(10)}] = &Membership{
		ID: testID(101), TenantID: acme, UserID: testID(10), Role: RoleMember, Roles: []string{RoleMember},
		Status: MembershipStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	db.users[testID(10)] = &User{ID: testID(10), Email: "privacy@example.com", Name: "Privacy", Status: UserStatusActive}
	db.sessions[testID(1)] = &Session{ID: testID(1), TenantID: tenant.DefaultID, UserID: testID(10)}
	db.sessions[testID(2)] = &Session{ID: testID(2), TenantID: acme, UserID: testID(10)}
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx})

	require.NoError(t, svc.RequestErasure(testTenantCtx(), testID(10), "user.erasure_requested"))

	for _, tenantID := range []uuid.UUID{tenant.DefaultID, acme} {
		m := db.memberships[[2]uuid.UUID{tenantID, testID(10)}]
		assert.Equal(t, MembershipStatusSuspended, m.Status)
		require.NotNil(t, m.ErasureRequestedAt, "the membership must be reserved")
		assert.Nil(t, m.BannedAt, "erasure is not a ban")
		assert.Empty(t, m.BannedEmailHash)
	}
	for id, session := range db.sessions {
		assert.NotNil(t, session.RevokedAt, "session %d must be revoked", id)
	}
	require.Len(t, jobsQueue.jobs, 3, "one erase job per membership plus the finalizer")
}

// A second owner's self-erasure sees the first owner's reservation and is
// refused before it can suspend itself or queue anything.
func TestSecondOwnerSelfErasureIsRefusedUpFront(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	db.users[testID(20)] = &User{ID: testID(20), Email: "other-owner@example.com", Name: "Other", Status: UserStatusActive}
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Role = RoleOwner
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Roles = []string{RoleOwner}
	db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(20)}] = &Membership{
		ID: testID(101), TenantID: tenant.DefaultID, UserID: testID(20), Role: RoleOwner, Roles: []string{RoleOwner},
		Status: MembershipStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	jobsQueue := &fakeJobs{}
	tx := &fakePrivateTx{stores: stores, jobs: jobsQueue}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx})

	require.NoError(t, svc.RequestErasure(testTenantCtx(), testID(10), "user.erasure_requested"))

	err := svc.RequestErasure(testTenantCtx(), testID(20), "user.erasure_requested")
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 409, resolvedStatus(appErr))
	assert.Equal(t, string(identitycatalog.CodeUserLastAdmin), appErr.Code)

	assert.Equal(t, MembershipStatusSuspended, db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Status)
	second := db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(20)}]
	assert.Equal(t, MembershipStatusActive, second.Status)
	assert.Nil(t, second.ErasureRequestedAt)
	assert.Len(t, jobsQueue.jobs, 2, "the refused request queues nothing")
}

func TestIdentityEraserAnonymizesInvitationAddresses(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, _ := privacyTestStores(repo)
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: &fakePrivateTx{stores: stores}})

	require.NoError(t, svc.Erase(testTenantCtx(), tenant.DefaultID, testID(10)))
	assert.Contains(t, repo.invitationAnonymizations, testID(10).String()+":erased+"+testID(10).String()+"@invalid")
}

func TestFinalizerAnonymizesInvitationAddresses(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	for key, m := range db.memberships {
		m.Status = MembershipStatusRemoved
		db.memberships[key] = m
	}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: &fakePrivateTx{stores: stores}})
	require.NoError(t, svc.HandleFinalizeJob(testTenantCtx(), mustJSON(FinalizeJob{UserID: testID(10)})))
	assert.Contains(t, repo.invitationAnonymizations, "all:"+testID(10).String()+":erased+"+testID(10).String()+"@invalid")
	assert.Equal(t, []uuid.UUID{testID(10)}, repo.anonymized)
}

// The finalizer must never see a removed membership without an identity
// eraser: without one the admin path would report success and leave the
// membership, sessions, and exports in place.
func TestEraseJobFailsWhenTheIdentityEraserIsMissing(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{Name: "files", Exporter: fakeExporter{}, Eraser: fakeExporter{}})
	markReserved(db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}])
	svc := NewPrivacyService(PrivacyDeps{
		Stores: stores, Tx: &fakePrivateTx{stores: stores, jobs: &fakeJobs{}},
		Storage: failingPutStorage{}, Registry: registry,
	})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	err := svc.HandleEraseJob(ctx, mustJSON(EraseJob{UserID: testID(10)}))
	require.Error(t, err)
	assert.Equal(t, MembershipStatusSuspended, db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Status)
}

func TestErasureJobIsIdempotentAndDeletesExports(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(testTenantCtx(), testID(10))
	path := "1/" + testID(10).String() + "/exports/x.json"
	expires := time.Now().Add(time.Hour)
	stored := repo.exports[e.ID]
	stored.Status, stored.StoragePath, stored.ExpiresAt = DataExportReady, path, &expires
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Put(testTenantCtx(), path, strings.NewReader("{}"), 2, "application/json"))

	stores, db := privacyTestStores(repo)
	registry := privacycontracts.NewRegistry()
	tx := &fakePrivateTx{stores: stores}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: store, Registry: registry})
	registry.Register(privacycontracts.Provider{Name: "identity", Exporter: svc, Eraser: svc})
	registry.Register(privacycontracts.Provider{Name: "files", Exporter: fakeExporter{}, Eraser: fakeExporter{}})
	markReserved(db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}])
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	// The erasure request pre-expires exports; the eraser must still delete
	// the already-expired document's blob.
	_, err = repo.ExpireForUser(ctx, testID(10))
	require.NoError(t, err)
	require.NoError(t, svc.HandleEraseJob(ctx, mustJSON(EraseJob{UserID: testID(10)})))
	require.NoError(t, svc.HandleEraseJob(ctx, mustJSON(EraseJob{UserID: testID(10)})))
	assert.Equal(t, MembershipStatusRemoved, db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Status)
	_, err = store.Get(testTenantCtx(), path)
	assert.ErrorIs(t, err, storage.ErrNotFound)
	assert.Empty(t, repo.exports)
}

func TestFinalizeWaitsUntilEveryMembershipIsRemoved(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	acme := testID(2)
	db.memberships[[2]uuid.UUID{acme, testID(10)}] = &Membership{ID: testID(101), TenantID: acme, UserID: testID(10), Role: RoleMember, Status: MembershipStatusRemoved}
	tx := &fakePrivateTx{stores: stores}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx})
	err := svc.HandleFinalizeJob(testTenantCtx(), mustJSON(FinalizeJob{UserID: testID(10)}))
	assert.ErrorIs(t, err, ErrErasurePending)
	assert.Empty(t, repo.anonymized)
}

func TestFinalizeAnonymizesWhenAllMembershipsAreRemoved(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	for key, m := range db.memberships {
		m.Status = MembershipStatusRemoved
		db.memberships[key] = m
	}
	tx := &fakePrivateTx{stores: stores}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx})
	require.NoError(t, svc.HandleFinalizeJob(testTenantCtx(), mustJSON(FinalizeJob{UserID: testID(10)})))
	assert.Equal(t, []uuid.UUID{testID(10)}, repo.anonymized)
	assert.Equal(t, []uuid.UUID{testID(10)}, repo.deleted)
	require.Len(t, tx.audits, 1)
	assert.Equal(t, "user.erased", tx.audits[0].Action)
}

func TestFinalizerSweepsAnExportCreatedBeforeFinalization(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	for key, m := range db.memberships {
		m.Status = MembershipStatusRemoved
		db.memberships[key] = m
	}
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: &fakePrivateTx{stores: stores}, Storage: store})

	e, err := repo.Create(testTenantCtx(), testID(10))
	require.NoError(t, err)
	path := "1/testID(10)/exports/late.json"
	expires := time.Now().Add(time.Hour)
	stored := repo.exports[e.ID]
	stored.Status, stored.StoragePath, stored.ExpiresAt = DataExportReady, path, &expires
	require.NoError(t, store.Put(testTenantCtx(), path, strings.NewReader("{}"), 2, "application/json"))

	require.NoError(t, svc.HandleFinalizeJob(testTenantCtx(), mustJSON(FinalizeJob{UserID: testID(10)})))
	assert.Empty(t, repo.exports)
	_, err = store.Get(testTenantCtx(), path)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestExportJobFailureMarksRowFailedAndUnblocksARequest(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(testTenantCtx(), testID(10))
	stores, _ := privacyTestStores(repo)
	svc := NewPrivacyService(PrivacyDeps{
		Stores: stores, Tx: &fakePrivateTx{stores: stores, jobs: &fakeJobs{}}, Storage: failingPutStorage{},
	})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	err := svc.HandleExportJob(ctx, mustJSON(ExportJob{ExportID: e.ID, UserID: testID(10)}))
	require.Error(t, err)
	got, _ := repo.Get(testTenantCtx(), testID(10), e.ID)
	assert.Equal(t, DataExportFailed, got.Status)

	_, err = svc.RequestExport(ctx, testID(10))
	require.NoError(t, err, "a failed export must not keep the active index forever")
}

func TestExportJobPanicMarksRowFailed(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(testTenantCtx(), testID(10))
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{
		Name: "panic", Exporter: panickingExporter{}, Eraser: fakeExporter{},
	})
	svc, _, _, _, _ := newPrivacyTestService(t, repo, registry)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	func() {
		defer func() { _ = recover() }()
		_ = svc.HandleExportJob(ctx, mustJSON(ExportJob{ExportID: e.ID, UserID: testID(10)}))
	}()
	got, _ := repo.Get(testTenantCtx(), testID(10), e.ID)
	assert.Equal(t, DataExportFailed, got.Status)
}

func TestEraseJobRefusesAReactivatedLastOwnerBeforeAnyEraser(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	m := db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}]
	m.Role = RoleOwner
	m.Roles = []string{RoleOwner}
	markReserved(m)
	m.Status = MembershipStatusActive
	eraser := &recordingEraser{}
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{Name: "files", Exporter: fakeExporter{}, Eraser: eraser})
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: &fakePrivateTx{stores: stores}, Registry: registry})

	err := svc.HandleEraseJob(tenant.WithID(context.Background(), tenant.DefaultID), mustJSON(EraseJob{UserID: testID(10)}))
	require.Error(t, err)
	assert.True(t, jobs.IsPermanent(err))
	assert.False(t, eraser.called)
}

// A job for a live membership that no request transaction reserved is a
// permanent refusal: retrying can never create the missing request.
func TestEraseJobPermanentlyRefusesAnUnreservedMembership(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	eraser := &recordingEraser{}
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{Name: "files", Exporter: fakeExporter{}, Eraser: eraser})
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: &fakePrivateTx{stores: stores}, Registry: registry})

	err := svc.HandleEraseJob(tenant.WithID(context.Background(), tenant.DefaultID), mustJSON(EraseJob{UserID: testID(10)}))
	require.Error(t, err)
	assert.True(t, jobs.IsPermanent(err))
	assert.False(t, eraser.called)
	assert.Equal(t, MembershipStatusActive, db.memberships[[2]uuid.UUID{tenant.DefaultID, testID(10)}].Status)
}

func TestFinalizerCleansCredentialsForAnAlreadyDeletedAccount(t *testing.T) {
	repo := newFakePrivacyRepo()
	repo.anonymizeNotFound = true
	stores, db := privacyTestStores(repo)
	for key, m := range db.memberships {
		m.Status = MembershipStatusRemoved
		db.memberships[key] = m
	}
	tx := &fakePrivateTx{stores: stores}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx})
	require.NoError(t, svc.HandleFinalizeJob(testTenantCtx(), mustJSON(FinalizeJob{UserID: testID(10)})))
	assert.Equal(t, []uuid.UUID{testID(10)}, repo.deleted)
	assert.Empty(t, tx.audits, "a fully erased row must not write a duplicate audit event")
}

type failingPutStorage struct{}

func (failingPutStorage) Put(context.Context, string, io.Reader, int64, string) error {
	return errors.New("put failed")
}
func (failingPutStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, storage.ErrNotFound
}
func (failingPutStorage) Delete(context.Context, string) error { return nil }
func (failingPutStorage) List(context.Context, string) ([]storage.Object, error) {
	return nil, nil
}
func (failingPutStorage) URL(string) string { return "" }

type panickingExporter struct{}

func (panickingExporter) Export(context.Context, uuid.UUID, uuid.UUID) (privacycontracts.Section, error) {
	panic("exporter panic")
}
func (panickingExporter) Erase(context.Context, uuid.UUID, uuid.UUID) error { return nil }

type recordingEraser struct{ called bool }

func (r *recordingEraser) Erase(context.Context, uuid.UUID, uuid.UUID) error {
	r.called = true
	return nil
}

type failingEraser struct{ err error }

func (f failingEraser) Erase(context.Context, uuid.UUID, uuid.UUID) error { return f.err }

// txState tracks whether the fake transactor is inside a transaction and
// whether the last-owner lock ran there.
type txState struct {
	inTx       bool
	lockedInTx bool
}

type txFlaggingTransactor struct {
	state *txState
	inner Transactor
}

func (t txFlaggingTransactor) InTx(ctx context.Context, fn func(context.Context, TxRepos) error) error {
	t.state.inTx = true
	defer func() { t.state.inTx = false }()
	return t.inner.InTx(ctx, fn)
}

type lockRecordingMemberships struct {
	memMemberships
	state *txState
}

func (r lockRecordingMemberships) LockOwnerChanges(context.Context) error {
	r.state.lockedInTx = r.state.inTx
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// markReserved simulates the request transaction's erasure reservation.
func markReserved(m *Membership) {
	now := time.Now()
	m.Status = MembershipStatusSuspended
	m.ErasureRequestedAt = &now
	m.RemovedAt = nil
}
