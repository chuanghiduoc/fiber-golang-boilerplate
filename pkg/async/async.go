package async

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
)

// tracker counts goroutines started by Go so shutdown can wait for them
// before closing the pool and cache they use. A mutex-guarded counter with a
// per-busy-period channel is used instead of sync.WaitGroup because Go may be
// called concurrently with Wait, which WaitGroup forbids at a zero count.
var tracker struct {
	mu      sync.Mutex
	running int
	idle    chan struct{} // closed when running drops back to zero
}

// Go runs fn in a new goroutine with panic recovery.
// Any panic is logged and does not crash the process.
func Go(fn func()) {
	tracker.mu.Lock()
	if tracker.running == 0 {
		tracker.idle = make(chan struct{})
	}
	tracker.running++
	tracker.mu.Unlock()

	go func() {
		defer release()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("async goroutine panicked",
					slog.Any("panic", r),
					slog.String("stack", string(debug.Stack())),
				)
			}
		}()
		fn()
	}()
}

func release() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.running--
	if tracker.running == 0 {
		close(tracker.idle)
	}
}

// Wait blocks until every goroutine started by Go has returned, or ctx is
// done. It returns ctx.Err() on timeout; the remaining goroutines keep running
// and the caller decides whether to proceed. Call it after the HTTP server
// has stopped so no new work is started.
func Wait(ctx context.Context) error {
	tracker.mu.Lock()
	if tracker.running == 0 {
		tracker.mu.Unlock()
		return nil
	}
	idle := tracker.idle
	tracker.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
