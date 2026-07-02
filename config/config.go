package config

import (
	"fmt"
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
}

type AdminConfig struct {
	Email    string `env:"ADMIN_EMAIL"`
	Password string `env:"ADMIN_PASSWORD"`
	Name     string `env:"ADMIN_NAME" envDefault:"Admin"`
}

type AppConfig struct {
	Port                     int    `env:"APP_PORT" envDefault:"8080"`
	Env                      string `env:"APP_ENV" envDefault:"local"`
	BodyLimit                int    `env:"APP_BODY_LIMIT" envDefault:"4194304"` // 4MB
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
	AllowHeaders     string `env:"CORS_ALLOW_HEADERS" envDefault:"Origin,Content-Type,Accept,Authorization"`
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
	Database        string `env:"DB_DATABASE" envDefault:"fiber_app"`
	Username        string `env:"DB_USERNAME" envDefault:"postgres"`
	Password        string `env:"DB_PASSWORD" envDefault:"postgres"`
	Schema          string `env:"DB_SCHEMA" envDefault:"public"`
	SSLMode         string `env:"DB_SSLMODE" envDefault:"disable"`
	MaxConns        int32  `env:"DB_MAX_CONNS" envDefault:"25"`
	MinConns        int32  `env:"DB_MIN_CONNS" envDefault:"5"`
	MaxConnLifetime int    `env:"DB_MAX_CONN_LIFETIME" envDefault:"3600"`   // seconds
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

// Headers returns the list of allowed CORS headers.
func (c CORSConfig) Headers() []string {
	return splitCSV(c.AllowHeaders)
}

func (db DBConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s&search_path=%s",
		db.Username, db.Password, db.Host, db.Port, db.Database, db.SSLMode, db.Schema,
	)
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

func (cfg *Config) Validate() error {
	if cfg.App.Port < 1 || cfg.App.Port > 65535 {
		return fmt.Errorf("APP_PORT must be between 1 and 65535")
	}

	// JWT validation
	if cfg.App.Env != "local" && cfg.App.Env != "test" {
		if cfg.JWT.Secret == "" || cfg.JWT.Secret == "secret" {
			return fmt.Errorf("JWT_SECRET must be set to a secure value in %s environment", cfg.App.Env)
		}
		if len(cfg.JWT.Secret) < 32 {
			return fmt.Errorf("JWT_SECRET must be at least 32 characters for security")
		}
	}
	if cfg.JWT.ExpireHour < 1 {
		return fmt.Errorf("JWT_EXPIRE_HOUR must be at least 1")
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
	if cfg.DB.MaxConns < 1 {
		return fmt.Errorf("DB_MAX_CONNS must be at least 1")
	}
	if cfg.DB.MinConns < 0 {
		return fmt.Errorf("DB_MIN_CONNS must not be negative")
	}
	if cfg.DB.MinConns > cfg.DB.MaxConns {
		return fmt.Errorf("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", cfg.DB.MinConns, cfg.DB.MaxConns)
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

	// CORS: AllowCredentials with wildcard origin is a spec violation
	if cfg.CORS.AllowCredentials && cfg.CORS.AllowOrigins == "*" {
		return fmt.Errorf("CORS_ALLOW_CREDENTIALS cannot be true when CORS_ALLOW_ORIGINS is '*'")
	}

	// Trusting proxies without an allowlist would let any client spoof
	// X-Forwarded-For; require explicit proxy IPs/CIDRs.
	if cfg.App.TrustProxy && len(cfg.App.TrustedProxiesList()) == 0 {
		return fmt.Errorf("APP_TRUSTED_PROXIES must list at least one IP/CIDR when APP_TRUST_PROXY=true")
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
	return nil
}
