package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

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
	case "s3", "minio":
		return NewS3Storage(cfg)
	default:
		return nil, fmt.Errorf("unsupported storage driver: %s", cfg.Driver)
	}
}
