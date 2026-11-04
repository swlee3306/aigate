package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
	"aigate/internal/report"
)

const exitGateError = 64

// requireTTY is a variable so tests can exercise the interactive path.
var requireTTY = func() error { return approval.RequireTTY(os.Stdin) }

const usage = `usage:
  aigate run --repo DIR --base REF [--head HEAD] [--task-class NAME] [--approve HASH]
  aigate check REPORT.json --repo DIR
  aigate approve --repo DIR --base REF [--head HEAD]
  aigate ledger verify [--repo DIR]
exit codes: 0 ok, 1 behavior_failure, 2 verification_incomplete, 3 check_weakened, 64 gate error
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

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "aigate: gate error: %v\n", err)
	return exitGateError
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("run", stderr)
	o := gate.Options{}
	fs.StringVar(&o.Repo, "repo", ".", "repository directory")
	fs.StringVar(&o.Base, "base", "", "base ref (default: merge-base with origin default branch)")
	fs.StringVar(&o.Head, "head", "HEAD", "head ref")
	fs.StringVar(&o.TaskClass, "task-class", "", "task class recorded in the ledger")
	fs.StringVar(&o.ApproveHash, "approve", "", "protected-diff hash (must also be granted via 'aigate approve')")
	if c, done := parse(fs, args); done {
		return c
	}
	r, err := gate.Run(context.Background(), o)
	if err != nil {
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
	if c, done := parse(fs, args); done {
		return c
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	if path == "" {
		fmt.Fprint(stderr, usage)
		return exitGateError
	}
	rep, err := report.Load(path)
	if err != nil {
		return fail(stderr, err)
	}
	id, top, err := gate.RepoID(*repo)
	if err != nil {
		return fail(stderr, err)
	}
	head, err := gitx.Resolve(top, "HEAD")
	if err != nil {
		return fail(stderr, err)
	}
	dirty, err := gitx.Dirty(top)
	if err != nil {
		return fail(stderr, err)
	}
	home, err := gate.Home("")
	if err != nil {
		return fail(stderr, err)
	}
	st := approval.Store{Path: gate.ApprovalsPath(home)}
	problems := report.Check(rep, head, dirty != "", func(h string) bool { return st.Valid(id, rep.Base, h) })
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
	p, err := gate.Prepare(o)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "base: %s\nhead: %s\n", p.Base, p.Head)
	fmt.Fprintf(stdout, "files touched (%d):\n", len(p.Diff.TouchedFiles))
	for _, f := range p.Diff.TouchedFiles {
		fmt.Fprintf(stdout, "  %s\n", f)
	}
	fmt.Fprintf(stdout, "removed/changed lines (%d):\n", len(p.Diff.RemovedLines))
	for i, l := range p.Diff.RemovedLines {
		if i == 40 {
			fmt.Fprintf(stdout, "  ... (%d more)\n", len(p.Diff.RemovedLines)-40)
			break
		}
		fmt.Fprintf(stdout, "  - %s\n", l)
	}
	fmt.Fprintf(stdout, "added guarded files (%d):\n", len(p.AddedGuarded))
	for _, f := range p.AddedGuarded {
		fmt.Fprintf(stdout, "  + %s\n", f)
	}
	fmt.Fprintf(stdout, "hash: %s\n", p.Diff.Hash)
	if len(p.Diff.RemovedLines) == 0 && len(p.AddedGuarded) == 0 {
		fmt.Fprintln(stdout, "nothing needs approval.")
		return 0
	}
	fmt.Fprint(stdout, "Type 'yes' to approve this exact diff: ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		fmt.Fprintln(stderr, "aigate: not approved")
		return exitGateError
	}
	home, err := gate.Home("")
	if err != nil {
		return fail(stderr, err)
	}
	st := approval.Store{Path: gate.ApprovalsPath(home)}
	if err := st.Grant(p.RepoID, p.Base, p.Diff.Hash, time.Now().UTC()); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "approved.")
	return 0
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
