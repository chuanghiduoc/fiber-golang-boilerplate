// Package service owns feature-flag administration rules. Evaluation
// precedence and caching live in internal/platform/features; this service
// validates compile-time keys, restricts process-wide flag writes to the
// platform role, scopes tenant targets to the request tenant, and invalidates
// the local snapshot after a successful write.
package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Audit actions for the domain events written after each mutation. Flag
// create/update are Tier 1: the action is recorded in the mutation
// transaction. Target writes and flag delete are Guarded at the HTTP layer;
// these post-commit events carry the flag-specific metadata and link to the
// intent row through ParentID.
const (
	actionFlagDeleted   = "admin.feature_flag.deleted"
	actionTargetUpdated = "admin.feature_flag_target.updated"
	actionTargetDeleted = "admin.feature_flag_target.deleted"
	resourceFlag        = "feature_flag"
	resourceTarget      = "feature_flag_target"
)

// Service is the feature-flag administration surface.
type Service interface {
	ListFlags(ctx context.Context, subject features.Subject) ([]Flag, error)
	ListEvaluated(ctx context.Context, subject features.Subject) ([]EvaluatedFlag, error)
	GetFlag(ctx context.Context, key string, subject features.Subject) (*Flag, error)
	CreateFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject) (*Flag, error)
	UpsertFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject) (*Flag, error)
	UpdateFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject, version ExpectedVersion) (*Flag, error)
	DeleteFlag(ctx context.Context, key string) error
	DeleteFlagVersion(ctx context.Context, key string, version ExpectedVersion) error
	ListTargets(ctx context.Context, key string) ([]Target, error)
	ListAllTargets(ctx context.Context, key string) ([]Target, error)
	PutTarget(ctx context.Context, key string, ref TargetRef, enabled bool) (*Target, error)
	DeleteTarget(ctx context.Context, key string, ref TargetRef) error
}

type service struct {
	repository Repository
	flags      features.Evaluator
	catalog    *features.Catalog
	authorizer authz.Authorizer
	audit      audit.Recorder
	tx         Transactor
}

// New returns a service. A nil evaluator falls back to compiled/environment
// defaults and a nil recorder discards domain events, so tests and minimal
// deployments stay nil-safe. tx is required for the Tier 1 flag mutations.
func New(repository Repository, flags features.Evaluator, recorder audit.Recorder, tx Transactor, catalog *features.Catalog, authorizer authz.Authorizer) Service {
	if flags == nil {
		flags = features.NewEvaluator(features.Options{Catalog: catalog})
	}
	if catalog == nil {
		catalog = features.EmptyCatalog()
	}
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &service{repository: repository, flags: flags, catalog: catalog, authorizer: authorizer, audit: recorder, tx: tx}
}

func (s *service) ListFlags(ctx context.Context, subject features.Subject) ([]Flag, error) {
	rows, err := s.repository.ListFlags(ctx)
	if err != nil {
		return nil, apperror.Internal(err, "failed to list feature flags")
	}
	byKey := make(map[features.Key]Flag, len(rows))
	for _, row := range rows {
		byKey[features.Key(row.Key)] = row
	}

	definitions := s.catalog.Definitions()
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

// ListEvaluated returns every catalog flag's effective value for the request
// tenant and caller.
func (s *service) ListEvaluated(ctx context.Context, subject features.Subject) ([]EvaluatedFlag, error) {
	rows, err := s.repository.ListFlags(ctx)
	if err != nil {
		return nil, apperror.Internal(err, "failed to list feature flags")
	}
	killSwitch := make(map[features.Key]bool, len(rows))
	for _, row := range rows {
		killSwitch[features.Key(row.Key)] = row.KillSwitch
	}

	definitions := s.catalog.Definitions()
	out := make([]EvaluatedFlag, 0, len(definitions))
	for _, def := range definitions {
		result := s.flags.Evaluate(ctx, def.Key, subject)
		out = append(out, EvaluatedFlag{
			Key:        string(def.Key),
			Enabled:    result.Enabled,
			KillSwitch: killSwitch[def.Key],
			Source:     string(result.Source),
		})
	}
	return out, nil
}

func (s *service) GetFlag(ctx context.Context, key string, subject features.Subject) (*Flag, error) {
	def, err := s.lookup(key)
	if err != nil {
		return nil, err
	}
	row, _, err := s.getRow(ctx, key)
	if err != nil {
		return nil, err
	}
	resp := s.flagResponse(ctx, def, row, subject)
	return &resp, nil
}

// CreateFlag creates the process-wide database row for a catalog key. It is a
// 409 when a row already exists: POST never silently replaces state.
func (s *service) CreateFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject) (*Flag, error) {
	def, err := s.lookup(key)
	if err != nil {
		return nil, err
	}
	if err := s.requirePlatformPermission(ctx, authz.PermissionPlatformFeaturesWrite); err != nil {
		return nil, err
	}
	if in.KillSwitch == nil {
		return nil, apperror.NewBadRequest("killSwitch is required")
	}

	description := def.Description
	if in.Description != nil && *in.Description != "" {
		description = *in.Description
	}
	row, err := s.writeFlag(ctx, key, func(ctx context.Context, tx TxRepos) (Flag, error) {
		return tx.Flags.CreateFlag(ctx, key, FlagWrite{
			Enabled:     in.Enabled,
			KillSwitch:  *in.KillSwitch,
			Description: description,
		})
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	resp := s.flagResponse(ctx, def, &row, subject)
	return &resp, nil
}

// UpsertFlag replaces a process-wide flag row without a precondition.
func (s *service) UpsertFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject) (*Flag, error) {
	return s.UpdateFlag(ctx, key, in, subject, UnconditionalVersion())
}

// UpdateFlag replaces the database default, kill switch, and description of a
// process-wide flag row. A nil Enabled stores NULL so environment and compiled
// defaults apply again. version is the caller's If-Match precondition and is
// enforced by the write statement itself.
func (s *service) UpdateFlag(ctx context.Context, key string, in UpsertFlagInput, subject features.Subject, version ExpectedVersion) (*Flag, error) {
	def, err := s.lookup(key)
	if err != nil {
		return nil, err
	}
	if err := s.requirePlatformPermission(ctx, authz.PermissionPlatformFeaturesWrite); err != nil {
		return nil, err
	}
	if in.KillSwitch == nil {
		return nil, apperror.NewBadRequest("killSwitch is required")
	}

	existing, configured, err := s.getRow(ctx, key)
	if err != nil {
		return nil, err
	}
	description := def.Description
	if configured && existing.Description != "" {
		description = existing.Description
	}
	// An explicit null (or empty) description means "use the catalog text", so
	// the stored row and the response always agree.
	if in.Description != nil && *in.Description != "" {
		description = *in.Description
	}

	write := FlagWrite{Enabled: in.Enabled, KillSwitch: *in.KillSwitch, Description: description}
	row, err := s.writeFlag(ctx, key, func(ctx context.Context, tx TxRepos) (Flag, error) {
		switch {
		case version.Unconditional:
			return tx.Flags.UpsertFlag(ctx, key, write)
		case version.Exists:
			return tx.Flags.UpdateFlagIfUnchanged(ctx, key, write, version.UpdatedAt)
		default:
			return tx.Flags.CreateFlagIfAbsent(ctx, key, write)
		}
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	resp := s.flagResponse(ctx, def, &row, subject)
	return &resp, nil
}

// writeFlag runs one Tier 1 flag write: the row and its audit event commit
// together, and an audit failure rolls the write back.
func (s *service) writeFlag(
	ctx context.Context,
	key string,
	persist func(ctx context.Context, tx TxRepos) (Flag, error),
) (Flag, error) {
	var row Flag
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		updated, err := persist(ctx, tx)
		if err != nil {
			return err
		}
		row = updated

		event := audit.NewServiceEvent(ctx, "admin.feature_flag.updated", resourceFlag)
		event.ResourceID = key
		event.Metadata = map[string]any{
			"feature":    key,
			"killSwitch": updated.KillSwitch,
			"configured": updated.Configured,
		}
		if updated.Configured {
			event.Metadata["enabled"] = updated.Enabled
		}
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	if err != nil {
		return Flag{}, flagWriteError(err)
	}
	return row, nil
}

func (s *service) DeleteFlag(ctx context.Context, key string) error {
	return s.DeleteFlagVersion(ctx, key, UnconditionalVersion())
}

// DeleteFlagVersion deletes a flag row, enforcing version when it is
// conditional. A caller that saw no row leaves a concurrently created row
// alone.
func (s *service) DeleteFlagVersion(ctx context.Context, key string, version ExpectedVersion) error {
	if _, err := s.lookup(key); err != nil {
		return err
	}
	if err := s.requirePlatformPermission(ctx, authz.PermissionPlatformFeaturesWrite); err != nil {
		return err
	}
	// Targets reference the flag with ON DELETE RESTRICT; the repository maps
	// that violation to 409 without naming the tenants that own them.
	var err error
	switch {
	case version.Unconditional:
		if delErr := s.repository.DeleteFlag(ctx, key); delErr != nil && !errors.Is(delErr, apperror.ErrNotFound) {
			err = delErr
		}
	case version.Exists:
		err = s.repository.DeleteFlagIfUnchanged(ctx, key, version.UpdatedAt)
	default:
		// The caller saw no row; a row that appeared concurrently must survive.
		return nil
	}
	if err != nil {
		if isAppError(err) {
			return err
		}
		return apperror.Internal(err, "failed to delete feature flag")
	}
	s.invalidate(ctx)
	s.recordEvent(ctx, actionFlagDeleted, resourceFlag, key, map[string]any{"feature": key})
	return nil
}

func (s *service) ListTargets(ctx context.Context, key string) ([]Target, error) {
	if _, err := s.lookup(key); err != nil {
		return nil, err
	}
	rows, err := s.repository.ListTargets(ctx, key)
	if err != nil {
		return nil, apperror.Internal(err, "failed to list feature flag targets")
	}
	return rows, nil
}

// ListAllTargets lists a flag's targets across every tenant. It is the
// platform view and needs the process-wide read permission.
func (s *service) ListAllTargets(ctx context.Context, key string) ([]Target, error) {
	if _, err := s.lookup(key); err != nil {
		return nil, err
	}
	if err := s.requirePlatformPermission(ctx, authz.PermissionPlatformFeaturesRead); err != nil {
		return nil, err
	}
	rows, err := s.repository.ListAllTargets(ctx, key)
	if err != nil {
		return nil, apperror.Internal(err, "failed to list feature flag targets")
	}
	return rows, nil
}

// PutTarget upserts the target at its item URI: a nil ref.TenantID targets the
// request tenant, and a non-nil one is the platform's cross-tenant form.
func (s *service) PutTarget(ctx context.Context, key string, ref TargetRef, enabled bool) (*Target, error) {
	if _, err := s.lookup(key); err != nil {
		return nil, err
	}
	tenantID, err := s.resolveTargetTenant(ctx, ref)
	if err != nil {
		return nil, err
	}
	if ref.UserID != nil && *ref.UserID == uuid.Nil {
		return nil, apperror.NewBadRequest("userId must be a valid UUID")
	}

	target, err := s.repository.UpsertTarget(ctx, key, tenantID, ref.UserID, enabled)
	if err != nil {
		return nil, targetWriteError(err)
	}
	s.invalidate(ctx)

	metadata := targetMetadata(key, tenantID, ref.UserID)
	metadata["enabled"] = target.Enabled
	s.recordEvent(ctx, actionTargetUpdated, resourceTarget, targetIDString(tenantID, ref.UserID), metadata)
	return &target, nil
}

func (s *service) DeleteTarget(ctx context.Context, key string, ref TargetRef) error {
	if _, err := s.lookup(key); err != nil {
		return err
	}
	tenantID, err := s.resolveTargetTenant(ctx, ref)
	if err != nil {
		return err
	}
	if err := s.repository.DeleteTarget(ctx, key, tenantID, ref.UserID); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("feature flag target not found")
		}
		return targetWriteError(err)
	}
	s.invalidate(ctx)
	s.recordEvent(ctx, actionTargetDeleted, resourceTarget, targetIDString(tenantID, ref.UserID),
		targetMetadata(key, tenantID, ref.UserID))
	return nil
}

// requirePlatformPermission restricts process-wide flag state and cross-tenant
// target writes to a platform role: they change behavior for every tenant.
func (s *service) requirePlatformPermission(ctx context.Context, permission authz.Permission) error {
	return s.authorizer.Authorize(ctx, permission, authz.Resource{}, authz.AllowPolicy{})
}

// resolveTargetTenant returns the tenant a target write applies to. Only the
// platform form carries an explicit tenant; it is authorized as a process-wide
// write before the repository bypasses row-level security for that statement.
func (s *service) resolveTargetTenant(ctx context.Context, ref TargetRef) (uuid.UUID, error) {
	if ref.TenantID != nil {
		if err := s.requirePlatformPermission(ctx, authz.PermissionPlatformFeaturesWrite); err != nil {
			return uuid.Nil, err
		}
		return *ref.TenantID, nil
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return uuid.Nil, apperror.Internal(err, "tenant context is required")
	}
	return tenantID, nil
}

// targetIDString is the canonical target id: "tenant:{tenantId}" for a
// tenant-wide target and "user:{userId}" inside one tenant.
func targetIDString(tenantID uuid.UUID, userID *uuid.UUID) string {
	if userID == nil {
		return "tenant:" + tenantID.String()
	}
	return "user:" + userID.String()
}

func targetMetadata(key string, tenantID uuid.UUID, userID *uuid.UUID) map[string]any {
	metadata := map[string]any{
		"feature":  key,
		"targetId": targetIDString(tenantID, userID),
		"tenantId": tenantID.String(),
	}
	if userID != nil {
		metadata["userId"] = userID.String()
	}
	return metadata
}

func (s *service) getRow(ctx context.Context, key string) (*Flag, bool, error) {
	row, err := s.repository.GetFlag(ctx, key)
	if err == nil {
		return &row, true, nil
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return nil, false, nil
	}
	return nil, false, apperror.Internal(err, "failed to read feature flag")
}

// invalidate is best-effort: the database write is already committed. A stale
// local snapshot converges at the next TTL expiry even if invalidation fails.
func (s *service) invalidate(ctx context.Context) {
	if err := s.flags.Invalidate(ctx); err != nil {
		slog.Warn("failed to invalidate feature flag cache", slog.Any("error", err))
	}
}

// recordEvent writes the post-commit domain event. It cannot roll the
// committed write back, so a failure is logged; the route's Guard intent row
// still records the mutation and its outcome.
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

func (s *service) lookup(key string) (features.Definition, error) {
	def, ok := s.catalog.Lookup(key)
	if !ok {
		return features.Definition{}, apperror.NewNotFound("feature flag not found")
	}
	return def, nil
}

func isAppError(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr)
}
