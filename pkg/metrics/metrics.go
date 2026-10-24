package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests.",
		},
		[]string{"method", "path", "status"},
	)

	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	JobsClaimedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_claimed_total",
			Help: "Total number of jobs claimed by workers, partitioned by job kind.",
		},
		[]string{"kind"},
	)

	JobsSucceededTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_succeeded_total",
			Help: "Total number of jobs whose successful outcome was recorded, partitioned by job kind.",
		},
		[]string{"kind"},
	)

	JobsRetriedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_retried_total",
			Help: "Total number of failed jobs requeued for another attempt, partitioned by job kind.",
		},
		[]string{"kind"},
	)

	JobsDeadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_dead_total",
			Help: "Total number of jobs dead-lettered after exhausting their attempts, partitioned by job kind.",
		},
		[]string{"kind"},
	)

	JobsClaimErrorsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "jobs_claim_errors_total",
			Help: "Total number of failed job claim attempts; the worker retries with backoff.",
		},
	)

	JobsPurgedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "jobs_purged_total",
			Help: "Total number of terminal (succeeded or dead) job rows deleted by retention.",
		},
	)

	AuditWriteFailuresTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "audit_write_failures_total",
			Help: "Total number of audit events that could not be recorded.",
		},
	)

	EventsPublishedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "events_published_total",
			Help: "Total number of events published, partitioned by event name and transport.",
		},
		[]string{"event", "mode"},
	)

	EventsHandledTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "events_handled_total",
			Help: "Total number of event handler invocations, partitioned by event and outcome.",
		},
		[]string{"event", "outcome"},
	)

	EventsEnqueueErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "events_enqueue_errors_total",
			Help: "Total number of durable event jobs that could not be enqueued.",
		},
		[]string{"event"},
	)

	SchedulerRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "scheduler_runs_total",
			Help: "Total number of scheduler slots processed, partitioned by schedule and outcome.",
		},
		[]string{"schedule", "outcome"},
	)

	SchedulerEnqueueErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "scheduler_enqueue_errors_total",
			Help: "Total number of scheduled jobs that could not be enqueued.",
		},
		[]string{"schedule"},
	)

	SchedulerLeader = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "scheduler_leader",
			Help: "1 when this process holds the scheduler leader lease, otherwise 0.",
		},
	)

	SchedulerRunsPurgedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "scheduler_runs_purged_total",
			Help: "Total number of scheduler run-ledger rows deleted by retention.",
		},
	)
)
