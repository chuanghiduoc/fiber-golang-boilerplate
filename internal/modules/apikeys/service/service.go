// Package service implements tenant-scoped API-key administration: creation,
// listing, metadata updates, revocation, and rotation with a grace window.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const (
	// RotationGrace is how long a rotated key remains valid after its successor
	// is issued. Revoking the old key explicitly always wins.
	RotationGrace = 24 * time.Hour
	maxNameRunes  = 255
	maxScopes     = 100
)

// Service is the API-key administration surface consumed by the HTTP handler.
type Service interface {
	Create(ctx context.Context, callerUserID int64, req apikeydto.CreateRequest) (*apikeydto.KeyWithSecretResponse, error)
	List(ctx context.Context, limit int, startingAfter string) ([]apikeydto.APIKeyResponse, bool, error)
	Get(ctx context.Context, id int64) (*apikeydto.APIKeyResponse, error)
	Update(ctx context.Context, id int64, req apikeydto.UpdateRequest) (*apikeydto.APIKeyResponse, error)
	Revoke(ctx context.Context, id int64) error
	Rotate(ctx context.Context, id int64) (*apikeydto.RotateResponse, error)
}

type service struct {
	repo       repository.APIKeyRepository
	principals identitycontracts.PrincipalReader
	txManager  *database.TxManager
	audit      audit.TxRecorder
	now        func() time.Time
}

// NewService builds the API-key service. Every lifecycle mutation is Tier 1
// audited: the audit row is written in the mutation's transaction, so a
// failed audit write rolls the change back (503 system.audit_unavailable). A
// nil auditRecorder disables auditing for tests and minimal deployments.
func NewService(
	repo repository.APIKeyRepository,
	principals identitycontracts.PrincipalReader,
	txManager *database.TxManager,
	auditRecorder audit.TxRecorder,
) Service {
	return &service{
		repo:       repo,
		principals: principals,
		txManager:  txManager,
		audit:      auditRecorder,
		now:        time.Now,
	}
}

// mutate runs fn on a transaction-backed repository (or the pool repository
// when no transaction manager is wired) and records the audit event fn
// returns in the same transaction. A nil event means nothing changed.
func (s *service) mutate(ctx context.Context, fn func(repo repository.APIKeyRepository) (*audit.Event, error)) error {
	if s.txManager == nil {
		event, err := fn(s.repo)
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, nil, event)
	}
	return s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		event, err := fn(repository.NewAPIKeyRepository(tx))
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, tx, event)
	})
}

func (s *service) recordAudit(ctx context.Context, tx pgx.Tx, event *audit.Event) error {
	if event == nil {
		return nil
	}
	if err := audit.RecordInTx(ctx, s.audit, tx, *event); err != nil {
		slog.Error("failed to write transactional audit event",
			slog.String("action", event.Action),
			slog.Any("error", err),
		)
		return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
	}
	return nil
}

// keyEvent builds a service audit event for key. Metadata never contains the
// secret or its hash.
func keyEvent(ctx context.Context, action string, key *repository.APIKey, extra map[string]any) *audit.Event {
	event := audit.NewServiceEvent(ctx, action, "api_key")
	event.ResourceID = strconv.FormatInt(key.ID, 10)
	scopes := make([]string, len(key.Scopes))
	for i, scope := range key.Scopes {
		scopes[i] = string(scope)
	}
	// Key names must avoid the sanitizer's sensitive fragments (for example
	// "api_key"/"apikey"), otherwise the value is stored as "[redacted]".
	metadata := map[string]any{
		"name":        key.Name,
		"ownerUserId": key.UserID,
		"scopes":      scopes,
	}
	for k, v := range extra {
		metadata[k] = v
	}
	event.Metadata = metadata
	return &event
}

func (s *service) Create(ctx context.Context, callerUserID int64, req apikeydto.CreateRequest) (*apikeydto.KeyWithSecretResponse, error) {
	name, err := normalizeName(req.Name)
	if err != nil {
		return nil, err
	}

	ownerID := req.UserID
	if ownerID == 0 {
		ownerID = callerUserID
	}
	if ownerID <= 0 {
		return nil, apperror.NewBadRequest("userId is required")
	}

	principal, err := s.loadPrincipal(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	scopes, err := normalizeScopes(req.Scopes, principal.Role)
	if err != nil {
		return nil, err
	}
	if err := ensureCallerCanGrant(ctx, scopes); err != nil {
		return nil, err
	}
	expiresAt, err := validateExpiry(req.ExpiresAt, s.now())
	if err != nil {
		return nil, err
	}

	credentials, err := apikey.Generate()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate API key")
	}

	var created *repository.APIKey
	err = s.mutate(ctx, func(repo repository.APIKeyRepository) (*audit.Event, error) {
		key, createErr := repo.Create(ctx, repository.CreateParams{
			UserID:    ownerID,
			Name:      name,
			Prefix:    credentials.Prefix,
			Hash:      credentials.Hash,
			Scopes:    scopes,
			ExpiresAt: expiresAt,
		})
		if createErr != nil {
			if errors.Is(createErr, apperror.ErrNotFound) {
				return nil, apperror.NewNotFound("user not found")
			}
			return nil, apperror.NewInternal("failed to create API key")
		}
		created = key
		return keyEvent(ctx, "api_key.created", key, nil), nil
	})
	if err != nil {
		return nil, err
	}

	return &apikeydto.KeyWithSecretResponse{
		APIKeyResponse: toResponse(created),
		Key:            credentials.Key,
	}, nil
}

func (s *service) List(ctx context.Context, limit int, startingAfter string) ([]apikeydto.APIKeyResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	keys, err := s.repo.ListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list API keys")
	}
	hasMore := len(keys) > pageSize
	if hasMore {
		keys = keys[:pageSize]
	}
	responses := make([]apikeydto.APIKeyResponse, len(keys))
	for i := range keys {
		responses[i] = toResponse(&keys[i])
	}
	return responses, hasMore, nil
}

func (s *service) Get(ctx context.Context, id int64) (*apikeydto.APIKeyResponse, error) {
	key, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	resp := toResponse(key)
	return &resp, nil
}

func (s *service) Update(ctx context.Context, id int64, req apikeydto.UpdateRequest) (*apikeydto.APIKeyResponse, error) {
	if req.Name == nil && len(req.Scopes) == 0 {
		return nil, apperror.NewBadRequest("at least one of name or scopes is required")
	}
	existing, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.RevokedAt != nil {
		return nil, apperror.NewConflict("API key has been revoked")
	}

	name := existing.Name
	if req.Name != nil {
		name, err = normalizeName(*req.Name)
		if err != nil {
			return nil, err
		}
	}
	scopes := existing.Scopes
	if len(req.Scopes) > 0 {
		principal, err := s.loadPrincipal(ctx, existing.UserID)
		if err != nil {
			return nil, err
		}
		scopes, err = normalizeScopes(req.Scopes, principal.Role)
		if err != nil {
			return nil, err
		}
		if err := ensureCallerCanGrant(ctx, scopes); err != nil {
			return nil, err
		}
	}

	var updated *repository.APIKey
	err = s.mutate(ctx, func(repo repository.APIKeyRepository) (*audit.Event, error) {
		key, updateErr := repo.Update(ctx, id, name, scopes)
		if updateErr != nil {
			if errors.Is(updateErr, apperror.ErrNotFound) {
				return nil, apperror.NewNotFound("API key not found")
			}
			return nil, apperror.NewInternal("failed to update API key")
		}
		updated = key
		return keyEvent(ctx, "api_key.updated", key, nil), nil
	})
	if err != nil {
		return nil, err
	}
	resp := toResponse(updated)
	return &resp, nil
}

func (s *service) Revoke(ctx context.Context, id int64) error {
	return s.mutate(ctx, func(repo repository.APIKeyRepository) (*audit.Event, error) {
		revoked, err := repo.Revoke(ctx, id)
		if err == nil {
			return keyEvent(ctx, "api_key.revoked", revoked, nil), nil
		}
		if !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to revoke API key")
		}

		// Revocation is idempotent: revoking an already-revoked key succeeds
		// without a second audit row, while a missing or cross-tenant id is
		// concealed as not found.
		existing, getErr := repo.GetByID(ctx, id)
		if getErr != nil {
			if errors.Is(getErr, apperror.ErrNotFound) {
				return nil, apperror.NewNotFound("API key not found")
			}
			return nil, apperror.NewInternal("failed to revoke API key")
		}
		if existing.RevokedAt != nil {
			return nil, nil
		}
		return nil, apperror.NewNotFound("API key not found")
	})
}

func (s *service) Rotate(ctx context.Context, id int64) (*apikeydto.RotateResponse, error) {
	graceSeconds := int32(RotationGrace / time.Second)

	var result *apikeydto.RotateResponse
	err := s.mutate(ctx, func(repo repository.APIKeyRepository) (*audit.Event, error) {
		rotated, rotateErr := s.rotate(ctx, repo, id, graceSeconds)
		if rotateErr != nil {
			return nil, rotateErr
		}
		result = rotated
		event := audit.NewServiceEvent(ctx, "api_key.rotated", "api_key")
		event.ResourceID = strconv.FormatInt(rotated.PreviousKeyID, 10)
		event.Metadata = map[string]any{
			"successorId":           rotated.ID,
			"previousKeyValidUntil": rotated.PreviousKeyValidUntil,
			"ownerUserId":           rotated.UserID,
		}
		return &event, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *service) rotate(
	ctx context.Context,
	repo repository.APIKeyRepository,
	id int64,
	graceSeconds int32,
) (*apikeydto.RotateResponse, error) {
	existing, err := repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("API key not found")
		}
		return nil, apperror.NewInternal("failed to rotate API key")
	}
	if existing.RevokedAt != nil {
		return nil, apperror.NewConflict("API key has been revoked")
	}
	if existing.RotatedAt != nil {
		return nil, apperror.NewConflict("API key has already been rotated")
	}
	// Rotation returns a fresh secret carrying the existing scopes, so the
	// caller must hold all of them, exactly as for create/update. Otherwise
	// an API key scoped only to rotation could mint a broader credential.
	if err := ensureCallerCanGrant(ctx, existing.Scopes); err != nil {
		return nil, err
	}

	rotated, err := repo.MarkRotated(ctx, id, graceSeconds)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			// Another request rotated or revoked the key between the read and
			// the conditional update.
			return nil, apperror.NewConflict("API key was rotated or revoked concurrently")
		}
		return nil, apperror.NewInternal("failed to rotate API key")
	}

	credentials, err := apikey.Generate()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate API key")
	}
	created, err := repo.Create(ctx, repository.CreateParams{
		UserID:    rotated.UserID,
		Name:      rotated.Name,
		Prefix:    credentials.Prefix,
		Hash:      credentials.Hash,
		Scopes:    rotated.Scopes,
		ExpiresAt: rotated.ExpiresAt,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to create rotated API key")
	}

	validUntil := s.now().UTC().Add(RotationGrace)
	if rotated.RotationGraceUntil != nil {
		validUntil = *rotated.RotationGraceUntil
	}
	return &apikeydto.RotateResponse{
		KeyWithSecretResponse: apikeydto.KeyWithSecretResponse{
			APIKeyResponse: toResponse(created),
			Key:            credentials.Key,
		},
		PreviousKeyID:         rotated.ID,
		PreviousKeyValidUntil: validUntil,
	}, nil
}

func (s *service) get(ctx context.Context, id int64) (*repository.APIKey, error) {
	key, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("API key not found")
		}
		return nil, apperror.NewInternal("failed to get API key")
	}
	return key, nil
}

func (s *service) loadPrincipal(ctx context.Context, userID int64) (identitycontracts.Principal, error) {
	tenantID := tenant.EffectiveID(ctx)
	if s.principals == nil {
		// Tests and minimal deployments may omit the identity adapter. Scope
		// validation is then limited to the catalog; production always wires it.
		return identitycontracts.Principal{ID: userID, TenantID: tenantID}, nil
	}
	principal, err := s.principals.GetPrincipal(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return identitycontracts.Principal{}, apperror.NewNotFound("user not found")
		}
		return identitycontracts.Principal{}, apperror.NewInternal("failed to load API key owner")
	}
	if principal.ID != userID || principal.TenantID != tenantID {
		return identitycontracts.Principal{}, apperror.NewNotFound("user not found")
	}
	return principal, nil
}

func normalizeScopes(raw []string, role string) ([]authz.Permission, error) {
	if len(raw) == 0 {
		return nil, apperror.NewBadRequest("at least one scope is required")
	}
	if len(raw) > maxScopes {
		return nil, apperror.NewBadRequest("too many scopes")
	}
	seen := make(map[authz.Permission]struct{}, len(raw))
	scopes := make([]authz.Permission, 0, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, apperror.NewBadRequest("scopes must not contain empty values")
		}
		permission := authz.Permission(value)
		if !authz.IsKnownPermission(permission) {
			return nil, apperror.NewBadRequest(fmt.Sprintf("unknown scope %q", value))
		}
		if role != "" && !authz.Has(role, permission) {
			return nil, apperror.NewBadRequest(fmt.Sprintf("scope %q is not granted to the key owner", value))
		}
		if _, ok := seen[permission]; ok {
			continue
		}
		seen[permission] = struct{}{}
		scopes = append(scopes, permission)
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i] < scopes[j] })
	return scopes, nil
}

func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", apperror.NewBadRequest("name is required")
	}
	if len([]rune(name)) > maxNameRunes {
		return "", apperror.NewBadRequest("name is too long")
	}
	return name, nil
}

func ensureCallerCanGrant(ctx context.Context, scopes []authz.Permission) error {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok {
		// Trusted direct callers (background jobs, tests) have no HTTP subject;
		// the route layer has already performed the permission check.
		return nil
	}
	for _, scope := range scopes {
		if !subject.Has(scope) {
			return apperror.NewBadRequest(fmt.Sprintf("scope %q is not granted to the caller", scope))
		}
	}
	return nil
}

func validateExpiry(expiresAt *time.Time, now time.Time) (*time.Time, error) {
	if expiresAt == nil {
		return nil, nil
	}
	value := expiresAt.UTC()
	if !value.After(now.UTC()) {
		return nil, apperror.NewBadRequest("expiresAt must be in the future")
	}
	return &value, nil
}

func toResponse(key *repository.APIKey) apikeydto.APIKeyResponse {
	scopes := make([]string, len(key.Scopes))
	for i, scope := range key.Scopes {
		scopes[i] = string(scope)
	}
	return apikeydto.APIKeyResponse{
		ID:                 key.ID,
		UserID:             key.UserID,
		Name:               key.Name,
		Prefix:             key.Prefix,
		Scopes:             scopes,
		ExpiresAt:          key.ExpiresAt,
		RevokedAt:          key.RevokedAt,
		RotatedAt:          key.RotatedAt,
		RotationGraceUntil: key.RotationGraceUntil,
		LastUsedAt:         key.LastUsedAt,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
	}
}

func buildCursor(limit int, startingAfter string) (cur pagination.DBCursor, pageSize int, err error) {
	pageSize = pagination.NormalizeLimit(limit)
	cur = pagination.DBCursor{Limit: pagination.RowLimit(pageSize + 1)}
	if startingAfter != "" {
		ts, id, decErr := pagination.DecodeCursor(startingAfter)
		if decErr != nil {
			return pagination.DBCursor{}, 0, apperror.NewBadRequest("invalid cursor")
		}
		cur.HasCursor = true
		cur.CreatedAt = ts
		cur.ID = id
	}
	return cur, pageSize, nil
}
