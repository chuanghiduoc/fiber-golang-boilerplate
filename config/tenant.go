package config

import (
	"fmt"
	"regexp"
)

// TenantConfig controls request-tenant resolution. The default slug is the
// seeded tenant used when a request does not send X-Tenant-Slug in local/test.
// Production validation requires RequireHeader so an HTTP write cannot land in
// the default tenant by omission.
type TenantConfig struct {
	DefaultSlug   string `env:"TENANT_DEFAULT_SLUG" envDefault:"default"`
	RequireHeader bool   `env:"TENANT_REQUIRE_HEADER" envDefault:"false"`
}

var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// validate keeps the default slug DNS-safe: slugs are external identifiers.
func (t TenantConfig) validate() error {
	if !tenantSlugPattern.MatchString(t.DefaultSlug) {
		return fmt.Errorf("TENANT_DEFAULT_SLUG must match [a-z0-9-]{1,63} (got %q)", t.DefaultSlug)
	}
	return nil
}

// validateProduction makes tenant selection explicit for every HTTP write:
// header-less requests must not silently select the default tenant, and
// links then carry a known slug.
func (t TenantConfig) validateProduction() error {
	if !t.RequireHeader {
		return fmt.Errorf("TENANT_REQUIRE_HEADER must be true in production so header-less writes cannot land in the default tenant")
	}
	return nil
}
