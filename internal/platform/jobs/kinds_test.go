//go:build integration

package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

func TestKindFilterOptionsReachTheStore(t *testing.T) {
	worker := NewWorker(nil, "filtered",
		WithKinds(" report.render ", "report.render", "", "export.csv"),
		WithExcludeKinds("email.send.encrypted"),
	)
	pg, ok := worker.store.(*pgStore)
	require.True(t, ok)
	assert.Equal(t, []string{"report.render", "export.csv"}, pg.includeKinds,
		"kinds are trimmed and de-duplicated in order")
	assert.Equal(t, []string{"email.send.encrypted"}, pg.excludeKinds)
}

func TestKindFilterDefaultsToNoFilter(t *testing.T) {
	worker := NewWorker(nil, "general", WithKinds(" ", ""))
	pg, ok := worker.store.(*pgStore)
	require.True(t, ok)
	// nil (not an empty slice) is what makes the claim query bind NULL and
	// skip the filter; an empty array would match nothing.
	assert.Nil(t, pg.includeKinds)
	assert.Nil(t, pg.excludeKinds)
}

// TestClaimIntegrationKindFilters proves the include/exclude filters narrow
// the due set and that the claim returns a non-negative database-clock lag.
func TestClaimIntegrationKindFilters(t *testing.T) {
	fx := setupIntegrationDB(t)
	pools := fx.Pools
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 20*time.Second)
	defer cancel()

	queue := NewQueue(pgtest.Scoped(pools))
	for _, kind := range []string{"kinds.fast", "kinds.slow", "kinds.other"} {
		_, err := queue.Enqueue(ctx, Job{Kind: kind, RunAt: time.Now().Add(-time.Minute)})
		require.NoError(t, err)
	}

	dedicated := &pgStore{db: pgtest.Scoped(pools), includeKinds: []string{"kinds.slow"}}
	batch, err := dedicated.Claim(ctx, "slow-worker", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, batch.Jobs, 1)
	assert.Equal(t, "kinds.slow", batch.Jobs[0].Kind)
	assert.GreaterOrEqual(t, batch.Jobs[0].QueueLag, time.Duration(0))

	general := &pgStore{db: pgtest.Scoped(pools), excludeKinds: []string{"kinds.slow", "kinds.other"}}
	batch, err = general.Claim(ctx, "general-worker", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, batch.Jobs, 1)
	assert.Equal(t, "kinds.fast", batch.Jobs[0].Kind)

	unfiltered := newPGStore(pgtest.Scoped(pools))
	batch, err = unfiltered.Claim(ctx, "any-worker", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, batch.Jobs, 1)
	assert.Equal(t, "kinds.other", batch.Jobs[0].Kind)
}
