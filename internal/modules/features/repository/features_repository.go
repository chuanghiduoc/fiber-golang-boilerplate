// Package repository owns persistence for the feature-flag admin module.
// Every target query is scoped to the request tenant; the evaluator's
// process-wide store lives in internal/platform/features.
package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	featuressqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Repository is the feature-flag admin persistence contract.
type Repository interface {
	ListFlags(ctx context.Context) ([]featuressqlc.FeatureFlag, error)
	GetFlag(ctx context.Context, key string) (featuressqlc.FeatureFlag, error)
	UpsertFlag(ctx context.Context, params featuressqlc.UpsertFeatureFlagParams) (featuressqlc.FeatureFlag, error)
	DeleteFlag(ctx context.Context, key string) error
	ListTargets(ctx context.Context, key string) ([]featuressqlc.FeatureFlagTarget, error)
	UpsertTarget(ctx context.Context, key string, userID int64, enabled bool) (featuressqlc.FeatureFlagTarget, error)
	DeleteTarget(ctx context.Context, key string, id int64) error
}

type featureRepository struct {
	q *featuressqlc.Queries
}

// New returns a repository backed by db.
func New(db featuressqlc.DBTX) Repository {
	return &featureRepository{q: featuressqlc.New(db)}
}

func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

func (r *featureRepository) ListFlags(ctx context.Context) ([]featuressqlc.FeatureFlag, error) {
	return r.q.ListFeatureFlags(ctx)
}

func (r *featureRepository) GetFlag(ctx context.Context, key string) (featuressqlc.FeatureFlag, error) {
	flag, err := r.q.GetFeatureFlag(ctx, key)
	if err != nil {
		return featuressqlc.FeatureFlag{}, wrapErr(err)
	}
	return flag, nil
}

func (r *featureRepository) UpsertFlag(ctx context.Context, params featuressqlc.UpsertFeatureFlagParams) (featuressqlc.FeatureFlag, error) {
	flag, err := r.q.UpsertFeatureFlag(ctx, params)
	if err != nil {
		return featuressqlc.FeatureFlag{}, wrapErr(err)
	}
	return flag, nil
}

// DeleteFlag relies on the RESTRICT foreign key instead of a count-then-delete
// check, so a target inserted concurrently in any tenant still blocks the
// delete. The conflict deliberately does not say which tenants own targets.
func (r *featureRepository) DeleteFlag(ctx context.Context, key string) error {
	if _, err := r.q.DeleteFeatureFlag(ctx, key); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == flagKeyFK {
			return apperror.NewConflict("feature flag has targets; delete them before deleting the flag")
		}
		return wrapErr(err)
	}
	return nil
}

func (r *featureRepository) ListTargets(ctx context.Context, key string) ([]featuressqlc.FeatureFlagTarget, error) {
	return r.q.ListFeatureFlagTargets(ctx, featuressqlc.ListFeatureFlagTargetsParams{
		TenantID: tenantID(ctx),
		FlagKey:  key,
	})
}

func (r *featureRepository) UpsertTarget(ctx context.Context, key string, userID int64, enabled bool) (featuressqlc.FeatureFlagTarget, error) {
	if userID == 0 {
		target, err := r.q.UpsertFeatureFlagTenantTarget(ctx, featuressqlc.UpsertFeatureFlagTenantTargetParams{
			FlagKey:  key,
			TenantID: tenantID(ctx),
			Enabled:  enabled,
		})
		if err != nil {
			return featuressqlc.FeatureFlagTarget{}, wrapErr(err)
		}
		return target, nil
	}

	target, err := r.q.UpsertFeatureFlagUserTarget(ctx, featuressqlc.UpsertFeatureFlagUserTargetParams{
		FlagKey:  key,
		TenantID: tenantID(ctx),
		UserID:   pgtype.Int8{Int64: userID, Valid: true},
		Enabled:  enabled,
	})
	if err != nil {
		return featuressqlc.FeatureFlagTarget{}, wrapErr(err)
	}
	return target, nil
}

func (r *featureRepository) DeleteTarget(ctx context.Context, key string, id int64) error {
	if _, err := r.q.DeleteFeatureFlagTarget(ctx, featuressqlc.DeleteFeatureFlagTargetParams{
		ID:       id,
		TenantID: tenantID(ctx),
		FlagKey:  key,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
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
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503": // foreign_key_violation
			if pgErr.ConstraintName == flagKeyFK {
				// A target upsert lost a race with a concurrent flag delete.
				return apperror.NewConflict("feature flag was modified concurrently; retry the request")
			}
			return apperror.NewBadRequest("target references a user or tenant outside the current scope")
		case "23505": // unique_violation
			return apperror.NewConflict("feature flag target already exists")
		}
	}
	return err
}
