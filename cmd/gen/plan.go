package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	dirMode     fs.FileMode = 0o750
	newFileMode fs.FileMode = 0o600
)

// fileOp is one file the generator writes. Paths are slash-separated and
// relative to the repository root.
type fileOp struct {
	path    string
	content []byte
	// create means the file must not exist yet; otherwise it is an update of
	// an existing file whose content was read while planning.
	create bool
}

// plan is the complete set of writes for one generator run. Nothing touches
// the disk until apply, so a planning error leaves the tree unchanged. Every
// file access goes through repo (an os.Root), so no path can escape the
// repository, whatever a name or a symlink contains.
type plan struct {
	repo *os.Root
	ops  []fileOp
	// regenerated are directories a verification step rewrites (sqlc
	// output). They are snapshotted before apply and restored on rollback.
	regenerated []string
}

func (p *plan) create(rel string, content []byte) {
	p.ops = append(p.ops, fileOp{path: rel, content: content, create: true})
}

func (p *plan) update(rel string, content []byte) {
	p.ops = append(p.ops, fileOp{path: rel, content: content})
}

// native converts a slash-separated repository path for os.Root.
func native(rel string) string { return filepath.FromSlash(rel) }

// paths returns the planned paths in a stable order.
func (p *plan) paths() []string {
	out := make([]string, 0, len(p.ops))
	for _, op := range p.ops {
		out = append(out, op.path)
	}
	slices.Sort(out)
	return out
}

// check fails before any write when a file to create already exists or a
// file to update has disappeared since planning.
func (p *plan) check() error {
	for _, op := range p.ops {
		_, err := p.repo.Stat(native(op.path))
		switch {
		case op.create && err == nil:
			return fmt.Errorf("%s already exists; refusing to overwrite it", op.path)
		case op.create && !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("stat %s: %w", op.path, err)
		case !op.create && err != nil:
			return fmt.Errorf("stat %s: %w", op.path, err)
		}
	}
	return nil
}

// printDiff writes a unified diff of every planned change to w.
func (p *plan) printDiff(w io.Writer) error {
	for _, op := range p.ops {
		var before []byte
		if !op.create {
			raw, err := p.repo.ReadFile(native(op.path))
			if err != nil {
				return fmt.Errorf("read %s: %w", op.path, err)
			}
			before = raw
		}
		if _, err := io.WriteString(w, unifiedDiff(op.path, op.create, string(before), string(op.content))); err != nil {
			return err
		}
	}
	return nil
}

// original is the pre-apply state of one written path.
type original struct {
	existed bool
	content []byte
	mode    fs.FileMode
}

// transaction records everything apply changed so rollback can restore it.
type transaction struct {
	repo        *os.Root
	originals   map[string]original
	order       []string
	createdDirs []string                     // deepest first
	snapshots   map[string]map[string][]byte // dir -> file name -> content; nil = dir absent
}

// apply writes every planned file. On a write error it rolls back what it
// already wrote and returns the error.
func (p *plan) apply() (*transaction, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	tx := &transaction{repo: p.repo, originals: map[string]original{}, snapshots: map[string]map[string][]byte{}}
	for _, dir := range p.regenerated {
		snapshot, err := tx.snapshotDir(dir)
		if err != nil {
			return nil, err
		}
		tx.snapshots[dir] = snapshot
	}
	for _, op := range p.ops {
		if err := tx.write(op); err != nil {
			return nil, errors.Join(err, tx.rollback())
		}
	}
	return tx, nil
}

func (tx *transaction) write(op fileOp) error {
	orig := original{mode: newFileMode}
	if info, err := tx.repo.Stat(native(op.path)); err == nil {
		raw, readErr := tx.repo.ReadFile(native(op.path))
		if readErr != nil {
			return fmt.Errorf("read %s: %w", op.path, readErr)
		}
		orig = original{existed: true, content: raw, mode: info.Mode().Perm()}
	}
	if err := tx.mkdirAll(path.Dir(op.path)); err != nil {
		return err
	}
	tx.originals[op.path] = orig
	tx.order = append(tx.order, op.path)
	if err := tx.repo.WriteFile(native(op.path), op.content, orig.mode); err != nil {
		return fmt.Errorf("write %s: %w", op.path, err)
	}
	return nil
}

// mkdirAll creates dir and records every directory it had to create.
func (tx *transaction) mkdirAll(dir string) error {
	var missing []string
	for d := dir; d != "." && d != "/"; d = path.Dir(d) {
		if _, err := tx.repo.Stat(native(d)); err == nil {
			break
		}
		missing = append(missing, d)
	}
	if err := tx.repo.MkdirAll(native(dir), dirMode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tx.createdDirs = append(missing, tx.createdDirs...)
	return nil
}

// rollback restores every written file and the regenerated directories, and
// removes the directories apply created. Every failure is reported.
func (tx *transaction) rollback() error {
	var errs []error
	for i := len(tx.order) - 1; i >= 0; i-- {
		rel := tx.order[i]
		orig := tx.originals[rel]
		if orig.existed {
			if err := tx.repo.WriteFile(native(rel), orig.content, orig.mode); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", rel, err))
			}
			continue
		}
		if err := tx.repo.Remove(native(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", rel, err))
		}
	}
	for dir, snapshot := range tx.snapshots {
		if err := tx.restoreDir(dir, snapshot); err != nil {
			errs = append(errs, err)
		}
	}
	for _, dir := range tx.createdDirs {
		if err := tx.repo.Remove(native(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove directory %s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

// snapshotDir reads the regular files directly inside dir. A missing
// directory yields a nil snapshot.
func (tx *transaction) snapshotDir(dir string) (map[string][]byte, error) {
	entries, err := fs.ReadDir(tx.repo.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", dir, err)
	}
	out := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("snapshot %s: unexpected subdirectory %s", dir, entry.Name())
		}
		raw, err := tx.repo.ReadFile(native(dir + "/" + entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", dir, err)
		}
		out[entry.Name()] = raw
	}
	return out, nil
}

// restoreDir makes dir match snapshot exactly (nil: dir removed).
func (tx *transaction) restoreDir(dir string, snapshot map[string][]byte) error {
	if snapshot == nil {
		if err := tx.repo.RemoveAll(native(dir)); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		return nil
	}
	entries, err := fs.ReadDir(tx.repo.FS(), dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("restore %s: %w", dir, err)
	}
	if err := tx.repo.MkdirAll(native(dir), dirMode); err != nil {
		return fmt.Errorf("restore %s: %w", dir, err)
	}
	for _, entry := range entries {
		if _, keep := snapshot[entry.Name()]; !keep {
			if err := tx.repo.RemoveAll(native(dir + "/" + entry.Name())); err != nil {
				return fmt.Errorf("restore %s: %w", dir, err)
			}
		}
	}
	for name, content := range snapshot {
		if err := tx.repo.WriteFile(native(dir+"/"+name), content, newFileMode); err != nil {
			return fmt.Errorf("restore %s: %w", dir, err)
		}
	}
	return nil
}

// unifiedDiff renders a minimal unified diff (3 lines of context) between
// before and after. A created file diffs against /dev/null.
func unifiedDiff(rel string, created bool, before, after string) string {
	var b bytes.Buffer
	from := "a/" + rel
	if created {
		from = "/dev/null"
	}
	fmt.Fprintf(&b, "--- %s\n+++ b/%s\n", from, rel)
	oldLines, newLines := splitLines(before), splitLines(after)
	edits := lineEdits(oldLines, newLines)
	for _, h := range hunks(edits, 3) {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
		for _, e := range edits[h.from:h.to] {
			b.WriteString(string(e.kind) + e.text + "\n")
		}
	}
	return b.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type edit struct {
	kind byte // ' ', '-', '+'
	text string
}

// lineEdits is a longest-common-subsequence line diff. Generated and edited
// files are a few hundred lines, so the O(n*m) table is fine.
func lineEdits(a, b []string) []edit {
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	edits := make([]edit, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			edits = append(edits, edit{' ', a[i]})
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			// Deletions before additions, the conventional diff order.
			edits = append(edits, edit{'-', a[i]})
			i++
		default:
			edits = append(edits, edit{'+', b[j]})
			j++
		}
	}
	return edits
}

type hunk struct {
	from, to           int // edit index range
	oldStart, oldCount int
	newStart, newCount int
}

// hunks groups changed edits with up to context unchanged lines around them.
func hunks(edits []edit, context int) []hunk {
	var out []hunk
	for i := 0; i < len(edits); {
		if edits[i].kind == ' ' {
			i++
			continue
		}
		from := max(i-context, 0)
		to := i
		for to < len(edits) {
			if edits[to].kind != ' ' {
				to++
				continue
			}
			run := to
			for run < len(edits) && edits[run].kind == ' ' {
				run++
			}
			if run == len(edits) || run-to > 2*context {
				to = min(to+context, len(edits))
				break
			}
			to = run
		}
		out = append(out, newHunk(edits, from, to))
		i = to
	}
	return out
}

func newHunk(edits []edit, from, to int) hunk {
	h := hunk{from: from, to: to, oldStart: 1, newStart: 1}
	for _, e := range edits[:from] {
		if e.kind != '+' {
			h.oldStart++
		}
		if e.kind != '-' {
			h.newStart++
		}
	}
	for _, e := range edits[from:to] {
		if e.kind != '+' {
			h.oldCount++
		}
		if e.kind != '-' {
			h.newCount++
		}
	}
	if h.oldCount == 0 {
		h.oldStart--
	}
	if h.newCount == 0 {
		h.newStart--
	}
	return h
}
