package apikey

import (
	"context"
	"errors"
	"log/slog"
	"time"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const (
	defaultLastUsedTouchInterval = time.Minute
	lastUsedWriteTimeout         = 2 * time.Second
)

// Verified is a successful API-key authentication: the stored key record and
// the current owner principal. Callers must not log Hash or the plaintext key.
type Verified struct {
	Key       Key
	Principal identitycontracts.Principal
}

// Verifier validates credentials against the store and binds them to the
// current owner principal.
type Verifier struct {
	store           Store
	principals      identitycontracts.PrincipalReader
	now             func() time.Time
	touchInterval   time.Duration
	lastUsedTimeout time.Duration
}

// Option customizes a Verifier. Options exist for tests and deterministic
// clock injection; production uses the defaults.
type Option func(*Verifier)

// WithClock replaces the time source used for expiry and rotation grace.
func WithClock(now func() time.Time) Option {
	return func(v *Verifier) {
		if now != nil {
			v.now = now
		}
	}
}

// WithLastUsedInterval controls how often a successful authentication writes
// last_used_at. A negative interval disables the write.
func WithLastUsedInterval(interval time.Duration) Option {
	return func(v *Verifier) {
		v.touchInterval = interval
	}
}

// NewVerifier builds a verifier. principals may be nil for tests or minimal
// deployments; in that case the key's own tenant/user binding is used and the
// principal role is left empty. Production always supplies the identity
// adapter so bans, role changes, and permission changes take effect
// immediately.
func NewVerifier(store Store, principals identitycontracts.PrincipalReader, opts ...Option) *Verifier {
	v := &Verifier{
		store:           store,
		principals:      principals,
		now:             time.Now,
		touchInterval:   defaultLastUsedTouchInterval,
		lastUsedTimeout: lastUsedWriteTimeout,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Verify validates raw for the tenant selected by the request. expectedTenantID
// is only non-zero when the client explicitly supplied X-Tenant-Slug; a
// mismatch fails with 403 tenant mismatch, mirroring JWT handling. The key's
// tenant is authoritative otherwise, so non-default keys work without a header.
//
// Expired, revoked, malformed, unknown, or rotation-grace-expired keys fail
// with 401. A missing owner account or a non-active owning tenant fails with
// 401 as well.
func (v *Verifier) Verify(ctx context.Context, raw string, expectedTenantID int64) (Verified, error) {
	if v == nil || v.store == nil {
		return Verified{}, apperror.NewUnauthorized("invalid API key")
	}
	if !Valid(raw) {
		return Verified{}, apperror.NewUnauthorized("invalid API key")
	}

	key, err := v.store.FindByHash(ctx, Hash(raw))
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return Verified{}, apperror.NewUnauthorized("invalid API key")
		}
		return Verified{}, apperror.NewInternal("failed to verify API key")
	}

	if expectedTenantID != 0 && key.TenantID != expectedTenantID {
		return Verified{}, apperror.NewCode(apperror.CodePermissionDenied, "tenant mismatch")
	}

	now := v.now().UTC()
	if key.RevokedAt != nil {
		return Verified{}, apperror.NewUnauthorized("API key has been revoked")
	}
	if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
		return Verified{}, apperror.NewUnauthorized("API key has expired")
	}
	if key.RotatedAt != nil {
		// Older keys remain valid until the rotation grace window closes. An
		// explicit revocation above always wins.
		if key.RotationGraceUntil == nil || !now.Before(*key.RotationGraceUntil) {
			return Verified{}, apperror.NewUnauthorized("API key has been rotated")
		}
	}

	principal := identitycontracts.Principal{
		ID:       key.UserID,
		TenantID: key.TenantID,
	}
	if v.principals != nil {
		loaded, loadErr := v.principals.GetPrincipal(ctx, key.TenantID, key.UserID)
		if loadErr != nil {
			if errors.Is(loadErr, apperror.ErrNotFound) {
				return Verified{}, apperror.NewUnauthorized("API key owner is not active")
			}
			return Verified{}, apperror.NewInternal("failed to verify API key owner")
		}
		// The JWT path rejects suspended tenants through the session store;
		// keys must not outlive a tenant suspension either. The request tenant
		// middleware cannot catch this because the key's tenant is
		// authoritative and may differ from the header-less default.
		if loaded.TenantStatus != identitycontracts.TenantStatusActive {
			return Verified{}, apperror.NewUnauthorized("tenant is not active")
		}
		// A ban revokes every key issued before it; unban restores the account
		// but not those keys. Both timestamps come from the database clock.
		if loaded.CredentialsRevokedAt != nil && !key.CreatedAt.After(*loaded.CredentialsRevokedAt) {
			return Verified{}, apperror.NewUnauthorized("API key has been revoked")
		}
		principal = loaded
	}
	if principal.ID != key.UserID || principal.TenantID != key.TenantID {
		// Defensive: a principal lookup that crosses the key binding is invalid.
		return Verified{}, apperror.NewUnauthorized("API key owner is not active")
	}

	v.touchLastUsed(ctx, key, now)

	return Verified{Key: key, Principal: principal}, nil
}

func (v *Verifier) touchLastUsed(ctx context.Context, key Key, now time.Time) {
	if v.touchInterval < 0 {
		return
	}
	if key.LastUsedAt != nil && now.Sub(*key.LastUsedAt) < v.touchInterval {
		return
	}
	// The write is synchronous but throttled to once per touchInterval per key
	// and bounded by lastUsedTimeout, so it adds at most that latency to one
	// request per interval. A failure is logged and never fails the request.
	// It must run under the key's own tenant: the request may have resolved the
	// default tenant while the key belongs to another one, and RLS would
	// otherwise update no row.
	touchCtx := tenant.WithID(context.WithoutCancel(ctx), key.TenantID)
	touchCtx, cancel := context.WithTimeout(touchCtx, v.lastUsedTimeout)
	defer cancel()
	if err := v.store.TouchLastUsed(touchCtx, key.ID, now); err != nil {
		slog.Warn("failed to update API key last-used timestamp",
			slog.Int64("api_key_id", key.ID),
			slog.Any("error", err),
		)
	}
}
