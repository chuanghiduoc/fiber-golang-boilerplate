package config

import "fmt"

// Credential strength rules for every environment except local and test.
const (
	minJWTSecretLength     = 32
	minAdminPasswordLength = 12
)

// sampleJWTSecrets are public values (empty, the env default, and the .env.example
// and docker-compose fallback) that must never sign tokens outside local/test.
var sampleJWTSecrets = []string{"", "secret", "your-super-secret-key-change-in-production"}

// sampleAdminPassword is the .env.example and docker-compose ADMIN_PASSWORD.
const sampleAdminPassword = "Admin123!" // ggignore: public sample value, rejected in production

type JWTConfig struct {
	Secret            string `env:"JWT_SECRET" envDefault:"secret"`
	ExpireHour        int    `env:"JWT_EXPIRE_HOUR" envDefault:"24"`
	RefreshExpireDays int    `env:"JWT_REFRESH_EXPIRE_DAYS" envDefault:"30"`
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
	return nil
}

// validate checks token lifetimes.
func (j JWTConfig) validate() error {
	if j.ExpireHour < 1 {
		return fmt.Errorf("JWT_EXPIRE_HOUR must be at least 1")
	}
	if j.RefreshExpireDays < 1 {
		return fmt.Errorf("JWT_REFRESH_EXPIRE_DAYS must be at least 1")
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
