package config

import (
	"fmt"
	"time"
)

// APIKeysConfig bounds API-key credential lifetime (OWASP API2:2023).
// MaxLifetime caps every newly issued key: an omitted expiresAt becomes
// now + MaxLifetime, a later expiresAt is rejected, and rotation never extends
// an existing key's expiry beyond it. Existing rows with NULL expiry keep
// working until they are rotated or revoked.
type APIKeysConfig struct {
	MaxLifetime time.Duration `env:"APIKEYS_MAX_LIFETIME" envDefault:"8760h"`
}

// validate requires a positive maximum lifetime.
func (a APIKeysConfig) validate() error {
	if a.MaxLifetime <= 0 {
		return fmt.Errorf("APIKEYS_MAX_LIFETIME must be greater than 0")
	}
	return nil
}
