package arch_test

import (
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

var (
	typedOnce      sync.Once
	typedPkgs      []*packages.Package
	typedLoadErr   error
	typedPkgErrors int
)

// typedPackages loads and type-checks the production tree once per test
// binary. Four arch tests inspect the same ASTs (inline SQL, SQL constants,
// route permissions, audit policies); loading the tree separately for each one
// dominates the package run time and risks the ten-minute timeout under
// -race. The load includes integration-tagged non-test files because the SQL
// allowlist names sites in internal/testutil/pgtest; _test.go files are never
// part of packages.Load output without Tests:true.
func typedPackages(t *testing.T) []*packages.Package {
	t.Helper()
	typedOnce.Do(func() {
		cfg := &packages.Config{
			Dir:        repoRoot(t),
			BuildFlags: []string{"-tags=integration"},
			Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
				packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
				packages.NeedImports | packages.NeedDeps,
		}
		typedPkgs, typedLoadErr = packages.Load(cfg, "./internal/...", "./pkg/...", "./cmd/...")
		if typedLoadErr == nil {
			typedPkgErrors = packages.PrintErrors(typedPkgs)
		}
	})
	if typedLoadErr != nil {
		t.Fatalf("load packages: %v", typedLoadErr)
	}
	if typedPkgErrors > 0 {
		t.Fatalf("%d package load errors; the arch type-check cannot inspect the tree", typedPkgErrors)
	}
	return typedPkgs
}
