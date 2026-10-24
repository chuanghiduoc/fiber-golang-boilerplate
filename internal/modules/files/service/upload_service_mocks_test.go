package service

import (
	"context"
	"io"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// cursorLess reports whether (ts1,id1) sorts before (ts2,id2) in the
// (created_at DESC, id DESC) order used by cursor pagination.
func cursorLess(ts1 time.Time, id1 int64, ts2 time.Time, id2 int64) bool {
	if ts1.Equal(ts2) {
		return id1 < id2
	}
	return ts1.Before(ts2)
}

// ---------------------------------------------------------------------------
// mockFileRepo
// ---------------------------------------------------------------------------

type mockFileRepo struct {
	files  map[int64]*filesqlc.File
	nextID int64
}

var _ filesrepo.FileRepository = (*mockFileRepo)(nil)

func newMockFileRepo() *mockFileRepo {
	return &mockFileRepo{files: make(map[int64]*filesqlc.File), nextID: 1}
}

func (m *mockFileRepo) Create(_ context.Context, params filesqlc.CreateFileParams) (*filesqlc.File, error) {
	f := &filesqlc.File{
		ID:           m.nextID,
		UserID:       params.UserID,
		OriginalName: params.OriginalName,
		StoragePath:  params.StoragePath,
		MimeType:     params.MimeType,
		Size:         params.Size,
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	m.files[m.nextID] = f
	m.nextID++
	return f, nil
}

func (m *mockFileRepo) GetByID(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *mockFileRepo) GetByIDIncludingDeleted(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *mockFileRepo) ListByUserID(_ context.Context, userID int64, limit, offset int32) ([]filesqlc.File, error) {
	var result []filesqlc.File
	for _, f := range m.files {
		if f.UserID == userID {
			result = append(result, *f)
		}
	}
	start := int(offset)
	if start > len(result) {
		return nil, nil
	}
	end := start + int(limit)
	if end > len(result) {
		end = len(result)
	}
	return result[start:end], nil
}

func (m *mockFileRepo) ListByUserCursor(_ context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]filesqlc.File, error) {
	return m.listCursor(&userID, cur, includeDeleted), nil
}

func (m *mockFileRepo) AdminListCursor(_ context.Context, cur pagination.DBCursor) ([]filesqlc.File, error) {
	return m.listCursor(nil, cur, true), nil
}

func (m *mockFileRepo) listCursor(userID *int64, cur pagination.DBCursor, includeDeleted bool) []filesqlc.File {
	all := make([]filesqlc.File, 0, len(m.files))
	for _, f := range m.files {
		if userID != nil && f.UserID != *userID {
			continue
		}
		if !includeDeleted && f.DeletedAt.Valid {
			continue
		}
		all = append(all, *f)
	}
	sort.Slice(all, func(i, j int) bool {
		return cursorLess(all[j].CreatedAt.Time, all[j].ID, all[i].CreatedAt.Time, all[i].ID)
	})
	out := make([]filesqlc.File, 0, len(all))
	for _, f := range all {
		if cur.HasCursor && !cursorLess(f.CreatedAt.Time, f.ID, cur.CreatedAt, cur.ID) {
			continue
		}
		out = append(out, f)
		if len(out) >= int(cur.Limit) {
			break
		}
	}
	return out
}

func (m *mockFileRepo) CountByUserID(_ context.Context, userID int64) (int64, error) {
	var count int64
	for _, f := range m.files {
		if f.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (m *mockFileRepo) Delete(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return f, nil
}

func (m *mockFileRepo) Restore(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{}
	return f, nil
}

func (m *mockFileRepo) AdminList(_ context.Context, limit, offset int32) ([]filesqlc.File, error) {
	all := make([]filesqlc.File, 0, len(m.files))
	for _, f := range m.files {
		all = append(all, *f)
	}
	start := int(offset)
	if start > len(all) {
		return nil, nil
	}
	end := start + int(limit)
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], nil
}

func (m *mockFileRepo) AdminCount(_ context.Context) (int64, error) {
	return int64(len(m.files)), nil
}

func (m *mockFileRepo) AdminFileStats(_ context.Context) (filesqlc.AdminFileStatsRow, error) {
	var stats filesqlc.AdminFileStatsRow
	for _, f := range m.files {
		if f.DeletedAt.Valid {
			continue
		}
		stats.TotalFiles++
		stats.TotalFileSize += f.Size
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// mockStorage
// ---------------------------------------------------------------------------

type mockStorage struct {
	files        map[string][]byte
	deletedPaths []string
	putErr       error
	getErr       error
	delErr       error
	baseURL      string
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
