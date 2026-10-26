package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartModuleLoopsJoinsEveryLoop proves the shutdown owner receives a
// completion signal only after every module background loop has returned. The
// loops share the worker context, so canceling it must unblock them all; the
// old unjoined goroutines left no way to know when they were done, and the
// database pools could close underneath them.
func TestStartModuleLoopsJoinsEveryLoop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan string, 2)
	exited := make(chan string, 2)
	loops := []func(context.Context){
		func(ctx context.Context) {
			started <- "webhooks"
			<-ctx.Done()
			exited <- "webhooks"
		},
		func(ctx context.Context) {
			started <- "other"
			<-ctx.Done()
			exited <- "other"
		},
	}

	done := startModuleLoops(ctx, loops)
	require.NotNil(t, done)

	for range loops {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("a module background loop did not start")
		}
	}

	select {
	case <-done:
		t.Fatal("loops were reported done before their context was canceled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loops were not joined after cancellation")
	}

	// Every loop must have run its deferred cleanup before done closed.
	seen := map[string]bool{}
	for range loops {
		select {
		case name := <-exited:
			seen[name] = true
		default:
			t.Fatalf("done closed before every loop exited: %v", seen)
		}
	}
	assert.Len(t, seen, len(loops))
}

// TestStartModuleLoopsNoLoops proves a process without module loops (or the
// APP_ROLE=api path) does not wait for anything.
func TestStartModuleLoopsNoLoops(t *testing.T) {
	t.Parallel()

	done := startModuleLoops(context.Background(), nil)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("an empty loop set never completes")
	}
}

// TestStartModuleLoopsWaitsForIndependentExit proves a loop that returns on
// its own still counts toward completion, so one finished loop cannot leave the
// join hanging.
func TestStartModuleLoopsWaitsForIndependentExit(t *testing.T) {
	t.Parallel()

	var ran atomic.Int32
	done := startModuleLoops(context.Background(), []func(context.Context){
		func(context.Context) { ran.Add(1) },
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a self-terminating loop was not joined")
	}
	assert.Equal(t, int32(1), ran.Load())
}
