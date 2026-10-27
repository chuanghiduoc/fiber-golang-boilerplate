package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilesConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     FilesConfig
		wantErr string
	}{
		{name: "defaults", cfg: FilesConfig{}},
		{name: "quotas and none", cfg: FilesConfig{TenantQuotaBytes: 1 << 30, UserQuotaBytes: 1 << 20, ScanDriver: "none"}},
		{name: "negative tenant quota", cfg: FilesConfig{TenantQuotaBytes: -1}, wantErr: "FILES_TENANT_QUOTA_BYTES"},
		{name: "negative user quota", cfg: FilesConfig{UserQuotaBytes: -1}, wantErr: "FILES_USER_QUOTA_BYTES"},
		{name: "unknown scanner", cfg: FilesConfig{ScanDriver: "virustotal"}, wantErr: "FILES_SCAN_DRIVER"},
		{name: "clamav without address", cfg: FilesConfig{ScanDriver: "clamav"}, wantErr: "FILES_CLAMAV_ADDR"},
		{name: "clamav bad address", cfg: FilesConfig{ScanDriver: "clamav", ClamAVAddr: "clamd", ClamAVTimeout: 5 * time.Second}, wantErr: "FILES_CLAMAV_ADDR"},
		{name: "clamav bad port", cfg: FilesConfig{ScanDriver: "clamav", ClamAVAddr: "clamd:http", ClamAVTimeout: 5 * time.Second}, wantErr: "FILES_CLAMAV_ADDR"},
		{name: "clamav without timeout", cfg: FilesConfig{ScanDriver: "clamav", ClamAVAddr: "clamd:3310"}, wantErr: "FILES_CLAMAV_TIMEOUT"},
		{name: "clamav", cfg: FilesConfig{ScanDriver: "clamav", ClamAVAddr: "clamd:3310", ClamAVTimeout: 5 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestFilesConfigEnabledScanDriver(t *testing.T) {
	assert.Equal(t, "none", FilesConfig{}.EnabledScanDriver())
	assert.Equal(t, "none", FilesConfig{ScanDriver: "none"}.EnabledScanDriver())
	assert.Equal(t, "clamav", FilesConfig{ScanDriver: "clamav"}.EnabledScanDriver())
}
