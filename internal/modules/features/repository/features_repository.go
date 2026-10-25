// Package repository owns persistence for the feature-flag admin module.
// Every target query is scoped to the request tenant; the evaluator's
// process-wide store lives in internal/platform/features.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	featuressqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
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

func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
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
	rows, err := r.q.ListFeatureFlagTargets(ctx, featuressqlc.ListFeatureFlagTargetsParams{
		TenantID: tenantID(ctx),
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

func (r *FeatureRepository) UpsertTarget(ctx context.Context, key string, userID int64, enabled bool) (featuressvc.Target, error) {
	if userID == 0 {
		row, err := r.q.UpsertFeatureFlagTenantTarget(ctx, featuressqlc.UpsertFeatureFlagTenantTargetParams{
			FlagKey:  key,
			TenantID: tenantID(ctx),
			Enabled:  enabled,
		})
		if err != nil {
			return featuressvc.Target{}, wrapErr(err)
		}
		return toTarget(row), nil
	}

	row, err := r.q.UpsertFeatureFlagUserTarget(ctx, featuressqlc.UpsertFeatureFlagUserTargetParams{
		FlagKey:  key,
		TenantID: tenantID(ctx),
		UserID:   pgtype.Int8{Int64: userID, Valid: true},
		Enabled:  enabled,
	})
	if err != nil {
		return featuressvc.Target{}, wrapErr(err)
	}
	return toTarget(row), nil
}

func (r *FeatureRepository) DeleteTarget(ctx context.Context, key string, id int64) error {
	if _, err := r.q.DeleteFeatureFlagTarget(ctx, featuressqlc.DeleteFeatureFlagTargetParams{
		ID:       id,
		TenantID: tenantID(ctx),
		FlagKey:  key,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
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
		userID := row.UserID.Int64
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

// flagKeyFK is the feature_flag_targets -> feature_flags constraint. It is ON
// DELETE RESTRICT, so it fires when a flag is deleted while targets exist (in
// any tenant), or when a target upsert races a concurrent flag delete.
const flagKeyFK = "feature_flag_targets_flag_key_fk"

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
		return apperror.NewBadRequest("target references a user or tenant outside the current scope")
	}
	if database.IsUniqueViolation(err) {
		return apperror.NewConflict("feature flag target already exists")
	}
	return err
}
