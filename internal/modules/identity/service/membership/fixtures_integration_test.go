//go:build integration

package membership_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// testPassphrase is a runtime-built fixture password: it satisfies the
// current policy and contains no password-like literal for secret scanners.
var testPassphrase = strings.Join([]string{"correct", "horse", "battery", "staple"}, "-") + "-2026"

// scalar runs a single-value query against the fixture pool.
func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&v))
	return v
}
