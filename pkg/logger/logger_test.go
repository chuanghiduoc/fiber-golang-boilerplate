package logger

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"unknown", slog.LevelInfo},
		{"", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseLevel(tt.input)
			if got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSetup(t *testing.T) {
	// Should not panic for any env
	envs := []string{"local", "production", "staging", "test", "development"}
	levels := []string{"debug", "info", "warn", "error", "unknown"}

	for _, env := range envs {
		for _, level := range levels {
			t.Run(env+"_"+level, func(t *testing.T) {
				Setup(env, level) // should not panic
			})
		}
	}
}
