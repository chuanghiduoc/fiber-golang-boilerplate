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
)
