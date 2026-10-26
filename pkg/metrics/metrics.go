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
			Help: "Total number of jobs dead-lettered by a handler failure after exhausting their attempts, partitioned by job kind.",
		},
		[]string{"kind"},
	)

	// JobsLeaseLostTotal counts running handlers that were canceled because
	// their worker lost the job lease; no outcome is recorded for them.
	JobsLeaseLostTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_lease_lost_total",
			Help: "Total number of running jobs whose handler was canceled because the worker lost the lease, partitioned by job kind.",
		},
		[]string{"kind"},
	)

	// JobsLeaseDeadTotal counts jobs the claim query dead-lettered because
	// their worker lease expired on the final attempt.
	JobsLeaseDeadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_lease_dead_total",
			Help: "Total number of jobs dead-lettered by lease expiry on their final attempt, partitioned by job kind.",
		},
		[]string{"kind"},
	)

	// JobsQueueLagSeconds is observed once per claimed job: the time between
	// the job becoming due (run_at) and its claim. It only sees jobs that are
	// claimed, so a fully stalled queue shows up as an absence of observations
	// (alert on jobs_claimed_total rate as well).
	JobsQueueLagSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "jobs_queue_lag_seconds",
			Help:    "Seconds a job waited between becoming due and being claimed, partitioned by job kind.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600},
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

	AuditIntentFailuresTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "audit_intent_failures_total",
			Help: "Total number of pre-write audit intent rows that could not be recorded.",
		},
	)

	AuditCompletionFailuresTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "audit_completion_failures_total",
			Help: "Total number of audit intent rows that could not be completed.",
		},
	)

	AuditTxMissingTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "audit_tx_missing_total",
			Help: "Total number of Tier 1 audit writes attempted without a database transaction.",
		},
	)

	AuditDomainMissingTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "audit_domain_missing_total",
			Help: "Total number of guarded mutations that succeeded without a domain audit event.",
		},
	)

	FeatureFlagEvaluationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "feature_flag_evaluations_total",
			Help: "Total number of feature flag evaluations, partitioned by key, precedence source, and result.",
		},
		[]string{"key", "source", "enabled"},
	)

	FeatureFlagSnapshotLoadsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "feature_flag_snapshot_loads_total",
			Help: "Total number of database snapshot loads, partitioned by result (success or error).",
		},
		[]string{"result"},
	)

	FeatureFlagCacheHitsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "feature_flag_cache_hits_total",
			Help: "Total number of feature flag evaluations served from a fresh in-process snapshot.",
		},
	)

	FeatureFlagCacheMissesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "feature_flag_cache_misses_total",
			Help: "Total number of feature flag evaluations that found the snapshot cold or expired.",
		},
	)

	FeatureFlagSnapshotStaleTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "feature_flag_snapshot_stale_total",
			Help: "Total number of evaluations served from a stale snapshot while the database was unavailable.",
		},
	)

	FeatureFlagSnapshotFallbackTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "feature_flag_snapshot_fallback_total",
			Help: "Total number of evaluations that fell back to environment/compiled defaults because no snapshot was usable.",
		},
	)

	FeatureFlagSnapshotAgeSeconds = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "feature_flag_snapshot_age_seconds",
			Help: "Age in seconds of the feature flag snapshot currently used for evaluation.",
		},
	)

	FeatureFlagInvalidationsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "feature_flag_invalidations_total",
			Help: "Total number of local feature flag snapshot invalidations.",
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

	RateLimitErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limit_errors_total",
			Help: "Total number of rate-limit counter failures; the request was allowed (fail-open), partitioned by tier.",
		},
		[]string{"tier"},
	)
)
