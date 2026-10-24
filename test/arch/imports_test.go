package arch_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModuleBoundaries enforces the modular-monolith dependency rules:
//   - a module may not import another module's internals;
//   - platform/shared packages may not depend on business modules.
func TestModuleBoundaries(t *testing.T) {
	root := repoRoot(t)
	modulesDir := filepath.Join(root, "internal", "modules")

	err := filepath.WalkDir(modulesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(modulesDir, path)
		if err != nil {
			return err
		}
		owner := strings.Split(rel, string(filepath.Separator))[0]
		for _, imp := range parseImports(t, path) {
			const prefix = "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/"
			if !strings.HasPrefix(imp, prefix) {
				continue
			}
			imported := strings.Split(strings.TrimPrefix(imp, prefix), "/")[0]
			if imported != owner {
				t.Errorf("%s: module %q imports another module %q (%s)", rel, owner, imported, imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk modules: %v", err)
	}

	for _, dir := range []string{"internal/platform", "pkg"} {
		checkNoBusinessImports(t, filepath.Join(root, dir))
	}

	t.Run("modules use only their own generated sqlc package", func(t *testing.T) {
		// sqlc output is module-scoped. Importing another module's generated
		// package would leak its persistence model across a boundary, even
		// when the import would otherwise look like any module subpackage.
		err := filepath.WalkDir(modulesDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, err := filepath.Rel(modulesDir, path)
			if err != nil {
				return err
			}
			owner := strings.Split(rel, string(filepath.Separator))[0]
			const prefix = "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/"
			for _, imp := range parseImports(t, path) {
				if !strings.HasPrefix(imp, prefix) {
					continue
				}
				parts := strings.Split(strings.TrimPrefix(imp, prefix), "/")
				if len(parts) == 2 && parts[1] == "sqlc" && parts[0] != owner {
					t.Errorf("%s: module %q imports module %q generated sqlc package", rel, owner, parts[0])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk modules: %v", err)
		}
	})

	t.Run("shared sqlc package is removed", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(root, "internal", "sqlc")); !os.IsNotExist(err) {
			t.Errorf("internal/sqlc must not exist; generated code is module-scoped")
		}
		for _, dir := range []string{"internal", "pkg", "cmd", "test"} {
			checkForbiddenImports(t, filepath.Join(root, dir), []string{"internal/sqlc"})
		}
	})

	t.Run("modules do not use deleted legacy packages", func(t *testing.T) {
		// These packages no longer exist. The rule documents the target layout
		// so a future module does not reintroduce a shared legacy layer.
		legacy := []string{"handler", "service", "dto", "repository"}
		forbidden := make([]string, 0, len(legacy))
		for _, pkg := range legacy {
			forbidden = append(forbidden, "internal/"+pkg)
		}
		checkForbiddenImports(t, modulesDir, forbidden)
	})

	t.Run("only the composition root imports bootstrap", func(t *testing.T) {
		// internal/bootstrap imports every module, so anything below it
		// (app, platform, pkg, contracts, modules) importing it would create
		// a cycle or a hidden cross-module dependency.
		for _, dir := range []string{"internal/app", "internal/platform", "internal/contracts", "internal/modules", "pkg"} {
			checkForbiddenImports(t, filepath.Join(root, dir), []string{"internal/bootstrap"})
		}
		checkForbiddenImports(t, filepath.Join(root, "internal", "app"), []string{"internal/modules/"})
	})

	t.Run("contracts stay stdlib-only", func(t *testing.T) {
		checkStdlibOnly(t, filepath.Join(root, "internal", "contracts"))
	})

	t.Run("admin depends on contracts not module internals", func(t *testing.T) {
		checkForbiddenImports(t, filepath.Join(root, "internal", "modules", "admin"), []string{
			"internal/modules/identity",
			"internal/modules/files",
			"internal/sqlc",
			"pkg/storage",
			"pkg/database",
			"internal/platform/session",
		})
	})
}

func checkForbiddenImports(t *testing.T, dir string, forbidden []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		for _, imp := range parseImports(t, path) {
			for _, prefix := range forbidden {
				needle := "github.com/chuanghiduoc/fiber-golang-boilerplate/" + prefix
				if strings.HasPrefix(imp, needle) {
					t.Errorf("%s must not import %s", path, imp)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// checkStdlibOnly rejects any import whose first path segment contains a dot,
// which is how Go distinguishes standard-library packages from external or
// module-local ones.
func checkStdlibOnly(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		for _, imp := range parseImports(t, path) {
			first := strings.SplitN(imp, "/", 2)[0]
			if strings.Contains(first, ".") {
				t.Errorf("%s must stay stdlib-only, found import %s", path, imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func checkNoBusinessImports(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		for _, imp := range parseImports(t, path) {
			if strings.Contains(imp, "/internal/modules/") {
				t.Errorf("%s must not import a business module (%s)", path, imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

func parseImports(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	imports := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		imports = append(imports, strings.Trim(spec.Path.Value, `"`))
	}
	return imports
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("go.mod not found")
		}
		wd = parent
	}
}
