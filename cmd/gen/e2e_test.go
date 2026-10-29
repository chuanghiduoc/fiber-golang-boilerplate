//go:build generator_e2e

package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedCodeBuilds generates every cmd/gen slice in a throwaway copy of
// the repository and runs the generator's real verification steps. The unit
// tests replace g.run with a recorder, so without this test a broken template
// can pass CI until someone runs the generator by hand.
func TestGeneratedCodeBuilds(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	copyRepository(t, root, repo)

	commands := [][]string{
		{"run", "./cmd/gen", "module", "generator_e2e_probe"},
		{"run", "./cmd/gen", "resource", "--module", "generator_e2e_probe", "invoice"},
		{"run", "./cmd/gen", "command", "--module", "generator_e2e_probe", "IssueInvoice"},
		{"run", "./cmd/gen", "query", "--module", "generator_e2e_probe", "ListOverdueInvoices"},
		{"vet", "./internal/modules/generator_e2e_probe/..."},
	}
	for _, args := range commands {
		cmd := exec.Command("go", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// copyRepository copies the working tree (including uncommitted files) but not
// tool caches or agent state. The copy is inside t.TempDir, so the test never
// writes to the checkout.
func copyRepository(t *testing.T, src, dst string) {
	t.Helper()
	skipDirs := map[string]bool{".git": true, ".claude": true, ".workbuddy-ai": true}
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
	if err != nil {
		t.Fatalf("copy repository: %v", err)
	}
}
