// Package drivers selects a storage.Storage implementation from config. It is
// the only package that links every driver; modules depend on pkg/storage.
package drivers

import (
	"fmt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/storage/s3store"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// New builds the storage driver named by cfg.Driver.
func New(cfg config.StorageConfig) (storage.Storage, error) {
	switch cfg.Driver {
	case "local":
		return storage.NewLocalStorage(cfg.LocalPath)
	case s3store.DriverS3, s3store.DriverSeaweedFS:
		return s3store.New(cfg)
	default:
		return nil, fmt.Errorf("unsupported storage driver: %s", cfg.Driver)
	}
}
