package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type fakePrivacyRepo struct {
	exports           map[int64]*DataExport
	nextID            int64
	sessions          []Session
	anonymized        []int64
	deleted           []int64
	anonymizeNotFound bool
}

func newFakePrivacyRepo() *fakePrivacyRepo {
	return &fakePrivacyRepo{exports: map[int64]*DataExport{}, nextID: 1}
}

func (r *fakePrivacyRepo) Create(_ context.Context, userID int64) (*DataExport, error) {
	for _, e := range r.exports {
		if e.UserID == userID && (e.Status == DataExportQueued || e.Status == DataExportRunning) {
			return nil, ErrDataExportInProgress
		}
	}
	e := &DataExport{ID: r.nextID, TenantID: 1, UserID: userID, Status: DataExportQueued, CreatedAt: time.Now()}
	r.nextID++
	r.exports[e.ID] = e
	c := *e
	return &c, nil
}

func (r *fakePrivacyRepo) Get(_ context.Context, userID, id int64) (*DataExport, error) {
	e, ok := r.exports[id]
	if !ok || e.UserID != userID {
		return nil, apperror.ErrNotFound
	}
	c := *e
	return &c, nil
}

func (r *fakePrivacyRepo) ListForUser(_ context.Context, userID int64) ([]DataExport, error) {
	var out []DataExport
	for _, e := range r.exports {
		if e.UserID == userID {
			out = append(out, *e)
		}
	}
	return out, nil
}

func (r *fakePrivacyRepo) ListCursor(context.Context, int64, pagination.DBCursor) ([]DataExport, error) {
	return nil, nil
}

func (r *fakePrivacyRepo) MarkRunning(_ context.Context, userID, id int64) (bool, error) {
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

func (r *fakePrivacyRepo) MarkReady(_ context.Context, userID, id int64, path string, expires time.Time) (bool, error) {
	e, ok := r.exports[id]
	if !ok || e.UserID != userID || e.Status != DataExportRunning {
		return false, nil
	}
	e.Status, e.StoragePath, e.ExpiresAt = DataExportReady, path, &expires
	return true, nil
}

func (r *fakePrivacyRepo) MarkFailed(_ context.Context, userID, id int64) error {
	if e, ok := r.exports[id]; ok && e.UserID == userID {
		e.Status = DataExportFailed
	}
	return nil
}

func (r *fakePrivacyRepo) ExpireForUser(_ context.Context, userID int64) ([]DataExport, error) {
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

func (r *fakePrivacyRepo) DeleteForUser(_ context.Context, userID int64) error {
	for id, e := range r.exports {
		if e.UserID == userID {
			delete(r.exports, id)
		}
	}
	return nil
}

func (r *fakePrivacyRepo) Purgeable(context.Context, int32) ([]DataExport, error) { return nil, nil }
func (r *fakePrivacyRepo) Delete(_ context.Context, id int64) error {
	delete(r.exports, id)
	return nil
}

func (r *fakePrivacyRepo) ListSessions(context.Context, int64) ([]Session, error) {
	return r.sessions, nil
}

func (r *fakePrivacyRepo) AnonymizeUser(_ context.Context, userID int64, _ string) (*User, error) {
	if r.anonymizeNotFound {
		return nil, apperror.ErrNotFound
	}
	r.anonymized = append(r.anonymized, userID)
	return &User{ID: userID, Status: UserStatusDeleted}, nil
}

func (r *fakePrivacyRepo) DeleteCredentialsAndIdentities(_ context.Context, userID int64) error {
	r.deleted = append(r.deleted, userID)
	return nil
}

func (r *fakePrivacyRepo) DeleteSessionsInTenant(context.Context, int64) error { return nil }
func (r *fakePrivacyRepo) DeleteSessionsForUser(context.Context, int64) error  { return nil }

type fakeJobs struct {
	mu   sync.Mutex
	jobs []jobs.Job
}

func (j *fakeJobs) Enqueue(_ context.Context, job jobs.Job) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jobs = append(j.jobs, job)
	return int64(len(j.jobs)), nil
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

type fakeNotifier struct {
	got []notificationcontracts.CreateNotificationInput
}

func (n *fakeNotifier) Notify(_ context.Context, in notificationcontracts.CreateNotificationInput) error {
	n.got = append(n.got, in)
	return nil
}

type fakeExporter struct{ section privacycontracts.Section }

func (e fakeExporter) Export(context.Context, int64, int64) (privacycontracts.Section, error) {
	return e.section, nil
}
func (e fakeExporter) Erase(context.Context, int64, int64) error { return nil }

func privacyTestStores(repo *fakePrivacyRepo) (Stores, *memDB) {
	db := newMemDB()
	db.users[10] = &User{ID: 10, Email: "privacy@example.com", Name: "Privacy", Status: UserStatusActive}
	db.memberships[[2]int64{tenant.DefaultID, 10}] = &Membership{
		ID: 100, TenantID: tenant.DefaultID, UserID: 10, Role: RoleUser,
		Status: MembershipStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	stores := db.stores()
	stores.Privacy = repo
	return stores, db
}

func newPrivacyTestService(t *testing.T, repo *fakePrivacyRepo, registry *privacycontracts.Registry) (*PrivacyService, *fakeJobs, *fakeNotifier, storage.Storage, Stores) {
	t.Helper()
	stores, _ := privacyTestStores(repo)
	tx := &fakePrivateTx{stores: stores, jobs: &fakeJobs{}}
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	notifier := &fakeNotifier{}
	svc := NewPrivacyService(PrivacyDeps{
		Stores: stores, Tx: tx, Storage: store, Registry: registry, Notifier: notifier,
	})
	return svc, tx.jobs, notifier, store, stores
}

func TestRequestExportCreatesRowAndJob(t *testing.T) {
	repo := newFakePrivacyRepo()
	svc, jobQueue, _, _, _ := newPrivacyTestService(t, repo, privacycontracts.NewRegistry())
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	export, err := svc.RequestExport(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, DataExportQueued, export.Status)
	require.Len(t, jobQueue.jobs, 1)
	assert.Equal(t, JobKindDataExport, jobQueue.jobs[0].Kind)
	var payload exportJob
	require.NoError(t, json.Unmarshal(jobQueue.jobs[0].Payload, &payload))
	assert.Equal(t, export.ID, payload.ExportID)
	assert.Equal(t, int64(10), payload.UserID)
}

func TestRequestExportConflictIs409(t *testing.T) {
	repo := newFakePrivacyRepo()
	svc, _, _, _, _ := newPrivacyTestService(t, repo, privacycontracts.NewRegistry())
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	_, err := svc.RequestExport(ctx, 10)
	require.NoError(t, err)
	_, err = svc.RequestExport(ctx, 10)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 409, appErr.Status)
	assert.Equal(t, string(identitycatalog.CodeDataExportInProgress), appErr.Code)
}

func TestExportJobWritesBlobMarksReadyAndNotifies(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(context.Background(), 10)
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{
		Name:     "files",
		Exporter: fakeExporter{privacycontracts.Section{Name: "files", Data: json.RawMessage(`{"n":1}`)}},
		Eraser:   fakeExporter{},
	})
	svc, _, notifier, store, _ := newPrivacyTestService(t, repo, registry)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	require.NoError(t, svc.HandleExportJob(ctx, mustJSON(exportJob{ExportID: e.ID, UserID: 10})))

	got, _ := repo.Get(context.Background(), 10, e.ID)
	require.Equal(t, DataExportReady, got.Status)
	require.NotEmpty(t, got.StoragePath)
	require.NotNil(t, got.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(DataExportTTL), *got.ExpiresAt, time.Minute)
	reader, err := store.Get(context.Background(), got.StoragePath)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	var doc exportDocument
	require.NoError(t, json.NewDecoder(reader).Decode(&doc))
	var section map[string]int
	require.NoError(t, json.Unmarshal(doc.Sections["files"], &section))
	assert.Equal(t, 1, section["n"])
	require.Len(t, notifier.got, 1)
	assert.Equal(t, int64(10), notifier.got[0].UserID)
	assert.Equal(t, "privacy.data_export_ready", notifier.got[0].Type)

	// At-least-once redelivery is a no-op and does not notify twice.
	require.NoError(t, svc.HandleExportJob(ctx, mustJSON(exportJob{ExportID: e.ID, UserID: 10})))
	assert.Len(t, notifier.got, 1)
}

func TestExportJobSkipsAnErasedExport(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(context.Background(), 10)
	svc, _, notifier, _, _ := newPrivacyTestService(t, repo, privacycontracts.NewRegistry())
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	_, err := repo.ExpireForUser(ctx, 10)
	require.NoError(t, err)

	require.NoError(t, svc.HandleExportJob(ctx, mustJSON(exportJob{ExportID: e.ID, UserID: 10})))
	got, _ := repo.Get(context.Background(), 10, e.ID)
	assert.Equal(t, DataExportExpired, got.Status)
	assert.Empty(t, got.StoragePath)
	assert.Empty(t, notifier.got)
}

func TestIdentityExportIncludesProfileMembershipAndSessions(t *testing.T) {
	repo := newFakePrivacyRepo()
	repo.sessions = []Session{{ID: 7, TenantID: tenant.DefaultID, UserID: 10, UserAgent: "agent", CreatedAt: time.Now()}}
	stores, _ := privacyTestStores(repo)
	svc := NewPrivacyService(PrivacyDeps{Stores: stores})
	section, err := svc.Export(context.Background(), tenant.DefaultID, 10)
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
			ID int64 `json:"id"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	assert.Equal(t, "privacy@example.com", out.Profile.Email)
	assert.Equal(t, RoleUser, out.Membership.Role)
	require.Len(t, out.Sessions, 1)
	assert.Equal(t, int64(7), out.Sessions[0].ID)
	assert.NotContains(t, strings.ToLower(string(section.Data)), "password")
}

func TestErasureJobIsIdempotentAndDeletesExports(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(context.Background(), 10)
	path := "1/10/exports/x.json"
	expires := time.Now().Add(time.Hour)
	stored := repo.exports[e.ID]
	stored.Status, stored.StoragePath, stored.ExpiresAt = DataExportReady, path, &expires
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Put(context.Background(), path, strings.NewReader("{}"), 2, "application/json"))

	stores, db := privacyTestStores(repo)
	registry := privacycontracts.NewRegistry()
	tx := &fakePrivateTx{stores: stores}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx, Storage: store, Registry: registry})
	registry.Register(privacycontracts.Provider{Name: "identity", Exporter: svc, Eraser: svc})
	registry.Register(privacycontracts.Provider{Name: "files", Exporter: fakeExporter{}, Eraser: fakeExporter{}})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	// The erasure request pre-expires exports; the eraser must still delete
	// the already-expired document's blob.
	_, err = repo.ExpireForUser(ctx, 10)
	require.NoError(t, err)
	require.NoError(t, svc.HandleEraseJob(ctx, mustJSON(eraseJob{UserID: 10})))
	require.NoError(t, svc.HandleEraseJob(ctx, mustJSON(eraseJob{UserID: 10})))
	assert.Equal(t, MembershipStatusRemoved, db.memberships[[2]int64{tenant.DefaultID, 10}].Status)
	_, err = store.Get(context.Background(), path)
	assert.ErrorIs(t, err, storage.ErrNotFound)
	assert.Empty(t, repo.exports)
}

func TestFinalizeWaitsUntilEveryMembershipIsRemoved(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	acme := int64(2)
	db.memberships[[2]int64{acme, 10}] = &Membership{ID: 101, TenantID: acme, UserID: 10, Role: RoleUser, Status: MembershipStatusRemoved}
	tx := &fakePrivateTx{stores: stores}
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: tx})
	err := svc.HandleFinalizeJob(context.Background(), mustJSON(finalizeJob{UserID: 10}))
	assert.ErrorIs(t, err, errErasurePending)
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
	require.NoError(t, svc.HandleFinalizeJob(context.Background(), mustJSON(finalizeJob{UserID: 10})))
	assert.Equal(t, []int64{10}, repo.anonymized)
	assert.Equal(t, []int64{10}, repo.deleted)
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

	e, err := repo.Create(context.Background(), 10)
	require.NoError(t, err)
	path := "1/10/exports/late.json"
	expires := time.Now().Add(time.Hour)
	stored := repo.exports[e.ID]
	stored.Status, stored.StoragePath, stored.ExpiresAt = DataExportReady, path, &expires
	require.NoError(t, store.Put(context.Background(), path, strings.NewReader("{}"), 2, "application/json"))

	require.NoError(t, svc.HandleFinalizeJob(context.Background(), mustJSON(finalizeJob{UserID: 10})))
	assert.Empty(t, repo.exports)
	_, err = store.Get(context.Background(), path)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestExportJobFailureMarksRowFailedAndUnblocksARequest(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(context.Background(), 10)
	stores, _ := privacyTestStores(repo)
	svc := NewPrivacyService(PrivacyDeps{
		Stores: stores, Tx: &fakePrivateTx{stores: stores, jobs: &fakeJobs{}}, Storage: failingPutStorage{},
	})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	err := svc.HandleExportJob(ctx, mustJSON(exportJob{ExportID: e.ID, UserID: 10}))
	require.Error(t, err)
	got, _ := repo.Get(context.Background(), 10, e.ID)
	assert.Equal(t, DataExportFailed, got.Status)

	_, err = svc.RequestExport(ctx, 10)
	require.NoError(t, err, "a failed export must not keep the active index forever")
}

func TestExportJobPanicMarksRowFailed(t *testing.T) {
	repo := newFakePrivacyRepo()
	e, _ := repo.Create(context.Background(), 10)
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{
		Name: "panic", Exporter: panickingExporter{}, Eraser: fakeExporter{},
	})
	svc, _, _, _, _ := newPrivacyTestService(t, repo, registry)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	func() {
		defer func() { _ = recover() }()
		_ = svc.HandleExportJob(ctx, mustJSON(exportJob{ExportID: e.ID, UserID: 10}))
	}()
	got, _ := repo.Get(context.Background(), 10, e.ID)
	assert.Equal(t, DataExportFailed, got.Status)
}

func TestEraseJobRefusesTheLastAdminBeforeAnyEraser(t *testing.T) {
	repo := newFakePrivacyRepo()
	stores, db := privacyTestStores(repo)
	db.memberships[[2]int64{tenant.DefaultID, 10}].Role = RoleAdmin
	eraser := &recordingEraser{}
	registry := privacycontracts.NewRegistry()
	registry.Register(privacycontracts.Provider{Name: "files", Exporter: fakeExporter{}, Eraser: eraser})
	svc := NewPrivacyService(PrivacyDeps{Stores: stores, Tx: &fakePrivateTx{stores: stores}, Registry: registry})

	err := svc.HandleEraseJob(tenant.WithID(context.Background(), tenant.DefaultID), mustJSON(eraseJob{UserID: 10}))
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(apperror.CodeUserLastAdmin), appErr.Code)
	assert.False(t, eraser.called)
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
	require.NoError(t, svc.HandleFinalizeJob(context.Background(), mustJSON(finalizeJob{UserID: 10})))
	assert.Equal(t, []int64{10}, repo.deleted)
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
func (failingPutStorage) URL(string) string                    { return "" }

type panickingExporter struct{}

func (panickingExporter) Export(context.Context, int64, int64) (privacycontracts.Section, error) {
	panic("exporter panic")
}
func (panickingExporter) Erase(context.Context, int64, int64) error { return nil }

type recordingEraser struct{ called bool }

func (r *recordingEraser) Erase(context.Context, int64, int64) error {
	r.called = true
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
