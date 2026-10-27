package app

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// Infra is the shared infrastructure handed to module constructors
// (<module>.Build). The composition root creates it once; a module builds its
// own repositories, services, and handlers from it and receives any other
// module's capability only as an internal/contracts value. Keep it to
// process-wide infrastructure: module-owned configuration sections (OAuth for
// identity, storage limits for files, webhook delivery for webhooks) are passed
// to that module's Build explicitly, so no module can read another module's
// settings.
//
// Construction must be side-effect free (no I/O), so tests can build every
// module from an Infra with nil Storage to inspect routes.
type Infra struct {
	// Platform is the process-wide configuration every module may read.
	Platform PlatformConfig
	// DB is the tenant-scoped statement handle. Database pools themselves stay
	// private to cmd/api: modules never reach a raw pool, so no module can
	// bypass the RLS scope or close the process's connections.
	DB    database.DBTX
	Tx    *database.TxManager
	Cache cache.Cache
	// Audit is the process-wide recorder; it implements both audit.Recorder
	// (Tier 2 guards, best-effort events) and audit.TxRecorder (Tier 1).
	Audit *audit.PostgresRecorder
	// Queue is the durable jobs outbox.
	Queue *jobs.Queue
	// PayloadCodec seals job payloads at rest with DATA_ENCRYPTION_KEY. It is
	// required whenever Queue is set and a module enqueues personal data.
	PayloadCodec *jobs.PayloadCodec
	// Email enqueues encrypted email.send.encrypted jobs; the platform worker delivers
	// them (see cmd/api). Services that send mail use this, never pkg/email.
	Email   *jobs.QueuedSender
	Storage storage.Storage
	Flags   features.Evaluator
	// Events is the domain event bus. Producers publish with PublishTx inside
	// their transaction; subscribers (webhooks) register through
	// app.EventProvider. Nil (tests) disables publishing.
	Events *events.Bus
	// Catalogs are the injected capability catalogs. The composition root
	// creates them before building modules (they start with the platform
	// entries), passes the same instances to services and transports, adds
	// every module provider after Build, and seals them before serving.
	Catalogs Catalogs
}

// PlatformConfig is the configuration shared by every module: application
// settings (environment, frontend URL, email verification), JWT lifetimes and
// keys, and tenant resolution. Module-owned sections are not here.
type PlatformConfig struct {
	App    config.AppConfig
	JWT    config.JWTConfig
	Tenant config.TenantConfig
}

// RLSResolver scopes a statement or transaction to the context tenant, or
// bypasses isolation when the context is marked with
// database.WithRLSBypass. cmd/api and cmd/seed inject it into every
// database.TxManager and database.Scoped they build.
//
// It is deliberately fail-closed: only a tenant stored in the context
// (tenant.WithID) becomes the scope. An unset tenant maps to 0, which the
// policy sees as "no tenant rows"; it never invents a tenant. Background work
// sets the tenant explicitly (the job worker restores the job's tenant_id),
// and global work marks the context with database.WithRLSBypass.
func RLSResolver(ctx context.Context) database.Scope {
	return database.Scope{TenantID: tenant.IDFromContext(ctx), Bypass: database.RLSBypass(ctx)}
}

// NewPlatformConfig extracts the shared sections from cfg.
func NewPlatformConfig(cfg *config.Config) PlatformConfig {
	return PlatformConfig{App: cfg.App, JWT: cfg.JWT, Tenant: cfg.Tenant}
}
