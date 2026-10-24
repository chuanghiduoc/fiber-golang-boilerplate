// Package service implements tenant-scoped API-key administration: creation,
// listing, metadata updates, revocation, and rotation with a grace window.
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts"
	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
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
	principals contracts.PrincipalReader
	txManager  *database.TxManager
	now        func() time.Time
}

func NewService(repo repository.APIKeyRepository, principals contracts.PrincipalReader, txManager *database.TxManager) Service {
	return &service{
		repo:       repo,
		principals: principals,
		txManager:  txManager,
		now:        time.Now,
	}
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

	created, err := s.repo.Create(ctx, repository.CreateParams{
		UserID:    ownerID,
		Name:      name,
		Prefix:    credentials.Prefix,
		Hash:      credentials.Hash,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to create API key")
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

	updated, err := s.repo.Update(ctx, id, name, scopes)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("API key not found")
		}
		return nil, apperror.NewInternal("failed to update API key")
	}
	resp := toResponse(updated)
	return &resp, nil
}

func (s *service) Revoke(ctx context.Context, id int64) error {
	_, err := s.repo.Revoke(ctx, id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, apperror.ErrNotFound) {
		return apperror.NewInternal("failed to revoke API key")
	}

	// Revocation is idempotent: revoking an already-revoked key succeeds, while
	// a missing or cross-tenant id is concealed as not found.
	existing, getErr := s.repo.GetByID(ctx, id)
	if getErr != nil {
		if errors.Is(getErr, apperror.ErrNotFound) {
			return apperror.NewNotFound("API key not found")
		}
		return apperror.NewInternal("failed to revoke API key")
	}
	if existing.RevokedAt != nil {
		return nil
	}
	return apperror.NewNotFound("API key not found")
}

func (s *service) Rotate(ctx context.Context, id int64) (*apikeydto.RotateResponse, error) {
	graceSeconds := int32(RotationGrace / time.Second)
	if s.txManager == nil {
		return s.rotate(ctx, s.repo, id, graceSeconds)
	}

	var result *apikeydto.RotateResponse
	err := s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		var rotateErr error
		result, rotateErr = s.rotate(ctx, repository.NewAPIKeyRepository(tx), id, graceSeconds)
		return rotateErr
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

func (s *service) loadPrincipal(ctx context.Context, userID int64) (contracts.Principal, error) {
	tenantID := tenant.EffectiveID(ctx)
	if s.principals == nil {
		// Tests and minimal deployments may omit the identity adapter. Scope
		// validation is then limited to the catalog; production always wires it.
		return contracts.Principal{ID: userID, TenantID: tenantID}, nil
	}
	principal, err := s.principals.GetPrincipal(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return contracts.Principal{}, apperror.NewNotFound("user not found")
		}
		return contracts.Principal{}, apperror.NewInternal("failed to load API key owner")
	}
	if principal.ID != userID || principal.TenantID != tenantID {
		return contracts.Principal{}, apperror.NewNotFound("user not found")
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
