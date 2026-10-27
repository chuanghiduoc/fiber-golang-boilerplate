package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// healthcheckCommand is the subcommand used by the container health check.
	// The distroless runtime has no shell, wget, or curl, so the probe is part
	// of the binary itself.
	healthcheckCommand = "healthcheck"
	// defaultHealthcheckTimeout bounds one probe so a wedged listener cannot
	// hold the health check past the container runtime's timeout.
	defaultHealthcheckTimeout = 5 * time.Second
	// defaultAppPort matches config.AppConfig's APP_PORT default. The
	// healthcheck subcommand reads the environment directly instead of loading
	// the full configuration: a probe must work even when a deployment secret
	// is temporarily unreadable.
	defaultAppPort = "8080"
)

// runHealthcheck probes the local readiness endpoint and returns the process
// exit code: 0 when the endpoint answers 200, 1 when it answers anything else
// or cannot be reached, 2 for an invalid invocation. It prints only the URL
// and status, never a response body.
func runHealthcheck(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	port := getenv("APP_PORT")
	if port == "" {
		port = defaultAppPort
	}
	defaultURL := fmt.Sprintf("http://127.0.0.1:%s/readyz", port)

	fs := flag.NewFlagSet(healthcheckCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", defaultURL, "readiness URL to probe")
	timeout := fs.Duration("timeout", defaultHealthcheckTimeout, "request timeout")
	fs.Usage = func() {
		fmt.Fprintf(stdout, "Usage: %s [--url URL] [--timeout DURATION]\n\n", healthcheckCommand)
		fmt.Fprintln(stdout, "Probe the application's readiness endpoint. Exits 0 when it answers 200,")
		fmt.Fprintln(stdout, "1 when it answers any other status or cannot be reached, and 2 for an")
		fmt.Fprintln(stdout, "invalid invocation.")
		fmt.Fprintln(stdout)
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		fs.SetOutput(stderr)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", healthcheckCommand, fs.Arg(0))
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintf(stderr, "%s: --timeout must be positive\n", healthcheckCommand)
		return 2
	}

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(*url)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", healthcheckCommand, *url, err)
		return 1
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Fprintf(stderr, "%s: close response body: %v\n", healthcheckCommand, closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "%s: %s returned HTTP %d\n", healthcheckCommand, *url, resp.StatusCode)
		return 1
	}
	fmt.Fprintf(stdout, "ok: %s\n", *url)
	return 0
}
