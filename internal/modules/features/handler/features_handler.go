// Package handler owns HTTP transport for feature-flag administration.
package handler

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/uuid"

	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featuresservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const (
	platformFlagsPath       = "/api/v1/platform/feature-flags"
	platformFlagPath        = "/api/v1/platform/feature-flags/{key}"
	platformFlagTargetsPath = "/api/v1/platform/feature-flags/{key}/targets"
	platformFlagTargetPath  = "/api/v1/platform/feature-flags/{key}/targets/{targetId}"
	tenantFlagsPath         = "/api/v1/feature-flags"
	tenantFlagTargetsPath   = "/api/v1/feature-flags/{key}/targets"
	tenantFlagTargetPath    = "/api/v1/feature-flags/{key}/targets/{targetId}"

	platformTag = "Platform feature flags"
	tenantTag   = "Feature flags"

	// etagUnconfigured is the validator of a flag with no database row. It
	// changes as soon as a create or update writes one.
	etagUnconfigured = "unconfigured"
)

// Handler binds requests, validates input, and calls the service.
type Handler struct {
	service featuresservice.Service
}

// NewHandler returns a handler backed by service.
func NewHandler(service featuresservice.Service) *Handler {
	return &Handler{service: service}
}

// Register declares every feature-flag operation. Process-wide flag rows live
// under /platform; tenant admins manage their own targets under /feature-flags.
func (h *Handler) Register(api *httpapi.API) {

	requiredGuard := func(action, resource string) httpapi.AuditPolicy {
		return httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{Action: action, Resource: resource}}
	}

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: platformFlagsPath, OperationID: "listFeatureFlags",
		Tags: []string{platformTag}, Summary: "List feature flags",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.listPlatformFlags)

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: platformFlagPath, OperationID: "getFeatureFlag",
		Tags: []string{platformTag}, Summary: "Get a feature flag",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.getPlatformFlag)

	httpapi.Register(api, httpapi.Op{
		Method: "POST", Path: platformFlagsPath, OperationID: "createFeatureFlag",
		Tags: []string{platformTag}, Summary: "Create a feature flag",
		Description: "Create the process-wide database row for a compile-time flag key.",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: httpapi.AuditPolicy{Tier1Action: "admin.feature_flag.updated"},
	}, h.createPlatformFlag)

	httpapi.Register(api, httpapi.Op{
		Method: "PATCH", Path: platformFlagPath, OperationID: "updateFeatureFlag",
		Tags: []string{platformTag}, Summary: "Update a feature flag",
		Description: "RFC 7396 merge patch on enabled, killSwitch, and description.",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: httpapi.AuditPolicy{Tier1Action: "admin.feature_flag.updated"},
	}, h.updatePlatformFlag)

	httpapi.Register(api, httpapi.Op{
		Method: "DELETE", Path: platformFlagPath, OperationID: "deleteFeatureFlag",
		Tags: []string{platformTag}, Summary: "Delete a feature flag row",
		Description: "Delete the process-wide row, reverting the flag to environment and compiled defaults.",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: requiredGuard("admin.feature_flag.deleted", "feature_flag"),
	}, h.deletePlatformFlag)

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: platformFlagTargetsPath, OperationID: "listFeatureFlagTargets",
		Tags: []string{platformTag}, Summary: "List feature flag targets across tenants",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.listPlatformTargets)

	httpapi.Register(api, httpapi.Op{
		Method: "PUT", Path: platformFlagTargetPath, OperationID: "putFeatureFlagTarget",
		Tags: []string{platformTag}, Summary: "Put a feature flag target",
		Description: "Upsert at the item URI; targetId is tenant:{tenantId} or user:{tenantId}:{userId}.",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: requiredGuard("admin.feature_flag_target.updated", "feature_flag_target"),
	}, h.putPlatformTarget)

	httpapi.Register(api, httpapi.Op{
		Method: "DELETE", Path: platformFlagTargetPath, OperationID: "deleteFeatureFlagTarget",
		Tags: []string{platformTag}, Summary: "Delete a feature flag target",
	}, httpapi.Policy{
		Permission: authz.PermissionPlatformFeaturesWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: requiredGuard("admin.feature_flag_target.deleted", "feature_flag_target"),
	}, h.deletePlatformTarget)

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: tenantFlagsPath, OperationID: "listTenantFeatureFlags",
		Tags: []string{tenantTag}, Summary: "List the tenant's effective feature flags",
	}, httpapi.Policy{
		Permission: featurescatalog.PermissionAdminFeaturesRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.listTenantFlags)

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: tenantFlagTargetsPath, OperationID: "listTenantFeatureFlagTargets",
		Tags: []string{tenantTag}, Summary: "List the tenant's feature flag targets",
	}, httpapi.Policy{
		Permission: featurescatalog.PermissionAdminFeaturesRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.listTenantTargets)

	httpapi.Register(api, httpapi.Op{
		Method: "PUT", Path: tenantFlagTargetPath, OperationID: "putTenantFeatureFlagTarget",
		Tags: []string{tenantTag}, Summary: "Put a target for the tenant",
		Description: "Upsert at the item URI; targetId is tenant or user:{userId} inside the request tenant.",
	}, httpapi.Policy{
		Permission: featurescatalog.PermissionAdminFeaturesWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: requiredGuard("admin.feature_flag_target.updated", "feature_flag_target"),
	}, h.putTenantTarget)

	httpapi.Register(api, httpapi.Op{
		Method: "DELETE", Path: tenantFlagTargetPath, OperationID: "deleteTenantFeatureFlagTarget",
		Tags: []string{tenantTag}, Summary: "Delete a target for the tenant",
	}, httpapi.Policy{
		Permission: featurescatalog.PermissionAdminFeaturesWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: requiredGuard("admin.feature_flag_target.deleted", "feature_flag_target"),
	}, h.deleteTenantTarget)
}

// listFlagsInput has no parameters: the catalog is tiny and lists are complete.
type listFlagsInput struct{}

// flagInput addresses one flag. IfMatch is the config-resource validator.
type flagInput struct {
	Key     string `path:"key" maxLength:"128"`
	IfMatch string `header:"If-Match"`
}

// flagKeyInput addresses one flag without a conditional header.
type flagKeyInput struct {
	Key string `path:"key" maxLength:"128"`
}

// createFlagInput carries the process-wide row to create.
type createFlagInput struct {
	Body featuresdto.CreateFeatureFlagRequest
}

// updateFlagInput carries a merge patch.
type updateFlagInput struct {
	Key     string                               `path:"key" maxLength:"128"`
	IfMatch string                               `header:"If-Match"`
	Body    featuresdto.UpdateFeatureFlagRequest `contentType:"application/merge-patch+json"`
}

// platformTargetInput addresses a cross-tenant target by canonical id.
type platformTargetInput struct {
	Key      string `path:"key" maxLength:"128"`
	TargetID string `path:"targetId" maxLength:"120"`
	Body     featuresdto.PutTargetRequest
}

// platformTargetKeyInput addresses a target without a body (delete).
type platformTargetKeyInput struct {
	Key      string `path:"key" maxLength:"128"`
	TargetID string `path:"targetId" maxLength:"120"`
}

// tenantTargetInput addresses a target inside the request tenant.
type tenantTargetInput struct {
	Key      string `path:"key" maxLength:"128"`
	TargetID string `path:"targetId" maxLength:"120"`
	Body     featuresdto.PutTargetRequest
}

// tenantTargetKeyInput addresses a target inside the request tenant without a
// body (delete).
type tenantTargetKeyInput struct {
	Key      string `path:"key" maxLength:"128"`
	TargetID string `path:"targetId" maxLength:"120"`
}

func (h *Handler) listPlatformFlags(ctx context.Context, _ *listFlagsInput) (*httpapi.ListOutput[featuresdto.FeatureFlagResponse], error) {
	flags, err := h.service.ListFlags(ctx, requestSubject(ctx))
	if err != nil {
		return nil, err
	}
	items := toFlagResponses(flags)
	return &httpapi.ListOutput[featuresdto.FeatureFlagResponse]{
		Body: pagination.NewList(platformFlagsPath, items, false, ""),
	}, nil
}

func (h *Handler) getPlatformFlag(ctx context.Context, in *flagKeyInput) (*httpapi.ETagOutput[featuresdto.FeatureFlagResponse], error) {
	flag, err := h.service.GetFlag(ctx, in.Key, requestSubject(ctx))
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[featuresdto.FeatureFlagResponse]{
		ETag: flagETag(flag),
		Body: toFlagResponse(*flag),
	}, nil
}

func (h *Handler) createPlatformFlag(ctx context.Context, in *createFlagInput) (*httpapi.Created[featuresdto.FeatureFlagResponse], error) {
	flag, err := h.service.CreateFlag(ctx, in.Body.Key, toUpsertFlagInput(in.Body.Enabled, in.Body.KillSwitch, in.Body.Description), requestSubject(ctx))
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated(platformFlagsPath+"/"+in.Body.Key, toFlagResponse(*flag)), nil
}

func (h *Handler) updatePlatformFlag(ctx context.Context, in *updateFlagInput) (*httpapi.ETagOutput[featuresdto.FeatureFlagResponse], error) {
	current, err := h.service.GetFlag(ctx, in.Key, requestSubject(ctx))
	if err != nil {
		return nil, err
	}
	if err := httpapi.CheckIfMatchValue(in.IfMatch, flagETag(current)); err != nil {
		return nil, err
	}
	if err := requireExistingForIfMatch(in.IfMatch, current); err != nil {
		return nil, err
	}
	// An empty merge patch changes nothing: return the current representation
	// without touching updated_at, the ETag, or the audit trail.
	if !in.Body.Enabled.Present && !in.Body.KillSwitch.Present && !in.Body.Description.Present {
		return &httpapi.ETagOutput[featuresdto.FeatureFlagResponse]{
			ETag: flagETag(current),
			Body: toFlagResponse(*current),
		}, nil
	}
	flag, err := h.service.UpdateFlag(ctx, in.Key, resolveFlagUpdate(current, in.Body), requestSubject(ctx), expectedVersion(in.IfMatch, current))
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[featuresdto.FeatureFlagResponse]{
		ETag: flagETag(flag),
		Body: toFlagResponse(*flag),
	}, nil
}

func (h *Handler) deletePlatformFlag(ctx context.Context, in *flagInput) (*httpapi.Empty, error) {
	current, err := h.service.GetFlag(ctx, in.Key, requestSubject(ctx))
	if err != nil {
		return nil, err
	}
	if err := httpapi.CheckIfMatchValue(in.IfMatch, flagETag(current)); err != nil {
		return nil, err
	}
	if err := requireExistingForIfMatch(in.IfMatch, current); err != nil {
		return nil, err
	}
	if err := h.service.DeleteFlagVersion(ctx, in.Key, expectedVersion(in.IfMatch, current)); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

func (h *Handler) listPlatformTargets(ctx context.Context, in *flagKeyInput) (*httpapi.ListOutput[featuresdto.FeatureFlagTargetResponse], error) {
	targets, err := h.service.ListAllTargets(ctx, in.Key)
	if err != nil {
		return nil, err
	}
	items := toPlatformTargetResponses(targets)
	return &httpapi.ListOutput[featuresdto.FeatureFlagTargetResponse]{
		Body: pagination.NewList(platformFlagsPath+"/"+in.Key+"/targets", items, false, ""),
	}, nil
}

func (h *Handler) putPlatformTarget(ctx context.Context, in *platformTargetInput) (*httpapi.Output[featuresdto.FeatureFlagTargetResponse], error) {
	ref, err := parsePlatformTargetID(in.TargetID)
	if err != nil {
		return nil, err
	}
	if in.Body.Enabled == nil {
		return nil, apperror.NewBadRequest("enabled is required")
	}
	target, err := h.service.PutTarget(ctx, in.Key, ref, *in.Body.Enabled)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[featuresdto.FeatureFlagTargetResponse]{
		Body: toPlatformTargetResponse(*target),
	}, nil
}

func (h *Handler) deletePlatformTarget(ctx context.Context, in *platformTargetKeyInput) (*httpapi.Empty, error) {
	ref, err := parsePlatformTargetID(in.TargetID)
	if err != nil {
		return nil, err
	}
	if err := h.service.DeleteTarget(ctx, in.Key, ref); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

func (h *Handler) listTenantFlags(ctx context.Context, _ *listFlagsInput) (*httpapi.ListOutput[featuresdto.EvaluatedFlagResponse], error) {
	flags, err := h.service.ListEvaluated(ctx, requestSubject(ctx))
	if err != nil {
		return nil, err
	}
	items := toEvaluatedFlagResponses(flags)
	return &httpapi.ListOutput[featuresdto.EvaluatedFlagResponse]{
		Body: pagination.NewList(tenantFlagsPath, items, false, ""),
	}, nil
}

func (h *Handler) listTenantTargets(ctx context.Context, in *flagKeyInput) (*httpapi.ListOutput[featuresdto.FeatureFlagTargetResponse], error) {
	targets, err := h.service.ListTargets(ctx, in.Key)
	if err != nil {
		return nil, err
	}
	items := toTenantTargetResponses(targets)
	return &httpapi.ListOutput[featuresdto.FeatureFlagTargetResponse]{
		Body: pagination.NewList(tenantFlagsPath+"/"+in.Key+"/targets", items, false, ""),
	}, nil
}

func (h *Handler) putTenantTarget(ctx context.Context, in *tenantTargetInput) (*httpapi.Output[featuresdto.FeatureFlagTargetResponse], error) {
	ref, err := parseTenantTargetID(in.TargetID)
	if err != nil {
		return nil, err
	}
	if in.Body.Enabled == nil {
		return nil, apperror.NewBadRequest("enabled is required")
	}
	target, err := h.service.PutTarget(ctx, in.Key, ref, *in.Body.Enabled)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[featuresdto.FeatureFlagTargetResponse]{
		Body: toTenantTargetResponse(*target),
	}, nil
}

func (h *Handler) deleteTenantTarget(ctx context.Context, in *tenantTargetKeyInput) (*httpapi.Empty, error) {
	ref, err := parseTenantTargetID(in.TargetID)
	if err != nil {
		return nil, err
	}
	if err := h.service.DeleteTarget(ctx, in.Key, ref); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// requestSubject builds the evaluator subject from the authenticated caller.
func requestSubject(ctx context.Context) features.Subject {
	subject, _ := authz.SubjectFromContext(ctx)
	return features.SubjectFromContext(ctx, subject.UserID)
}

// flagETag is the config-resource validator: the row's update time, or a
// stable marker while no row exists. The value is a strong entity-tag; the
// huma output writes header values verbatim.
func flagETag(flag *featuresservice.Flag) string {
	if flag == nil || flag.UpdatedAt == nil {
		return `"` + etagUnconfigured + `"`
	}
	return `"` + strconv.FormatInt(flag.UpdatedAt.UTC().UnixNano(), 10) + `"`
}

// requireExistingForIfMatch applies the RFC 9110 rule that If-Match * requires
// a current representation; CheckIfMatchValue only compares entity-tags.
func requireExistingForIfMatch(ifMatch string, current *featuresservice.Flag) error {
	if strings.TrimSpace(ifMatch) == "*" && (current == nil || current.UpdatedAt == nil) {
		return apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
	}
	return nil
}

// expectedVersion turns the request's If-Match precondition into the version
// the repository enforces in the write statement. An absent header is
// unconditional.
func expectedVersion(ifMatch string, current *featuresservice.Flag) featuresservice.ExpectedVersion {
	if strings.TrimSpace(ifMatch) == "" {
		return featuresservice.UnconditionalVersion()
	}
	if current == nil || current.UpdatedAt == nil {
		return featuresservice.ExpectedVersion{Exists: false}
	}
	return featuresservice.ExpectedVersion{Exists: true, UpdatedAt: current.UpdatedAt.UTC()}
}

// resolveFlagUpdate applies an RFC 7396 merge patch to the current row state.
func resolveFlagUpdate(current *featuresservice.Flag, patch featuresdto.UpdateFeatureFlagRequest) featuresservice.UpsertFlagInput {
	var enabled *bool
	if current.Configured {
		value := current.Enabled
		enabled = &value
	}
	if patch.Enabled.Present {
		if patch.Enabled.Null {
			enabled = nil
		} else {
			value := patch.Enabled.Value
			enabled = &value
		}
	}

	killSwitch := current.KillSwitch
	if patch.KillSwitch.Present {
		killSwitch = !patch.KillSwitch.Null && patch.KillSwitch.Value
	}

	description := current.Description
	if patch.Description.Present {
		if patch.Description.Null {
			description = ""
		} else {
			description = patch.Description.Value
		}
	}

	return featuresservice.UpsertFlagInput{
		Enabled:     enabled,
		KillSwitch:  &killSwitch,
		Description: &description,
	}
}

// toUpsertFlagInput resolves a create body. A nil killSwitch is rejected by
// the service.
func toUpsertFlagInput(enabled, killSwitch *bool, description *string) featuresservice.UpsertFlagInput {
	return featuresservice.UpsertFlagInput{
		Enabled:     enabled,
		KillSwitch:  killSwitch,
		Description: description,
	}
}

// parsePlatformTargetID decodes tenant:{uuid} and user:{tenantUuid}:{userUuid}.
func parsePlatformTargetID(raw string) (featuresservice.TargetRef, error) {
	parts := strings.Split(raw, ":")
	switch {
	case len(parts) == 2 && parts[0] == "tenant":
		tenantID, err := parseTargetUUID(parts[1])
		if err != nil {
			return featuresservice.TargetRef{}, err
		}
		return featuresservice.TargetRef{TenantID: &tenantID}, nil
	case len(parts) == 3 && parts[0] == "user":
		tenantID, err := parseTargetUUID(parts[1])
		if err != nil {
			return featuresservice.TargetRef{}, err
		}
		userID, err := parseTargetUUID(parts[2])
		if err != nil {
			return featuresservice.TargetRef{}, err
		}
		return featuresservice.TargetRef{TenantID: &tenantID, UserID: &userID}, nil
	default:
		return featuresservice.TargetRef{}, invalidTargetID()
	}
}

// parseTenantTargetID decodes tenant and user:{userUuid}.
func parseTenantTargetID(raw string) (featuresservice.TargetRef, error) {
	parts := strings.Split(raw, ":")
	switch {
	case len(parts) == 1 && parts[0] == "tenant":
		return featuresservice.TargetRef{}, nil
	case len(parts) == 2 && parts[0] == "user":
		userID, err := parseTargetUUID(parts[1])
		if err != nil {
			return featuresservice.TargetRef{}, err
		}
		return featuresservice.TargetRef{UserID: &userID}, nil
	default:
		return featuresservice.TargetRef{}, invalidTargetID()
	}
}

func parseTargetUUID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, invalidTargetID()
	}
	return id, nil
}

func invalidTargetID() error {
	return apperror.NewBadRequest("targetId has an unsupported or invalid format")
}
