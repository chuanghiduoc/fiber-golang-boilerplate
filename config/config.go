// Package config loads and validates process configuration from environment
// variables. Each concern (app, database, cache, storage, ...) lives in its own
// file and owns its struct, env tags, accessors, and validation rules; this
// file only orchestrates loading and the order in which sections validate.
package config

import (
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	App          AppConfig
	DB           DBConfig
	JWT          JWTConfig
	Storage      StorageConfig
	Files        FilesConfig
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
	// APIKeys bounds API-key credential lifetime.
	APIKeys APIKeysConfig
	// Retention holds module purge windows (files trash, read notifications).
	Retention RetentionConfig
	// DataEncryption seals job payloads at rest.
	DataEncryption DataEncryptionConfig
	Jobs           JobsConfig
	// Password holds the new-password policy toggles.
	Password PasswordConfig
	// MFA bounds how long a second-factor verification stays valid.
	MFA MFAConfig
}

func Load() (*Config, error) {
	environment, err := loadEnvironment()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	cfg := &Config{}
	if err := env.ParseWithOptions(cfg, env.Options{Environment: environment}); err != nil {
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
	environment, err := loadEnvironment()
	if err != nil {
		return DBConfig{}, fmt.Errorf("failed to load database config: %w", err)
	}
	if err := env.ParseWithOptions(&parsed, env.Options{Environment: environment}); err != nil {
		return DBConfig{}, fmt.Errorf("failed to parse database config: %w", err)
	}
	if err := parsed.DB.Validate(); err != nil {
		return DBConfig{}, fmt.Errorf("invalid database config: %w", err)
	}
	appEnv := strings.ToLower(strings.TrimSpace(parsed.App.Env))
	if err := validateAppEnv(appEnv, parsed.App.Env); err != nil {
		return DBConfig{}, err
	}
	// Migration jobs skip the full application validation, but must not
	// silently downgrade transport security relative to the API.
	if appEnv == envProduction {
		if err := validateProductionSSLMode(parsed.DB.SSLMode); err != nil {
			return DBConfig{}, err
		}
	}
	return parsed.DB, nil
}

// LoadSeed loads the app environment, database, and one-shot admin settings.
// It deliberately skips the application-wide validation Load performs: the seed
// Job mounts the database credentials from the app Secret and ADMIN_* from its
// own Secret, so it must run without JWT, SMTP, metrics, or storage secrets. It
// still validates the database transport and the admin credentials the seed
// command applies.
func LoadSeed() (*Config, error) {
	environment, err := loadEnvironmentFor(seedSecretFileVars)
	if err != nil {
		return nil, fmt.Errorf("failed to load seed config: %w", err)
	}
	cfg := &Config{}
	if err := env.ParseWithOptions(cfg, env.Options{Environment: environment}); err != nil {
		return nil, fmt.Errorf("failed to parse seed config: %w", err)
	}
	appEnv := strings.ToLower(strings.TrimSpace(cfg.App.Env))
	if err := validateAppEnv(appEnv, cfg.App.Env); err != nil {
		return nil, err
	}
	if err := cfg.DB.Validate(); err != nil {
		return nil, fmt.Errorf("invalid database config: %w", err)
	}
	// Seeding skips application validation, but must not silently downgrade
	// the database transport security relative to the API.
	if appEnv == envProduction {
		if err := validateProductionSSLMode(cfg.DB.SSLMode); err != nil {
			return nil, err
		}
	}
	if err := cfg.Admin.validate(cfg.App.Env); err != nil {
		return nil, fmt.Errorf("invalid admin config: %w", err)
	}
	return cfg, nil
}

// Validate checks every section and returns the first violation. The order is
// part of the contract: when several settings are wrong, operators see the
// same first error as before, so the steps below are listed in the order they
// run rather than grouped by section.
func (cfg *Config) Validate() error {
	checks := []func() error{
		cfg.App.validate,
		// The sample JWT secret is public and must never sign tokens outside
		// local/test, so secrets are checked before any other section.
		func() error { return cfg.JWT.validateSigningSecret(cfg.App.Env) },
		func() error { return cfg.Admin.validate(cfg.App.Env) },
		cfg.JWT.validate,
		cfg.Tenant.validate,
		cfg.FeatureFlags.validate,
		func() error { return cfg.Webhook.validate(cfg.App.Env, cfg.JWT.Secret) },
		cfg.APIKeys.validate,
		func() error { return cfg.DataEncryption.validate(cfg.App.Env, cfg.JWT.Secret) },
		cfg.Jobs.validate,
		cfg.App.validateBodyLimits,
		cfg.RateLimit.validate,
		cfg.Storage.validateMaxFileSize,
		cfg.Files.validate,
		cfg.DB.Validate,
		// Observability: tracing is inert unless explicitly enabled.
		cfg.Tracing.Validate,
		cfg.Scheduler.Validate,
		cfg.Retention.validate,
		cfg.Cache.Validate,
		cfg.Email.validate,
		cfg.CORS.validate,
		cfg.App.validateTrustedProxies,
		cfg.OAuth.validate,
		cfg.Password.validate,
		cfg.MFA.validate,
		cfg.Storage.validateDriver,
	}
	if cfg.App.Env == envProduction {
		checks = append(checks, cfg.productionChecks()...)
	}
	return firstError(checks)
}

// productionChecks are safety rails that would break local development
// defaults, so they run only for APP_ENV=production, after every general check.
func (cfg *Config) productionChecks() []func() error {
	return []func() error{
		cfg.Tenant.validateProduction,
		func() error { return cfg.App.validateProductionBodyLimit(cfg.Storage.MaxFileSize) },
		cfg.Cache.validateProduction,
		cfg.Storage.validateProductionMIMETypes,
		cfg.Email.validateProduction,
		cfg.CORS.validateProduction,
		cfg.App.validateProductionFrontendURL,
		func() error { return validateProductionSSLMode(cfg.DB.SSLMode) },
		cfg.DB.validateProduction,
		cfg.Storage.validateProductionTransport,
		cfg.OAuth.validateProduction,
		cfg.App.validateProductionMetricsToken,
	}
}

// firstError runs checks in order and returns the first failure.
func firstError(checks []func() error) error {
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
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
