// Package scheduler runs registered interval and cron schedules on a single
// leader, using PostgreSQL for the leader lease and a unique run ledger. Every
// due slot is enqueued as a durable jobs job; no schedule survives only in
// memory.
//
// Cron day matching: when both day-of-month and day-of-week are restricted, a
// day matches if either field matches (classic cron OR rule). Only a literal
// "*" counts as unrestricted; a stepped wildcard such as "*/2" is a
// restriction, so "0 0 */2 * 1" runs on odd days of the month OR on Mondays.
package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrInvalidSchedule is returned (wrapped) when a Spec is malformed.
	ErrInvalidSchedule = errors.New("scheduler: invalid schedule")
	// ErrDuplicateSchedule is returned when two schedules share a name.
	ErrDuplicateSchedule = errors.New("scheduler: duplicate schedule")
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// CatchupPolicy controls what happens to slots missed while no scheduler was
// leader.
type CatchupPolicy string

const (
	// CatchupNone skips missed slots and waits for the next boundary.
	CatchupNone CatchupPolicy = "none"
	// CatchupLatest enqueues only the most recent missed slot.
	CatchupLatest CatchupPolicy = "latest"
	// CatchupAll enqueues every missed slot, bounded by CatchupLimit per pass.
	CatchupAll CatchupPolicy = "all"
)

// ParseCatchup converts a config value to a CatchupPolicy.
func ParseCatchup(value string) (CatchupPolicy, error) {
	switch CatchupPolicy(strings.ToLower(strings.TrimSpace(value))) {
	case CatchupNone:
		return CatchupNone, nil
	case CatchupLatest:
		return CatchupLatest, nil
	case CatchupAll:
		return CatchupAll, nil
	default:
		return "", fmt.Errorf("%w: catch-up must be none, latest, or all (got %q)", ErrInvalidSchedule, value)
	}
}

// Spec declares one schedule. Exactly one of Interval or Cron must be set.
// Interval schedules align to Unix epoch boundaries; Cron expressions are
// standard 5-field expressions evaluated in UTC. A nil TenantID lets the
// queue resolve the context tenant when the job is inserted.
type Spec struct {
	Name        string
	Interval    time.Duration
	Cron        string
	Kind        string
	TenantID    uuid.UUID
	Payload     json.RawMessage
	MaxAttempts int
}

// Validate checks a Spec without registering it.
func (s Spec) Validate() error {
	if !namePattern.MatchString(strings.TrimSpace(s.Name)) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalidSchedule, s.Name, namePattern)
	}
	if !namePattern.MatchString(strings.TrimSpace(s.Kind)) {
		return fmt.Errorf("%w: job kind %q must match %s", ErrInvalidSchedule, s.Kind, namePattern)
	}
	if s.MaxAttempts < 0 {
		return fmt.Errorf("%w: max attempts must not be negative", ErrInvalidSchedule)
	}
	if len(s.Payload) > 0 && !json.Valid(s.Payload) {
		return fmt.Errorf("%w: payload must be valid JSON", ErrInvalidSchedule)
	}

	hasInterval := s.Interval > 0
	hasCron := strings.TrimSpace(s.Cron) != ""
	switch {
	case hasInterval && hasCron:
		return fmt.Errorf("%w: set exactly one of interval or cron", ErrInvalidSchedule)
	case hasInterval:
		if s.Interval < time.Second || s.Interval%time.Second != 0 {
			return fmt.Errorf("%w: interval must be a whole number of seconds", ErrInvalidSchedule)
		}
	case hasCron:
		if _, err := parseCron(s.Cron); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSchedule, err)
		}
	default:
		return fmt.Errorf("%w: interval or cron is required", ErrInvalidSchedule)
	}
	return nil
}

// Schedule is a validated, parsed Spec held by a Registry.
type Schedule struct {
	spec Spec
	cron *cronSchedule
}

// Spec returns a copy of the schedule declaration.
func (s Schedule) Spec() Spec {
	spec := s.spec
	if len(spec.Payload) > 0 {
		spec.Payload = append(json.RawMessage(nil), spec.Payload...)
	}
	return spec
}

// Name returns the unique schedule name.
func (s Schedule) Name() string { return s.spec.Name }

func (s Schedule) latestDue(now time.Time) (time.Time, bool) {
	if s.cron == nil {
		return intervalSlot(now, s.spec.Interval), true
	}
	return s.cron.latestDue(now)
}

func (s Schedule) next(after time.Time) (time.Time, bool) {
	if s.cron == nil {
		return intervalSlot(after, s.spec.Interval).Add(s.spec.Interval), true
	}
	return s.cron.Next(after)
}

func intervalSlot(at time.Time, interval time.Duration) time.Time {
	seconds := int64(interval / time.Second)
	unix := at.UTC().Unix()
	// Floor division; schedules only deal with post-epoch times in practice,
	// but keeping the math correct avoids surprises in tests.
	slot := unix / seconds
	if unix < 0 && unix%seconds != 0 {
		slot--
	}
	return time.Unix(slot*seconds, 0).UTC()
}

// jobPayload is stable and deterministic so retries and catch-up passes
// produce byte-identical payloads for the same slot.
type jobPayload struct {
	Schedule     string          `json:"schedule"`
	ScheduledFor time.Time       `json:"scheduledFor"`
	Data         json.RawMessage `json:"data,omitempty"`
}

func (s Schedule) jobPayload(slot time.Time) (json.RawMessage, error) {
	payload, err := json.Marshal(jobPayload{
		Schedule:     s.spec.Name,
		ScheduledFor: slot.UTC(),
		Data:         s.spec.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("scheduler: marshal payload for %q: %w", s.spec.Name, err)
	}
	return payload, nil
}

func (s Schedule) idempotencyKey(slot time.Time) string {
	return fmt.Sprintf("scheduler:%s:%s", s.spec.Name, slot.UTC().Format(time.RFC3339Nano))
}

// Registry holds schedules by name. Registration is intended to happen during
// startup, before the scheduler runs; Schedules returns a stable sorted copy.
type Registry struct {
	mu        sync.RWMutex
	schedules map[string]Schedule
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{schedules: make(map[string]Schedule)}
}

// Register validates and adds spec. Names must be unique.
func (r *Registry) Register(spec Spec) error {
	if r == nil {
		return fmt.Errorf("%w: registry is nil", ErrInvalidSchedule)
	}
	spec.Name = strings.TrimSpace(spec.Name)
	spec.Kind = strings.TrimSpace(spec.Kind)
	spec.Cron = strings.TrimSpace(spec.Cron)
	if err := spec.Validate(); err != nil {
		return err
	}

	parsed := Schedule{spec: spec}
	if spec.Cron != "" {
		cron, err := parseCron(spec.Cron)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSchedule, err)
		}
		parsed.cron = cron
	}
	if len(spec.Payload) > 0 {
		parsed.spec.Payload = append(json.RawMessage(nil), spec.Payload...)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.schedules[spec.Name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateSchedule, spec.Name)
	}
	r.schedules[spec.Name] = parsed
	return nil
}

// Len returns the number of registered schedules.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.schedules)
}

// Schedules returns a copy sorted by name.
func (r *Registry) Schedules() []Schedule {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]Schedule, 0, len(r.schedules))
	for _, schedule := range r.schedules {
		out = append(out, schedule)
	}
	r.mu.RUnlock()
	sortSchedules(out)
	return out
}

func sortSchedules(schedules []Schedule) {
	// Avoid importing sort for a tiny, startup-time list.
	for i := 1; i < len(schedules); i++ {
		for j := i; j > 0 && schedules[j].Name() < schedules[j-1].Name(); j-- {
			schedules[j], schedules[j-1] = schedules[j-1], schedules[j]
		}
	}
}
