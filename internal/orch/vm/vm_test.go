package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	sandbox, root, conf, repo, argLog, wrapper string
	base                                       string
	vm                                         *AgentVM
}

const task = "7"

// fake claude/codex: behaviour comes from $SANDBOX/agent.mode.
const fakeAgent = `#!/bin/sh
sb=$(dirname "$0")/..
echo "$0 $*" >> "$sb/agent.args"
cat > "$sb/agent.stdin"
git config core.hooksPath >> "$sb/agent.hooks" 2>&1
case $(cat "$sb/agent.mode") in
commit) echo hello > made.txt; git add -A; git -c user.name=a -c user.email=a@b commit -q -m "agent work"; echo done ;;
commit5) for i in 1 2 3 4 5; do echo $i > f$i; git add -A; git -c user.name=a -c user.email=a@b commit -q -m "c$i"; done ;;
big) head -c 200000 /dev/urandom > big.bin; git add -A; git -c user.name=a -c user.email=a@b commit -q -m big ;;
leave) echo uncommitted > left.txt; echo "left it" ;;
none) echo "did nothing" ;;
noisy) i=0; while [ $i -lt 4000 ]; do echo "line $i xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"; i=$((i+1)); done; echo THE-END ;;
fail) echo failing; exit 3 ;;
sleep) echo $$ > "$sb/agent.pids"; sleep 60 & echo $! >> "$sb/agent.pids"; sleep 60 ;;
esac
`

func newFixture(t *testing.T, conf string) *fixture {
	t.Helper()
	sb, _ := filepath.EvalSymlinks(t.TempDir())
	f := &fixture{sandbox: sb, root: filepath.Join(sb, "work"), conf: filepath.Join(sb, "agent-runner.conf"), argLog: filepath.Join(sb, "args.log")}
	f.wrapper, _ = filepath.Abs("../../../infra/agent-runner/agent-runner")
	// git runs ssh with an allowlisted environment, so the fake carries its settings itself
	src, err := os.ReadFile("testdata/fakessh.sh")
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(sb, "fakessh.sh")
	hdr := fmt.Sprintf("#!/bin/sh\nAGENT_RUNNER=%s\nAGENT_RUNNER_CONF=%s\nAIGATE_FAKE_ARGLOG=%s\nexport AGENT_RUNNER AGENT_RUNNER_CONF AIGATE_FAKE_ARGLOG\n",
		f.wrapper, f.conf, f.argLog)
	write(t, fake, hdr+strings.TrimPrefix(string(src), "#!/bin/sh\n"))
	write(t, filepath.Join(sb, "bin", "claude"), fakeAgent)
	write(t, filepath.Join(sb, "bin", "codex"), fakeAgent)
	write(t, filepath.Join(sb, "agent.mode"), "commit")
	write(t, f.conf, fmt.Sprintf("ROOT=%s/work\nCLAUDE_BIN=%s/bin/claude\nCODEX_BIN=%s/bin/codex\n%s\n", sb, sb, sb, conf))
	write(t, filepath.Join(sb, "known_hosts"), "agent-vm ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n")
	t.Setenv("AGENT_RUNNER_CONF", f.conf)
	f.repo = filepath.Join(sb, "orch-repo")
	if err := os.MkdirAll(f.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, f.repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(f.repo, "a.txt"), "a")
	git(t, f.repo, "add", "-A")
	git(t, f.repo, "commit", "-q", "-m", "base")
	f.base = git(t, f.repo, "rev-parse", "HEAD")
	f.vm = &AgentVM{Host: "agent-vm", KeyPath: filepath.Join(sb, "id"), KnownHosts: filepath.Join(sb, "known_hosts"),
		SSHBin: fake, ConnectTimeout: 5 * time.Second, Grace: 10 * time.Second}
	return f
}

func (f *fixture) mode(t *testing.T, m string) {
	t.Helper()
	write(t, filepath.Join(f.sandbox, "agent.mode"), m)
}
func (f *fixture) ws() string { return filepath.Join(f.root, task, "repo") }

func (f *fixture) prepared(t *testing.T) {
	t.Helper()
	if err := f.vm.Prepare(context.Background(), task, f.repo, f.base); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) run(t *testing.T) RunResult {
	t.Helper()
	res, err := f.vm.Run(context.Background(), task, "claude", "do the thing", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f *fixture) logText(t *testing.T) string {
	b, _ := os.ReadFile(f.argLog)
	return string(b)
}

// wrapperRun runs the wrapper directly as sshd's forced command would.
func (f *fixture) wrapperRun(cmd string, stdin string) (int, string) {
	c := exec.Command(f.wrapper)
	c.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+cmd)
	c.Stdin = strings.NewReader(stdin)
	out, err := c.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return -1, err.Error()
	}
	return 0, string(out)
}

func TestWrapperRejectsUnknownSubcommand(t *testing.T) {
	f := newFixture(t, "")
	bad := []string{
		"", "bash", "sh -c id", "id", "prepare", "prepare abc", "prepare 7 extra", "prepare 7; id", "prepare $(id)", "prepare 7\nid",
		"prepare 1234567890123456789", "prepare -7", "prepare ../7", "run 7", "run 7 evil", "run 7 claude extra", "run 7 claude; id", "run 7 CLAUDE",
		"run abc claude", "collect-info", "collect-info 7 8", "collect-info 7;id", "collect", "cat /etc/passwd", "git-upload-pack '/srv/aigate/work/7/repo'",
		"upload-pack", "upload-pack /7", "upload-pack '/etc'", "upload-pack '/7/../8'", "upload-pack '/7/repo'", "upload-pack '/7' --foo",
		"upload-pack '../7'", "upload-pack '/srv/aigate/work/7/repo'", "upload-pack '/7';id", "upload-pack \"/7\"", "prepare 7\tx",
	}
	for _, c := range bad {
		rc, out := f.wrapperRun(c, "stdin data")
		if rc == 0 {
			t.Errorf("%q was accepted: %s", c, out)
		}
	}
	if _, err := os.Stat(f.root); err == nil {
		t.Fatalf("a rejected command had a side effect: %s exists", f.root)
	}
	if rc, _ := f.wrapperRun("collect-info 7", ""); rc == 0 {
		t.Fatal("collect-info of a task without a workspace must fail")
	}
}

func TestPrepareResetsWorkspace(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	if got := git(t, f.ws(), "rev-parse", "HEAD"); got != f.base {
		t.Fatalf("workspace HEAD %s, want %s", got, f.base)
	}
	if b := git(t, f.ws(), "symbolic-ref", "--short", "HEAD"); b != "task-7" {
		t.Fatalf("branch %q", b)
	}
	if h := git(t, f.ws(), "config", "core.hooksPath"); h != "/dev/null" {
		t.Fatalf("hooksPath %q", h)
	}
	// the agent leaves a mess: file, other branch, tag, new commit
	write(t, filepath.Join(f.ws(), "junk.txt"), "junk")
	git(t, f.ws(), "branch", "junk")
	git(t, f.ws(), "tag", "t1")
	git(t, f.ws(), "add", "-A")
	git(t, f.ws(), "commit", "-q", "-m", "junk")
	write(t, filepath.Join(f.repo, "b.txt"), "b")
	git(t, f.repo, "add", "-A")
	git(t, f.repo, "commit", "-q", "-m", "second")
	base2 := git(t, f.repo, "rev-parse", "HEAD")
	if err := f.vm.Prepare(context.Background(), task, f.repo, base2); err != nil {
		t.Fatal(err)
	}
	if got := git(t, f.ws(), "rev-parse", "HEAD"); got != base2 {
		t.Fatalf("after reset HEAD %s, want %s", got, base2)
	}
	if _, err := os.Stat(filepath.Join(f.ws(), "junk.txt")); err == nil {
		t.Fatal("junk.txt survived the reset")
	}
	if refs := git(t, f.ws(), "for-each-ref", "--format=%(refname)"); refs != "refs/heads/task-"+task {
		t.Fatalf("refs after reset: %q", refs)
	}
	if _, err := os.Stat(filepath.Join(f.root, task, "in.bundle")); err == nil {
		t.Fatal("bundle left behind")
	}
}

func TestPrepareRejectsOversizeBundle(t *testing.T) {
	f := newFixture(t, "PREP_MAX=200")
	if err := f.vm.Prepare(context.Background(), task, f.repo, f.base); err == nil {
		t.Fatal("oversize bundle accepted")
	}
	if _, err := os.Stat(filepath.Join(f.root, task)); err == nil {
		t.Fatal("workspace left after rejected bundle")
	}
}

func TestPrepareRejectsBadBundleRefs(t *testing.T) {
	f := newFixture(t, "")
	bundle := filepath.Join(f.sandbox, "x.bundle")
	git(t, f.repo, "bundle", "create", "-q", bundle, "main")
	c := exec.Command(f.wrapper)
	c.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND=prepare 7")
	in, _ := os.Open(bundle)
	c.Stdin = in
	if out, err := c.CombinedOutput(); err == nil {
		t.Fatalf("bundle with refs/heads/main accepted: %s", out)
	}
}

func TestRunPassesPromptAndFixedArgs(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	f.mode(t, "none")
	res := f.run(t)
	if res.RC != 0 || res.TimedOut || res.Committed || !strings.Contains(res.Tail, "did nothing") {
		t.Fatalf("%+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(f.sandbox, "agent.args"))
	if !strings.Contains(string(args), "-p --permission-mode acceptEdits --allowedTools Bash,Edit,Write,Read,Glob,Grep") {
		t.Fatalf("claude args: %s", args)
	}
	if in, _ := os.ReadFile(filepath.Join(f.sandbox, "agent.stdin")); string(in) != "do the thing" {
		t.Fatalf("prompt on stdin: %q", in)
	}
	if h, _ := os.ReadFile(filepath.Join(f.sandbox, "agent.hooks")); strings.TrimSpace(string(h)) != "/dev/null" {
		t.Fatalf("agent sees hooksPath %q", h)
	}
	// codex
	if _, err := f.vm.Run(context.Background(), task, "codex", "p2", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(filepath.Join(f.sandbox, "agent.args"))
	if !strings.Contains(string(args), "exec --skip-git-repo-check --sandbox workspace-write -") {
		t.Fatalf("codex args: %s", args)
	}
	// the caller cannot pick anything else
	if _, err := f.vm.Run(context.Background(), task, "bash", "x", time.Second); err == nil {
		t.Fatal("unknown agent accepted")
	}
	if _, err := f.vm.Run(context.Background(), task, "claude", strings.Repeat("x", PromptMaxBytes+1), time.Second); err == nil {
		t.Fatal("oversize prompt accepted")
	}
}

func TestRunCommitsLeftoverChanges(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	f.mode(t, "leave")
	res := f.run(t)
	if !res.Committed {
		t.Fatalf("wrapper did not commit leftovers: %+v", res)
	}
	if n := git(t, f.ws(), "rev-list", "--count", f.base+"..HEAD"); n != "1" {
		t.Fatalf("commits %s", n)
	}
	if a := git(t, f.ws(), "log", "-1", "--format=%an <%ae>"); a != "agent-runner <agent-runner@localhost>" {
		t.Fatalf("identity %q", a)
	}
	if s := git(t, f.ws(), "status", "--porcelain"); s != "" {
		t.Fatalf("dirty after final commit: %q", s)
	}
}

func TestRunReportsExitCodeAndTailOnly(t *testing.T) {
	f := newFixture(t, "TAIL_MAX=1000")
	f.prepared(t)
	f.mode(t, "noisy")
	res := f.run(t)
	if len(res.Tail) != 1000 || !strings.HasSuffix(strings.TrimSpace(res.Tail), "THE-END") {
		t.Fatalf("tail %d bytes, ends %q", len(res.Tail), res.Tail[max(0, len(res.Tail)-20):])
	}
	f.mode(t, "fail")
	if res := f.run(t); res.RC != 3 || res.TimedOut {
		t.Fatalf("%+v", res)
	}
}

func TestRunTimeoutKillsAgent(t *testing.T) {
	f := newFixture(t, "RUN_TIMEOUT=1")
	f.prepared(t)
	f.mode(t, "sleep")
	start := time.Now()
	res := f.run(t)
	if !res.TimedOut || res.RC != 124 || res.Committed {
		t.Fatalf("%+v", res)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	pids, _ := os.ReadFile(filepath.Join(f.sandbox, "agent.pids"))
	fields := strings.Fields(string(pids))
	if len(fields) != 2 {
		t.Fatalf("pids %q", pids)
	}
	for _, p := range fields {
		n, _ := strconv.Atoi(p)
		time.Sleep(50 * time.Millisecond)
		if err := syscall.Kill(n, 0); err == nil {
			t.Errorf("process %d is still alive after the timeout", n)
		}
	}
}

func TestCollectBringsOnlyTheTaskBranch(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	res := f.run(t)
	_ = res
	head, n, err := f.vm.Collect(context.Background(), task, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || head != git(t, f.ws(), "rev-parse", "HEAD") {
		t.Fatalf("head %s n %d", head, n)
	}
	if got := git(t, f.repo, "rev-parse", "refs/orch/7/head"); got != head {
		t.Fatalf("ref %s", got)
	}
	if refs := git(t, f.repo, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main\nrefs/orch/7/head" {
		t.Fatalf("refs in the orchestrator repo: %q", refs)
	}
	log := f.logText(t)
	for _, want := range []string{"StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "ForwardAgent=no", "BatchMode=yes", "ClearAllForwardings=yes", "upload-pack '/7'"} {
		if !strings.Contains(log, want) {
			t.Errorf("ssh arguments lack %q:\n%s", want, log)
		}
	}
}

func TestCollectRejectsNoCommit(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	f.mode(t, "none")
	f.run(t)
	if _, _, err := f.vm.Collect(context.Background(), task, f.repo); !errors.Is(err, ErrNoCommit) {
		t.Fatalf("err = %v, want ErrNoCommit", err)
	}
	if strings.Contains(f.logText(t), "upload-pack") {
		t.Fatal("fetched although there is nothing to collect")
	}
}

func TestCollectRejectsHugeHistory(t *testing.T) {
	f := newFixture(t, "")
	f.vm.MaxCommits = 3
	f.prepared(t)
	f.mode(t, "commit5")
	f.run(t)
	_, _, err := f.vm.Collect(context.Background(), task, f.repo)
	if !errors.Is(err, ErrOverLimit) || !strings.Contains(err.Error(), "commits") {
		t.Fatalf("err = %v, want ErrOverLimit (commits)", err)
	}
	if strings.Contains(f.logText(t), "upload-pack") {
		t.Fatal("fetched before rejecting")
	}
	if out := git(t, f.repo, "for-each-ref", "refs/orch"); out != "" {
		t.Fatalf("refs written: %q", out)
	}
}

func TestCollectRejectsHugeObjects(t *testing.T) {
	f := newFixture(t, "")
	f.vm.MaxBytes = 50_000
	f.prepared(t)
	f.mode(t, "big")
	f.run(t)
	_, _, err := f.vm.Collect(context.Background(), task, f.repo)
	if !errors.Is(err, ErrOverLimit) || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("err = %v, want ErrOverLimit (bytes)", err)
	}
	if strings.Contains(f.logText(t), "upload-pack") {
		t.Fatal("fetched before rejecting")
	}
}

func TestCollectRejectsExtraRefs(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	f.run(t)
	// thousands of other branches and tags in the agent's repo, one of them with its own commit
	other := git(t, f.ws(), "commit-tree", "-m", "other", "HEAD^{tree}")
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&sb, "create refs/heads/extra-%d %s\n", i, f.base)
		fmt.Fprintf(&sb, "create refs/tags/v%d %s\n", i, f.base)
	}
	fmt.Fprintf(&sb, "create refs/heads/other %s\ncreate refs/tags/other %s\n", other, other)
	c := exec.Command("git", "-C", f.ws(), "update-ref", "--stdin")
	c.Stdin = strings.NewReader(sb.String())
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	head, _, err := f.vm.Collect(context.Background(), task, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if refs := git(t, f.repo, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main\nrefs/orch/7/head" {
		t.Fatalf("extra refs came along: %.200q", refs)
	}
	if err := exec.Command("git", "-C", f.repo, "cat-file", "-e", other).Run(); err == nil {
		t.Fatal("the commit of another branch was fetched")
	}
	if got := git(t, f.repo, "rev-parse", "refs/orch/7/head"); got != head {
		t.Fatal("wrong head")
	}
}

func TestWrapperUploadPackServesOnlyThatTaskRepo(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	// a second task and a lookalike repo elsewhere must not be reachable through task 7's command
	if err := f.vm.Prepare(context.Background(), "8", f.repo, f.base); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"upload-pack '/7/../8'", "upload-pack '/7/repo'", "upload-pack '/srv/x'", "upload-pack '" + f.root + "/8/repo/.git'"} {
		if rc, _ := f.wrapperRun(c, ""); rc == 0 {
			t.Errorf("%q accepted", c)
		}
	}
	// advertisement of the legit one lists the task branch (and HEAD, which is it), nothing else
	git(t, f.ws(), "branch", "junk")
	git(t, f.ws(), "tag", "t1")
	c := exec.Command(f.wrapper)
	c.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND=upload-pack '/7'")
	c.Stdin = strings.NewReader("0000")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if !strings.Contains(string(out), "refs/heads/task-7") || strings.Contains(string(out), "refs/tags") || strings.Contains(string(out), "junk") {
		t.Fatalf("advertised refs: %q", out)
	}
}

func TestCollectNeedsPinnedHostKey(t *testing.T) {
	f := newFixture(t, "")
	f.prepared(t)
	f.run(t)
	f.vm.Host = "other-vm"
	if _, _, err := f.vm.Collect(context.Background(), task, f.repo); err == nil {
		t.Fatal("collected from a host that is not in known_hosts")
	}
	if out := git(t, f.repo, "for-each-ref", "refs/orch"); out != "" {
		t.Fatalf("refs written: %q", out)
	}
}

func TestParseFramesAreStrict(t *testing.T) {
	good := "AGENT-RUNNER-RUN1\nrc=0\ntimedout=0\ncommitted=1\ntail=2\nhi\nEND\n"
	if _, err := parseRun([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"", good + "x", strings.Replace(good, "tail=2", "tail=99999", 1), strings.Replace(good, "rc=0", "rc=256", 1),
		strings.Replace(good, "hi\n", "h\n", 1), strings.TrimSuffix(good, "END\n")} {
		if _, err := parseRun([]byte(b)); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
	sha := strings.Repeat("a", 40)
	gi := "AGENT-RUNNER-INFO1\nhead=" + sha + "\nbase=" + sha + "\ncommits=1\nbytes=5\nEND\n"
	if _, err := parseInfo([]byte(gi)); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"", gi + "x", strings.Replace(gi, "commits=1", "commits=-1", 1), strings.Replace(gi, sha, "zz", 1), strings.Replace(gi, "bytes=5", "bytes=05", 1)} {
		if _, err := parseInfo([]byte(b)); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
}
