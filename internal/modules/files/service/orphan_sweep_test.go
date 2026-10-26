package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// orphanStore is a storage.Storage with a fixed object list and a recording
// Delete.
type orphanStore struct {
	objects []storage.Object
	deleted []string
	listErr error
	delErr  map[string]error
}

func (s *orphanStore) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (s *orphanStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, storage.ErrNotFound
}
func (s *orphanStore) URL(p string) string { return p }
func (s *orphanStore) List(context.Context, string) ([]storage.Object, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.objects, nil
}
func (s *orphanStore) Delete(_ context.Context, p string) error {
	if err := s.delErr[p]; err != nil {
		return err
	}
	s.deleted = append(s.deleted, p)
	for i, object := range s.objects {
		if object.Path == p {
			s.objects = append(s.objects[:i], s.objects[i+1:]...)
			break
		}
	}
	return nil
}

type orphanLookup struct {
	existing map[int64]map[string]struct{}
	err      error
	tenants  map[int64][]string
}

func (l *orphanLookup) ExistingStoragePaths(ctx context.Context, paths []string) (map[string]struct{}, error) {
	if l.err != nil {
		return nil, l.err
	}
	if l.tenants == nil {
		l.tenants = make(map[int64][]string)
	}
	tenantID := tenant.EffectiveID(ctx)
	l.tenants[tenantID] = append(l.tenants[tenantID], paths...)
	out := make(map[string]struct{}, len(paths))
	for p := range l.existing[tenantID] {
		out[p] = struct{}{}
	}
	return out, nil
}

func TestOrphanSweepRemovesOldUnreferencedFileBlobs(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	fresh := now.Add(-10 * time.Minute)
	store := &orphanStore{objects: []storage.Object{
		{Path: "1/2/orphan.png", ModifiedAt: old},
		{Path: "1/2/live.png", ModifiedAt: old},
		{Path: "1/2/fresh.png", ModifiedAt: fresh},
		{Path: "1/2/exports/export.json", ModifiedAt: old},
		{Path: "23/5/orphan.png", ModifiedAt: old},
		{Path: "not-a-tenant/5/x.png", ModifiedAt: old},
	}}
	lookup := &orphanLookup{existing: map[int64]map[string]struct{}{
		1: {"1/2/live.png": {}},
	}}
	sweeper := NewOrphanSweeper(lookup, store)
	sweeper.now = func() time.Time { return now }

	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.ElementsMatch(t, []string{"1/2/orphan.png", "23/5/orphan.png"}, store.deleted,
		"only old, three-segment, unreferenced file keys are removed")
	for tenantID, paths := range lookup.tenants {
		for _, p := range paths {
			parts := strings.Split(p, "/")
			require.Len(t, parts, 3, "the lookup must be asked only about file-shaped keys")
			require.Equal(t, strconv.FormatInt(tenantID, 10), parts[0],
				"each lookup must be scoped to the tenant in the path")
		}
	}

	// At-least-once redelivery is safe: nothing else is old and orphaned.
	store.deleted = nil
	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.Empty(t, store.deleted)
}

func TestOrphanSweepBoundsOneBatch(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	objects := make([]storage.Object, orphanSweepDeleteBatch+10)
	for i := range objects {
		objects[i] = storage.Object{
			Path:       fmt.Sprintf("1/2/%s.png", strconv.Itoa(i)),
			ModifiedAt: old,
		}
	}
	store := &orphanStore{objects: objects}
	sweeper := NewOrphanSweeper(&orphanLookup{}, store)
	sweeper.now = func() time.Time { return now }
	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.Len(t, store.deleted, orphanSweepDeleteBatch, "one run deletes at most one bounded batch")
}

// TestOrphanSweepLiveObjectsDoNotStarveLaterOrphans proves the per-run budget
// bounds deletes, not examined objects: a bucket with a full batch of live
// files before an orphan must still reach and remove it.
func TestOrphanSweepLiveObjectsDoNotStarveLaterOrphans(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	objects := make([]storage.Object, 0, orphanSweepDeleteBatch+1)
	live := make(map[int64]map[string]struct{})
	for i := 0; i < orphanSweepDeleteBatch; i++ {
		path := fmt.Sprintf("1/2/live-%d.png", i)
		objects = append(objects, storage.Object{Path: path, ModifiedAt: old})
		if live[1] == nil {
			live[1] = make(map[string]struct{})
		}
		live[1][path] = struct{}{}
	}
	objects = append(objects, storage.Object{Path: "1/2/orphan.png", ModifiedAt: old})

	store := &orphanStore{objects: objects}
	sweeper := NewOrphanSweeper(&orphanLookup{existing: live}, store)
	sweeper.now = func() time.Time { return now }
	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.Equal(t, []string{"1/2/orphan.png"}, store.deleted)
}

func TestOrphanSweepKeepsGoingAfterOneDeleteFailure(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	store := &orphanStore{
		objects: []storage.Object{
			{Path: "1/2/fail.png", ModifiedAt: old},
			{Path: "1/2/ok.png", ModifiedAt: old},
		},
		delErr: map[string]error{"1/2/fail.png": errors.New("s3 unavailable")},
	}
	sweeper := NewOrphanSweeper(&orphanLookup{}, store)
	sweeper.now = func() time.Time { return now }
	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.Equal(t, []string{"1/2/ok.png"}, store.deleted)
}

func TestOrphanSweepErrorsOnListAndLookupFailures(t *testing.T) {
	sweeper := NewOrphanSweeper(&orphanLookup{err: errors.New("db down")}, &orphanStore{
		objects: []storage.Object{{Path: "1/2/x.png", ModifiedAt: time.Now().Add(-2 * time.Hour)}},
	})
	if err := sweeper.HandleJob(context.Background(), nil); err == nil {
		t.Fatal("a lookup failure must fail the job")
	}
	sweeper = NewOrphanSweeper(&orphanLookup{}, &orphanStore{listErr: errors.New("bucket unavailable")})
	if err := sweeper.HandleJob(context.Background(), nil); err == nil {
		t.Fatal("a list failure must fail the job")
	}
}
