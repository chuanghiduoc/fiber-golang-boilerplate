package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeRepository struct {
	flags        map[string]Flag
	targets      []Target
	nextTargetID int
	lastTenantID uuid.UUID
	deletedKeys  []string
	deletedIDs   []uuid.UUID
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		flags:        make(map[string]Flag),
		nextTargetID: 1,
	}
}

func (r *fakeRepository) ListFlags(context.Context) ([]Flag, error) {
	out := make([]Flag, 0, len(r.flags))
	for _, flag := range r.flags {
		out = append(out, flag)
	}
	return out, nil
}

func (r *fakeRepository) GetFlag(_ context.Context, key string) (Flag, error) {
	flag, ok := r.flags[key]
	if !ok {
		return Flag{}, apperror.ErrNotFound
	}
	return flag, nil
}

func (r *fakeRepository) CreateFlag(ctx context.Context, key string, write FlagWrite) (Flag, error) {
	if _, ok := r.flags[key]; ok {
		return Flag{}, apperror.NewConflict("feature flag already exists; update it instead")
	}
	return r.UpsertFlag(ctx, key, write)
}

func (r *fakeRepository) CreateFlagIfAbsent(ctx context.Context, key string, write FlagWrite) (Flag, error) {
	if _, ok := r.flags[key]; ok {
		return Flag{}, apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
	}
	return r.UpsertFlag(ctx, key, write)
}

func (r *fakeRepository) UpdateFlagIfUnchanged(ctx context.Context, key string, write FlagWrite, expected time.Time) (Flag, error) {
	existing, ok := r.flags[key]
	if !ok || existing.UpdatedAt == nil || !existing.UpdatedAt.Equal(expected) {
		return Flag{}, apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
	}
	return r.UpsertFlag(ctx, key, write)
}

func (r *fakeRepository) DeleteFlagIfUnchanged(ctx context.Context, key string, expected time.Time) error {
	existing, ok := r.flags[key]
	if !ok || existing.UpdatedAt == nil || !existing.UpdatedAt.Equal(expected) {
		return apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
	}
	return r.DeleteFlag(ctx, key)
}

func (r *fakeRepository) UpsertFlag(_ context.Context, key string, write FlagWrite) (Flag, error) {
	now := time.Now()
	flag := Flag{
		Key:         key,
		Enabled:     write.Enabled != nil && *write.Enabled,
		Configured:  write.Enabled != nil,
		KillSwitch:  write.KillSwitch,
		Description: write.Description,
		CreatedAt:   &now,
		UpdatedAt:   &now,
	}
	if existing, ok := r.flags[key]; ok {
		flag.CreatedAt = existing.CreatedAt
	}
	r.flags[key] = flag
	return flag, nil
}

func (r *fakeRepository) DeleteFlag(_ context.Context, key string) error {
	r.deletedKeys = append(r.deletedKeys, key)
	if _, ok := r.flags[key]; !ok {
		return apperror.ErrNotFound
	}
	delete(r.flags, key)
	return nil
}

func (r *fakeRepository) ListTargets(ctx context.Context, key string) ([]Target, error) {
	tid := tenant.IDFromContext(ctx)
	out := make([]Target, 0, len(r.targets))
	for _, target := range r.targets {
		if target.TenantID == tid && target.FlagKey == key {
			out = append(out, target)
		}
	}
	return out, nil
}

func (r *fakeRepository) ListAllTargets(_ context.Context, key string) ([]Target, error) {
	out := make([]Target, 0, len(r.targets))
	for _, target := range r.targets {
		if target.FlagKey == key {
			out = append(out, target)
		}
	}
	return out, nil
}

func (r *fakeRepository) UpsertTarget(_ context.Context, key string, tenantID uuid.UUID, userID *uuid.UUID, enabled bool) (Target, error) {
	r.lastTenantID = tenantID
	now := time.Now()
	target := Target{
		ID:        testID(r.nextTargetID),
		FlagKey:   key,
		TenantID:  tenantID,
		Enabled:   enabled,
		CreatedAt: &now,
		UpdatedAt: &now,
	}
	if userID != nil {
		userIDCopy := *userID
		target.UserID = &userIDCopy
	}
	for i := range r.targets {
		existing := r.targets[i]
		if existing.FlagKey == target.FlagKey && existing.TenantID == target.TenantID &&
			(existing.UserID == nil) == (target.UserID == nil) &&
			(target.UserID == nil || *existing.UserID == *target.UserID) {
			target.ID = existing.ID
			target.CreatedAt = existing.CreatedAt
			r.targets[i] = target
			return target, nil
		}
	}
	r.nextTargetID++
	r.targets = append(r.targets, target)
	return target, nil
}

func (r *fakeRepository) DeleteTarget(_ context.Context, key string, tenantID uuid.UUID, userID *uuid.UUID) error {
	for i := range r.targets {
		target := r.targets[i]
		if target.FlagKey != key || target.TenantID != tenantID {
			continue
		}
		if (target.UserID == nil) != (userID == nil) {
			continue
		}
		if userID != nil && *target.UserID != *userID {
			continue
		}
		r.deletedIDs = append(r.deletedIDs, target.ID)
		r.targets = append(r.targets[:i], r.targets[i+1:]...)
		return nil
	}
	return apperror.ErrNotFound
}

// fakeTransactor runs a mutation callback against the same in-memory
// repository, recording its Tier 1 audit event through the fake writer.
type fakeTransactor struct {
	repo  Repository
	audit AuditWriter
}

func (t fakeTransactor) InTx(ctx context.Context, fn func(context.Context, TxRepos) error) error {
	return fn(ctx, TxRepos{Flags: t.repo, Audit: t.audit})
}

// nopAuditWriter accepts Tier 1 events for tests that only assert side effects.
type nopAuditWriter struct{}

func (nopAuditWriter) RecordTx(context.Context, audit.Event) error { return nil }

func newTestService(repo Repository, evaluator features.Evaluator, recorder audit.Recorder) Service {
	writer := AuditWriter(nopAuditWriter{})
	if typed, ok := recorder.(AuditWriter); ok {
		writer = typed
	}
	return New(repo, evaluator, recorder, fakeTransactor{repo: repo, audit: writer}, testCatalog, testAuthorizer())
}

type fakeEvaluator struct {
	result        features.Result
	invalidations int
}

func (f *fakeEvaluator) Enabled(context.Context, features.Key, features.Subject) bool {
	return f.result.Enabled
}

func (f *fakeEvaluator) Evaluate(context.Context, features.Key, features.Subject) features.Result {
	return f.result
}

func (f *fakeEvaluator) Invalidate(context.Context) error {
	f.invalidations++
	return nil
}

func tenantCtx(id uuid.UUID) context.Context {
	return tenant.WithID(context.Background(), id)
}

// platformCtx is a tenant 1 request whose subject holds the platform flag
// permission.
func platformCtx() context.Context {
	return authz.WithSubject(tenantCtx(testID(1)), authz.Subject{
		UserID: testID(1), TenantID: testID(1),
		Permissions: map[authz.Permission]struct{}{authz.PermissionPlatformFeaturesWrite: {}},
	})
}

func TestListFlagsMergesCatalogAndDatabaseState(t *testing.T) {
	repo := newFakeRepository()
	repo.flags[string(testKeyUserRegistration)] = Flag{
		Key:         string(testKeyUserRegistration),
		Enabled:     false,
		Configured:  true,
		KillSwitch:  true,
		Description: "custom",
	}
	evaluator := &fakeEvaluator{result: features.Result{Enabled: true, Source: features.SourceUserTarget}}
	svc := newTestService(repo, evaluator, nil)

	flags, err := svc.ListFlags(tenantCtx(testID(2)), features.Subject{TenantID: testID(2), UserID: testID(7)})
	if err != nil {
		t.Fatalf("ListFlags: %v", err)
	}
	if len(flags) != len(testCatalog.Definitions()) {
		t.Fatalf("ListFlags returned %d flags, want %d", len(flags), len(testCatalog.Definitions()))
	}
	for _, flag := range flags {
		if flag.Key != string(testKeyUserRegistration) {
			if flag.Configured {
				t.Errorf("flag %q unexpectedly configured", flag.Key)
			}
			continue
		}
		if !flag.Configured || flag.Enabled || !flag.KillSwitch || flag.Description != "custom" {
			t.Fatalf("user_registration = %+v", flag)
		}
		if !flag.Effective || flag.Source != string(features.SourceUserTarget) {
			t.Fatalf("effective state = %+v", flag)
		}
	}
}

func TestGetFlagIncludesTargetsForTenant(t *testing.T) {
	repo := newFakeRepository()
	repo.targets = []Target{
		{ID: testID(1), FlagKey: string(testKeyFileUploads), TenantID: testID(1), Enabled: false},
		{ID: testID(2), FlagKey: string(testKeyFileUploads), TenantID: testID(2), Enabled: true},
	}
	svc := newTestService(repo, &fakeEvaluator{}, nil)

	flag, err := svc.GetFlag(tenantCtx(testID(2)), string(testKeyFileUploads), features.Subject{TenantID: testID(2)})
	if err != nil {
		t.Fatalf("GetFlag: %v", err)
	}
	if flag.Key != string(testKeyFileUploads) || flag.Configured {
		t.Fatalf("flag = %+v, want compiled file_uploads", flag)
	}
	targets, err := svc.ListTargets(tenantCtx(testID(2)), string(testKeyFileUploads))
	if err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != testID(2) || targets[0].TenantID != testID(2) {
		t.Fatalf("targets = %+v, want only tenant 2 target", targets)
	}
}

func TestUpsertFlagPreservesExistingDescriptionAndInvalidates(t *testing.T) {
	repo := newFakeRepository()
	repo.flags[string(testKeyFileUploads)] = Flag{
		Key:         string(testKeyFileUploads),
		Enabled:     true,
		Configured:  true,
		Description: "custom upload policy",
	}
	evaluator := &fakeEvaluator{result: features.Result{Enabled: false, Source: features.SourceKillSwitch}}
	svc := newTestService(repo, evaluator, nil)
	enabled, kill := false, true

	flag, err := svc.UpsertFlag(platformCtx(), string(testKeyFileUploads), UpsertFlagInput{
		Enabled:    &enabled,
		KillSwitch: &kill,
	}, features.Subject{TenantID: testID(1)})
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	if flag.Description != "custom upload policy" {
		t.Fatalf("description = %q, want existing description preserved", flag.Description)
	}
	if flag.Enabled || !flag.KillSwitch || !flag.Configured {
		t.Fatalf("flag = %+v", flag)
	}
	if evaluator.invalidations != 1 {
		t.Fatalf("invalidations = %d, want 1", evaluator.invalidations)
	}
}

func TestUpsertFlagUsesCatalogDescriptionForNewRow(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo, &fakeEvaluator{}, nil)
	enabled, kill := true, false

	flag, err := svc.UpsertFlag(platformCtx(), string(testKeyGoogleOAuth), UpsertFlagInput{
		Enabled:    &enabled,
		KillSwitch: &kill,
	}, features.Subject{TenantID: testID(1)})
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	def, _ := testCatalog.Lookup(string(testKeyGoogleOAuth))
	if flag.Description != def.Description {
		t.Fatalf("description = %q, want catalog description %q", flag.Description, def.Description)
	}
}

func TestPutTargetUsesRequestTenant(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo, &fakeEvaluator{}, nil)
	userID := testID(42)

	target, err := svc.PutTarget(tenantCtx(testID(3)), string(testKeyFileUploads), TargetRef{UserID: &userID}, true)
	if err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if target.TenantID != testID(3) || target.UserID == nil || *target.UserID != testID(42) || !target.Enabled {
		t.Fatalf("target = %+v", target)
	}
	if repo.lastTenantID != testID(3) {
		t.Fatalf("repository tenant = %s, want %s", repo.lastTenantID, testID(3))
	}
}

func TestDeleteFlagIsIdempotent(t *testing.T) {
	repo := newFakeRepository()
	evaluator := &fakeEvaluator{}
	svc := newTestService(repo, evaluator, nil)

	if err := svc.DeleteFlag(platformCtx(), string(testKeyFileUploads)); err != nil {
		t.Fatalf("DeleteFlag missing row: %v", err)
	}
	if evaluator.invalidations != 1 {
		t.Fatalf("invalidations = %d, want 1", evaluator.invalidations)
	}
}

func TestDeleteTargetNotFoundIs404(t *testing.T) {
	repo := newFakeRepository()
	evaluator := &fakeEvaluator{}
	svc := newTestService(repo, evaluator, nil)

	err := svc.DeleteTarget(tenantCtx(testID(1)), string(testKeyFileUploads), TargetRef{UserID: uuidPtr(testID(99))})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 404 {
		t.Fatalf("DeleteTarget error = %v, want 404", err)
	}
	if evaluator.invalidations != 0 {
		t.Fatalf("invalidations = %d, want 0", evaluator.invalidations)
	}
}

func TestUnknownFlagKeyIs404(t *testing.T) {
	svc := newTestService(newFakeRepository(), &fakeEvaluator{}, nil)
	ctx := tenantCtx(testID(1))

	if _, err := svc.GetFlag(ctx, "typo", features.Subject{}); !isNotFound(err) {
		t.Fatalf("GetFlag error = %v, want 404", err)
	}
	if _, err := svc.UpsertFlag(ctx, "typo", UpsertFlagInput{}, features.Subject{}); !isNotFound(err) {
		t.Fatalf("UpsertFlag error = %v, want 404", err)
	}
	if err := svc.DeleteFlag(ctx, "typo"); !isNotFound(err) {
		t.Fatalf("DeleteFlag error = %v, want 404", err)
	}
	if _, err := svc.ListTargets(ctx, "typo"); !isNotFound(err) {
		t.Fatalf("ListTargets error = %v, want 404", err)
	}
	if _, err := svc.PutTarget(ctx, "typo", TargetRef{}, true); !isNotFound(err) {
		t.Fatalf("PutTarget error = %v, want 404", err)
	}
	if err := svc.DeleteTarget(ctx, "typo", TargetRef{}); !isNotFound(err) {
		t.Fatalf("DeleteTarget error = %v, want 404", err)
	}
}

func isNotFound(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && resolvedStatus(appErr) == 404
}

type recordingAudit struct {
	events []audit.Event
}

func (r *recordingAudit) Record(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return nil
}

// RecordTx implements AuditWriter for Tier 1 flag writes.
func (r *recordingAudit) RecordTx(ctx context.Context, event audit.Event) error {
	return r.Record(ctx, event)
}

func uuidPtr(value uuid.UUID) *uuid.UUID { return &value }

func isPermissionDenied(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && resolvedStatus(appErr) == 403 && appErr.Code == string(apperror.CodePermissionDenied)
}

func TestGlobalFlagWritesRequirePlatformTenant(t *testing.T) {
	repo := newFakeRepository()
	evaluator := &fakeEvaluator{}
	svc := newTestService(repo, evaluator, nil)
	enabled, kill := true, true
	in := UpsertFlagInput{Enabled: &enabled, KillSwitch: &kill}
	key := string(testKeyFileUploads)

	// A tenant 2 admin, and a context with no resolved tenant (no fallback),
	// must not change process-wide state.
	for _, ctx := range []context.Context{tenantCtx(testID(2)), context.Background()} {
		if _, err := svc.UpsertFlag(ctx, key, in, features.Subject{}); !isPermissionDenied(err) {
			t.Fatalf("UpsertFlag error = %v, want 403 permission.denied", err)
		}
		if err := svc.DeleteFlag(ctx, key); !isPermissionDenied(err) {
			t.Fatalf("DeleteFlag error = %v, want 403 permission.denied", err)
		}
	}
	if len(repo.flags) != 0 || len(repo.deletedKeys) != 0 || evaluator.invalidations != 0 {
		t.Fatalf("denied writes reached the repository: flags=%v deleted=%v invalidations=%d",
			repo.flags, repo.deletedKeys, evaluator.invalidations)
	}

	// Tenant-scoped target management stays available to other tenants.
	if _, err := svc.PutTarget(tenantCtx(testID(2)), key, TargetRef{}, true); err != nil {
		t.Fatalf("PutTarget for tenant 2: %v", err)
	}
}

func TestUpsertFlagWithoutEnabledClearsDatabaseDefault(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo, &fakeEvaluator{}, nil)
	kill := true

	flag, err := svc.UpsertFlag(platformCtx(), string(testKeyGoogleOAuth), UpsertFlagInput{
		KillSwitch: &kill,
	}, features.Subject{TenantID: testID(1)})
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	if repo.flags[string(testKeyGoogleOAuth)].Configured {
		t.Fatal("omitted enabled must store NULL")
	}
	def, _ := testCatalog.Lookup(string(testKeyGoogleOAuth))
	if flag.Configured || flag.Enabled != def.Default || !flag.KillSwitch {
		t.Fatalf("flag = %+v, want unconfigured default with kill switch", flag)
	}
}

func TestDeleteFlagPropagatesTargetConflict(t *testing.T) {
	svc := newTestService(&conflictRepository{fakeRepository: newFakeRepository()}, &fakeEvaluator{}, nil)

	err := svc.DeleteFlag(platformCtx(), string(testKeyFileUploads))
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 409 {
		t.Fatalf("DeleteFlag error = %v, want 409", err)
	}
}

type conflictRepository struct {
	*fakeRepository
}

func (r *conflictRepository) DeleteFlag(context.Context, string) error {
	return apperror.NewConflict("feature flag has targets; delete them before deleting the flag")
}

func TestMutationsRecordDomainEventsWithMetadata(t *testing.T) {
	repo := newFakeRepository()
	recorder := &recordingAudit{}
	svc := newTestService(repo, &fakeEvaluator{result: features.Result{Source: features.SourceKillSwitch}}, recorder)
	ctx := platformCtx()
	enabled, kill := false, true
	key := string(testKeyFileUploads)
	userID := testID(42)

	if _, err := svc.UpsertFlag(ctx, key, UpsertFlagInput{Enabled: &enabled, KillSwitch: &kill}, features.Subject{TenantID: testID(1)}); err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	target, err := svc.PutTarget(ctx, key, TargetRef{UserID: &userID}, false)
	if err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if err := svc.DeleteTarget(ctx, key, TargetRef{UserID: &userID}); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if err := svc.DeleteFlag(ctx, key); err != nil {
		t.Fatalf("DeleteFlag: %v", err)
	}

	if len(recorder.events) != 4 {
		t.Fatalf("events = %d, want 4", len(recorder.events))
	}
	flagEvent := recorder.events[0]
	if flagEvent.Action != "admin.feature_flag.updated" || flagEvent.ResourceID != key ||
		flagEvent.Metadata["feature"] != key || flagEvent.Metadata["enabled"] != false ||
		flagEvent.Metadata["killSwitch"] != true || flagEvent.Metadata["configured"] != true {
		t.Fatalf("flag event = %+v", flagEvent)
	}
	targetEvent := recorder.events[1]
	if targetEvent.Action != "admin.feature_flag_target.updated" ||
		targetEvent.Metadata["targetId"] != "user:"+target.UserID.String() ||
		targetEvent.Metadata["userId"] != testID(42).String() {
		t.Fatalf("target event = %+v", targetEvent)
	}
	deleteEvent := recorder.events[2]
	if deleteEvent.Action != "admin.feature_flag_target.deleted" ||
		deleteEvent.Metadata["targetId"] != "user:"+target.UserID.String() {
		t.Fatalf("delete event = %+v", deleteEvent)
	}
	if recorder.events[3].Action != "admin.feature_flag.deleted" {
		t.Fatalf("flag delete event = %+v", recorder.events[3])
	}
}

func TestCreateFlagRejectsExistingRow(t *testing.T) {
	repo := newFakeRepository()
	repo.flags[string(testKeyFileUploads)] = Flag{Key: string(testKeyFileUploads), Configured: true}
	svc := newTestService(repo, &fakeEvaluator{}, nil)
	kill := false

	_, err := svc.CreateFlag(platformCtx(), string(testKeyFileUploads), UpsertFlagInput{KillSwitch: &kill}, features.Subject{})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 409 {
		t.Fatalf("CreateFlag error = %v, want 409", err)
	}
}

func TestCreateFlagWritesTier1Event(t *testing.T) {
	repo := newFakeRepository()
	recorder := &recordingAudit{}
	svc := newTestService(repo, &fakeEvaluator{}, recorder)
	kill := false

	flag, err := svc.CreateFlag(platformCtx(), string(testKeyFileUploads), UpsertFlagInput{KillSwitch: &kill}, features.Subject{TenantID: testID(1)})
	if err != nil {
		t.Fatalf("CreateFlag: %v", err)
	}
	if flag.KillSwitch {
		t.Fatalf("flag = %+v, want the created row", flag)
	}
	if _, ok := repo.flags[string(testKeyFileUploads)]; !ok {
		t.Fatal("CreateFlag did not store the row")
	}
	if len(recorder.events) != 1 || recorder.events[0].Action != "admin.feature_flag.updated" {
		t.Fatalf("events = %+v, want one Tier 1 flag update", recorder.events)
	}
}

func TestCreateFlagRequiresPlatformPermission(t *testing.T) {
	svc := newTestService(newFakeRepository(), &fakeEvaluator{}, nil)
	kill := false
	if _, err := svc.CreateFlag(tenantCtx(testID(2)), string(testKeyFileUploads), UpsertFlagInput{KillSwitch: &kill}, features.Subject{}); !isPermissionDenied(err) {
		t.Fatalf("CreateFlag error = %v, want 403 permission.denied", err)
	}
}

func TestListEvaluatedUsesTheCallerSubject(t *testing.T) {
	repo := newFakeRepository()
	repo.flags[string(testKeyFileUploads)] = Flag{Key: string(testKeyFileUploads), KillSwitch: true}
	evaluator := &fakeEvaluator{result: features.Result{Enabled: false, Source: features.SourceKillSwitch}}
	svc := newTestService(repo, evaluator, nil)

	flags, err := svc.ListEvaluated(tenantCtx(testID(2)), features.Subject{TenantID: testID(2)})
	if err != nil {
		t.Fatalf("ListEvaluated: %v", err)
	}
	if len(flags) != len(testCatalog.Definitions()) {
		t.Fatalf("ListEvaluated returned %d flags, want %d", len(flags), len(testCatalog.Definitions()))
	}
	for _, flag := range flags {
		if flag.Key != string(testKeyFileUploads) {
			continue
		}
		if flag.Enabled || !flag.KillSwitch || flag.Source != string(features.SourceKillSwitch) {
			t.Fatalf("evaluated flag = %+v", flag)
		}
	}
}

func TestListAllTargetsRequiresPlatformRead(t *testing.T) {
	repo := newFakeRepository()
	repo.targets = []Target{
		{ID: testID(1), FlagKey: string(testKeyFileUploads), TenantID: testID(1), Enabled: false},
		{ID: testID(2), FlagKey: string(testKeyFileUploads), TenantID: testID(2), Enabled: true},
	}
	svc := newTestService(repo, &fakeEvaluator{}, nil)

	if _, err := svc.ListAllTargets(tenantCtx(testID(1)), string(testKeyFileUploads)); !isPermissionDenied(err) {
		t.Fatalf("ListAllTargets without the platform permission = %v, want 403", err)
	}

	ctx := authz.WithSubject(tenantCtx(testID(1)), authz.Subject{
		UserID: testID(1), TenantID: testID(1),
		Permissions: map[authz.Permission]struct{}{authz.PermissionPlatformFeaturesRead: {}},
	})
	targets, err := svc.ListAllTargets(ctx, string(testKeyFileUploads))
	if err != nil {
		t.Fatalf("ListAllTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want both tenants", targets)
	}
}

func TestPutTargetCrossTenantRequiresPlatformWrite(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo, &fakeEvaluator{}, nil)
	tenantID := testID(3)

	if _, err := svc.PutTarget(tenantCtx(testID(2)), string(testKeyFileUploads), TargetRef{TenantID: &tenantID}, true); !isPermissionDenied(err) {
		t.Fatalf("PutTarget for another tenant = %v, want 403", err)
	}

	target, err := svc.PutTarget(platformCtx(), string(testKeyFileUploads), TargetRef{TenantID: &tenantID}, true)
	if err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if target.TenantID != tenantID || repo.lastTenantID != tenantID {
		t.Fatalf("target = %+v (repository tenant %s), want %s", target, repo.lastTenantID, tenantID)
	}
}

func TestUpdateFlagEnforcesTheReadVersion(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo, &fakeEvaluator{}, nil)
	ctx := platformCtx()
	subject := features.Subject{TenantID: testID(1)}
	key := string(testKeyFileUploads)
	kill := false

	created, err := svc.UpsertFlag(ctx, key, UpsertFlagInput{KillSwitch: &kill}, subject)
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	if created.UpdatedAt == nil {
		t.Fatal("created flag has no version")
	}

	stale := ExpectedVersion{Exists: true, UpdatedAt: created.UpdatedAt.Add(-time.Second)}
	if _, err := svc.UpdateFlag(ctx, key, UpsertFlagInput{KillSwitch: &kill}, subject, stale); !isPreconditionFailed(err) {
		t.Fatalf("UpdateFlag with a stale version = %v, want 412", err)
	}
	if _, err := svc.UpdateFlag(ctx, key, UpsertFlagInput{KillSwitch: &kill}, subject, ExpectedVersion{Exists: false}); !isPreconditionFailed(err) {
		t.Fatalf("UpdateFlag expecting no row = %v, want 412", err)
	}

	enabled := true
	fresh := ExpectedVersion{Exists: true, UpdatedAt: *created.UpdatedAt}
	updated, err := svc.UpdateFlag(ctx, key, UpsertFlagInput{Enabled: &enabled, KillSwitch: &kill}, subject, fresh)
	if err != nil {
		t.Fatalf("UpdateFlag with the current version: %v", err)
	}
	if !updated.Enabled {
		t.Fatalf("updated flag = %+v, want enabled", updated)
	}
}

func TestDeleteFlagEnforcesTheReadVersion(t *testing.T) {
	repo := newFakeRepository()
	svc := newTestService(repo, &fakeEvaluator{}, nil)
	ctx := platformCtx()
	key := string(testKeyFileUploads)
	kill := false

	created, err := svc.UpsertFlag(ctx, key, UpsertFlagInput{KillSwitch: &kill}, features.Subject{})
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	stale := ExpectedVersion{Exists: true, UpdatedAt: created.UpdatedAt.Add(-time.Second)}
	if err := svc.DeleteFlagVersion(ctx, key, stale); !isPreconditionFailed(err) {
		t.Fatalf("DeleteFlagVersion with a stale version = %v, want 412", err)
	}
	if _, ok := repo.flags[key]; !ok {
		t.Fatal("a refused delete must keep the row")
	}
	if err := svc.DeleteFlagVersion(ctx, key, ExpectedVersion{Exists: true, UpdatedAt: *created.UpdatedAt}); err != nil {
		t.Fatalf("DeleteFlagVersion: %v", err)
	}
	if _, ok := repo.flags[key]; ok {
		t.Fatal("the row must be deleted")
	}
	// A caller that saw no row does not delete a concurrently created one.
	repo.flags[key] = Flag{Key: key, Configured: true}
	if err := svc.DeleteFlagVersion(ctx, key, ExpectedVersion{Exists: false}); err != nil {
		t.Fatalf("DeleteFlagVersion expecting no row: %v", err)
	}
	if _, ok := repo.flags[key]; !ok {
		t.Fatal("a caller that saw no row must not delete it")
	}
}

func isPreconditionFailed(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && resolvedStatus(appErr) == 412
}
