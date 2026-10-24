// Package events provides a typed in-process event bus with optional durable
// delivery through the PostgreSQL jobs queue. Local handlers run immediately
// in the publisher's goroutine; durable subscriptions enqueue one idempotent
// job per event and handler so delivery survives a process restart.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidEvent is returned (wrapped) when event metadata or data is invalid.
var ErrInvalidEvent = errors.New("events: invalid event")

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Event is the unit delivered to local and durable handlers. TenantID,
// RequestID, and ActorID are populated from the publishing context unless the
// caller set them explicitly.
type Event struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	TenantID   int64           `json:"tenantId"`
	RequestID  string          `json:"requestId,omitempty"`
	ActorID    int64           `json:"actorId,omitempty"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// NewEvent builds an event with a fresh id and timestamp. data may be any
// JSON-marshalable value; a json.RawMessage is embedded verbatim after
// validation, and nil becomes an empty object.
func NewEvent(name string, data any) (Event, error) {
	event := Event{
		ID:         uuid.NewString(),
		Name:       name,
		OccurredAt: time.Now().UTC(),
		Data:       json.RawMessage(`{}`),
	}
	switch value := data.(type) {
	case nil:
		// Metadata-only event; Data already defaults to {}.
	case json.RawMessage:
		if len(value) > 0 {
			event.Data = append(json.RawMessage(nil), value...)
		}
	case []byte:
		if len(value) > 0 {
			event.Data = append(json.RawMessage(nil), value...)
		}
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return Event{}, fmt.Errorf("%w: marshal data: %w", ErrInvalidEvent, err)
		}
		event.Data = raw
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

// Validate checks the event name, id, timestamp, tenant, and JSON data.
func (e Event) Validate() error {
	if !namePattern.MatchString(e.Name) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalidEvent, e.Name, namePattern)
	}
	if e.ID != "" && !idPattern.MatchString(e.ID) {
		return fmt.Errorf("%w: id %q must match %s", ErrInvalidEvent, e.ID, idPattern)
	}
	if e.TenantID < 0 {
		return fmt.Errorf("%w: tenant id must not be negative", ErrInvalidEvent)
	}
	if e.ActorID < 0 {
		return fmt.Errorf("%w: actor id must not be negative", ErrInvalidEvent)
	}
	if len(e.Data) > 0 && !json.Valid(e.Data) {
		return fmt.Errorf("%w: data must be valid JSON", ErrInvalidEvent)
	}
	return nil
}

// Decode unmarshals the event data into T. An empty data payload decodes as the
// zero value, which keeps metadata-only events ergonomic.
func Decode[T any](event Event) (T, error) {
	var value T
	if len(event.Data) == 0 {
		return value, nil
	}
	if err := json.Unmarshal(event.Data, &value); err != nil {
		return value, fmt.Errorf("events: decode %q data: %w", event.Name, err)
	}
	return value, nil
}

func cleanName(value string) string {
	return strings.TrimSpace(value)
}
