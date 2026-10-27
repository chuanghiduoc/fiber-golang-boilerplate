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

	"github.com/google/uuid"
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
	existing map[uuid.UUID]map[string]struct{}
	err      error
	tenants  map[uuid.UUID][]string
}

func (l *orphanLookup) ExistingStoragePaths(ctx context.Context, paths []string) (map[string]struct{}, error) {
	if l.err != nil {
		return nil, l.err
	}
	if l.tenants == nil {
		l.tenants = make(map[uuid.UUID][]string)
	}
	tenantID := tenant.IDFromContext(ctx)
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
	tenantOne, tenantOther := testID(1), testID(23)
	userOne, userOther := testID(2), testID(5)
	orphan := blobPath(tenantOne, userOne, "orphan.png")
	live := blobPath(tenantOne, userOne, "live.png")
	freshBlob := blobPath(tenantOne, userOne, "fresh.png")
	exportBlob := blobPath(tenantOne, userOne, "exports/export.json")
	otherOrphan := blobPath(tenantOther, userOther, "orphan.png")
	store := &orphanStore{objects: []storage.Object{
		{Path: orphan, ModifiedAt: old},
		{Path: live, ModifiedAt: old},
		{Path: freshBlob, ModifiedAt: fresh},
		{Path: exportBlob, ModifiedAt: old},
		{Path: otherOrphan, ModifiedAt: old},
		{Path: "not-a-tenant/5/x.png", ModifiedAt: old},
	}}
	lookup := &orphanLookup{existing: map[uuid.UUID]map[string]struct{}{
		tenantOne: {live: {}},
	}}
	sweeper := NewOrphanSweeper(lookup, store)
	sweeper.now = func() time.Time { return now }

	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.ElementsMatch(t, []string{orphan, otherOrphan}, store.deleted,
		"only old, three-segment, unreferenced file keys are removed")
	for tenantID, paths := range lookup.tenants {
		for _, p := range paths {
			parts := strings.Split(p, "/")
			require.Len(t, parts, 3, "the lookup must be asked only about file-shaped keys")
			require.Equal(t, tenantID.String(), parts[0],
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
			Path:       blobPath(testID(1), testID(2), strconv.Itoa(i)+".png"),
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
	tenantID, userID := testID(1), testID(2)
	objects := make([]storage.Object, 0, orphanSweepDeleteBatch+1)
	live := make(map[uuid.UUID]map[string]struct{})
	for i := 0; i < orphanSweepDeleteBatch; i++ {
		path := blobPath(tenantID, userID, fmt.Sprintf("live-%d.png", i))
		objects = append(objects, storage.Object{Path: path, ModifiedAt: old})
		if live[tenantID] == nil {
			live[tenantID] = make(map[string]struct{})
		}
		live[tenantID][path] = struct{}{}
	}
	orphan := blobPath(tenantID, userID, "orphan.png")
	objects = append(objects, storage.Object{Path: orphan, ModifiedAt: old})

	store := &orphanStore{objects: objects}
	sweeper := NewOrphanSweeper(&orphanLookup{existing: live}, store)
	sweeper.now = func() time.Time { return now }
	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.Equal(t, []string{orphan}, store.deleted)
}

func TestOrphanSweepKeepsGoingAfterOneDeleteFailure(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	failing := blobPath(testID(1), testID(2), "fail.png")
	ok := blobPath(testID(1), testID(2), "ok.png")
	store := &orphanStore{
		objects: []storage.Object{
			{Path: failing, ModifiedAt: old},
			{Path: ok, ModifiedAt: old},
		},
		delErr: map[string]error{failing: errors.New("s3 unavailable")},
	}
	sweeper := NewOrphanSweeper(&orphanLookup{}, store)
	sweeper.now = func() time.Time { return now }
	require.NoError(t, sweeper.HandleJob(context.Background(), nil))
	assert.Equal(t, []string{ok}, store.deleted)
}

func TestOrphanSweepErrorsOnListAndLookupFailures(t *testing.T) {
	sweeper := NewOrphanSweeper(&orphanLookup{err: errors.New("db down")}, &orphanStore{
		objects: []storage.Object{{Path: blobPath(testID(1), testID(2), "x.png"), ModifiedAt: time.Now().Add(-2 * time.Hour)}},
	})
	if err := sweeper.HandleJob(context.Background(), nil); err == nil {
		t.Fatal("a lookup failure must fail the job")
	}
	sweeper = NewOrphanSweeper(&orphanLookup{}, &orphanStore{listErr: errors.New("bucket unavailable")})
	if err := sweeper.HandleJob(context.Background(), nil); err == nil {
		t.Fatal("a list failure must fail the job")
	}
}
