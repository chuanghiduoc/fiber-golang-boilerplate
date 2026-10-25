package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRetentionRepo struct {
	remaining int64
	calls     int
	gotDays   int32
	err       error
}

func (r *fakeRetentionRepo) PurgeRead(_ context.Context, retentionDays, limit int32) (int64, error) {
	r.calls++
	r.gotDays = retentionDays
	if r.err != nil {
		return 0, r.err
	}
	deleted := min(r.remaining, int64(limit))
	r.remaining -= deleted
	return deleted, nil
}

func TestRetentionPurgeDrainsInBatches(t *testing.T) {
	repo := &fakeRetentionRepo{remaining: retentionPurgeBatchSize*2 + 1}
	require.NoError(t, NewRetentionPurger(repo, 180).HandleJob(context.Background(), nil))
	assert.Zero(t, repo.remaining)
	assert.Equal(t, 3, repo.calls)
	assert.Equal(t, int32(180), repo.gotDays)
}

func TestRetentionPurgeErrorFails(t *testing.T) {
	repo := &fakeRetentionRepo{err: errors.New("connection refused")}
	require.Error(t, NewRetentionPurger(repo, 180).HandleJob(context.Background(), nil))
}

func TestRetentionPurgeCanceledJobFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := &fakeRetentionRepo{err: context.Canceled}
	require.ErrorIs(t, NewRetentionPurger(repo, 180).HandleJob(ctx, nil), context.Canceled)
}
