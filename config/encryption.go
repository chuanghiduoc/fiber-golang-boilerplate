package config

import (
	"fmt"
	"slices"
	"strings"
)

// minDataEncryptionKeyLength is the shortest DATA_ENCRYPTION_KEY (and
// previous key outside local/test) accepted.
const minDataEncryptionKeyLength = 32

// DataEncryptionConfig holds the key that seals job payloads at rest (email
// and password-reset jobs). It is separate from JWT_SECRET so signing and
// encryption keys rotate independently.
//
// Outside production an empty Key falls back to JWT_SECRET, which is the key
// every release before DATA_ENCRYPTION_KEY used. When switching an existing
// deployment to a dedicated key, list the old JWT_SECRET in PreviousKeys until
// every job sealed with it has been delivered or dead-lettered.
type DataEncryptionConfig struct {
	Key string `env:"DATA_ENCRYPTION_KEY"`
	// PreviousKeys are comma-separated decryption-only keys kept during a
	// rotation.
	PreviousKeys string `env:"DATA_ENCRYPTION_PREVIOUS_KEYS"`
}

// ResolvedKey returns the dedicated key, or the JWT secret as the
// local/development fallback (production requires the dedicated key).
func (d DataEncryptionConfig) ResolvedKey(jwtSecret string) string {
	if key := strings.TrimSpace(d.Key); key != "" {
		return key
	}
	return jwtSecret
}

// PreviousKeyList returns the parsed DATA_ENCRYPTION_PREVIOUS_KEYS entries.
func (d DataEncryptionConfig) PreviousKeyList() []string {
	return splitCSV(d.PreviousKeys)
}

// validate checks key strength everywhere and, in production, requires a
// dedicated key that differs from the JWT signing secret.
func (d DataEncryptionConfig) validate(appEnv, jwtSecret string) error {
	key := strings.TrimSpace(d.Key)
	if key != "" && len(key) < minDataEncryptionKeyLength {
		return fmt.Errorf("DATA_ENCRYPTION_KEY must be at least %d characters", minDataEncryptionKeyLength)
	}
	if appEnv == envProduction {
		if key == "" {
			return fmt.Errorf("DATA_ENCRYPTION_KEY must be set in production")
		}
		if key == jwtSecret {
			return fmt.Errorf("DATA_ENCRYPTION_KEY must differ from JWT_SECRET")
		}
	}
	previous := d.PreviousKeyList()
	if slices.Contains(previous, d.ResolvedKey(jwtSecret)) {
		return fmt.Errorf("DATA_ENCRYPTION_PREVIOUS_KEYS must not contain the active encryption key")
	}
	if !usesSampleSecrets(appEnv) {
		for _, p := range previous {
			if len(p) < minDataEncryptionKeyLength {
				return fmt.Errorf("DATA_ENCRYPTION_PREVIOUS_KEYS entries must be at least %d characters", minDataEncryptionKeyLength)
			}
		}
	}
	return nil
}
