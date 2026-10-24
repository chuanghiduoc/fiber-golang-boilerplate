package main

import "testing"

func TestRunIsDefined(t *testing.T) {
	// Compile-level guard: main must keep a testable run() entry point.
	_ = run
}

func TestJobsRequireTraceParentColumn(t *testing.T) {
	// Fresh installs get jobs.trace_parent from the consolidated baseline.
	// cmd/migrate must verify it so an older schema cannot silently break
	// queue inserts after deployment.
	found := false
	for _, column := range requiredColumns["jobs"] {
		if column == "trace_parent" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("requiredColumns[jobs] must include trace_parent")
	}
}
