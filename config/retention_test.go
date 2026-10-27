package config

import (
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetentionDefaults(t *testing.T) {
	var cfg RetentionConfig
	require.NoError(t, env.ParseWithOptions(&cfg, env.Options{Environment: map[string]string{}}))
	assert.Equal(t, 30*day, cfg.FilesTrashRetention)
	assert.Equal(t, 180*day, cfg.NotificationsRetention)
	assert.Equal(t, int32(30), cfg.FilesTrashRetentionDays())
	assert.Equal(t, int32(180), cfg.NotificationsRetentionDays())
	require.NoError(t, cfg.validate())
}

func TestRetentionValidate(t *testing.T) {
	assert.NoError(t, RetentionConfig{}.validate(), "an unset programmatic block is allowed")
	assert.ErrorContains(t, RetentionConfig{FilesTrashRetention: 0, NotificationsRetention: 180 * day}.validate(), "FILES_TRASH_RETENTION")
	assert.ErrorContains(t, RetentionConfig{FilesTrashRetention: 3651 * day, NotificationsRetention: 180 * day}.validate(), "FILES_TRASH_RETENTION")
	assert.ErrorContains(t, RetentionConfig{FilesTrashRetention: 30 * day, NotificationsRetention: 0}.validate(), "NOTIFICATIONS_RETENTION")
	assert.ErrorContains(t, RetentionConfig{FilesTrashRetention: 30 * day, NotificationsRetention: 3651 * day}.validate(), "NOTIFICATIONS_RETENTION")
	assert.ErrorContains(t, RetentionConfig{FilesTrashRetention: 30*day + time.Hour, NotificationsRetention: day}.validate(), "whole number of days")
	assert.NoError(t, RetentionConfig{FilesTrashRetention: 3650 * day, NotificationsRetention: day}.validate())
}
