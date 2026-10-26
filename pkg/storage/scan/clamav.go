package scan

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// clamavChunkSize bounds one INSTREAM chunk. clamd reads the stream
// incrementally, so a large upload does not need to fit memory.
const clamavChunkSize = 32 << 10

// maxClamdResponseBytes bounds the NUL-terminated reply. A real reply is one
// short line; anything larger is a broken or hostile peer.
const maxClamdResponseBytes = 8 << 10

// clamavScanner implements the clamd INSTREAM protocol:
//
//	client: "zINSTREAM\0"
//	client: [4-byte big-endian length][chunk]... [0x00000000]
//	server: "stream: OK\0" or "stream: <signature> FOUND\0"
//
// A response that is neither OK nor FOUND is an error, so a protocol problem
// fails closed.
type clamavScanner struct {
	addr    string
	timeout time.Duration
}

func (s *clamavScanner) Scan(ctx context.Context, r io.Reader, _ int64) (Verdict, error) {
	dialer := net.Dialer{Timeout: s.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return VerdictClean, fmt.Errorf("scan: connect to clamd: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Closing the socket is the only way to cancel a running INSTREAM
	// exchange, so a deadline-less caller cancellation must close it too.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	// Honor whichever ends first: the caller's context or the configured
	// scan timeout. clamd has no request cancellation, so closing the socket
	// (deferred) is what actually stops the work.
	deadline := time.Now().Add(s.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return VerdictClean, fmt.Errorf("scan: set clamd deadline: %w", err)
	}

	if _, err := io.WriteString(conn, "zINSTREAM\x00"); err != nil {
		return VerdictClean, fmt.Errorf("scan: send INSTREAM: %w", err)
	}
	buf := make([]byte, clamavChunkSize)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			var size [4]byte
			binary.BigEndian.PutUint32(size[:], uint32(n)) // #nosec G115 -- n <= len(buf) = clamavChunkSize (32 KiB)
			if _, err := conn.Write(size[:]); err != nil {
				return VerdictClean, fmt.Errorf("scan: send chunk length: %w", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return VerdictClean, fmt.Errorf("scan: send chunk: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return VerdictClean, fmt.Errorf("scan: read upload: %w", readErr)
		}
	}
	// A zero-length chunk terminates the stream.
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return VerdictClean, fmt.Errorf("scan: send INSTREAM terminator: %w", err)
	}

	response, err := bufio.NewReader(io.LimitReader(conn, maxClamdResponseBytes)).ReadString(0)
	if err != nil {
		return VerdictClean, fmt.Errorf("scan: read clamd response: %w", err)
	}
	return parseClamdResponse(strings.TrimSuffix(response, "\x00"))
}

// parseClamdResponse maps clamd's INSTREAM reply to a verdict. clamd answers
// "stream: OK" for a clean stream and "stream: <signature> FOUND" for a match;
// every other reply (including a size-limit or protocol ERROR) is a scan
// failure, which the caller treats as fail closed.
func parseClamdResponse(response string) (Verdict, error) {
	response = strings.TrimSpace(response)
	switch {
	case response == "stream: OK":
		return VerdictClean, nil
	case strings.HasSuffix(response, " FOUND"):
		return VerdictInfected, nil
	default:
		return VerdictClean, fmt.Errorf("scan: clamd returned %q", response)
	}
}
