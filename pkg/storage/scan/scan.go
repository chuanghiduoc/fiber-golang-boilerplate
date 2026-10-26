// Package scan is the upload malware-scanning hook. It stays framework-free and
// independent of the storage driver: the files service opens the stored blob and
// streams it through a Scanner before committing metadata.
package scan

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Verdict is the scanner's conclusion for one stream.
type Verdict int

const (
	// VerdictClean means the scanner found nothing.
	VerdictClean Verdict = iota
	// VerdictInfected means the scanner matched a malware signature.
	VerdictInfected
)

func (v Verdict) String() string {
	switch v {
	case VerdictClean:
		return "clean"
	case VerdictInfected:
		return "infected"
	default:
		return fmt.Sprintf("verdict(%d)", int(v))
	}
}

// Scanner inspects exactly one upload. size is the expected stream length and
// is advisory: drivers stream until EOF, so a lying size cannot truncate the
// scan. An error means the verdict is unknown and callers must fail closed.
type Scanner interface {
	Scan(ctx context.Context, r io.Reader, size int64) (Verdict, error)
}

// Config selects and configures a driver.
type Config struct {
	// Driver is "none" (default) or "clamav".
	Driver string
	// ClamAVAddr is clamd's host:port.
	ClamAVAddr string
	// Timeout bounds one connection and its response. Non-positive uses
	// DefaultClamAVTimeout.
	Timeout time.Duration
}

// DefaultClamAVTimeout bounds a scan when Config.Timeout is unset.
const DefaultClamAVTimeout = 10 * time.Second

// New builds the configured Scanner. The none driver is a deliberate no-op;
// the clamav driver talks clamd INSTREAM over TCP.
func New(cfg Config) (Scanner, error) {
	switch cfg.Driver {
	case "", "none":
		return noneScanner{}, nil
	case "clamav":
		if cfg.ClamAVAddr == "" {
			return nil, fmt.Errorf("scan: clamav driver requires an address")
		}
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultClamAVTimeout
		}
		return &clamavScanner{addr: cfg.ClamAVAddr, timeout: timeout}, nil
	default:
		return nil, fmt.Errorf("scan: unsupported driver %q (supported: none, clamav)", cfg.Driver)
	}
}

// noneScanner accepts every upload without reading it.
type noneScanner struct{}

func (noneScanner) Scan(context.Context, io.Reader, int64) (Verdict, error) {
	return VerdictClean, nil
}
