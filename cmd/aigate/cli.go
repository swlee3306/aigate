package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/textsafe"
)

const exitGateError = 64

// requireTTY is a variable so tests can exercise the interactive path.
var requireTTY = func() error { return approval.RequireTTY(os.Stdin) }

const usage = `usage:
  aigate run --repo DIR (--base REF | --initial) [--head HEAD] [--task-class NAME] [--approve HASH]
  aigate check REPORT.json --repo DIR --base REF
  aigate approve --repo DIR --base REF [--head HEAD]
  aigate ledger verify [--repo DIR]
exit codes: 0 ok, 1 behavior_failure, 2 verification_incomplete, 3 check_weakened,
  64 gate error (also: interrupted by SIGINT/SIGTERM — nothing is recorded)
check: 0 fresh, 1 not fresh (problems printed), 64 gate error
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

const beginMark, endMark = "----- BEGIN HASHED DIFF -----\n", "----- END HASHED DIFF -----\n"

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
	// Everything shown is checked BEFORE anything is printed. A file that
	// became binary, or a source/config/text-named file that is binary at
	// head, is refused: its summary line would let one NUL byte hide the
	// content the human approves. Then a control sequence (ESC, lone CR, ...)
	// or bidi override in a line or path could make the screen hide what the
	// human approves. Genuine binary files are shown as a summary line; the
	// hash stays over the raw bytes (HashedText).
	if msg, _ := binaryRefusal(p); msg != "" {
		fmt.Fprintf(stderr, "aigate: approve 거부: %s. 바이너리 요약 한 줄로는 사람이 내용을 볼 수 없어 승인하지 않습니다(승인은 기록하지 않음). NUL 바이트를 없앤 커밋으로 다시 실행하세요.\n", msg)
		return exitGateError
	}
	display := p.Diff.DisplayText(textsafe.Escape)
	if msg, _ := unsafeApproveText(p, display); msg != "" {
		fmt.Fprintf(stderr, "aigate: approve 거부: %s. 터미널 제어·비정상 문자가 있는 diff는 화면에서 내용을 숨길 수 있어 승인하지 않습니다(승인은 기록하지 않음). 해당 문자를 없앤 커밋으로 다시 실행하세요.\n", msg)
		return exitGateError
	}
	fmt.Fprintf(stdout, "base: %s\nhead: %s\n", p.Base, p.Head)
	fmt.Fprintf(stdout, "files touched (%d):\n", len(p.Diff.TouchedFiles))
	for _, f := range p.Diff.TouchedFiles {
		fmt.Fprintf(stdout, "  %s\n", f)
	}
	fmt.Fprintf(stdout, "changed or deleted existing protected files (any hunk, additions too): %d; removed lines: %d; added guarded files: %d\n",
		len(p.Diff.Changed()), len(p.Diff.RemovedLines), len(p.AddedGuarded))
	for _, f := range p.Diff.Changed() {
		fmt.Fprintf(stdout, "  ! %s\n", f)
	}
	for _, f := range p.AddedGuarded {
		fmt.Fprintf(stdout, "  + %s\n", f)
	}
	// The full text the hash is computed over, untruncated: the human
	// approves exactly what they read (binary files as a summary line).
	fmt.Fprint(stdout, "the approval binds exactly this text (sha256 prefix = hash):\n", beginMark, display, endMark)
	if n := binaryCount(p); n > 0 {
		fmt.Fprintf(stdout, "(%d binary file(s) shown as a summary line; the hash covers their raw bytes)\n", n)
	}
	fmt.Fprintf(stdout, "hash: %s\n", p.Diff.Hash)
	if !p.NeedsApproval() {
		fmt.Fprintln(stdout, "nothing needs approval.")
		return 0
	}
	fmt.Fprint(stdout, "Type 'yes' to approve this exact diff: ")
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		answer <- line
	}()
	var line string
	select {
	case line = <-answer:
	case <-ctx.Done():
		fmt.Fprintln(stderr, "\naigate: interrupted; not approved")
		return exitGateError
	}
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

func binaryCount(p gate.Protected) int {
	n := 0
	for _, f := range p.Diff.Files {
		if f.Summarised() {
			n++
		}
	}
	return n
}

// Refusal reason codes (also the ledger's approval_refused "reason").
const (
	reasonUnsafeBytes      = "unsafe_bytes"
	reasonTextBecameBinary = "text_became_binary"
	reasonBinarySource     = "binary_source_file"
)

// textLikeExt are extensions (lower case) of files that are source, config
// or text: binary content in them is never a genuine fixture to summarise.
var textLikeExt = map[string]bool{}

// textLikeName are extension-less file names (lower case) treated the same.
var textLikeName = map[string]bool{}

func init() {
	for _, e := range strings.Fields(".ts .tsx .js .jsx .mjs .cjs .go .py .rb .java .kt .c .h .cc .cpp .cs .rs .php .sh .bash .zsh .ini .cfg .conf .toml .yml .yaml .json .xml .html .css .md .txt .lock .mod .sum .gradle .properties .env") {
		textLikeExt[e] = true
	}
	for _, n := range strings.Fields("Makefile Dockerfile Jenkinsfile Rakefile .gitattributes .gitmodules .npmrc conftest") {
		textLikeName[strings.ToLower(n)] = true
	}
}

// isTextLike reports whether a path names a source, config or text file
// (case-insensitive extension, or one of the known extension-less names).
func isTextLike(p string) bool {
	base := strings.ToLower(path.Base(p))
	return textLikeName[base] || textLikeExt[path.Ext(base)]
}

// binaryRefusal returns a description (escaped path, never raw bytes) and a
// reason code when a protected file must not be shown as a binary summary:
// a text file that became binary (M/T), or a file that is binary at head and
// added or modified although its name says source/config/text.
func binaryRefusal(p gate.Protected) (msg, reason string) {
	for _, f := range p.Diff.Files {
		if !f.HeadBinary || f.Status == "D" {
			continue
		}
		where := "NUL 바이트"
		if f.HeadNULLine > 0 {
			where = fmt.Sprintf("head %d번째 줄에 \\x00(NUL) 바이트", f.HeadNULLine)
		}
		if f.Status != "A" && !f.BaseBinary {
			return fmt.Sprintf("보호 파일 '%s' 가 텍스트에서 바이너리로 바뀌었습니다(base는 텍스트, %s)", textsafe.Escape(f.Path), where), reasonTextBecameBinary
		}
		if isTextLike(f.Path) {
			return fmt.Sprintf("보호 파일 '%s' 는 소스·설정·텍스트 파일 이름인데 head에서 바이너리입니다(%s)", textsafe.Escape(f.Path), where), reasonBinarySource
		}
	}
	return "", ""
}

// unsafeApproveText returns a description (with escapes only, never the raw
// bytes) of the first unsafe byte in a path or line approve would print, and
// the reason code, or "" when everything is safe to show.
func unsafeApproveText(p gate.Protected, display string) (string, string) {
	lists := []struct {
		what  string
		paths []string
	}{{"변경 파일", p.Diff.TouchedFiles}, {"변경·삭제 파일", p.Diff.Changed()}, {"새 보호 파일", p.AddedGuarded}, {"새 파일", p.Diff.AddedFiles}}
	for _, l := range lists {
		for i, f := range l.paths {
			// a path is printed on one line: a newline in it is unsafe too
			if fd, bad := textsafe.FirstUnsafe(f); bad {
				return fmt.Sprintf("%s 목록 %d번째 경로 '%s' 의 %d번째 바이트에 %s", l.what, i+1, textsafe.Escape(f), fd.Offset+1, fd.Desc), reasonUnsafeBytes
			} else if j := strings.IndexByte(f, '\n'); j >= 0 {
				return fmt.Sprintf("%s 목록 %d번째 경로 '%s' 의 %d번째 바이트에 \\x0a(줄바꿈)", l.what, i+1, textsafe.Escape(f), j+1), reasonUnsafeBytes
			}
		}
	}
	if fd, bad := textsafe.FirstUnsafe(display); bad {
		file := "(파일 머리 앞)"
		lines := strings.Split(display[:fd.Offset], "\n")
		for k := len(lines) - 1; k >= 0; k-- {
			if strings.HasPrefix(lines[k], "diff --git ") || strings.HasPrefix(lines[k], "A ") {
				file = textsafe.Escape(lines[k])
				break
			}
		}
		return fmt.Sprintf("승인할 diff 텍스트 %d번째 줄(%s)에 %s", fd.Line, file, fd.Desc), reasonUnsafeBytes
	}
	return "", ""
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
