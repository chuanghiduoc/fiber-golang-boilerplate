package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage/scan"
)

type fakeScanner struct {
	verdict scan.Verdict
	err     error
	calls   int
	data    []byte
	onScan  func()
}

func (f *fakeScanner) Scan(_ context.Context, r io.Reader, _ int64) (scan.Verdict, error) {
	f.calls++
	data, err := io.ReadAll(r)
	if err != nil {
		return scan.VerdictClean, err
	}
	f.data = data
	if f.onScan != nil {
		f.onScan()
	}
	return f.verdict, f.err
}

func scanService(repo *mockFileRepo, store *mockStorage, scanner scan.Scanner) UploadService {
	return NewUploadService(repo, newFakeTx(repo), store, testAuthorizer(), UploadOptions{Scanner: scanner})
}

func requireFileCode(t *testing.T, err error, code apperror.Code, status int) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want an apperror", err)
	}
	if appErr.Code != string(code) || resolvedStatus(appErr) != status {
		t.Fatalf("error = %q/%d, want %q/%d", appErr.Code, resolvedStatus(appErr), code, status)
	}
}

func TestUpload_ScanInfectedRejectsAndRemovesBlob(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	scanner := &fakeScanner{verdict: scan.VerdictInfected}
	svc := scanService(repo, store, scanner)

	_, err := svc.Upload(systemCtx(), testID(10), "evil.bin", strings.NewReader("malware"), 7, "application/octet-stream")
	requireFileCode(t, err, catalog.CodeFileInfected, 422)
	if scanner.calls != 1 {
		t.Fatalf("scanner calls = %d, want 1", scanner.calls)
	}
	if len(repo.files) != 0 {
		t.Fatal("an infected upload must not create metadata")
	}
	if len(store.files) != 0 {
		t.Fatal("an infected blob must be removed")
	}
	if len(store.deletedPaths) != 1 {
		t.Fatalf("deleted blobs = %d, want 1", len(store.deletedPaths))
	}
}

func TestUpload_ScanErrorFailsClosedAndRemovesBlob(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	scanner := &fakeScanner{err: errors.New("clamd unavailable")}
	svc := scanService(repo, store, scanner)

	_, err := svc.Upload(systemCtx(), testID(10), "maybe.bin", strings.NewReader("data"), 4, "application/octet-stream")
	requireFileCode(t, err, catalog.CodeFileScanUnavailable, 503)
	if len(repo.files) != 0 {
		t.Fatal("a failed scan must not create metadata")
	}
	if len(store.files) != 0 || len(store.deletedPaths) != 1 {
		t.Fatal("a failed scan must remove the stored blob")
	}
}

func TestUpload_ScanUnknownVerdictFailsClosed(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	scanner := &fakeScanner{verdict: scan.Verdict(99)}
	svc := scanService(repo, store, scanner)

	_, err := svc.Upload(systemCtx(), testID(10), "odd.bin", strings.NewReader("data"), 4, "application/octet-stream")
	requireFileCode(t, err, catalog.CodeFileScanUnavailable, 503)
	if len(repo.files) != 0 || len(store.files) != 0 {
		t.Fatal("an unknown verdict must not keep metadata or the blob")
	}
}

func TestUpload_ScanReadFailureFailsClosed(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	store.getErr = errors.New("storage unavailable")
	svc := scanService(repo, store, &fakeScanner{verdict: scan.VerdictClean})

	_, err := svc.Upload(systemCtx(), testID(10), "unreadable.bin", strings.NewReader("data"), 4, "application/octet-stream")
	requireFileCode(t, err, catalog.CodeFileScanUnavailable, 503)
	if len(repo.files) != 0 || len(store.files) != 0 {
		t.Fatal("a scan read failure must not keep metadata or the blob")
	}
}

func TestUpload_ScanRunsBeforeMetadataCommit(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	scanner := &fakeScanner{verdict: scan.VerdictClean}
	scanner.onScan = func() {
		if len(repo.files) != 0 {
			t.Errorf("metadata was committed before the scan: %d rows", len(repo.files))
		}
	}
	svc := scanService(repo, store, scanner)

	file, err := svc.Upload(systemCtx(), testID(10), "safe.bin", strings.NewReader("safe"), 4, "application/octet-stream")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if scanner.calls != 1 {
		t.Fatalf("scanner calls = %d, want 1", scanner.calls)
	}
	if string(scanner.data) != "safe" {
		t.Fatalf("scanned data = %q, want the stored blob", scanner.data)
	}
	if file == nil || len(repo.files) != 1 {
		t.Fatal("a clean scan must commit the metadata row")
	}
	if len(store.deletedPaths) != 0 {
		t.Fatal("a clean scan must keep the blob")
	}
}
