package scheduler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpecValidate(t *testing.T) {
	t.Parallel()

	validInterval := Spec{Name: "cleanup", Interval: 5 * time.Minute, Kind: "maintenance.cleanup"}
	require.NoError(t, validInterval.Validate())

	validCron := Spec{Name: "report", Cron: "@daily", Kind: "reports.daily", TenantID: testID(2)}
	require.NoError(t, validCron.Validate())

	invalid := []Spec{
		{Name: "bad name", Interval: time.Minute, Kind: "k"},
		{Name: "ok", Interval: time.Minute},
		{Name: "ok", Interval: time.Minute, Kind: "bad kind"},
		{Name: "ok", Kind: "k"},
		{Name: "ok", Interval: time.Minute, Cron: "@daily", Kind: "k"},
		{Name: "ok", Interval: 500 * time.Millisecond, Kind: "k"},
		{Name: "ok", Interval: 1500 * time.Millisecond, Kind: "k"},
		{Name: "ok", Interval: time.Minute, Kind: "k", MaxAttempts: -1},
		{Name: "ok", Interval: time.Minute, Kind: "k", Payload: json.RawMessage(`{`)},
		{Name: "ok", Cron: "not cron", Kind: "k"},
	}
	for _, spec := range invalid {
		spec := spec
		t.Run(spec.Name+"/"+spec.Cron, func(t *testing.T) {
			t.Parallel()
			require.Error(t, spec.Validate())
		})
	}
}

func TestIntervalScheduleAlignment(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.Register(Spec{
		Name:     "cleanup",
		Interval: 5 * time.Minute,
		Kind:     "maintenance.cleanup",
	}))
	schedule := registry.Schedules()[0]

	now := time.Date(2026, 10, 24, 12, 7, 30, 0, time.UTC)
	latest, ok := schedule.latestDue(now)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 10, 24, 12, 5, 0, 0, time.UTC), latest)

	next, ok := schedule.next(latest)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 10, 24, 12, 10, 0, 0, time.UTC), next)

	next, ok = schedule.next(now)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 10, 24, 12, 10, 0, 0, time.UTC), next)
}

func TestScheduleJobPayloadIsDeterministic(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.Register(Spec{
		Name:     "cleanup",
		Interval: time.Minute,
		Kind:     "maintenance.cleanup",
		Payload:  json.RawMessage(`{"batch":25}`),
	}))
	schedule := registry.Schedules()[0]
	slot := time.Date(2026, 10, 24, 12, 5, 0, 0, time.UTC)

	first, err := schedule.jobPayload(slot)
	require.NoError(t, err)
	second, err := schedule.jobPayload(slot)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
	assert.JSONEq(t, `{"schedule":"cleanup","scheduledFor":"2026-10-24T12:05:00Z","data":{"batch":25}}`, string(first))
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:05:00Z", schedule.idempotencyKey(slot))
}

func TestRegistryRejectsDuplicatesAndSorts(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.Register(Spec{Name: "z", Interval: time.Minute, Kind: "k.z"}))
	require.NoError(t, registry.Register(Spec{Name: "a", Cron: "@hourly", Kind: "k.a"}))
	assert.Equal(t, 2, registry.Len())

	require.ErrorIs(t, registry.Register(Spec{Name: "a", Interval: time.Minute, Kind: "k.a"}), ErrDuplicateSchedule)

	schedules := registry.Schedules()
	require.Len(t, schedules, 2)
	assert.Equal(t, "a", schedules[0].Name())
	assert.Equal(t, "z", schedules[1].Name())

	assert.Equal(t, 0, (*Registry)(nil).Len())
	assert.Empty(t, (*Registry)(nil).Schedules())
	assert.Error(t, (*Registry)(nil).Register(Spec{Name: "x", Interval: time.Minute, Kind: "k"}))
}

func TestParseCatchup(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"none", "latest", "all", " ALL "} {
		policy, err := ParseCatchup(value)
		require.NoError(t, err)
		assert.NotEmpty(t, policy)
	}
	_, err := ParseCatchup("sometimes")
	require.Error(t, err)
}
