package config

import (
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetentionDefaults(t *testing.T) {
	var cfg RetentionConfig
	require.NoError(t, env.ParseWithOptions(&cfg, env.Options{Environment: map[string]string{}}))
	assert.Equal(t, int32(30), cfg.FilesTrashRetentionDays())
	assert.Equal(t, int32(180), cfg.NotificationsRetentionDays())
	require.NoError(t, cfg.validate())
}

func TestRetentionValidate(t *testing.T) {
	assert.NoError(t, RetentionConfig{}.validate(), "an unset programmatic block is allowed")
	assert.ErrorContains(t, RetentionConfig{FilesTrashDays: 0, NotificationsDays: 180}.validate(), "FILES_TRASH_RETENTION_DAYS")
	assert.ErrorContains(t, RetentionConfig{FilesTrashDays: 3651, NotificationsDays: 180}.validate(), "FILES_TRASH_RETENTION_DAYS")
	assert.ErrorContains(t, RetentionConfig{FilesTrashDays: 30, NotificationsDays: 0}.validate(), "NOTIFICATIONS_RETENTION_DAYS")
	assert.ErrorContains(t, RetentionConfig{FilesTrashDays: 30, NotificationsDays: 3651}.validate(), "NOTIFICATIONS_RETENTION_DAYS")
	assert.NoError(t, RetentionConfig{FilesTrashDays: 3650, NotificationsDays: 1}.validate())
}
