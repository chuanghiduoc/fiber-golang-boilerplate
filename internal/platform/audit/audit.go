// Package audit records security-relevant actions in an append-only trail.
//
// The package is deliberately dependency-light: the [Recorder] interface keeps
// callers independent of storage, [Noop] is a safe default when auditing is
// disabled, and [PostgresRecorder] is the production implementation.
//
// Mutations are audited in two tiers:
//
//   - Tier 1 services write a terminal event with [TxRecorder.RecordTx] inside
//     the same database transaction as the mutation. If the audit insert
//     fails, the mutation rolls back.
//   - Tier 2 routes use [Guard], which writes a pre-write intent row
//     (outcome=started) before the handler runs and completes it with the
//     observed outcome afterwards. A required Guard fails closed with
//     system.audit_unavailable when the intent cannot be stored.
package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidEvent is returned (wrapped) when an [Event] cannot be recorded.
var ErrInvalidEvent = errors.New("audit: invalid event")

// ErrAlreadyCompleted is returned when a completion targets a row that is not
// a started HTTP intent row (it was already completed or is not completable).
var ErrAlreadyCompleted = errors.New("audit: event is not a started http intent")

// ErrTxRequired is returned by [PostgresRecorder.RecordTx] when no transaction
// was supplied. A missing transaction is a programming error: it would turn a
// transactional audit guarantee into a best-effort write.
var ErrTxRequired = errors.New("audit: transaction is required")

// Actor types recorded in audit_events.actor_type.
const (
	ActorTypeUser      = "user"
	ActorTypeService   = "service"
	ActorTypeJob       = "job"
	ActorTypeSystem    = "system"
	ActorTypeAnonymous = "anonymous"
	ActorTypeUnknown   = "unknown"
)

// Event sources recorded in audit_events.source.
const (
	SourceHTTP    = "http"
	SourceService = "service"
	SourceJob     = "job"
	SourceSystem  = "system"
)

// Outcomes recorded in audit_events.outcome. "started" is only valid for a
// pre-write intent row; [Recorder] and [TxRecorder] accept terminal outcomes
// only.
const (
	OutcomeStarted   = "started"
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeDenied    = "denied"
	OutcomeUnknown   = "unknown"
)

// Event describes a single auditable action.
//
// TenantID is the owning tenant. A zero value is resolved from the recording
// context (falling back to the compatibility default tenant). ActorID is the
// acting user, or 0 for anonymous/system actions. Metadata must be
// JSON-marshalable and is sanitized before storage. CreatedAt defaults to the
// time the event is recorded when left zero.
type Event struct {
	TenantID           int64
	ParentID           int64
	ActorID            int64
	ActorType          string
	ActorRole          string
	Action             string
	Resource           string
	ResourceID         string
	Metadata           map[string]any
	IP                 string
	UserAgent          string
	RequestID          string
	Source             string
	Outcome            string
	StatusCode         int
	ErrorCode          string
	IdempotencyKeyHash string
	CompletedAt        time.Time
	CreatedAt          time.Time
}

// Recorder persists terminal audit events.
//
// Implementations must be safe for concurrent use.
type Recorder interface {
	Record(ctx context.Context, event Event) error
}

// TxRecorder persists a terminal audit event through a caller-owned
// transaction. The event commits or rolls back with the mutation. It also
// embeds [Recorder] so a service without a transaction manager can degrade to
// a non-transactional write (and report it through metrics).
type TxRecorder interface {
	Recorder
	RecordTx(ctx context.Context, tx pgx.Tx, event Event) error
}

// IDRecorder persists a pre-write intent event and returns its row ID.
type IDRecorder interface {
	RecordID(ctx context.Context, event Event) (int64, error)
}

// Completion describes the observed outcome of a guarded request.
type Completion struct {
	ActorID     int64
	ActorType   string
	ActorRole   string
	Outcome     string
	StatusCode  int
	ErrorCode   string
	CompletedAt time.Time
}

// CompletionRecorder completes a started HTTP intent row.
type CompletionRecorder interface {
	Complete(ctx context.Context, id int64, completion Completion) error
}

// MetadataLocalKey is the Fiber locals key handlers may set to a
// map[string]any. The audit middleware merges it into the event metadata for
// successful mutations. Values must be JSON-marshalable and must never contain
// secrets such as plaintext tokens or API keys.
const MetadataLocalKey = "audit_metadata"

// WriteTimeout bounds each audit write the HTTP layer makes on a detached
// context: the Guard intent and completion rows and the Middleware fallback
// row. Completion and fallback run after the handler returned, so a request
// can outlive its APP_REQUEST_TIMEOUT by up to one WriteTimeout (a guarded
// route never also gets a fallback row).
const WriteTimeout = 2 * time.Second

// Noop discards every event. It is useful in tests and when auditing is
// disabled by configuration.
//
// Noop deliberately does not implement [IDRecorder]. A Guard in required mode
// therefore fails closed when it is wired with Noop instead of silently
// accepting mutations without a durable intent row.
type Noop struct{}

var (
	_ Recorder   = Noop{}
	_ TxRecorder = Noop{}
)

// Record implements [Recorder] and always succeeds.
func (Noop) Record(context.Context, Event) error {
	return nil
}

// RecordTx implements [TxRecorder] and always succeeds.
func (Noop) RecordTx(context.Context, pgx.Tx, Event) error {
	return nil
}

// Validate reports whether the event has the fields required by the schema and
// a terminal outcome. An empty outcome defaults to succeeded.
func (e Event) Validate() error {
	if strings.TrimSpace(e.Action) == "" {
		return fmt.Errorf("%w: action is required", ErrInvalidEvent)
	}
	if strings.TrimSpace(e.Resource) == "" {
		return fmt.Errorf("%w: resource is required", ErrInvalidEvent)
	}

	outcome := e.EffectiveOutcome()
	if outcome == OutcomeStarted {
		return fmt.Errorf("%w: started is only valid for a pre-write intent", ErrInvalidEvent)
	}
	if !validOutcome(outcome) {
		return fmt.Errorf("%w: unknown outcome %q", ErrInvalidEvent, outcome)
	}
	if source := e.EffectiveSource(); !validSource(source) {
		return fmt.Errorf("%w: unknown source %q", ErrInvalidEvent, source)
	}
	if actorType := e.EffectiveActorType(); !validActorType(actorType) {
		return fmt.Errorf("%w: unknown actor type %q", ErrInvalidEvent, actorType)
	}
	if e.StatusCode != 0 && (e.StatusCode < 100 || e.StatusCode > 599) {
		return fmt.Errorf("%w: status code %d is out of range", ErrInvalidEvent, e.StatusCode)
	}
	return nil
}

// EffectiveOutcome returns the stored outcome, defaulting to succeeded.
func (e Event) EffectiveOutcome() string {
	if e.Outcome == "" {
		return OutcomeSucceeded
	}
	return e.Outcome
}

// EffectiveSource returns the stored source, defaulting to http.
func (e Event) EffectiveSource() string {
	if e.Source == "" {
		return SourceHTTP
	}
	return e.Source
}

// EffectiveActorType returns the stored actor type, defaulting to unknown.
func (e Event) EffectiveActorType() string {
	if e.ActorType == "" {
		return ActorTypeUnknown
	}
	return e.ActorType
}

func (e Event) validateIntent() error {
	if strings.TrimSpace(e.Action) == "" {
		return fmt.Errorf("%w: action is required", ErrInvalidEvent)
	}
	if strings.TrimSpace(e.Resource) == "" {
		return fmt.Errorf("%w: resource is required", ErrInvalidEvent)
	}
	if e.Outcome != "" && e.Outcome != OutcomeStarted {
		return fmt.Errorf("%w: intent outcome must be started", ErrInvalidEvent)
	}
	if source := e.EffectiveSource(); !validSource(source) {
		return fmt.Errorf("%w: unknown source %q", ErrInvalidEvent, source)
	}
	if actorType := e.EffectiveActorType(); !validActorType(actorType) {
		return fmt.Errorf("%w: unknown actor type %q", ErrInvalidEvent, actorType)
	}
	return nil
}

// ValidateTerminalCompletion rejects started/non-terminal completion outcomes.
func (c Completion) Validate() error {
	outcome := c.Outcome
	if outcome == "" {
		outcome = OutcomeSucceeded
	}
	if !validTerminalOutcome(outcome) {
		return fmt.Errorf("%w: completion outcome %q is not terminal", ErrInvalidEvent, outcome)
	}
	if c.StatusCode != 0 && (c.StatusCode < 100 || c.StatusCode > 599) {
		return fmt.Errorf("%w: status code %d is out of range", ErrInvalidEvent, c.StatusCode)
	}
	if actorType := c.ActorType; actorType != "" && !validActorType(actorType) {
		return fmt.Errorf("%w: unknown actor type %q", ErrInvalidEvent, actorType)
	}
	return nil
}

func validTerminalOutcome(outcome string) bool {
	switch outcome {
	case OutcomeSucceeded, OutcomeFailed, OutcomeDenied, OutcomeUnknown:
		return true
	default:
		return false
	}
}

func validOutcome(outcome string) bool {
	return outcome == OutcomeStarted || validTerminalOutcome(outcome)
}

func validSource(source string) bool {
	switch source {
	case SourceHTTP, SourceService, SourceJob, SourceSystem:
		return true
	default:
		return false
	}
}

func validActorType(actorType string) bool {
	switch actorType {
	case ActorTypeUser, ActorTypeService, ActorTypeJob, ActorTypeSystem,
		ActorTypeAnonymous, ActorTypeUnknown:
		return true
	default:
		return false
	}
}

// ActorTypeForRole maps an authenticated role to an actor type. Every known
// role is a human user; the role itself is recorded in actor_role, so an
// admin is actor_type user with actor_role admin.
func ActorTypeForRole(role string) string {
	switch strings.ToLower(role) {
	case "owner", "admin", "member", "platform_admin", "user":
		return ActorTypeUser
	default:
		return ActorTypeUnknown
	}
}
