package handler

import (
	"fmt"

	"github.com/google/uuid"
)

// testID returns the deterministic UUIDv7 fixture id for n. Tests use small
// integer ids instead of repeating UUID literals.
func testID(n uint64) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}
