package config

import (
	"fmt"
	"slices"
)

// JobsConfig narrows which job kinds this process's worker claims, so a
// deployment can run dedicated workers per kind (bulkheads): for example one
// worker with JOBS_WORKER_KINDS=webhook.delivery and the general worker with
// JOBS_WORKER_EXCLUDE_KINDS=webhook.delivery. Every kind must be claimed by
// some worker, or its jobs stay queued. Empty lists claim every kind.
type JobsConfig struct {
	WorkerKinds        []string `env:"JOBS_WORKER_KINDS" envSeparator:","`
	WorkerExcludeKinds []string `env:"JOBS_WORKER_EXCLUDE_KINDS" envSeparator:","`
}

// WorkerKindList returns the parsed JOBS_WORKER_KINDS entries.
func (j JobsConfig) WorkerKindList() []string {
	return trimmedList(j.WorkerKinds)
}

// WorkerExcludeKindList returns the parsed JOBS_WORKER_EXCLUDE_KINDS entries.
func (j JobsConfig) WorkerExcludeKindList() []string {
	return trimmedList(j.WorkerExcludeKinds)
}

// validate rejects a kind that is both included and excluded: the worker
// would never claim it, which is always a configuration mistake.
func (j JobsConfig) validate() error {
	excluded := j.WorkerExcludeKindList()
	for _, kind := range j.WorkerKindList() {
		if slices.Contains(excluded, kind) {
			return fmt.Errorf("job kind %q is listed in both JOBS_WORKER_KINDS and JOBS_WORKER_EXCLUDE_KINDS", kind)
		}
	}
	return nil
}
