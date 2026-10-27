package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
)

// FeatureFlagsConfig controls environment overrides, emergency force-off, and
// the in-process snapshot cache used by internal/platform/features.
type FeatureFlagsConfig struct {
	// Flags is a key=true|false list; a bare key means true, e.g.
	// "user_registration,email_verification=false".
	Flags []string `env:"FEATURE_FLAGS" envSeparator:","`
	// ForceOff lists keys that always evaluate false, even when a database
	// default or user/tenant target enables them.
	ForceOff []string `env:"FEATURE_FLAGS_FORCE_OFF" envSeparator:","`
	// CacheTTL is how long a loaded snapshot is considered fresh.
	CacheTTL time.Duration `env:"FEATURE_FLAGS_CACHE_TTL" envDefault:"30s"`
	// MaxStaleness bounds how long the last good snapshot may be used while
	// the database is unavailable.
	MaxStaleness time.Duration `env:"FEATURE_FLAGS_MAX_STALENESS" envDefault:"5m"`
}

// EffectiveCacheTTL returns the configured snapshot TTL, applying the package
// default for zero values.
func (f FeatureFlagsConfig) EffectiveCacheTTL() time.Duration {
	if f.CacheTTL <= 0 {
		return features.DefaultCacheTTL
	}
	return f.CacheTTL
}

// EffectiveMaxStaleness returns the configured stale-snapshot bound. It is
// never lower than the cache TTL.
func (f FeatureFlagsConfig) EffectiveMaxStaleness() time.Duration {
	maxStaleness := features.DefaultMaxStaleness
	if f.MaxStaleness > 0 {
		maxStaleness = f.MaxStaleness
	}
	if ttl := f.EffectiveCacheTTL(); maxStaleness < ttl {
		maxStaleness = ttl
	}
	return maxStaleness
}

// EnvValues parses FEATURE_FLAGS. Config validation has already rejected
// malformed input in the normal Load path; invalid values here degrade to an
// empty map so a partially built Config in tests cannot panic.
func (f FeatureFlagsConfig) EnvValues() map[features.Key]bool {
	values, err := features.ParseFlags(strings.Join(f.Flags, ","))
	if err != nil {
		return map[features.Key]bool{}
	}
	return values
}

// ForceOffValues parses FEATURE_FLAGS_FORCE_OFF.
func (f FeatureFlagsConfig) ForceOffValues() map[features.Key]struct{} {
	values, err := features.ParseForceOff(strings.Join(f.ForceOff, ","))
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
	if _, err := features.ParseFlags(strings.Join(f.Flags, ",")); err != nil {
		return fmt.Errorf("FEATURE_FLAGS: %w", err)
	}
	if _, err := features.ParseForceOff(strings.Join(f.ForceOff, ",")); err != nil {
		return fmt.Errorf("FEATURE_FLAGS_FORCE_OFF: %w", err)
	}
	if f.CacheTTL < 0 {
		return fmt.Errorf("FEATURE_FLAGS_CACHE_TTL must not be negative")
	}
	if f.MaxStaleness < 0 {
		return fmt.Errorf("FEATURE_FLAGS_MAX_STALENESS must not be negative")
	}
	if f.CacheTTL > 0 && f.MaxStaleness > 0 && f.MaxStaleness < f.CacheTTL {
		return fmt.Errorf(
			"FEATURE_FLAGS_MAX_STALENESS (%s) must be at least FEATURE_FLAGS_CACHE_TTL (%s)",
			f.MaxStaleness, f.CacheTTL,
		)
	}
	return nil
}

// ValidateKnownKeys rejects FEATURE_FLAGS / FEATURE_FLAGS_FORCE_OFF entries
// that are not in the injected flag catalog, so a typo cannot silently
// disable a feature. Pass the catalog the composition root built from every
// FlagProvider; Validate only checks syntax.
func (f FeatureFlagsConfig) ValidateKnownKeys(catalog *features.Catalog) error {
	if catalog == nil {
		return fmt.Errorf("FEATURE_FLAGS: no feature-flag catalog")
	}
	parsed, err := features.ParseFlags(strings.Join(f.Flags, ","))
	if err != nil {
		return fmt.Errorf("FEATURE_FLAGS: %w", err)
	}
	if err := catalog.ValidateFlags(parsed); err != nil {
		return fmt.Errorf("FEATURE_FLAGS: %w", err)
	}
	forceOff, err := features.ParseForceOff(strings.Join(f.ForceOff, ","))
	if err != nil {
		return fmt.Errorf("FEATURE_FLAGS_FORCE_OFF: %w", err)
	}
	if err := catalog.ValidateForceOff(forceOff); err != nil {
		return fmt.Errorf("FEATURE_FLAGS_FORCE_OFF: %w", err)
	}
	return nil
}
