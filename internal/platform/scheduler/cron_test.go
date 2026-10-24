package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustCron(t *testing.T, expr string) *cronSchedule {
	t.Helper()
	schedule, err := parseCron(expr)
	require.NoError(t, err)
	return schedule
}

func utc(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, time.UTC)
}

func TestCronNext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		expr string
		from time.Time
		want time.Time
	}{
		{
			name: "every minute",
			expr: "* * * * *",
			from: utc(2026, time.October, 24, 12, 0, 30),
			want: utc(2026, time.October, 24, 12, 1, 0),
		},
		{
			name: "quarter hours",
			expr: "*/15 * * * *",
			from: utc(2026, time.October, 24, 12, 7, 0),
			want: utc(2026, time.October, 24, 12, 15, 0),
		},
		{
			name: "weekday morning",
			expr: "0 9 * * 1-5",
			from: utc(2026, time.October, 24, 12, 0, 0), // Saturday
			want: utc(2026, time.October, 26, 9, 0, 0),  // Monday
		},
		{
			name: "first of month",
			expr: "0 0 1 * *",
			from: utc(2026, time.October, 24, 12, 0, 0),
			want: utc(2026, time.November, 1, 0, 0, 0),
		},
		{
			name: "leap day",
			expr: "0 0 29 2 *",
			from: utc(2026, time.October, 24, 12, 0, 0),
			want: utc(2028, time.February, 29, 0, 0, 0),
		},
		{
			name: "daily alias",
			expr: "@daily",
			from: utc(2026, time.October, 24, 12, 0, 0),
			want: utc(2026, time.October, 25, 0, 0, 0),
		},
		{
			name: "weekly alias",
			expr: "@weekly",
			from: utc(2026, time.October, 24, 12, 0, 0), // Saturday
			want: utc(2026, time.October, 25, 0, 0, 0),  // Sunday
		},
		{
			name: "sunday as seven",
			expr: "0 0 * * 7",
			from: utc(2026, time.October, 24, 12, 0, 0),
			want: utc(2026, time.October, 25, 0, 0, 0),
		},
		{
			name: "single value with step",
			expr: "5/20 * * * *",
			from: utc(2026, time.October, 24, 12, 21, 0),
			want: utc(2026, time.October, 24, 12, 25, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := mustCron(t, tt.expr).Next(tt.from)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCronPrev(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		expr string
		from time.Time
		want time.Time
	}{
		{
			name: "weekday morning",
			expr: "0 9 * * 1-5",
			from: utc(2026, time.October, 26, 8, 0, 0), // Monday
			want: utc(2026, time.October, 23, 9, 0, 0), // Friday
		},
		{
			name: "hourly",
			expr: "@hourly",
			from: utc(2026, time.October, 24, 12, 34, 59),
			want: utc(2026, time.October, 24, 12, 0, 0),
		},
		{
			name: "quarter hours",
			expr: "*/15 * * * *",
			from: utc(2026, time.October, 24, 12, 7, 0),
			want: utc(2026, time.October, 24, 12, 0, 0),
		},
		{
			name: "exact boundary is exclusive",
			expr: "@daily",
			from: utc(2026, time.October, 24, 0, 0, 0),
			want: utc(2026, time.October, 23, 0, 0, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := mustCron(t, tt.expr).Prev(tt.from)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCronLatestDue(t *testing.T) {
	t.Parallel()

	daily := mustCron(t, "@daily")
	got, ok := daily.latestDue(utc(2026, time.October, 24, 12, 0, 30))
	require.True(t, ok)
	assert.Equal(t, utc(2026, time.October, 24, 0, 0, 0), got)

	got, ok = daily.latestDue(utc(2026, time.October, 24, 0, 0, 0))
	require.True(t, ok)
	assert.Equal(t, utc(2026, time.October, 24, 0, 0, 0), got)
}

func TestCronDayOfMonthOrDayOfWeek(t *testing.T) {
	t.Parallel()

	// Both fields are restricted, so cron ORs them: the 13th or any Friday.
	both := mustCron(t, "0 0 13 * 5")
	assert.True(t, both.dayMatches(utc(2026, time.October, 13, 0, 0, 0)), "13th matches")
	assert.True(t, both.dayMatches(utc(2026, time.October, 16, 0, 0, 0)), "Friday matches")
	assert.False(t, both.dayMatches(utc(2026, time.October, 14, 0, 0, 0)), "Wednesday matches neither")

	// A wildcard day-of-week makes day-of-month authoritative.
	domOnly := mustCron(t, "0 0 13 * *")
	assert.True(t, domOnly.dayMatches(utc(2026, time.October, 13, 0, 0, 0)))
	assert.False(t, domOnly.dayMatches(utc(2026, time.October, 16, 0, 0, 0)))

	// A wildcard day-of-month makes day-of-week authoritative.
	dowOnly := mustCron(t, "0 0 * * 5")
	assert.True(t, dowOnly.dayMatches(utc(2026, time.October, 16, 0, 0, 0)))
	assert.False(t, dowOnly.dayMatches(utc(2026, time.October, 13, 0, 0, 0)))
}

func TestCronRejectsInvalidExpressions(t *testing.T) {
	t.Parallel()

	invalid := []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 8",
		"*/0 * * * *",
		"10-5 * * * *",
		"a * * * *",
		"@sometimes",
		"1,,2 * * * *",
	}
	for _, expr := range invalid {
		expr := expr
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			_, err := parseCron(expr)
			require.Error(t, err)
		})
	}
}

func TestCronAliases(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"@yearly":   "0 0 1 1 *",
		"@annually": "0 0 1 1 *",
		"@monthly":  "0 0 1 * *",
		"@weekly":   "0 0 * * 0",
		"@daily":    "0 0 * * *",
		"@midnight": "0 0 * * *",
		"@hourly":   "0 * * * *",
	}
	for alias, expanded := range tests {
		alias, expanded := alias, expanded
		t.Run(alias, func(t *testing.T) {
			t.Parallel()
			from := utc(2026, time.October, 24, 12, 30, 0)
			got, ok := mustCron(t, alias).Next(from)
			require.True(t, ok)
			want, ok := mustCron(t, expanded).Next(from)
			require.True(t, ok)
			assert.Equal(t, want, got)
		})
	}
}
