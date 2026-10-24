// Package webhooks defines the transport-neutral event and dispatcher types
// that business modules use to emit webhook-triggering events without
// importing the webhooks module's DTOs, services, repository, or SQL.
//
// The package is deliberately stdlib-only. The webhooks module provides the
// production Dispatcher; other modules depend only on Event and Dispatcher.
package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// EventFileUploaded is emitted after a file is stored.
	EventFileUploaded = "file.uploaded"
	// EventFileDeleted is emitted after a file is soft-deleted.
	EventFileDeleted = "file.deleted"
	// EventFileRestored is emitted after a soft-deleted file is restored.
	EventFileRestored = "file.restored"
	// EventUserRegistered is emitted after a user account is created.
	EventUserRegistered = "user.registered"

	maxEventIDLen   = 128
	maxEventTypeLen = 128
)

var (
	// ErrInvalidEvent is returned (wrapped) when an Event cannot be dispatched.
	ErrInvalidEvent = errors.New("webhooks: invalid event")
	// ErrInvalidDispatcherEvent is returned by Dispatcher implementations when
	// an event cannot be validated.
	ErrInvalidDispatcherEvent = ErrInvalidEvent
)

// Event is a domain fact worth delivering to tenant-configured endpoints.
//
// ID uniquely identifies the event for delivery deduplication. Callers may
// leave it empty; dispatchers assign one before persisting deliveries. Type is
// a stable dotted name such as "file.uploaded". TenantID may be left zero to
// resolve the request tenant from the context. Data must be valid JSON and is
// delivered as the event's "data" member.
type Event struct {
	ID         string
	Type       string
	TenantID   int64
	OccurredAt time.Time
	Data       json.RawMessage
}

// Validate checks the event fields that do not depend on a dispatcher's clock
// or tenant resolution. Empty IDs are allowed (the dispatcher assigns one) and
// empty Data defaults to an empty JSON object.
func (e Event) Validate() error {
	if strings.TrimSpace(e.Type) == "" {
		return fmt.Errorf("%w: type is required", ErrInvalidEvent)
	}
	if len(e.Type) > maxEventTypeLen {
		return fmt.Errorf("%w: type exceeds %d characters", ErrInvalidEvent, maxEventTypeLen)
	}
	if !ValidType(e.Type) {
		return fmt.Errorf("%w: type must match [a-z0-9._-]", ErrInvalidEvent)
	}
	if strings.Contains(e.Type, "*") {
		return fmt.Errorf("%w: type must be a concrete event name, not a pattern", ErrInvalidEvent)
	}
	if e.ID != "" && (len(e.ID) > maxEventIDLen || !ValidID(e.ID)) {
		return fmt.Errorf("%w: id is invalid", ErrInvalidEvent)
	}
	if e.TenantID < 0 {
		return fmt.Errorf("%w: tenant id must not be negative", ErrInvalidEvent)
	}
	if len(e.Data) > 0 && !json.Valid(e.Data) {
		return fmt.Errorf("%w: data must be valid JSON", ErrInvalidEvent)
	}
	return nil
}

// Normalized returns a copy with defaults applied: an empty Data becomes `{}`
// and a zero OccurredAt becomes now. It does not resolve TenantID or ID.
func (e Event) Normalized(now time.Time) Event {
	if len(e.Data) == 0 {
		e.Data = json.RawMessage(`{}`)
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = now.UTC()
	}
	return e
}

// Dispatcher delivers domain events to the tenant's matching webhook
// endpoints. Dispatch must be safe to retry: an event ID already dispatched to
// an endpoint must not create a second delivery. The returned count is the
// number of newly created deliveries.
type Dispatcher interface {
	Dispatch(ctx context.Context, events ...Event) (int, error)
}

// Noop is a Dispatcher that discards every event. It is the safe default when
// the webhooks module is absent (tests and minimal deployments).
type Noop struct{}

var _ Dispatcher = Noop{}

// Dispatch implements Dispatcher and always succeeds.
func (Noop) Dispatch(context.Context, ...Event) (int, error) { return 0, nil }

// ValidID reports whether s is a stable event/delivery identifier suitable for
// a header: 1-128 characters from [A-Za-z0-9._:-].
func ValidID(s string) bool {
	if s == "" || len(s) > maxEventIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= 'A' && ch <= 'Z':
		case ch >= '0' && ch <= '9':
		case ch == '.', ch == '_', ch == ':', ch == '-':
		default:
			return false
		}
	}
	return true
}

// Matches reports whether eventType matches any of patterns. Patterns are
// exact event names, a trailing-wildcard prefix such as "file.*", or "*".
func Matches(patterns []string, eventType string) bool {
	for _, pattern := range patterns {
		if Match(pattern, eventType) {
			return true
		}
	}
	return false
}

// Match reports whether a single pattern matches eventType.
func Match(pattern, eventType string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "*" {
		return eventType != ""
	}
	if pattern == eventType {
		return eventType != ""
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return eventType != "" && strings.HasPrefix(eventType, prefix) && len(eventType) > len(prefix)
	}
	return false
}

// ValidType reports whether s is a stable lowercase event/pattern name.
// Wildcard patterns "file.*" and "*" are accepted in addition to full names.
func ValidType(s string) bool {
	if s == "*" {
		return true
	}
	if s == "" || len(s) > maxEventTypeLen {
		return false
	}
	body := s
	if strings.HasSuffix(s, ".*") {
		body = strings.TrimSuffix(s, ".*")
		if body == "" {
			return false
		}
	}
	for i := 0; i < len(body); i++ {
		ch := body[i]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '.', ch == '_', ch == '-':
		default:
			return false
		}
	}
	return true
}

// NormalizePatterns trims, validates, and deduplicates event patterns while
// preserving their order. It returns an empty slice when every entry is
// invalid; callers decide whether that is an error or an all-events default.
func NormalizePatterns(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if !ValidType(pattern) {
			continue
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		out = append(out, pattern)
	}
	return out
}
