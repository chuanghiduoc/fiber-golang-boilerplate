package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// fakeTrashRepo models PurgeBatch: it hands out the oldest rows still in the
// trash and removes the returned IDs only when purge succeeds.
type fakeTrashRepo struct {
	trash    []TrashedFile
	batches  int
	gotDays  int32
	batchErr error
}

func (r *fakeTrashRepo) PurgeBatch(ctx context.Context, retentionDays, limit int32, purge TrashPurgeFunc) (int64, error) {
	r.batches++
	r.gotDays = retentionDays
	if r.batchErr != nil {
		return 0, r.batchErr
	}
	batch := slices.Clone(r.trash[:min(int(limit), len(r.trash))])
	if len(batch) == 0 {
		return 0, nil
	}
	ids, err := purge(ctx, batch)
	if err != nil {
		return 0, err
	}
	r.trash = slices.DeleteFunc(r.trash, func(f TrashedFile) bool { return slices.Contains(ids, f.ID) })
	return int64(len(ids)), nil
}

// fakeBlobStore fails Delete for the paths in errs.
type fakeBlobStore struct {
	deleted []string
	errs    map[string]error
}

func (s *fakeBlobStore) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (s *fakeBlobStore) Get(context.Context, string) (io.ReadCloser, error)          { return nil, nil }
func (s *fakeBlobStore) URL(path string) string                                      { return path }
func (s *fakeBlobStore) Delete(_ context.Context, path string) error {
	if err := s.errs[path]; err != nil {
		return err
	}
	s.deleted = append(s.deleted, path)
	return nil
}

func trashOf(n int) []TrashedFile {
	files := make([]TrashedFile, n)
	for i := range files {
		files[i] = TrashedFile{ID: int64(i + 1), TenantID: int64(i%2 + 1), StoragePath: fmt.Sprintf("%d/7/blob-%d.png", i%2+1, i+1)}
	}
	return files
}

func TestTrashPurgeDrainsInBatches(t *testing.T) {
	repo := &fakeTrashRepo{trash: trashOf(trashPurgeBatchSize*2 + 5)}
	store := &fakeBlobStore{}
	purger := NewTrashPurger(repo, store, 30)

	require.NoError(t, purger.HandleJob(context.Background(), nil))
	assert.Empty(t, repo.trash)
	assert.Equal(t, 3, repo.batches, "two full batches and one short batch")
	assert.Len(t, store.deleted, trashPurgeBatchSize*2+5)
	assert.Equal(t, int32(30), repo.gotDays)

	// At-least-once redelivery finds nothing left and still succeeds.
	require.NoError(t, purger.HandleJob(context.Background(), nil))
}

func TestTrashPurgeMissingBlobIsSuccess(t *testing.T) {
	repo := &fakeTrashRepo{trash: trashOf(2)}
	store := &fakeBlobStore{errs: map[string]error{
		repo.trash[0].StoragePath: fmt.Errorf("wrapped: %w", storage.ErrNotFound),
	}}

	require.NoError(t, NewTrashPurger(repo, store, 30).HandleJob(context.Background(), nil))
	assert.Empty(t, repo.trash, "a row whose blob is already gone is hard-deleted")
}

func TestTrashPurgeKeepsRowsWhoseBlobDeleteFailed(t *testing.T) {
	repo := &fakeTrashRepo{trash: trashOf(3)}
	failing := repo.trash[1]
	store := &fakeBlobStore{errs: map[string]error{failing.StoragePath: errors.New("s3 unavailable")}}

	err := NewTrashPurger(repo, store, 30).HandleJob(context.Background(), nil)
	require.Error(t, err, "a failed blob delete is reported so the job retries")
	assert.Equal(t, []TrashedFile{failing}, repo.trash, "only the failed row stays in the trash")
	assert.Equal(t, 1, repo.batches, "the run stops instead of reselecting the failed row")
}

func TestTrashPurgeRepositoryErrorFails(t *testing.T) {
	repo := &fakeTrashRepo{batchErr: errors.New("connection refused")}
	require.Error(t, NewTrashPurger(repo, &fakeBlobStore{}, 30).HandleJob(context.Background(), nil))
}

func TestTrashPurgeCanceledJobFailsWithoutDeletingRows(t *testing.T) {
	repo := &fakeTrashRepo{trash: trashOf(2)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewTrashPurger(repo, &fakeBlobStore{}, 30).HandleJob(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, repo.trash, 2, "a canceled batch hard-deletes nothing")
}

func TestTrashPurgeDriverTimeoutIsNotBudgetExhaustion(t *testing.T) {
	// A driver timeout (e.g. pgx connect) also wraps DeadlineExceeded, but the
	// run budget is untouched, so the job must fail and retry.
	repo := &fakeTrashRepo{batchErr: fmt.Errorf("connect: %w", context.DeadlineExceeded)}
	err := NewTrashPurger(repo, &fakeBlobStore{}, 30).HandleJob(context.Background(), nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
