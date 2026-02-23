package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type LocalStorage struct {
	basePath string
}

func NewLocalStorage(basePath string) (*LocalStorage, error) {
	abs, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("invalid local storage path: %w", err)
	}

	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	return &LocalStorage{basePath: abs}, nil
}

func (s *LocalStorage) safePath(path string) (string, error) {
	fullPath := filepath.Join(s.basePath, path)

	resolved, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		resolved, err = resolveExistingPrefix(fullPath)
		if err != nil {
			return "", fmt.Errorf("invalid path: %w", err)
		}
	}

	if !strings.HasPrefix(resolved, s.basePath+string(filepath.Separator)) && resolved != s.basePath {
		return "", fmt.Errorf("path traversal detected")
	}
	return resolved, nil
}

// resolveExistingPrefix resolves symlinks on the longest existing ancestor of
// the given path, then appends the remaining non-existent suffix. This prevents
// symlink bypass when intermediate directories are symlinks but the final target
// does not yet exist.
func resolveExistingPrefix(fullPath string) (string, error) {
	current := fullPath
	var suffix string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			abs, err := filepath.Abs(fullPath)
			if err != nil {
				return "", err
			}
			return abs, nil
		}
		if suffix == "" {
			suffix = filepath.Base(current)
		} else {
			suffix = filepath.Join(filepath.Base(current), suffix)
		}
		current = parent
	}
}

func (s *LocalStorage) Put(_ context.Context, path string, reader io.Reader, _ int64, _ string) error {
	fullPath, err := s.safePath(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	f, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, reader); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

func (s *LocalStorage) Get(_ context.Context, path string) (io.ReadCloser, error) {
	fullPath, err := s.safePath(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	return f, nil
}

func (s *LocalStorage) Delete(_ context.Context, path string) error {
	fullPath, err := s.safePath(path)
	if err != nil {
		return err
	}

	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	return nil
}

func (s *LocalStorage) URL(path string) string {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "/uploads/"
	}
	return "/uploads/" + path
}
