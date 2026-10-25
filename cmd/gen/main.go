// Command gen scaffolds code that follows ADR 0002 (module layering), so the
// architecture tests in test/arch pass for generated code with no allowlist
// entries.
//
//	go run ./cmd/gen module   [--dry-run] <name>
//	go run ./cmd/gen resource [--dry-run] [--plural <plural>] --module <module> <name>
//	go run ./cmd/gen command  [--dry-run] [--no-http] --module <module> <Name>
//	go run ./cmd/gen query    [--dry-run] [--no-http] --module <module> <Name>
//
// Every run renders all files in memory first, writes them, then runs sqlc
// (module/resource), go build ./..., go vet (with and without the integration
// tag), the module's tests, and test/arch. If any step fails, every file the
// run wrote is restored or removed. --dry-run prints a unified diff and
// writes nothing. Flags may follow the name.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"time"
)

const usage = `Usage:
  go run ./cmd/gen module   [--dry-run] <name>
  go run ./cmd/gen resource [--dry-run] [--plural <plural>] --module <module> <name>
  go run ./cmd/gen command  [--dry-run] [--no-http] --module <module> <Name>
  go run ./cmd/gen query    [--dry-run] [--no-http] --module <module> <Name>

Make targets: make gen module=billing | make gen resource=invoice module=billing |
make gen command=IssueInvoice module=billing | make gen query=ListOverdueInvoices module=billing
(add dry_run=1 for --dry-run and no_http=1 for --no-http).

module    internal/modules/<name>: module.go, build.go, catalog/, service/ports.go,
          repository/, handler/, dto/, queries/, sqlc.yaml, README.md, tests, a
          <UTC ts>_<name>_init migration pair, and the internal/bootstrap entry.
resource  a tenant-scoped CRUD slice (<module>_<plural> table, sqlc queries,
          repository, service, handler, dto, routes, permissions, error code,
          en/vi strings, unit and integration tests) in a cmd/gen module.
command   service/command_<name>.go (+ HTTP POST route unless --no-http).
query     service/query_<name>.go (+ HTTP GET route unless --no-http).
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		if errors.Is(err, errUsage) {
			if err != errUsage {
				fmt.Fprintln(os.Stderr, "gen:", err)
			}
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

// options are the parsed flags of one subcommand.
type options struct {
	name    string
	module  string
	plural  string
	dryRun  bool
	noHTTP  bool
	rootDir string
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return errUsage
	}
	sub := args[0]
	opts, err := parseOptions(sub, args[1:])
	if err != nil {
		return err
	}
	root := opts.rootDir
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if root, err = findRoot(wd); err != nil {
			return err
		}
	}
	repo, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	modulePath, err := readModulePath(repo)
	if err != nil {
		return err
	}
	g := &generator{
		repo:       repo,
		modulePath: modulePath,
		dryRun:     opts.dryRun,
		out:        out,
		now:        time.Now,
		run: func(ctx context.Context, name string, args ...string) error {
			return runCommand(ctx, root, out, name, args...)
		},
	}
	switch sub {
	case "module":
		return g.generateModule(ctx, opts.name)
	case "resource":
		return g.generateResource(ctx, opts.module, opts.name, opts.plural)
	case kindCommand, kindQuery:
		return g.generateUseCase(ctx, sub, opts.module, opts.name, !opts.noHTTP)
	default:
		return fmt.Errorf("unknown subcommand %q: %w", sub, errUsage)
	}
}

// parseOptions parses flags before and after the single positional name.
func parseOptions(sub string, args []string) (options, error) {
	var opts options
	fs := flag.NewFlagSet(sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print a diff and write nothing")
	fs.StringVar(&opts.rootDir, "root", "", "repository root (default: the go.mod above the working directory)")
	if sub != "module" {
		fs.StringVar(&opts.module, "module", "", "target module (created by cmd/gen)")
	}
	if sub == "resource" {
		fs.StringVar(&opts.plural, "plural", "", "plural of an irregular resource name (default: English rules)")
	}
	if sub == kindCommand || sub == kindQuery {
		fs.BoolVar(&opts.noHTTP, "no-http", false, "generate only the service use case, no handler/route")
	}
	if err := fs.Parse(args); err != nil {
		return opts, fmt.Errorf("%s: %v: %w", sub, err, errUsage)
	}
	if fs.NArg() == 0 {
		return opts, fmt.Errorf("%s: missing name: %w", sub, errUsage)
	}
	opts.name = fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return opts, fmt.Errorf("%s: %v: %w", sub, err, errUsage)
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("%s: expected exactly one name, got extra %q: %w", sub, fs.Args(), errUsage)
	}
	if sub != "module" && opts.module == "" {
		return opts, fmt.Errorf("%s: --module is required: %w", sub, errUsage)
	}
	return opts, nil
}

// runCommand runs one verification command in root, streaming its output.
func runCommand(ctx context.Context, root string, out io.Writer, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		if name == "sqlc" {
			return fmt.Errorf("sqlc not found on PATH; install the CI version: go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1: %w", err)
		}
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}
