package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TxManager manages database transactions.
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager creates a new TxManager.
func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
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
		// The rollback must still run if the request context was canceled.
		rbErr := tx.Rollback(context.WithoutCancel(ctx))
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

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}
