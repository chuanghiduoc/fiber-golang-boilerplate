package config

import (
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
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

func TestCORSConfig_DefaultsIncludeIdempotencyHeaders(t *testing.T) {
	t.Setenv("CORS_ALLOW_HEADERS", "")
	t.Setenv("CORS_EXPOSE_HEADERS", "")

	var c CORSConfig
	if err := env.Parse(&c); err != nil {
		t.Fatalf("parse CORS defaults: %v", err)
	}

	if !contains(c.Headers(), "Idempotency-Key") {
		t.Errorf("default CORS allowed headers = %v, want Idempotency-Key", c.Headers())
	}
	for _, header := range []string{"Accept-Language", "Accept-Version"} {
		if !contains(c.Headers(), header) {
			t.Errorf("default CORS allowed headers = %v, want %s", c.Headers(), header)
		}
	}
	for _, header := range []string{"Location", "ETag", "Idempotent-Replayed", "Content-Language", "API-Version", "Deprecation", "Sunset", "Link"} {
		if !contains(c.ExposedHeaders(), header) {
			t.Errorf("default CORS exposed headers = %v, want %s", c.ExposedHeaders(), header)
		}
	}
}

func TestLoad_I18NDefaultLocale(t *testing.T) {
	t.Setenv("I18N_DEFAULT_LOCALE", "vi")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.I18N.DefaultLocale != "vi" {
		t.Errorf("I18N_DEFAULT_LOCALE = %q, want vi", cfg.I18N.DefaultLocale)
	}
}

func TestI18NConfigDefaultLocale(t *testing.T) {
	t.Setenv("I18N_DEFAULT_LOCALE", "")
	var cfg I18NConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("parse I18N defaults: %v", err)
	}
	if cfg.DefaultLocale != "en" {
		t.Errorf("I18N_DEFAULT_LOCALE = %q, want en", cfg.DefaultLocale)
	}

	cfg.DefaultLocale = " VI "
	if got := cfg.Locale(); got != "vi" {
		t.Errorf("Locale() = %q, want vi", got)
	}
	if got := (I18NConfig{}).Locale(); got != "en" {
		t.Errorf("zero Locale() = %q, want en", got)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestValidate_RejectsWildcardTrustedProxy(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.TrustProxy = true
	cfg.App.TrustedProxies = "0.0.0.0/0"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() should reject a wildcard trusted proxy")
	}
}

func TestValidate_WebhookSettings(t *testing.T) {
	t.Run("dedicated key", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.SecretEncryptionKey = strings.Repeat("k", 32)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() rejected a valid webhook key: %v", err)
		}
		if got := cfg.Webhook.ResolvedSecretKey("jwt"); got != strings.Repeat("k", 32) {
			t.Errorf("ResolvedSecretKey() = %q, want the dedicated key", got)
		}
	})

	t.Run("fallback to JWT secret", func(t *testing.T) {
		cfg := baseValidConfig()
		if got := cfg.Webhook.ResolvedSecretKey(cfg.JWT.Secret); got != cfg.JWT.Secret {
			t.Errorf("ResolvedSecretKey() = %q, want JWT fallback", got)
		}
	})

	t.Run("short key", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.SecretEncryptionKey = "too-short"
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should reject a short webhook key")
		}
	})

	t.Run("production requires dedicated key", func(t *testing.T) {
		cfg := productionValidConfig()
		cfg.Webhook.SecretEncryptionKey = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should require a dedicated key in production")
		}
	})

	t.Run("timeout bounds", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.ConnectTimeoutSeconds = 20
		cfg.Webhook.RequestTimeoutSeconds = 10
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should reject connect timeout above request timeout")
		}
	})

	t.Run("retry bounds", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.RetryMaxSeconds = 1
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should reject retry max below retry base")
		}
	})

	t.Run("attempt cap", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.MaxAttempts = 51
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should reject more than 50 attempts")
		}
	})

	t.Run("retention bounds", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.RetentionDays = 0
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should reject zero retention")
		}
	})

	t.Run("timestamp tolerance", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.TimestampToleranceSeconds = 5
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should reject a 5s timestamp tolerance")
		}
	})

	t.Run("invalid SSRF allowlist", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Webhook.SSRFAllowlist = "10.0.0.0/99"
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate() should reject an invalid allowlist CIDR")
		}
	})
}

func TestLoad_WebhookDefaults(t *testing.T) {
	t.Setenv("WEBHOOK_CONNECT_TIMEOUT_SECS", "")
	t.Setenv("WEBHOOK_REQUEST_TIMEOUT_SECS", "")
	t.Setenv("WEBHOOK_RETRY_BASE_SECS", "")
	t.Setenv("WEBHOOK_RETRY_MAX_SECS", "")
	t.Setenv("WEBHOOK_MAX_ATTEMPTS", "")
	t.Setenv("WEBHOOK_RETENTION_DAYS", "")
	t.Setenv("WEBHOOK_TIMESTAMP_TOLERANCE_SECS", "")
	t.Setenv("WEBHOOK_SECRET_ENCRYPTION_KEY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Webhook.ConnectTimeoutSeconds != 5 || cfg.Webhook.RequestTimeoutSeconds != 10 {
		t.Errorf("unexpected timeout defaults: %+v", cfg.Webhook)
	}
	if cfg.Webhook.MaxAttempts != 8 || cfg.Webhook.RetentionDays != 30 {
		t.Errorf("unexpected attempt/retention defaults: %+v", cfg.Webhook)
	}
	if cfg.Webhook.TimestampTolerance().Seconds() != 300 {
		t.Errorf("timestamp tolerance = %s, want 5m", cfg.Webhook.TimestampTolerance())
	}
	if cfg.Webhook.ResolvedSecretKey(cfg.JWT.Secret) == "" {
		t.Error("local webhook key must fall back to JWT_SECRET")
	}
}

func TestValidate_APIKeyMaxLifetime(t *testing.T) {
	for _, lifetime := range []time.Duration{0, -time.Hour} {
		cfg := baseValidConfig()
		cfg.APIKeys.MaxLifetime = lifetime
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() should reject APIKEYS_MAX_LIFETIME %s", lifetime)
		}
	}
}

func TestLoad_APIKeyDefaults(t *testing.T) {
	t.Setenv("APIKEYS_MAX_LIFETIME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.APIKeys.MaxLifetime != 8760*time.Hour {
		t.Errorf("APIKEYS_MAX_LIFETIME = %s, want 8760h", cfg.APIKeys.MaxLifetime)
	}
}

func TestValidate_WebhookAllowedPorts(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Webhook.AllowedPortsCSV = "8443, 9443"
	ports, err := cfg.Webhook.AllowedPorts()
	if err != nil {
		t.Fatalf("AllowedPorts: %v", err)
	}
	if !containsInt(ports, 80) || !containsInt(ports, 443) || !containsInt(ports, 8443) || !containsInt(ports, 9443) {
		t.Fatalf("ports = %v, want defaults plus configured entries", ports)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected valid ports: %v", err)
	}

	for _, value := range []string{"0", "65536", "-1", "abc", "80,not-a-port"} {
		cfg := baseValidConfig()
		cfg.Webhook.AllowedPortsCSV = value
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() should reject WEBHOOK_ALLOWED_PORTS=%q", value)
		}
	}
}

func TestLoad_WebhookAllowedPortDefaults(t *testing.T) {
	t.Setenv("WEBHOOK_ALLOWED_PORTS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	ports, err := cfg.Webhook.AllowedPorts()
	if err != nil {
		t.Fatalf("AllowedPorts: %v", err)
	}
	if len(ports) != 2 || !containsInt(ports, 80) || !containsInt(ports, 443) {
		t.Fatalf("default ports = %v, want 80 and 443", ports)
	}
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestAppConfig_TrustedProxiesList(t *testing.T) {
	a := AppConfig{TrustedProxies: "10.0.0.1, 192.168.0.0/16 ,"}
	got := a.TrustedProxiesList()
	if len(got) != 2 {
		t.Errorf("TrustedProxiesList() len = %d, want 2", len(got))
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
	expected := "postgres://user:pass@localhost:5432/testdb?search_path=public&sslmode=disable"
	if dsn != expected {
		t.Errorf("DSN() = %q, want %q", dsn, expected)
	}
}

func TestDBConfig_DSNForUsesOnlyTheCredentials(t *testing.T) {
	db := DBConfig{
		Host:     "db.internal",
		Port:     5433,
		Database: "app",
		Username: "app",
		Password: "app-secret",
		SSLMode:  "verify-full",
		Schema:   "tenant",
	}

	dsn := db.DSNFor("app_bypass", "bypass-secret")
	for _, want := range []string{
		"app_bypass:bypass-secret@db.internal:5433/app",
		"search_path=tenant",
		"sslmode=verify-full",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSNFor() = %q, want it to contain %q", dsn, want)
		}
	}
	if strings.Contains(dsn, "app-secret") {
		t.Errorf("DSNFor() must not leak the normal role password: %q", dsn)
	}
}

func TestDBConfig_BypassConfigured(t *testing.T) {
	if (DBConfig{}).BypassConfigured() {
		t.Error("an empty bypass username must not be configured")
	}
	if !(DBConfig{BypassUsername: " app_bypass "}).BypassConfigured() {
		t.Error("a non-empty bypass username must be configured")
	}
}

func TestDBConfig_ValidateProductionBypassRole(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantErr  bool
		wantText string
	}{
		{
			name:   "valid",
			mutate: func(*Config) {},
		},
		{
			name:     "missing username",
			mutate:   func(c *Config) { c.DB.BypassUsername = "" },
			wantErr:  true,
			wantText: "DB_BYPASS_USERNAME is required",
		},
		{
			name:     "missing password",
			mutate:   func(c *Config) { c.DB.BypassPassword = "" },
			wantErr:  true,
			wantText: "DB_BYPASS_PASSWORD is required",
		},
		{
			name: "same as the normal role",
			mutate: func(c *Config) {
				c.DB.Username = "app"
				c.DB.BypassUsername = "app"
			},
			wantErr:  true,
			wantText: "must differ from DB_USERNAME",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := productionValidConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("Validate() = %v, want it to mention %q", err, tc.wantText)
			}
		})
	}
}

func TestDBConfig_DSN_SSLROOTCERT(t *testing.T) {
	db := DBConfig{
		Host:        "localhost",
		Port:        5432,
		Database:    "testdb",
		Username:    "user",
		Password:    "pass",
		SSLMode:     "verify-full",
		Schema:      "public",
		SSLRootCert: "/etc/ssl/certs/db-ca.pem",
	}

	dsn := db.DSN()
	if !strings.Contains(dsn, "sslrootcert=%2Fetc%2Fssl%2Fcerts%2Fdb-ca.pem") {
		t.Errorf("DSN() = %q, want it to contain the URL-escaped sslrootcert", dsn)
	}

	db.SSLRootCert = "  "
	if strings.Contains(db.DSN(), "sslrootcert") {
		t.Errorf("DSN() = %q, want no sslrootcert for a blank DB_SSLROOTCERT", db.DSN())
	}

	db.SSLMode = "VERIFY-FULL"
	if !strings.Contains(db.DSN(), "sslmode=verify-full") {
		t.Errorf("DSN() = %q, want a lowercased sslmode for pgx", db.DSN())
	}
}

// baseValidConfig returns a fully valid config. Each Validate test starts from
// this baseline and mutates exactly one field so failures isolate one rule.
func baseValidConfig() Config {
	return Config{
		App: AppConfig{
			Port: 8080, Env: "local", BodyLimit: 12582912, JSONBodyLimit: 1048576,
			RequestTimeout: 12, FrontendURL: "http://localhost:3000",
		},
		JWT:       JWTConfig{Secret: "secret", ExpireHour: 24, RefreshExpireDays: 30, SessionMaxLifetime: 720 * time.Hour},
		DB:        DBConfig{MaxConns: 25, MinConns: 5, ConnectTimeout: 10},
		Storage:   StorageConfig{Driver: "local", LocalPath: "./uploads", MaxFileSize: 10485760},
		RateLimit: RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		CORS:      CORSConfig{AllowOrigins: "*"},
		Cache:     CacheConfig{Driver: "memory", MemoryMaxEntries: DefaultMemoryMaxEntries},
		Email:     EmailConfig{Driver: "console"},
		Tenant:    TenantConfig{DefaultSlug: "default"},
		Webhook: WebhookConfig{
			ConnectTimeoutSeconds: 5, RequestTimeoutSeconds: 10,
			RetryBaseSeconds: 5, RetryMaxSeconds: 3600, MaxAttempts: 8,
			RetentionDays: 30, TimestampToleranceSeconds: 300,
		},
		APIKeys: APIKeysConfig{MaxLifetime: 8760 * time.Hour},
	}
}

func TestValidate_ValidLocalConfig(t *testing.T) {
	cfg := baseValidConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() returned error for valid config: %v", err)
	}
}

func TestSchedulerConfig_Defaults(t *testing.T) {
	var cfg SchedulerConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("parse scheduler defaults: %v", err)
	}
	if cfg.Enabled {
		t.Error("scheduler must default to disabled")
	}
	if cfg.LeaseTTL() != 30*time.Second {
		t.Errorf("LeaseTTL() = %v, want 30s", cfg.LeaseTTL())
	}
	if cfg.HeartbeatInterval() != 10*time.Second {
		t.Errorf("HeartbeatInterval() = %v, want 10s", cfg.HeartbeatInterval())
	}
	if cfg.PollInterval() != 15*time.Second {
		t.Errorf("PollInterval() = %v, want 15s", cfg.PollInterval())
	}
	if cfg.Catchup != "latest" {
		t.Errorf("Catchup = %q, want latest", cfg.Catchup)
	}
	if cfg.Retention() != 30*24*time.Hour {
		t.Errorf("Retention() = %v, want 720h", cfg.Retention())
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("default scheduler config must validate: %v", err)
	}
}

func TestSchedulerConfig_Validate(t *testing.T) {
	valid := SchedulerConfig{
		Enabled: true, LeaseTTLSecs: 30, HeartbeatSecs: 10, PollSecs: 15,
		Catchup: "all", CatchupLimit: 10, RetentionDays: 7,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid scheduler config rejected: %v", err)
	}

	tests := []struct {
		name string
		cfg  SchedulerConfig
	}{
		{"zero config", SchedulerConfig{}},
		{"lease ttl", SchedulerConfig{LeaseTTLSecs: 0, HeartbeatSecs: 1, PollSecs: 1, Catchup: "none", CatchupLimit: 1, RetentionDays: 1}},
		{"heartbeat", SchedulerConfig{LeaseTTLSecs: 10, HeartbeatSecs: 10, PollSecs: 1, Catchup: "none", CatchupLimit: 1, RetentionDays: 1}},
		{"poll", SchedulerConfig{LeaseTTLSecs: 10, HeartbeatSecs: 1, PollSecs: 0, Catchup: "none", CatchupLimit: 1, RetentionDays: 1}},
		{"catchup", SchedulerConfig{LeaseTTLSecs: 10, HeartbeatSecs: 1, PollSecs: 1, Catchup: "sometimes", CatchupLimit: 1, RetentionDays: 1}},
		{"catchup limit", SchedulerConfig{LeaseTTLSecs: 10, HeartbeatSecs: 1, PollSecs: 1, Catchup: "none", CatchupLimit: 0, RetentionDays: 1}},
		{"retention", SchedulerConfig{LeaseTTLSecs: 10, HeartbeatSecs: 1, PollSecs: 1, Catchup: "none", CatchupLimit: 1, RetentionDays: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Zero config is valid by design (unset optional block), all other
			// cases must fail their respective rule.
			if tt.name == "zero config" {
				if err := tt.cfg.Validate(); err != nil {
					t.Fatalf("zero scheduler config must validate: %v", err)
				}
				return
			}
			if err := tt.cfg.Validate(); err == nil {
				t.Error("Validate() should fail")
			}
		})
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.Port = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for port 0")
	}
}

func TestValidate_HighPort(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.Port = 70000
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for port > 65535")
	}
}

func TestValidate_InsecureJWTSecret_Production(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.Env = "production" // secret stays the insecure default "secret"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for insecure JWT secret in production")
	}
}

func TestValidate_InvalidExpireHour(t *testing.T) {
	cfg := baseValidConfig()
	cfg.JWT.ExpireHour = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for ExpireHour < 1")
	}
}

func TestValidate_InvalidSessionMaxLifetime(t *testing.T) {
	for _, lifetime := range []time.Duration{0, -time.Hour} {
		cfg := baseValidConfig()
		cfg.JWT.SessionMaxLifetime = lifetime
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() should fail for SessionMaxLifetime %s", lifetime)
		}
	}
}

func TestValidate_TenantSlug(t *testing.T) {
	t.Run("valid custom slug", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.Tenant.DefaultSlug = "acme-corp"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() rejected a valid tenant slug: %v", err)
		}
	})

	for _, slug := range []string{"", "Acme", "has space", "under_score", strings.Repeat("a", 64)} {
		t.Run("invalid "+slug, func(t *testing.T) {
			cfg := baseValidConfig()
			cfg.Tenant.DefaultSlug = slug
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() should reject tenant slug %q", slug)
			}
		})
	}
}

func TestLoad_TenantDefaults(t *testing.T) {
	t.Setenv("TENANT_DEFAULT_SLUG", "")
	t.Setenv("TENANT_REQUIRE_HEADER", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Tenant.DefaultSlug != "default" {
		t.Errorf("TENANT_DEFAULT_SLUG = %q, want default", cfg.Tenant.DefaultSlug)
	}
	if cfg.Tenant.RequireHeader {
		t.Error("TENANT_REQUIRE_HEADER should default to false")
	}
}

func TestLoad_TenantRequireHeader(t *testing.T) {
	t.Setenv("TENANT_REQUIRE_HEADER", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if !cfg.Tenant.RequireHeader {
		t.Error("TENANT_REQUIRE_HEADER=true was not parsed")
	}
}

func TestValidate_InvalidBodyLimit(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.BodyLimit = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for BodyLimit < 1")
	}
}

func TestValidate_InvalidRateLimit(t *testing.T) {
	cfg := baseValidConfig()
	cfg.RateLimit.StrictMax = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for StrictMax < 1")
	}
}

func TestValidate_InvalidRateLimitWindow(t *testing.T) {
	cfg := baseValidConfig()
	cfg.RateLimit.StrictWindow = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for StrictWindow < 1")
	}
}

func TestValidate_InvalidMaxFileSize(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage.MaxFileSize = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for MaxFileSize < 1")
	}
}

func TestValidate_InvalidMaxConns(t *testing.T) {
	cfg := baseValidConfig()
	cfg.DB.MaxConns = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for DB_MAX_CONNS < 1")
	}
}

func TestValidate_MinConnsExceedsMax(t *testing.T) {
	cfg := baseValidConfig()
	cfg.DB.MinConns = 30
	cfg.DB.MaxConns = 25
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail when DB_MIN_CONNS > DB_MAX_CONNS")
	}
}

func TestValidate_RedisDriver_MissingURL(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Cache = CacheConfig{Driver: "redis", MemoryMaxEntries: DefaultMemoryMaxEntries}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for redis driver without REDIS_URL")
	}
}

func TestValidate_UnknownCacheDriver(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Cache.Driver = "memcached"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for unknown cache driver")
	}
}

func TestValidate_UnknownEmailDriver(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Email.Driver = "mailgun"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for unknown email driver")
	}
}

func TestValidate_SMTPDriver_MissingHost(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Email = EmailConfig{Driver: "smtp", SMTPPort: 587}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for smtp driver without SMTP_HOST")
	}
}

func TestValidate_GoogleOAuth_MissingSecret(t *testing.T) {
	cfg := baseValidConfig()
	cfg.OAuth.GoogleClientID = "some-id"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail when GoogleClientID set without GoogleClientSecret")
	}
}

func TestValidate_S3Driver_MissingEndpoint(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "s3", MaxFileSize: 10485760}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without endpoint")
	}
}

func TestValidate_LocalDriver_MissingPath(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage.LocalPath = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for local driver without path")
	}
}

func TestValidate_S3Driver_MissingAccessKey(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", MaxFileSize: 10485760}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without access key")
	}
}

func TestValidate_S3Driver_MissingSecretKey(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", S3AccessKey: "key", MaxFileSize: 10485760}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without secret key")
	}
}

func TestValidate_S3Driver_MissingBucket(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", S3AccessKey: "key", S3SecretKey: "secret", MaxFileSize: 10485760}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for s3 driver without bucket")
	}
}

func TestValidate_S3Driver_Success(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "s3", S3Endpoint: "http://localhost:9000", S3AccessKey: "key", S3SecretKey: "secret", S3Bucket: "bucket", MaxFileSize: 10485760}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() should pass for valid s3 config, got: %v", err)
	}
}

func TestValidate_SeaweedFSDriver(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "seaweedfs", S3Endpoint: "seaweedfs:8333", S3AccessKey: "key", S3SecretKey: "secret", S3Bucket: "bucket", MaxFileSize: 10485760}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() should pass for valid seaweedfs config, got: %v", err)
	}
	cfg.Storage.S3Endpoint = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for seaweedfs driver without endpoint")
	}
}

// TestValidate_MinIODriverRemoved pins the breaking rename: STORAGE_DRIVER=minio
// fails startup instead of silently selecting another backend.
func TestValidate_MinIODriverRemoved(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "minio", S3Endpoint: "minio:9000", S3AccessKey: "key", S3SecretKey: "secret", S3Bucket: "bucket", MaxFileSize: 10485760}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should reject the removed minio driver")
	}
}

func TestValidate_UnknownStorageDriver(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Storage = StorageConfig{Driver: "gcs", MaxFileSize: 10485760}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for unknown storage driver")
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

func TestLoadDatabase_IgnoresApplicationSecrets(t *testing.T) {
	// A production migration job must not need JWT/admin/CORS/SMTP secrets.
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", "")
	t.Setenv("ADMIN_EMAIL", "")
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_PORT", "5433")
	t.Setenv("DB_DATABASE", "app")
	t.Setenv("DB_USERNAME", "migrator")
	t.Setenv("DB_PASSWORD", "secret")
	t.Setenv("DB_SSLMODE", "verify-full")
	t.Setenv("DB_CONNECT_TIMEOUT", "10")
	t.Setenv("DB_MAX_CONNS", "5")
	t.Setenv("DB_MIN_CONNS", "1")

	db, err := LoadDatabase()
	if err != nil {
		t.Fatalf("LoadDatabase() returned error: %v", err)
	}
	if db.Host != "db.internal" || db.Database != "app" || db.SSLMode != "verify-full" {
		t.Fatalf("unexpected database config: %+v", db)
	}
}

func TestLoadDatabase_RejectsInvalidPoolSettings(t *testing.T) {
	t.Setenv("DB_MAX_CONNS", "0")
	if _, err := LoadDatabase(); err == nil {
		t.Fatal("LoadDatabase() should reject DB_MAX_CONNS=0")
	}
}

func TestLoadDatabase_RejectsUnverifiedProductionTransport(t *testing.T) {
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("APP_ENV", "Production")
			t.Setenv("DB_SSLMODE", mode)
			t.Setenv("DB_CONNECT_TIMEOUT", "10")
			t.Setenv("DB_MAX_CONNS", "5")
			t.Setenv("DB_MIN_CONNS", "1")
			if _, err := LoadDatabase(); err == nil {
				t.Fatalf("LoadDatabase() should reject DB_SSLMODE=%s in production", mode)
			}
		})
	}
}

func TestLoadDatabase_RejectsUnknownEnvironment(t *testing.T) {
	t.Setenv("APP_ENV", "prod")
	t.Setenv("DB_CONNECT_TIMEOUT", "10")
	t.Setenv("DB_MAX_CONNS", "5")
	t.Setenv("DB_MIN_CONNS", "1")
	if _, err := LoadDatabase(); err == nil {
		t.Fatal("LoadDatabase() should reject an unknown APP_ENV")
	}
}

func TestDBConfig_DSN_EscapesSpecialCharacters(t *testing.T) {
	db := DBConfig{
		Host:     "localhost",
		Port:     5432,
		Database: "test db",
		Username: "user@example.com",
		Password: "p@ss:word/with?chars",
		SSLMode:  "disable",
		Schema:   "public",
	}

	dsn := db.DSN()
	for _, want := range []string{"user%40example.com", "p%40ss%3Aword%2Fwith%3Fchars", "test%20db"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN() = %q, want it to contain %q", dsn, want)
		}
	}
}

func productionValidConfig() Config {
	cfg := baseValidConfig()
	cfg.App.Env = "production"
	cfg.App.FrontendURL = "https://app.example.com"
	cfg.JWT.Secret = strings.Repeat("a", 32)
	cfg.Webhook.SecretEncryptionKey = strings.Repeat("w", 32)
	cfg.DataEncryption.Key = strings.Repeat("d", 32)
	cfg.Storage.AllowedMIMETypes = "image/png,application/pdf"
	cfg.CORS.AllowOrigins = "https://app.example.com"
	cfg.Email = EmailConfig{Driver: "smtp", SMTPHost: "smtp.example.com", SMTPPort: 587, SMTPRequireTLS: true}
	cfg.App.MetricsAuthToken = "metrics-token-123456"
	cfg.DB.SSLMode = "verify-full"
	cfg.DB.BypassUsername = "app_bypass"
	cfg.DB.BypassPassword = "bypass-secret"
	cfg.Tenant.RequireHeader = true
	cfg.Cache = CacheConfig{Driver: "redis", RedisURL: "redis://redis:6379/0", MemoryMaxEntries: DefaultMemoryMaxEntries}
	return cfg
}

func TestValidate_MemoryCacheInProduction(t *testing.T) {
	cfg := productionValidConfig()
	cfg.Cache = CacheConfig{Driver: "memory", MemoryMaxEntries: DefaultMemoryMaxEntries}
	if err := cfg.Validate(); err == nil {
		t.Fatal("production must reject CACHE_DRIVER=memory without the single-replica opt-in")
	}
	cfg.Cache.AllowMemoryInProduction = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("CACHE_ALLOW_MEMORY_IN_PRODUCTION=true should allow memory cache: %v", err)
	}
}

func TestValidate_JSONBodyLimit(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.JSONBodyLimit = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail for APP_JSON_BODY_LIMIT < 1")
	}
	cfg.App.JSONBodyLimit = cfg.App.BodyLimit + 1
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() should fail when APP_JSON_BODY_LIMIT exceeds APP_BODY_LIMIT")
	}
	if got := (AppConfig{}).EffectiveJSONBodyLimit(); got != DefaultJSONBodyLimit {
		t.Errorf("EffectiveJSONBodyLimit() for zero value = %d, want %d", got, DefaultJSONBodyLimit)
	}
}

func TestValidate_ProductionSafetyRails(t *testing.T) {
	t.Run("valid production config", func(t *testing.T) {
		cfg := productionValidConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("production config should be valid: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"body limit below file limit", func(c *Config) { c.App.BodyLimit = 1024 }},
		{"empty MIME allowlist", func(c *Config) { c.Storage.AllowedMIMETypes = "" }},
		{"console email", func(c *Config) { c.Email = EmailConfig{Driver: "console"} }},
		{"smtp without TLS", func(c *Config) { c.Email.SMTPRequireTLS = false }},
		{"wildcard CORS", func(c *Config) { c.CORS.AllowOrigins = "*" }},
		{"http frontend", func(c *Config) { c.App.FrontendURL = "http://app.example.com" }},
		{"refresh expiry zero", func(c *Config) { c.JWT.RefreshExpireDays = 0 }},
		{"session lifetime zero", func(c *Config) { c.JWT.SessionMaxLifetime = 0 }},
		{"invalid env", func(c *Config) { c.App.Env = "prod" }},
		{"missing metrics token", func(c *Config) { c.App.MetricsAuthToken = "" }},
		{"tenant header not required", func(c *Config) { c.Tenant.RequireHeader = false }},
		{"weak admin password", func(c *Config) {
			c.Admin = AdminConfig{Email: "admin@example.com", Password: "Admin123!"}
		}},
		{"known default JWT secret", func(c *Config) {
			c.JWT.Secret = "your-super-secret-key-change-in-production"
		}},
		{"db ssl require", func(c *Config) { c.DB.SSLMode = "require" }},
		{"db ssl verify-ca", func(c *Config) { c.DB.SSLMode = "verify-ca" }},
		{"db ssl disabled", func(c *Config) { c.DB.SSLMode = "disable" }},
		{"db ssl disabled uppercase", func(c *Config) { c.DB.SSLMode = "DISABLE" }},
		{"db ssl allow", func(c *Config) { c.DB.SSLMode = "allow" }},
		{"db ssl prefer", func(c *Config) { c.DB.SSLMode = "Prefer" }},
		{"s3 without ssl", func(c *Config) {
			c.Storage = StorageConfig{
				Driver: "s3", S3Endpoint: "s3.amazonaws.com", S3AccessKey: "ak", S3SecretKey: "sk",
				S3Bucket: "uploads", MaxFileSize: 10485760, AllowedMIMETypes: "image/png",
			}
		}},
		{"request timeout beyond shutdown drain", func(c *Config) { c.App.RequestTimeout = MaxRequestTimeoutSeconds + 1 }},
		{"empty cors origins", func(c *Config) { c.CORS.AllowOrigins = "" }},
		{"malformed cors origin", func(c *Config) { c.CORS.AllowOrigins = "not-a-url" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := productionValidConfig()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate() should reject this production config")
			}
		})
	}
}

func TestValidate_ProductionTLSAccepted(t *testing.T) {
	for _, mode := range []string{"verify-full", "VERIFY-FULL"} {
		cfg := productionValidConfig()
		cfg.DB.SSLMode = mode
		if err := cfg.Validate(); err != nil {
			t.Errorf("DB_SSLMODE=%s should be accepted in production: %v", mode, err)
		}
	}

	for _, mode := range []string{"require", "verify-ca"} {
		cfg := productionValidConfig()
		cfg.DB.SSLMode = mode
		if err := cfg.Validate(); err == nil {
			t.Errorf("DB_SSLMODE=%s does not verify the server certificate and must be rejected in production", mode)
		}
	}

	cfg := productionValidConfig()
	cfg.Storage = StorageConfig{
		Driver: "seaweedfs", S3Endpoint: "seaweedfs:8333", S3AccessKey: "ak", S3SecretKey: "sk",
		S3Bucket: "uploads", MaxFileSize: 10485760, AllowedMIMETypes: "image/png",
		S3AllowInsecure: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("STORAGE_S3_ALLOW_INSECURE=true should permit plaintext storage: %v", err)
	}
	cfg.Storage.S3AllowInsecure = false
	cfg.Storage.S3UseSSL = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("STORAGE_S3_USE_SSL=true should be accepted in production: %v", err)
	}
}

func TestValidate_RequestTimeoutFitsShutdownBudget(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.RequestTimeout = MaxRequestTimeoutSeconds
	if err := cfg.Validate(); err != nil {
		t.Fatalf("APP_REQUEST_TIMEOUT=%d should be valid: %v", MaxRequestTimeoutSeconds, err)
	}
	cfg.App.RequestTimeout = MaxRequestTimeoutSeconds + 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("APP_REQUEST_TIMEOUT beyond the shutdown drain window must be rejected")
	}
	if time.Duration(MaxRequestTimeoutSeconds)*time.Second+PostHandlerReserve+ShutdownReserve > ShutdownBudget {
		t.Fatal("the maximum request timeout plus the reserve must fit the shutdown budget")
	}
}

func TestLoad_CacheRedisDefaults(t *testing.T) {
	var c CacheConfig
	if err := env.Parse(&c); err != nil {
		t.Fatalf("parse cache defaults: %v", err)
	}
	if c.RedisDialTimeout() != 2*time.Second || c.RedisReadTimeout() != 500*time.Millisecond ||
		c.RedisWriteTimeout() != 500*time.Millisecond || c.RedisPoolTimeout() != time.Second || c.RedisMaxRetries != 1 {
		t.Fatalf("unexpected Redis defaults: %+v", c)
	}
	if c.MemoryMaxEntries != DefaultMemoryMaxEntries {
		t.Fatalf("MemoryMaxEntries = %d, want %d", c.MemoryMaxEntries, DefaultMemoryMaxEntries)
	}
	if (CacheConfig{}).RedisReadTimeout() != DefaultRedisReadTimeout {
		t.Fatal("a zero Redis timeout must fall back to the package default")
	}
}

func TestValidate_CacheMemoryMaxEntries(t *testing.T) {
	for _, value := range []int{0, -1} {
		cfg := baseValidConfig()
		cfg.Cache.MemoryMaxEntries = value
		if err := cfg.Validate(); err == nil {
			t.Fatalf("CACHE_MEMORY_MAX_ENTRIES=%d must be rejected", value)
		}
	}
	cfg := baseValidConfig()
	cfg.Cache.MemoryMaxEntries = 1
	if err := cfg.Validate(); err != nil {
		t.Fatalf("CACHE_MEMORY_MAX_ENTRIES=1 should be valid: %v", err)
	}
}

func TestValidate_CacheRedisBounds(t *testing.T) {
	for name, mutate := range map[string]func(*CacheConfig){
		"negative timeout": func(c *CacheConfig) { c.RedisReadTimeoutMS = -1 },
		"negative retries": func(c *CacheConfig) { c.RedisMaxRetries = -1 },
		"too many retries": func(c *CacheConfig) { c.RedisMaxRetries = 11 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseValidConfig()
			mutate(&cfg.Cache)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() should reject this cache config")
			}
		})
	}
}

func TestLoad_DefaultsIncludePatchAndFitShutdown(t *testing.T) {
	var parsed struct {
		App  AppConfig
		CORS CORSConfig
	}
	if err := env.Parse(&parsed); err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if !contains(parsed.CORS.Methods(), "PATCH") {
		t.Errorf("default CORS_ALLOW_METHODS = %q, want PATCH included", parsed.CORS.AllowMethods)
	}
	if parsed.App.RequestTimeout > MaxRequestTimeoutSeconds {
		t.Errorf("default APP_REQUEST_TIMEOUT %d exceeds %d", parsed.App.RequestTimeout, MaxRequestTimeoutSeconds)
	}
}

func TestLoad_JWTDefaults(t *testing.T) {
	var parsed struct{ JWT JWTConfig }
	if err := env.Parse(&parsed); err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if parsed.JWT.ExpireHour != 1 {
		t.Errorf("default JWT_EXPIRE_HOUR = %d, want 1", parsed.JWT.ExpireHour)
	}
	if parsed.JWT.SessionMaxLifetime != 720*time.Hour {
		t.Errorf("default SESSION_MAX_LIFETIME = %s, want 720h", parsed.JWT.SessionMaxLifetime)
	}
}

func TestValidate_StagingRejectsKnownDefaults(t *testing.T) {
	cfg := baseValidConfig()
	cfg.App.Env = "staging"
	cfg.JWT.Secret = "your-super-secret-key-change-in-production"
	if err := cfg.Validate(); err == nil {
		t.Fatal("staging must reject the published sample JWT secret")
	}

	cfg.JWT.Secret = strings.Repeat("b", 32)
	cfg.Admin = AdminConfig{Email: "admin@example.com", Password: "Admin123!"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("staging must reject the default admin password")
	}
}

func TestFeatureFlagsConfigDefaults(t *testing.T) {
	var c FeatureFlagsConfig
	if err := env.Parse(&c); err != nil {
		t.Fatalf("parse feature flag defaults: %v", err)
	}
	if c.CacheTTL() != time.Duration(c.CacheTTLSeconds)*time.Second {
		t.Fatalf("CacheTTL() = %v, want %d seconds", c.CacheTTL(), c.CacheTTLSeconds)
	}
	if c.MaxStaleness() < c.CacheTTL() {
		t.Fatalf("MaxStaleness() = %v, must be >= CacheTTL() %v", c.MaxStaleness(), c.CacheTTL())
	}
}

func TestFeatureFlagsConfigParsedValues(t *testing.T) {
	c := FeatureFlagsConfig{Flags: "user_registration=false,google_oauth=true", ForceOff: "file_uploads"}
	values := c.EnvValues()
	if values["user_registration"] || !values["google_oauth"] {
		t.Fatalf("EnvValues() = %+v", values)
	}
	forceOff := c.ForceOffValues()
	if _, ok := forceOff["file_uploads"]; !ok {
		t.Fatalf("ForceOffValues() = %+v", forceOff)
	}
}

// TestFeatureFlagsValidateKnownKeys covers the post-registration check. Load
// cannot know module-owned keys, so unknown keys pass Validate and are
// rejected here once the catalog is complete.
func TestFeatureFlagsValidateKnownKeys(t *testing.T) {
	if err := features.Register("configtest", features.Definition{
		Key: "configtest_flag", Description: "config test flag", Default: true,
	}); err != nil {
		t.Fatal(err)
	}
	valid := FeatureFlagsConfig{Flags: "configtest_flag=false", ForceOff: "configtest_flag"}
	if err := valid.ValidateKnownKeys(); err != nil {
		t.Fatalf("registered keys rejected: %v", err)
	}
	for name, cfg := range map[string]FeatureFlagsConfig{
		"unknown flag":      {Flags: "typo=true"},
		"unknown force-off": {ForceOff: "typo"},
		"malformed flag":    {Flags: "configtest_flag="},
	} {
		t.Run(name, func(t *testing.T) {
			if err := cfg.ValidateKnownKeys(); err == nil {
				t.Error("ValidateKnownKeys() should reject this config")
			}
		})
	}

	syntaxOnly := baseValidConfig()
	syntaxOnly.FeatureFlags.Flags = "typo=true"
	if err := syntaxOnly.Validate(); err != nil {
		t.Fatalf("Validate must only check syntax (keys are registered later): %v", err)
	}
}

func TestValidate_FeatureFlags(t *testing.T) {
	t.Run("valid flags", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.FeatureFlags = FeatureFlagsConfig{
			Flags:               "user_registration=false,email_verification=true",
			ForceOff:            "google_oauth",
			CacheTTLSeconds:     30,
			MaxStalenessSeconds: 300,
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid feature flags rejected: %v", err)
		}
	})

	t.Run("zero timings use defaults", func(t *testing.T) {
		cfg := baseValidConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("zero feature flag timings should be valid: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"malformed flag", func(c *Config) { c.FeatureFlags.Flags = "user_registration=" }},
		{"duplicate flag", func(c *Config) { c.FeatureFlags.Flags = "file_uploads=true,file_uploads=false" }},
		{"negative TTL", func(c *Config) { c.FeatureFlags.CacheTTLSeconds = -1 }},
		{"negative staleness", func(c *Config) { c.FeatureFlags.MaxStalenessSeconds = -1 }},
		{"staleness below TTL", func(c *Config) {
			c.FeatureFlags.CacheTTLSeconds = 60
			c.FeatureFlags.MaxStalenessSeconds = 30
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseValidConfig()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate() should reject this feature flag config")
			}
		})
	}
}
