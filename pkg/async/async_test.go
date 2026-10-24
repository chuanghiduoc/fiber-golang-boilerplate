package async

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGo_Runs(t *testing.T) {
	done := make(chan struct{})
	Go(func() {
		close(done)
	})

	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Fatal("function did not execute within timeout")
	}
}

func TestGo_RecoversPanic(t *testing.T) {
	done := make(chan struct{})
	Go(func() {
		defer close(done)
		panic("test panic")
	})

	select {
	case <-done:
		// success — panic was recovered, goroutine completed
	case <-time.After(1 * time.Second):
		t.Fatal("panicking goroutine did not recover within timeout")
	}
}

func TestWait_BlocksUntilGoroutinesFinish(t *testing.T) {
	unblock := make(chan struct{})
	finished := make(chan struct{})
	Go(func() {
		<-unblock
		close(finished)
	})

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with a running goroutine = %v, want deadline exceeded", err)
	}

	close(unblock)
	if err := Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Wait returned before the goroutine finished")
	}
}

func TestWait_NoGoroutinesReturnsImmediately(t *testing.T) {
	if err := Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx); err != nil {
		t.Fatalf("Wait with nothing running = %v, want nil", err)
	}
}

func TestWait_ConcurrentGo(t *testing.T) {
	var wg sync.WaitGroup
	var ran atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Go(func() { ran.Add(1) })
		}()
	}
	wg.Wait() // every Go call has registered its goroutine
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Wait(ctx); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	if got := ran.Load(); got != 100 {
		t.Fatalf("ran = %d, want 100", got)
	}
}
