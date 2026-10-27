package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// APIKey is the module's persistence-neutral representation. Hash never leaves
// the module/platform boundary and is never serialized to HTTP clients.
type APIKey struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	UserID             uuid.UUID
	Name               string
	Prefix             string
	Hash               []byte
	Scopes             []authz.Permission
	ExpiresAt          time.Time
	RevokedAt          *time.Time
	RotatedAt          *time.Time
	RotationGraceUntil *time.Time
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// NewKey is the input for creating a key row. The owning tenant is not a
// field: the repository always takes it from the request context. Expiry is
// required: every key expires.
type NewKey struct {
	UserID    uuid.UUID
	Name      string
	Prefix    string
	Hash      []byte
	Scopes    []authz.Permission
	ExpiresAt time.Time
}

// CreateInput asks for a new key. The owner is always the authenticated
// caller, so it is not part of the input.
type CreateInput struct {
	Name      string
	Scopes    []string
	ExpiresAt *time.Time
}

// UpdateInput changes mutable metadata. Nil fields are unchanged.
type UpdateInput struct {
	Name   *string
	Scopes []string
}

// CreatedKey is a new key and its plaintext secret, returned exactly once.
type CreatedKey struct {
	APIKey
	Secret string
}

// RotatedKey is the replacement key and the grace window granted to its
// predecessor.
type RotatedKey struct {
	CreatedKey
	PreviousKeyID         uuid.UUID
	PreviousKeyValidUntil time.Time
}

// APIKeyRepository is the key persistence port. Implementations scope every
// tenant-facing query to the request tenant (tenant.Require(ctx)) and return
// apperror.ErrNotFound when no row matches. GetByHash is the deliberate
// exception: it is the pre-tenant possession lookup.
type APIKeyRepository interface {
	Create(ctx context.Context, key NewKey) (*APIKey, error)
	GetByID(ctx context.Context, id uuid.UUID) (*APIKey, error)
	GetByHash(ctx context.Context, hash []byte) (*APIKey, error)
	ListCursor(ctx context.Context, cur pagination.DBCursor) ([]APIKey, error)
	ListByUserCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor) ([]APIKey, error)
	Update(ctx context.Context, id uuid.UUID, name string, scopes []authz.Permission) (*APIKey, error)
	Revoke(ctx context.Context, id uuid.UUID) (*APIKey, error)
	MarkRotated(ctx context.Context, id uuid.UUID, graceSeconds int32) (*APIKey, error)
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error
}

// AuditWriter records a Tier 1 audit event in the current transaction, so the
// event commits or rolls back with the mutation.
type AuditWriter interface {
	RecordTx(ctx context.Context, event audit.Event) error
}

// PrincipalLocker loads an API-key owner inside the transaction and locks the
// membership row until the key write commits.
type PrincipalLocker interface {
	LockPrincipal(ctx context.Context, tenantID, userID uuid.UUID) (identitycontracts.Principal, error)
}

// TxRepos are the collaborators bound to one transaction. Principals is nil
// when the wired identity reader cannot lock; the service then fails closed.
type TxRepos struct {
	Keys       APIKeyRepository
	Audit      AuditWriter
	Principals PrincipalLocker
}

// Transactor runs fn in one transaction: when fn returns an error every write
// made through tx rolls back, otherwise all of them commit together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error
}
