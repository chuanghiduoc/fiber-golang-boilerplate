// Package fakedb provides a scriptable database handle and result rows for
// repository unit tests. It is test-only support: production code never
// imports it.
package fakedb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Call is one recorded Exec or Query invocation.
type Call struct {
	SQL  string
	Args []any
}

// DB implements the sqlc DBTX interface with per-call hooks. When a hook is
// nil the call is a no-op (Exec reports zero rows, Query returns no rows).
type DB struct {
	ExecCalls  []Call
	QueryCalls []Call

	ExecFn     func(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryFn    func(context.Context, string, ...any) (pgx.Rows, error)
	QueryRowFn func(context.Context, string, ...any) pgx.Row
}

// Exec records the call, then delegates to ExecFn when set.
func (d *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.ExecCalls = append(d.ExecCalls, Call{SQL: sql, Args: args})
	if d.ExecFn != nil {
		return d.ExecFn(ctx, sql, args...)
	}
	return Deleted(0), nil
}

// Query records the call, then delegates to QueryFn when set.
func (d *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.QueryCalls = append(d.QueryCalls, Call{SQL: sql, Args: args})
	if d.QueryFn != nil {
		return d.QueryFn(ctx, sql, args...)
	}
	return NewRows(), nil
}

// QueryRow records the call, then delegates to QueryRowFn when set.
func (d *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if d.QueryRowFn != nil {
		return d.QueryRowFn(ctx, sql, args...)
	}
	return errorRow{err: fmt.Errorf("fakedb: QueryRow not scripted")}
}

// Deleted returns a command tag reporting rows affected by a delete.
func Deleted(rows int64) pgconn.CommandTag {
	return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", rows))
}

type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

// Rows is an in-memory pgx.Rows implementation. Data holds one slice per
// row, positionally matching the query's selected columns.
type Rows struct {
	Data   [][]any
	ErrVal error

	pos    int
	closed bool
}

// NewRows returns rows over values.
func NewRows(values ...[]any) *Rows {
	return &Rows{Data: values}
}

func (r *Rows) Close() {
	r.closed = true
}

func (r *Rows) Err() error {
	return r.ErrVal
}

func (r *Rows) CommandTag() pgconn.CommandTag {
	return pgconn.NewCommandTag(fmt.Sprintf("SELECT %d", len(r.Data)))
}

func (r *Rows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (r *Rows) Next() bool {
	if r.closed || r.pos >= len(r.Data) {
		r.closed = true
		return false
	}
	r.pos++
	return true
}

func (r *Rows) Scan(dest ...any) error {
	if r.pos == 0 || r.pos > len(r.Data) {
		return fmt.Errorf("fakedb: Scan called without a row")
	}
	row := r.Data[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("fakedb: Scan got %d destinations for %d columns", len(dest), len(row))
	}
	for i := range dest {
		if err := assign(dest[i], row[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *Rows) Values() ([]any, error) {
	if r.pos == 0 || r.pos > len(r.Data) {
		return nil, fmt.Errorf("fakedb: Values called without a row")
	}
	return r.Data[r.pos-1], nil
}

func (r *Rows) RawValues() [][]byte { return nil }

func (r *Rows) Conn() *pgx.Conn { return nil }

func (r *Rows) TypeMap() *pgtype.Map { return nil }

func assign(dest, value any) error {
	switch d := dest.(type) {
	case *int64:
		v, ok := value.(int64)
		if !ok {
			return fmt.Errorf("fakedb: cannot scan %T into *int64", value)
		}
		*d = v
	case *string:
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("fakedb: cannot scan %T into *string", value)
		}
		*d = v
	default:
		return fmt.Errorf("fakedb: unsupported scan destination %T", dest)
	}
	return nil
}
