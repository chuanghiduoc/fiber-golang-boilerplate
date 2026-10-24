package config

import "fmt"

type EmailConfig struct {
	Driver       string `env:"EMAIL_DRIVER" envDefault:"console"`
	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     int    `env:"SMTP_PORT" envDefault:"587"`
	SMTPUsername string `env:"SMTP_USERNAME"`
	SMTPPassword string `env:"SMTP_PASSWORD"`
	FromAddress  string `env:"EMAIL_FROM_ADDRESS" envDefault:"noreply@localhost"`
	FromName     string `env:"EMAIL_FROM_NAME" envDefault:"Fiber App"`
	// SMTPRequireTLS fails the send when the server does not offer STARTTLS,
	// preventing a downgrade that would send message content (reset/verification
	// links) in cleartext. Leave false for local relays without TLS.
	SMTPRequireTLS bool `env:"SMTP_REQUIRE_TLS" envDefault:"false"`
}

// validate checks the email driver and its SMTP endpoint.
func (e EmailConfig) validate() error {
	switch e.Driver {
	case "console":
	case "smtp":
		if e.SMTPHost == "" {
			return fmt.Errorf("SMTP_HOST is required when EMAIL_DRIVER=smtp")
		}
		if e.SMTPPort < 1 || e.SMTPPort > maxTCPPort {
			return fmt.Errorf("SMTP_PORT must be between 1 and %d", maxTCPPort)
		}
	default:
		return fmt.Errorf("EMAIL_DRIVER must be one of: console, smtp (got %q)", e.Driver)
	}
	return nil
}

// validateProduction requires real delivery over STARTTLS: reset and
// verification links must not be logged to the console or sent in cleartext.
func (e EmailConfig) validateProduction() error {
	if e.Driver == "console" {
		return fmt.Errorf("EMAIL_DRIVER=console must not be used in production")
	}
	if e.Driver == "smtp" && !e.SMTPRequireTLS {
		return fmt.Errorf("SMTP_REQUIRE_TLS must be true when EMAIL_DRIVER=smtp in production")
	}
	return nil
}
