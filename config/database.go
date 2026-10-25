package config

import (
	"fmt"
	"net"
	"net/url"
	"slices"
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
	// QueryExecMode is the pgx query execution mode. cache_statement (the
	// pgx default) prepares named statements per connection, which a
	// transaction-mode pooler must support (PgBouncer 1.21+ with
	// max_prepared_statements > 0). cache_describe and describe_exec use
	// unnamed statements and keep server-side parameter types. pgx's exec
	// and simple_protocol modes are deliberately not accepted: without
	// described parameter types, []byte arguments bound to json/jsonb
	// columns (audit metadata) are sent as bytea and the insert fails.
	// See pkg/database.QueryExecModes.
	QueryExecMode string `env:"DB_QUERY_EXEC_MODE" envDefault:"cache_statement"`
	// MigrateHost and MigratePort, when set, replace Host/Port for
	// cmd/migrate only. golang-migrate holds a session-level advisory lock
	// (pg_advisory_lock) for the whole run, which a transaction-mode pooler
	// does not preserve, so migrations must reach PostgreSQL directly (or a
	// session-mode pool) while the API keeps using the pooler.
	MigrateHost string `env:"DB_MIGRATE_HOST"`
	MigratePort int    `env:"DB_MIGRATE_PORT"`
}

// dbQueryExecModes are the accepted DB_QUERY_EXEC_MODE values; pkg/database
// maps them onto pgx.QueryExecMode.
var dbQueryExecModes = []string{"cache_statement", "cache_describe", "describe_exec"}

// EffectiveQueryExecMode returns QueryExecMode, or cache_statement (the pgx
// default) for a zero value in DBConfig literals.
func (db DBConfig) EffectiveQueryExecMode() string {
	if db.QueryExecMode == "" {
		return "cache_statement"
	}
	return db.QueryExecMode
}

// ForMigration returns the settings cmd/migrate connects with: DB_MIGRATE_HOST
// and DB_MIGRATE_PORT override the API's host/port when set. Credentials,
// database, schema, and TLS settings are shared.
func (db DBConfig) ForMigration() DBConfig {
	if host := strings.TrimSpace(db.MigrateHost); host != "" {
		db.Host = host
	}
	if db.MigratePort != 0 {
		db.Port = db.MigratePort
	}
	return db
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
	if !slices.Contains(dbQueryExecModes, db.EffectiveQueryExecMode()) {
		return fmt.Errorf("DB_QUERY_EXEC_MODE must be one of: %s (got %q)", strings.Join(dbQueryExecModes, ", "), db.QueryExecMode)
	}
	if db.MigratePort < 0 || db.MigratePort > maxTCPPort {
		return fmt.Errorf("DB_MIGRATE_PORT must be between 1 and %d", maxTCPPort)
	}
	return nil
}

// DSN returns the connection URL. search_path is a startup parameter, which
// PostgreSQL applies to the whole session. It is deliberately not a SET in a
// pgx AfterConnect hook: behind a transaction-mode pooler that SET would run
// on one server connection while later transactions land on others. With
// PgBouncer, either forward it (track_extra_parameters = search_path, on
// versions that support it) or drop it (ignore_startup_parameters = search_path) and
// set it on the role (ALTER ROLE ... SET search_path).
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
