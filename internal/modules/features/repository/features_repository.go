// Package repository owns persistence for the feature-flag admin module.
// Every target query is scoped to the request tenant; the evaluator's
// process-wide store lives in internal/platform/features.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	featuressqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// FeatureRepository implements service.Repository over sqlc and maps rows to
// service types.
type FeatureRepository struct {
	q *featuressqlc.Queries
}

var _ featuressvc.Repository = (*FeatureRepository)(nil)

// New returns a repository backed by db.
func New(db featuressqlc.DBTX) *FeatureRepository {
	return &FeatureRepository{q: featuressqlc.New(db)}
}

func (r *FeatureRepository) ListFlags(ctx context.Context) ([]featuressvc.Flag, error) {
	rows, err := r.q.ListFeatureFlags(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]featuressvc.Flag, len(rows))
	for i := range rows {
		out[i] = toFlag(rows[i])
	}
	return out, nil
}

func (r *FeatureRepository) GetFlag(ctx context.Context, key string) (featuressvc.Flag, error) {
	row, err := r.q.GetFeatureFlag(ctx, key)
	if err != nil {
		return featuressvc.Flag{}, wrapErr(err)
	}
	return toFlag(row), nil
}

func (r *FeatureRepository) CreateFlag(ctx context.Context, key string, write featuressvc.FlagWrite) (featuressvc.Flag, error) {
	row, err := r.q.InsertFeatureFlag(ctx, featuressqlc.InsertFeatureFlagParams{
		FlagKey:     key,
		Enabled:     boolParam(write.Enabled),
		KillSwitch:  write.KillSwitch,
		Description: write.Description,
	})
	if err != nil {
		if database.IsUniqueViolation(err) {
			return featuressvc.Flag{}, apperror.NewConflict("feature flag already exists; update it instead")
		}
		return featuressvc.Flag{}, wrapErr(err)
	}
	return toFlag(row), nil
}

// CreateFlagIfAbsent inserts a row only while none exists. A concurrent
// create makes the precondition fail with 412 instead of overwriting it.
func (r *FeatureRepository) CreateFlagIfAbsent(ctx context.Context, key string, write featuressvc.FlagWrite) (featuressvc.Flag, error) {
	row, err := r.q.InsertFeatureFlagIfAbsent(ctx, featuressqlc.InsertFeatureFlagIfAbsentParams{
		FlagKey:     key,
		Enabled:     boolParam(write.Enabled),
		KillSwitch:  write.KillSwitch,
		Description: write.Description,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return featuressvc.Flag{}, preconditionFailed()
		}
		return featuressvc.Flag{}, wrapErr(err)
	}
	return toFlag(row), nil
}

func (r *FeatureRepository) UpsertFlag(ctx context.Context, key string, write featuressvc.FlagWrite) (featuressvc.Flag, error) {
	row, err := r.q.UpsertFeatureFlag(ctx, featuressqlc.UpsertFeatureFlagParams{
		FlagKey:     key,
		Enabled:     boolParam(write.Enabled),
		KillSwitch:  write.KillSwitch,
		Description: write.Description,
	})
	if err != nil {
		return featuressvc.Flag{}, wrapErr(err)
	}
	return toFlag(row), nil
}

// UpdateFlagIfUnchanged writes the row only while its updated_at still matches
// the validator the caller read, so a concurrent update cannot be lost.
func (r *FeatureRepository) UpdateFlagIfUnchanged(ctx context.Context, key string, write featuressvc.FlagWrite, expected time.Time) (featuressvc.Flag, error) {
	row, err := r.q.UpdateFeatureFlagIfUnchanged(ctx, featuressqlc.UpdateFeatureFlagIfUnchangedParams{
		Enabled:           boolParam(write.Enabled),
		KillSwitch:        write.KillSwitch,
		Description:       write.Description,
		FlagKey:           key,
		ExpectedUpdatedAt: timestamptzParam(expected),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return featuressvc.Flag{}, preconditionFailed()
		}
		return featuressvc.Flag{}, wrapErr(err)
	}
	return toFlag(row), nil
}

// DeleteFlagIfUnchanged deletes the row only while its updated_at still matches
// the validator the caller read.
func (r *FeatureRepository) DeleteFlagIfUnchanged(ctx context.Context, key string, expected time.Time) error {
	if _, err := r.q.DeleteFeatureFlagIfUnchanged(ctx, featuressqlc.DeleteFeatureFlagIfUnchangedParams{
		FlagKey:           key,
		ExpectedUpdatedAt: timestamptzParam(expected),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return preconditionFailed()
		}
		return wrapErr(err)
	}
	return nil
}

// DeleteFlag relies on the RESTRICT foreign key instead of a count-then-delete
// check, so a target inserted concurrently in any tenant still blocks the
// delete. The conflict deliberately does not say which tenants own targets.
func (r *FeatureRepository) DeleteFlag(ctx context.Context, key string) error {
	if _, err := r.q.DeleteFeatureFlag(ctx, key); err != nil {
		if constraint, ok := database.ForeignKeyViolation(err); ok && constraint == flagKeyFK {
			return apperror.NewConflict("feature flag has targets; delete them before deleting the flag")
		}
		return wrapErr(err)
	}
	return nil
}

func (r *FeatureRepository) ListTargets(ctx context.Context, key string) ([]featuressvc.Target, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListFeatureFlagTargets(ctx, featuressqlc.ListFeatureFlagTargetsParams{
		TenantID: tenantID,
		FlagKey:  key,
	})
	if err != nil {
		return nil, err
	}
	out := make([]featuressvc.Target, len(rows))
	for i := range rows {
		out[i] = toTarget(rows[i])
	}
	return out, nil
}

// ListAllTargets is the platform cross-tenant view: it bypasses row-level
// security for this one statement. The service checks the process-wide read
// permission before calling it.
func (r *FeatureRepository) ListAllTargets(ctx context.Context, key string) ([]featuressvc.Target, error) {
	rows, err := r.q.ListAllFeatureFlagTargets(database.WithRLSBypass(ctx), key)
	if err != nil {
		return nil, err
	}
	out := make([]featuressvc.Target, len(rows))
	for i := range rows {
		out[i] = toTarget(rows[i])
	}
	return out, nil
}

// UpsertTarget writes the target in tenantID. A tenant other than the request
// tenant is the platform cross-tenant form and runs on the bypass pool for
// this statement; the service has already authorized that write.
func (r *FeatureRepository) UpsertTarget(ctx context.Context, key string, tenantID uuid.UUID, userID *uuid.UUID, enabled bool) (featuressvc.Target, error) {
	ctx = crossTenantScope(ctx, tenantID)
	if userID == nil {
		row, err := r.q.UpsertFeatureFlagTenantTarget(ctx, featuressqlc.UpsertFeatureFlagTenantTargetParams{
			FlagKey:  key,
			TenantID: tenantID,
			Enabled:  enabled,
		})
		if err != nil {
			return featuressvc.Target{}, wrapErr(err)
		}
		return toTarget(row), nil
	}

	row, err := r.q.UpsertFeatureFlagUserTarget(ctx, featuressqlc.UpsertFeatureFlagUserTargetParams{
		FlagKey:  key,
		TenantID: tenantID,
		UserID:   uuid.NullUUID{UUID: *userID, Valid: true},
		Enabled:  enabled,
	})
	if err != nil {
		return featuressvc.Target{}, wrapErr(err)
	}
	return toTarget(row), nil
}

func (r *FeatureRepository) DeleteTarget(ctx context.Context, key string, tenantID uuid.UUID, userID *uuid.UUID) error {
	ctx = crossTenantScope(ctx, tenantID)
	if userID == nil {
		if _, err := r.q.DeleteFeatureFlagTenantTarget(ctx, featuressqlc.DeleteFeatureFlagTenantTargetParams{
			FlagKey:  key,
			TenantID: tenantID,
		}); err != nil {
			return wrapErr(err)
		}
		return nil
	}
	if _, err := r.q.DeleteFeatureFlagUserTarget(ctx, featuressqlc.DeleteFeatureFlagUserTargetParams{
		FlagKey:  key,
		TenantID: tenantID,
		UserID:   uuid.NullUUID{UUID: *userID, Valid: true},
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

// crossTenantScope marks a statement that targets a different tenant than the
// request for the row-level-security bypass. The caller has authorized the
// cross-tenant capability; a request-tenant write keeps the normal scope.
func crossTenantScope(ctx context.Context, tenantID uuid.UUID) context.Context {
	if tenantID == tenant.IDFromContext(ctx) {
		return ctx
	}
	return database.WithRLSBypass(ctx)
}

func toFlag(row featuressqlc.FeatureFlag) featuressvc.Flag {
	flag := featuressvc.Flag{
		Key:         row.FlagKey,
		Description: row.Description,
		KillSwitch:  row.KillSwitch,
		CreatedAt:   timePtr(row.CreatedAt),
		UpdatedAt:   timePtr(row.UpdatedAt),
	}
	if row.Enabled.Valid {
		flag.Enabled = row.Enabled.Bool
		flag.Configured = true
	}
	return flag
}

func toTarget(row featuressqlc.FeatureFlagTarget) featuressvc.Target {
	target := featuressvc.Target{
		ID:        row.ID,
		FlagKey:   row.FlagKey,
		TenantID:  row.TenantID,
		Enabled:   row.Enabled,
		CreatedAt: timePtr(row.CreatedAt),
		UpdatedAt: timePtr(row.UpdatedAt),
	}
	if row.UserID.Valid {
		userID := row.UserID.UUID
		target.UserID = &userID
	}
	return target
}

func boolParam(value *bool) pgtype.Bool {
	if value == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *value, Valid: true}
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// timestamptzParam binds a validator timestamp.
func timestamptzParam(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

// preconditionFailed is the 412 a conditional write returns when the row no
// longer matches the caller's validator.
func preconditionFailed() error {
	return apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
}

// flagKeyFK is the feature_flag_targets -> feature_flags constraint. It is ON
// DELETE RESTRICT, so it fires when a flag is deleted while targets exist (in
// any tenant), or when a target upsert races a concurrent flag delete.
const flagKeyFK = "fk_feature_flag_targets_flag_key"

// wrapErr translates persistence errors into application sentinels without
// leaking SQL or driver types to the service.
func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	if constraint, ok := database.ForeignKeyViolation(err); ok {
		if constraint == flagKeyFK {
			// A target upsert lost a race with a concurrent flag delete.
			return apperror.NewConflict("feature flag was modified concurrently; retry the request")
		}
		// A target may only reference a membership in its own tenant. A
		// cross-tenant or unknown user/tenant id behaves as not found.
		return apperror.ErrNotFound
	}
	if database.IsUniqueViolation(err) {
		return apperror.NewConflict("feature flag target already exists")
	}
	return err
}
