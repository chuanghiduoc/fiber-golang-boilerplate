// Package events provides a typed in-process event bus with optional durable
// delivery through the PostgreSQL jobs queue. Local handlers run immediately
// in the publisher's goroutine; durable subscriptions enqueue one idempotent
// job per event and handler so delivery survives a process restart.
//
// Every event uses one versioned envelope:
//
//	{
//	  "id":          "0192f1d2-...",          // UUIDv7, unique per event
//	  "type":        "file.uploaded",         // stable lowercase dotted name
//	  "dataVersion": 1,                       // version of the data schema
//	  "occurredAt":  "2026-10-25T08:00:00Z",  // UTC
//	  "tenantId":    1,
//	  "actor":       {"type": "user", "id": "42"},
//	  "data":        { ... }                  // per-type payload struct
//	}
//
// The envelope maps 1:1 onto CloudEvents 1.0 attributes: id -> id,
// type -> type, occurredAt -> time, data -> data (datacontenttype
// application/json), tenantId -> source "/tenants/<tenantId>" plus the
// extension "tenantid", dataVersion -> extension "dataversion", and
// actor.type/actor.id -> extensions "actortype"/"actorid". specversion is
// always "1.0". A future broker relay can therefore emit CloudEvents without
// touching producers.
//
// Payloads evolve additively: adding an optional field keeps dataVersion.
// Removing, renaming, or retyping a field, or changing its meaning, is a
// breaking change and needs a new dataVersion (or a new type).
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidEvent is returned (wrapped) when event metadata or data is invalid.
var ErrInvalidEvent = errors.New("events: invalid event")

// Actor types. The set is closed so consumers can switch on it.
const (
	// ActorUser is an authenticated (or just-registered) user; Actor.ID is
	// the user id.
	ActorUser = "user"
	// ActorService is a non-human caller such as an API key; Actor.ID names it.
	ActorService = "service"
	// ActorSystem is the application itself (jobs, schedules, maintenance).
	ActorSystem = "system"
)

const maxActorIDLen = 128

var (
	// namePattern validates handler names and job kinds.
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	// typePattern validates event types: lowercase, dotted, header- and
	// pattern-safe (webhook endpoint subscriptions match on it).
	typePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Actor identifies who caused an event.
type Actor struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// UserActor returns the actor for user id.
func UserActor(id int64) Actor {
	return Actor{Type: ActorUser, ID: strconv.FormatInt(id, 10)}
}

// Event is the versioned envelope delivered to local and durable handlers and,
// through the webhooks module, to external receivers. TenantID, Actor, and
// RequestID are populated from the publishing context unless the caller set
// them explicitly.
type Event struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	DataVersion int             `json:"dataVersion"`
	OccurredAt  time.Time       `json:"occurredAt"`
	TenantID    int64           `json:"tenantId"`
	Actor       Actor           `json:"actor"`
	Data        json.RawMessage `json:"data"`
	// RequestID correlates durable handling with the publishing request. It
	// is process-internal metadata, not part of the public envelope: webhook
	// deliveries never carry it.
	RequestID string `json:"requestId,omitempty"`
}

// NewEvent builds an event with a fresh UUIDv7 id and timestamp. data may be
// any JSON-marshalable value; a json.RawMessage is embedded verbatim after
// validation, and nil becomes an empty object. dataVersion must be >= 1.
func NewEvent(eventType string, dataVersion int, data any) (Event, error) {
	id, err := newID()
	if err != nil {
		return Event{}, err
	}
	event := Event{
		ID:          id,
		Type:        eventType,
		DataVersion: dataVersion,
		OccurredAt:  time.Now().UTC(),
		Data:        json.RawMessage(`{}`),
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

// newID returns a UUIDv7: time-ordered, so ids sort by creation and index
// well, and still unique across processes.
func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("events: generate event id: %w", err)
	}
	return id.String(), nil
}

// Validate checks the envelope. An empty ID or actor is allowed here because
// the bus fills them from the context before publishing.
func (e Event) Validate() error {
	if !typePattern.MatchString(e.Type) {
		return fmt.Errorf("%w: type %q must match %s", ErrInvalidEvent, e.Type, typePattern)
	}
	if e.DataVersion < 1 {
		return fmt.Errorf("%w: %s dataVersion must be >= 1", ErrInvalidEvent, e.Type)
	}
	if e.ID != "" && !idPattern.MatchString(e.ID) {
		return fmt.Errorf("%w: id %q must match %s", ErrInvalidEvent, e.ID, idPattern)
	}
	if e.TenantID < 0 {
		return fmt.Errorf("%w: tenant id must not be negative", ErrInvalidEvent)
	}
	if err := e.Actor.validate(); err != nil {
		return err
	}
	if len(e.Data) > 0 && !json.Valid(e.Data) {
		return fmt.Errorf("%w: data must be valid JSON", ErrInvalidEvent)
	}
	return nil
}

func (a Actor) validate() error {
	switch a.Type {
	case "":
		if a.ID != "" {
			return fmt.Errorf("%w: actor id without a type", ErrInvalidEvent)
		}
	case ActorUser, ActorService:
		if a.ID == "" || len(a.ID) > maxActorIDLen {
			return fmt.Errorf("%w: %s actor needs an id of at most %d bytes", ErrInvalidEvent, a.Type, maxActorIDLen)
		}
	case ActorSystem:
		if a.ID != "" {
			return fmt.Errorf("%w: system actor has no id", ErrInvalidEvent)
		}
	default:
		return fmt.Errorf("%w: unknown actor type %q", ErrInvalidEvent, a.Type)
	}
	return nil
}

// userID returns the actor's user id, or 0 when it is not a user actor.
func (a Actor) userID() int64 {
	if a.Type != ActorUser {
		return 0
	}
	id, err := strconv.ParseInt(a.ID, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// Decode unmarshals the event data into T. An empty data payload decodes as the
// zero value, which keeps metadata-only events ergonomic.
func Decode[T any](event Event) (T, error) {
	var value T
	if len(event.Data) == 0 {
		return value, nil
	}
	if err := json.Unmarshal(event.Data, &value); err != nil {
		return value, fmt.Errorf("events: decode %q data: %w", event.Type, err)
	}
	return value, nil
}

func cleanName(value string) string {
	return strings.TrimSpace(value)
}
