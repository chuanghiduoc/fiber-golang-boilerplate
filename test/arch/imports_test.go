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

	for _, dir := range []string{"internal/platform", "internal/shared", "pkg"} {
		checkNoBusinessImports(t, filepath.Join(root, dir))
	}

	t.Run("contracts stay transport-neutral", func(t *testing.T) {
		checkForbiddenImports(t, filepath.Join(root, "internal", "contracts"), []string{
			"internal/modules/",
			"internal/repository",
			"internal/dto",
			"internal/sqlc",
			"pkg/storage",
			"pkg/database",
			"internal/platform/session",
		})
	})

	t.Run("admin depends on contracts not module internals", func(t *testing.T) {
		checkForbiddenImports(t, filepath.Join(root, "internal", "modules", "admin"), []string{
			"internal/modules/identity",
			"internal/modules/files",
			"internal/repository",
			"internal/dto",
			"internal/sqlc",
			"internal/service",
			"internal/handler",
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

func checkNoBusinessImports(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		for _, imp := range parseImports(t, path) {
			if strings.Contains(imp, "/internal/modules/") {
				t.Errorf("%s must not import a business module (%s)", path, imp)
			}
		}
		return nil
	})
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
