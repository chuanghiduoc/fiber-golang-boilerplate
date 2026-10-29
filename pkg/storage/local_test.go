package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

func TestNewLocalStorage(t *testing.T) {
	dir := tempDir(t)
	ls, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage failed: %v", err)
	}
	if ls == nil {
		t.Fatal("expected non-nil LocalStorage")
	}
}

func TestLocalStorage_PutGetDelete(t *testing.T) {
	dir := tempDir(t)
	ls, _ := NewLocalStorage(dir)
	ctx := context.Background()

	content := []byte("hello world")
	err := ls.Put(ctx, "test/file.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Get
	reader, err := ls.Get(ctx, "test/file.txt")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	data, _ := io.ReadAll(reader)
	_ = reader.Close()

	if !bytes.Equal(data, content) {
		t.Errorf("Get returned %q, want %q", data, content)
	}

	// Delete
	err = ls.Delete(ctx, "test/file.txt")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Get after delete should fail with the not-found sentinel.
	_, err = ls.Get(ctx, "test/file.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete error = %v, want ErrNotFound", err)
	}
}

func TestLocalStorage_DeleteNonexistent(t *testing.T) {
	dir := tempDir(t)
	ls, _ := NewLocalStorage(dir)

	err := ls.Delete(context.Background(), "nonexistent.txt")
	if err != nil {
		t.Errorf("Delete nonexistent should not error, got: %v", err)
	}
}

func TestLocalStorage_URL(t *testing.T) {
	dir := tempDir(t)
	ls, _ := NewLocalStorage(dir)

	tests := []struct {
		path string
		want string
	}{
		{"uploads/file.txt", "/uploads/uploads/file.txt"},
		{"file.txt", "/uploads/file.txt"},
	}

	for _, tt := range tests {
		got := ls.URL(tt.path)
		if got != tt.want {
			t.Errorf("URL(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestLocalStorage_URL_PathTraversal(t *testing.T) {
	dir := tempDir(t)
	ls, _ := NewLocalStorage(dir)

	tests := []string{"../etc/passwd", "../../secret", "."}
	for _, path := range tests {
		got := ls.URL(path)
		if got != "/uploads/" {
			t.Errorf("URL(%q) = %q, want /uploads/ for traversal attempt", path, got)
		}
	}
}

func TestLocalStorage_SafePath_PathTraversal(t *testing.T) {
	dir := tempDir(t)
	ls, _ := NewLocalStorage(dir)

	_, err := ls.safePath("../../etc/passwd")
	if err == nil {
		t.Error("safePath should reject path traversal")
	}
}

func TestLocalStorage_PutCreatesSubdirectories(t *testing.T) {
	dir := tempDir(t)
	ls, _ := NewLocalStorage(dir)

	content := []byte("nested")
	err := ls.Put(context.Background(), "a/b/c/file.txt", bytes.NewReader(content), int64(len(content)), "text/plain")
	if err != nil {
		t.Fatalf("Put with nested path failed: %v", err)
	}

	// Verify file exists
	fullPath := filepath.Join(dir, "a", "b", "c", "file.txt")
	if _, err := os.Stat(fullPath); err != nil {
		t.Errorf("file should exist at %s", fullPath)
	}
}

func TestLocalStorage_PutHonorsCanceledContext(t *testing.T) {
	dir := tempDir(t)
	ls, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	body := []byte("must not be written")
	err = ls.Put(ctx, "canceled/file.txt", bytes.NewReader(body), int64(len(body)), "text/plain")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put error = %v, want context.Canceled", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled Put left %d entries in storage: %v", len(entries), entries)
	}
}

func TestLocalStorage_List(t *testing.T) {
	dir := tempDir(t)
	ls, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	ctx := context.Background()
	for path, content := range map[string]string{
		"1/2/a.txt":        "a",
		"1/2/nested/b.txt": "bb",
		"12/3/c.txt":       "ccc",
	} {
		if err := ls.Put(ctx, path, bytes.NewReader([]byte(content)), int64(len(content)), "text/plain"); err != nil {
			t.Fatalf("Put(%s): %v", path, err)
		}
	}

	objects, err := ls.List(ctx, "1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]int64{}
	for _, object := range objects {
		got[object.Path] = object.Size
		if object.ModifiedAt.IsZero() {
			t.Errorf("object %q has no modification time", object.Path)
		}
	}
	want := map[string]int64{"1/2/a.txt": 1, "1/2/nested/b.txt": 2}
	if len(got) != len(want) || got["1/2/a.txt"] != want["1/2/a.txt"] || got["1/2/nested/b.txt"] != want["1/2/nested/b.txt"] {
		t.Fatalf("List(1/) = %v, want %v", got, want)
	}

	all, err := ls.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List(\"\") returned %d objects, want 3", len(all))
	}
	empty, err := ls.List(ctx, "9/")
	if err != nil {
		t.Fatalf("List of a missing prefix: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("List of a missing prefix returned %d objects, want 0", len(empty))
	}
	if _, err := ls.List(ctx, "../"); err == nil {
		t.Fatal("List must reject a traversal prefix")
	}
}
