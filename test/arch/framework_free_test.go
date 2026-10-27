package arch_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// frameworkImports are the HTTP-framework packages business and core code
// must not reach. Fiber adapters live in *http subpackages (apperrorhttp,
// authzhttp, tenanthttp, audithttp, apikeyhttp, featureshttp) and handlers.
var frameworkImports = []string{"github.com/gofiber/", "github.com/valyala/fasthttp"}

// TestCoreAndServicesAreFrameworkFree walks the repository-local import graph
// (non-test files) of every module service/repository package and the core
// platform packages, and fails if any of them transitively imports Fiber.
// The AWS SDK is likewise confined to internal/storage/s3store.
func TestCoreAndServicesAreFrameworkFree(t *testing.T) {
	root := repoRoot(t)
	pkgs := []string{
		"pkg/apperror",
		"pkg/storage",
		"internal/database",
		"internal/platform/authz",
		"internal/platform/tenant",
		"internal/platform/audit",
		"internal/platform/apikey",
		"internal/platform/features",
	}
	for _, layer := range []string{"service", "repository"} {
		matches, err := filepath.Glob(filepath.Join(root, "internal", "modules", "*", layer))
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range matches {
			rel, err := filepath.Rel(root, dir)
			if err != nil {
				t.Fatal(err)
			}
			pkgs = append(pkgs, filepath.ToSlash(rel))
		}
	}

	for _, pkg := range pkgs {
		external := transitiveExternalImports(t, root, pkg)
		for _, imp := range external {
			for _, forbidden := range frameworkImports {
				if strings.HasPrefix(imp, forbidden) {
					t.Errorf("%s transitively imports HTTP framework package %s; move the Fiber code to an *http adapter", pkg, imp)
				}
			}
		}
	}

	for _, pkg := range []string{"pkg/storage", "internal/modules/files/service"} {
		for _, imp := range transitiveExternalImports(t, root, pkg) {
			if strings.HasPrefix(imp, "github.com/aws/") {
				t.Errorf("%s transitively imports %s; only internal/storage/s3store may link the AWS SDK", pkg, imp)
			}
		}
	}
}

// transitiveExternalImports returns the sorted external (non-module) imports
// reachable from pkg through repository-local packages, ignoring test files.
func transitiveExternalImports(t *testing.T, root, pkg string) []string {
	t.Helper()
	seen := map[string]bool{}
	external := map[string]bool{}
	var visit func(local string)
	visit = func(local string) {
		if seen[local] {
			return
		}
		seen[local] = true
		for _, imp := range packageImports(t, filepath.Join(root, filepath.FromSlash(local))) {
			if rest, ok := strings.CutPrefix(imp, modulePath+"/"); ok {
				visit(rest)
				continue
			}
			external[imp] = true
		}
	}
	visit(pkg)

	out := make([]string, 0, len(external))
	for imp := range external {
		out = append(out, imp)
	}
	slices.Sort(out)
	return out
}

// packageImports returns the imports of the non-test Go files directly in dir.
func packageImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var imports []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		imports = append(imports, parseImports(t, filepath.Join(dir, name))...)
	}
	return imports
}
