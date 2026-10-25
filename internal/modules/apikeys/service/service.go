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

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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
	Create(ctx context.Context, callerUserID int64, in CreateInput) (*CreatedKey, error)
	List(ctx context.Context, limit int, startingAfter string) ([]APIKey, bool, error)
	Get(ctx context.Context, id int64) (*APIKey, error)
	Update(ctx context.Context, id int64, in UpdateInput) (*APIKey, error)
	Revoke(ctx context.Context, id int64) error
	Rotate(ctx context.Context, id int64) (*RotatedKey, error)
}

type service struct {
	keys       APIKeyRepository
	principals identitycontracts.PrincipalReader
	tx         Transactor
	now        func() time.Time
}

// NewService builds the API-key service. Every lifecycle mutation is Tier 1
// audited: the audit row is written in the mutation's transaction, so a
// failed audit write rolls the change back (503 system.audit_unavailable).
func NewService(keys APIKeyRepository, principals identitycontracts.PrincipalReader, tx Transactor) Service {
	return &service{
		keys:       keys,
		principals: principals,
		tx:         tx,
		now:        time.Now,
	}
}

// mutate runs fn on a transaction and records the audit event it returns in
// the same transaction. A nil event means nothing changed.
func (s *service) mutate(ctx context.Context, fn func(ctx context.Context, tx TxRepos) (*audit.Event, error)) error {
	return s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		event, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		return s.recordAudit(ctx, tx, event)
	})
}

func (s *service) recordAudit(ctx context.Context, tx TxRepos, event *audit.Event) error {
	if event == nil {
		return nil
	}
	if err := tx.Audit.RecordTx(ctx, *event); err != nil {
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
func keyEvent(ctx context.Context, action string, key *APIKey, extra map[string]any) *audit.Event {
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

func (s *service) Create(ctx context.Context, callerUserID int64, in CreateInput) (*CreatedKey, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return nil, err
	}

	ownerID := in.UserID
	if ownerID == 0 {
		ownerID = callerUserID
	}
	if ownerID <= 0 {
		return nil, apperror.NewBadRequest("userId is required")
	}

	expiresAt, err := validateExpiry(in.ExpiresAt, s.now())
	if err != nil {
		return nil, err
	}
	credentials, err := apikey.Generate()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate API key")
	}

	var created *APIKey
	err = s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		// The owner stays locked until the key commits, so a concurrent ban
		// either finishes first (no row: 404) or waits and then revokes it.
		principal, ownerErr := s.lockActiveOwner(ctx, tx, ownerID)
		if ownerErr != nil {
			return nil, ownerErr
		}
		scopes, scopeErr := normalizeScopes(in.Scopes, principal.Role)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if grantErr := ensureCallerCanGrant(ctx, scopes); grantErr != nil {
			return nil, grantErr
		}
		key, createErr := tx.Keys.Create(ctx, NewKey{
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

	return &CreatedKey{APIKey: *created, Secret: credentials.Key}, nil
}

func (s *service) List(ctx context.Context, limit int, startingAfter string) ([]APIKey, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	keys, err := s.keys.ListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list API keys")
	}
	hasMore := len(keys) > pageSize
	if hasMore {
		keys = keys[:pageSize]
	}
	return keys, hasMore, nil
}

func (s *service) Get(ctx context.Context, id int64) (*APIKey, error) {
	return s.get(ctx, id)
}

func (s *service) Update(ctx context.Context, id int64, in UpdateInput) (*APIKey, error) {
	if in.Name == nil && len(in.Scopes) == 0 {
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
	if in.Name != nil {
		name, err = normalizeName(*in.Name)
		if err != nil {
			return nil, err
		}
	}
	scopes := existing.Scopes
	if len(in.Scopes) > 0 {
		principal, err := s.loadPrincipal(ctx, existing.UserID)
		if err != nil {
			return nil, err
		}
		scopes, err = normalizeScopes(in.Scopes, principal.Role)
		if err != nil {
			return nil, err
		}
		if err := ensureCallerCanGrant(ctx, scopes); err != nil {
			return nil, err
		}
	}

	var updated *APIKey
	err = s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		key, updateErr := tx.Keys.Update(ctx, id, name, scopes)
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
	return updated, nil
}

func (s *service) Revoke(ctx context.Context, id int64) error {
	return s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		revoked, err := tx.Keys.Revoke(ctx, id)
		if err == nil {
			return keyEvent(ctx, "api_key.revoked", revoked, nil), nil
		}
		if !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to revoke API key")
		}

		// Revocation is idempotent: revoking an already-revoked key succeeds
		// without a second audit row, while a missing or cross-tenant id is
		// concealed as not found.
		existing, getErr := tx.Keys.GetByID(ctx, id)
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

func (s *service) Rotate(ctx context.Context, id int64) (*RotatedKey, error) {
	graceSeconds := int32(RotationGrace / time.Second)

	var result *RotatedKey
	err := s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		rotated, rotateErr := s.rotate(ctx, tx, id, graceSeconds)
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

func (s *service) rotate(ctx context.Context, tx TxRepos, id int64, graceSeconds int32) (*RotatedKey, error) {
	existing, err := tx.Keys.GetByID(ctx, id)
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
	if err := s.ensureOwnerCanRotate(ctx, tx, existing); err != nil {
		return nil, err
	}
	// Rotation returns a fresh secret carrying the existing scopes, so the
	// caller must hold all of them, exactly as for create/update. Otherwise
	// an API key scoped only to rotation could mint a broader credential.
	if err := ensureCallerCanGrant(ctx, existing.Scopes); err != nil {
		return nil, err
	}

	rotated, err := tx.Keys.MarkRotated(ctx, id, graceSeconds)
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
	created, err := tx.Keys.Create(ctx, NewKey{
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
	return &RotatedKey{
		CreatedKey:            CreatedKey{APIKey: *created, Secret: credentials.Key},
		PreviousKeyID:         rotated.ID,
		PreviousKeyValidUntil: validUntil,
	}, nil
}

func (s *service) get(ctx context.Context, id int64) (*APIKey, error) {
	key, err := s.keys.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("API key not found")
		}
		return nil, apperror.NewInternal("failed to get API key")
	}
	return key, nil
}

// ensureOwnerCanRotate refuses to mint a successor for a key whose owner is
// no longer active, or whose credentials a ban revoked after the key was
// created: the verifier already rejects such a key, and rotating it would
// otherwise turn a dead credential into a live one (for example after an
// unban, which keeps credentials_revoked_at).
func (s *service) ensureOwnerCanRotate(ctx context.Context, tx TxRepos, key *APIKey) error {
	owner, err := s.lockActiveOwner(ctx, tx, key.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewConflict("API key owner is not active")
		}
		return err
	}
	if owner.CredentialsRevokedAt != nil && !key.CreatedAt.After(*owner.CredentialsRevokedAt) {
		return apperror.NewConflict("API key has been revoked")
	}
	return nil
}

// lockActiveOwner loads the key owner and holds the owner's membership row
// FOR SHARE until the key write commits. The API-key FK only takes KEY SHARE,
// which a ban's UPDATE does not wait for; without this lock a key inserted
// while its owner is being banned would survive the ban and come back to life
// on unban. A wired reader that cannot lock fails closed.
func (s *service) lockActiveOwner(ctx context.Context, tx TxRepos, userID int64) (identitycontracts.Principal, error) {
	if s.principals == nil {
		return s.loadPrincipal(ctx, userID)
	}
	if tx.Principals == nil {
		slog.Error("API key owner reader cannot lock the owner row; refusing the key write")
		return identitycontracts.Principal{}, apperror.NewInternal("failed to load API key owner")
	}
	return s.checkPrincipal(ctx, userID, func(tenantID int64) (identitycontracts.Principal, error) {
		return tx.Principals.LockPrincipal(ctx, tenantID, userID)
	})
}

func (s *service) loadPrincipal(ctx context.Context, userID int64) (identitycontracts.Principal, error) {
	if s.principals == nil {
		// Tests and minimal deployments may omit the identity adapter. Scope
		// validation is then limited to the catalog; production always wires it.
		return identitycontracts.Principal{ID: userID, TenantID: tenant.EffectiveID(ctx)}, nil
	}
	return s.checkPrincipal(ctx, userID, func(tenantID int64) (identitycontracts.Principal, error) {
		return s.principals.GetPrincipal(ctx, tenantID, userID)
	})
}

// checkPrincipal runs load for the request tenant and conceals a missing,
// inactive, or cross-tenant owner as 404 user not found.
func (s *service) checkPrincipal(
	ctx context.Context,
	userID int64,
	load func(tenantID int64) (identitycontracts.Principal, error),
) (identitycontracts.Principal, error) {
	tenantID := tenant.EffectiveID(ctx)
	principal, err := load(tenantID)
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
		// Fail closed: only a context marked by authz.SystemContext (a trusted
		// background caller) may mint scopes without a subject to bound them.
		if authz.IsSystemContext(ctx) {
			return nil
		}
		return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
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
