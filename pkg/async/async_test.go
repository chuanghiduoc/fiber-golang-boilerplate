package async

import (
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
