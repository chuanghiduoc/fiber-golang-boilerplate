package config

import (
	"fmt"
	"net"
	"strconv"
)

const maxTCPPortNumber = 65535

// FilesConfig holds the files module's own limits: storage quotas and the
// malware scanner. Upload size and the MIME allowlist stay in StorageConfig
// because they are properties of the storage layer.
type FilesConfig struct {
	// TenantQuotaBytes caps the bytes one tenant may store (live and trashed).
	// Zero means unlimited.
	TenantQuotaBytes int64 `env:"FILES_TENANT_QUOTA_BYTES" envDefault:"0"`
	// UserQuotaBytes caps the bytes one user may store within a tenant. Zero
	// means unlimited. It can only make the tenant quota stricter.
	UserQuotaBytes int64 `env:"FILES_USER_QUOTA_BYTES" envDefault:"0"`
	// ScanDriver selects the upload malware scanner: "none" (default) or
	// "clamav" (clamd INSTREAM over TCP).
	ScanDriver string `env:"FILES_SCAN_DRIVER" envDefault:"none"`
	// ClamAVAddr is clamd's host:port, required for the clamav driver.
	ClamAVAddr string `env:"FILES_CLAMAV_ADDR"`
	// ClamAVTimeoutSecs bounds one scan connection and its response.
	ClamAVTimeoutSecs int `env:"FILES_CLAMAV_TIMEOUT_SECS" envDefault:"10"`
}

// validate keeps quotas non-negative and requires the clamav driver's address.
func (f FilesConfig) validate() error {
	if f.TenantQuotaBytes < 0 {
		return fmt.Errorf("FILES_TENANT_QUOTA_BYTES must not be negative")
	}
	if f.UserQuotaBytes < 0 {
		return fmt.Errorf("FILES_USER_QUOTA_BYTES must not be negative")
	}
	switch f.ScanDriver {
	case "", "none":
		return nil
	case "clamav":
		if err := validateHostPort(f.ClamAVAddr); err != nil {
			return fmt.Errorf("FILES_CLAMAV_ADDR: %w", err)
		}
		if f.ClamAVTimeoutSecs < 1 {
			return fmt.Errorf("FILES_CLAMAV_TIMEOUT_SECS must be at least 1")
		}
		return nil
	default:
		return fmt.Errorf("FILES_SCAN_DRIVER must be one of: none, clamav (got %q)", f.ScanDriver)
	}
}

// EnabledScanDriver returns the normalized scanner name ("none" for an unset
// literal).
func (f FilesConfig) EnabledScanDriver() string {
	if f.ScanDriver == "" {
		return "none"
	}
	return f.ScanDriver
}

// validateHostPort requires a host:port with a usable numeric port, so a typo
// fails startup instead of turning every upload into a 503.
func validateHostPort(addr string) error {
	if addr == "" {
		return fmt.Errorf("is required for the clamav scan driver")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	if host == "" {
		return fmt.Errorf("must include a host")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > maxTCPPortNumber {
		return fmt.Errorf("port must be between 1 and %d", maxTCPPortNumber)
	}
	return nil
}
