package database

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoPool is returned when a scoped handle has no connection pool.
var ErrNoPool = errors.New("database: pool is nil")

// Scope is the row-level-security context of one statement or transaction.
// A zero TenantID sees no tenant-owned rows; Bypass disables tenant isolation
// for a deliberate cross-tenant operation.
type Scope struct {
	TenantID int64
	Bypass   bool
}

// ScopeResolver derives the scope from a context. The composition root injects
// it because this package must not import internal/platform packages.
type ScopeResolver func(context.Context) Scope

// Resolver builds a ScopeResolver from the tenant getter and the bypass
// marker, so callers do not repeat the two-line mapping.
func Resolver(tenantID func(context.Context) int64) ScopeResolver {
	return func(ctx context.Context) Scope {
		return Scope{TenantID: tenantID(ctx), Bypass: RLSBypass(ctx)}
	}
}

// DBTX is the query surface sqlc needs. *pgxpool.Pool, pgx.Tx, and Scoped
// implement it.
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type scopeConfig struct {
	resolve ScopeResolver
}

// ScopeOption configures how a TxManager or Scoped resolves its scope.
type ScopeOption func(*scopeConfig)

// WithResolver injects the context-to-scope resolver used by TxManager and
// Scoped.
func WithResolver(resolve ScopeResolver) ScopeOption {
	return func(cfg *scopeConfig) {
		if resolve != nil {
			cfg.resolve = resolve
		}
	}
}

// newScopeConfig defaults to a zero scope: without a resolver a query sees no
// tenant-owned rows rather than another tenant's.
func newScopeConfig(opts []ScopeOption) scopeConfig {
	cfg := scopeConfig{resolve: func(context.Context) Scope { return Scope{} }}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

type bypassKey struct{}

// WithRLSBypass marks ctx as a deliberate cross-tenant operation: transactions
// and scoped statements started from it ignore tenant isolation. Never use it
// on an HTTP request path that serves one tenant.
func WithRLSBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, bypassKey{}, true)
}

// RLSBypass reports whether ctx was marked with WithRLSBypass.
func RLSBypass(ctx context.Context) bool {
	bypass, _ := ctx.Value(bypassKey{}).(bool)
	return bypass
}

// setScopeSQL sets both settings in one statement. The third argument of
// set_config (true) makes them transaction-local, so a pooled connection is
// never left scoped to the previous caller.
const setScopeSQL = `SELECT set_config('app.tenant_id', $1, true), set_config('app.rls_bypass', $2, true)`

// scopeArgs maps a Scope onto the two text settings. A non-positive tenant
// becomes an empty string, which NULLIF turns into NULL and fails the policy
// closed.
func scopeArgs(scope Scope) (tenant, bypass string) {
	if scope.TenantID > 0 {
		tenant = strconv.FormatInt(scope.TenantID, 10)
	}
	bypass = "off"
	if scope.Bypass {
		bypass = "on"
	}
	return tenant, bypass
}

// Scoped runs every statement in its own short transaction that sets the RLS
// scope first. One pgx Batch means one network round trip, so PgBouncer's
// transaction mode keeps working.
type Scoped struct {
	pool    *pgxpool.Pool
	resolve ScopeResolver
}

var _ DBTX = (*Scoped)(nil)

// NewScoped returns a DBTX whose statements are scoped from their context.
func NewScoped(pool *pgxpool.Pool, opts ...ScopeOption) *Scoped {
	return &Scoped{pool: pool, resolve: newScopeConfig(opts).resolve}
}

// Exec implements DBTX.
func (s *Scoped) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if s == nil || s.pool == nil {
		return pgconn.CommandTag{}, ErrNoPool
	}
	conn, results, err := s.begin(ctx, sql, args)
	if err != nil {
		releaseScoped(conn, results)
		return pgconn.CommandTag{}, err
	}

	var tag pgconn.CommandTag
	tag, err = results.Exec()
	if err != nil {
		err = fmt.Errorf("database: exec: %w", err)
	} else if _, commitErr := results.Exec(); commitErr != nil {
		err = fmt.Errorf("database: commit: %w", commitErr)
	}
	if closeErr := results.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("database: exec: %w", closeErr)
	}
	conn.Release()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return tag, nil
}

// Query implements DBTX. The returned rows keep the connection until Close,
// which reads the COMMIT result and releases it. Close cannot return an error,
// so callers must check Err (the usual Next/Err loop does) to observe a failed
// COMMIT.
func (s *Scoped) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	conn, results, err := s.begin(ctx, sql, args)
	if err != nil {
		releaseScoped(conn, results)
		return nil, err
	}
	rows, err := results.Query()
	if err != nil {
		releaseScoped(conn, results)
		return nil, err
	}
	return &scopedRows{Rows: rows, conn: conn, results: results}, nil
}

// QueryRow implements DBTX. Everything runs on the first Scan, which closes
// the rows and commits.
func (s *Scoped) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return scopedRow{scoped: s, ctx: ctx, sql: sql, args: args}
}

// begin acquires a connection and queues BEGIN, the scope settings, and sql.
func (s *Scoped) begin(ctx context.Context, sql string, args []any) (*pgxpool.Conn, pgx.BatchResults, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	tenant, bypass := scopeArgs(s.resolve(ctx))
	batch := &pgx.Batch{}
	batch.Queue("BEGIN")
	batch.Queue(setScopeSQL, tenant, bypass)
	batch.Queue(sql, args...)
	batch.Queue("COMMIT")
	results := conn.SendBatch(ctx, batch)
	// Consume BEGIN and the settings before returning the statement's results.
	if _, err := results.Exec(); err != nil {
		return conn, results, err
	}
	if _, err := results.Exec(); err != nil {
		return conn, results, err
	}
	return conn, results, nil
}

// releaseScoped closes a batch and releases its connection. pgxpool discards
// a connection that is still inside a transaction instead of pooling it.
func releaseScoped(conn *pgxpool.Conn, results pgx.BatchResults) {
	if results != nil {
		_ = results.Close()
	}
	if conn != nil {
		conn.Release()
	}
}

// scopedRows commits when the caller finishes reading. A failed query leaves
// the transaction aborted, so it is closed and the connection discarded.
type scopedRows struct {
	pgx.Rows
	conn    *pgxpool.Conn
	results pgx.BatchResults
	done    bool
	err     error
}

func (r *scopedRows) Err() error {
	if err := r.Rows.Err(); err != nil {
		r.err = err
		r.finish()
		return err
	}
	r.finish()
	return r.err
}

func (r *scopedRows) Close() {
	r.Rows.Close()
	r.finish()
}

func (r *scopedRows) finish() {
	if r.done {
		return
	}
	r.done = true
	if r.err == nil {
		if _, err := r.results.Exec(); err != nil { // COMMIT
			r.err = err
		}
	}
	_ = r.results.Close()
	r.conn.Release()
}

// scopedRow runs its query on Scan, which is the only pgx.Row method.
type scopedRow struct {
	scoped *Scoped
	ctx    context.Context
	sql    string
	args   []any
}

func (r scopedRow) Scan(dest ...any) error {
	rows, err := r.scoped.Query(r.ctx, r.sql, r.args...)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	scanErr := rows.Scan(dest...)
	// Close commits the batch and Err surfaces a failed COMMIT. Reading the
	// error here matters: a deferred Close alone would let a deferred
	// constraint violation (or any commit failure) look like a successful
	// INSERT ... RETURNING.
	rows.Close()
	if scanErr != nil {
		return scanErr
	}
	return rows.Err()
}
