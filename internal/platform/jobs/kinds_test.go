package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

func TestKindFilterOptionsReachTheStore(t *testing.T) {
	worker := NewWorker(nil, "filtered",
		WithKinds(" report.render ", "report.render", "", "export.csv"),
		WithExcludeKinds("email.send"),
	)
	pg, ok := worker.store.(*pgStore)
	require.True(t, ok)
	assert.Equal(t, []string{"report.render", "export.csv"}, pg.includeKinds,
		"kinds are trimmed and de-duplicated in order")
	assert.Equal(t, []string{"email.send"}, pg.excludeKinds)
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
	pool := setupIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	queue := NewQueue(testutil.Scoped(pool))
	for _, kind := range []string{"kinds.fast", "kinds.slow", "kinds.other"} {
		_, err := queue.Enqueue(ctx, Job{Kind: kind})
		require.NoError(t, err)
	}

	dedicated := &pgStore{db: testutil.Scoped(pool), includeKinds: []string{"kinds.slow"}}
	claimed, err := dedicated.Claim(ctx, "slow-worker", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, "kinds.slow", claimed[0].Kind)
	assert.GreaterOrEqual(t, claimed[0].QueueLag, time.Duration(0))

	general := &pgStore{db: testutil.Scoped(pool), excludeKinds: []string{"kinds.slow", "kinds.other"}}
	claimed, err = general.Claim(ctx, "general-worker", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, "kinds.fast", claimed[0].Kind)

	unfiltered := newPGStore(testutil.Scoped(pool))
	claimed, err = unfiltered.Claim(ctx, "any-worker", 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, "kinds.other", claimed[0].Kind)
}
