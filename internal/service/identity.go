package service

import "strings"

// normalizeEmail canonicalizes an address before storage, lookup, and
// rate-limit keying. Email local-parts are technically case-sensitive, but every
// mainstream provider treats them case-insensitively and the API contract
// promises one account per address.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
