package config

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// SecretKeyBytes is the required decoded length of every application key:
// 32 random bytes (AES-256/HMAC-SHA256 strength per NIST SP 800-57).
const SecretKeyBytes = 32

// ValidateKey checks that a key setting is base64-encoded 32 random bytes.
// The name is the environment variable, so the startup error tells the
// operator which setting is wrong.
func ValidateKey(name, value string) error {
	_, err := DecodeKey(name, value)
	return err
}

// DecodeKey returns the raw 32 bytes behind a base64 key setting.
func DecodeKey(name, value string) ([]byte, error) {
	encoded := strings.TrimSpace(value)
	if encoded == "" {
		return nil, fmt.Errorf("%s must be set", name)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, fmt.Errorf("%s must be base64-encoded 32 random bytes", name)
	}
	if len(raw) != SecretKeyBytes {
		return nil, fmt.Errorf("%s must decode to exactly %d bytes", name, SecretKeyBytes)
	}
	return raw, nil
}
