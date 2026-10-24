package config

import "fmt"

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

// validate requires positive per-user tiers; the Auth tier may be zero (use
// the defaults) but not negative.
func (r RateLimitConfig) validate() error {
	if r.StrictMax < 1 || r.NormalMax < 1 || r.RelaxedMax < 1 {
		return fmt.Errorf("all RATE_LIMIT_*_MAX values must be at least 1")
	}
	if r.StrictWindow < 1 || r.NormalWindow < 1 || r.RelaxedWindow < 1 {
		return fmt.Errorf("all RATE_LIMIT_*_WINDOW_SECS values must be at least 1")
	}
	if r.AuthMax < 0 || r.AuthWindow < 0 {
		return fmt.Errorf("RATE_LIMIT_AUTH_MAX and RATE_LIMIT_AUTH_WINDOW_SECS must not be negative")
	}
	return nil
}
