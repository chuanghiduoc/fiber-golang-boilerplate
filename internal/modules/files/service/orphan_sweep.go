package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const (
	// OrphanSweepJobKind removes storage blobs that no files row references.
	// The scheduler enqueues it hourly.
	OrphanSweepJobKind = "files.orphans.sweep"
	// OrphanSweepInterval is how often unreferenced blobs are swept.
	OrphanSweepInterval = time.Hour
	// orphanSweepGrace keeps an object younger than this after its write: an
	// upload that is still between Put and its metadata commit must never be
	// mistaken for an orphan.
	orphanSweepGrace = time.Hour
	// orphanSweepDeleteBatch bounds how many orphan blobs one run attempts to
	// delete. A live object never consumes the budget, so a bucket with many
	// live files cannot starve later orphans.
	orphanSweepDeleteBatch = 1000
	// orphanSweepLookupBatch bounds one ExistingStoragePaths query.
	orphanSweepLookupBatch = 1000
)

// OrphanLookup reports which storage keys have a metadata row in the context
// tenant.
type OrphanLookup interface {
	ExistingStoragePaths(ctx context.Context, paths []string) (map[string]struct{}, error)
}

// OrphanSweeper deletes file blobs left behind by a failed upload or a lost
// metadata row. It only considers the files layout <tenantID>/<userID>/<name>;
// data-export blobs (<tenantID>/<userID>/exports/...) and anything else are
// left alone.
type OrphanSweeper struct {
	lookup  OrphanLookup
	storage storage.Storage
	now     func() time.Time
}

func NewOrphanSweeper(lookup OrphanLookup, store storage.Storage) *OrphanSweeper {
	return &OrphanSweeper{
		lookup:  lookup,
		storage: store,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// HandleJob lists storage objects, keeps those older than the grace period,
// and deletes the ones with no files row in their tenant. It attempts at most
// one bounded batch of deletes per run; the scheduler runs it hourly, so a
// backlog drains over several runs. Individual delete failures are logged and
// retried by a later run rather than failing the whole sweep.
func (s *OrphanSweeper) HandleJob(ctx context.Context, _ json.RawMessage) error {
	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return fmt.Errorf("files: list storage objects: %w", err)
	}
	cutoff := s.now().Add(-orphanSweepGrace)
	byTenant := make(map[uuid.UUID][]string)
	for _, object := range objects {
		if !object.ModifiedAt.Before(cutoff) {
			continue
		}
		tenantID, ok := fileBlobTenant(object.Path)
		if !ok {
			continue
		}
		byTenant[tenantID] = append(byTenant[tenantID], object.Path)
	}

	var attempted, swept int
	for tenantID, paths := range byTenant {
		for start := 0; start < len(paths); start += orphanSweepLookupBatch {
			end := min(start+orphanSweepLookupBatch, len(paths))
			existing, err := s.lookup.ExistingStoragePaths(tenant.WithID(ctx, tenantID), paths[start:end])
			if err != nil {
				return fmt.Errorf("files: check orphan candidates for tenant %s: %w", tenantID, err)
			}
			for _, path := range paths[start:end] {
				if _, ok := existing[path]; ok {
					continue
				}
				if attempted >= orphanSweepDeleteBatch {
					slog.Info("files: swept orphan blobs", slog.Int("objects", swept))
					return nil
				}
				attempted++
				if err := s.storage.Delete(ctx, path); err != nil {
					slog.Error("files: failed to delete orphan blob; a later sweep retries",
						slog.String("tenant_id", tenantID.String()), slog.String("path", path), slog.Any("error", err))
					continue
				}
				swept++
			}
		}
	}
	if swept > 0 {
		slog.Info("files: swept orphan blobs", slog.Int("objects", swept))
	}
	return nil
}

// fileBlobTenant validates the files blob layout and returns its tenant. Only
// exactly <tenant>/<user>/<name> keys with UUID tenant and user segments
// qualify, so export blobs and unrelated objects are never deleted.
func fileBlobTenant(path string) (uuid.UUID, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return uuid.Nil, false
	}
	tenantID, err := uuid.Parse(parts[0])
	if err != nil || tenantID == uuid.Nil {
		return uuid.Nil, false
	}
	userID, err := uuid.Parse(parts[1])
	if err != nil || userID == uuid.Nil {
		return uuid.Nil, false
	}
	return tenantID, true
}
