package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, repo, name, content string) {
	t.Helper()
	p := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo, msg string) string {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", msg)
	return git(t, repo, "rev-parse", "HEAD")
}

const cfg = `version: 1
protected: ["tests/**"]
checks:
  - id: unit
    adapter: exit-code
    command: ["sh", "-c", "test -f ok.flag"]
    timeout: 20s
`

func newRepo(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("AIGATE_HOME", t.TempDir())
	d, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, d, "init", "-q", "-b", "main")
	write(t, d, ".aigate.yml", cfg)
	write(t, d, "ok.flag", "")
	write(t, d, "tests/t.txt", "one\ntwo\n")
	return d, commit(t, d, "base")
}

func run(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	c := Main(args, strings.NewReader(""), &o, &e)
	return c, o.String(), e.String()
}

func TestExitCodes(t *testing.T) {
	repo, base := newRepo(t)
	commit(t, repo, "head") // base != head: self-compare is a gate error (C2)
	if c, out, _ := run("run", "--repo", repo, "--base", base); c != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("ok: %d %s", c, out)
	}
	// behavior failure
	git(t, repo, "rm", "-q", "ok.flag")
	commit(t, repo, "break")
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 1 {
		t.Fatalf("behavior: %d %s", c, e)
	}
	// weakened: protected line changed (ok.flag restored)
	git(t, repo, "reset", "-q", "--hard", base)
	write(t, repo, "tests/t.txt", "one\nTWO\n")
	commit(t, repo, "weaken")
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 3 {
		t.Fatalf("weakened: %d %s", c, e)
	}
	// incomplete: dirty tree
	git(t, repo, "reset", "-q", "--hard", base)
	commit(t, repo, "head") // base != head
	write(t, repo, "junk", "x")
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 2 {
		t.Fatalf("incomplete: %d %s", c, e)
	}
}

func TestGateErrorIs64(t *testing.T) {
	repo, _ := newRepo(t)
	c, out, e := run("run", "--repo", repo, "--base", "no-such-ref")
	if c != 64 || out != "" || e == "" {
		t.Fatalf("%d %q %q", c, out, e)
	}
}

func TestUnknownAndMissing(t *testing.T) {
	if c, _, e := run("bogus"); c != 64 || !strings.Contains(e, "usage") {
		t.Fatalf("%d %s", c, e)
	}
	if c, _, _ := run(); c != 64 {
		t.Fatalf("no args: %d", c)
	}
	if c, _, _ := run("ledger"); c != 64 {
		t.Fatalf("ledger: %d", c)
	}
	if c, _, _ := run("run", "--help"); c != 0 {
		t.Fatalf("help: %d", c)
	}
}

func TestCheckFreshAndStale(t *testing.T) {
	repo, base := newRepo(t)
	git(t, repo, "checkout", "-q", "-b", "f")
	write(t, repo, "a", "1")
	head := commit(t, repo, "f1")
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("run: %d %s", c, e)
	}
	rp := filepath.Join(repo, ".aigate", "reports", head[:10]+".json")
	if c, out, e := run("check", rp, "--repo", repo, "--base", base); c != 0 || !strings.Contains(out, "fresh") {
		t.Fatalf("fresh: %d %s %s", c, out, e)
	}
	write(t, repo, "b", "2")
	commit(t, repo, "f2")
	if c, out, _ := run("check", rp, "--repo", repo, "--base", base); c != 1 || strings.Contains(out, "fresh") {
		t.Fatalf("stale: %d %s", c, out)
	}
	if c, _, _ := run("check", filepath.Join(repo, "missing.json"), "--repo", repo, "--base", base); c != 64 {
		t.Fatalf("missing: %d", c)
	}
}

func TestApproveRefusesWithoutTTY(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/t.txt", "one\nTWO\n")
	commit(t, repo, "weaken")
	requireTTY = func() error { return errors.New("not a tty") }
	c, _, e := run("approve", "--repo", repo, "--base", base)
	if c != 64 || !strings.Contains(e, "refused") {
		t.Fatalf("%d %s", c, e)
	}
	if c, _, _ := run("run", "--repo", repo, "--base", base); c != 3 {
		t.Fatalf("still weakened expected, got %d", c)
	}
}

func TestApprovePositivePath(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/t.txt", "one\nTWO\n")
	commit(t, repo, "weaken")
	orig := requireTTY
	requireTTY = func() error { return nil }
	t.Cleanup(func() { requireTTY = orig })

	var o, e bytes.Buffer
	if c := Main([]string{"approve", "--repo", repo, "--base", base}, strings.NewReader("no\n"), &o, &e); c != 64 {
		t.Fatalf("no: %d", c)
	}
	if c, _, _ := run("run", "--repo", repo, "--base", base); c != 3 {
		t.Fatal("must remain weakened after declined approval")
	}
	o.Reset()
	e.Reset()
	if c := Main([]string{"approve", "--repo", repo, "--base", base}, strings.NewReader("yes\n"), &o, &e); c != 0 {
		t.Fatalf("yes: %d %s %s", c, o.String(), e.String())
	}
	if !strings.Contains(o.String(), "hash: ") || !strings.Contains(o.String(), "tests/t.txt") {
		t.Fatalf("summary: %s", o.String())
	}
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("after approval: %d %s", c, e)
	}
	if c, out, _ := run("ledger", "verify", "--repo", repo); c != 0 || !strings.HasPrefix(out, "OK ") {
		t.Fatalf("ledger: %d %s", c, out)
	}
}

// Formerly TestKnownGapForgedReportPassesCheck (Task 12, self S34): `check`
// trusted the report file in the agent-writable repo. Since the final review
// (C2) check requires the ledger's run_result for (head, base) to be ok and
// to carry this file's sha256, so a hand-written report is rejected.
func TestForgedReportFailsCheck(t *testing.T) {
	repo, base := newRepo(t)
	git(t, repo, "checkout", "-q", "-b", "f")
	write(t, repo, "tests/t.txt", "one\n") // a protected removal: run would say check_weakened
	head := commit(t, repo, "f1")
	forged := fmt.Sprintf(`{"schema_version":1,"category":"ok","exit_code":0,"repo":%q,"base":%q,"head":%q,`+
		`"config_hash":"x","tool_version":"0.1.0","started_at":"","finished_at":"","checks":[],"violations":[],"waived":[],"approvals":[],"initial":false}`,
		repo, base, head)
	rp := filepath.Join(repo, ".aigate", "reports", head[:10]+".json")
	write(t, repo, ".aigate/reports/"+head[:10]+".json", forged)
	if c, out, e := run("check", rp, "--repo", repo, "--base", base); c != 1 || strings.Contains(out, "fresh") {
		t.Fatalf("forged report passes check: %d %s %s", c, out, e)
	}
	if c, _, _ := run("run", "--repo", repo, "--base", base); c != 3 {
		t.Fatalf("the real verdict should be check_weakened (3), got %d", c)
	}
	// the real (weakened) report edited to ok is rejected as well
	b, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(strings.Replace(string(b), `"category": "check_weakened",`, `"category": "ok",`, 1), `"exit_code": 3`, `"exit_code": 0`, 1)
	write(t, repo, ".aigate/reports/"+head[:10]+".json", edited)
	if c, out, _ := run("check", rp, "--repo", repo, "--base", base); c != 1 || strings.Contains(out, "fresh") {
		t.Fatalf("edited report passes check: %d %s", c, out)
	}
}

// C2(b): run needs an explicit --base, or --initial.
func TestRunNeedsBaseOrInitial(t *testing.T) {
	repo, _ := newRepo(t)
	if c, out, e := run("run", "--repo", repo); c != 64 || out != "" || !strings.Contains(e, "--base") {
		t.Fatalf("no base: %d %q %q", c, out, e)
	}
	if c, _, e := run("run", "--repo", repo, "--base", "HEAD", "--initial"); c != 64 || !strings.Contains(e, "--initial") {
		t.Fatalf("both: %d %q", c, e)
	}
	if c, out, e := run("run", "--repo", repo, "--base", "HEAD"); c != 64 || out != "" || !strings.Contains(e, "same commit") {
		t.Fatalf("self-compare: %d %q %q", c, out, e)
	}
	c, out, e := run("run", "--repo", repo, "--initial")
	if c != 0 || !strings.Contains(out, "초기") {
		t.Fatalf("initial: %d %q %q", c, out, e)
	}
}

// C2(c): check needs --base and rejects initial reports and a wrong base.
func TestCheckNeedsMatchingBase(t *testing.T) {
	repo, base := newRepo(t)
	head0 := git(t, repo, "rev-parse", "HEAD")
	if c, _, _ := run("run", "--repo", repo, "--initial"); c != 0 {
		t.Fatal("initial run failed")
	}
	rp0 := filepath.Join(repo, ".aigate", "reports", head0[:10]+".json")
	if c, out, _ := run("check", rp0, "--repo", repo, "--base", base); c != 1 || strings.Contains(out, "fresh") {
		t.Fatalf("initial report accepted: %d %s", c, out)
	}
	mid := commit(t, repo, "mid")
	head := commit(t, repo, "head")
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("run: %d %s", c, e)
	}
	rp := filepath.Join(repo, ".aigate", "reports", head[:10]+".json")
	if c, _, e := run("check", rp, "--repo", repo); c != 64 || !strings.Contains(e, "--base") {
		t.Fatalf("check without --base: %d %q", c, e)
	}
	if c, out, _ := run("check", rp, "--repo", repo, "--base", mid); c != 1 || strings.Contains(out, "fresh") {
		t.Fatalf("wrong base accepted: %d %s", c, out)
	}
	if c, out, e := run("check", rp, "--repo", repo, "--base", base); c != 0 || !strings.Contains(out, "fresh") {
		t.Fatalf("fresh: %d %s %s", c, out, e)
	}
}

// I1: approve prints the whole hashed text, so what the human reads is what
// the hash binds.
func TestApprovePrintsFullHashedDiff(t *testing.T) {
	repo, base := newRepo(t)
	var old, nu strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&old, "line %d\n", i)
		fmt.Fprintf(&nu, "forged %d\n", i)
	}
	write(t, repo, "tests/big.txt", old.String())
	write(t, repo, ".aigate.yml", cfg+"approval_on_add: [\"**/conftest.py\"]\n")
	base = commit(t, repo, "base2")
	write(t, repo, "tests/big.txt", nu.String())
	write(t, repo, "conftest.py", "def pytest_runtest_call(item):\n    pass  # swallow everything\n")
	commit(t, repo, "weaken")
	orig := requireTTY
	requireTTY = func() error { return nil }
	t.Cleanup(func() { requireTTY = orig })
	var o, e bytes.Buffer
	if c := Main([]string{"approve", "--repo", repo, "--base", base}, strings.NewReader("no\n"), &o, &e); c != 64 {
		t.Fatalf("no: %d %s", c, e.String())
	}
	out := o.String()
	i, j := strings.Index(out, beginMark), strings.Index(out, endMark)
	if i < 0 || j < i {
		t.Fatalf("no hashed block:\n%s", out)
	}
	block := out[i+len(beginMark) : j]
	for _, want := range []string{"-line 0\n", "-line 59\n", "+forged 0\n", "+forged 59\n", "+    pass  # swallow everything\n", "A conftest.py\n"} {
		if !strings.Contains(block, want) {
			t.Errorf("hashed block lacks %q", want)
		}
	}
	sum := sha256.Sum256([]byte(block))
	if !strings.Contains(out, "hash: "+hex.EncodeToString(sum[:])+"\n") {
		t.Fatalf("displayed text does not hash to the shown hash %s:\n%s", hex.EncodeToString(sum[:]), out)
	}
}

// I5: Ctrl-C during a run is a gate error (64) with a clear message and
// leaves no worktree behind.
func TestInterruptIsGateError(t *testing.T) {
	repo, base := newRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	started := filepath.Join(t.TempDir(), "started")
	write(t, repo, ".aigate.yml", "version: 1\nchecks:\n  - id: unit\n    adapter: exit-code\n    command: [\"sh\", \"-c\", \"touch "+started+"; sleep 30\"]\n    timeout: 60s\n")
	base = commit(t, repo, "slow")
	commit(t, repo, "head")
	go func() {
		for i := 0; i < 400; i++ {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	c, out, e := run("run", "--repo", repo, "--base", base)
	if c != 64 || out != "" || !strings.Contains(e, "interrupted") {
		t.Fatalf("interrupt: %d %q %q", c, out, e)
	}
	if m, _ := filepath.Glob(filepath.Join(tmp, "aigate-wt-*")); len(m) != 0 {
		t.Fatalf("worktree dirs left: %v", m)
	}
	if wl := git(t, repo, "worktree", "list", "--porcelain"); strings.Count(wl, "worktree ") != 1 {
		t.Fatalf("worktree entries left: %s", wl)
	}
}

// approveTTY runs approve with the TTY check injected as passing.
func approveTTY(t *testing.T, repo, base, answer string) (int, string, string) {
	t.Helper()
	orig := requireTTY
	requireTTY = func() error { return nil }
	t.Cleanup(func() { requireTTY = orig })
	var o, e bytes.Buffer
	c := Main([]string{"approve", "--repo", repo, "--base", base}, strings.NewReader(answer), &o, &e)
	return c, o.String(), e.String()
}

// rawUnsafe reports raw control / bidi bytes that must never reach the
// terminal.
func rawUnsafe(s string) bool {
	return strings.ContainsAny(s, "\x1b\r\x00\x7f\u202e\u0085\u009b") || !utf8.ValidString(s)
}

// Decision 2: approve refuses (64, nothing printed to stdout, nothing granted)
// when the text it would show contains terminal control or otherwise unsafe
// bytes; stderr names the location with escapes, never the raw bytes.
func TestApproveRefusesUnsafeDiffText(t *testing.T) {
	pad := strings.Repeat("x", 8100) + "\n" // keeps a NUL past git's 8000-byte binary window
	cases := []struct {
		name, content, wantDesc string
	}{
		{"ESC cursor-up/erase", "one\ntwo\nok = 1\x1b[1A\x1b[2K\n", `\x1b`},
		{"carriage return", "one\ntwo\nshown\rhidden\n", `\x0d`},
		{"NUL past binary window", "one\ntwo\n" + pad + "nul\x00here\n", `\x00`},
		{"invalid UTF-8", "one\ntwo\nbad \xff\xfe\n", `\xff`},
		{"bidi override", "one\ntwo\nx = \"\u202eabc\"\n", "U+202E"},
		{"C1 CSI", "one\ntwo\n\u009b2J\n", "U+009B"},
		{"DEL", "one\ntwo\nx\x7f\n", `\x7f`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, base := newRepo(t)
			write(t, repo, "tests/t.txt", c.content) // only lines added after "two": a pure addition
			commit(t, repo, "inject")
			code, out, errOut := approveTTY(t, repo, base, "yes\n")
			if code != 64 {
				t.Fatalf("want 64, got %d\nstdout=%q\nstderr=%q", code, out, errOut)
			}
			if out != "" {
				t.Fatalf("nothing may be printed to stdout before refusing: %q", out)
			}
			if rawUnsafe(errOut) {
				t.Fatalf("stderr echoes raw unsafe bytes: %q", errOut)
			}
			if !strings.Contains(errOut, c.wantDesc) || !strings.Contains(errOut, "줄") || !strings.Contains(errOut, "tests/t.txt") {
				t.Fatalf("stderr should name the line, the file and %s: %q", c.wantDesc, errOut)
			}
			if c, _, _ := run("run", "--repo", repo, "--base", base); c != 3 {
				t.Fatalf("refused approve must grant nothing: run exit %d", c)
			}
		})
	}
}

// A path from `git diff --raw -z` is printed unquoted, so a newline or ESC
// in a protected file name is refused as well.
func TestApproveRefusesUnsafePath(t *testing.T) {
	for _, name := range []string{"tests/a\nb.txt", "tests/e\x1b[2Kx.txt"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			repo, base := newRepo(t)
			p := filepath.Join(repo, name)
			probe := filepath.Join(t.TempDir(), filepath.Base(name))
			if err := os.WriteFile(probe, []byte("x\n"), 0o644); err != nil {
				t.Skipf("filesystem refuses the name: %v (covered by textsafe unit tests)", err)
			}
			write(t, repo, ".aigate.yml", cfg+"approval_on_add: [\"tests/**\"]\n")
			base = commit(t, repo, "base with guard")
			if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			commit(t, repo, "odd name")
			code, out, errOut := approveTTY(t, repo, base, "yes\n")
			if code != 64 || out != "" || rawUnsafe(errOut) || strings.Contains(errOut, name) {
				t.Fatalf("want refusal without raw output: %d stdout=%q stderr=%q", code, out, errOut)
			}
			if !strings.Contains(errOut, "경로") {
				t.Fatalf("stderr should say a path is unsafe: %q", errOut)
			}
			if c, _, _ := run("run", "--repo", repo, "--base", base); c != 3 {
				t.Fatalf("refused approve must grant nothing: run exit %d", c)
			}
		})
	}
}

// A binary protected file is shown as one summary line, never as raw bytes,
// and still contributes to the hash: approving it works.
func TestApproveSummarisesBinaryFile(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/golden.bin", "a\x00b\x1b[2K\n")
	base = commit(t, repo, "base bin")
	write(t, repo, "tests/golden.bin", "a\x00b\x1b[2K\nc\x00\r\x1b[1A\n")
	head := commit(t, repo, "change bin")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 {
		t.Fatalf("approve: %d %q %q", code, out, errOut)
	}
	if rawUnsafe(out) {
		t.Fatalf("raw binary bytes printed: %q", out)
	}
	if !strings.Contains(out, "binary file changed: tests/golden.bin, 16 bytes (base 8 bytes), head blob "+git(t, repo, "rev-parse", head+":tests/golden.bin")+"\n") {
		t.Fatalf("no binary summary: %q", out)
	}
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("binary approval did not take: %d %s", c, e)
	}
}

// A clean diff with Korean text and tabs is shown and approved as before.
func TestApproveCleanKoreanAndTabs(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/t.txt", "one\ntwo\n\t# 합계를 확인한다\t(탭)\n")
	commit(t, repo, "korean")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || !strings.Contains(out, "+\t# 합계를 확인한다\t(탭)\n") || !strings.Contains(out, "approved.") {
		t.Fatalf("clean approve: %d %q %q", code, out, errOut)
	}
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("after approval: %d %s", c, e)
	}
}

// ITEM 2 (fix wave 3): a submodule (gitlink, mode 160000) added under the
// built-in guarded vendor/** is a normal protected change: run says
// check_weakened (not a gate error) and approve shows the Subproject line.
func TestGitlinkUnderVendorIsApprovable(t *testing.T) {
	repo, base := newRepo(t)
	git(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("3", 40)+",vendor/sub")
	git(t, repo, "commit", "-q", "-m", "add submodule")
	// an unpopulated submodule is an empty directory (else status says deleted)
	if err := os.MkdirAll(filepath.Join(repo, "vendor", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c, out, e := run("run", "--repo", repo, "--base", base); c != 3 || !strings.Contains(out, "protected_modified") {
		t.Fatalf("run: %d %s %s", c, out, e)
	}
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || !strings.Contains(out, "+Subproject commit "+strings.Repeat("3", 40)) || strings.Contains(out, "binary file changed") {
		t.Fatalf("approve: %d %q %q", code, out, errOut)
	}
	if c, out, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("after approval: %d %s %s", c, out, e)
	}
}

// ITEM 1 (fix wave 3): one NUL byte must not turn a protected text or source
// file into an uninformative "binary file changed" line the human approves
// blind. approve refuses (64, nothing on stdout, nothing granted) and names
// the path (escaped) and the reason.
func TestApproveRefusesNULMasking(t *testing.T) {
	cases := []struct {
		name, path, baseContent, headContent, wantReason string
	}{
		{"text became binary (ts comment)", "tests/a.test.ts", "it('x', () => {})\n", "it('x', () => {})\n// \x00\n", "텍스트에서 바이너리"},
		{"text became binary (no source ext)", "tests/golden.dat", "row 1\n", "row 1\n\x00\n", "텍스트에서 바이너리"},
		{"added binary source file", "tests/b.test.TS", "", "\x00it('y')\n", "바이너리"},
		{"binary on both sides with source ext", "tests/fixture.json", "{\x00}\n", "{\x00 }\n", "바이너리"},
		{"added binary Makefile", "tests/Makefile", "", "all:\x00\n", "바이너리"},
		{"added binary dotfile", "tests/.npmrc", "", "x\x00\n", "바이너리"},
		{"added binary conftest", "tests/conftest", "", "x\x00\n", "바이너리"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, base := newRepo(t)
			if c.baseContent != "" {
				write(t, repo, c.path, c.baseContent)
				base = commit(t, repo, "base2")
			}
			write(t, repo, c.path, c.headContent)
			commit(t, repo, "nul")
			code, out, errOut := approveTTY(t, repo, base, "yes\n")
			if code != 64 || out != "" || rawUnsafe(errOut) {
				t.Fatalf("want refusal: %d stdout=%q stderr=%q", code, out, errOut)
			}
			if !strings.Contains(errOut, c.path) || !strings.Contains(errOut, c.wantReason) || strings.Contains(errOut, "binary file changed") {
				t.Fatalf("stderr should name %s and %q: %q", c.path, c.wantReason, errOut)
			}
			if c.baseContent != "" {
				if rc, _, _ := run("run", "--repo", repo, "--base", base); rc != 3 {
					t.Fatalf("refused approve must grant nothing: run exit %d", rc)
				}
			}
		})
	}
}

// Genuine binary fixtures (binary on every side that exists, no source
// extension) keep the summary line, now with the head blob id; an added one
// and a deleted one too.
func TestApproveBinaryFixtureSummaries(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/old.bin", "o\x00\n")
	base = commit(t, repo, "base2")
	write(t, repo, "tests/new.png", "\x89PNG\r\n\x1a\n\x00\x1b\n") // fix wave 5: real PNG magic
	if err := os.Remove(filepath.Join(repo, "tests/old.bin")); err != nil {
		t.Fatal(err)
	}
	head := commit(t, repo, "fixtures")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || rawUnsafe(out) {
		t.Fatalf("approve: %d %q %q", code, out, errOut)
	}
	for _, s := range []string{
		"binary file changed: tests/new.png, 11 bytes (base 0 bytes), head blob " + git(t, repo, "rev-parse", head+":tests/new.png") + "\n",
		"binary file changed: tests/old.bin, 0 bytes (base 3 bytes), head blob " + strings.Repeat("0", 40) + "\n",
		"(2 binary file(s)",
	} {
		if !strings.Contains(out, s) {
			t.Fatalf("missing %q in %q", s, out)
		}
	}
}

// Binary became text: no summary hides the head text. With -U0 the base's
// binary lines are "-" lines; since fix wave 4 (item B) a dedicated refusal
// (binary_became_text) fires before the generic byte scan (details in
// TestApproveBinaryBecameTextDedicatedRefusal).
func TestApproveBinaryBecameTextNotSummarised(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/data.bin", "a\x00b\n")
	base = commit(t, repo, "base2")
	write(t, repo, "tests/data.bin", "now text\n")
	commit(t, repo, "b2t")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 64 || out != "" || rawUnsafe(errOut) || strings.Contains(errOut, "binary file changed") || !strings.Contains(errOut, "binary_became_text") {
		t.Fatalf("binary→text: %d %q %q", code, out, errOut)
	}
}

// ITEM 3 (fix wave 3): a protected file with CRLF line endings can be
// approved; the CRLF bytes are printed as-is and the shown block hashes to
// the shown hash (hash over the raw bytes).
func TestApproveCRLFFile(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/win.txt", "one\r\ntwo\r\n")
	base = commit(t, repo, "crlf base")
	write(t, repo, "tests/win.txt", "one\r\ntwo\r\nthree\r\n")
	commit(t, repo, "crlf append")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || !strings.Contains(out, "approved.") {
		t.Fatalf("crlf approve: %d %q %q", code, out, errOut)
	}
	i, j := strings.Index(out, beginMark), strings.Index(out, endMark)
	if i < 0 || j < i {
		t.Fatalf("no hashed block:\n%q", out)
	}
	block := out[i+len(beginMark) : j]
	if !strings.Contains(block, "+three\r\n") {
		t.Fatalf("CRLF not printed as-is: %q", block)
	}
	sum := sha256.Sum256([]byte(block))
	if !strings.Contains(out, "hash: "+hex.EncodeToString(sum[:])+"\n") {
		t.Fatalf("block does not hash to the shown hash: %q", out)
	}
	if c, _, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("after approval: %d %s", c, e)
	}
}

// A lone CR is still refused: "\r\r\n" (first CR lone), "\n\r" (CR before
// another byte) and a CR as the very last byte of the file (git prints it
// before "\ No newline at end of file").
func TestApproveRefusesLoneCR(t *testing.T) {
	for name, content := range map[string]string{
		"cr cr lf":       "one\ntwo\nx\r\r\n",
		"lf cr":          "one\ntwo\nx\n\rhidden\n",
		"cr at file end": "one\ntwo\nend\r",
	} {
		t.Run(name, func(t *testing.T) {
			repo, base := newRepo(t)
			write(t, repo, "tests/t.txt", content)
			commit(t, repo, "lone cr")
			code, out, errOut := approveTTY(t, repo, base, "yes\n")
			if code != 64 || out != "" || rawUnsafe(errOut) || !strings.Contains(errOut, `\x0d`) || !strings.Contains(errOut, "tests/t.txt") {
				t.Fatalf("want refusal naming \\x0d: %d %q %q", code, out, errOut)
			}
		})
	}
}
