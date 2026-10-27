package config

import (
	"fmt"
	"time"
)

type RateLimitConfig struct {
	StrictMax     int           `env:"RATE_LIMIT_STRICT_MAX" envDefault:"5"`
	StrictWindow  time.Duration `env:"RATE_LIMIT_STRICT_WINDOW" envDefault:"1m"`
	NormalMax     int           `env:"RATE_LIMIT_NORMAL_MAX" envDefault:"60"`
	NormalWindow  time.Duration `env:"RATE_LIMIT_NORMAL_WINDOW" envDefault:"1m"`
	RelaxedMax    int           `env:"RATE_LIMIT_RELAXED_MAX" envDefault:"120"`
	RelaxedWindow time.Duration `env:"RATE_LIMIT_RELAXED_WINDOW" envDefault:"1m"`
	// Auth is the per-client-IP budget checked before authentication on every
	// protected route, so bogus credentials cannot hit the API-key store
	// unthrottled. Every request from one IP (NAT included) counts, so it is
	// generous; the per-user tiers behind it stay the real quota. Zero uses
	// the defaults below.
	AuthMax    int           `env:"RATE_LIMIT_AUTH_MAX" envDefault:"300"`
	AuthWindow time.Duration `env:"RATE_LIMIT_AUTH_WINDOW" envDefault:"1m"`
}

// Pre-authentication limiter defaults for RateLimitConfig literals that leave
// the Auth tier unset (tests and tools); Load fills the env defaults.
const (
	DefaultRateLimitAuthMax    = 300
	DefaultRateLimitAuthWindow = time.Minute
)

// EffectiveAuth returns the pre-authentication limit and window in seconds.
func (r RateLimitConfig) EffectiveAuth() (maxRequests, windowSecs int) {
	maxRequests, window := r.AuthMax, r.AuthWindow
	if maxRequests <= 0 {
		maxRequests = DefaultRateLimitAuthMax
	}
	if window <= 0 {
		window = DefaultRateLimitAuthWindow
	}
	return maxRequests, int(window / time.Second)
}

// validate requires positive per-user tiers; the Auth tier may be zero (use
// the defaults) but not negative.
func (r RateLimitConfig) validate() error {
	if r.StrictMax < 1 || r.NormalMax < 1 || r.RelaxedMax < 1 {
		return fmt.Errorf("all RATE_LIMIT_*_MAX values must be at least 1")
	}
	if r.StrictWindow < time.Second || r.NormalWindow < time.Second || r.RelaxedWindow < time.Second {
		return fmt.Errorf("all RATE_LIMIT_*_WINDOW values must be at least 1s")
	}
	if r.AuthMax < 0 || r.AuthWindow < 0 {
		return fmt.Errorf("RATE_LIMIT_AUTH_MAX and RATE_LIMIT_AUTH_WINDOW must not be negative")
	}
	return nil
}
