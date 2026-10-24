package config

import (
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
)

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

// ForceOffValues parses FEATURE_FLAGS_FORCE_OFF.
func (f FeatureFlagsConfig) ForceOffValues() map[features.Key]struct{} {
	values, err := features.ParseForceOff(f.ForceOff)
	if err != nil {
		return map[features.Key]struct{}{}
	}
	return values
}

// validate rejects malformed or duplicate entries and negative cache bounds.
// Unknown keys are rejected by ValidateKnownKeys, which cmd/api calls after
// modules register their flags (the catalog is not complete yet when config
// loads). Zero cache values mean "use the package defaults".
func (f FeatureFlagsConfig) validate() error {
	if _, err := features.ParseFlags(f.Flags); err != nil {
		return fmt.Errorf("FEATURE_FLAGS: %w", err)
	}
	if _, err := features.ParseForceOff(f.ForceOff); err != nil {
		return fmt.Errorf("FEATURE_FLAGS_FORCE_OFF: %w", err)
	}
	if f.CacheTTLSeconds < 0 {
		return fmt.Errorf("FEATURE_FLAGS_CACHE_TTL_SECONDS must not be negative")
	}
	if f.MaxStalenessSeconds < 0 {
		return fmt.Errorf("FEATURE_FLAGS_MAX_STALENESS_SECONDS must not be negative")
	}
	if f.CacheTTLSeconds > 0 && f.MaxStalenessSeconds > 0 && f.MaxStalenessSeconds < f.CacheTTLSeconds {
		return fmt.Errorf(
			"FEATURE_FLAGS_MAX_STALENESS_SECONDS (%d) must be at least FEATURE_FLAGS_CACHE_TTL_SECONDS (%d)",
			f.MaxStalenessSeconds, f.CacheTTLSeconds,
		)
	}
	return nil
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
