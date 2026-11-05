package remote

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aigate/internal/gate"
	"aigate/internal/rules"
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

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
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

type fixture struct {
	repo, sandbox string
	ex            *SSH
	cfg           Config
	argLog        string
}

// newFixture sets up a local repo, a sandbox standing in for the VM and an
// SSH executor that talks to the fake ssh script.
func newFixture(t *testing.T, tune ...func(*Config)) *fixture {
	t.Helper()
	fake, err := filepath.Abs("testdata/fakessh.sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIGATE_SSH_BIN", fake)
	t.Setenv("AIGATE_FAKE_MODE", "")
	sandbox := realpath(t, t.TempDir())
	f := &fixture{repo: realpath(t, t.TempDir()), sandbox: sandbox, argLog: filepath.Join(sandbox, "args.log")}
	t.Setenv("AIGATE_FAKE_ARGLOG", f.argLog)
	git(t, f.repo, "init", "-q", "-b", "main")
	write(t, sandbox, "known_hosts", "vm1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n")
	f.cfg = Config{
		Host: "vm1", User: "ci", KeyPath: filepath.Join(sandbox, "id"),
		KnownHosts: filepath.Join(sandbox, "known_hosts"),
		RepoDir:    filepath.Join(sandbox, "repo.git"), WorkDir: filepath.Join(sandbox, "work"),
		ConnectTimeout: 5 * time.Second, Grace: 10 * time.Second,
	}
	for _, fn := range tune {
		fn(&f.cfg)
	}
	f.ex, err = NewSSH(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.ex.Close() })
	return f
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// prepared commits one file and prepares the executor for that commit.
func (f *fixture) prepared(t *testing.T) string {
	t.Helper()
	write(t, f.repo, "f", "x")
	sha := commit(t, f.repo, "c1")
	if err := f.ex.Prepare(context.Background(), f.repo, []string{sha}); err != nil {
		t.Fatal(err)
	}
	return sha
}

func req(sha, adapter string, script string) gate.CheckRequest {
	return gate.CheckRequest{SHA: sha, CheckID: "c", Argv: []string{"sh", "-c", script}, Adapter: adapter, Timeout: 20 * time.Second}
}

const goodJUnit = `printf '<testsuite name="s"><testcase classname="c" name="ok"/><testcase classname="c" name="bad"><failure/></testcase></testsuite>' > "$AIGATE_JUNIT"`

func TestPrepareSendsBundle(t *testing.T) {
	f := newFixture(t)
	write(t, f.repo, "f", "1")
	c1 := commit(t, f.repo, "c1")
	write(t, f.repo, "f", "2")
	c2 := commit(t, f.repo, "c2")
	if err := f.ex.Prepare(context.Background(), f.repo, []string{c1, c2}); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{c1, c2} {
		if out, err := exec.Command("git", "-C", f.cfg.RepoDir, "cat-file", "-e", sha+"^{commit}").CombinedOutput(); err != nil {
			t.Fatalf("%s missing on the VM: %v %s", sha, err, out)
		}
	}
	if got := git(t, f.cfg.RepoDir, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatalf("VM repo is not bare: %s", got)
	}
	// the repo itself got no new refs from the transfer
	if refs := git(t, f.repo, "for-each-ref"); strings.Contains(refs, "aigate") {
		t.Fatalf("local repo refs touched: %s", refs)
	}
	// a second Prepare with a new commit is incremental and idempotent
	write(t, f.repo, "f", "3")
	c3 := commit(t, f.repo, "c3")
	for i := 0; i < 2; i++ {
		if err := f.ex.Prepare(context.Background(), f.repo, []string{c3}); err != nil {
			t.Fatal(err)
		}
	}
	git(t, f.cfg.RepoDir, "cat-file", "-e", c3+"^{commit}")
}

func TestPrepareUnknownSHAFails(t *testing.T) {
	f := newFixture(t)
	write(t, f.repo, "f", "1")
	commit(t, f.repo, "c1")
	if err := f.ex.Prepare(context.Background(), f.repo, []string{strings.Repeat("a", 40)}); err == nil {
		t.Fatal("want error for a commit the repo does not have")
	}
}

func TestRunCheckReturnsParsedInputs(t *testing.T) {
	f := newFixture(t)
	sha := f.prepared(t)
	ctx := context.Background()

	out, err := f.ex.RunCheck(ctx, req(sha, "junit", goodJUnit+`; echo tail-junit; test -f f; exit 1`))
	if err != nil {
		t.Fatal(err)
	}
	if out.RC != 1 || out.TimedOut || out.SetupFailed || strings.TrimSpace(out.Tail) != "tail-junit" || !strings.Contains(string(out.JUnit), `name="bad"`) || out.GoTestJSON != nil {
		t.Fatalf("junit: %+v", out)
	}

	gojson := `printf '%s\n' '{"Action":"run","Package":"p","Test":"TestA"}' '{"Action":"pass","Package":"p","Test":"TestA"}'; echo err >&2`
	out, err = f.ex.RunCheck(ctx, req(sha, "gotest-json", gojson))
	if err != nil {
		t.Fatal(err)
	}
	if out.RC != 0 || !strings.HasPrefix(string(out.GoTestJSON), `{"Action":"run"`) || strings.Contains(string(out.GoTestJSON), "err") || !strings.Contains(out.Tail, "err") || out.JUnit != nil {
		t.Fatalf("gotest-json: %+v", out)
	}

	out, err = f.ex.RunCheck(ctx, req(sha, "exit-code", `echo boom; exit 3`))
	if err != nil || out.RC != 3 || strings.TrimSpace(out.Tail) != "boom" || out.JUnit != nil || out.GoTestJSON != nil {
		t.Fatalf("exit-code: %+v %v", out, err)
	}

	// the check runs in a checkout of the commit, with only the allow-listed env
	r := req(sha, "exit-code", `test "$(cat f)" = x && test "$FOO" = bar && test -z "$AIGATE_FAKE_MODE_X"; echo "rc=$?"`)
	r.Env = []string{"FOO=bar"}
	out, err = f.ex.RunCheck(ctx, r)
	if err != nil || out.RC != 0 || strings.TrimSpace(out.Tail) != "rc=0" {
		t.Fatalf("env/checkout: %+v %v", out, err)
	}

	// setup runs first and a failing setup is reported as such
	r = req(sha, "exit-code", `exit 0`)
	r.Setup = []string{"sh", "-c", "echo nosetup; exit 4"}
	out, err = f.ex.RunCheck(ctx, r)
	if err != nil || !out.SetupFailed || out.RC != 4 || strings.TrimSpace(out.Tail) != "nosetup" || out.JUnit != nil {
		t.Fatalf("setup: %+v %v", out, err)
	}
	r = req(sha, "exit-code", `test -f marker`)
	r.Setup = []string{"touch", "marker"}
	if out, err = f.ex.RunCheck(ctx, r); err != nil || out.SetupFailed || out.RC != 0 {
		t.Fatalf("setup ok: %+v %v", out, err)
	}

	// a command that cannot start gives rc 127 and no result bytes
	out, err = f.ex.RunCheck(ctx, gate.CheckRequest{SHA: sha, CheckID: "n", Argv: []string{"/nonexistent/aigate-cmd"}, Adapter: "junit", Timeout: 5 * time.Second})
	if err != nil || out.RC != 127 || out.JUnit != nil {
		t.Fatalf("127: %+v %v", out, err)
	}

	// an empty report is empty and non-nil; a missing one is nil
	out, err = f.ex.RunCheck(ctx, req(sha, "junit", `: > "$AIGATE_JUNIT"`))
	if err != nil || out.JUnit == nil || len(out.JUnit) != 0 {
		t.Fatalf("empty: %#v %v", out.JUnit, err)
	}
	// $AIGATE_JUNIT in argv is substituted like the local runner does
	out, err = f.ex.RunCheck(ctx, gate.CheckRequest{SHA: sha, CheckID: "s", Argv: []string{"sh", "-c", `echo hi > "$0"`, "${AIGATE_JUNIT}"}, Adapter: "junit", Timeout: 5 * time.Second})
	if err != nil || string(out.JUnit) != "hi\n" {
		t.Fatalf("substitution: %q %v", out.JUnit, err)
	}
	// hostile characters in arguments are data, not shell syntax
	marker := filepath.Join(f.sandbox, "pwned")
	out, err = f.ex.RunCheck(ctx, gate.CheckRequest{SHA: sha, CheckID: "q", Argv: []string{"echo", "'; touch " + marker + "; echo '", "$(touch " + marker + ")"}, Adapter: "exit-code", Timeout: 5 * time.Second})
	if err != nil || !strings.Contains(out.Tail, "$(touch") {
		t.Fatalf("quoting: %+v %v", out, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("argument was interpreted by a shell")
	}
	// the temp area is cleaned up
	if left, _ := filepath.Glob(filepath.Join(f.cfg.WorkDir, "aigate-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	if wts := git(t, f.cfg.RepoDir, "worktree", "list"); strings.Count(wts, "\n") != 0 {
		t.Fatalf("worktrees left: %s", wts)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestTimeoutKillsRemote(t *testing.T) {
	f := newFixture(t)
	sha := f.prepared(t)
	pidFile := filepath.Join(f.sandbox, "pids")
	r := req(sha, "exit-code", `sleep 60 & echo $! > `+pidFile+`; echo $$ >> `+pidFile+`; wait`)
	r.Timeout = time.Second
	start := time.Now()
	out, err := f.ex.RunCheck(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut || out.RC != 124 || out.JUnit != nil {
		t.Fatalf("%+v", out)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("took %v", d)
	}
	b, _ := os.ReadFile(pidFile)
	pids := strings.Fields(string(b))
	if len(pids) != 2 {
		t.Fatalf("pid file %q", b)
	}
	time.Sleep(300 * time.Millisecond)
	for _, p := range pids {
		n, _ := strconv.Atoi(p)
		if alive(n) {
			t.Fatalf("remote process %d survived the timeout", n)
		}
	}
	// a timeout in setup is a setup failure
	r = req(sha, "exit-code", `true`)
	r.Setup = []string{"sleep", "60"}
	r.Timeout = time.Second
	out, err = f.ex.RunCheck(context.Background(), r)
	if err != nil || !out.SetupFailed || !out.TimedOut {
		t.Fatalf("setup timeout: %+v %v", out, err)
	}
}

func TestHostKeyUnknownFails(t *testing.T) {
	f := newFixture(t)
	args := strings.Join(f.ex.sshArgs(), " ")
	for _, want := range []string{"StrictHostKeyChecking=yes", "BatchMode=yes", "UserKnownHostsFile=" + f.cfg.KnownHosts, "IdentitiesOnly=yes", "-i " + f.cfg.KeyPath,
		"ForwardAgent=no", "ForwardX11=no", "ClearAllForwardings=yes", "ConnectTimeout=5"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args lack %q: %s", want, args)
		}
	}
	for _, bad := range []string{"StrictHostKeyChecking=no", "StrictHostKeyChecking=accept-new", "StrictHostKeyChecking=off", "ForwardAgent=yes", "ForwardX11=yes", "ForwardX11Trusted=yes", "-A", "-X", "-Y", "-L", "-R", "-D", "-g"} {
		for _, a := range f.ex.sshArgs() {
			if a == bad || strings.HasPrefix(bad, "-") == false && strings.Contains(a, bad) {
				t.Errorf("ssh args contain %q", bad)
			}
		}
	}
	if strings.Count(args, "UserKnownHostsFile") != 1 {
		t.Errorf("want one UserKnownHostsFile: %s", args)
	}

	// a host that is not in known_hosts fails the connection (the fake honours
	// StrictHostKeyChecking=yes like ssh); nothing runs and nothing is returned
	write(t, f.sandbox, "known_hosts", "other ssh-ed25519 AAAA\n")
	write(t, f.repo, "f", "x")
	sha := commit(t, f.repo, "c1")
	if err := f.ex.Prepare(context.Background(), f.repo, []string{sha}); err == nil || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Fatalf("Prepare err = %v", err)
	}
	if out, err := f.ex.RunCheck(context.Background(), req(sha, "exit-code", "true")); err == nil {
		t.Fatalf("RunCheck connected: %+v", out)
	}
	if _, err := os.Stat(f.cfg.RepoDir); err == nil {
		t.Fatal("remote command ran despite the host key failure")
	}
	// the fake would accept the bypass; prove that is the only difference
	log, _ := os.ReadFile(f.argLog)
	if strings.Contains(string(log), "StrictHostKeyChecking=no") {
		t.Fatalf("bypass option sent:\n%s", log)
	}
}

func TestNewSSHRejectsUnsafeConfig(t *testing.T) {
	good := Config{Host: "vm1", User: "ci", KeyPath: "/k", KnownHosts: "/kh", RepoDir: "/srv/repo.git"}
	if _, err := NewSSH(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"option host":      func(c *Config) { c.Host = "-oProxyCommand=x" },
		"space host":       func(c *Config) { c.Host = "a b" },
		"option user":      func(c *Config) { c.User = "-x" },
		"no known_hosts":   func(c *Config) { c.KnownHosts = "" },
		"no key":           func(c *Config) { c.KeyPath = "" },
		"relative repo":    func(c *Config) { c.RepoDir = "repo.git" },
		"space known_host": func(c *Config) { c.KnownHosts = "/a b" },
	} {
		c := good
		mut(&c)
		if _, err := NewSSH(c); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestForgedJUnitRejected(t *testing.T) {
	f := newFixture(t)
	sha := f.prepared(t)
	ctx := context.Background()
	// a report over the size limit is an error, never a verdict
	_, err := f.ex.RunCheck(ctx, req(sha, "junit", `head -c 9437184 /dev/zero > "$AIGATE_JUNIT"`))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v", err)
	}
	// a forged report that violates the schema is passed on as bytes and
	// rejected by the gate's parser (see TestVMLiesEndAsIncomplete)
	forged := `printf '<html><body>all tests passed</body></html>' > "$AIGATE_JUNIT"`
	out, err := f.ex.RunCheck(ctx, req(sha, "junit", forged))
	if err != nil || !strings.Contains(string(out.JUnit), "<html>") {
		t.Fatalf("%+v %v", out, err)
	}
	// declared sizes beyond the limit are refused before any data is read
	t.Setenv("AIGATE_FAKE_MODE", "bigframe")
	if _, err := f.ex.RunCheck(ctx, req(sha, "junit", "true")); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("bigframe err = %v", err)
	}
	// a frame that sends more than it declared is cut off, not buffered
	t.Setenv("AIGATE_FAKE_MODE", "lenlie")
	if _, err := f.ex.RunCheck(ctx, req(sha, "junit", "true")); err == nil {
		t.Fatal("lenlie accepted")
	}
}

func TestOversizedOutputRejected(t *testing.T) {
	f := newFixture(t)
	sha := f.prepared(t)
	ctx := context.Background()
	for adapter, script := range map[string]string{
		"junit":       `head -c 9437184 /dev/zero > "$AIGATE_JUNIT"`,
		"gotest-json": `head -c 9437184 /dev/zero`,
	} {
		out, err := f.ex.RunCheck(ctx, req(sha, adapter, script))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("%s: err = %v out.len = %d", adapter, err, len(out.JUnit)+len(out.GoTestJSON))
		}
		if out.JUnit != nil || out.GoTestJSON != nil {
			t.Fatalf("%s: truncated bytes returned", adapter)
		}
	}
	// exactly at the limit is fine
	out, err := f.ex.RunCheck(ctx, req(sha, "junit", `head -c 8388608 /dev/zero > "$AIGATE_JUNIT"`))
	if err != nil || len(out.JUnit) != 8<<20 {
		t.Fatalf("at limit: %d %v", len(out.JUnit), err)
	}
	// the transport itself never buffers more than the cap
	t.Setenv("AIGATE_FAKE_MODE", "flood")
	start := time.Now()
	if _, err := f.ex.RunCheck(ctx, req(sha, "exit-code", "true")); err == nil {
		t.Fatal("flood accepted")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("flood took %v", d)
	}
}

func TestTruncatedXMLRejected(t *testing.T) {
	f := newFixture(t)
	sha := f.prepared(t)
	ctx := context.Background()
	// truncated XML from the check is returned verbatim for the parser to refuse
	out, err := f.ex.RunCheck(ctx, req(sha, "junit", `printf '<testsuite name="s"><testcase classname="c" na' > "$AIGATE_JUNIT"`))
	if err != nil || string(out.JUnit) != `<testsuite name="s"><testcase classname="c" na` {
		t.Fatalf("%q %v", out.JUnit, err)
	}
	// a reply cut off in transit is an infrastructure error, never a result
	t.Setenv("AIGATE_FAKE_MODE", "cutframe")
	out, err = f.ex.RunCheck(ctx, req(sha, "junit", goodJUnit+`; echo `+strings.Repeat("x", 200)))
	if err == nil {
		t.Fatalf("truncated reply accepted: %+v", out)
	}
	t.Setenv("AIGATE_FAKE_MODE", "unreachable")
	if _, err := f.ex.RunCheck(ctx, req(sha, "exit-code", "true")); err == nil || !strings.Contains(err.Error(), "255") && !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestEndlessOutputBounded(t *testing.T) {
	f := newFixture(t)
	sha := f.prepared(t)
	// endless stderr (or stdout for exit-code) under a timeout; endless stdout
	// for gotest-json is over the result cap and an error instead
	for _, adapter := range []string{"exit-code", "gotest-json", "junit"} {
		r := req(sha, adapter, `while :; do echo endless-output-line-0123456789 >&2; done`)
		r.Timeout = 2 * time.Second
		start := time.Now()
		out, err := f.ex.RunCheck(context.Background(), r)
		if err != nil {
			t.Fatalf("%s: %v", adapter, err)
		}
		if !out.TimedOut || out.RC != 124 || len(out.Tail) > 1500 || !strings.Contains(out.Tail, "endless-output-line") {
			t.Fatalf("%s: timedout=%v rc=%d tail len %d", adapter, out.TimedOut, out.RC, len(out.Tail))
		}
		if out.JUnit != nil || out.GoTestJSON != nil {
			t.Fatalf("%s: result bytes after a timeout", adapter)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("%s took %v", adapter, d)
		}
	}
	if _, err := f.ex.RunCheck(context.Background(), req(sha, "gotest-json", `while :; do echo endless-stdout; done`)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("endless gotest-json stdout: %v", err)
	}
	// output that is not endless keeps only its last 1500 bytes
	out, err := f.ex.RunCheck(context.Background(), req(sha, "exit-code", `i=0; while [ $i -lt 2000 ]; do echo line-$i; i=$((i+1)); done`))
	if err != nil || len(out.Tail) > 1500 || !strings.HasSuffix(strings.TrimSpace(out.Tail), "line-1999") {
		t.Fatalf("tail len %d err %v", len(out.Tail), err)
	}
}

func TestRCLieDoesNotPass(t *testing.T) {
	f := newFixture(t)
	sha := f.prepared(t)
	// the VM says rc=0 but produced no result file
	t.Setenv("AIGATE_FAKE_MODE", "rc_lie")
	out, err := f.ex.RunCheck(context.Background(), req(sha, "junit", `exit 1`))
	if err != nil {
		t.Fatal(err)
	}
	if out.JUnit != nil || out.GoTestJSON != nil {
		t.Fatalf("result bytes from nowhere: %+v", out)
	}
	// and a check that honestly exits 0 without a report is also "no result"
	t.Setenv("AIGATE_FAKE_MODE", "")
	out, err = f.ex.RunCheck(context.Background(), req(sha, "junit", `exit 0`))
	if err != nil || out.RC != 0 || out.JUnit != nil {
		t.Fatalf("%+v %v", out, err)
	}
}

// TestVMLiesEndAsIncomplete runs the real gate against the SSH executor with a
// VM that lies or is hostile: no variant may end as ok.
func TestVMLiesEndAsIncomplete(t *testing.T) {
	cases := []struct {
		name, mode, cmd string
		wantErr         bool
	}{
		{"rc lie without report", "rc_lie", `exit 1`, false},
		{"no report", "", `exit 0`, false},
		{"forged schema", "", `printf '<html>ok</html>' > "$AIGATE_JUNIT"`, false},
		{"truncated xml", "", `printf '<testsuite><testcase classname="c" name="a"' > "$AIGATE_JUNIT"`, false},
		{"oversized report", "", `head -c 9437184 /dev/zero > "$AIGATE_JUNIT"`, true},
		{"cut reply", "cutframe", `exit 0`, true},
		{"unreachable", "unreachable", `exit 0`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			yaml := "version: 1\nprotected:\n  - \".aigate.yml\"\nchecks:\n  - id: u\n    adapter: junit\n    command: [\"sh\", \"-c\", " + strconv.Quote(tc.cmd) + "]\n    timeout: 20s\n    required: true\n"
			write(t, f.repo, ".aigate.yml", yaml)
			write(t, f.repo, "f", "1")
			base := commit(t, f.repo, "c1")
			write(t, f.repo, "f", "2")
			commit(t, f.repo, "c2")
			t.Setenv("AIGATE_FAKE_MODE", tc.mode)
			r, err := gate.Run(context.Background(), gate.Options{Repo: f.repo, Base: base, HomeDir: t.TempDir(), Executor: f.ex})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an infrastructure error, got report %s", r.Category)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Category != rules.CategoryIncomplete || r.ExitCode == 0 {
				t.Fatalf("category %s exit %d: %+v", r.Category, r.ExitCode, r.Violations)
			}
		})
	}
}

func TestGoodRunThroughGateIsOK(t *testing.T) {
	f := newFixture(t)
	yaml := "version: 1\nprotected:\n  - \".aigate.yml\"\nchecks:\n  - id: u\n    adapter: junit\n    command: [\"sh\", \"-c\", " +
		strconv.Quote(`printf '<testsuite><testcase classname="c" name="a"/></testsuite>' > "$AIGATE_JUNIT"`) + "]\n    timeout: 20s\n    required: true\n"
	write(t, f.repo, ".aigate.yml", yaml)
	write(t, f.repo, "f", "1")
	base := commit(t, f.repo, "c1")
	write(t, f.repo, "f", "2")
	commit(t, f.repo, "c2")
	r, err := gate.Run(context.Background(), gate.Options{Repo: f.repo, Base: base, HomeDir: t.TempDir(), Executor: f.ex})
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK {
		t.Fatalf("category %s: %+v", r.Category, r.Violations)
	}
}

// A request without a timeout still gets a remote deadline.
func TestNoTimeoutStillBounded(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.DefaultTimeout = time.Second })
	sha := f.prepared(t)
	r := req(sha, "exit-code", `sleep 60`)
	r.Timeout = 0
	start := time.Now()
	out, err := f.ex.RunCheck(context.Background(), r)
	if err != nil || !out.TimedOut || out.RC != 124 {
		t.Fatalf("%+v %v", out, err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("took %v", d)
	}
}

// Git environment variables of the caller (say, aigate started from a hook)
// must not redirect the bundle work into another repository.
func TestPrepareIgnoresHostileGitEnv(t *testing.T) {
	f := newFixture(t)
	write(t, f.repo, "f", "1")
	sha := commit(t, f.repo, "c1")
	other := realpath(t, t.TempDir())
	git(t, other, "init", "-q", "--bare")
	t.Setenv("GIT_DIR", other)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other, "objects"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	err := f.ex.Prepare(context.Background(), f.repo, []string{sha})
	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_OBJECT_DIRECTORY")
	if err != nil {
		t.Fatal(err)
	}
	if refs := git(t, other, "for-each-ref"); refs != "" {
		t.Fatalf("refs landed in the wrong repo: %s", refs)
	}
	git(t, f.cfg.RepoDir, "cat-file", "-e", sha+"^{commit}")
}

// Successive Prepare calls for different commits keep all of them runnable.
func TestSuccessivePreparesKeepEarlierCommits(t *testing.T) {
	f := newFixture(t)
	write(t, f.repo, "f", "1")
	c1 := commit(t, f.repo, "c1")
	if err := f.ex.Prepare(context.Background(), f.repo, []string{c1}); err != nil {
		t.Fatal(err)
	}
	// c2 is a sibling, so c1 is not in its history
	git(t, f.repo, "checkout", "-q", "-b", "other")
	write(t, f.repo, "g", "2")
	c2 := commit(t, f.repo, "c2")
	if err := f.ex.Prepare(context.Background(), f.repo, []string{c2}); err != nil {
		t.Fatal(err)
	}
	refs := git(t, f.cfg.RepoDir, "for-each-ref", "--format=%(refname)")
	for _, sha := range []string{c1, c2} {
		if !strings.Contains(refs, "refs/aigate/"+sha) {
			t.Fatalf("no ref for %s: %s", sha, refs)
		}
		if out, err := f.ex.RunCheck(context.Background(), req(sha, "exit-code", "true")); err != nil || out.RC != 0 {
			t.Fatalf("%s: %+v %v", sha, out, err)
		}
	}
}

func TestJumpHostBuildsHardenedProxy(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.JumpHost = "jump@orch1" })
	args := f.ex.sshArgs()
	var proxy string
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "ProxyCommand="); ok && args[i-1] == "-o" {
			if proxy != "" {
				t.Fatalf("two ProxyCommand options: %v", args)
			}
			proxy = v
		}
	}
	if proxy == "" {
		t.Fatalf("no ProxyCommand in %v", args)
	}
	words := strings.Fields(proxy)
	if words[0] != f.ex.bin {
		t.Fatalf("proxy runs %q, want the ssh binary %q", words[0], f.ex.bin)
	}
	have := strings.Join(words[1:], " ")
	for _, want := range []string{
		"-F /dev/null", "-o BatchMode=yes", "-o StrictHostKeyChecking=yes",
		"-o UserKnownHostsFile=" + f.cfg.KnownHosts, "-o GlobalKnownHostsFile=/dev/null",
		"-o IdentitiesOnly=yes", "-o IdentityAgent=none", "-i " + f.cfg.KeyPath,
		"-o ForwardAgent=no", "-l jump", "-W %h:%p",
	} {
		if !strings.Contains(have, want) {
			t.Errorf("proxy lacks %q: %s", want, proxy)
		}
	}
	if !strings.HasSuffix(proxy, " -W %h:%p orch1") {
		t.Errorf("proxy must end with -W %%h:%%p and the jump host: %s", proxy)
	}
	if strings.Contains(have, "StrictHostKeyChecking=no") || strings.Contains(have, "ProxyCommand") {
		t.Errorf("proxy relaxes or nests: %s", proxy)
	}
	// without a jump host there is no proxy at all
	for _, a := range newFixture(t).ex.sshArgs() {
		if strings.HasPrefix(a, "Proxy") {
			t.Errorf("unexpected proxy option %q", a)
		}
	}
	// the target still comes last, after the user
	if n := len(args); args[n-1] != "vm1" || args[n-2] != "ci" {
		t.Errorf("target not last: %v", args)
	}
}

func TestJumpHostRejectsOptionInjection(t *testing.T) {
	good := newFixture(t).cfg
	for _, j := range []string{
		"-oProxyCommand=x@y", "a b@c", "a@b c", "@host", "user@", "user", "user@-oProxyCommand=x",
		"a@b;touch x", "a@b$(id)", "a@b'c", "a@b\"c", "a@b%h", "a\n@b", "a@b\nc", "-u@host", "u@host:22 -v",
	} {
		c := good
		c.JumpHost = j
		if _, err := NewSSH(c); err == nil {
			t.Errorf("jump host %q accepted", j)
		}
	}
	c := good
	c.JumpHost = "ops@10.0.0.5"
	if _, err := NewSSH(c); err != nil {
		t.Errorf("valid jump host rejected: %v", err)
	}
	// a binary path that ssh's shell would split cannot carry a proxy
	c.SSHBin = "/tmp/my ssh"
	if _, err := NewSSH(c); err == nil {
		t.Error("ssh binary with a space accepted with a jump host")
	}
}
