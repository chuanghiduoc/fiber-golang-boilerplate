package config

import "fmt"

type StorageConfig struct {
	Driver           string   `env:"STORAGE_DRIVER" envDefault:"local"`
	LocalPath        string   `env:"STORAGE_LOCAL_PATH" envDefault:"./uploads"`
	MaxFileSize      int64    `env:"STORAGE_MAX_FILE_SIZE" envDefault:"10485760"` // 10MB
	AllowedMIMETypes []string `env:"STORAGE_ALLOWED_MIME_TYPES" envSeparator:"," envDefault:"image/jpeg,image/png,image/gif,image/webp,application/pdf"`
	S3Endpoint       string   `env:"STORAGE_S3_ENDPOINT"`
	S3Region         string   `env:"STORAGE_S3_REGION" envDefault:"us-east-1"`
	S3Bucket         string   `env:"STORAGE_S3_BUCKET" envDefault:"uploads"`
	S3AccessKey      string   `env:"STORAGE_S3_ACCESS_KEY"`
	S3SecretKey      string   `env:"STORAGE_S3_SECRET_KEY"`
	S3UseSSL         bool     `env:"STORAGE_S3_USE_SSL" envDefault:"false"`
	// S3AllowInsecure acknowledges plaintext object-storage traffic in
	// production (for example a cluster-local SeaweedFS behind a
	// NetworkPolicy). Without it, production requires STORAGE_S3_USE_SSL=true.
	S3AllowInsecure bool `env:"STORAGE_S3_ALLOW_INSECURE" envDefault:"false"`
}

// AllowedTypes returns the list of allowed MIME types for uploads.
func (s StorageConfig) AllowedTypes() []string {
	return trimmedList(s.AllowedMIMETypes)
}

// UsesS3 reports whether the driver talks to an S3-compatible object store.
func (s StorageConfig) UsesS3() bool {
	return s.Driver == "s3" || s.Driver == "seaweedfs"
}

// validateMaxFileSize rejects a non-positive upload limit.
func (s StorageConfig) validateMaxFileSize() error {
	if s.MaxFileSize < 1 {
		return fmt.Errorf("STORAGE_MAX_FILE_SIZE must be at least 1 byte")
	}
	return nil
}

// validateDriver checks that the selected driver has its required settings.
func (s StorageConfig) validateDriver() error {
	switch s.Driver {
	case "local":
		if s.LocalPath == "" {
			return fmt.Errorf("STORAGE_LOCAL_PATH is required for local driver")
		}
		return nil
	case "s3", "seaweedfs":
		return s.validateS3()
	default:
		return fmt.Errorf("STORAGE_DRIVER must be one of: local, s3, seaweedfs (got %q)", s.Driver)
	}
}

func (s StorageConfig) validateS3() error {
	required := []struct{ name, value string }{
		{"STORAGE_S3_ENDPOINT", s.S3Endpoint},
		{"STORAGE_S3_ACCESS_KEY", s.S3AccessKey},
		{"STORAGE_S3_SECRET_KEY", s.S3SecretKey},
		{"STORAGE_S3_BUCKET", s.S3Bucket},
	}
	for _, field := range required {
		if field.value == "" {
			return fmt.Errorf("%s is required for %s driver", field.name, s.Driver)
		}
	}
	return nil
}

// validateProductionMIMETypes requires an explicit upload allowlist.
func (s StorageConfig) validateProductionMIMETypes() error {
	if len(s.AllowedTypes()) == 0 {
		return fmt.Errorf("STORAGE_ALLOWED_MIME_TYPES must not be empty in production")
	}
	return nil
}

// validateProductionTransport requires TLS to object storage, which carries
// user uploads and credentials. Storage reachable only over a trusted private
// network must opt out explicitly.
func (s StorageConfig) validateProductionTransport() error {
	if s.UsesS3() && !s.S3UseSSL && !s.S3AllowInsecure {
		return fmt.Errorf("STORAGE_S3_USE_SSL must be true in production (set STORAGE_S3_ALLOW_INSECURE=true only for storage on a trusted private network)")
	}
	return nil
}
