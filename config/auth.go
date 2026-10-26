package config

import (
	"fmt"
	"slices"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// Credential strength rules for every environment except local and test.
const (
	minJWTSecretLength     = 32
	minAdminPasswordLength = 12
)

// sampleJWTSecrets are public values (empty, the env default, and the .env.example
// and docker-compose fallback) that must never sign tokens outside local/test.
var sampleJWTSecrets = []string{"", "secret", "your-super-secret-key-change-in-production"}

// sampleAdminPassword is the .env.example ADMIN_PASSWORD.
const sampleAdminPassword = "Admin123!" // ggignore: public sample value, rejected in production

type JWTConfig struct {
	// Secret is the active signing key. Tokens carry its derived kid.
	Secret string `env:"JWT_SECRET" envDefault:"secret"`
	// PreviousSecrets are comma-separated retired signing keys that still
	// verify tokens during a rotation. They never sign.
	PreviousSecrets   string `env:"JWT_PREVIOUS_SECRETS"`
	ExpireHour        int    `env:"JWT_EXPIRE_HOUR" envDefault:"1"`
	RefreshExpireDays int    `env:"JWT_REFRESH_EXPIRE_DAYS" envDefault:"30"`
	// SessionMaxLifetime is the absolute lifetime of a sign-in session. Every
	// refresh token (initial or rotated) expires at the session's expiry.
	SessionMaxLifetime time.Duration `env:"SESSION_MAX_LIFETIME" envDefault:"720h"`
}

// PreviousSecretList returns the parsed JWT_PREVIOUS_SECRETS entries.
func (j JWTConfig) PreviousSecretList() []string {
	return splitCSV(j.PreviousSecrets)
}

// Keyring returns the verification keyring: the active secret plus every
// previous secret.
func (j JWTConfig) Keyring() *token.Keyring {
	return token.NewKeyring(j.Secret, j.PreviousSecretList()...)
}

type AdminConfig struct {
	Email    string `env:"ADMIN_EMAIL"`
	Password string `env:"ADMIN_PASSWORD"`
	Name     string `env:"ADMIN_NAME" envDefault:"Admin"`
}

type OAuthConfig struct {
	GoogleClientID     string `env:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `env:"GOOGLE_CLIENT_SECRET"`
	GoogleRedirectURL  string `env:"GOOGLE_REDIRECT_URL" envDefault:"http://localhost:8080/api/v1/auth/google/callback"`
	FrontendURL        string `env:"OAUTH_FRONTEND_URL" envDefault:"http://localhost:3000/auth/callback"`
}

// Password policy settings for new passwords.
type PasswordConfig struct {
	// BreachCheck is none (default) or hibp. With hibp, the Have I Been Pwned
	// range API is queried with k-anonymity (only the first five SHA-1 hex
	// characters leave the process); the check fails open with a warning when
	// the provider is unavailable, so a third party cannot block sign-ups.
	BreachCheck string `env:"PASSWORD_BREACH_CHECK" envDefault:"none"`
}

// HIBPEnabled reports whether the optional breached-password check is on.
func (p PasswordConfig) HIBPEnabled() bool {
	return p.BreachCheck == "hibp"
}

// validate accepts only the documented modes. An empty value is the compiled
// default ("none"), so a programmatically built Config needs no env parsing.
func (p PasswordConfig) validate() error {
	switch p.BreachCheck {
	case "", "none", "hibp":
		return nil
	default:
		return fmt.Errorf("PASSWORD_BREACH_CHECK must be one of: none, hibp")
	}
}

// DefaultMFAMaxAge is how fresh a second-factor verification stays valid when
// MFA_MAX_AGE is unset.
const DefaultMFAMaxAge = 12 * time.Hour

// MFAConfig bounds the freshness of a session's second factor.
type MFAConfig struct {
	// MaxAge is how long an MFA verification stays valid for MFA-gated
	// routes (NIST SP 800-63B rev4 §2.2.3). A session whose mfa_verified_at is
	// older is refused with 403 auth.mfa_required and must step up again.
	MaxAge time.Duration `env:"MFA_MAX_AGE" envDefault:"12h"`
}

func (m MFAConfig) validate() error {
	if m.MaxAge < 0 {
		return fmt.Errorf("MFA_MAX_AGE must not be negative")
	}
	return nil
}

// EffectiveMaxAge returns the configured freshness window, falling back to
// DefaultMFAMaxAge for the zero value so a programmatically built Config never
// disables the AAL2 age bound by accident.
func (m MFAConfig) EffectiveMaxAge() time.Duration {
	if m.MaxAge <= 0 {
		return DefaultMFAMaxAge
	}
	return m.MaxAge
}

// validateSigningSecret rejects sample or short JWT secrets in every
// environment except local and test, including staging and development.
func (j JWTConfig) validateSigningSecret(appEnv string) error {
	if usesSampleSecrets(appEnv) {
		return nil
	}
	for _, sample := range sampleJWTSecrets {
		if j.Secret == sample {
			return fmt.Errorf("JWT_SECRET must be set to a secure value in %s environment", appEnv)
		}
	}
	if len(j.Secret) < minJWTSecretLength {
		return fmt.Errorf("JWT_SECRET must be at least %d characters for security", minJWTSecretLength)
	}
	for _, previous := range j.PreviousSecretList() {
		if slices.Contains(sampleJWTSecrets, previous) || len(previous) < minJWTSecretLength {
			return fmt.Errorf("JWT_PREVIOUS_SECRETS entries must be at least %d characters and not a sample value", minJWTSecretLength)
		}
	}
	return nil
}

// validatePreviousSecrets rejects listing the active secret as a previous
// one: that is always an operator mistake during a rotation (the new secret
// was not set, or the lists were swapped).
func (j JWTConfig) validatePreviousSecrets() error {
	if slices.Contains(j.PreviousSecretList(), j.Secret) {
		return fmt.Errorf("JWT_PREVIOUS_SECRETS must not contain the active JWT_SECRET")
	}
	return nil
}

// validate checks the previous-secret list and token lifetimes.
func (j JWTConfig) validate() error {
	if err := j.validatePreviousSecrets(); err != nil {
		return err
	}
	if j.ExpireHour < 1 {
		return fmt.Errorf("JWT_EXPIRE_HOUR must be at least 1")
	}
	if j.RefreshExpireDays < 1 {
		return fmt.Errorf("JWT_REFRESH_EXPIRE_DAYS must be at least 1")
	}
	if j.SessionMaxLifetime <= 0 {
		return fmt.Errorf("SESSION_MAX_LIFETIME must be greater than zero")
	}
	return nil
}

// validate checks the optional seed admin outside local/test: email and
// password come as a pair, and the password must not be weak or the sample.
func (a AdminConfig) validate(appEnv string) error {
	if usesSampleSecrets(appEnv) || (a.Email == "" && a.Password == "") {
		return nil
	}
	if a.Email == "" || a.Password == "" {
		return fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be set together")
	}
	if len(a.Password) < minAdminPasswordLength || a.Password == sampleAdminPassword {
		return fmt.Errorf("ADMIN_PASSWORD is too weak for %s environment", appEnv)
	}
	return nil
}

// validate requires the client secret whenever Google sign-in is configured.
func (o OAuthConfig) validate() error {
	if o.GoogleClientID != "" && o.GoogleClientSecret == "" {
		return fmt.Errorf("GOOGLE_CLIENT_SECRET is required when GOOGLE_CLIENT_ID is set")
	}
	return nil
}

// validateProduction keeps the OAuth redirect and frontend callback on https
// when Google sign-in is configured.
func (o OAuthConfig) validateProduction() error {
	if o.GoogleClientID == "" {
		return nil
	}
	if !isHTTPSURL(o.FrontendURL) {
		return fmt.Errorf("OAUTH_FRONTEND_URL must use https in production")
	}
	if !isHTTPSURL(o.GoogleRedirectURL) {
		return fmt.Errorf("GOOGLE_REDIRECT_URL must use https in production")
	}
	return nil
}
