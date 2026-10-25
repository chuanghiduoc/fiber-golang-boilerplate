// Package repository owns API-key persistence. Every tenant-facing query is
// scoped by the tenant resolved from the request context; the only global
// lookup is by key hash, which is unique and used before a request tenant can
// be trusted.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	apikeyssqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// APIKey is the module's persistence-neutral representation. Hash never leaves
// the module/platform boundary and is never serialized to HTTP clients.
type APIKey struct {
	ID                 int64
	TenantID           int64
	UserID             int64
	Name               string
	Prefix             string
	Hash               string
	Scopes             []authz.Permission
	ExpiresAt          *time.Time
	RevokedAt          *time.Time
	RotatedAt          *time.Time
	RotationGraceUntil *time.Time
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// CreateParams creates a key for a tenant/user. TenantID is overridden by the
// repository with the request tenant.
type CreateParams struct {
	TenantID  int64
	UserID    int64
	Name      string
	Prefix    string
	Hash      string
	Scopes    []authz.Permission
	ExpiresAt *time.Time
}

// APIKeyRepository is the module's data-access contract.
type APIKeyRepository interface {
	Create(ctx context.Context, params CreateParams) (*APIKey, error)
	GetByID(ctx context.Context, id int64) (*APIKey, error)
	GetByHash(ctx context.Context, hash string) (*APIKey, error)
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]APIKey, error)
	Update(ctx context.Context, id int64, name string, scopes []authz.Permission) (*APIKey, error)
	Revoke(ctx context.Context, id int64) (*APIKey, error)
	MarkRotated(ctx context.Context, id int64, graceSeconds int32) (*APIKey, error)
	TouchLastUsed(ctx context.Context, id int64, at time.Time) error
}

type apiKeyRepository struct {
	q *apikeyssqlc.Queries
}

func NewAPIKeyRepository(db apikeyssqlc.DBTX) APIKeyRepository {
	return &apiKeyRepository{q: apikeyssqlc.New(db)}
}

func (r *apiKeyRepository) Create(ctx context.Context, params CreateParams) (*APIKey, error) {
	row, err := r.q.CreateAPIKey(ctx, apikeyssqlc.CreateAPIKeyParams{
		Scopes:    scopeStrings(params.Scopes),
		TenantID:  tenant.EffectiveID(ctx),
		UserID:    params.UserID,
		Name:      params.Name,
		Prefix:    params.Prefix,
		KeyHash:   params.Hash,
		ExpiresAt: timestamptz(params.ExpiresAt),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *apiKeyRepository) GetByID(ctx context.Context, id int64) (*APIKey, error) {
	row, err := r.q.GetAPIKeyByID(ctx, apikeyssqlc.GetAPIKeyByIDParams{
		ID:       id,
		TenantID: tenant.EffectiveID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *apiKeyRepository) GetByHash(ctx context.Context, hash string) (*APIKey, error) {
	row, err := r.q.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *apiKeyRepository) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]APIKey, error) {
	rows, err := r.q.ListAPIKeysCursor(ctx, apikeyssqlc.ListAPIKeysCursorParams{
		TenantID:        tenant.EffectiveID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, err
	}
	keys := make([]APIKey, len(rows))
	for i := range rows {
		keys[i] = *toAPIKey(keyRow(rows[i]))
	}
	return keys, nil
}

func (r *apiKeyRepository) Update(ctx context.Context, id int64, name string, scopes []authz.Permission) (*APIKey, error) {
	row, err := r.q.UpdateAPIKey(ctx, apikeyssqlc.UpdateAPIKeyParams{
		Scopes:   scopeStrings(scopes),
		Name:     name,
		ID:       id,
		TenantID: tenant.EffectiveID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *apiKeyRepository) Revoke(ctx context.Context, id int64) (*APIKey, error) {
	row, err := r.q.RevokeAPIKey(ctx, apikeyssqlc.RevokeAPIKeyParams{
		ID:       id,
		TenantID: tenant.EffectiveID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *apiKeyRepository) MarkRotated(ctx context.Context, id int64, graceSeconds int32) (*APIKey, error) {
	row, err := r.q.MarkAPIKeyRotated(ctx, apikeyssqlc.MarkAPIKeyRotatedParams{
		GraceSeconds: graceSeconds,
		ID:           id,
		TenantID:     tenant.EffectiveID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toAPIKey(keyRow(row)), nil
}

func (r *apiKeyRepository) TouchLastUsed(ctx context.Context, id int64, at time.Time) error {
	return r.q.TouchAPIKeyLastUsed(ctx, apikeyssqlc.TouchAPIKeyLastUsedParams{
		ID:         id,
		LastUsedAt: timestamptz(&at),
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
		apikeyssqlc.ListAPIKeysCursorRow | apikeyssqlc.UpdateAPIKeyRow | apikeyssqlc.RevokeAPIKeyRow |
		apikeyssqlc.MarkAPIKeyRotatedRow
}

func keyRow[T apiKeyRowTypes](row T) apiKeyRow {
	return apiKeyRow(row)
}

func toAPIKey(row apiKeyRow) *APIKey {
	scopes := make([]authz.Permission, len(row.Scopes))
	for i, scope := range row.Scopes {
		scopes[i] = authz.Permission(scope)
	}
	return &APIKey{
		ID:                 row.ID,
		TenantID:           row.TenantID,
		UserID:             row.UserID,
		Name:               row.Name,
		Prefix:             row.Prefix,
		Hash:               row.KeyHash,
		Scopes:             scopes,
		ExpiresAt:          timePtr(row.ExpiresAt),
		RevokedAt:          timePtr(row.RevokedAt),
		RotatedAt:          timePtr(row.RotatedAt),
		RotationGraceUntil: timePtr(row.RotationGraceUntil),
		LastUsedAt:         timePtr(row.LastUsedAt),
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

func timestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	// fk_api_keys_tenant_user: the owner does not exist in the key's tenant.
	// Surface it like any other missing owner instead of a 500.
	if _, ok := database.ForeignKeyViolation(err); ok {
		return apperror.ErrNotFound
	}
	return err
}
