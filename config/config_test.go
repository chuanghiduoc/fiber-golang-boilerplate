package config

import (
	"strings"
	"testing"
)

func TestStorageConfig_AllowedTypes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"default types", "image/jpeg,image/png,image/gif,image/webp,application/pdf", 5},
		{"single type", "image/jpeg", 1},
		{"with spaces", " image/jpeg , image/png ", 2},
		{"empty", "", 0},
		{"empty commas", ",,", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := StorageConfig{AllowedMIMETypes: tt.input}
			got := sc.AllowedTypes()
			if len(got) != tt.want {
				t.Errorf("AllowedTypes() len = %d, want %d", len(got), tt.want)
			}
			// Verify no whitespace in results
			for _, v := range got {
				if v != strings.TrimSpace(v) {
					t.Errorf("AllowedTypes() contains untrimmed value: %q", v)
				}
			}
		})
	}
}

func TestCORSConfig_Origins(t *testing.T) {
	c := CORSConfig{AllowOrigins: "http://localhost:3000, https://example.com"}
	got := c.Origins()
	if len(got) != 2 {
		t.Errorf("Origins() len = %d, want 2", len(got))
	}
	if got[0] != "http://localhost:3000" {
		t.Errorf("Origins()[0] = %q, want http://localhost:3000", got[0])
	}
}

func TestCORSConfig_Methods(t *testing.T) {
	c := CORSConfig{AllowMethods: "GET,POST,PUT,DELETE,OPTIONS"}
	got := c.Methods()
	if len(got) != 5 {
		t.Errorf("Methods() len = %d, want 5", len(got))
	}
}

func TestCORSConfig_Headers(t *testing.T) {
	c := CORSConfig{AllowHeaders: "Origin,Content-Type,Accept,Authorization"}
	got := c.Headers()
	if len(got) != 4 {
		t.Errorf("Headers() len = %d, want 4", len(got))
	}
}

func TestDBConfig_DSN(t *testing.T) {
	db := DBConfig{
		Host:     "localhost",
		Port:     5432,
		Database: "testdb",
		Username: "user",
		Password: "pass",
		SSLMode:  "disable",
		Schema:   "public",
	}

	dsn := db.DSN()
	expected := "postgres://user:pass@localhost:5432/testdb?sslmode=disable&search_path=public"
	if dsn != expected {
		t.Errorf("DSN() = %q, want %q", dsn, expected)
	}
}

func TestValidate_ValidLocalConfig(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() returned error for valid config: %v", err)
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	cfg := &Config{
		App: AppConfig{Port: 0, Env: "local"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for port 0")
	}
}

func TestValidate_InsecureJWTSecret_Production(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "production", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for insecure JWT secret in production")
	}
}

func TestValidate_InvalidExpireHour(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 0},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for ExpireHour < 1")
	}
}

func TestValidate_InvalidBodyLimit(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 0},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for BodyLimit < 1")
	}
}

func TestValidate_InvalidRateLimit(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 0, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for StrictMax < 1")
	}
}

func TestValidate_InvalidRateLimitWindow(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 0, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for StrictWindow < 1")
	}
}

func TestValidate_InvalidMaxFileSize(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 0},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for MaxFileSize < 1")
	}
}

func TestValidate_GoogleOAuth_MissingSecret(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		OAuth:     OAuthConfig{GoogleClientID: "some-id"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail when GoogleClientID set without GoogleClientSecret")
	}
}

func TestValidate_S3Driver_MissingEndpoint(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "s3", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without endpoint")
	}
}

func TestValidate_LocalDriver_MissingPath(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "local", LocalPath: "", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for local driver without path")
	}
}

func TestLoad_DefaultValues(t *testing.T) {
	// Load with default env values (should work in test env)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
	if cfg.App.Port != 8080 {
		t.Errorf("default port = %d, want 8080", cfg.App.Port)
	}
}

func TestValidate_S3Driver_MissingAccessKey(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without access key")
	}
}

func TestValidate_S3Driver_MissingSecretKey(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", S3AccessKey: "key", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without secret key")
	}
}

func TestValidate_S3Driver_MissingBucket(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", S3AccessKey: "key", S3SecretKey: "secret", S3Bucket: "", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without bucket")
	}
}

func TestValidate_S3Driver_Success(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", S3AccessKey: "key", S3SecretKey: "secret", S3Bucket: "bucket", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() should pass for valid s3 config, got: %v", err)
	}
}

func TestValidate_HighPort(t *testing.T) {
	cfg := &Config{
		App: AppConfig{Port: 70000, Env: "local"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for port > 65535")
	}
}

func TestValidate_UnknownStorageDriver(t *testing.T) {
	cfg := &Config{
		App:       AppConfig{Port: 8080, Env: "local", BodyLimit: 4194304},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24},
		Storage:   StorageConfig{Driver: "gcs", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for unknown storage driver")
	}
}
