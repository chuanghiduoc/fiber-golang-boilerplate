package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type fakePrivacyFiles struct {
	files   []File
	deleted []uuid.UUID
}

func (f *fakePrivacyFiles) ListForUser(context.Context, uuid.UUID, uuid.UUID) ([]File, error) {
	return f.files, nil
}

func (f *fakePrivacyFiles) HardDelete(_ context.Context, _, _, id uuid.UUID) error {
	f.deleted = append(f.deleted, id)
	for i, file := range f.files {
		if file.ID == id {
			f.files = append(f.files[:i], f.files[i+1:]...)
			break
		}
	}
	return nil
}

func TestFilesExporterIncludesMetadataAndContent(t *testing.T) {
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	path := "1/10/a.txt"
	require.NoError(t, store.Put(context.Background(), path, strings.NewReader("hello"), 5, "text/plain"))
	repo := &fakePrivacyFiles{files: []File{{
		ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "a.txt", StoragePath: path,
		MimeType: "text/plain", Size: 5, CreatedAt: time.Now(),
	}}}
	section, err := NewPrivacyExporter(repo, store).Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	assert.Equal(t, "files", section.Name)
	var out struct {
		Files []struct {
			Name    string `json:"name"`
			Content string `json:"contentBase64"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.Files, 1)
	assert.Equal(t, "a.txt", out.Files[0].Name)
	assert.Equal(t, "hello", strings.TrimSpace(string(mustDecode(t, out.Files[0].Content))))
}

func TestFilesEraserDeletesBlobThenRowIdempotently(t *testing.T) {
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	path := "1/10/a.txt"
	require.NoError(t, store.Put(context.Background(), path, strings.NewReader("hello"), 5, "text/plain"))
	repo := &fakePrivacyFiles{files: []File{{ID: testID(1), TenantID: testID(1), UserID: testID(10), StoragePath: path}}}
	eraser := NewPrivacyEraser(repo, store)
	require.NoError(t, eraser.Erase(context.Background(), testID(1), testID(10)))
	assert.Equal(t, []uuid.UUID{testID(1)}, repo.deleted)
	_, err = store.Get(context.Background(), path)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestFilesEraserKeepsRowWhenBlobDeleteFails(t *testing.T) {
	repo := &fakePrivacyFiles{files: []File{{ID: testID(1), TenantID: testID(1), UserID: testID(10), StoragePath: "1/10/a.txt"}}}
	eraser := NewPrivacyEraser(repo, failingDeleteStorage{})
	err := eraser.Erase(context.Background(), testID(1), testID(10))
	assert.Error(t, err)
	assert.Empty(t, repo.deleted)
}

type failingDeleteStorage struct{}

func (failingDeleteStorage) Put(context.Context, string, io.Reader, int64, string) error {
	return nil
}
func (failingDeleteStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, storage.ErrNotFound
}
func (failingDeleteStorage) Delete(context.Context, string) error { return fmt.Errorf("disk full") }
func (failingDeleteStorage) List(context.Context, string) ([]storage.Object, error) {
	return nil, nil
}
func (failingDeleteStorage) URL(string) string { return "" }

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err)
	return b
}

func TestFilesExporterOmitsContentPastTheInlineBudget(t *testing.T) {
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()
	repo := &fakePrivacyFiles{}
	for i, body := range []string{"12345", "678", "9"} {
		path := fmt.Sprintf("1/10/%d.txt", i)
		require.NoError(t, store.Put(ctx, path, strings.NewReader(body), int64(len(body)), "text/plain"))
		// Size is understated on purpose: the budget must count real bytes.
		repo.files = append(repo.files, File{ID: testID(i + 1), TenantID: testID(1), UserID: testID(10), StoragePath: path, Size: 1})
	}
	exporter := NewPrivacyExporter(repo, store)
	exporter.inlineLimit = 6

	section, err := exporter.Export(ctx, testID(1), testID(10))
	require.NoError(t, err)
	var out struct {
		Files []struct {
			Content string `json:"contentBase64"`
			Omitted bool   `json:"contentOmitted"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.Files, 3)
	assert.Equal(t, "12345", string(mustDecode(t, out.Files[0].Content)))
	assert.True(t, out.Files[1].Omitted, "5+3 bytes exceed the 6-byte budget")
	assert.Equal(t, "9", string(mustDecode(t, out.Files[2].Content)), "a later file that still fits is embedded")
}
