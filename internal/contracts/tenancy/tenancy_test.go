package tenancy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCheckPurgeBudgetMapsOnlyDeadlineToResumableStop(t *testing.T) {
	if err := CheckPurgeBudget(context.Background()); err != nil {
		t.Fatalf("background budget = %v, want nil", err)
	}

	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	if err := CheckPurgeBudget(deadline); !errors.Is(err, ErrPurgeBudgetExceeded) {
		t.Fatalf("deadline budget = %v, want ErrPurgeBudgetExceeded", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err := CheckPurgeBudget(cancelled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled budget = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrPurgeBudgetExceeded) {
		t.Fatal("a parent cancellation must not be reported as a resumable purge budget")
	}
}
