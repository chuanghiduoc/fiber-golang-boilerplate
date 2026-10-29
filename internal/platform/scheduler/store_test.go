package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
)

func TestPGStoreRejectsNilTxManager(t *testing.T) {
	store := &pgStore{pool: new(pgxpool.Pool)}
	run := runRecord{
		Schedule:     "test",
		ScheduledFor: time.Now().UTC(),
		Status:       runStatusEnqueued,
	}

	if _, err := store.EnqueueRun(context.Background(), noopQueue{}, run,
		jobs.Job{Kind: "test.job"}); !errors.Is(err, ErrNoTx) {
		t.Fatalf("EnqueueRun error = %v, want ErrNoTx", err)
	}

	run.Status = runStatusSkipped
	if _, err := store.SkipRun(context.Background(), run); !errors.Is(err, ErrNoTx) {
		t.Fatalf("SkipRun error = %v, want ErrNoTx", err)
	}
}
