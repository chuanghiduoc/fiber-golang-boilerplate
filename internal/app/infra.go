package app

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// Infra is the shared infrastructure handed to module constructors
// (<module>.Build). The composition root creates it once; a module builds its
// own repositories, services, and handlers from it and receives any other
// module's capability only as an internal/contracts value. Keep it to
// process-wide infrastructure: module-specific settings come from Config.
//
// Construction must be side-effect free (no I/O), so tests can build every
// module from an Infra with nil Pool/Storage to inspect routes.
type Infra struct {
	Config *config.Config
	Pool   *pgxpool.Pool
	Tx     *database.TxManager
	Cache  cache.Cache
	// Audit is the process-wide recorder; it implements both audit.Recorder
	// (Tier 2 guards, best-effort events) and audit.TxRecorder (Tier 1).
	Audit *audit.PostgresRecorder
	// Queue is the durable jobs outbox.
	Queue *jobs.Queue
	// Email enqueues encrypted email.send jobs; the platform worker delivers
	// them (see cmd/api). Services that send mail use this, never pkg/email.
	Email   *jobs.QueuedSender
	Storage storage.Storage
	Flags   features.Evaluator
}
