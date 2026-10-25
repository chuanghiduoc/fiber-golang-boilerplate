// Package service owns feature-flag administration rules. Evaluation
// precedence and caching live in internal/platform/features; this service
// validates compile-time keys, restricts process-wide flag writes to a
// platform role, scopes targets to the request tenant, and invalidates the
// local snapshot after a successful write.
package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Audit actions for the domain events written after each mutation. The
// route-level audithttp.Guard intent row is the fail-closed record; these events
// carry the flag-specific metadata and link to it through ParentID.
const (
	actionFlagUpdated   = "admin.feature_flag.updated"
	actionFlagDeleted   = "admin.feature_flag.deleted"
	actionTargetUpdated = "admin.feature_flag_target.updated"
	actionTargetDeleted = "admin.feature_flag_target.deleted"
	resourceFlag        = "feature_flag"
	resourceTarget      = "feature_flag_target"
)

// Service is the feature-flag administration surface.
type Service interface {
	ListFlags(ctx context.Context, subject features.Subject) ([]Flag, error)
	GetFlag(ctx context.Context, key string, subject features.Subject) (*FlagDetail, error)
	UpsertFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject) (*Flag, error)
	DeleteFlag(ctx context.Context, key string) error
	ListTargets(ctx context.Context, key string) ([]Target, error)
	UpsertTarget(ctx context.Context, key string, in UpsertTargetInput) (*Target, error)
	DeleteTarget(ctx context.Context, key string, id int64) error
}

type service struct {
	repository Repository
	flags      features.Evaluator
	audit      audit.Recorder
}

// New returns a service. A nil evaluator falls back to compiled/environment
// defaults and a nil recorder discards domain events, so tests and minimal
// deployments stay nil-safe.
func New(repository Repository, flags features.Evaluator, recorder audit.Recorder) Service {
	if flags == nil {
		flags = features.NewEvaluator(features.Options{})
	}
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &service{repository: repository, flags: flags, audit: recorder}
}

func (s *service) ListFlags(ctx context.Context, subject features.Subject) ([]Flag, error) {
	rows, err := s.repository.ListFlags(ctx)
	if err != nil {
		return nil, err
	}
	byKey := make(map[features.Key]Flag, len(rows))
	for _, row := range rows {
		byKey[features.Key(row.Key)] = row
	}

	definitions := features.Catalog()
	out := make([]Flag, 0, len(definitions))
	for _, def := range definitions {
		row, configured := byKey[def.Key]
		var rowPtr *Flag
		if configured {
			rowPtr = &row
		}
		out = append(out, s.flagResponse(ctx, def, rowPtr, subject))
	}
	return out, nil
}

func (s *service) GetFlag(ctx context.Context, key string, subject features.Subject) (*FlagDetail, error) {
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

	return &FlagDetail{
		Flag:    s.flagResponse(ctx, def, row, subject),
		Targets: targets,
	}, nil
}

func (s *service) UpsertFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject) (*Flag, error) {
	def, err := lookup(key)
	if err != nil {
		return nil, err
	}
	if err := requirePlatformAdmin(ctx); err != nil {
		return nil, err
	}
	if in.KillSwitch == nil {
		return nil, apperror.NewBadRequest("killSwitch is required")
	}

	description := def.Description
	existing, configured, err := s.getRow(ctx, key)
	if err != nil {
		return nil, err
	}
	if configured && existing.Description != "" {
		description = existing.Description
	}
	if in.Description != nil {
		description = *in.Description
	}

	// A nil Enabled stores NULL: the row keeps its kill switch and targets but
	// has no database default, so environment/compiled defaults apply.
	row, err := s.repository.UpsertFlag(ctx, key, FlagWrite{
		Enabled:     in.Enabled,
		KillSwitch:  *in.KillSwitch,
		Description: description,
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx)

	resp := s.flagResponse(ctx, def, &row, subject)
	metadata := map[string]any{
		"feature":    resp.Key,
		"killSwitch": resp.KillSwitch,
		"source":     resp.Source,
	}
	if row.Configured {
		metadata["enabled"] = row.Enabled
	}
	s.recordEvent(ctx, actionFlagUpdated, resourceFlag, key, metadata)
	return &resp, nil
}

func (s *service) DeleteFlag(ctx context.Context, key string) error {
	if _, err := lookup(key); err != nil {
		return err
	}
	if err := requirePlatformAdmin(ctx); err != nil {
		return err
	}
	// Targets reference the flag with ON DELETE RESTRICT; the repository maps
	// that violation to 409 without naming the tenants that own them.
	if err := s.repository.DeleteFlag(ctx, key); err != nil && !errors.Is(err, apperror.ErrNotFound) {
		return err
	}
	s.invalidate(ctx)
	s.recordEvent(ctx, actionFlagDeleted, resourceFlag, key, map[string]any{"feature": key})
	return nil
}

func (s *service) ListTargets(ctx context.Context, key string) ([]Target, error) {
	if _, err := lookup(key); err != nil {
		return nil, err
	}
	return s.repository.ListTargets(ctx, key)
}

func (s *service) UpsertTarget(ctx context.Context, key string, in UpsertTargetInput) (*Target, error) {
	if _, err := lookup(key); err != nil {
		return nil, err
	}
	if in.Enabled == nil {
		return nil, apperror.NewBadRequest("enabled is required")
	}

	var userID int64
	if in.UserID != nil {
		userID = *in.UserID
		if userID < 1 {
			return nil, apperror.NewBadRequest("userId must be positive")
		}
	}

	target, err := s.repository.UpsertTarget(ctx, key, userID, *in.Enabled)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx)

	metadata := map[string]any{
		"feature":  target.FlagKey,
		"targetId": target.ID,
		"enabled":  target.Enabled,
	}
	if target.UserID != nil {
		metadata["userId"] = *target.UserID
	}
	s.recordEvent(ctx, actionTargetUpdated, resourceTarget, strconv.FormatInt(target.ID, 10), metadata)
	return &target, nil
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
	s.recordEvent(ctx, actionTargetDeleted, resourceTarget, strconv.FormatInt(id, 10),
		map[string]any{"feature": key, "targetId": id})
	return nil
}

// requirePlatformAdmin restricts process-wide flag rows (database default,
// kill switch, delete) to a platform role assignment: those writes change
// behavior for every tenant.
func requirePlatformAdmin(ctx context.Context) error {
	return authz.Authorize(ctx, authz.PermissionPlatformFeaturesWrite, authz.Resource{}, authz.AllowPolicy{})
}

func (s *service) getRow(ctx context.Context, key string) (*Flag, bool, error) {
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

// recordEvent writes the post-commit domain event. It cannot roll the
// committed write back, so a failure is logged; the route's required
// audithttp.Guard intent row still records the mutation and its outcome.
func (s *service) recordEvent(ctx context.Context, action, resource, resourceID string, metadata map[string]any) {
	event := audit.NewServiceEvent(ctx, action, resource)
	event.ResourceID = resourceID
	event.Metadata = metadata
	if err := s.audit.Record(ctx, event); err != nil {
		slog.Warn("failed to record feature flag audit event",
			slog.String("action", action),
			slog.Any("error", err),
		)
	}
}

func (s *service) flagResponse(ctx context.Context, def features.Definition, row *Flag, subject features.Subject) Flag {
	resp := Flag{
		Key:         string(def.Key),
		Description: def.Description,
		Enabled:     def.Default,
	}
	if row != nil {
		if row.Description != "" {
			resp.Description = row.Description
		}
		resp.KillSwitch = row.KillSwitch
		// A row without a database default (created to anchor targets or a
		// kill switch) is not "configured": the compiled default is shown.
		if row.Configured {
			resp.Enabled = row.Enabled
			resp.Configured = true
		}
		resp.CreatedAt = row.CreatedAt
		resp.UpdatedAt = row.UpdatedAt
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
