package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// NormalizeEmail canonicalizes an address before storage, lookup, and
// rate-limit keying. Handlers use it too, so deciding whether a request
// changes the email matches the service exactly. Email local-parts are
// technically case-sensitive, but every mainstream provider treats them
// case-insensitively and the API contract promises one account per address.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

type clientIPKey struct{}

type userAgentKey struct{}

// WithClientIP records the caller's IP for per-source login throttling and
// the session record. Handlers pass Fiber's c.IP(), which honors the
// trusted-proxy configuration; the value is cloned because Fiber strings
// alias pooled request buffers. Services never read HTTP state directly.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, strings.Clone(strings.TrimSpace(ip)))
}

// WithUserAgent records the caller's User-Agent for the session record.
func WithUserAgent(ctx context.Context, userAgent string) context.Context {
	return context.WithValue(ctx, userAgentKey{}, strings.Clone(Truncate(strings.TrimSpace(userAgent), maxUserAgentLength)))
}

// maxUserAgentLength matches sessions.user_agent VARCHAR(512).
const maxUserAgentLength = 512

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ClientIPFromContext returns the IP stored by WithClientIP, or "" for
// internal callers and tests, which then share one source bucket.
func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

func UserAgentFromContext(ctx context.Context) string {
	ua, _ := ctx.Value(userAgentKey{}).(string)
	return ua
}

// emailKeyBytes is how much of the SHA-256 digest keys an address (128 bits:
// collision-free in practice, half the key length).
const emailKeyBytes = 16

// EmailKey builds a per-address rate-limit cache key. The address appears
// only as a truncated SHA-256 of its normalized form, so cache keys (visible
// to cache operators, dumps, and monitoring) never carry the raw email.
// Users and their passwords are global, so the key is too: a
// password guess in one tenant counts against the same account everywhere.
func EmailKey(prefix, email string) string {
	digest := sha256.Sum256([]byte(NormalizeEmail(email)))
	return prefix + hex.EncodeToString(digest[:emailKeyBytes])
}

// NewBanMarker returns the marker a ban records for email. The marker is a
// keyed HMAC (never computed in SQL and never an unkeyed hash), so a database
// reader cannot confirm or dictionary-recover the address.
func NewBanMarker(bans BanHasher, email string) (BanMarker, error) {
	if bans == nil {
		return BanMarker{}, apperror.NewInternal("ban marker is not configured")
	}
	hash, kid := bans.Marker(email)
	if len(hash) == 0 || kid == "" {
		return BanMarker{}, apperror.NewInternal("ban marker is not configured")
	}
	return BanMarker{Hash: hash, KeyID: kid}, nil
}

// RemovalBanMarker backfills the marker of a banned membership whose row
// predates keyed markers (banned_at set, no hash). A membership without a ban,
// or one that already carries a marker, needs none: removal preserves it.
func RemovalBanMarker(bans BanHasher, membership *Membership, email string) (BanMarker, error) {
	if membership.BannedAt == nil || len(membership.BannedEmailHash) != 0 {
		return BanMarker{}, nil
	}
	return NewBanMarker(bans, email)
}

// EmailBanned reports whether a retained ban marker in the request tenant
// matches email under the active or any previous configured key.
func (s Stores) EmailBanned(ctx context.Context, email string) (bool, error) {
	if s.Bans == nil {
		return false, apperror.NewInternal("ban marker is not configured")
	}
	banned, err := s.Memberships.EmailBanned(ctx, s.Bans.Markers(email))
	if err != nil {
		return false, apperror.NewInternal("failed to check email availability")
	}
	return banned, nil
}

// HashToken is how every opaque identity token (refresh, reset,
// verification, invitation) is stored: the raw SHA-256 digest, never the
// plaintext, so a database leak cannot be replayed.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// opaqueTokenBytes is the entropy of every opaque identity token.
const opaqueTokenBytes = 32

// NewOpaqueToken returns a random hex token and its storage hash.
func NewOpaqueToken() (plain string, hash []byte, err error) {
	b := make([]byte, opaqueTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	plain = hex.EncodeToString(b)
	return plain, HashToken(plain), nil
}

// TokenLink builds a frontend link that carries both the token and the tenant
// slug. Browser navigation and emailed links cannot send X-Tenant-Slug, so the
// API accepts this validated `tenant` query parameter on the browser-facing
// auth paths. For global flows (password reset, email verification) the slug
// only tells the frontend which tenant to sign in to afterwards. The slug is
// omitted for internal/test contexts that never resolved one.
func TokenLink(ctx context.Context, baseURL, path, token string) string {
	base := strings.TrimRight(baseURL, "/")
	link := base + "/" + strings.TrimLeft(path, "/")

	u, err := url.Parse(link)
	if err != nil {
		// Preserve the previous formatting behavior for malformed
		// configuration; config validation rejects this at startup.
		fallback := link + "?token=" + url.QueryEscape(token)
		if slug := tenant.SlugFromContext(ctx); slug != "" {
			fallback += "&tenant=" + url.QueryEscape(slug)
		}
		return fallback
	}
	q := u.Query()
	q.Set("token", token)
	if slug := tenant.SlugFromContext(ctx); slug != "" {
		q.Set("tenant", slug)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// WithLinkTenantSlug makes sure an emailed link carries the slug of the
// request tenant. The request slug is absent when an authenticated
// credential's tenant overrode a header-less default resolution (JWTAuth and
// the API-key middleware clear it); it is then looked up from the tenant ID.
func WithLinkTenantSlug(ctx context.Context, tenants TenantRepository) (context.Context, error) {
	if tenant.SlugFromContext(ctx) != "" || tenants == nil {
		return ctx, nil
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant slug for link: %w", err)
	}
	found, err := tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant slug for link: %w", err)
	}
	return tenant.WithSlug(ctx, found.Slug), nil
}
