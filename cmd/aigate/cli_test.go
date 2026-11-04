package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	if c, out, e := run("check", rp, "--repo", repo); c != 0 || !strings.Contains(out, "fresh") {
		t.Fatalf("fresh: %d %s %s", c, out, e)
	}
	write(t, repo, "b", "2")
	commit(t, repo, "f2")
	if c, out, _ := run("check", rp, "--repo", repo); c != 1 || strings.Contains(out, "fresh") {
		t.Fatalf("stale: %d %s", c, out)
	}
	if c, _, _ := run("check", filepath.Join(repo, "missing.json"), "--repo", repo); c != 64 {
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
