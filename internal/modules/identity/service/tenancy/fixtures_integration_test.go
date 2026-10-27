//go:build integration

package tenancy_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/google/uuid"
)

// testID returns the deterministic UUIDv7 fixture id for n. Integration tests
// use small integer ids instead of repeating UUID literals.
func testID(n uint64) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

// scalar runs a single-value query against the fixture pool.
func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&v))
	return v
}
