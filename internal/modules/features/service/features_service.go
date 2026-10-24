// Package service owns feature-flag administration rules. Evaluation
// precedence and caching live in internal/platform/features; this service
// validates compile-time keys, scopes targets to the request tenant, and
// invalidates the local snapshot after a successful write.
package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featuressqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Service is the feature-flag administration surface.
type Service interface {
	ListFlags(ctx context.Context, subject features.Subject) ([]featuresdto.FlagResponse, error)
	GetFlag(ctx context.Context, key string, subject features.Subject) (*featuresdto.FlagDetailResponse, error)
	UpsertFlag(ctx context.Context, key string, req featuresdto.UpsertFlagRequest, subject features.Subject) (*featuresdto.FlagResponse, error)
	DeleteFlag(ctx context.Context, key string) error
	ListTargets(ctx context.Context, key string) ([]featuresdto.TargetResponse, error)
	UpsertTarget(ctx context.Context, key string, req featuresdto.UpsertTargetRequest) (*featuresdto.TargetResponse, error)
	DeleteTarget(ctx context.Context, key string, id int64) error
}

type service struct {
	repository Repository
	flags      features.Evaluator
}

// Repository is the persistence surface the service needs.
type Repository interface {
	ListFlags(ctx context.Context) ([]featuressqlc.FeatureFlag, error)
	GetFlag(ctx context.Context, key string) (featuressqlc.FeatureFlag, error)
	UpsertFlag(ctx context.Context, params featuressqlc.UpsertFeatureFlagParams) (featuressqlc.FeatureFlag, error)
	DeleteFlag(ctx context.Context, key string) error
	ListTargets(ctx context.Context, key string) ([]featuressqlc.FeatureFlagTarget, error)
	UpsertTarget(ctx context.Context, key string, userID int64, enabled bool) (featuressqlc.FeatureFlagTarget, error)
	DeleteTarget(ctx context.Context, key string, id int64) error
}

// New returns a service. A nil evaluator falls back to compiled/environment
// defaults so tests and minimal deployments stay nil-safe.
func New(repository Repository, flags features.Evaluator) Service {
	if flags == nil {
		flags = features.NewEvaluator(features.Options{})
	}
	return &service{repository: repository, flags: flags}
}

func (s *service) ListFlags(ctx context.Context, subject features.Subject) ([]featuresdto.FlagResponse, error) {
	rows, err := s.repository.ListFlags(ctx)
	if err != nil {
		return nil, err
	}
	byKey := make(map[features.Key]featuressqlc.FeatureFlag, len(rows))
	for _, row := range rows {
		byKey[features.Key(row.FlagKey)] = row
	}

	definitions := features.Catalog()
	out := make([]featuresdto.FlagResponse, 0, len(definitions))
	for _, def := range definitions {
		row, configured := byKey[def.Key]
		var rowPtr *featuressqlc.FeatureFlag
		if configured {
			rowPtr = &row
		}
		out = append(out, s.flagResponse(ctx, def, rowPtr, subject))
	}
	return out, nil
}

func (s *service) GetFlag(ctx context.Context, key string, subject features.Subject) (*featuresdto.FlagDetailResponse, error) {
	def, err := lookup(key)
	if err != nil {
		return nil, err
	}

	row, _, err := s.getRow(ctx, key)
	if err != nil {
		return nil, err
	}
	targets, err := s.repository.ListTargets(ctx, key)
	if err != nil {
		return nil, err
	}

	return &featuresdto.FlagDetailResponse{
		Flag:    s.flagResponse(ctx, def, row, subject),
		Targets: toTargetResponses(targets),
	}, nil
}

func (s *service) UpsertFlag(ctx context.Context, key string, req featuresdto.UpsertFlagRequest, subject features.Subject) (*featuresdto.FlagResponse, error) {
	def, err := lookup(key)
	if err != nil {
		return nil, err
	}
	if req.Enabled == nil || req.KillSwitch == nil {
		return nil, apperror.NewBadRequest("enabled and killSwitch are required")
	}

	description := def.Description
	existing, configured, err := s.getRow(ctx, key)
	if err != nil {
		return nil, err
	}
	if configured {
		description = existing.Description
	}
	if req.Description != nil {
		description = *req.Description
	}

	row, err := s.repository.UpsertFlag(ctx, featuressqlc.UpsertFeatureFlagParams{
		FlagKey:     key,
		Enabled:     *req.Enabled,
		KillSwitch:  *req.KillSwitch,
		Description: description,
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx)

	resp := s.flagResponse(ctx, def, &row, subject)
	return &resp, nil
}

func (s *service) DeleteFlag(ctx context.Context, key string) error {
	if _, err := lookup(key); err != nil {
		return err
	}
	if err := s.repository.DeleteFlag(ctx, key); err != nil && !errors.Is(err, apperror.ErrNotFound) {
		return err
	}
	s.invalidate(ctx)
	return nil
}

func (s *service) ListTargets(ctx context.Context, key string) ([]featuresdto.TargetResponse, error) {
	if _, err := lookup(key); err != nil {
		return nil, err
	}
	targets, err := s.repository.ListTargets(ctx, key)
	if err != nil {
		return nil, err
	}
	return toTargetResponses(targets), nil
}

func (s *service) UpsertTarget(ctx context.Context, key string, req featuresdto.UpsertTargetRequest) (*featuresdto.TargetResponse, error) {
	if _, err := lookup(key); err != nil {
		return nil, err
	}
	if req.Enabled == nil {
		return nil, apperror.NewBadRequest("enabled is required")
	}

	var userID int64
	if req.UserID != nil {
		userID = *req.UserID
		if userID < 1 {
			return nil, apperror.NewBadRequest("userId must be positive")
		}
	}

	target, err := s.repository.UpsertTarget(ctx, key, userID, *req.Enabled)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx)

	resp := toTargetResponse(target)
	return &resp, nil
}

func (s *service) DeleteTarget(ctx context.Context, key string, id int64) error {
	if _, err := lookup(key); err != nil {
		return err
	}
	if err := s.repository.DeleteTarget(ctx, key, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("feature flag target not found")
		}
		return err
	}
	s.invalidate(ctx)
	return nil
}

func (s *service) getRow(ctx context.Context, key string) (*featuressqlc.FeatureFlag, bool, error) {
	row, err := s.repository.GetFlag(ctx, key)
	if err == nil {
		return &row, true, nil
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return nil, false, nil
	}
	return nil, false, err
}

// invalidate is best-effort: the database write is already committed. A stale
// local snapshot converges at the next TTL expiry even if invalidation fails.
func (s *service) invalidate(ctx context.Context) {
	if err := s.flags.Invalidate(ctx); err != nil {
		slog.Warn("failed to invalidate feature flag cache", slog.Any("error", err))
	}
}

func (s *service) flagResponse(ctx context.Context, def features.Definition, row *featuressqlc.FeatureFlag, subject features.Subject) featuresdto.FlagResponse {
	resp := featuresdto.FlagResponse{
		Key:         string(def.Key),
		Description: def.Description,
		Enabled:     def.Default,
		Configured:  false,
	}
	if row != nil {
		resp.Description = row.Description
		resp.Enabled = row.Enabled
		resp.KillSwitch = row.KillSwitch
		resp.Configured = true
		resp.CreatedAt = timePtr(row.CreatedAt)
		resp.UpdatedAt = timePtr(row.UpdatedAt)
	}

	result := s.flags.Evaluate(ctx, def.Key, subject)
	resp.Effective = result.Enabled
	resp.Source = string(result.Source)
	return resp
}

func lookup(key string) (features.Definition, error) {
	def, ok := features.Lookup(key)
	if !ok {
		return features.Definition{}, apperror.NewNotFound("feature flag not found")
	}
	return def, nil
}

func toTargetResponses(targets []featuressqlc.FeatureFlagTarget) []featuresdto.TargetResponse {
	out := make([]featuresdto.TargetResponse, len(targets))
	for i := range targets {
		out[i] = toTargetResponse(targets[i])
	}
	return out
}

func toTargetResponse(target featuressqlc.FeatureFlagTarget) featuresdto.TargetResponse {
	resp := featuresdto.TargetResponse{
		ID:        target.ID,
		FlagKey:   target.FlagKey,
		TenantID:  target.TenantID,
		Enabled:   target.Enabled,
		CreatedAt: timePtr(target.CreatedAt),
		UpdatedAt: timePtr(target.UpdatedAt),
	}
	if target.UserID.Valid {
		userID := target.UserID.Int64
		resp.UserID = &userID
	}
	return resp
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}
