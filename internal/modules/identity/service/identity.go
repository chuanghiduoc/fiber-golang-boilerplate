package service

import (
	"context"
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
