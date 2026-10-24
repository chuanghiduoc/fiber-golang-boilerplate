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
)
