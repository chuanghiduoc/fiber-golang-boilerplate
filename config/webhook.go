package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// Webhook delivery and verification bounds.
const (
	maxWebhookRequestTimeout     = 120 * time.Second
	maxWebhookRetryMax           = 7 * 24 * time.Hour
	maxWebhookAttempts           = 50
	maxWebhookRetention          = 3650 * day // ten years
	minWebhookTimestampTolerance = 30 * time.Second
	maxWebhookTimestampTolerance = 3600 * time.Second
)

// defaultWebhookPorts are always allowed for outbound delivery targets. The
// operator list extends them (a receiver on :8443 can be added); it cannot
// remove 443/80.
var defaultWebhookPorts = []int{443, 80}

// WebhookConfig controls outbound delivery, inbound verification, and secret
// encryption at rest. Stored secrets are sealed with SecretEncryptionKey, a
// dedicated base64-encoded 32-byte key; there is no JWT_SECRET fallback in any
// environment (one key per purpose, NIST SP 800-57).
type WebhookConfig struct {
	SecretEncryptionKey string `env:"WEBHOOK_SECRET_ENCRYPTION_KEY"`
	// SecretEncryptionPreviousKeys are decryption-only keys: stored secrets
	// keep the key that sealed them until they are rotated, so a retired key
	// must stay listed while any row still uses it.
	SecretEncryptionPreviousKeys []string      `env:"WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS" envSeparator:","`
	ConnectTimeout               time.Duration `env:"WEBHOOK_CONNECT_TIMEOUT" envDefault:"5s"`
	RequestTimeout               time.Duration `env:"WEBHOOK_REQUEST_TIMEOUT" envDefault:"10s"`
	RetryBase                    time.Duration `env:"WEBHOOK_RETRY_BASE" envDefault:"5s"`
	RetryMax                     time.Duration `env:"WEBHOOK_RETRY_MAX" envDefault:"1h"`
	MaxAttempts                  int           `env:"WEBHOOK_MAX_ATTEMPTS" envDefault:"8"`
	Retention                    time.Duration `env:"WEBHOOK_RETENTION" envDefault:"720h"`
	TimestampTolerance           time.Duration `env:"WEBHOOK_TIMESTAMP_TOLERANCE" envDefault:"5m"`
	// SSRFAllowlist permits private/loopback targets for trusted receivers.
	// Entries are IPs, CIDRs, or exact hostnames. Empty keeps the default deny
	// for every non-public address.
	SSRFAllowlist []string `env:"WEBHOOK_SSRF_ALLOWLIST" envSeparator:","`
	// AllowedPorts extends the always-allowed outbound destinations 443 and 80
	// with a list of ports (1-65535).
	AllowedPorts []string `env:"WEBHOOK_ALLOWED_PORTS" envSeparator:"," envDefault:"443,80"`
	// RequireHTTPS rejects http:// endpoint URLs. Empty means the default:
	// true in production, false otherwise. Accepts true/false.
	RequireHTTPS string `env:"WEBHOOK_REQUIRE_HTTPS"`
}

// RequiresHTTPS resolves the plaintext-URL policy: an explicit setting wins,
// otherwise production requires HTTPS and other environments allow HTTP.
func (w WebhookConfig) RequiresHTTPS(appEnv string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(w.RequireHTTPS)) {
	case "":
		return strings.EqualFold(strings.TrimSpace(appEnv), envProduction), nil
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("WEBHOOK_REQUIRE_HTTPS must be true or false")
	}
}

// ResolvedSecretKey returns the dedicated webhook encryption key. Validation
// guarantees it is set.
func (w WebhookConfig) ResolvedSecretKey() string {
	return strings.TrimSpace(w.SecretEncryptionKey)
}

// PreviousSecretKeyList returns WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS.
func (w WebhookConfig) PreviousSecretKeyList() []string {
	return trimmedList(w.SecretEncryptionPreviousKeys)
}

// SSRFAllowlistEntries returns the parsed allowlist entries.
func (w WebhookConfig) SSRFAllowlistEntries() []string {
	return trimmedList(w.SSRFAllowlist)
}

// AllowedPortsList returns the outbound destination ports: 443 and 80 plus
// every configured WEBHOOK_ALLOWED_PORTS entry. A zero value therefore keeps
// the safe default for config literals built in tests.
func (w WebhookConfig) AllowedPortsList() ([]int, error) {
	ports := append([]int(nil), defaultWebhookPorts...)
	seen := map[int]struct{}{443: {}, 80: {}}
	for _, raw := range w.AllowedPorts {
		port, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid WEBHOOK_ALLOWED_PORTS entry %q: must be an integer between 1 and 65535", raw)
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	return ports, nil
}

// validate checks delivery bounds, then secret encryption and the SSRF
// allowlist. The dedicated key must be strong and is mandatory in every
// environment.
func (w WebhookConfig) validate(appEnv string) error {
	if err := w.validateDelivery(); err != nil {
		return err
	}
	if err := w.validateSecretKey(); err != nil {
		return err
	}
	if _, err := webhook.NewGuard(w.SSRFAllowlistEntries()); err != nil {
		return fmt.Errorf("invalid WEBHOOK_SSRF_ALLOWLIST: %w", err)
	}
	ports, err := w.AllowedPortsList()
	if err != nil {
		return err
	}
	if _, err := webhook.NewGuardWithPorts(w.SSRFAllowlistEntries(), ports); err != nil {
		return fmt.Errorf("invalid WEBHOOK_ALLOWED_PORTS: %w", err)
	}
	if _, err := w.RequiresHTTPS(appEnv); err != nil {
		return err
	}
	return nil
}

// validateDelivery checks timeouts, retry backoff, attempts, retention, and
// the inbound timestamp tolerance.
func (w WebhookConfig) validateDelivery() error {
	if w.ConnectTimeout < time.Second {
		return fmt.Errorf("WEBHOOK_CONNECT_TIMEOUT must be at least 1s")
	}
	if w.RequestTimeout < time.Second {
		return fmt.Errorf("WEBHOOK_REQUEST_TIMEOUT must be at least 1s")
	}
	if w.RequestTimeout > maxWebhookRequestTimeout {
		return fmt.Errorf("WEBHOOK_REQUEST_TIMEOUT must be at most %s", maxWebhookRequestTimeout)
	}
	if w.ConnectTimeout > w.RequestTimeout {
		return fmt.Errorf("WEBHOOK_CONNECT_TIMEOUT must not exceed WEBHOOK_REQUEST_TIMEOUT")
	}
	if w.RetryBase < time.Second {
		return fmt.Errorf("WEBHOOK_RETRY_BASE must be at least 1s")
	}
	if w.RetryMax < w.RetryBase {
		return fmt.Errorf("WEBHOOK_RETRY_MAX must be at least WEBHOOK_RETRY_BASE")
	}
	if w.RetryMax > maxWebhookRetryMax {
		return fmt.Errorf("WEBHOOK_RETRY_MAX must be at most %s", maxWebhookRetryMax)
	}
	if w.MaxAttempts < 1 || w.MaxAttempts > maxWebhookAttempts {
		return fmt.Errorf("WEBHOOK_MAX_ATTEMPTS must be between 1 and %d", maxWebhookAttempts)
	}
	if w.Retention < day || w.Retention > maxWebhookRetention {
		return fmt.Errorf("WEBHOOK_RETENTION must be between 24h and %s", maxWebhookRetention)
	}
	if w.TimestampTolerance < minWebhookTimestampTolerance || w.TimestampTolerance > maxWebhookTimestampTolerance {
		return fmt.Errorf("WEBHOOK_TIMESTAMP_TOLERANCE must be between %s and %s",
			minWebhookTimestampTolerance, maxWebhookTimestampTolerance)
	}
	return nil
}

// validateSecretKey checks the dedicated key that seals endpoint and source
// secrets.
func (w WebhookConfig) validateSecretKey() error {
	if err := ValidateKey("WEBHOOK_SECRET_ENCRYPTION_KEY", w.SecretEncryptionKey); err != nil {
		return err
	}
	for _, previous := range w.PreviousSecretKeyList() {
		if previous == w.ResolvedSecretKey() {
			return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS must not contain the active key")
		}
		if err := ValidateKey("WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS", previous); err != nil {
			return err
		}
	}
	return nil
}
