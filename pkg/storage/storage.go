// Package storage defines the object-storage interface and the local
// filesystem driver. The S3 driver lives in pkg/storage/s3store and the
// config-driven selection in pkg/storage/drivers, so packages that only need
// the interface do not link the AWS SDK.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned (wrapped) by Get when no object exists at the path.
var ErrNotFound = errors.New("storage: object not found")

// Object is one stored blob returned by List.
type Object struct {
	// Path is the storage key, using forward slashes on every driver.
	Path string
	Size int64
	// ModifiedAt is the object's last write time, used by the orphan sweep.
	ModifiedAt time.Time
}

type Storage interface {
	Put(ctx context.Context, path string, reader io.Reader, size int64, contentType string) error
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
	// List returns every object whose key starts with prefix, recursively.
	// An empty prefix lists the whole store.
	List(ctx context.Context, prefix string) ([]Object, error)
	URL(path string) string
}
