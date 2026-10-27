package config

import (
	"fmt"
	"slices"
	"strings"
)

// DataEncryptionConfig holds the key that seals job payloads at rest (email
// and password-reset jobs) and the keyed ban marker. It is separate from
// JWT_SECRET so signing and encryption keys rotate independently.
//
// There is no fallback to JWT_SECRET in any environment: one key per purpose
// (NIST SP 800-57). The key is base64-encoded 32 random bytes.
type DataEncryptionConfig struct {
	Key string `env:"DATA_ENCRYPTION_KEY"`
	// PreviousKeys are comma-separated decryption-only keys kept during a
	// rotation.
	PreviousKeys []string `env:"DATA_ENCRYPTION_PREVIOUS_KEYS" envSeparator:","`
}

// ResolvedKey returns the dedicated key. Validation guarantees it is set.
func (d DataEncryptionConfig) ResolvedKey() string {
	return strings.TrimSpace(d.Key)
}

// PreviousKeyList returns the parsed DATA_ENCRYPTION_PREVIOUS_KEYS entries.
func (d DataEncryptionConfig) PreviousKeyList() []string {
	return trimmedList(d.PreviousKeys)
}

// validate requires a dedicated, base64-encoded 32-byte key in every
// environment and checks the decrypt-only previous keys.
func (d DataEncryptionConfig) validate() error {
	if err := ValidateKey("DATA_ENCRYPTION_KEY", d.Key); err != nil {
		return err
	}
	previous := d.PreviousKeyList()
	if slices.Contains(previous, d.ResolvedKey()) {
		return fmt.Errorf("DATA_ENCRYPTION_PREVIOUS_KEYS must not contain the active encryption key")
	}
	for _, p := range previous {
		if err := ValidateKey("DATA_ENCRYPTION_PREVIOUS_KEYS", p); err != nil {
			return err
		}
	}
	return nil
}
