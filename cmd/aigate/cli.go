package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"aigate/internal/approval"
	"aigate/internal/approval/ui"
	"aigate/internal/gate"
	"aigate/internal/ledger"
)

const exitGateError = 64

// requireTTY is a variable so tests can exercise the interactive path.
var requireTTY = func() error { return approval.RequireTTY(os.Stdin) }

const usage = `usage:
  aigate run --repo DIR (--base REF | --initial) [--head HEAD] [--task-class NAME] [--approve HASH]
  aigate check REPORT.json --repo DIR --base REF
  aigate approve --repo DIR --base REF [--head HEAD]
  aigate ledger verify [--repo DIR]
  aigate stats [--repo DIR] [--days N (default 14)] [--json]
exit codes: 0 ok, 1 behavior_failure, 2 verification_incomplete, 3 check_weakened,
  64 gate error (also: interrupted by SIGINT/SIGTERM — nothing is recorded)
check: 0 fresh, 1 not fresh (problems printed), 64 gate error
stats: per-UTC-day counts from the ledger for today and the N-1 days before,
  plus a per-rule count of runs; 0 ok (also "no events"), 1 ledger fails to
  verify, 64 gate error
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitGateError
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "approve":
		return cmdApprove(args[1:], stdin, stdout, stderr)
	case "ledger":
		return cmdLedger(args[1:], stdout, stderr)
	case "stats":
		return cmdStats(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "aigate: unknown subcommand %q\n%s", args[0], usage)
	return exitGateError
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("aigate "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parse returns (code, done): done means the caller should return code.
func parse(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return exitGateError, true
	}
	return 0, false
}

// signalContext is cancelled on SIGINT or SIGTERM. The runner then kills
// the check's process group and the gate removes its worktrees; the command
// ends with a gate error (64), never a verdict.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "aigate: gate error: %v\n", err)
	return exitGateError
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("run", stderr)
	o := gate.Options{}
	fs.StringVar(&o.Repo, "repo", ".", "repository directory")
	fs.StringVar(&o.Base, "base", "", "base ref the task started from (required unless --initial)")
	fs.BoolVar(&o.Initial, "initial", false, "initial mode: no base, head-only green check (the report is not completion evidence for check)")
	fs.StringVar(&o.Head, "head", "HEAD", "head ref")
	fs.StringVar(&o.TaskClass, "task-class", "", "task class recorded in the ledger")
	fs.StringVar(&o.ApproveHash, "approve", "", "protected-diff hash (must also be granted via 'aigate approve')")
	if c, done := parse(fs, args); done {
		return c
	}
	switch {
	case o.Base == "" && !o.Initial:
		fmt.Fprintf(stderr, "aigate: run needs --base REF (the commit the task started from) or --initial\n%s", usage)
		return exitGateError
	case o.Base != "" && o.Initial:
		fmt.Fprintf(stderr, "aigate: --base and --initial are mutually exclusive\n%s", usage)
		return exitGateError
	}
	ctx, stop := signalContext()
	defer stop()
	r, err := gate.Run(ctx, o)
	if err != nil {
		if ctx.Err() != nil {
			return fail(stderr, fmt.Errorf("interrupted; nothing recorded: %w", err))
		}
		return fail(stderr, err)
	}
	fmt.Fprint(stdout, r.Markdown())
	return r.ExitCode
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	var path string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	fs := newFlags("check", stderr)
	repo := fs.String("repo", ".", "repository directory")
	base := fs.String("base", "", "base ref the run was made against (required)")
	if c, done := parse(fs, args); done {
		return c
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	if path == "" || *base == "" {
		fmt.Fprintf(stderr, "aigate: check needs REPORT.json and --base REF\n%s", usage)
		return exitGateError
	}
	ctx, stop := signalContext()
	defer stop()
	problems, err := gate.CheckReport(gate.CheckOptions{Repo: *repo, Base: *base, ReportPath: path})
	if err != nil {
		return fail(stderr, err)
	}
	if ctx.Err() != nil {
		return fail(stderr, errors.New("interrupted"))
	}
	if len(problems) == 0 {
		fmt.Fprintln(stdout, "fresh")
		return 0
	}
	for _, p := range problems {
		fmt.Fprintln(stdout, p)
	}
	return 1
}

func cmdApprove(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("approve", stderr)
	o := gate.Options{}
	fs.StringVar(&o.Repo, "repo", ".", "repository directory")
	fs.StringVar(&o.Base, "base", "", "base ref")
	fs.StringVar(&o.Head, "head", "HEAD", "head ref")
	if c, done := parse(fs, args); done {
		return c
	}
	if err := requireTTY(); err != nil {
		fmt.Fprintf(stderr, "aigate: approve refused: %v (a human must run this in a terminal)\n", err)
		return exitGateError
	}
	ctx, stop := signalContext()
	defer stop()
	p, err := gate.Prepare(o)
	if err != nil {
		return fail(stderr, err)
	}
	home, err := gate.Home("")
	if err != nil {
		return fail(stderr, err)
	}
	// The checks, the diff screen and the prompt are shared with `orch approve`.
	switch ui.Review(ctx, p, gate.LedgerPath(home, p.RepoID), "aigate", stdin, stdout, stderr) {
	case ui.NothingToApprove:
		return 0
	case ui.Confirmed:
		return ui.Record(p, home, "aigate", stdout, stderr)
	}
	return exitGateError
}

func cmdLedger(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprint(stderr, usage)
		return exitGateError
	}
	fs := newFlags("ledger verify", stderr)
	repo := fs.String("repo", ".", "repository directory")
	if c, done := parse(fs, args[1:]); done {
		return c
	}
	id, _, err := gate.RepoID(*repo)
	if err != nil {
		return fail(stderr, err)
	}
	home, err := gate.Home("")
	if err != nil {
		return fail(stderr, err)
	}
	n, err := ledger.Verify(filepath.Clean(gate.LedgerPath(home, id)))
	if err != nil {
		fmt.Fprintf(stdout, "FAIL after %d valid events: %v\n", n, err)
		return 1
	}
	fmt.Fprintf(stdout, "OK %d events\n", n)
	return 0
}
