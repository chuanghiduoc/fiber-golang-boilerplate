package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeRepository struct {
	flags        map[string]Flag
	targets      []Target
	nextTargetID int64
	lastTenantID int64
	deletedKeys  []string
	deletedIDs   []int64
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
	tid := tenant.EffectiveID(ctx)
	out := make([]Target, 0, len(r.targets))
	for _, target := range r.targets {
		if target.TenantID == tid && target.FlagKey == key {
			out = append(out, target)
		}
	}
	return out, nil
}

func (r *fakeRepository) UpsertTarget(ctx context.Context, key string, userID int64, enabled bool) (Target, error) {
	r.lastTenantID = tenant.EffectiveID(ctx)
	now := time.Now()
	target := Target{
		ID:        r.nextTargetID,
		FlagKey:   key,
		TenantID:  r.lastTenantID,
		Enabled:   enabled,
		CreatedAt: &now,
		UpdatedAt: &now,
	}
	if userID != 0 {
		target.UserID = &userID
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

func (r *fakeRepository) DeleteTarget(ctx context.Context, key string, id int64) error {
	r.deletedIDs = append(r.deletedIDs, id)
	tid := tenant.EffectiveID(ctx)
	for i := range r.targets {
		target := r.targets[i]
		if target.ID == id && target.TenantID == tid && target.FlagKey == key {
			r.targets = append(r.targets[:i], r.targets[i+1:]...)
			return nil
		}
	}
	return apperror.ErrNotFound
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

func tenantCtx(id int64) context.Context {
	return tenant.WithID(context.Background(), id)
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
	svc := New(repo, evaluator, nil)

	flags, err := svc.ListFlags(tenantCtx(2), features.Subject{TenantID: 2, UserID: 7})
	if err != nil {
		t.Fatalf("ListFlags: %v", err)
	}
	if len(flags) != len(features.Catalog()) {
		t.Fatalf("ListFlags returned %d flags, want %d", len(flags), len(features.Catalog()))
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
		{ID: 1, FlagKey: string(testKeyFileUploads), TenantID: 1, Enabled: false},
		{ID: 2, FlagKey: string(testKeyFileUploads), TenantID: 2, Enabled: true},
	}
	svc := New(repo, &fakeEvaluator{}, nil)

	detail, err := svc.GetFlag(tenantCtx(2), string(testKeyFileUploads), features.Subject{TenantID: 2})
	if err != nil {
		t.Fatalf("GetFlag: %v", err)
	}
	if detail.Flag.Key != string(testKeyFileUploads) || detail.Flag.Configured {
		t.Fatalf("flag = %+v, want compiled file_uploads", detail.Flag)
	}
	if len(detail.Targets) != 1 || detail.Targets[0].ID != 2 || detail.Targets[0].TenantID != 2 {
		t.Fatalf("targets = %+v, want only tenant 2 target", detail.Targets)
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
	svc := New(repo, evaluator, nil)
	enabled, kill := false, true

	flag, err := svc.UpsertFlag(tenantCtx(1), string(testKeyFileUploads), UpsertFlagInput{
		Enabled:    &enabled,
		KillSwitch: &kill,
	}, features.Subject{TenantID: 1})
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
	svc := New(repo, &fakeEvaluator{}, nil)
	enabled, kill := true, false

	flag, err := svc.UpsertFlag(tenantCtx(1), string(testKeyGoogleOAuth), UpsertFlagInput{
		Enabled:    &enabled,
		KillSwitch: &kill,
	}, features.Subject{TenantID: 1})
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	def, _ := features.Lookup(string(testKeyGoogleOAuth))
	if flag.Description != def.Description {
		t.Fatalf("description = %q, want catalog description %q", flag.Description, def.Description)
	}
}

func TestUpsertTargetUsesRequestTenant(t *testing.T) {
	repo := newFakeRepository()
	svc := New(repo, &fakeEvaluator{}, nil)
	userID := int64(42)
	enabled := true

	target, err := svc.UpsertTarget(tenantCtx(3), string(testKeyFileUploads), UpsertTargetInput{
		UserID:  &userID,
		Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}
	if target.TenantID != 3 || target.UserID == nil || *target.UserID != 42 || !target.Enabled {
		t.Fatalf("target = %+v", target)
	}
	if repo.lastTenantID != 3 {
		t.Fatalf("repository tenant = %d, want 3", repo.lastTenantID)
	}
}

func TestDeleteFlagIsIdempotent(t *testing.T) {
	repo := newFakeRepository()
	evaluator := &fakeEvaluator{}
	svc := New(repo, evaluator, nil)

	if err := svc.DeleteFlag(tenantCtx(1), string(testKeyFileUploads)); err != nil {
		t.Fatalf("DeleteFlag missing row: %v", err)
	}
	if evaluator.invalidations != 1 {
		t.Fatalf("invalidations = %d, want 1", evaluator.invalidations)
	}
}

func TestDeleteTargetNotFoundIs404(t *testing.T) {
	repo := newFakeRepository()
	evaluator := &fakeEvaluator{}
	svc := New(repo, evaluator, nil)

	err := svc.DeleteTarget(tenantCtx(1), string(testKeyFileUploads), 99)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 404 {
		t.Fatalf("DeleteTarget error = %v, want 404", err)
	}
	if evaluator.invalidations != 0 {
		t.Fatalf("invalidations = %d, want 0", evaluator.invalidations)
	}
}

func TestUnknownFlagKeyIs404(t *testing.T) {
	svc := New(newFakeRepository(), &fakeEvaluator{}, nil)
	ctx := tenantCtx(1)

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
	if _, err := svc.UpsertTarget(ctx, "typo", UpsertTargetInput{}); !isNotFound(err) {
		t.Fatalf("UpsertTarget error = %v, want 404", err)
	}
	if err := svc.DeleteTarget(ctx, "typo", 1); !isNotFound(err) {
		t.Fatalf("DeleteTarget error = %v, want 404", err)
	}
}

func isNotFound(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && appErr.Status == 404
}

type recordingAudit struct {
	events []audit.Event
}

func (r *recordingAudit) Record(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return nil
}

func isPermissionDenied(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && appErr.Status == 403 && appErr.Code == string(apperror.CodePermissionDenied)
}

func TestGlobalFlagWritesRequirePlatformTenant(t *testing.T) {
	repo := newFakeRepository()
	evaluator := &fakeEvaluator{}
	svc := New(repo, evaluator, nil)
	enabled, kill := true, true
	in := UpsertFlagInput{Enabled: &enabled, KillSwitch: &kill}
	key := string(testKeyFileUploads)

	// A tenant 2 admin, and a context with no resolved tenant (no EffectiveID
	// fallback), must not change process-wide state.
	for _, ctx := range []context.Context{tenantCtx(2), context.Background()} {
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
	if _, err := svc.UpsertTarget(tenantCtx(2), key, UpsertTargetInput{Enabled: &enabled}); err != nil {
		t.Fatalf("UpsertTarget for tenant 2: %v", err)
	}
}

func TestUpsertFlagWithoutEnabledClearsDatabaseDefault(t *testing.T) {
	repo := newFakeRepository()
	svc := New(repo, &fakeEvaluator{}, nil)
	kill := true

	flag, err := svc.UpsertFlag(tenantCtx(1), string(testKeyGoogleOAuth), UpsertFlagInput{
		KillSwitch: &kill,
	}, features.Subject{TenantID: 1})
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	if repo.flags[string(testKeyGoogleOAuth)].Configured {
		t.Fatal("omitted enabled must store NULL")
	}
	def, _ := features.Lookup(string(testKeyGoogleOAuth))
	if flag.Configured || flag.Enabled != def.Default || !flag.KillSwitch {
		t.Fatalf("flag = %+v, want unconfigured default with kill switch", flag)
	}
}

func TestDeleteFlagPropagatesTargetConflict(t *testing.T) {
	svc := New(&conflictRepository{fakeRepository: newFakeRepository()}, &fakeEvaluator{}, nil)

	err := svc.DeleteFlag(tenantCtx(1), string(testKeyFileUploads))
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 409 {
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
	svc := New(repo, &fakeEvaluator{result: features.Result{Source: features.SourceKillSwitch}}, recorder)
	ctx := tenantCtx(1)
	enabled, kill := false, true
	key := string(testKeyFileUploads)
	userID := int64(42)

	if _, err := svc.UpsertFlag(ctx, key, UpsertFlagInput{Enabled: &enabled, KillSwitch: &kill}, features.Subject{TenantID: 1}); err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	target, err := svc.UpsertTarget(ctx, key, UpsertTargetInput{UserID: &userID, Enabled: &enabled})
	if err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}
	if err := svc.DeleteTarget(ctx, key, target.ID); err != nil {
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
		flagEvent.Metadata["killSwitch"] != true || flagEvent.Metadata["source"] != "kill_switch" {
		t.Fatalf("flag event = %+v", flagEvent)
	}
	targetEvent := recorder.events[1]
	if targetEvent.Action != "admin.feature_flag_target.updated" || targetEvent.Metadata["userId"] != int64(42) {
		t.Fatalf("target event = %+v", targetEvent)
	}
	if recorder.events[2].Action != "admin.feature_flag_target.deleted" || recorder.events[3].Action != "admin.feature_flag.deleted" {
		t.Fatalf("delete events = %q, %q", recorder.events[2].Action, recorder.events[3].Action)
	}
}
