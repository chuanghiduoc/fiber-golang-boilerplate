package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rollbackTimeout bounds the detached rollback in WithTx. pgx closes the
// connection when a rollback fails, so the pool never reuses a connection with
// an open transaction.
const rollbackTimeout = 5 * time.Second

// TxManager manages database transactions. Every transaction starts with the
// RLS scope resolved from its context, so a pooled connection is never reused
// with another tenant's settings.
type TxManager struct {
	pool    *pgxpool.Pool
	resolve ScopeResolver
}

// NewTxManager creates a new TxManager.
func NewTxManager(pool *pgxpool.Pool, opts ...ScopeOption) *TxManager {
	return &TxManager{pool: pool, resolve: newScopeConfig(opts).resolve}
}

// WithTx executes fn within a database transaction.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
//
// Usage in services:
//
//	err := txManager.WithTx(ctx, func(tx pgx.Tx) error {
//	    userRepo := repository.NewUserRepository(tx)
//	    fileRepo := filesrepo.NewFileRepository(tx)
//	    // ... transactional operations ...
//	    return nil
//	})
func (tm *TxManager) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	tx, err := tm.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Always roll back if the function panics or returns early. A panic used to
	// skip both Commit and Rollback and leak a pooled connection.
	defer func() {
		// The rollback must still run if the request context was canceled, but
		// bounded so a hung connection cannot block the caller forever.
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		rbErr := tx.Rollback(rbCtx)
		if rbErr == nil || errors.Is(rbErr, pgx.ErrTxClosed) {
			return
		}
		if err != nil {
			// Join so callers can still errors.Is/As the original business error
			// (e.g. apperror.ErrNotFound) while retaining the rollback failure.
			err = errors.Join(err, fmt.Errorf("rollback failed: %w", rbErr))
			return
		}
		err = fmt.Errorf("rollback failed: %w", rbErr)
	}()

	tenant, bypass := scopeArgs(tm.resolve(ctx))
	if _, err := tx.Exec(ctx, setScopeSQL, tenant, bypass); err != nil {
		return fmt.Errorf("set transaction scope: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}
