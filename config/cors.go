package config

import "fmt"

// corsWildcardOrigin allows every origin; it cannot be combined with
// credentials and is rejected in production.
const corsWildcardOrigin = "*"

type CORSConfig struct {
	AllowOrigins     string `env:"CORS_ALLOW_ORIGINS" envDefault:"*"`
	AllowMethods     string `env:"CORS_ALLOW_METHODS" envDefault:"GET,POST,PUT,PATCH,DELETE,OPTIONS"`
	AllowHeaders     string `env:"CORS_ALLOW_HEADERS" envDefault:"Origin,Content-Type,Accept,Accept-Language,Accept-Version,Authorization,Idempotency-Key,X-Tenant-Slug"`
	ExposeHeaders    string `env:"CORS_EXPOSE_HEADERS" envDefault:"Location,ETag,Idempotent-Replayed,Content-Language,API-Version,Deprecation,Sunset,Link,RateLimit,RateLimit-Policy,Retry-After,X-RateLimit-Limit,X-RateLimit-Remaining,X-RateLimit-Reset,X-Request-Id"`
	AllowCredentials bool   `env:"CORS_ALLOW_CREDENTIALS" envDefault:"false"`
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

// validate checks every origin explicitly: Fiber treats an empty origin list
// as allow-all and can panic on malformed origins.
func (c CORSConfig) validate() error {
	origins := c.Origins()
	if len(origins) == 0 {
		return fmt.Errorf("CORS_ALLOW_ORIGINS must not be empty")
	}
	for _, origin := range origins {
		if origin == corsWildcardOrigin {
			if c.AllowCredentials {
				return fmt.Errorf("CORS_ALLOW_CREDENTIALS cannot be true when CORS_ALLOW_ORIGINS contains '*'")
			}
			continue
		}
		if !isAbsoluteHTTPURL(origin) {
			return fmt.Errorf("CORS_ALLOW_ORIGINS contains invalid origin %q", origin)
		}
	}
	return nil
}

// validateProduction rejects the wildcard origin.
func (c CORSConfig) validateProduction() error {
	for _, origin := range c.Origins() {
		if origin == corsWildcardOrigin {
			return fmt.Errorf("CORS_ALLOW_ORIGINS must not contain '*' in production")
		}
	}
	return nil
}
