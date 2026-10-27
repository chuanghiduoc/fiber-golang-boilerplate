//go:build integration

package pgtest

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// Main starts the package's single PostgreSQL server, runs the test binary, and
// terminates the container. Add it to a package as:
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
func Main(m *testing.M) {
	Start()
	code := m.Run()
	Stop()
	os.Exit(code)
}

// Start boots the package's PostgreSQL server. Main calls it; a package with a
// custom TestMain calls Start/Stop around its own m.Run.
func Start() {
	if err := startShared(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: start PostgreSQL: %v\n", err)
		os.Exit(1)
	}
}

// Stop terminates the package's server. It is a no-op when Start was never
// called (New starts the server lazily).
func Stop() {
	if shared.admin != nil {
		shared.admin.Close()
		shared.admin = nil
	}
	if shared.cleanup != nil {
		shared.cleanup()
		shared.cleanup = nil
	}
}
