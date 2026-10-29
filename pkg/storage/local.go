package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type LocalStorage struct {
	basePath string
}

const (
	copyBufferSize = 32 << 10
	// maxEmptyReads matches io.Copy's tolerance for a Reader that returns
	// (0, nil) repeatedly. Without it, a broken upload stream spins forever.
	maxEmptyReads = 100
)

func NewLocalStorage(basePath string) (*LocalStorage, error) {
	abs, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("invalid local storage path: %w", err)
	}

	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	// Canonicalize the base path (resolve symlinks / short names) so the prefix
	// check in safePath compares like-for-like. Without this, a base dir that is
	// itself a symlink (macOS /var, Windows 8.3 temp dirs) makes every safePath
	// call falsely report path traversal.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
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

func (s *LocalStorage) Put(ctx context.Context, path string, reader io.Reader, size int64, _ string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fullPath, err := s.safePath(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write to a temporary file and rename only after the full content has
	// been flushed. A failed/truncated upload must never leave a partial blob
	// at the final path.
	f, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	tmpName := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpName)
	}

	written, err := copyWithContext(ctx, f, reader)
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to write file: %w", err)
	}
	if size >= 0 && written != size {
		cleanup()
		return fmt.Errorf("file size mismatch: expected %d bytes, wrote %d", size, written)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("failed to flush file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to write file: %w", err)
	}
	if err := os.Rename(tmpName, fullPath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to commit file: %w", err)
	}

	return nil
}

// copyWithContext is io.Copy with cancellation between reads. io.Copy alone
// cannot be interrupted, so a canceled upload would keep consuming disk and
// delaying handler cleanup until the source reached EOF.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, copyBufferSize)
	var written int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			emptyReads = 0
			w, writeErr := dst.Write(buf[:n])
			written += int64(w)
			if writeErr != nil {
				return written, writeErr
			}
			if w != n {
				return written, io.ErrShortWrite
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= maxEmptyReads {
				return written, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func (s *LocalStorage) Get(_ context.Context, path string) (io.ReadCloser, error) {
	fullPath, err := s.safePath(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(fullPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
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

// List walks prefix recursively and returns every regular file below it. Paths
// are returned relative to the storage root with forward slashes, matching the
// keys used by Put/Get.
func (s *LocalStorage) List(ctx context.Context, prefix string) ([]Object, error) {
	fullPath, err := s.safePath(prefix)
	if err != nil {
		return nil, err
	}
	var objects []Object
	err = filepath.WalkDir(fullPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				// Like the S3 driver, a prefix with no objects is empty, not an
				// error.
				return fs.SkipAll
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(s.basePath, path)
		if err != nil {
			return err
		}
		objects = append(objects, Object{
			Path:       filepath.ToSlash(relative),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list storage: %w", err)
	}
	return objects, nil
}

func (s *LocalStorage) URL(path string) string {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "/uploads/"
	}
	return "/uploads/" + path
}
