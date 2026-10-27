package handler

import (
	"fmt"

	"github.com/google/uuid"
)

// testID builds a deterministic UUIDv7-shaped id for fixtures.
func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}
