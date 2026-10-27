// Package repository owns API-key persistence. Every tenant-facing query is
// scoped by the tenant resolved from the request context; the only global
// lookup is by key hash, which is unique and used before a request tenant can
// be trusted.
package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	apikeyssqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// APIKeyRepository implements service.APIKeyRepository over sqlc.
type APIKeyRepository struct {
	q *apikeyssqlc.Queries
}

var _ apikeysvc.APIKeyRepository = (*APIKeyRepository)(nil)

// NewAPIKeyRepository binds the repository to db: a pool for standalone reads
// and writes, or a pgx.Tx inside Transactor.InTx.
func NewAPIKeyRepository(db apikeyssqlc.DBTX) *APIKeyRepository {
	return &APIKeyRepository{q: apikeyssqlc.New(db)}
}

func (r *APIKeyRepository) Create(ctx context.Context, key apikeysvc.NewKey) (*apikeysvc.APIKey, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.CreateAPIKey(ctx, apikeyssqlc.CreateAPIKeyParams{
		Scopes:    scopeStrings(key.Scopes),
		TenantID:  tenantID,
		UserID:    key.UserID,
		Name:      key.Name,
		Prefix:    key.Prefix,
		KeyHash:   key.Hash,
		ExpiresAt: database.TimestamptzPtr(&key.ExpiresAt),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *APIKeyRepository) GetByID(ctx context.Context, id uuid.UUID) (*apikeysvc.APIKey, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetAPIKeyByID(ctx, apikeyssqlc.GetAPIKeyByIDParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *APIKeyRepository) GetByHash(ctx context.Context, hash []byte) (*apikeysvc.APIKey, error) {
	// Possession of the hash authenticates the caller before a request tenant
	// can be trusted: the key's tenant is learned from this row. This single
	// statement is therefore a deliberate cross-tenant read; the verifier
	// still compares the key's tenant with an explicit X-Tenant-Slug and
	// otherwise authorizes the key's own tenant. Nothing else inherits the
	// bypass.
	row, err := r.q.GetAPIKeyByHash(database.WithRLSBypass(ctx), hash)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *APIKeyRepository) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]apikeysvc.APIKey, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListAPIKeysCursor(ctx, apikeyssqlc.ListAPIKeysCursorParams{
		TenantID:        tenantID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, err
	}
	return toAPIKeys(rows), nil
}

func (r *APIKeyRepository) ListByUserCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor) ([]apikeysvc.APIKey, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListAPIKeysByUserCursor(ctx, apikeyssqlc.ListAPIKeysByUserCursorParams{
		TenantID:        tenantID,
		UserID:          userID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, err
	}
	return toAPIKeys(rows), nil
}

func toAPIKeys[T apiKeyRowTypes](rows []T) []apikeysvc.APIKey {
	keys := make([]apikeysvc.APIKey, len(rows))
	for i := range rows {
		keys[i] = *toAPIKey(keyRow(rows[i]))
	}
	return keys
}

func (r *APIKeyRepository) Update(ctx context.Context, id uuid.UUID, name string, scopes []authz.Permission) (*apikeysvc.APIKey, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.UpdateAPIKey(ctx, apikeyssqlc.UpdateAPIKeyParams{
		Scopes:   scopeStrings(scopes),
		Name:     name,
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *APIKeyRepository) Revoke(ctx context.Context, id uuid.UUID) (*apikeysvc.APIKey, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.RevokeAPIKey(ctx, apikeyssqlc.RevokeAPIKeyParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *APIKeyRepository) MarkRotated(ctx context.Context, id uuid.UUID, graceSeconds int32) (*apikeysvc.APIKey, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.MarkAPIKeyRotated(ctx, apikeyssqlc.MarkAPIKeyRotatedParams{
		GraceSeconds: graceSeconds,
		ID:           id,
		TenantID:     tenantID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	return r.q.TouchAPIKeyLastUsed(ctx, apikeyssqlc.TouchAPIKeyLastUsedParams{
		ID:         id,
		LastUsedAt: database.TimestamptzPtr(&at),
	})
}

// scopeStrings converts scopes to the text[] parameter. nil becomes an empty
// array: unnest(NULL) inserts nothing, but the returned scopes column must be
// an empty list, not NULL.
func scopeStrings(scopes []authz.Permission) []string {
	out := make([]string, len(scopes))
	for i, scope := range scopes {
		out[i] = string(scope)
	}
	return out
}

// apiKeyRow is the column set every api_keys query returns: the table columns
// followed by the sorted scopes array. sqlc emits one identically shaped row
// struct per query; keyRow converts any of them.
type apiKeyRow = apikeyssqlc.GetAPIKeyByIDRow

type apiKeyRowTypes interface {
	apikeyssqlc.CreateAPIKeyRow | apikeyssqlc.GetAPIKeyByIDRow | apikeyssqlc.GetAPIKeyByHashRow |
		apikeyssqlc.ListAPIKeysCursorRow | apikeyssqlc.ListAPIKeysByUserCursorRow |
		apikeyssqlc.UpdateAPIKeyRow | apikeyssqlc.RevokeAPIKeyRow |
		apikeyssqlc.MarkAPIKeyRotatedRow
}

func keyRow[T apiKeyRowTypes](row T) apiKeyRow {
	return apiKeyRow(row)
}

func toAPIKey(row apiKeyRow) *apikeysvc.APIKey {
	scopes := make([]authz.Permission, len(row.Scopes))
	for i, scope := range row.Scopes {
		scopes[i] = authz.Permission(scope)
	}
	return &apikeysvc.APIKey{
		ID:                 row.ID,
		TenantID:           row.TenantID,
		UserID:             row.UserID,
		Name:               row.Name,
		Prefix:             row.Prefix,
		Hash:               row.KeyHash,
		Scopes:             scopes,
		ExpiresAt:          row.ExpiresAt.Time,
		RevokedAt:          database.TimePtr(row.RevokedAt),
		RotatedAt:          database.TimePtr(row.RotatedAt),
		RotationGraceUntil: database.TimePtr(row.RotationGraceUntil),
		LastUsedAt:         database.TimePtr(row.LastUsedAt),
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}
}

func wrapErr(err error) error {
	if mapped := database.MapError(err); mapped != err {
		return mapped
	}
	// fk_api_keys_tenant_user: the owner does not exist in the key's tenant.
	// Surface it like any other missing owner instead of a 500.
	if _, ok := database.ForeignKeyViolation(err); ok {
		return apperror.ErrNotFound
	}
	return err
}
