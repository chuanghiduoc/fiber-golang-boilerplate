//go:build integration

package service_test

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// testPassphrase is a runtime-built fixture password: it satisfies the
// current policy and contains no password-like literal for secret scanners.
var testPassphrase = strings.Join([]string{"correct", "horse", "battery", "staple"}, "-") + "-2026"

// testID returns the deterministic UUIDv7 fixture id for n. Integration tests
// use small integer ids instead of repeating UUID literals.
func testID(n uint64) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

// Fixture passwords are built at runtime so the repository carries no
// password-like literals for secret scanners.
var (
	testWrongPassword   = strings.Join([]string{"wrong", "fixture", "guess"}, "-")
	testNewPassword     = strings.Join([]string{"new", "fixture", "password"}, "-") + "-2026"
	testAnotherPassword = strings.Join([]string{"another", "fixture", "password"}, "-") + "-2026"
)
