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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

type Config struct {
	App          AppConfig
	DB           DBConfig
	JWT          JWTConfig
	Storage      StorageConfig
	OAuth        OAuthConfig
	CORS         CORSConfig
	RateLimit    RateLimitConfig
	Cache        CacheConfig
	Email        EmailConfig
	Admin        AdminConfig
	Tenant       TenantConfig
	Tracing      TracingConfig
	I18N         I18NConfig
	FeatureFlags FeatureFlagsConfig
	Scheduler    SchedulerConfig
	Webhook      WebhookConfig
}

// I18NConfig controls request locale negotiation. Locale tags are matched
// against the embedded catalogs; unsupported values fail at startup.
type I18NConfig struct {
	DefaultLocale string `env:"I18N_DEFAULT_LOCALE" envDefault:"en"`
}

// Locale returns the normalized configured default locale, or "en".
func (c I18NConfig) Locale() string {
	locale := strings.ToLower(strings.TrimSpace(c.DefaultLocale))
	if locale == "" {
		return "en"
	}
	return locale
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
	// TrustIncoming honors an incoming W3C traceparent. It is off by default
	// because a client-supplied sampled flag would force sampling under the
	// parent-based sampler. With APP_TRUST_PROXY=true it is honored only from
	// peers in APP_TRUSTED_PROXIES.
	TrustIncoming bool `env:"OTEL_TRUST_INCOMING_TRACE" envDefault:"false"`

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

// FeatureFlagsConfig controls environment overrides, emergency force-off, and
// the in-process snapshot cache used by internal/platform/features.
type FeatureFlagsConfig struct {
	// Flags is a comma-separated key=true|false list; a bare key means true,
	// e.g. "user_registration,email_verification=false".
	Flags string `env:"FEATURE_FLAGS"`
	// ForceOff is a comma-separated list of keys that always evaluate false,
	// even when a database default or user/tenant target enables them.
	ForceOff string `env:"FEATURE_FLAGS_FORCE_OFF"`
	// CacheTTLSeconds is how long a loaded snapshot is considered fresh.
	CacheTTLSeconds int `env:"FEATURE_FLAGS_CACHE_TTL_SECONDS" envDefault:"30"`
	// MaxStalenessSeconds bounds how long the last good snapshot may be used
	// while the database is unavailable.
	MaxStalenessSeconds int `env:"FEATURE_FLAGS_MAX_STALENESS_SECONDS" envDefault:"300"`
}

// CacheTTL returns the configured snapshot TTL, applying the package default
// for zero values.
func (f FeatureFlagsConfig) CacheTTL() time.Duration {
	if f.CacheTTLSeconds <= 0 {
		return features.DefaultCacheTTL
	}
	return time.Duration(f.CacheTTLSeconds) * time.Second
}

// MaxStaleness returns the configured stale-snapshot bound. It is never lower
// than the cache TTL.
func (f FeatureFlagsConfig) MaxStaleness() time.Duration {
	maxStaleness := features.DefaultMaxStaleness
	if f.MaxStalenessSeconds > 0 {
		maxStaleness = time.Duration(f.MaxStalenessSeconds) * time.Second
	}
	if ttl := f.CacheTTL(); maxStaleness < ttl {
		maxStaleness = ttl
	}
	return maxStaleness
}

// EnvValues parses FEATURE_FLAGS. Config validation has already rejected
// malformed input in the normal Load path; invalid values here degrade to an
// empty map so a partially built Config in tests cannot panic.
func (f FeatureFlagsConfig) EnvValues() map[features.Key]bool {
	values, err := features.ParseFlags(f.Flags)
	if err != nil {
		return map[features.Key]bool{}
	}
	return values
}

// ValidateKnownKeys rejects FEATURE_FLAGS / FEATURE_FLAGS_FORCE_OFF entries
// that are not in the registered flag catalog, so a typo cannot silently
// disable a feature. Flags are registered by modules at startup, so call this
// after app.RegisterCatalogs; Validate only checks syntax.
func (f FeatureFlagsConfig) ValidateKnownKeys() error {
	parsed, err := features.ParseFlags(f.Flags)
	if err != nil {
		return fmt.Errorf("FEATURE_FLAGS: %w", err)
	}
	if err := features.KnownKeysError(parsed); err != nil {
		return fmt.Errorf("FEATURE_FLAGS: %w", err)
	}
	forceOff, err := features.ParseForceOff(f.ForceOff)
	if err != nil {
		return fmt.Errorf("FEATURE_FLAGS_FORCE_OFF: %w", err)
	}
	if err := features.KnownForceOffError(forceOff); err != nil {
		return fmt.Errorf("FEATURE_FLAGS_FORCE_OFF: %w", err)
	}
	return nil
}

// ForceOffValues parses FEATURE_FLAGS_FORCE_OFF.
func (f FeatureFlagsConfig) ForceOffValues() map[features.Key]struct{} {
	values, err := features.ParseForceOff(f.ForceOff)
	if err != nil {
		return map[features.Key]struct{}{}
	}
	return values
}

// SchedulerConfig controls the optional PostgreSQL-leader scheduler. Interval
// and cron schedules execute in UTC; the default is disabled so a deployment
// only runs schedules after an explicit opt-in.
type SchedulerConfig struct {
	Enabled       bool   `env:"SCHEDULER_ENABLED" envDefault:"false"`
	LeaseTTLSecs  int    `env:"SCHEDULER_LEASE_TTL_SECS" envDefault:"30"`
	HeartbeatSecs int    `env:"SCHEDULER_HEARTBEAT_SECS" envDefault:"10"`
	PollSecs      int    `env:"SCHEDULER_POLL_SECS" envDefault:"15"`
	Catchup       string `env:"SCHEDULER_CATCHUP" envDefault:"latest"`
	CatchupLimit  int    `env:"SCHEDULER_CATCHUP_LIMIT" envDefault:"100"`
	RetentionDays int    `env:"SCHEDULER_RETENTION_DAYS" envDefault:"30"`
}

// LeaseTTL returns the configured lease duration.
func (s SchedulerConfig) LeaseTTL() time.Duration {
	return time.Duration(s.LeaseTTLSecs) * time.Second
}

// HeartbeatInterval returns the configured lease heartbeat interval.
func (s SchedulerConfig) HeartbeatInterval() time.Duration {
	return time.Duration(s.HeartbeatSecs) * time.Second
}

// PollInterval returns the configured schedule evaluation interval.
func (s SchedulerConfig) PollInterval() time.Duration {
	return time.Duration(s.PollSecs) * time.Second
}

// Retention returns the configured scheduler_runs retention window.
func (s SchedulerConfig) Retention() time.Duration {
	return time.Duration(s.RetentionDays) * 24 * time.Hour
}

// Validate checks scheduler timing and catch-up settings. The defaults are
// valid even when the scheduler is disabled; enabling it never requires a
// second config pass.
func (s SchedulerConfig) Validate() error {
	// Programmatic configs in tests and embedders may leave the optional
	// scheduler block unset; env.Load fills it with defaults.
	if s == (SchedulerConfig{}) {
		return nil
	}
	if s.LeaseTTLSecs < 1 {
		return fmt.Errorf("SCHEDULER_LEASE_TTL_SECS must be at least 1")
	}
	if s.HeartbeatSecs < 1 {
		return fmt.Errorf("SCHEDULER_HEARTBEAT_SECS must be at least 1")
	}
	if s.HeartbeatSecs >= s.LeaseTTLSecs {
		return fmt.Errorf("SCHEDULER_HEARTBEAT_SECS (%d) must be less than SCHEDULER_LEASE_TTL_SECS (%d)", s.HeartbeatSecs, s.LeaseTTLSecs)
	}
	if s.PollSecs < 1 {
		return fmt.Errorf("SCHEDULER_POLL_SECS must be at least 1")
	}
	switch strings.ToLower(strings.TrimSpace(s.Catchup)) {
	case "none", "latest", "all":
	default:
		return fmt.Errorf("SCHEDULER_CATCHUP must be one of: none, latest, all (got %q)", s.Catchup)
	}
	if s.CatchupLimit < 1 {
		return fmt.Errorf("SCHEDULER_CATCHUP_LIMIT must be at least 1")
	}
	if s.RetentionDays < 1 {
		return fmt.Errorf("SCHEDULER_RETENTION_DAYS must be at least 1")
	}
	return nil
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
	BodyLimit int `env:"APP_BODY_LIMIT" envDefault:"12582912"` // 12MB
	// JSONBodyLimit caps every non-multipart request body (JSON, form, raw).
	// Only multipart/form-data uploads may use the larger APP_BODY_LIMIT.
	JSONBodyLimit int    `env:"APP_JSON_BODY_LIMIT" envDefault:"1048576"` // 1 MiB
	LogLevel      string `env:"LOG_LEVEL" envDefault:"info"`
	// RequestTimeout must fit the shutdown drain window
	// (ShutdownBudget - ShutdownReserve) so an in-flight request can finish
	// before the process stops waiting for it.
	RequestTimeout           int    `env:"APP_REQUEST_TIMEOUT" envDefault:"15"` // seconds
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

// ShutdownBudget is the total graceful-shutdown time after SIGTERM.
// Kubernetes allows terminationGracePeriodSeconds (30s) including the 5s
// preStop sleep, so 20s leaves a 5s margin before SIGKILL. ShutdownReserve is
// held back from draining requests and jobs for the tracing flush and closing
// shared resources. Keep deploy/k8s/base/deployment.yaml in sync.
const (
	ShutdownBudget  = 20 * time.Second
	ShutdownReserve = 5 * time.Second
)

// MaxRequestTimeoutSeconds is the largest APP_REQUEST_TIMEOUT that still lets
// an in-flight request finish inside the shutdown drain window.
const MaxRequestTimeoutSeconds = int((ShutdownBudget - ShutdownReserve) / time.Second)

// TrustedProxiesList returns the parsed list of trusted proxy IPs/CIDRs.
func (a AppConfig) TrustedProxiesList() []string {
	return splitCSV(a.TrustedProxies)
}

// DefaultJSONBodyLimit is the APP_JSON_BODY_LIMIT default (1 MiB).
const DefaultJSONBodyLimit = 1 << 20

// EffectiveJSONBodyLimit returns JSONBodyLimit, or the 1 MiB default for a
// zero value. Load rejects explicit values below 1; the fallback only covers
// AppConfig literals built in tests and tools.
func (a AppConfig) EffectiveJSONBodyLimit() int {
	if a.JSONBodyLimit <= 0 {
		return DefaultJSONBodyLimit
	}
	return a.JSONBodyLimit
}

type CORSConfig struct {
	AllowOrigins     string `env:"CORS_ALLOW_ORIGINS" envDefault:"*"`
	AllowMethods     string `env:"CORS_ALLOW_METHODS" envDefault:"GET,POST,PUT,PATCH,DELETE,OPTIONS"`
	AllowHeaders     string `env:"CORS_ALLOW_HEADERS" envDefault:"Origin,Content-Type,Accept,Accept-Language,Accept-Version,Authorization,Idempotency-Key,X-Tenant-Slug"`
	ExposeHeaders    string `env:"CORS_EXPOSE_HEADERS" envDefault:"Location,ETag,Idempotent-Replayed,Content-Language,API-Version,Deprecation,Sunset,Link"`
	AllowCredentials bool   `env:"CORS_ALLOW_CREDENTIALS" envDefault:"false"`
}

type RateLimitConfig struct {
	StrictMax     int `env:"RATE_LIMIT_STRICT_MAX" envDefault:"5"`
	StrictWindow  int `env:"RATE_LIMIT_STRICT_WINDOW_SECS" envDefault:"60"`
	NormalMax     int `env:"RATE_LIMIT_NORMAL_MAX" envDefault:"60"`
	NormalWindow  int `env:"RATE_LIMIT_NORMAL_WINDOW_SECS" envDefault:"60"`
	RelaxedMax    int `env:"RATE_LIMIT_RELAXED_MAX" envDefault:"120"`
	RelaxedWindow int `env:"RATE_LIMIT_RELAXED_WINDOW_SECS" envDefault:"60"`
	// Auth is the per-client-IP budget checked before authentication on every
	// protected route, so bogus credentials cannot hit the API-key store
	// unthrottled. Every request from one IP (NAT included) counts, so it is
	// generous; the per-user tiers behind it stay the real quota. Zero uses
	// the defaults below.
	AuthMax    int `env:"RATE_LIMIT_AUTH_MAX" envDefault:"300"`
	AuthWindow int `env:"RATE_LIMIT_AUTH_WINDOW_SECS" envDefault:"60"`
}

// Pre-authentication limiter defaults for RateLimitConfig literals that leave
// the Auth tier unset (tests and tools); Load fills the env defaults.
const (
	DefaultRateLimitAuthMax    = 300
	DefaultRateLimitAuthWindow = 60
)

// EffectiveAuth returns the pre-authentication limit and window in seconds.
func (r RateLimitConfig) EffectiveAuth() (maxRequests, windowSecs int) {
	maxRequests, windowSecs = r.AuthMax, r.AuthWindow
	if maxRequests <= 0 {
		maxRequests = DefaultRateLimitAuthMax
	}
	if windowSecs <= 0 {
		windowSecs = DefaultRateLimitAuthWindow
	}
	return maxRequests, windowSecs
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
	// AllowMemoryInProduction acknowledges a single-replica production
	// deployment. The memory driver keeps rate limits, idempotency, login
	// lockout, and session invalidation per process, so it is rejected in
	// production unless this is explicitly true.
	AllowMemoryInProduction bool `env:"CACHE_ALLOW_MEMORY_IN_PRODUCTION" envDefault:"false"`
	// Redis client bounds. go-redis defaults (5s dial, 3s read, 3 retries)
	// would turn a blackholed Redis into multi-second stalls on every request
	// even though rate limiting and idempotency fail open, so these are short.
	// They override timeout/retry parameters in REDIS_URL.
	RedisDialTimeoutMS  int `env:"REDIS_DIAL_TIMEOUT_MS" envDefault:"2000"`
	RedisReadTimeoutMS  int `env:"REDIS_READ_TIMEOUT_MS" envDefault:"500"`
	RedisWriteTimeoutMS int `env:"REDIS_WRITE_TIMEOUT_MS" envDefault:"500"`
	RedisPoolTimeoutMS  int `env:"REDIS_POOL_TIMEOUT_MS" envDefault:"1000"`
	// RedisMaxRetries is the number of retries after the first attempt; 0
	// disables retries.
	RedisMaxRetries int `env:"REDIS_MAX_RETRIES" envDefault:"1"`
}

// Redis client defaults, used when a CacheConfig literal leaves a timeout at
// zero (tests and tools); Load always fills the env defaults.
const (
	DefaultRedisDialTimeout  = 2 * time.Second
	DefaultRedisReadTimeout  = 500 * time.Millisecond
	DefaultRedisWriteTimeout = 500 * time.Millisecond
	DefaultRedisPoolTimeout  = time.Second
	maxRedisRetries          = 10
)

func millisOrDefault(ms int, fallback time.Duration) time.Duration {
	if ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// RedisDialTimeout returns the Redis connect timeout.
func (c CacheConfig) RedisDialTimeout() time.Duration {
	return millisOrDefault(c.RedisDialTimeoutMS, DefaultRedisDialTimeout)
}

// RedisReadTimeout returns the Redis per-command read timeout.
func (c CacheConfig) RedisReadTimeout() time.Duration {
	return millisOrDefault(c.RedisReadTimeoutMS, DefaultRedisReadTimeout)
}

// RedisWriteTimeout returns the Redis per-command write timeout.
func (c CacheConfig) RedisWriteTimeout() time.Duration {
	return millisOrDefault(c.RedisWriteTimeoutMS, DefaultRedisWriteTimeout)
}

// RedisPoolTimeout returns how long a command waits for a free connection.
func (c CacheConfig) RedisPoolTimeout() time.Duration {
	return millisOrDefault(c.RedisPoolTimeoutMS, DefaultRedisPoolTimeout)
}

// Validate checks the cache driver and Redis client bounds.
func (c CacheConfig) Validate() error {
	switch c.Driver {
	case "memory":
	case "redis":
		if c.RedisURL == "" {
			return fmt.Errorf("REDIS_URL is required when CACHE_DRIVER=redis")
		}
	default:
		return fmt.Errorf("CACHE_DRIVER must be one of: memory, redis (got %q)", c.Driver)
	}
	if c.RedisDialTimeoutMS < 0 || c.RedisReadTimeoutMS < 0 || c.RedisWriteTimeoutMS < 0 || c.RedisPoolTimeoutMS < 0 {
		return fmt.Errorf("REDIS_*_TIMEOUT_MS values must not be negative")
	}
	if c.RedisMaxRetries < 0 || c.RedisMaxRetries > maxRedisRetries {
		return fmt.Errorf("REDIS_MAX_RETRIES must be between 0 and %d", maxRedisRetries)
	}
	return nil
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
	// S3AllowInsecure acknowledges plaintext object-storage traffic in
	// production (for example a cluster-local SeaweedFS behind a
	// NetworkPolicy). Without it, production requires STORAGE_S3_USE_SSL=true.
	S3AllowInsecure bool `env:"STORAGE_S3_ALLOW_INSECURE" envDefault:"false"`
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
	// silently downgrade transport security relative to the API.
	if appEnv == "production" {
		if err := validateProductionSSLMode(parsed.DB.SSLMode); err != nil {
			return DBConfig{}, err
		}
	}
	return parsed.DB, nil
}

// validateProductionSSLMode rejects DB_SSLMODE values that permit plaintext:
// disable never encrypts, and allow/prefer silently fall back to plaintext
// when the server or a network attacker refuses TLS. The API and cmd/migrate
// share this rule.
func validateProductionSSLMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "disable", "allow", "prefer":
		return fmt.Errorf("DB_SSLMODE=%s permits plaintext in production; use require, verify-ca, or verify-full", mode)
	}
	return nil
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
	if cfg.App.RequestTimeout > MaxRequestTimeoutSeconds {
		return fmt.Errorf("APP_REQUEST_TIMEOUT (%d) must be at most %d seconds so in-flight requests finish within the shutdown budget",
			cfg.App.RequestTimeout, MaxRequestTimeoutSeconds)
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

	// Feature flags: reject malformed or duplicate entries here. Unknown keys
	// are rejected by FeatureFlagsConfig.ValidateKnownKeys, which cmd/api calls
	// after modules register their flags (the catalog is not complete yet when
	// config loads). Zero cache values mean "use the package defaults";
	// negative values are configuration errors.
	if _, err := features.ParseFlags(cfg.FeatureFlags.Flags); err != nil {
		return fmt.Errorf("FEATURE_FLAGS: %w", err)
	}
	if _, err := features.ParseForceOff(cfg.FeatureFlags.ForceOff); err != nil {
		return fmt.Errorf("FEATURE_FLAGS_FORCE_OFF: %w", err)
	}
	if cfg.FeatureFlags.CacheTTLSeconds < 0 {
		return fmt.Errorf("FEATURE_FLAGS_CACHE_TTL_SECONDS must not be negative")
	}
	if cfg.FeatureFlags.MaxStalenessSeconds < 0 {
		return fmt.Errorf("FEATURE_FLAGS_MAX_STALENESS_SECONDS must not be negative")
	}
	if cfg.FeatureFlags.CacheTTLSeconds > 0 && cfg.FeatureFlags.MaxStalenessSeconds > 0 &&
		cfg.FeatureFlags.MaxStalenessSeconds < cfg.FeatureFlags.CacheTTLSeconds {
		return fmt.Errorf(
			"FEATURE_FLAGS_MAX_STALENESS_SECONDS (%d) must be at least FEATURE_FLAGS_CACHE_TTL_SECONDS (%d)",
			cfg.FeatureFlags.MaxStalenessSeconds, cfg.FeatureFlags.CacheTTLSeconds,
		)
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
	if cfg.App.JSONBodyLimit < 1 {
		return fmt.Errorf("APP_JSON_BODY_LIMIT must be at least 1 byte")
	}
	if cfg.App.JSONBodyLimit > cfg.App.BodyLimit {
		return fmt.Errorf("APP_JSON_BODY_LIMIT (%d) must not exceed APP_BODY_LIMIT (%d)", cfg.App.JSONBodyLimit, cfg.App.BodyLimit)
	}
	if cfg.RateLimit.StrictMax < 1 || cfg.RateLimit.NormalMax < 1 || cfg.RateLimit.RelaxedMax < 1 {
		return fmt.Errorf("all RATE_LIMIT_*_MAX values must be at least 1")
	}
	if cfg.RateLimit.StrictWindow < 1 || cfg.RateLimit.NormalWindow < 1 || cfg.RateLimit.RelaxedWindow < 1 {
		return fmt.Errorf("all RATE_LIMIT_*_WINDOW_SECS values must be at least 1")
	}
	if cfg.RateLimit.AuthMax < 0 || cfg.RateLimit.AuthWindow < 0 {
		return fmt.Errorf("RATE_LIMIT_AUTH_MAX and RATE_LIMIT_AUTH_WINDOW_SECS must not be negative")
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
	if err := cfg.Scheduler.Validate(); err != nil {
		return err
	}

	if err := cfg.Cache.Validate(); err != nil {
		return err
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
	case "s3", "seaweedfs":
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
		return fmt.Errorf("STORAGE_DRIVER must be one of: local, s3, seaweedfs (got %q)", cfg.Storage.Driver)
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
		if cfg.Cache.Driver == "memory" && !cfg.Cache.AllowMemoryInProduction {
			return fmt.Errorf("CACHE_DRIVER=memory is not shared across replicas; use redis in production or set CACHE_ALLOW_MEMORY_IN_PRODUCTION=true for a single-replica deployment")
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
		if err := validateProductionSSLMode(cfg.DB.SSLMode); err != nil {
			return err
		}
		// Object storage carries user uploads and credentials. A deployment
		// whose storage is reachable only over a trusted private network must
		// opt out explicitly.
		if (cfg.Storage.Driver == "s3" || cfg.Storage.Driver == "seaweedfs") &&
			!cfg.Storage.S3UseSSL && !cfg.Storage.S3AllowInsecure {
			return fmt.Errorf("STORAGE_S3_USE_SSL must be true in production (set STORAGE_S3_ALLOW_INSECURE=true only for storage on a trusted private network)")
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
