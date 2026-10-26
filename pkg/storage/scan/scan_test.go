package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClamd is a minimal clamd INSTREAM server: it verifies the command and
// chunk framing, records the streamed payload, and answers with response.
type fakeClamd struct {
	listener net.Listener
	response string

	mu      sync.Mutex
	payload []byte
	command string
	err     error
	done    chan struct{}
}

func newFakeClamd(t *testing.T, response string) *fakeClamd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeClamd{listener: ln, response: response, done: make(chan struct{})}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeClamd) addr() string { return f.listener.Addr().String() }

func (f *fakeClamd) serve() {
	defer close(f.done)
	conn, err := f.listener.Accept()
	if err != nil {
		f.recordErr(err)
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	command, err := readUntilNUL(conn)
	if err != nil {
		f.recordErr(err)
		return
	}
	f.mu.Lock()
	f.command = command
	f.mu.Unlock()

	for {
		var sizeBuf [4]byte
		if _, err := io.ReadFull(conn, sizeBuf[:]); err != nil {
			f.recordErr(err)
			return
		}
		size := binary.BigEndian.Uint32(sizeBuf[:])
		if size == 0 {
			break
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(conn, chunk); err != nil {
			f.recordErr(err)
			return
		}
		f.mu.Lock()
		f.payload = append(f.payload, chunk...)
		f.mu.Unlock()
	}
	if _, err := conn.Write([]byte(f.response + "\x00")); err != nil {
		f.recordErr(err)
	}
}

func readUntilNUL(r io.Reader) (string, error) {
	var b strings.Builder
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			return "", err
		}
		if one[0] == 0 {
			return b.String(), nil
		}
		b.WriteByte(one[0])
	}
}

func (f *fakeClamd) recordErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeClamd) result() (payload []byte, command string, err error) {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payload, f.command, f.err
}

func TestClamAVScanClean(t *testing.T) {
	server := newFakeClamd(t, "stream: OK")
	scanner, err := New(Config{Driver: "clamav", ClamAVAddr: server.addr(), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := []byte("safe file contents")
	verdict, err := scanner.Scan(context.Background(), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if verdict != VerdictClean {
		t.Fatalf("verdict = %v, want clean", verdict)
	}
	payload, command, serveErr := server.result()
	if serveErr != nil {
		t.Fatalf("fake clamd: %v", serveErr)
	}
	if command != "zINSTREAM" {
		t.Fatalf("command = %q, want zINSTREAM", command)
	}
	if !bytes.Equal(payload, body) {
		t.Fatalf("streamed payload = %q, want %q", payload, body)
	}
}

func TestClamAVScanInfected(t *testing.T) {
	server := newFakeClamd(t, "stream: Eicar-Test-Signature FOUND")
	scanner, err := New(Config{Driver: "clamav", ClamAVAddr: server.addr(), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	verdict, err := scanner.Scan(context.Background(), strings.NewReader("X5O!P%@AP"), 9)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if verdict != VerdictInfected {
		t.Fatalf("verdict = %v, want infected", verdict)
	}
}

// TestClamAVScanEmptyStream checks the zero-length terminator path: a zero-byte
// upload still sends zINSTREAM then an empty stream.
func TestClamAVScanEmptyStream(t *testing.T) {
	server := newFakeClamd(t, "stream: OK")
	scanner, err := New(Config{Driver: "clamav", ClamAVAddr: server.addr(), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if verdict, err := scanner.Scan(context.Background(), bytes.NewReader(nil), 0); err != nil || verdict != VerdictClean {
		t.Fatalf("Scan(empty) = %v, %v; want clean, nil", verdict, err)
	}
	payload, _, serveErr := server.result()
	if serveErr != nil {
		t.Fatalf("fake clamd: %v", serveErr)
	}
	if len(payload) != 0 {
		t.Fatalf("payload = %q, want empty", payload)
	}
}

func TestClamAVScanServerErrorFailsClosed(t *testing.T) {
	server := newFakeClamd(t, "INSTREAM size limit exceeded. ERROR")
	scanner, err := New(Config{Driver: "clamav", ClamAVAddr: server.addr(), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := scanner.Scan(context.Background(), strings.NewReader("data"), 4); err == nil {
		t.Fatal("Scan must return an error for a clamd ERROR response")
	}
}

func TestClamAVScanUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	requireClosed(t, ln)
	scanner, err := New(Config{Driver: "clamav", ClamAVAddr: addr, Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := scanner.Scan(context.Background(), strings.NewReader("data"), 4); err == nil {
		t.Fatal("Scan must fail when clamd is unreachable")
	}
}

func requireClosed(t *testing.T, ln net.Listener) {
	t.Helper()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
}

func TestNewNoneDriver(t *testing.T) {
	scanner, err := New(Config{Driver: "none"})
	if err != nil {
		t.Fatalf("New(none): %v", err)
	}
	verdict, err := scanner.Scan(context.Background(), strings.NewReader("anything"), 8)
	if err != nil || verdict != VerdictClean {
		t.Fatalf("Scan(none) = %v, %v; want clean, nil", verdict, err)
	}
}

func TestNewRejectsUnknownDriverAndMissingAddress(t *testing.T) {
	if _, err := New(Config{Driver: "virustotal"}); err == nil {
		t.Fatal("New must reject an unknown driver")
	}
	if _, err := New(Config{Driver: "clamav"}); err == nil {
		t.Fatal("New must require FILES_CLAMAV_ADDR for clamav")
	}
}

// TestClamAVScanHonoursCancel covers a caller that cancels without a deadline:
// the connection must be closed so the scan does not run to its timeout.
func TestClamAVScanHonoursCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = readUntilNUL(conn)
		<-time.After(2 * time.Second)
	}()

	scanner, err := New(Config{Driver: "clamav", ClamAVAddr: ln.Addr().String(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = scanner.Scan(ctx, strings.NewReader("data"), 4)
	if err == nil {
		t.Fatal("Scan must fail when the context is canceled")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Scan took %v, want it to stop on cancellation", elapsed)
	}
}

// TestClamAVScanHonoursContext proves a stalled clamd cannot hang an upload
// past its context deadline.
func TestClamAVScanHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		// Read the command then never answer, so only the deadline frees the
		// client.
		defer func() { _ = conn.Close() }()
		_, _ = readUntilNUL(conn)
		<-time.After(2 * time.Second)
	}()

	scanner, err := New(Config{Driver: "clamav", ClamAVAddr: ln.Addr().String(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = scanner.Scan(ctx, strings.NewReader("data"), 4)
	if err == nil {
		t.Fatal("Scan must fail when the context expires")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Scan took %v, want about the 200ms context deadline", elapsed)
	}
}
