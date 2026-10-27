package config

import (
	"fmt"
	"strings"
	"time"
)

// day is the unit for *_RETENTION settings.
const day = 24 * time.Hour

// SchedulerConfig controls the optional PostgreSQL-leader scheduler. Interval
// and cron schedules execute in UTC; the default is disabled so a deployment
// only runs schedules after an explicit opt-in.
type SchedulerConfig struct {
	Enabled           bool          `env:"SCHEDULER_ENABLED" envDefault:"false"`
	LeaseTTL          time.Duration `env:"SCHEDULER_LEASE_TTL" envDefault:"30s"`
	HeartbeatInterval time.Duration `env:"SCHEDULER_HEARTBEAT_INTERVAL" envDefault:"10s"`
	PollInterval      time.Duration `env:"SCHEDULER_POLL_INTERVAL" envDefault:"15s"`
	Catchup           string        `env:"SCHEDULER_CATCHUP" envDefault:"latest"`
	CatchupLimit      int           `env:"SCHEDULER_CATCHUP_LIMIT" envDefault:"100"`
	Retention         time.Duration `env:"SCHEDULER_RETENTION" envDefault:"720h"`
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
	if s.LeaseTTL < time.Second {
		return fmt.Errorf("SCHEDULER_LEASE_TTL must be at least 1s")
	}
	if s.HeartbeatInterval < time.Second {
		return fmt.Errorf("SCHEDULER_HEARTBEAT_INTERVAL must be at least 1s")
	}
	if s.HeartbeatInterval >= s.LeaseTTL {
		return fmt.Errorf("SCHEDULER_HEARTBEAT_INTERVAL (%s) must be less than SCHEDULER_LEASE_TTL (%s)", s.HeartbeatInterval, s.LeaseTTL)
	}
	if s.PollInterval < time.Second {
		return fmt.Errorf("SCHEDULER_POLL_INTERVAL must be at least 1s")
	}
	switch strings.ToLower(strings.TrimSpace(s.Catchup)) {
	case "none", "latest", "all":
	default:
		return fmt.Errorf("SCHEDULER_CATCHUP must be one of: none, latest, all (got %q)", s.Catchup)
	}
	if s.CatchupLimit < 1 {
		return fmt.Errorf("SCHEDULER_CATCHUP_LIMIT must be at least 1")
	}
	if s.Retention < day {
		return fmt.Errorf("SCHEDULER_RETENTION must be at least 24h")
	}
	return nil
}
