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
	if !strings.Contains(out, "hash: "+hex.EncodeToString(sum[:])[:16]+"\n") {
		t.Fatalf("displayed text does not hash to the shown hash %s:\n%s", hex.EncodeToString(sum[:])[:16], out)
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
