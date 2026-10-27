package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// rollbackTimeout bounds the detached rollback in WithTx. pgx closes the
// connection when a rollback fails, so the pool never reuses a connection with
// an open transaction.
const rollbackTimeout = 5 * time.Second

// TxManager manages database transactions. Every transaction starts with the
// RLS scope resolved from its context, so a pooled connection is never reused
// with another tenant's settings. The pool is chosen once at BEGIN: the
// dedicated bypass role pool when the context (or its resolver) asks for
// bypass, the normal role otherwise.
type TxManager struct {
	pools   Pools
	resolve ScopeResolver
}

// NewTxManagerPools creates a TxManager that starts cross-tenant transactions
// on the bypass pool and every other transaction on the normal pool.
func NewTxManagerPools(pools Pools, opts ...ScopeOption) *TxManager {
	return &TxManager{pools: pools, resolve: newScopeConfig(opts).resolve}
}

// WithTx executes fn within a database transaction.
// If fn returns an error the transaction is rolled back; otherwise it is committed.
//
// The transaction runs on the bypass pool when ctx (or its resolver) requests
// bypass and that pool exists. That choice is fixed at BEGIN: marking a
// context with WithRLSBypass inside fn cannot move the transaction to another
// connection, so those statements keep running on the transaction's pool. On
// a normal transaction they stay tenant-scoped and see no foreign rows (fail
// closed). Callers that need every tenant must mark the context before
// WithTx.
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
	bypass := tm.resolve(ctx).Bypass || RLSBypass(ctx)
	pool, _ := tm.pools.forBypass(bypass)
	if pool == nil {
		return ErrNoPool
	}
	tx, err := pool.Begin(ctx)
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

	if _, err := tx.Exec(ctx, setScopeSQL, scopeArgs(tm.resolve(ctx))); err != nil {
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
