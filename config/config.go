package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/caarlos0/env/v11"
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
	Tracing   TracingConfig
}

// TracingConfig controls OpenTelemetry trace generation and export. Tracing is
// disabled by default; when disabled, the application uses a no-op provider
// and never opens a collector connection.
type TracingConfig struct {
	Enabled          bool   `env:"OTEL_ENABLED" envDefault:"false"`
	Exporter         string `env:"OTEL_TRACES_EXPORTER" envDefault:"otlp"` // otlp|none
	Protocol         string `env:"OTEL_EXPORTER_OTLP_PROTOCOL" envDefault:"grpc"`
	Endpoint         string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"localhost:4317"`
	Insecure         bool   `env:"OTEL_EXPORTER_OTLP_INSECURE" envDefault:"true"`
	ServiceName      string `env:"OTEL_SERVICE_NAME" envDefault:"fiber-golang-boilerplate"`
	ServiceNamespace string `env:"OTEL_SERVICE_NAMESPACE"`
	ServiceVersion   string `env:"OTEL_SERVICE_VERSION" envDefault:"dev"`
	// DeploymentEnvironment is the logical environment (local, staging,
	// production) reported on every span resource.
	DeploymentEnvironment string `env:"OTEL_DEPLOYMENT_ENVIRONMENT" envDefault:"local"`
	// InstanceID identifies one process. Empty means the hostname is used.
	InstanceID string `env:"OTEL_SERVICE_INSTANCE_ID"`
	// Sampler accepts the standard OTel names. The default samples every
	// trace at the root, but always follows a parent's sampling decision.
	Sampler    string  `env:"OTEL_TRACES_SAMPLER" envDefault:"parentbased_traceidratio"`
	SamplerArg float64 `env:"OTEL_TRACES_SAMPLER_ARG" envDefault:"1.0"`
	// ExcludePaths is a comma-separated list of exact request paths (for
	// example health and metrics probes) that must not create spans.
	ExcludePaths string `env:"OTEL_TRACING_EXCLUDE_PATHS" envDefault:"/healthz,/readyz,/health,/metrics"`
	// InstrumentDB adds sanitized client spans for pgx queries that run under
	// a recording parent span.
	InstrumentDB bool `env:"OTEL_INSTRUMENT_DB" envDefault:"true"`

	BatchTimeout       int `env:"OTEL_BATCH_TIMEOUT_SECS" envDefault:"5"`
	ExportTimeout      int `env:"OTEL_EXPORT_TIMEOUT_SECS" envDefault:"30"`
	ShutdownTimeout    int `env:"OTEL_SHUTDOWN_TIMEOUT_SECS" envDefault:"10"`
	MaxQueueSize       int `env:"OTEL_BATCH_MAX_QUEUE_SIZE" envDefault:"2048"`
	MaxExportBatchSize int `env:"OTEL_BATCH_MAX_EXPORT_BATCH_SIZE" envDefault:"512"`
}

// ExcludedPaths returns the parsed list of paths that are not traced.
func (t TracingConfig) ExcludedPaths() []string {
	return splitCSV(t.ExcludePaths)
}

// Validate checks tracing settings only when tracing is enabled. A disabled
// configuration is inert, so stale or partial OTEL_* values cannot break a
// deployment that does not use tracing.
func (t TracingConfig) Validate() error {
	if !t.Enabled {
		return nil
	}

	switch strings.ToLower(strings.TrimSpace(t.Exporter)) {
	case "otlp", "none":
	default:
		return fmt.Errorf("OTEL_TRACES_EXPORTER must be one of: otlp, none (got %q)", t.Exporter)
	}
	if proto := strings.ToLower(strings.TrimSpace(t.Protocol)); proto != "grpc" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL must be grpc (got %q)", t.Protocol)
	}
	if strings.TrimSpace(t.ServiceName) == "" {
		return fmt.Errorf("OTEL_SERVICE_NAME must not be empty")
	}
	if strings.TrimSpace(t.DeploymentEnvironment) == "" {
		return fmt.Errorf("OTEL_DEPLOYMENT_ENVIRONMENT must not be empty")
	}
	if strings.TrimSpace(t.Sampler) == "" {
		return fmt.Errorf("OTEL_TRACES_SAMPLER must not be empty")
	}
	switch strings.ToLower(strings.TrimSpace(t.Sampler)) {
	case "parentbased_traceidratio", "parentbased_always_on", "parentbased_always_off",
		"traceidratio", "always_on", "always_off":
	default:
		return fmt.Errorf("OTEL_TRACES_SAMPLER %q is not supported", t.Sampler)
	}
	if t.SamplerArg < 0 || t.SamplerArg > 1 {
		return fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be between 0 and 1 (got %v)", t.SamplerArg)
	}
	if strings.EqualFold(strings.TrimSpace(t.Exporter), "otlp") {
		if err := validateOTLPEndpoint(t.Endpoint); err != nil {
			return err
		}
	}
	if t.BatchTimeout < 1 {
		return fmt.Errorf("OTEL_BATCH_TIMEOUT_SECS must be at least 1 second")
	}
	if t.ExportTimeout < 1 {
		return fmt.Errorf("OTEL_EXPORT_TIMEOUT_SECS must be at least 1 second")
	}
	if t.ShutdownTimeout < 1 {
		return fmt.Errorf("OTEL_SHUTDOWN_TIMEOUT_SECS must be at least 1 second")
	}
	if t.MaxQueueSize < 1 {
		return fmt.Errorf("OTEL_BATCH_MAX_QUEUE_SIZE must be at least 1")
	}
	if t.MaxExportBatchSize < 1 {
		return fmt.Errorf("OTEL_BATCH_MAX_EXPORT_BATCH_SIZE must be at least 1")
	}
	if t.MaxExportBatchSize > t.MaxQueueSize {
		return fmt.Errorf("OTEL_BATCH_MAX_EXPORT_BATCH_SIZE (%d) must not exceed OTEL_BATCH_MAX_QUEUE_SIZE (%d)",
			t.MaxExportBatchSize, t.MaxQueueSize)
	}
	for _, path := range t.ExcludedPaths() {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("OTEL_TRACING_EXCLUDE_PATHS entries must start with '/' (got %q)", path)
		}
	}
	return nil
}

// validateOTLPEndpoint rejects URLs and empty values so a typo fails at
// startup instead of silently exporting nowhere.
func validateOTLPEndpoint(endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must not be empty when OTEL_TRACES_EXPORTER=otlp")
	}
	if strings.Contains(endpoint, "://") {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must be host:port without a scheme (got %q)", endpoint)
	}
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must be host:port (got %q)", endpoint)
	}
	return nil
}

// TenantConfig controls request-tenant resolution. The default slug preserves
// compatibility for requests that do not send X-Tenant-Slug in local/test.
// Production validation requires RequireHeader so HTTP writes cannot silently
// fall back to the compatibility tenant.
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

	// Observability: tracing is inert unless explicitly enabled.
	if err := cfg.Tracing.Validate(); err != nil {
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
		// Header-less requests must not silently fall back to the compatibility
		// tenant in production. Requiring the header makes tenant selection
		// explicit for every HTTP write and lets links carry a known slug.
		if !cfg.Tenant.RequireHeader {
			return fmt.Errorf("TENANT_REQUIRE_HEADER must be true in production so header-less writes cannot fall back to the default tenant")
		}
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
