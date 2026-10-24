package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type DBConfig struct {
	Host     string `env:"DB_HOST" envDefault:"localhost"`
	Port     int    `env:"DB_PORT" envDefault:"5432"`
	Database string `env:"DB_DATABASE" envDefault:"boilerplate"`
	Username string `env:"DB_USERNAME" envDefault:"postgres"`
	// Password has no default: credentials always come from the environment
	// (.env, Compose, or the Kubernetes Secret), never from code.
	Password        string `env:"DB_PASSWORD"`
	Schema          string `env:"DB_SCHEMA" envDefault:"public"`
	SSLMode         string `env:"DB_SSLMODE" envDefault:"disable"`
	ConnectTimeout  int    `env:"DB_CONNECT_TIMEOUT" envDefault:"10"` // seconds
	MaxConns        int32  `env:"DB_MAX_CONNS" envDefault:"25"`
	MinConns        int32  `env:"DB_MIN_CONNS" envDefault:"5"`
	MaxConnLifetime int    `env:"DB_MAX_CONN_LIFETIME" envDefault:"3600"` // seconds
	MaxConnIdleTime int    `env:"DB_MAX_CONN_IDLE_TIME" envDefault:"300"` // seconds
}

// Validate checks the database connection and pool settings.
func (db DBConfig) Validate() error {
	if db.ConnectTimeout < 1 {
		return fmt.Errorf("DB_CONNECT_TIMEOUT must be at least 1 second")
	}
	if db.MaxConns < 1 {
		return fmt.Errorf("DB_MAX_CONNS must be at least 1")
	}
	if db.MinConns < 0 {
		return fmt.Errorf("DB_MIN_CONNS must not be negative")
	}
	if db.MinConns > db.MaxConns {
		return fmt.Errorf("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", db.MinConns, db.MaxConns)
	}
	return nil
}

func (db DBConfig) DSN() string {
	q := url.Values{}
	q.Set("sslmode", db.SSLMode)
	q.Set("search_path", db.Schema)

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(db.Username, db.Password),
		Host:     net.JoinHostPort(db.Host, strconv.Itoa(db.Port)),
		Path:     "/" + db.Database,
		RawQuery: q.Encode(),
	}
	return u.String()
}

// validateProductionSSLMode rejects DB_SSLMODE values that permit plaintext:
// disable never encrypts, and allow/prefer silently fall back to plaintext
// when the server or a network attacker refuses TLS. The API and cmd/migrate
// share this rule.
func validateProductionSSLMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "disable", "allow", "prefer":
		return fmt.Errorf("DB_SSLMODE=%s permits plaintext in production; use require, verify-ca, or verify-full", mode)
	}
	return nil
}
