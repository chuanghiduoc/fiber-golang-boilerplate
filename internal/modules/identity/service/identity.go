package service

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// normalizeEmail canonicalizes an address before storage, lookup, and
// rate-limit keying. Email local-parts are technically case-sensitive, but every
// mainstream provider treats them case-insensitively and the API contract
// promises one account per address inside a tenant.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// scopedKey prefixes a rate-limit cache key with the request tenant. The same
// address can exist in multiple tenants, so a failed login in one tenant must
// not lock the account out in another.
func scopedKey(ctx context.Context, prefix, email string) string {
	return prefix + strings.TrimSpace(email) + ":tenant=" + strconv.FormatInt(tenant.EffectiveID(ctx), 10)
}

// tokenLink builds a frontend link that carries both the token and the tenant
// slug. Browser navigation and emailed links cannot send X-Tenant-Slug, so the
// API accepts this validated `tenant` query parameter on reset/verify. The slug
// is omitted for internal/test contexts that never resolved one; those callers
// fall back to the default tenant exactly as before.
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
