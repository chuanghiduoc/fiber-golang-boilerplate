// Package service implements tenant-scoped API-key administration: creation,
// listing, metadata updates, revocation, and rotation with a grace window.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
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
	// defaultMaxLifetime matches APIKEYS_MAX_LIFETIME's default when the
	// service is constructed without configuration (tests).
	defaultMaxLifetime = 8760 * time.Hour
	maxNameRunes       = 255
	maxScopes          = 100
)

// Service is the API-key administration surface consumed by the HTTP handler.
type Service interface {
	Create(ctx context.Context, callerUserID uuid.UUID, in CreateInput) (*CreatedKey, error)
	List(ctx context.Context, limit int, startingAfter string) ([]APIKey, bool, error)
	ListOwnedBy(ctx context.Context, ownerID uuid.UUID, limit int, startingAfter string) ([]APIKey, bool, error)
	ListForMember(ctx context.Context, memberID uuid.UUID, limit int, startingAfter string) ([]APIKey, bool, error)
	Get(ctx context.Context, id uuid.UUID) (*APIKey, error)
	GetOwnedBy(ctx context.Context, ownerID, id uuid.UUID) (*APIKey, error)
	Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*APIKey, error)
	UpdateOwnedBy(ctx context.Context, ownerID, id uuid.UUID, in UpdateInput) (*APIKey, error)
	UpdateForMember(ctx context.Context, memberID, id uuid.UUID, in UpdateInput) (*APIKey, error)
	Revoke(ctx context.Context, id uuid.UUID) error
	RevokeOwnedBy(ctx context.Context, ownerID, id uuid.UUID) error
	RevokeForMember(ctx context.Context, memberID, id uuid.UUID) error
	Rotate(ctx context.Context, callerUserID, id uuid.UUID) (*RotatedKey, error)
}

type service struct {
	keys        APIKeyRepository
	principals  identitycontracts.PrincipalReader
	ranks       identitycontracts.MemberRankChecker
	tx          Transactor
	authorizer  authz.Authorizer
	now         func() time.Time
	maxLifetime time.Duration
}

// NewService builds the API-key service. Every lifecycle mutation is Tier 1
// audited: the audit row is written in the mutation's transaction, so a
// failed audit write rolls the change back (503 system.audit_unavailable).
// maxLifetime caps every newly issued key; a non-positive value falls back to
// the documented default for tests and minimal deployments. ranks is identity's
// member hierarchy; nil fails closed when a caller manages another member's key.
func NewService(keys APIKeyRepository, principals identitycontracts.PrincipalReader, ranks identitycontracts.MemberRankChecker, tx Transactor, authorizer authz.Authorizer, maxLifetime time.Duration) Service {
	if maxLifetime <= 0 {
		maxLifetime = defaultMaxLifetime
	}
	return &service{
		keys:        keys,
		principals:  principals,
		ranks:       ranks,
		tx:          tx,
		authorizer:  authorizer,
		now:         time.Now,
		maxLifetime: maxLifetime,
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
	event.ResourceID = key.ID.String()
	scopes := make([]string, len(key.Scopes))
	for i, scope := range key.Scopes {
		scopes[i] = string(scope)
	}
	// Key names must avoid the sanitizer's sensitive fragments (for example
	// "api_key"/"apikey"), otherwise the value is stored as "[redacted]".
	metadata := map[string]any{
		"name":        key.Name,
		"ownerUserId": key.UserID.String(),
		"scopes":      scopes,
	}
	for k, v := range extra {
		metadata[k] = v
	}
	event.Metadata = metadata
	return &event
}

func (s *service) Create(ctx context.Context, callerUserID uuid.UUID, in CreateInput) (*CreatedKey, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return nil, err
	}

	// The owner is always the authenticated caller. An administrator may list
	// and revoke another member's keys, but a secret is only ever issued to the
	// user who will act with it, so audience routes (/me/*) and the audit actor
	// cannot be borrowed by creating a key for someone else.
	ownerID := callerUserID
	if ownerID == uuid.Nil {
		return nil, apperror.NewBadRequest("authenticated user is required")
	}

	expiresAt, err := s.expiry(in.ExpiresAt, s.now())
	if err != nil {
		return nil, err
	}
	credentials, err := apikey.Generate()
	if err != nil {
		return nil, apperror.Internal(err, "failed to generate API key")
	}

	var created *APIKey
	err = s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		// The owner stays locked until the key commits, so a concurrent ban
		// either finishes first (no row: 404) or waits and then revokes it.
		principal, ownerErr := s.lockActiveOwner(ctx, tx, ownerID)
		if ownerErr != nil {
			return nil, ownerErr
		}
		scopes, scopeErr := s.normalizeScopes(in.Scopes, principal)
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
			return nil, apperror.Internal(createErr, "failed to create API key")
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
		return nil, false, apperror.Internal(err, "failed to list API keys")
	}
	more := hasMore(keys, pageSize)
	return page(keys, pageSize), more, nil
}

// ListOwnedBy lists one member's keys. The route layer scopes the owner to the
// authenticated caller for /me; the user id is part of the query predicate so a
// caller can never page through another member's keys.
func (s *service) ListOwnedBy(ctx context.Context, ownerID uuid.UUID, limit int, startingAfter string) ([]APIKey, bool, error) {
	if ownerID == uuid.Nil {
		return nil, false, apperror.NewBadRequest("authenticated user is required")
	}
	return s.listByUser(ctx, ownerID, limit, startingAfter)
}

// ListForMember lists another member's keys. The route's admin:api_keys:read
// permission already restricts the caller to owner/admin; the user id is part
// of the query so the page is scoped to the member on the path.
func (s *service) ListForMember(ctx context.Context, memberID uuid.UUID, limit int, startingAfter string) ([]APIKey, bool, error) {
	if memberID == uuid.Nil {
		return nil, false, apperror.NewNotFound("user not found")
	}
	// Conceal an unknown, cross-tenant, or inactive member as not found instead
	// of returning an empty page for an id the caller cannot address.
	if _, err := s.loadPrincipal(ctx, memberID); err != nil {
		return nil, false, err
	}
	return s.listByUser(ctx, memberID, limit, startingAfter)
}

func (s *service) listByUser(ctx context.Context, userID uuid.UUID, limit int, startingAfter string) ([]APIKey, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	keys, err := s.keys.ListByUserCursor(ctx, userID, cur)
	if err != nil {
		return nil, false, apperror.Internal(err, "failed to list API keys")
	}
	more := hasMore(keys, pageSize)
	return page(keys, pageSize), more, nil
}

// page trims the extra row fetched to detect the next page.
func page(keys []APIKey, pageSize int) []APIKey {
	if len(keys) > pageSize {
		return keys[:pageSize]
	}
	return keys
}

// hasMore reports whether the repository returned the look-ahead row.
func hasMore(keys []APIKey, pageSize int) bool {
	return len(keys) > pageSize
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (*APIKey, error) {
	return s.get(ctx, id)
}

// GetOwnedBy returns a key only when ownerID owns it. A key belonging to
// another member is concealed as not found, never as forbidden.
func (s *service) GetOwnedBy(ctx context.Context, ownerID, id uuid.UUID) (*APIKey, error) {
	key, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if key.UserID != ownerID {
		return nil, apperror.NewNotFound("API key not found")
	}
	return key, nil
}

func (s *service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*APIKey, error) {
	existing, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	caller, hasCaller := authz.SubjectFromContext(ctx)
	isOwner := (!hasCaller && authz.IsSystemContext(ctx)) || (hasCaller && caller.UserID == existing.UserID)
	if !isOwner {
		if err := s.ensureCanManageKey(ctx, existing); err != nil {
			return nil, err
		}
	}
	return s.updateKey(ctx, existing, in, isOwner)
}

// UpdateOwnedBy updates the caller's own key. Ownership is checked before any
// mutation, so the self-service route cannot reach another member's key even
// when the caller holds api_keys:update.
func (s *service) UpdateOwnedBy(ctx context.Context, ownerID, id uuid.UUID, in UpdateInput) (*APIKey, error) {
	existing, err := s.GetOwnedBy(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}
	return s.updateKey(ctx, existing, in, true)
}

// UpdateForMember updates another member's key after the member id on the path
// and the caller's rank both allow it.
func (s *service) UpdateForMember(ctx context.Context, memberID, id uuid.UUID, in UpdateInput) (*APIKey, error) {
	existing, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.UserID != memberID {
		return nil, apperror.NewNotFound("API key not found")
	}
	if err := s.ensureCanManageKey(ctx, existing); err != nil {
		return nil, err
	}
	return s.updateKey(ctx, existing, in, false)
}

func (s *service) updateKey(ctx context.Context, existing *APIKey, in UpdateInput, isOwner bool) (*APIKey, error) {
	if in.Name == nil && in.Scopes == nil {
		return nil, apperror.NewBadRequest("at least one of name or scopes is required")
	}
	if existing.RevokedAt != nil {
		return nil, apperror.NewConflict("API key has been revoked")
	}

	name := existing.Name
	var err error
	if in.Name != nil {
		name, err = normalizeName(*in.Name)
		if err != nil {
			return nil, err
		}
	}
	scopes := existing.Scopes
	if in.Scopes != nil {
		principal, err := s.loadPrincipal(ctx, existing.UserID)
		if err != nil {
			return nil, err
		}
		scopes, err = s.normalizeScopes(in.Scopes, principal)
		if err != nil {
			return nil, err
		}
		if isOwner {
			if err := ensureCallerCanGrant(ctx, scopes); err != nil {
				return nil, err
			}
		} else if !scopesSubset(scopes, existing.Scopes) {
			// Another member may narrow or revoke a key, but never widen it:
			// otherwise an admin could grant an owner's key a scope and then
			// act with the owner's authority.
			return nil, apperror.NewCode(apperror.CodePermissionDenied, "only the key owner may widen scopes")
		}
	}

	var updated *APIKey
	err = s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		key, updateErr := tx.Keys.Update(ctx, existing.ID, name, scopes)
		if updateErr != nil {
			if errors.Is(updateErr, apperror.ErrNotFound) {
				return nil, apperror.NewNotFound("API key not found")
			}
			return nil, apperror.Internal(updateErr, "failed to update API key")
		}
		updated = key
		return keyEvent(ctx, "api_key.updated", key, nil), nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *service) Revoke(ctx context.Context, id uuid.UUID) error {
	existing, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.ensureCanManageKey(ctx, existing); err != nil {
		return err
	}
	return s.revoke(ctx, existing)
}

// RevokeOwnedBy revokes the caller's own key.
func (s *service) RevokeOwnedBy(ctx context.Context, ownerID, id uuid.UUID) error {
	existing, err := s.GetOwnedBy(ctx, ownerID, id)
	if err != nil {
		return err
	}
	return s.revoke(ctx, existing)
}

// RevokeForMember revokes another member's key after the member id on the path
// and the caller's rank both allow it.
func (s *service) RevokeForMember(ctx context.Context, memberID, id uuid.UUID) error {
	existing, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if existing.UserID != memberID {
		return apperror.NewNotFound("API key not found")
	}
	if err := s.ensureCanManageKey(ctx, existing); err != nil {
		return err
	}
	return s.revoke(ctx, existing)
}

func (s *service) revoke(ctx context.Context, existing *APIKey) error {
	if existing.RevokedAt != nil {
		// Revocation is idempotent and writes no second audit row.
		return nil
	}
	return s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		revoked, err := tx.Keys.Revoke(ctx, existing.ID)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				// A concurrent revoke won the race; the outcome is the same.
				current, getErr := tx.Keys.GetByID(ctx, existing.ID)
				if getErr != nil {
					if errors.Is(getErr, apperror.ErrNotFound) {
						return nil, apperror.NewNotFound("API key not found")
					}
					return nil, apperror.Internal(getErr, "failed to revoke API key")
				}
				if current.RevokedAt != nil {
					return nil, nil
				}
				return nil, apperror.NewNotFound("API key not found")
			}
			return nil, apperror.Internal(err, "failed to revoke API key")
		}
		return keyEvent(ctx, "api_key.revoked", revoked, nil), nil
	})
}

func (s *service) Rotate(ctx context.Context, callerUserID, id uuid.UUID) (*RotatedKey, error) {
	graceSeconds := int32(RotationGrace / time.Second)

	var result *RotatedKey
	err := s.mutate(ctx, func(ctx context.Context, tx TxRepos) (*audit.Event, error) {
		rotated, rotateErr := s.rotate(ctx, tx, callerUserID, id, graceSeconds)
		if rotateErr != nil {
			return nil, rotateErr
		}
		result = rotated
		event := audit.NewServiceEvent(ctx, "api_key.rotated", "api_key")
		event.ResourceID = rotated.PreviousKeyID.String()
		event.Metadata = map[string]any{
			"successorId":           rotated.ID.String(),
			"previousKeyValidUntil": rotated.PreviousKeyValidUntil,
			"ownerUserId":           rotated.UserID.String(),
		}
		return &event, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *service) rotate(ctx context.Context, tx TxRepos, callerUserID, id uuid.UUID, graceSeconds int32) (*RotatedKey, error) {
	existing, err := tx.Keys.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("API key not found")
		}
		return nil, apperror.Internal(err, "failed to rotate API key")
	}
	// Rotation returns a fresh secret, so it is owner-only. Another member's
	// key is concealed as not found (the list/revoke admin surface stays
	// available) and its state is checked after this so a revoked key does not
	// reveal its existence either.
	if existing.UserID != callerUserID {
		return nil, apperror.NewNotFound("API key not found")
	}
	if existing.RevokedAt != nil {
		return nil, apperror.NewConflict("API key has been revoked")
	}
	if existing.RotatedAt != nil {
		return nil, apperror.NewConflict("API key has already been rotated")
	}
	// A key past its expiry is already unusable; a successor inheriting that
	// expiry would be dead on arrival.
	if !existing.ExpiresAt.After(s.now()) {
		return nil, apperror.NewConflict("API key has expired")
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
		return nil, apperror.Internal(err, "failed to rotate API key")
	}

	credentials, err := apikey.Generate()
	if err != nil {
		return nil, apperror.Internal(err, "failed to generate API key")
	}
	created, err := tx.Keys.Create(ctx, NewKey{
		UserID:    rotated.UserID,
		Name:      rotated.Name,
		Prefix:    credentials.Prefix,
		Hash:      credentials.Hash,
		Scopes:    rotated.Scopes,
		ExpiresAt: s.rotationExpiry(rotated.ExpiresAt, s.now()),
	})
	if err != nil {
		return nil, apperror.Internal(err, "failed to create rotated API key")
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

func (s *service) get(ctx context.Context, id uuid.UUID) (*APIKey, error) {
	key, err := s.keys.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("API key not found")
		}
		return nil, apperror.Internal(err, "failed to get API key")
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
func (s *service) lockActiveOwner(ctx context.Context, tx TxRepos, userID uuid.UUID) (identitycontracts.Principal, error) {
	if s.principals == nil {
		return s.loadPrincipal(ctx, userID)
	}
	if tx.Principals == nil {
		slog.Error("API key owner reader cannot lock the owner row; refusing the key write")
		return identitycontracts.Principal{}, apperror.NewInternal("failed to load API key owner")
	}
	return s.checkPrincipal(ctx, userID, func(tenantID uuid.UUID) (identitycontracts.Principal, error) {
		return tx.Principals.LockPrincipal(ctx, tenantID, userID)
	})
}

func (s *service) loadPrincipal(ctx context.Context, userID uuid.UUID) (identitycontracts.Principal, error) {
	if s.principals == nil {
		// Tests and minimal deployments may omit the identity adapter. Scope
		// validation is then limited to the catalog; production always wires it.
		tenantID, err := tenant.Require(ctx)
		if err != nil {
			return identitycontracts.Principal{}, apperror.Internal(err, "tenant context is required")
		}
		return identitycontracts.Principal{ID: userID, TenantID: tenantID}, nil
	}
	return s.checkPrincipal(ctx, userID, func(tenantID uuid.UUID) (identitycontracts.Principal, error) {
		return s.principals.GetPrincipal(ctx, tenantID, userID)
	})
}

// checkPrincipal runs load for the request tenant and conceals a missing,
// inactive, or cross-tenant owner as 404 user not found.
func (s *service) checkPrincipal(
	ctx context.Context,
	userID uuid.UUID,
	load func(tenantID uuid.UUID) (identitycontracts.Principal, error),
) (identitycontracts.Principal, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return identitycontracts.Principal{}, apperror.Internal(err, "tenant context is required")
	}
	principal, err := load(tenantID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return identitycontracts.Principal{}, apperror.NewNotFound("user not found")
		}
		return identitycontracts.Principal{}, apperror.Internal(err, "failed to load API key owner")
	}
	if principal.ID != userID || principal.TenantID != tenantID {
		return identitycontracts.Principal{}, apperror.NewNotFound("user not found")
	}
	return principal, nil
}

// ensureCanManageKey applies identity's member hierarchy to another member's
// key. The owner manages their own keys; a trusted system caller is allowed;
// everyone else must outrank the key's owner. A wired rank checker is required
// for a real caller, so a missing one fails closed instead of skipping the
// check.
func (s *service) ensureCanManageKey(ctx context.Context, key *APIKey) error {
	caller, hasCaller := authz.SubjectFromContext(ctx)
	if !hasCaller {
		if authz.IsSystemContext(ctx) {
			return nil
		}
		return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
	}
	if caller.UserID == key.UserID {
		return nil
	}
	if s.ranks == nil {
		slog.Error("API-key management has no member rank checker; refusing a cross-member operation",
			slog.String("key_id", key.ID.String()),
			slog.String("owner_user_id", key.UserID.String()),
		)
		return apperror.NewInternal("failed to check API key owner rank")
	}
	return s.ranks.CanManageMember(ctx, key.UserID)
}

// scopesSubset reports whether every scope in candidate is already present in
// allowed.
func scopesSubset(candidate, allowed []authz.Permission) bool {
	have := make(map[authz.Permission]struct{}, len(allowed))
	for _, scope := range allowed {
		have[scope] = struct{}{}
	}
	for _, scope := range candidate {
		if _, ok := have[scope]; !ok {
			return false
		}
	}
	return true
}

func (s *service) normalizeScopes(raw []string, principal identitycontracts.Principal) ([]authz.Permission, error) {
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
		if !s.authorizer.IsKnownPermission(permission) {
			return nil, apperror.NewBadRequest(fmt.Sprintf("unknown scope %q", value))
		}
		if !s.ownerHasPermission(principal, permission) {
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

func (s *service) ownerHasPermission(principal identitycontracts.Principal, permission authz.Permission) bool {
	if principal.Permissions == nil {
		// An empty role is not a valid principal. Falling through here would
		// treat a partially loaded owner as holding every scope in the catalog.
		return principal.Role != "" && s.authorizer.Has(principal.Role, permission)
	}
	for _, granted := range principal.Permissions {
		if granted == string(permission) {
			return true
		}
	}
	return false
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

// expiry applies APIKEYS_MAX_LIFETIME to a new key: an omitted expiresAt
// becomes now + max, a later one is rejected, and an earlier one is preserved.
func (s *service) expiry(requested *time.Time, now time.Time) (time.Time, error) {
	now = now.UTC()
	latest := now.Add(s.maxLifetime)
	if requested == nil {
		return latest, nil
	}
	value := requested.UTC()
	if !value.After(now) {
		return time.Time{}, apperror.NewBadRequest("expiresAt must be in the future")
	}
	if value.After(latest) {
		return time.Time{}, apperror.NewCode(apikeyscatalog.CodeAPIKeyExpiryExceedsMaxLifetime,
			"expiresAt exceeds APIKEYS_MAX_LIFETIME")
	}
	return value, nil
}

// rotationExpiry never extends a successor beyond the original key's expiry or
// the configured maximum.
func (s *service) rotationExpiry(original, now time.Time) time.Time {
	latest := now.UTC().Add(s.maxLifetime)
	if original.IsZero() || original.After(latest) {
		return latest
	}
	return original.UTC()
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
