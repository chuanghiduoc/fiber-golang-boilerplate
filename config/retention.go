package config

import "fmt"

// maxRetentionDays caps the *_RETENTION_DAYS settings below at ten years,
// like WEBHOOK_RETENTION_DAYS, so a typo cannot overflow the int32 day count
// the purge queries take.
const maxRetentionDays = 3650

// RetentionConfig holds the data-retention windows of module-owned purge
// jobs. Each job runs only when the scheduler is enabled (SCHEDULER_ENABLED);
// without it nothing is purged.
type RetentionConfig struct {
	// FilesTrashDays is how long a soft-deleted file stays restorable before
	// files.trash.purge deletes its blob and row.
	FilesTrashDays int `env:"FILES_TRASH_RETENTION_DAYS" envDefault:"30"`
	// NotificationsDays is how long a read notification is kept after it was
	// read before notifications.retention.purge deletes it. Unread
	// notifications are never purged.
	NotificationsDays int `env:"NOTIFICATIONS_RETENTION_DAYS" envDefault:"180"`
}

// FilesTrashRetentionDays returns FILES_TRASH_RETENTION_DAYS as the int32 the
// purge query takes; validate bounds it.
func (r RetentionConfig) FilesTrashRetentionDays() int32 {
	return int32(r.FilesTrashDays) // #nosec G115 -- validated 1..3650 days
}

// NotificationsRetentionDays returns NOTIFICATIONS_RETENTION_DAYS as the
// int32 the purge query takes; validate bounds it.
func (r RetentionConfig) NotificationsRetentionDays() int32 {
	return int32(r.NotificationsDays) // #nosec G115 -- validated 1..3650 days
}

func (r RetentionConfig) validate() error {
	// Programmatic configs in tests and embedders may leave the block unset;
	// env parsing always fills the defaults.
	if r == (RetentionConfig{}) {
		return nil
	}
	if r.FilesTrashDays < 1 || r.FilesTrashDays > maxRetentionDays {
		return fmt.Errorf("FILES_TRASH_RETENTION_DAYS must be between 1 and %d", maxRetentionDays)
	}
	if r.NotificationsDays < 1 || r.NotificationsDays > maxRetentionDays {
		return fmt.Errorf("NOTIFICATIONS_RETENTION_DAYS must be between 1 and %d", maxRetentionDays)
	}
	return nil
}
