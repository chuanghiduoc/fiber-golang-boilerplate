package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

type Config struct {
	App       AppConfig
	DB        DBConfig
	JWT       JWTConfig
	Storage   StorageConfig
	OAuth     OAuthConfig
	CORS      CORSConfig
	RateLimit RateLimitConfig
	Cache     CacheConfig
	Email     EmailConfig
	Admin     AdminConfig
	Tenant    TenantConfig
	Webhook   WebhookConfig
}

// WebhookConfig controls outbound delivery, inbound verification, and secret
// encryption at rest. The signing key is sealed with a key derived from
// SecretEncryptionKey; when that is empty the JWT secret is used as a
// compatibility fallback (never in production).
type WebhookConfig struct {
	SecretEncryptionKey       string `env:"WEBHOOK_SECRET_ENCRYPTION_KEY"`
	ConnectTimeoutSeconds     int    `env:"WEBHOOK_CONNECT_TIMEOUT_SECS" envDefault:"5"`
	RequestTimeoutSeconds     int    `env:"WEBHOOK_REQUEST_TIMEOUT_SECS" envDefault:"10"`
	RetryBaseSeconds          int    `env:"WEBHOOK_RETRY_BASE_SECS" envDefault:"5"`
	RetryMaxSeconds           int    `env:"WEBHOOK_RETRY_MAX_SECS" envDefault:"3600"`
	MaxAttempts               int    `env:"WEBHOOK_MAX_ATTEMPTS" envDefault:"8"`
	RetentionDays             int    `env:"WEBHOOK_RETENTION_DAYS" envDefault:"30"`
	TimestampToleranceSeconds int    `env:"WEBHOOK_TIMESTAMP_TOLERANCE_SECS" envDefault:"300"`
	// SSRFAllowlist permits private/loopback targets for trusted receivers.
	// Entries are comma-separated IPs, CIDRs, or exact hostnames. Empty keeps
	// the default deny for every non-public address.
	SSRFAllowlist string `env:"WEBHOOK_SSRF_ALLOWLIST"`
}

// ResolvedSecretKey returns the dedicated webhook encryption key, or the JWT
// secret as the documented compatibility fallback.
func (w WebhookConfig) ResolvedSecretKey(jwtSecret string) string {
	if key := strings.TrimSpace(w.SecretEncryptionKey); key != "" {
		return key
	}
	return jwtSecret
}

// SSRFAllowlistEntries returns the parsed allowlist entries.
func (w WebhookConfig) SSRFAllowlistEntries() []string {
	return splitCSV(w.SSRFAllowlist)
}

// RequestTimeout returns the per-delivery HTTP timeout.
func (w WebhookConfig) RequestTimeout() time.Duration {
	return time.Duration(w.RequestTimeoutSeconds) * time.Second
}

// ConnectTimeout returns the TCP/TLS connect timeout.
func (w WebhookConfig) ConnectTimeout() time.Duration {
	return time.Duration(w.ConnectTimeoutSeconds) * time.Second
}

// RetryBase returns the first retry backoff delay.
func (w WebhookConfig) RetryBase() time.Duration {
	return time.Duration(w.RetryBaseSeconds) * time.Second
}

// RetryMax returns the maximum retry backoff delay.
func (w WebhookConfig) RetryMax() time.Duration {
	return time.Duration(w.RetryMaxSeconds) * time.Second
}

// Retention returns how long delivery and inbound-event history is retained.
func (w WebhookConfig) Retention() time.Duration {
	return time.Duration(w.RetentionDays) * 24 * time.Hour
}

// TimestampTolerance returns the accepted inbound signature clock skew.
func (w WebhookConfig) TimestampTolerance() time.Duration {
	return time.Duration(w.TimestampToleranceSeconds) * time.Second
}

// TenantConfig controls request-tenant resolution. The default slug preserves
// compatibility for requests that do not send X-Tenant-Slug.
type TenantConfig struct {
	DefaultSlug   string `env:"TENANT_DEFAULT_SLUG" envDefault:"default"`
	RequireHeader bool   `env:"TENANT_REQUIRE_HEADER" envDefault:"false"`
}

var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

type AdminConfig struct {
	Email    string `env:"ADMIN_EMAIL"`
	Password string `env:"ADMIN_PASSWORD"`
	Name     string `env:"ADMIN_NAME" envDefault:"Admin"`
}

type AppConfig struct {
	Port int    `env:"APP_PORT" envDefault:"8080"`
	Env  string `env:"APP_ENV" envDefault:"local"`
	// BodyLimit must be larger than STORAGE_MAX_FILE_SIZE because multipart
	// framing adds overhead before the upload handler sees the file.
	BodyLimit                int    `env:"APP_BODY_LIMIT" envDefault:"12582912"` // 12MB
	LogLevel                 string `env:"LOG_LEVEL" envDefault:"info"`
	RequestTimeout           int    `env:"APP_REQUEST_TIMEOUT" envDefault:"30"` // seconds
	FrontendURL              string `env:"APP_FRONTEND_URL" envDefault:"http://localhost:3000"`
	RequireEmailVerification bool   `env:"REQUIRE_EMAIL_VERIFICATION" envDefault:"false"`
	// TrustProxy enables reading the client IP from ProxyHeader only when the
	// direct peer is in TrustedProxies. Required for correct rate limiting
	// behind a reverse proxy; leaving it off prevents X-Forwarded-For spoofing.
	TrustProxy     bool   `env:"APP_TRUST_PROXY" envDefault:"false"`
	TrustedProxies string `env:"APP_TRUSTED_PROXIES"` // comma-separated IPs or CIDR ranges
	// MetricsAuthToken, when set, requires a matching Bearer token to access
	// the /metrics endpoint. Leave empty to keep /metrics open (local/dev only).
	MetricsAuthToken string `env:"METRICS_AUTH_TOKEN"`
	// ErrorDocsBaseURL is prepended to the RFC 9457 problem "type" URI, e.g.
	// "https://docs.example.com" yields ".../errors/not-found". Empty = relative.
	ErrorDocsBaseURL string `env:"ERROR_DOCS_BASE_URL"`
}

// TrustedProxiesList returns the parsed list of trusted proxy IPs/CIDRs.
func (a AppConfig) TrustedProxiesList() []string {
	return splitCSV(a.TrustedProxies)
}

type CORSConfig struct {
	AllowOrigins     string `env:"CORS_ALLOW_ORIGINS" envDefault:"*"`
	AllowMethods     string `env:"CORS_ALLOW_METHODS" envDefault:"GET,POST,PUT,DELETE,OPTIONS"`
	AllowHeaders     string `env:"CORS_ALLOW_HEADERS" envDefault:"Origin,Content-Type,Accept,Authorization,Idempotency-Key,X-Tenant-Slug"`
	ExposeHeaders    string `env:"CORS_EXPOSE_HEADERS" envDefault:"Location,ETag,Idempotent-Replayed"`
	AllowCredentials bool   `env:"CORS_ALLOW_CREDENTIALS" envDefault:"false"`
}

type RateLimitConfig struct {
	StrictMax     int `env:"RATE_LIMIT_STRICT_MAX" envDefault:"5"`
	StrictWindow  int `env:"RATE_LIMIT_STRICT_WINDOW_SECS" envDefault:"60"`
	NormalMax     int `env:"RATE_LIMIT_NORMAL_MAX" envDefault:"60"`
	NormalWindow  int `env:"RATE_LIMIT_NORMAL_WINDOW_SECS" envDefault:"60"`
	RelaxedMax    int `env:"RATE_LIMIT_RELAXED_MAX" envDefault:"120"`
	RelaxedWindow int `env:"RATE_LIMIT_RELAXED_WINDOW_SECS" envDefault:"60"`
}

type DBConfig struct {
	Host            string `env:"DB_HOST" envDefault:"localhost"`
	Port            int    `env:"DB_PORT" envDefault:"5432"`
	Database        string `env:"DB_DATABASE" envDefault:"boilerplate"`
	Username        string `env:"DB_USERNAME" envDefault:"postgres"`
	Password        string `env:"DB_PASSWORD" envDefault:"postgres"`
	Schema          string `env:"DB_SCHEMA" envDefault:"public"`
	SSLMode         string `env:"DB_SSLMODE" envDefault:"disable"`
	ConnectTimeout  int    `env:"DB_CONNECT_TIMEOUT" envDefault:"10"` // seconds
	MaxConns        int32  `env:"DB_MAX_CONNS" envDefault:"25"`
	MinConns        int32  `env:"DB_MIN_CONNS" envDefault:"5"`
	MaxConnLifetime int    `env:"DB_MAX_CONN_LIFETIME" envDefault:"3600"` // seconds
	MaxConnIdleTime int    `env:"DB_MAX_CONN_IDLE_TIME" envDefault:"300"` // seconds
}

type JWTConfig struct {
	Secret            string `env:"JWT_SECRET" envDefault:"secret"`
	ExpireHour        int    `env:"JWT_EXPIRE_HOUR" envDefault:"24"`
	RefreshExpireDays int    `env:"JWT_REFRESH_EXPIRE_DAYS" envDefault:"30"`
}

type CacheConfig struct {
	Driver   string `env:"CACHE_DRIVER" envDefault:"memory"`
	RedisURL string `env:"REDIS_URL"`
}

type EmailConfig struct {
	Driver       string `env:"EMAIL_DRIVER" envDefault:"console"`
	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     int    `env:"SMTP_PORT" envDefault:"587"`
	SMTPUsername string `env:"SMTP_USERNAME"`
	SMTPPassword string `env:"SMTP_PASSWORD"`
	FromAddress  string `env:"EMAIL_FROM_ADDRESS" envDefault:"noreply@localhost"`
	FromName     string `env:"EMAIL_FROM_NAME" envDefault:"Fiber App"`
	// SMTPRequireTLS fails the send when the server does not offer STARTTLS,
	// preventing a downgrade that would send message content (reset/verification
	// links) in cleartext. Leave false for local relays without TLS.
	SMTPRequireTLS bool `env:"SMTP_REQUIRE_TLS" envDefault:"false"`
}

type StorageConfig struct {
	Driver           string `env:"STORAGE_DRIVER" envDefault:"local"`
	LocalPath        string `env:"STORAGE_LOCAL_PATH" envDefault:"./uploads"`
	MaxFileSize      int64  `env:"STORAGE_MAX_FILE_SIZE" envDefault:"10485760"` // 10MB
	AllowedMIMETypes string `env:"STORAGE_ALLOWED_MIME_TYPES" envDefault:"image/jpeg,image/png,image/gif,image/webp,application/pdf"`
	S3Endpoint       string `env:"STORAGE_S3_ENDPOINT"`
	S3Region         string `env:"STORAGE_S3_REGION" envDefault:"us-east-1"`
	S3Bucket         string `env:"STORAGE_S3_BUCKET" envDefault:"uploads"`
	S3AccessKey      string `env:"STORAGE_S3_ACCESS_KEY"`
	S3SecretKey      string `env:"STORAGE_S3_SECRET_KEY"`
	S3UseSSL         bool   `env:"STORAGE_S3_USE_SSL" envDefault:"false"`
}

// splitCSV splits a comma-separated string into trimmed, non-empty values.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// AllowedTypes returns the list of allowed MIME types for uploads.
func (s StorageConfig) AllowedTypes() []string {
	return splitCSV(s.AllowedMIMETypes)
}

type OAuthConfig struct {
	GoogleClientID     string `env:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `env:"GOOGLE_CLIENT_SECRET"`
	GoogleRedirectURL  string `env:"GOOGLE_REDIRECT_URL" envDefault:"http://localhost:8080/api/v1/auth/google/callback"`
	FrontendURL        string `env:"OAUTH_FRONTEND_URL" envDefault:"http://localhost:3000/auth/callback"`
}

// Origins returns the list of allowed CORS origins.
func (c CORSConfig) Origins() []string {
	return splitCSV(c.AllowOrigins)
}

// Methods returns the list of allowed CORS methods.
func (c CORSConfig) Methods() []string {
	return splitCSV(c.AllowMethods)
}

// Headers returns the list of allowed CORS request headers.
func (c CORSConfig) Headers() []string {
	return splitCSV(c.AllowHeaders)
}

// ExposedHeaders returns the response headers browser clients may read.
func (c CORSConfig) ExposedHeaders() []string {
	return splitCSV(c.ExposeHeaders)
}

// Validate checks the database connection and pool settings.
func (db DBConfig) Validate() error {
	if db.ConnectTimeout < 1 {
		return fmt.Errorf("DB_CONNECT_TIMEOUT must be at least 1 second")
	}
	if db.MaxConns < 1 {
		return fmt.Errorf("DB_MAX_CONNS must be at least 1")
	}
	if db.MinConns < 0 {
		return fmt.Errorf("DB_MIN_CONNS must not be negative")
	}
	if db.MinConns > db.MaxConns {
		return fmt.Errorf("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", db.MinConns, db.MaxConns)
	}
	return nil
}

func (db DBConfig) DSN() string {
	q := url.Values{}
	q.Set("sslmode", db.SSLMode)
	q.Set("search_path", db.Schema)

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(db.Username, db.Password),
		Host:     net.JoinHostPort(db.Host, strconv.Itoa(db.Port)),
		Path:     "/" + db.Database,
		RawQuery: q.Encode(),
	}
	return u.String()
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// LoadDatabase loads and validates only the database settings. Schema tooling
// (cmd/migrate) must be able to run without application secrets such as JWT
// keys, metrics tokens, or SMTP credentials, so it deliberately skips the
// application-wide validation performed by Load.
func LoadDatabase() (DBConfig, error) {
	var parsed struct {
		App struct {
			Env string `env:"APP_ENV" envDefault:"local"`
		}
		DB DBConfig
	}
	if err := env.Parse(&parsed); err != nil {
		return DBConfig{}, fmt.Errorf("failed to parse database config: %w", err)
	}
	if err := parsed.DB.Validate(); err != nil {
		return DBConfig{}, fmt.Errorf("invalid database config: %w", err)
	}
	appEnv := strings.ToLower(strings.TrimSpace(parsed.App.Env))
	switch appEnv {
	case "local", "test", "development", "staging", "production":
	default:
		return DBConfig{}, fmt.Errorf("APP_ENV must be one of: local, test, development, staging, production (got %q)", parsed.App.Env)
	}
	// Migration jobs skip the full application validation, but must not
	// silently downgrade transport security relative to the API. allow/prefer
	// permit a plaintext fallback, so they are not acceptable in production.
	if appEnv == "production" {
		switch strings.ToLower(parsed.DB.SSLMode) {
		case "disable", "allow", "prefer":
			return DBConfig{}, fmt.Errorf("DB_SSLMODE=%s permits plaintext in production; use require or verify-full", parsed.DB.SSLMode)
		}
	}
	return parsed.DB, nil
}

func (cfg *Config) Validate() error {
	if cfg.App.Port < 1 || cfg.App.Port > 65535 {
		return fmt.Errorf("APP_PORT must be between 1 and 65535")
	}
	switch cfg.App.Env {
	case "local", "test", "development", "staging", "production":
	default:
		return fmt.Errorf("APP_ENV must be one of: local, test, development, staging, production (got %q)", cfg.App.Env)
	}
	if cfg.App.RequestTimeout < 1 {
		return fmt.Errorf("APP_REQUEST_TIMEOUT must be at least 1 second")
	}
	if cfg.App.FrontendURL != "" {
		u, err := url.Parse(cfg.App.FrontendURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("APP_FRONTEND_URL must be an absolute http(s) URL")
		}
	}

	// JWT/admin validation for every non-local environment, including staging
	// and development. The sample secret is public and must never sign tokens
	// outside local/test.
	if cfg.App.Env != "local" && cfg.App.Env != "test" {
		if cfg.JWT.Secret == "" || cfg.JWT.Secret == "secret" ||
			cfg.JWT.Secret == "your-super-secret-key-change-in-production" {
			return fmt.Errorf("JWT_SECRET must be set to a secure value in %s environment", cfg.App.Env)
		}
		if len(cfg.JWT.Secret) < 32 {
			return fmt.Errorf("JWT_SECRET must be at least 32 characters for security")
		}
		if cfg.Admin.Email != "" || cfg.Admin.Password != "" {
			if cfg.Admin.Email == "" || cfg.Admin.Password == "" {
				return fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be set together")
			}
			if len(cfg.Admin.Password) < 12 || cfg.Admin.Password == "Admin123!" {
				return fmt.Errorf("ADMIN_PASSWORD is too weak for %s environment", cfg.App.Env)
			}
		}
	}
	if cfg.JWT.ExpireHour < 1 {
		return fmt.Errorf("JWT_EXPIRE_HOUR must be at least 1")
	}
	if cfg.JWT.RefreshExpireDays < 1 {
		return fmt.Errorf("JWT_REFRESH_EXPIRE_DAYS must be at least 1")
	}

	// Tenant slugs are used as external identifiers and must stay DNS-safe.
	if !tenantSlugPattern.MatchString(cfg.Tenant.DefaultSlug) {
		return fmt.Errorf("TENANT_DEFAULT_SLUG must match [a-z0-9-]{1,63} (got %q)", cfg.Tenant.DefaultSlug)
	}

	// Webhook delivery bounds and secret encryption. The dedicated key must be
	// strong and is mandatory in production; outside production the JWT secret
	// is the documented fallback so local installs work without extra setup.
	wh := cfg.Webhook
	if wh.ConnectTimeoutSeconds < 1 {
		return fmt.Errorf("WEBHOOK_CONNECT_TIMEOUT_SECS must be at least 1")
	}
	if wh.RequestTimeoutSeconds < 1 {
		return fmt.Errorf("WEBHOOK_REQUEST_TIMEOUT_SECS must be at least 1")
	}
	if wh.RequestTimeoutSeconds > 120 {
		return fmt.Errorf("WEBHOOK_REQUEST_TIMEOUT_SECS must be at most 120")
	}
	if wh.ConnectTimeoutSeconds > wh.RequestTimeoutSeconds {
		return fmt.Errorf("WEBHOOK_CONNECT_TIMEOUT_SECS must not exceed WEBHOOK_REQUEST_TIMEOUT_SECS")
	}
	if wh.RetryBaseSeconds < 1 {
		return fmt.Errorf("WEBHOOK_RETRY_BASE_SECS must be at least 1")
	}
	if wh.RetryMaxSeconds < wh.RetryBaseSeconds {
		return fmt.Errorf("WEBHOOK_RETRY_MAX_SECS must be at least WEBHOOK_RETRY_BASE_SECS")
	}
	if wh.RetryMaxSeconds > 604800 {
		return fmt.Errorf("WEBHOOK_RETRY_MAX_SECS must be at most 604800")
	}
	if wh.MaxAttempts < 1 || wh.MaxAttempts > 50 {
		return fmt.Errorf("WEBHOOK_MAX_ATTEMPTS must be between 1 and 50")
	}
	if wh.RetentionDays < 1 || wh.RetentionDays > 3650 {
		return fmt.Errorf("WEBHOOK_RETENTION_DAYS must be between 1 and 3650")
	}
	if wh.TimestampToleranceSeconds < 30 || wh.TimestampToleranceSeconds > 3600 {
		return fmt.Errorf("WEBHOOK_TIMESTAMP_TOLERANCE_SECS must be between 30 and 3600")
	}
	if key := strings.TrimSpace(wh.SecretEncryptionKey); key != "" && len(key) < 32 {
		return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_KEY must be at least 32 characters")
	}
	if cfg.App.Env == "production" && strings.TrimSpace(wh.SecretEncryptionKey) == "" {
		return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_KEY must be set in production")
	}
	if strings.TrimSpace(wh.ResolvedSecretKey(cfg.JWT.Secret)) == "" {
		return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_KEY or JWT_SECRET must be set")
	}
	if _, err := webhook.NewGuard(wh.SSRFAllowlistEntries()); err != nil {
		return fmt.Errorf("invalid WEBHOOK_SSRF_ALLOWLIST: %w", err)
	}

	if cfg.App.BodyLimit < 1 {
		return fmt.Errorf("APP_BODY_LIMIT must be at least 1 byte")
	}
	if cfg.RateLimit.StrictMax < 1 || cfg.RateLimit.NormalMax < 1 || cfg.RateLimit.RelaxedMax < 1 {
		return fmt.Errorf("all RATE_LIMIT_*_MAX values must be at least 1")
	}
	if cfg.RateLimit.StrictWindow < 1 || cfg.RateLimit.NormalWindow < 1 || cfg.RateLimit.RelaxedWindow < 1 {
		return fmt.Errorf("all RATE_LIMIT_*_WINDOW_SECS values must be at least 1")
	}
	if cfg.Storage.MaxFileSize < 1 {
		return fmt.Errorf("STORAGE_MAX_FILE_SIZE must be at least 1 byte")
	}

	// Database pool validation
	if err := cfg.DB.Validate(); err != nil {
		return err
	}

	// Cache config validation
	switch cfg.Cache.Driver {
	case "memory":
	case "redis":
		if cfg.Cache.RedisURL == "" {
			return fmt.Errorf("REDIS_URL is required when CACHE_DRIVER=redis")
		}
	default:
		return fmt.Errorf("CACHE_DRIVER must be one of: memory, redis (got %q)", cfg.Cache.Driver)
	}

	// Email config validation
	switch cfg.Email.Driver {
	case "console":
	case "smtp":
		if cfg.Email.SMTPHost == "" {
			return fmt.Errorf("SMTP_HOST is required when EMAIL_DRIVER=smtp")
		}
		if cfg.Email.SMTPPort < 1 || cfg.Email.SMTPPort > 65535 {
			return fmt.Errorf("SMTP_PORT must be between 1 and 65535")
		}
	default:
		return fmt.Errorf("EMAIL_DRIVER must be one of: console, smtp (got %q)", cfg.Email.Driver)
	}

	// CORS validation. Fiber treats an empty origin list as allow-all and can
	// panic on malformed origins, so validate explicitly.
	origins := cfg.CORS.Origins()
	if len(origins) == 0 {
		return fmt.Errorf("CORS_ALLOW_ORIGINS must not be empty")
	}
	for _, origin := range origins {
		if origin == "*" {
			if cfg.CORS.AllowCredentials {
				return fmt.Errorf("CORS_ALLOW_CREDENTIALS cannot be true when CORS_ALLOW_ORIGINS contains '*'")
			}
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("CORS_ALLOW_ORIGINS contains invalid origin %q", origin)
		}
	}

	// Trusting proxies without an allowlist would let any client spoof
	// X-Forwarded-For; require explicit proxy IPs/CIDRs.
	if cfg.App.TrustProxy && len(cfg.App.TrustedProxiesList()) == 0 {
		return fmt.Errorf("APP_TRUSTED_PROXIES must list at least one IP/CIDR when APP_TRUST_PROXY=true")
	}
	if cfg.App.TrustProxy {
		for _, proxy := range cfg.App.TrustedProxiesList() {
			if proxy == "0.0.0.0/0" || proxy == "::/0" {
				return fmt.Errorf("APP_TRUSTED_PROXIES must not trust every address (%s)", proxy)
			}
		}
	}

	if cfg.OAuth.GoogleClientID != "" && cfg.OAuth.GoogleClientSecret == "" {
		return fmt.Errorf("GOOGLE_CLIENT_SECRET is required when GOOGLE_CLIENT_ID is set")
	}
	switch cfg.Storage.Driver {
	case "local":
		if cfg.Storage.LocalPath == "" {
			return fmt.Errorf("STORAGE_LOCAL_PATH is required for local driver")
		}
	case "s3", "minio":
		if cfg.Storage.S3Endpoint == "" {
			return fmt.Errorf("STORAGE_S3_ENDPOINT is required for %s driver", cfg.Storage.Driver)
		}
		if cfg.Storage.S3AccessKey == "" {
			return fmt.Errorf("STORAGE_S3_ACCESS_KEY is required for %s driver", cfg.Storage.Driver)
		}
		if cfg.Storage.S3SecretKey == "" {
			return fmt.Errorf("STORAGE_S3_SECRET_KEY is required for %s driver", cfg.Storage.Driver)
		}
		if cfg.Storage.S3Bucket == "" {
			return fmt.Errorf("STORAGE_S3_BUCKET is required for %s driver", cfg.Storage.Driver)
		}
	default:
		return fmt.Errorf("STORAGE_DRIVER must be one of: local, s3, minio (got %q)", cfg.Storage.Driver)
	}

	// Production-only safety rails. These would break local development
	// defaults, so they are enforced only for the production environment.
	if cfg.App.Env == "production" {
		if cfg.App.BodyLimit <= int(cfg.Storage.MaxFileSize) {
			return fmt.Errorf("APP_BODY_LIMIT (%d) must be greater than STORAGE_MAX_FILE_SIZE (%d) to allow multipart framing", cfg.App.BodyLimit, cfg.Storage.MaxFileSize)
		}
		if len(cfg.Storage.AllowedTypes()) == 0 {
			return fmt.Errorf("STORAGE_ALLOWED_MIME_TYPES must not be empty in production")
		}
		if cfg.Email.Driver == "console" {
			return fmt.Errorf("EMAIL_DRIVER=console must not be used in production")
		}
		if cfg.Email.Driver == "smtp" && !cfg.Email.SMTPRequireTLS {
			return fmt.Errorf("SMTP_REQUIRE_TLS must be true when EMAIL_DRIVER=smtp in production")
		}
		for _, origin := range origins {
			if origin == "*" {
				return fmt.Errorf("CORS_ALLOW_ORIGINS must not contain '*' in production")
			}
		}
		if u, err := url.Parse(cfg.App.FrontendURL); err != nil || u.Scheme != "https" {
			return fmt.Errorf("APP_FRONTEND_URL must use https in production")
		}
		if cfg.DB.SSLMode == "disable" {
			return fmt.Errorf("DB_SSLMODE must not be disable in production")
		}
		if cfg.OAuth.GoogleClientID != "" {
			if u, err := url.Parse(cfg.OAuth.FrontendURL); err != nil || u.Scheme != "https" {
				return fmt.Errorf("OAUTH_FRONTEND_URL must use https in production")
			}
			if u, err := url.Parse(cfg.OAuth.GoogleRedirectURL); err != nil || u.Scheme != "https" {
				return fmt.Errorf("GOOGLE_REDIRECT_URL must use https in production")
			}
		}
		if cfg.App.MetricsAuthToken == "" {
			return fmt.Errorf("METRICS_AUTH_TOKEN must be set in production")
		}
		if len(cfg.App.MetricsAuthToken) < 16 {
			return fmt.Errorf("METRICS_AUTH_TOKEN must be at least 16 characters")
		}
	}
	return nil
}
