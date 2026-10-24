package storage

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// ErrNotFound is returned (wrapped) by Get when no object exists at the path.
var ErrNotFound = errors.New("storage: object not found")

type Storage interface {
	Put(ctx context.Context, path string, reader io.Reader, size int64, contentType string) error
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Delete(ctx context.Context, path string) error
	URL(path string) string
}

func NewStorage(cfg config.StorageConfig) (Storage, error) {
	switch cfg.Driver {
	case "local":
		return NewLocalStorage(cfg.LocalPath)
	case DriverS3, DriverSeaweedFS:
		return NewS3Storage(cfg)
	default:
		return nil, fmt.Errorf("unsupported storage driver: %s", cfg.Driver)
	}
}
