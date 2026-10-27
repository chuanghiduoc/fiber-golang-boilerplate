package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// defaultWebAuthnRPName is the relying-party display name browsers show when
// WEBAUTHN_RP_NAME is not configured. Production still requires an explicit
// value so a deployment cannot accidentally present the boilerplate name.
const defaultWebAuthnRPName = "Fiber Golang Boilerplate"

// webAuthnRPIDPattern is the hostname shape accepted for a relying-party id:
// lower-case DNS labels, no scheme, no port, no path. "localhost" is allowed
// for local development; IP addresses are not valid WebAuthn RP IDs.
var webAuthnRPIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

// WebAuthnConfig configures the passkey relying party. Passing an empty RP ID
// disables passkeys (outside production); setting any passkey setting without
// an RP ID is a configuration error. Production requires RP ID, RP name, and
// at least one HTTPS origin.
type WebAuthnConfig struct {
	// RPID is the effective domain of the relying party, for example
	// "example.com". It must contain no scheme or port.
	RPID string `env:"WEBAUTHN_RP_ID"`
	// RPName is the human-readable relying-party name shown by browsers.
	RPName string `env:"WEBAUTHN_RP_NAME"`
	// RPOrigins is the comma-separated list of fully qualified HTTPS origins
	// allowed to run ceremonies for this relying party.
	RPOrigins []string `env:"WEBAUTHN_RP_ORIGINS" envSeparator:","`
}

// Enabled reports whether passkeys are configured.
func (w WebAuthnConfig) Enabled() bool { return strings.TrimSpace(w.RPID) != "" }

// DisplayName returns the configured relying-party name, or the documented
// default for local installations.
func (w WebAuthnConfig) DisplayName() string {
	if name := strings.TrimSpace(w.RPName); name != "" {
		return name
	}
	return defaultWebAuthnRPName
}

// OriginList returns the parsed WEBAUTHN_RP_ORIGINS entries.
func (w WebAuthnConfig) OriginList() []string {
	return trimmedList(w.RPOrigins)
}

// validate checks the relying-party settings. An entirely empty section
// disables passkeys; a partial section is rejected. Origins must be absolute
// http/https origins whose host is the RP ID or one of its subdomains.
func (w WebAuthnConfig) validate() error {
	rpID := strings.TrimSpace(w.RPID)
	name := strings.TrimSpace(w.RPName)
	origins := w.OriginList()
	if rpID == "" {
		switch {
		case name != "":
			return fmt.Errorf("WEBAUTHN_RP_NAME requires WEBAUTHN_RP_ID")
		case len(origins) > 0:
			return fmt.Errorf("WEBAUTHN_RP_ORIGINS requires WEBAUTHN_RP_ID")
		default:
			return nil
		}
	}
	if !webAuthnRPIDPattern.MatchString(rpID) || net.ParseIP(rpID) != nil {
		return fmt.Errorf("WEBAUTHN_RP_ID must be a lower-case domain name without scheme or port")
	}
	if len(origins) == 0 {
		return fmt.Errorf("WEBAUTHN_RP_ORIGINS is required when WEBAUTHN_RP_ID is set")
	}
	seen := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if _, ok := seen[origin]; ok {
			return fmt.Errorf("WEBAUTHN_RP_ORIGINS contains duplicate origin %q", origin)
		}
		seen[origin] = struct{}{}
		if err := validateWebAuthnOrigin(origin, rpID); err != nil {
			return err
		}
	}
	return nil
}

// validateProduction requires a complete passkey configuration and HTTPS
// origins: a production browser will not run WebAuthn over plain HTTP.
func (w WebAuthnConfig) validateProduction() error {
	if !w.Enabled() {
		return fmt.Errorf("WEBAUTHN_RP_ID must be set in production")
	}
	if strings.TrimSpace(w.RPName) == "" {
		return fmt.Errorf("WEBAUTHN_RP_NAME must be set in production")
	}
	origins := w.OriginList()
	if len(origins) == 0 {
		return fmt.Errorf("WEBAUTHN_RP_ORIGINS must be set in production")
	}
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
			return fmt.Errorf("WEBAUTHN_RP_ORIGINS must use https in production (%q)", origin)
		}
	}
	return nil
}

// validateWebAuthnOrigin checks one configured origin against the RP ID.
func validateWebAuthnOrigin(origin, rpID string) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("invalid WEBAUTHN_RP_ORIGINS entry %q: %w", origin, err)
	}
	if !slices.Contains([]string{"http", "https"}, strings.ToLower(parsed.Scheme)) {
		return fmt.Errorf("invalid WEBAUTHN_RP_ORIGINS entry %q: scheme must be http or https", origin)
	}
	if parsed.User != nil {
		return fmt.Errorf("invalid WEBAUTHN_RP_ORIGINS entry %q: credentials are not allowed", origin)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("invalid WEBAUTHN_RP_ORIGINS entry %q: path is not allowed", origin)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid WEBAUTHN_RP_ORIGINS entry %q: query and fragment are not allowed", origin)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("invalid WEBAUTHN_RP_ORIGINS entry %q: host is required", origin)
	}
	if host != rpID && !strings.HasSuffix(host, "."+rpID) {
		return fmt.Errorf("WEBAUTHN_RP_ORIGINS entry %q is not the relying-party domain %q or a subdomain", origin, rpID)
	}
	return nil
}
