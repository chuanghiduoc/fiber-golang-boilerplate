// Package audit records security-relevant actions in an append-only trail.
//
// The package is deliberately dependency-light: the [Recorder] interface keeps
// callers independent of storage, [Noop] is a safe default when auditing is
// disabled, and [PostgresRecorder] is the production implementation.
package audit

import (
	"context"
	"errors"
	"time"
)

// ErrInvalidEvent is returned (wrapped) when an [Event] cannot be recorded.
var ErrInvalidEvent = errors.New("audit: invalid event")

// Event describes a single auditable action.
//
// ActorID is the acting user, or 0 for anonymous/system actions. Metadata must
// be JSON-marshalable and should never contain secrets. CreatedAt defaults to
// the time the event is recorded when left zero.
type Event struct {
	ActorID    int64
	Action     string
	Resource   string
	ResourceID string
	Metadata   map[string]any
	IP         string
	UserAgent  string
	RequestID  string
	CreatedAt  time.Time
}

// Recorder persists audit events.
//
// Implementations must be safe for concurrent use.
type Recorder interface {
	Record(ctx context.Context, event Event) error
}

// Noop discards every event. It is useful in tests and when auditing is
// disabled by configuration.
type Noop struct{}

var _ Recorder = Noop{}

// Record implements [Recorder] and always succeeds.
func (Noop) Record(context.Context, Event) error {
	return nil
}
