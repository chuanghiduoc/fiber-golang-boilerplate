package config

import "strings"

// defaultLocale is used when I18N_DEFAULT_LOCALE is blank.
const defaultLocale = "en"

// I18NConfig controls request locale negotiation. Locale tags are matched
// against the embedded catalogs; unsupported values fail at startup.
type I18NConfig struct {
	DefaultLocale string `env:"I18N_DEFAULT_LOCALE" envDefault:"en"`
}

// Locale returns the normalized configured default locale, or "en".
func (c I18NConfig) Locale() string {
	locale := strings.ToLower(strings.TrimSpace(c.DefaultLocale))
	if locale == "" {
		return defaultLocale
	}
	return locale
}
