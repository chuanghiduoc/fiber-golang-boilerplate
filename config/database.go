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
	Password string `env:"DB_PASSWORD"`
	Schema   string `env:"DB_SCHEMA" envDefault:"public"`
	SSLMode  string `env:"DB_SSLMODE" envDefault:"disable"`
	// SSLRootCert is an optional path to the CA bundle that verifies the server
	// certificate. verify-full without it uses the system trust store.
	SSLRootCert     string `env:"DB_SSLROOTCERT"`
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
	// MigrateUsername and MigratePassword, when set, replace Username/Password
	// for cmd/migrate only. Migrations need the schema owner (DDL); the API and
	// worker connect as the non-superuser app role so RLS applies.
	MigrateUsername string `env:"DB_MIGRATE_USERNAME"`
	MigratePassword string `env:"DB_MIGRATE_PASSWORD"`
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

// ForMigration returns the settings cmd/migrate connects with: DB_MIGRATE_*
// override the API's host, port, and credentials when set. The database,
// schema, and TLS settings are shared.
func (db DBConfig) ForMigration() DBConfig {
	if host := strings.TrimSpace(db.MigrateHost); host != "" {
		db.Host = host
	}
	if db.MigratePort != 0 {
		db.Port = db.MigratePort
	}
	if username := strings.TrimSpace(db.MigrateUsername); username != "" {
		db.Username = username
	}
	if db.MigratePassword != "" {
		db.Password = db.MigratePassword
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
	// libpq/pgx compare sslmode case-sensitively; validation accepts
	// DB_SSLMODE case-insensitively, so normalize before building the DSN.
	q.Set("sslmode", strings.ToLower(strings.TrimSpace(db.SSLMode)))
	q.Set("search_path", db.Schema)
	if rootCert := strings.TrimSpace(db.SSLRootCert); rootCert != "" {
		q.Set("sslrootcert", rootCert)
	}

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(db.Username, db.Password),
		Host:     net.JoinHostPort(db.Host, strconv.Itoa(db.Port)),
		Path:     "/" + db.Database,
		RawQuery: q.Encode(),
	}
	return u.String()
}

// validateProductionSSLMode requires certificate verification in production.
// disable never encrypts, allow/prefer fall back to plaintext, and require
// encrypts but accepts any server certificate (so a network attacker with any
// certificate can impersonate the database). Only verify-full both encrypts
// and verifies the host name against DB_SSLROOTCERT (or the system trust
// store). The API and cmd/migrate share this rule.
func validateProductionSSLMode(mode string) error {
	if strings.ToLower(strings.TrimSpace(mode)) != "verify-full" {
		return fmt.Errorf("DB_SSLMODE=%s does not verify the database server certificate in production; use verify-full", mode)
	}
	return nil
}
