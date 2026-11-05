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
	"aigate/internal/gitx"
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
  aigate stats [--repo DIR] [--days N (default 14)] [--json]
exit codes: 0 ok, 1 behavior_failure, 2 verification_incomplete, 3 check_weakened,
  64 gate error (also: interrupted by SIGINT/SIGTERM — nothing is recorded)
check: 0 fresh, 1 not fresh (problems printed), 64 gate error
stats: per-UTC-day counts from the ledger for today and the N-1 days before;
  0 ok (also "no events"), 1 ledger fails to verify, 64 gate error
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
	// became binary or became text, or any file binary at head that is not an
	// allowlisted genuine binary type (binaryRefusal), is refused: a summary
	// line would let one NUL byte hide the content the human approves. Then a
	// control sequence (ESC, lone CR, ...) or bidi override in a line or path
	// could make the screen hide what the human approves. Allowlisted binary
	// files and deleted binaries are shown as a summary line; the hash stays
	// over the raw bytes (HashedText).
	home, err := gate.Home("")
	if err != nil {
		return fail(stderr, err)
	}
	ledgerPath := gate.LedgerPath(home, p.RepoID)
	// refuse records approval_refused FIRST (reason code only, never raw
	// bytes or paths), then explains; a ledger failure is a gate error too.
	refuse := func(msg, reason, why string) int {
		lerr := ledger.Append(ledgerPath, "approval_refused", p.RepoID, map[string]any{"base": p.Base, "reason": reason}, time.Now().UTC())
		fmt.Fprintf(stderr, "aigate: approve 거부: %s (이유 코드 %s). %s\n", msg, reason, why)
		if lerr != nil {
			fmt.Fprintf(stderr, "aigate: gate error: 거부 사건을 이력(ledger)에 기록하지 못했습니다: %v\n", lerr)
		}
		return exitGateError
	}
	if msg, reason := binaryRefusal(p); msg != "" {
		why := "바이너리 요약 한 줄로는 사람이 내용을 볼 수 없어 승인하지 않습니다(승인은 기록하지 않음). NUL 바이트를 없앤 커밋으로 다시 실행하세요."
		switch reason {
		case reasonBinaryBecameText:
			why = "승인하지 않습니다(승인은 기록하지 않음). 변경을 '이 파일 삭제 + 텍스트를 새 이름의 파일로 추가'로 나눈 커밋으로 다시 실행하세요: 삭제된 바이너리는 요약 한 줄로, 추가한 텍스트는 그대로 보여 검토할 수 있습니다."
		case reasonBinaryExecutable, reasonBinaryUnknown:
			why = "바이너리 요약 한 줄은 허용 목록의 진짜 바이너리 형식(실행 권한·'#!' 시작·'.bin' 경로가 없는 파일)에만 쓰고, 그 밖의 바이너리는 사람이 내용을 볼 수 없어 승인하지 않습니다(승인은 기록하지 않음). 직접 `git diff --text`로 검토하고 텍스트 파일로 두거나 허용 목록 형식의 바이너리로 바꾼 커밋으로 다시 실행하세요."
		}
		return refuse(msg, reason, why)
	}
	display := p.Diff.DisplayText(textsafe.Escape)
	if msg, reason := unsafeApproveText(p, display); msg != "" {
		return refuse(msg, reason, "터미널 제어·비정상 문자가 있는 diff는 화면에서 내용을 숨길 수 있어 승인하지 않습니다(승인은 기록하지 않음). 해당 문자를 없앤 커밋으로 다시 실행하세요.")
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
	st := approval.Store{Path: gate.ApprovalsPath(home)}
	if err := st.Grant(p.RepoID, p.Base, p.Diff.Hash, time.Now().UTC()); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "approved.")
	// The grant is done; the ledger event comes after it. If the append
	// fails, say plainly that the approval itself WAS recorded.
	if err := ledger.Append(ledgerPath, "approval_granted", p.RepoID, map[string]any{
		"base": p.Base, "hash": p.Diff.Hash, "files_changed": len(p.Diff.TouchedFiles),
	}, time.Now().UTC()); err != nil {
		fmt.Fprintf(stderr, "aigate: gate error: 승인은 %s 에 기록되었습니다(유효함). 그러나 이력(ledger)에 approval_granted 사건을 기록하지 못했습니다: %v\n", gate.ApprovalsPath(home), err)
		return exitGateError
	}
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
	reasonBinaryBecameText = "binary_became_text"
	// A file binary at head that is not an allowlisted genuine binary type
	// (binaryRefusal): executable (exec bit, "#!" start, ".bin" component),
	// source/config/text-named (the pre-allowlist rule, kept so old ledger
	// events keep their meaning), or any other type.
	reasonBinaryExecutable = "binary_executable"
	reasonBinarySource     = "binary_source_file"
	reasonBinaryUnknown    = "binary_unknown_type"
)

// binaryAllowExt are the extensions (lower case, with the dot) of genuine
// binary types that approve may show as a one-line summary. This is an
// ALLOWLIST: everything else binary at head is refused.
var binaryAllowExt = map[string]bool{}

// textLikeExt / textLikeName (lower case) only CLASSIFY a refusal as
// binary_source_file; they decide nothing (none of them is allowlisted).
var textLikeExt = map[string]bool{}
var textLikeName = map[string]bool{}

func init() {
	for _, e := range strings.Fields("png jpg jpeg gif webp ico icns bmp tif tiff heic avif svgz pdf zip gz tgz bz2 xz zst 7z tar woff woff2 ttf otf eot wasm node so dylib dll exe bin dat db sqlite sqlite3 mp3 mp4 m4a mov wav ogg flac webm psd class jar pb npy npz parquet") {
		binaryAllowExt["."+e] = true
	}
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

// hasBinDir reports whether a path component is ".bin" (node_modules/.bin,
// any **/.bin/**); a file named x.bin is not.
func hasBinDir(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, ".bin") {
			return true
		}
	}
	return false
}

// summaryRefusal returns "" when f (binary at head, not deleted) is an
// allowlisted genuine binary that may be summarised, else the reason code and
// a short Korean explanation of the rule it breaks.
func summaryRefusal(f gitx.FileChange) (reason, why string) {
	var exe []string
	if f.HeadMode != "100644" {
		exe = append(exe, "git 모드 "+f.HeadMode+"(일반 실행 불가 파일 100644 아님)")
	}
	if f.HeadShebang {
		exe = append(exe, "내용이 '#!'로 시작")
	}
	if hasBinDir(f.Path) {
		exe = append(exe, "'.bin' 경로 아래")
	}
	switch {
	case len(exe) > 0:
		return reasonBinaryExecutable, "실행될 수 있는 파일입니다(" + strings.Join(exe, ", ") + ")"
	case isTextLike(f.Path):
		return reasonBinarySource, "소스·설정·텍스트 파일 이름입니다"
	case !binaryAllowExt[strings.ToLower(path.Ext(path.Base(f.Path)))]:
		return reasonBinaryUnknown, "확장자가 요약을 허용하는 바이너리 형식 목록에 없습니다"
	}
	return "", ""
}

// binaryRefusal returns a description (escaped path, never raw bytes) and a
// reason code when a protected file must not be shown, checked before the
// generic byte scan: a text file that became binary (M/T), a binary file
// that became text (M/T: its NUL is on the base "-" side), or a file binary
// at head (added, modified or type-changed) that is not an allowlisted
// genuine binary (summaryRefusal). Deleted files are always a summary.
func binaryRefusal(p gate.Protected) (msg, reason string) {
	for _, f := range p.Diff.Files {
		if f.Status == "D" || f.Status == "A" && !f.HeadBinary {
			continue
		}
		if f.Status != "A" && f.BaseBinary && !f.HeadBinary {
			return fmt.Sprintf("보호 파일 '%s' 가 바이너리에서 텍스트로 바뀌었습니다(base는 바이너리, head는 텍스트). base의 NUL 바이트가 diff의 '-' 줄에 남아 이 diff는 화면에 그대로 보일 수 없습니다", textsafe.Escape(f.Path)), reasonBinaryBecameText
		}
		if !f.HeadBinary {
			continue
		}
		where := "NUL 바이트"
		if f.HeadNULLine > 0 {
			where = fmt.Sprintf("head %d번째 줄에 \\x00(NUL) 바이트", f.HeadNULLine)
		}
		if f.Status != "A" && !f.BaseBinary {
			return fmt.Sprintf("보호 파일 '%s' 가 텍스트에서 바이너리로 바뀌었습니다(base는 텍스트, %s)", textsafe.Escape(f.Path), where), reasonTextBecameBinary
		}
		if r, why := summaryRefusal(f); r != "" {
			return fmt.Sprintf("보호 파일 '%s' 는 head에서 바이너리(%s)인데 %s", textsafe.Escape(f.Path), where, why), r
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
	fd, bad := textsafe.FirstUnsafe(display)
	// A file whose last byte is a lone CR shows as "+x\r" + "\n\ No newline
	// at end of file": the "\n" is git's, so that CR is lone in the file.
	if k := strings.Index(display, "\r\n\\ "); k >= 0 && (!bad || k < fd.Offset) {
		fd, bad = textsafe.Finding{Offset: k, Line: strings.Count(display[:k], "\n") + 1, Desc: `\x0d`}, true
	}
	if bad {
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
