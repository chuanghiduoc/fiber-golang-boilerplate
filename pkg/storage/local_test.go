package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
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

	// Get after delete should fail
	_, err = ls.Get(ctx, "test/file.txt")
	if err == nil {
		t.Error("Get after Delete should fail")
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

func TestNewStorage_Local(t *testing.T) {
	dir := tempDir(t)
	s, err := NewStorage(config.StorageConfig{Driver: "local", LocalPath: dir})
	if err != nil {
		t.Fatalf("NewStorage(local) failed: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Storage")
	}
}

func TestNewStorage_Unsupported(t *testing.T) {
	_, err := NewStorage(config.StorageConfig{Driver: "gcs"})
	if err == nil {
		t.Error("NewStorage should fail for unsupported driver")
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
