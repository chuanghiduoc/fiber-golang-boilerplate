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
	maxWebhookRequestTimeoutSecs  = 120
	maxWebhookRetryMaxSecs        = 7 * 24 * 60 * 60 // one week
	maxWebhookAttempts            = 50
	maxWebhookRetentionDays       = 3650 // ten years
	minWebhookTimestampTolerance  = 30
	maxWebhookTimestampTolerance  = 3600
	minWebhookEncryptionKeyLength = 32
)

// defaultWebhookPorts are always allowed for outbound delivery targets. The
// operator list extends them (a receiver on :8443 can be added); it cannot
// remove 443/80.
var defaultWebhookPorts = []int{443, 80}

// WebhookConfig controls outbound delivery, inbound verification, and secret
// encryption at rest. The signing key is sealed with a key derived from
// SecretEncryptionKey; when that is empty the JWT secret is used as a
// compatibility fallback (never in production).
type WebhookConfig struct {
	SecretEncryptionKey string `env:"WEBHOOK_SECRET_ENCRYPTION_KEY"`
	// SecretEncryptionPreviousKeys are comma-separated decryption-only keys:
	// stored secrets keep the key that sealed them until they are rotated, so
	// a retired key must stay listed while any row still uses it.
	SecretEncryptionPreviousKeys string `env:"WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS"`
	ConnectTimeoutSeconds        int    `env:"WEBHOOK_CONNECT_TIMEOUT_SECS" envDefault:"5"`
	RequestTimeoutSeconds        int    `env:"WEBHOOK_REQUEST_TIMEOUT_SECS" envDefault:"10"`
	RetryBaseSeconds             int    `env:"WEBHOOK_RETRY_BASE_SECS" envDefault:"5"`
	RetryMaxSeconds              int    `env:"WEBHOOK_RETRY_MAX_SECS" envDefault:"3600"`
	MaxAttempts                  int    `env:"WEBHOOK_MAX_ATTEMPTS" envDefault:"8"`
	RetentionDays                int    `env:"WEBHOOK_RETENTION_DAYS" envDefault:"30"`
	TimestampToleranceSeconds    int    `env:"WEBHOOK_TIMESTAMP_TOLERANCE_SECS" envDefault:"300"`
	// SSRFAllowlist permits private/loopback targets for trusted receivers.
	// Entries are comma-separated IPs, CIDRs, or exact hostnames. Empty keeps
	// the default deny for every non-public address.
	SSRFAllowlist string `env:"WEBHOOK_SSRF_ALLOWLIST"`
	// AllowedPortsCSV extends the always-allowed outbound destinations 443 and
	// 80 with a comma-separated list of ports (1-65535).
	AllowedPortsCSV string `env:"WEBHOOK_ALLOWED_PORTS" envDefault:"443,80"`
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

// ResolvedSecretKey returns the dedicated webhook encryption key, or the JWT
// secret as the documented compatibility fallback.
func (w WebhookConfig) ResolvedSecretKey(jwtSecret string) string {
	if key := strings.TrimSpace(w.SecretEncryptionKey); key != "" {
		return key
	}
	return jwtSecret
}

// PreviousSecretKeyList returns the parsed
// WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS entries.
func (w WebhookConfig) PreviousSecretKeyList() []string {
	return splitCSV(w.SecretEncryptionPreviousKeys)
}

// SSRFAllowlistEntries returns the parsed allowlist entries.
func (w WebhookConfig) SSRFAllowlistEntries() []string {
	return splitCSV(w.SSRFAllowlist)
}

// AllowedPorts returns the outbound destination ports: 443 and 80 plus every
// configured WEBHOOK_ALLOWED_PORTS entry. The zero value therefore keeps the
// safe default for config literals built in tests.
func (w WebhookConfig) AllowedPorts() ([]int, error) {
	ports := append([]int(nil), defaultWebhookPorts...)
	seen := map[int]struct{}{443: {}, 80: {}}
	for _, raw := range splitCSV(w.AllowedPortsCSV) {
		port, err := strconv.Atoi(raw)
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

// RequestTimeout returns the per-delivery HTTP timeout.
func (w WebhookConfig) RequestTimeout() time.Duration {
	return time.Duration(w.RequestTimeoutSeconds) * time.Second
}

// ConnectTimeout returns the TCP/TLS connect timeout.
func (w WebhookConfig) ConnectTimeout() time.Duration {
	return time.Duration(w.ConnectTimeoutSeconds) * time.Second
}

// RetryBase returns the first retry backoff delay.
func (w WebhookConfig) RetryBase() time.Duration {
	return time.Duration(w.RetryBaseSeconds) * time.Second
}

// RetryMax returns the maximum retry backoff delay.
func (w WebhookConfig) RetryMax() time.Duration {
	return time.Duration(w.RetryMaxSeconds) * time.Second
}

// Retention returns how long delivery and inbound-event history is retained.
func (w WebhookConfig) Retention() time.Duration {
	return time.Duration(w.RetentionDays) * day
}

// TimestampTolerance returns the accepted inbound signature clock skew.
func (w WebhookConfig) TimestampTolerance() time.Duration {
	return time.Duration(w.TimestampToleranceSeconds) * time.Second
}

// validate checks delivery bounds, then secret encryption and the SSRF
// allowlist. The dedicated key must be strong and is mandatory in production;
// outside production the JWT secret is the documented fallback so local
// installs work without extra setup.
func (w WebhookConfig) validate(appEnv, jwtSecret string) error {
	if err := w.validateDelivery(); err != nil {
		return err
	}
	if err := w.validateSecretKey(appEnv, jwtSecret); err != nil {
		return err
	}
	if _, err := webhook.NewGuard(w.SSRFAllowlistEntries()); err != nil {
		return fmt.Errorf("invalid WEBHOOK_SSRF_ALLOWLIST: %w", err)
	}
	ports, err := w.AllowedPorts()
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
	if w.ConnectTimeoutSeconds < 1 {
		return fmt.Errorf("WEBHOOK_CONNECT_TIMEOUT_SECS must be at least 1")
	}
	if w.RequestTimeoutSeconds < 1 {
		return fmt.Errorf("WEBHOOK_REQUEST_TIMEOUT_SECS must be at least 1")
	}
	if w.RequestTimeoutSeconds > maxWebhookRequestTimeoutSecs {
		return fmt.Errorf("WEBHOOK_REQUEST_TIMEOUT_SECS must be at most %d", maxWebhookRequestTimeoutSecs)
	}
	if w.ConnectTimeoutSeconds > w.RequestTimeoutSeconds {
		return fmt.Errorf("WEBHOOK_CONNECT_TIMEOUT_SECS must not exceed WEBHOOK_REQUEST_TIMEOUT_SECS")
	}
	if w.RetryBaseSeconds < 1 {
		return fmt.Errorf("WEBHOOK_RETRY_BASE_SECS must be at least 1")
	}
	if w.RetryMaxSeconds < w.RetryBaseSeconds {
		return fmt.Errorf("WEBHOOK_RETRY_MAX_SECS must be at least WEBHOOK_RETRY_BASE_SECS")
	}
	if w.RetryMaxSeconds > maxWebhookRetryMaxSecs {
		return fmt.Errorf("WEBHOOK_RETRY_MAX_SECS must be at most %d", maxWebhookRetryMaxSecs)
	}
	if w.MaxAttempts < 1 || w.MaxAttempts > maxWebhookAttempts {
		return fmt.Errorf("WEBHOOK_MAX_ATTEMPTS must be between 1 and %d", maxWebhookAttempts)
	}
	if w.RetentionDays < 1 || w.RetentionDays > maxWebhookRetentionDays {
		return fmt.Errorf("WEBHOOK_RETENTION_DAYS must be between 1 and %d", maxWebhookRetentionDays)
	}
	if w.TimestampToleranceSeconds < minWebhookTimestampTolerance || w.TimestampToleranceSeconds > maxWebhookTimestampTolerance {
		return fmt.Errorf("WEBHOOK_TIMESTAMP_TOLERANCE_SECS must be between %d and %d",
			minWebhookTimestampTolerance, maxWebhookTimestampTolerance)
	}
	return nil
}

// validateSecretKey checks the key that seals endpoint and source secrets.
func (w WebhookConfig) validateSecretKey(appEnv, jwtSecret string) error {
	key := strings.TrimSpace(w.SecretEncryptionKey)
	if key != "" && len(key) < minWebhookEncryptionKeyLength {
		return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_KEY must be at least %d characters", minWebhookEncryptionKeyLength)
	}
	if appEnv == envProduction && key == "" {
		return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_KEY must be set in production")
	}
	if strings.TrimSpace(w.ResolvedSecretKey(jwtSecret)) == "" {
		return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_KEY or JWT_SECRET must be set")
	}
	for _, previous := range w.PreviousSecretKeyList() {
		if previous == w.ResolvedSecretKey(jwtSecret) {
			return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS must not contain the active key")
		}
		if !usesSampleSecrets(appEnv) && len(previous) < minWebhookEncryptionKeyLength {
			return fmt.Errorf("WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS entries must be at least %d characters", minWebhookEncryptionKeyLength)
		}
	}
	return nil
}
