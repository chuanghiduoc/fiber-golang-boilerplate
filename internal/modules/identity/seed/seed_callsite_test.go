package seed_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedPackageSuffix is the import path suffix of the package these tests guard.
const seedPackageSuffix = "/internal/modules/identity/seed"

// TestSeedRunsOnlyFromCommand pins the operational invariant that account
// creation is a one-shot Job: cmd/api startup must never call seed.Admin (or
// import the package), so ADMIN_PASSWORD never reaches API or worker pods.
func TestSeedRunsOnlyFromCommand(t *testing.T) {
	root := repoRoot(t)

	// The identity module (production code) must not import the seeder at all.
	err := filepath.WalkDir(filepath.Join(root, "internal", "modules", "identity"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, spec := range file.Imports {
			if strings.HasSuffix(strings.Trim(spec.Path.Value, `"`), seedPackageSuffix) {
				t.Errorf("%s imports the seed package; only cmd/seed may create accounts", relative(t, root, path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk identity module: %v", err)
	}

	// seed.Admin may only be called from cmd/seed/main.go.
	allowed := "cmd/seed/main.go"
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Admin" {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok || pkg.Name != "seed" {
					return true
				}
				if relative(t, root, path) != allowed {
					t.Errorf("seed.Admin called from %s; only %s may seed", relative(t, root, path), allowed)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}

// repoRoot walks up from the test's working directory to the go.mod root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the test working directory")
		}
		dir = parent
	}
}

func relative(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("relative path for %s: %v", path, err)
	}
	return filepath.ToSlash(rel)
}
