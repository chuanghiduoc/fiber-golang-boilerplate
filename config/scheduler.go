package config

import (
	"fmt"
	"strings"
	"time"
)

// day is the unit for *_RETENTION_DAYS settings.
const day = 24 * time.Hour

// SchedulerConfig controls the optional PostgreSQL-leader scheduler. Interval
// and cron schedules execute in UTC; the default is disabled so a deployment
// only runs schedules after an explicit opt-in.
type SchedulerConfig struct {
	Enabled       bool   `env:"SCHEDULER_ENABLED" envDefault:"false"`
	LeaseTTLSecs  int    `env:"SCHEDULER_LEASE_TTL_SECS" envDefault:"30"`
	HeartbeatSecs int    `env:"SCHEDULER_HEARTBEAT_SECS" envDefault:"10"`
	PollSecs      int    `env:"SCHEDULER_POLL_SECS" envDefault:"15"`
	Catchup       string `env:"SCHEDULER_CATCHUP" envDefault:"latest"`
	CatchupLimit  int    `env:"SCHEDULER_CATCHUP_LIMIT" envDefault:"100"`
	RetentionDays int    `env:"SCHEDULER_RETENTION_DAYS" envDefault:"30"`
}

// LeaseTTL returns the configured lease duration.
func (s SchedulerConfig) LeaseTTL() time.Duration {
	return time.Duration(s.LeaseTTLSecs) * time.Second
}

// HeartbeatInterval returns the configured lease heartbeat interval.
func (s SchedulerConfig) HeartbeatInterval() time.Duration {
	return time.Duration(s.HeartbeatSecs) * time.Second
}

// PollInterval returns the configured schedule evaluation interval.
func (s SchedulerConfig) PollInterval() time.Duration {
	return time.Duration(s.PollSecs) * time.Second
}

// Retention returns the configured scheduler_runs retention window.
func (s SchedulerConfig) Retention() time.Duration {
	return time.Duration(s.RetentionDays) * day
}

// Validate checks scheduler timing and catch-up settings. The defaults are
// valid even when the scheduler is disabled; enabling it never requires a
// second config pass.
func (s SchedulerConfig) Validate() error {
	// Programmatic configs in tests and embedders may leave the optional
	// scheduler block unset; env.Load fills it with defaults.
	if s == (SchedulerConfig{}) {
		return nil
	}
	if s.LeaseTTLSecs < 1 {
		return fmt.Errorf("SCHEDULER_LEASE_TTL_SECS must be at least 1")
	}
	if s.HeartbeatSecs < 1 {
		return fmt.Errorf("SCHEDULER_HEARTBEAT_SECS must be at least 1")
	}
	if s.HeartbeatSecs >= s.LeaseTTLSecs {
		return fmt.Errorf("SCHEDULER_HEARTBEAT_SECS (%d) must be less than SCHEDULER_LEASE_TTL_SECS (%d)", s.HeartbeatSecs, s.LeaseTTLSecs)
	}
	if s.PollSecs < 1 {
		return fmt.Errorf("SCHEDULER_POLL_SECS must be at least 1")
	}
	switch strings.ToLower(strings.TrimSpace(s.Catchup)) {
	case "none", "latest", "all":
	default:
		return fmt.Errorf("SCHEDULER_CATCHUP must be one of: none, latest, all (got %q)", s.Catchup)
	}
	if s.CatchupLimit < 1 {
		return fmt.Errorf("SCHEDULER_CATCHUP_LIMIT must be at least 1")
	}
	if s.RetentionDays < 1 {
		return fmt.Errorf("SCHEDULER_RETENTION_DAYS must be at least 1")
	}
	return nil
}
