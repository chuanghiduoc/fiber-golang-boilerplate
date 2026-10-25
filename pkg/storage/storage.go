// Package storage defines the object-storage interface and the local
// filesystem driver. The S3 driver lives in pkg/storage/s3store and the
// config-driven selection in pkg/storage/drivers, so packages that only need
// the interface do not link the AWS SDK.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned (wrapped) by Get when no object exists at the path.
var ErrNotFound = errors.New("storage: object not found")

type Storage interface {
	Put(ctx context.Context, path string, reader io.Reader, size int64, contentType string) error
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
	URL(path string) string
}
