package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// TestWarnRiskySettingsScanDisabled proves production never silently accepts
// uploads without malware scanning.
func TestWarnRiskySettingsScanDisabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{
			name: "production with no scanner",
			cfg:  &config.Config{App: config.AppConfig{Env: "production", Role: config.RoleAPI, BodyLimit: 1 << 24}},
			want: true,
		},
		{
			name: "production with explicit none",
			cfg: &config.Config{
				App:   config.AppConfig{Env: "production", Role: config.RoleAPI, BodyLimit: 1 << 24},
				Files: config.FilesConfig{ScanDriver: "none"},
			},
			want: true,
		},
		{
			name: "production with clamav",
			cfg: &config.Config{
				App:   config.AppConfig{Env: "production", Role: config.RoleAPI, BodyLimit: 1 << 24},
				Files: config.FilesConfig{ScanDriver: "clamav", ClamAVAddr: "clamd:3310", ClamAVTimeoutSecs: 5},
			},
		},
		{
			name: "staging without a scanner",
			cfg:  &config.Config{App: config.AppConfig{Env: "staging", Role: config.RoleAPI, BodyLimit: 1 << 24}},
		},
		{
			name: "production worker with no scanner",
			cfg:  &config.Config{App: config.AppConfig{Env: "production", Role: config.RoleWorker, BodyLimit: 1 << 24}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			warnRiskySettings(tc.cfg)

			got := strings.Contains(buf.String(), "FILES_SCAN_DRIVER")
			if got != tc.want {
				t.Fatalf("scan warning = %v, want %v (log: %s)", got, tc.want, buf.String())
			}
		})
	}
}

// TestWarnRiskySettingsSchedulerDisabled proves production never silently runs
// with retention and scheduled jobs switched off on the process that can run
// them.
func TestWarnRiskySettingsSchedulerDisabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{
			name: "production worker without scheduler",
			cfg: &config.Config{App: config.AppConfig{
				Env: "production", Role: config.RoleWorker, BodyLimit: 1 << 24,
			}},
			want: true,
		},
		{
			name: "production all-in-one without scheduler",
			cfg: &config.Config{App: config.AppConfig{
				Env: "production", Role: config.RoleAll, BodyLimit: 1 << 24,
			}},
			want: true,
		},
		{
			name: "production api-only role",
			cfg: &config.Config{App: config.AppConfig{
				Env: "production", Role: config.RoleAPI, BodyLimit: 1 << 24,
			}},
		},
		{
			name: "production with scheduler",
			cfg: &config.Config{
				App:       config.AppConfig{Env: "production", Role: config.RoleWorker, BodyLimit: 1 << 24},
				Scheduler: config.SchedulerConfig{Enabled: true},
			},
		},
		{
			name: "staging worker without scheduler",
			cfg: &config.Config{App: config.AppConfig{
				Env: "staging", Role: config.RoleWorker, BodyLimit: 1 << 24,
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			warnRiskySettings(tc.cfg)

			got := strings.Contains(buf.String(), "SCHEDULER_ENABLED")
			if got != tc.want {
				t.Fatalf("scheduler warning = %v, want %v (log: %s)", got, tc.want, buf.String())
			}
		})
	}
}
