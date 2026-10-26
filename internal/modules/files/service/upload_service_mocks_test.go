package service

import (
	"context"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

func timePtr(t time.Time) *time.Time { return &t }

// cursorLess reports whether (ts1,id1) sorts before (ts2,id2) in the
// (created_at DESC, id DESC) order used by cursor pagination.
func cursorLess(ts1 time.Time, id1 int64, ts2 time.Time, id2 int64) bool {
	if ts1.Equal(ts2) {
		return id1 < id2
	}
	return ts1.Before(ts2)
}

// ---------------------------------------------------------------------------
// fakeTx
// ---------------------------------------------------------------------------

// fakeTx runs fn directly against files. It has no rollback; tests that need
// atomicity run against PostgreSQL (test/integration).
type fakeTx struct {
	files  FileRepository
	audit  AuditWriter
	events *recordingPublisher
}

var _ Transactor = (*fakeTx)(nil)

// newFakeTx builds a transactor with a no-op audit writer and a recording
// event publisher.
func newFakeTx(files FileRepository) *fakeTx {
	return &fakeTx{
		files:  files,
		audit:  auditWriterFunc(func(context.Context, audit.Event) error { return nil }),
		events: &recordingPublisher{},
	}
}

func (f *fakeTx) InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error {
	return fn(ctx, TxRepos{Files: f.files, Audit: f.audit, Events: f.events})
}

type auditWriterFunc func(ctx context.Context, event audit.Event) error

func (f auditWriterFunc) RecordTx(ctx context.Context, event audit.Event) error { return f(ctx, event) }

// recordingPublisher records events published in the transaction.
type recordingPublisher struct {
	events []events.Event
	err    error
}

func (p *recordingPublisher) Publish(_ context.Context, event events.Event) error {
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, event)
	return nil
}

// ---------------------------------------------------------------------------
// mockFileRepo
// ---------------------------------------------------------------------------

type mockFileRepo struct {
	files  map[int64]*File
	nextID int64
	// quotaLocks counts LockQuota calls; the usage totals are computed from
	// files, so tests seed rows instead of stubbing them.
	quotaLocks int
}

// scoped mirrors the repository's tenant predicate so service tests cannot
// accidentally read another tenant's rows.
func (m *mockFileRepo) scoped(ctx context.Context, f *File) bool {
	return f.TenantID == tenant.EffectiveID(ctx)
}

var _ FileRepository = (*mockFileRepo)(nil)

func newMockFileRepo() *mockFileRepo {
	return &mockFileRepo{files: make(map[int64]*File), nextID: 1}
}

func (m *mockFileRepo) Create(ctx context.Context, input NewFile) (*File, error) {
	f := &File{
		ID:           m.nextID,
		TenantID:     tenant.EffectiveID(ctx),
		UserID:       input.UserID,
		OriginalName: input.OriginalName,
		StoragePath:  input.StoragePath,
		MimeType:     input.MimeType,
		Size:         input.Size,
		CreatedAt:    time.Now(),
	}
	m.files[m.nextID] = f
	m.nextID++
	return f, nil
}

func (m *mockFileRepo) GetByID(ctx context.Context, id int64) (*File, error) {
	f, ok := m.files[id]
	if !ok || f.DeletedAt != nil || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *mockFileRepo) GetByIDIncludingDeleted(ctx context.Context, id int64) (*File, error) {
	f, ok := m.files[id]
	if !ok || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *mockFileRepo) ListByUserCursor(ctx context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]File, error) {
	return m.listCursor(ctx, &userID, cur, includeDeleted), nil
}

func (m *mockFileRepo) AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]File, error) {
	return m.listCursor(ctx, nil, cur, true), nil
}

func (m *mockFileRepo) listCursor(ctx context.Context, userID *int64, cur pagination.DBCursor, includeDeleted bool) []File {
	all := make([]File, 0, len(m.files))
	for _, f := range m.files {
		if !m.scoped(ctx, f) {
			continue
		}
		if userID != nil && f.UserID != *userID {
			continue
		}
		if !includeDeleted && f.DeletedAt != nil {
			continue
		}
		all = append(all, *f)
	}
	sort.Slice(all, func(i, j int) bool {
		return cursorLess(all[j].CreatedAt, all[j].ID, all[i].CreatedAt, all[i].ID)
	})
	out := make([]File, 0, len(all))
	for _, f := range all {
		if cur.HasCursor && !cursorLess(f.CreatedAt, f.ID, cur.CreatedAt, cur.ID) {
			continue
		}
		out = append(out, f)
		if len(out) >= int(cur.Limit) {
			break
		}
	}
	return out
}

func (m *mockFileRepo) Delete(ctx context.Context, id int64) (*File, error) {
	f, ok := m.files[id]
	if !ok || f.DeletedAt != nil || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = timePtr(time.Now())
	return f, nil
}

func (m *mockFileRepo) Restore(ctx context.Context, id int64) (*File, error) {
	f, ok := m.files[id]
	if !ok || f.DeletedAt == nil || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = nil
	return f, nil
}

func (m *mockFileRepo) AdminFileStats(ctx context.Context) (FileStats, error) {
	var stats FileStats
	for _, f := range m.files {
		if f.DeletedAt != nil || !m.scoped(ctx, f) {
			continue
		}
		stats.TotalFiles++
		stats.TotalFileSize += f.Size
	}
	return stats, nil
}

func (m *mockFileRepo) LockQuota(context.Context) error {
	m.quotaLocks++
	return nil
}

func (m *mockFileRepo) StorageUsage(ctx context.Context, userID int64) (tenantBytes, userBytes int64, err error) {
	for _, f := range m.files {
		if !m.scoped(ctx, f) {
			continue
		}
		tenantBytes += f.Size
		if f.UserID == userID {
			userBytes += f.Size
		}
	}
	return tenantBytes, userBytes, nil
}

// ---------------------------------------------------------------------------
// mockStorage
// ---------------------------------------------------------------------------

type mockStorage struct {
	files          map[string][]byte
	deletedPaths   []string
	putErr         error
	getErr         error
	delErr         error
	listErr        error
	listModifiedAt time.Time
	baseURL        string
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		files:   make(map[string][]byte),
		baseURL: "http://localhost:8080/files",
	}
}

func (m *mockStorage) Put(_ context.Context, path string, reader io.Reader, _ int64, _ string) error {
	if m.putErr != nil {
		return m.putErr
	}
	data, _ := io.ReadAll(reader)
	m.files[path] = data
	return nil
}

func (m *mockStorage) Get(_ context.Context, path string) (io.ReadCloser, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	data, ok := m.files[path]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return io.NopCloser(io.NewSectionReader(readerAt(data), 0, int64(len(data)))), nil
}

func (m *mockStorage) Delete(_ context.Context, path string) error {
	if m.delErr != nil {
		return m.delErr
	}
	m.deletedPaths = append(m.deletedPaths, path)
	delete(m.files, path)
	return nil
}

func (m *mockStorage) List(_ context.Context, prefix string) ([]storage.Object, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	objects := make([]storage.Object, 0, len(m.files))
	for path, data := range m.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		objects = append(objects, storage.Object{Path: path, Size: int64(len(data)), ModifiedAt: m.listModifiedAt})
	}
	return objects, nil
}

func (m *mockStorage) URL(path string) string {
	return m.baseURL + "/" + path
}

// readerAt wraps []byte to implement io.ReaderAt
type readerAt []byte

func (r readerAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(r)) {
		return 0, io.EOF
	}
	n = copy(p, r[off:])
	if n < len(p) {
		err = io.EOF
	}
	return
}
