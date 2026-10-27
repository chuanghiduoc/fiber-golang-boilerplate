//go:build !integration

package pgtest

import "testing"

// Main runs the test binary. Integration support is compiled only with the
// integration build tag, so unit-test packages that share a TestMain can call
// it unconditionally.
func Main(m *testing.M) { m.Run() }

// Start is a no-op outside integration builds.
func Start() {}

// Stop is a no-op outside integration builds.
func Stop() {}
