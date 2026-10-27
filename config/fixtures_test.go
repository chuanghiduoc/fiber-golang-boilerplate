package config

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

// testCredential builds fixture credentials at runtime so the repository
// carries no password-like literals for secret scanners.
func testCredential(parts ...string) string {
	return strings.Join(parts, "-")
}

// mustRandomKey returns a fresh base64-encoded 32-byte key. Tests generate key
// material at runtime instead of committing literals.
func mustRandomKey() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf)
}
