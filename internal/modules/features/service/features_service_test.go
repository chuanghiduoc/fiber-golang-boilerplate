package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featuressqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeRepository struct {
	flags        map[string]featuressqlc.FeatureFlag
	targets      []featuressqlc.FeatureFlagTarget
	nextTargetID int64
	lastTenantID int64
	deletedKeys  []string
	deletedIDs   []int64
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		flags:        make(map[string]featuressqlc.FeatureFlag),
		nextTargetID: 1,
	}
}

func (r *fakeRepository) ListFlags(context.Context) ([]featuressqlc.FeatureFlag, error) {
	out := make([]featuressqlc.FeatureFlag, 0, len(r.flags))
	for _, flag := range r.flags {
		out = append(out, flag)
	}
	return out, nil
}

func (r *fakeRepository) GetFlag(_ context.Context, key string) (featuressqlc.FeatureFlag, error) {
	flag, ok := r.flags[key]
	if !ok {
		return featuressqlc.FeatureFlag{}, apperror.ErrNotFound
	}
	return flag, nil
}

func (r *fakeRepository) UpsertFlag(_ context.Context, params featuressqlc.UpsertFeatureFlagParams) (featuressqlc.FeatureFlag, error) {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	flag := featuressqlc.FeatureFlag{
		FlagKey:     params.FlagKey,
		Enabled:     params.Enabled,
		KillSwitch:  params.KillSwitch,
		Description: params.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if existing, ok := r.flags[params.FlagKey]; ok {
		flag.CreatedAt = existing.CreatedAt
	}
	r.flags[params.FlagKey] = flag
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

func (r *fakeRepository) ListTargets(ctx context.Context, key string) ([]featuressqlc.FeatureFlagTarget, error) {
	tid := tenant.EffectiveID(ctx)
	out := make([]featuressqlc.FeatureFlagTarget, 0, len(r.targets))
	for _, target := range r.targets {
		if target.TenantID == tid && target.FlagKey == key {
			out = append(out, target)
		}
	}
	return out, nil
}

func (r *fakeRepository) UpsertTarget(ctx context.Context, key string, userID int64, enabled bool) (featuressqlc.FeatureFlagTarget, error) {
	r.lastTenantID = tenant.EffectiveID(ctx)
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	target := featuressqlc.FeatureFlagTarget{
		ID:        r.nextTargetID,
		FlagKey:   key,
		TenantID:  r.lastTenantID,
		Enabled:   enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if userID != 0 {
		target.UserID = pgtype.Int8{Int64: userID, Valid: true}
	}
	for i := range r.targets {
		existing := r.targets[i]
		if existing.FlagKey == target.FlagKey && existing.TenantID == target.TenantID &&
			existing.UserID.Valid == target.UserID.Valid &&
			(!target.UserID.Valid || existing.UserID.Int64 == target.UserID.Int64) {
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
	repo.flags[string(features.KeyUserRegistration)] = featuressqlc.FeatureFlag{
		FlagKey:     string(features.KeyUserRegistration),
		Enabled:     false,
		KillSwitch:  true,
		Description: "custom",
	}
	evaluator := &fakeEvaluator{result: features.Result{Enabled: true, Source: features.SourceUserTarget}}
	svc := New(repo, evaluator)

	flags, err := svc.ListFlags(tenantCtx(2), features.Subject{TenantID: 2, UserID: 7})
	if err != nil {
		t.Fatalf("ListFlags: %v", err)
	}
	if len(flags) != len(features.Catalog()) {
		t.Fatalf("ListFlags returned %d flags, want %d", len(flags), len(features.Catalog()))
	}
	for _, flag := range flags {
		if flag.Key != string(features.KeyUserRegistration) {
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
	repo.targets = []featuressqlc.FeatureFlagTarget{
		{ID: 1, FlagKey: string(features.KeyFileUploads), TenantID: 1, Enabled: false},
		{ID: 2, FlagKey: string(features.KeyFileUploads), TenantID: 2, Enabled: true},
	}
	svc := New(repo, &fakeEvaluator{})

	detail, err := svc.GetFlag(tenantCtx(2), string(features.KeyFileUploads), features.Subject{TenantID: 2})
	if err != nil {
		t.Fatalf("GetFlag: %v", err)
	}
	if detail.Flag.Key != string(features.KeyFileUploads) || detail.Flag.Configured {
		t.Fatalf("flag = %+v, want compiled file_uploads", detail.Flag)
	}
	if len(detail.Targets) != 1 || detail.Targets[0].ID != 2 || detail.Targets[0].TenantID != 2 {
		t.Fatalf("targets = %+v, want only tenant 2 target", detail.Targets)
	}
}

func TestUpsertFlagPreservesExistingDescriptionAndInvalidates(t *testing.T) {
	repo := newFakeRepository()
	repo.flags[string(features.KeyFileUploads)] = featuressqlc.FeatureFlag{
		FlagKey:     string(features.KeyFileUploads),
		Enabled:     true,
		Description: "custom upload policy",
	}
	evaluator := &fakeEvaluator{result: features.Result{Enabled: false, Source: features.SourceKillSwitch}}
	svc := New(repo, evaluator)
	enabled, kill := false, true

	flag, err := svc.UpsertFlag(tenantCtx(1), string(features.KeyFileUploads), featuresdto.UpsertFlagRequest{
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
	svc := New(repo, &fakeEvaluator{})
	enabled, kill := true, false

	flag, err := svc.UpsertFlag(tenantCtx(1), string(features.KeyGoogleOAuth), featuresdto.UpsertFlagRequest{
		Enabled:    &enabled,
		KillSwitch: &kill,
	}, features.Subject{TenantID: 1})
	if err != nil {
		t.Fatalf("UpsertFlag: %v", err)
	}
	def, _ := features.Lookup(string(features.KeyGoogleOAuth))
	if flag.Description != def.Description {
		t.Fatalf("description = %q, want catalog description %q", flag.Description, def.Description)
	}
}

func TestUpsertTargetUsesRequestTenant(t *testing.T) {
	repo := newFakeRepository()
	svc := New(repo, &fakeEvaluator{})
	userID := int64(42)
	enabled := true

	target, err := svc.UpsertTarget(tenantCtx(3), string(features.KeyFileUploads), featuresdto.UpsertTargetRequest{
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
	svc := New(repo, evaluator)

	if err := svc.DeleteFlag(tenantCtx(1), string(features.KeyFileUploads)); err != nil {
		t.Fatalf("DeleteFlag missing row: %v", err)
	}
	if evaluator.invalidations != 1 {
		t.Fatalf("invalidations = %d, want 1", evaluator.invalidations)
	}
}

func TestDeleteTargetNotFoundIs404(t *testing.T) {
	repo := newFakeRepository()
	evaluator := &fakeEvaluator{}
	svc := New(repo, evaluator)

	err := svc.DeleteTarget(tenantCtx(1), string(features.KeyFileUploads), 99)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 404 {
		t.Fatalf("DeleteTarget error = %v, want 404", err)
	}
	if evaluator.invalidations != 0 {
		t.Fatalf("invalidations = %d, want 0", evaluator.invalidations)
	}
}

func TestUnknownFlagKeyIs404(t *testing.T) {
	svc := New(newFakeRepository(), &fakeEvaluator{})
	ctx := tenantCtx(1)

	if _, err := svc.GetFlag(ctx, "typo", features.Subject{}); !isNotFound(err) {
		t.Fatalf("GetFlag error = %v, want 404", err)
	}
	if _, err := svc.UpsertFlag(ctx, "typo", featuresdto.UpsertFlagRequest{}, features.Subject{}); !isNotFound(err) {
		t.Fatalf("UpsertFlag error = %v, want 404", err)
	}
	if err := svc.DeleteFlag(ctx, "typo"); !isNotFound(err) {
		t.Fatalf("DeleteFlag error = %v, want 404", err)
	}
	if _, err := svc.ListTargets(ctx, "typo"); !isNotFound(err) {
		t.Fatalf("ListTargets error = %v, want 404", err)
	}
	if _, err := svc.UpsertTarget(ctx, "typo", featuresdto.UpsertTargetRequest{}); !isNotFound(err) {
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
