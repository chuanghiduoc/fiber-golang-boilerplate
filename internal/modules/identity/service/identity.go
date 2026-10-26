package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
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
	return context.WithValue(ctx, userAgentKey{}, strings.Clone(truncate(strings.TrimSpace(userAgent), maxUserAgentLength)))
}

// maxUserAgentLength matches sessions.user_agent VARCHAR(512).
const maxUserAgentLength = 512

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// clientIPFromContext returns the IP stored by WithClientIP, or "" for
// internal callers and tests, which then share one source bucket.
func clientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

func userAgentFromContext(ctx context.Context) string {
	ua, _ := ctx.Value(userAgentKey{}).(string)
	return ua
}

// emailKeyBytes is how much of the SHA-256 digest keys an address (128 bits:
// collision-free in practice, half the key length).
const emailKeyBytes = 16

// emailKey builds a per-address rate-limit cache key. The address appears
// only as a truncated SHA-256 of its normalized form, so cache keys (visible
// to cache operators, dumps, and monitoring) never carry the raw email.
// Users and their passwords are global, so the key is too: a
// password guess in one tenant counts against the same account everywhere.
func emailKey(prefix, email string) string {
	digest := sha256.Sum256([]byte(NormalizeEmail(email)))
	return prefix + hex.EncodeToString(digest[:emailKeyBytes])
}

// bannedEmailHash is the pseudonymous per-tenant ban marker stored on a
// membership: the lower-case hex SHA-256 of the normalized address. It is the
// same value BanMembership and EmailBannedInTenant compute in SQL, so the
// marker survives an erasure that rewrites users.email.
func bannedEmailHash(email string) string {
	digest := sha256.Sum256([]byte(NormalizeEmail(email)))
	return hex.EncodeToString(digest[:])
}

// hashToken is how every opaque identity token (refresh, reset,
// verification, invitation) is stored: a SHA-256 hex digest, never the
// plaintext, so a database leak cannot be replayed.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// opaqueTokenBytes is the entropy of every opaque identity token.
const opaqueTokenBytes = 32

// newOpaqueToken returns a random hex token and its storage hash.
func newOpaqueToken() (plain, hash string, err error) {
	b := make([]byte, opaqueTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	plain = hex.EncodeToString(b)
	return plain, hashToken(plain), nil
}

// tokenLink builds a frontend link that carries both the token and the tenant
// slug. Browser navigation and emailed links cannot send X-Tenant-Slug, so the
// API accepts this validated `tenant` query parameter on the browser-facing
// auth paths. For global flows (password reset, email verification) the slug
// only tells the frontend which tenant to sign in to afterwards. The slug is
// omitted for internal/test contexts that never resolved one.
func tokenLink(ctx context.Context, baseURL, path, token string) string {
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

// withLinkTenantSlug makes sure an emailed link carries the slug of the
// request tenant. The request slug is absent when an authenticated
// credential's tenant overrode a header-less default resolution (JWTAuth and
// the API-key middleware clear it); it is then looked up from the tenant ID.
func withLinkTenantSlug(ctx context.Context, tenants TenantRepository) (context.Context, error) {
	if tenant.SlugFromContext(ctx) != "" || tenants == nil {
		return ctx, nil
	}
	found, err := tenants.GetByID(ctx, tenant.EffectiveID(ctx))
	if err != nil {
		return nil, fmt.Errorf("resolve tenant slug for link: %w", err)
	}
	return tenant.WithSlug(ctx, found.Slug), nil
}
