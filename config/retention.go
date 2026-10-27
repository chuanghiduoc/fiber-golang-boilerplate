package config

import (
	"errors"
	"fmt"
	"time"
)

// maxRetention caps the retention settings below at ten years, like
// WEBHOOK_RETENTION, so a typo cannot overflow the int32 day count the purge
// queries take.
const maxRetention = 3650 * day

// RetentionConfig holds the data-retention windows of module-owned purge
// jobs. Each job runs only when the scheduler is enabled (SCHEDULER_ENABLED);
// without it nothing is purged.
type RetentionConfig struct {
	// FilesTrashRetention is how long a soft-deleted file stays restorable
	// before files.trash.purge deletes its blob and row.
	FilesTrashRetention time.Duration `env:"FILES_TRASH_RETENTION" envDefault:"720h"`
	// NotificationsRetention is how long a read notification is kept after it
	// was read before notifications.retention.purge deletes it. Unread
	// notifications are never purged.
	NotificationsRetention time.Duration `env:"NOTIFICATIONS_RETENTION" envDefault:"4320h"`
}

// FilesTrashRetentionDays returns FILES_TRASH_RETENTION as the int32 day count
// the purge query takes; validate bounds it to whole days.
func (r RetentionConfig) FilesTrashRetentionDays() int32 {
	return int32(r.FilesTrashRetention / day) // #nosec G115 -- validated 1..3650 whole days
}

// NotificationsRetentionDays returns NOTIFICATIONS_RETENTION as the int32 day
// count the purge query takes; validate bounds it to whole days.
func (r RetentionConfig) NotificationsRetentionDays() int32 {
	return int32(r.NotificationsRetention / day) // #nosec G115 -- validated 1..3650 whole days
}

func (r RetentionConfig) validate() error {
	// Programmatic configs in tests and embedders may leave the block unset;
	// env parsing always fills the defaults.
	if r == (RetentionConfig{}) {
		return nil
	}
	var errs []error
	if r.FilesTrashRetention < day || r.FilesTrashRetention > maxRetention {
		errs = append(errs, fmt.Errorf("FILES_TRASH_RETENTION must be between 24h and %dh", maxRetention/day))
	} else if r.FilesTrashRetention%day != 0 {
		errs = append(errs, fmt.Errorf("FILES_TRASH_RETENTION must be a whole number of days"))
	}
	if r.NotificationsRetention < day || r.NotificationsRetention > maxRetention {
		errs = append(errs, fmt.Errorf("NOTIFICATIONS_RETENTION must be between 24h and %dh", maxRetention/day))
	} else if r.NotificationsRetention%day != 0 {
		errs = append(errs, fmt.Errorf("NOTIFICATIONS_RETENTION must be a whole number of days"))
	}
	return errors.Join(errs...)
}
