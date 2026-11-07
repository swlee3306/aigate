package vm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// holdAgent: records every start (pid, child pid) as a latch, then stays alive until $SANDBOX/release exists.
// The mode file decides: "hold" waits, anything else exits at once.
const holdAgent = `#!/bin/sh
sb=$(dirname "$0")/..
cat > /dev/null
if [ "$(cat "$sb/agent.mode")" = hold ]; then
  sleep 300 &
  echo "$! $$" >> "$sb/hold.starts"
  while [ ! -f "$sb/release" ]; do sleep 0.1; done
  echo released
else
  echo quick
fi
`

type wrapperProc struct {
	cmd  *exec.Cmd
	out  *lockedBuf
	done chan struct{}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type holdStart struct{ child, leader int }

// reapFixture is a fixture whose fake agents can be held and released.
func newReapFixture(t *testing.T) *fixture {
	f := newFixture(t, "")
	write(t, filepath.Join(f.sandbox, "bin", "claude"), holdAgent)
	write(t, filepath.Join(f.sandbox, "bin", "codex"), holdAgent)
	f.mode(t, "quick")
	t.Cleanup(func() {
		// last resort: nothing the test started may outlive it
		_ = os.WriteFile(filepath.Join(f.sandbox, "release"), nil, 0o644)
		for _, s := range f.starts() {
			_ = syscall.Kill(-s.leader, syscall.SIGKILL)
			_ = syscall.Kill(s.leader, syscall.SIGKILL)
			_ = syscall.Kill(s.child, syscall.SIGKILL)
		}
	})
	return f
}

func (f *fixture) starts() []holdStart {
	b, _ := os.ReadFile(filepath.Join(f.sandbox, "hold.starts"))
	var out []holdStart
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fs := strings.Fields(ln)
		if len(fs) == 2 {
			c, _ := strconv.Atoi(fs[0])
			l, _ := strconv.Atoi(fs[1])
			out = append(out, holdStart{child: c, leader: l})
		}
	}
	return out
}

func (f *fixture) waitStarts(t *testing.T, n int) []holdStart {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s := f.starts(); len(s) >= n {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent start #%d never happened", n)
	return nil
}

func (f *fixture) release() {
	_ = os.WriteFile(filepath.Join(f.sandbox, "release"), nil, 0o644)
}

// startWrapper starts the wrapper like sshd's forced command and reaps it in the background.
func (f *fixture) startWrapper(t *testing.T, cmd, stdin string) *wrapperProc {
	t.Helper()
	c := exec.Command(f.wrapper)
	c.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+cmd)
	c.Stdin = strings.NewReader(stdin)
	out := &lockedBuf{}
	c.Stdout, c.Stderr = out, out
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	w := &wrapperProc{cmd: c, out: out, done: make(chan struct{})}
	go func() { _ = c.Wait(); close(w.done) }()
	t.Cleanup(func() {
		select {
		case <-w.done:
		default:
			_ = c.Process.Kill()
			select {
			case <-w.done:
			case <-time.After(10 * time.Second):
			}
		}
	})
	return w
}

func (w *wrapperProc) waitDone(t *testing.T, what string) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not exit; output %q", what, w.out.String())
	}
}

func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	s := strings.TrimSpace(string(out))
	return s != "" && !strings.HasPrefix(s, "Z")
}

func waitDead(t *testing.T, what string, pid int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if !alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s (pid %d) is still alive", what, pid)
}

func (f *fixture) runPid(task string) string { return filepath.Join(f.root, task, "run.pid") }

func (f *fixture) runCmd(task string) string { return "run " + task + " claude" }

// foreignSleep starts an unrelated long-lived process in its own process group.
func foreignSleep(t *testing.T) int {
	t.Helper()
	c := exec.Command("sleep", "300")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() { _ = c.Process.Kill(); <-done })
	return c.Process.Pid
}

func psField(t *testing.T, field string, pid int) string {
	t.Helper()
	c := exec.Command("ps", "-o", field+"=", "-p", strconv.Itoa(pid))
	c.Env = append(os.Environ(), "LC_ALL=C") // the wrapper runs with LC_ALL=C; lstart is locale dependent
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// (a) + (f, while running): a second run of the same task kills the leftover run's whole process group first.
func TestReapSecondRunKillsLeftoverGroup(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	f.mode(t, "hold")
	w1 := f.startWrapper(t, f.runCmd(task), "p1")
	s1 := f.waitStarts(t, 1)[0]

	// run.pid names the leader, mode 600, written before the agent ran
	b, err := os.ReadFile(f.runPid(task))
	if err != nil {
		t.Fatal(err)
	}
	if first := strings.Fields(string(b))[0]; first != strconv.Itoa(s1.leader) {
		t.Fatalf("run.pid leader %q, agent pid %d", first, s1.leader)
	}
	if st, _ := os.Stat(f.runPid(task)); st.Mode().Perm() != 0o600 {
		t.Fatalf("run.pid mode %v", st.Mode().Perm())
	}
	if !alive(s1.child) {
		t.Fatal("child of the first agent should be alive")
	}

	f.mode(t, "quick")
	code, out := f.wrapperRun(f.runCmd(task), "p2")
	if code != 0 || !strings.Contains(out, "reaped previous agent run (pid "+strconv.Itoa(s1.leader)+")") || !strings.Contains(out, "AGENT-RUNNER-RUN1") {
		t.Fatalf("code %d out %q", code, out)
	}
	waitDead(t, "first agent", s1.leader)
	waitDead(t, "first agent's child", s1.child)
	w1.waitDone(t, "first wrapper")
	if _, err := os.Stat(f.runPid(task)); !os.IsNotExist(err) {
		t.Fatalf("run.pid should be gone after a normal run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, task, "run.lock")); !os.IsNotExist(err) {
		t.Fatalf("run.lock should be gone: %v", err)
	}
	if n := len(f.starts()); n != 1 {
		t.Fatalf("hold agent started %d times", n)
	}
}

// (b) a recycled or foreign pid in run.pid is never killed.
func TestReapNeverKillsForeignPid(t *testing.T) {
	cases := []struct {
		name string
		body func(t *testing.T, pid int) string
	}{
		{"start marker mismatch", func(t *testing.T, pid int) string {
			return strconv.Itoa(pid) + "\nMon Jan  1 00:00:00 2001\nclaude\n999999\nMon Jan  1 00:00:00 2001\nagent-runner\n"
		}},
		{"start marker right, command does not match", func(t *testing.T, pid int) string {
			return strconv.Itoa(pid) + "\n" + psField(t, "lstart", pid) + "\nclaude-not-this\n999999\nMon Jan  1 00:00:00 2001\n/x/not-this-wrapper\n"
		}},
		{"unreadable file", func(t *testing.T, pid int) string { return "garbage\n" }},
		{"pid 1", func(t *testing.T, pid int) string { return "1\nx\nclaude\n1\nx\n/x\n" }},
		{"pid not numeric", func(t *testing.T, pid int) string { return "$(touch /tmp/pwned)\nx\nclaude\n2\nx\n/x\n" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newReapFixture(t)
			f.prepared(t)
			victim := foreignSleep(t)
			write(t, f.runPid(task), c.body(t, victim))
			code, out := f.wrapperRun(f.runCmd(task), "p")
			if code != 0 || strings.Contains(out, "reaped") {
				t.Fatalf("code %d out %q", code, out)
			}
			if !alive(victim) {
				t.Fatal("foreign process was killed")
			}
			if _, err := os.Stat(f.runPid(task)); !os.IsNotExist(err) {
				t.Fatalf("run.pid should be removed: %v", err)
			}
		})
	}
}

// (c) stale run.pid with a dead pid is removed silently.
func TestReapStalePidFileRemovedSilently(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	write(t, f.runPid(task), strconv.Itoa(c.Process.Pid)+"\nMon Jan  1 00:00:00 2001\nclaude\n"+strconv.Itoa(c.Process.Pid)+"\nMon Jan  1 00:00:00 2001\nagent-runner\n")
	code, out := f.wrapperRun(f.runCmd(task), "p")
	if code != 0 || strings.Contains(out, "reaped") {
		t.Fatalf("code %d out %q", code, out)
	}
	if _, err := os.Stat(f.runPid(task)); !os.IsNotExist(err) {
		t.Fatal("stale run.pid not removed")
	}
}

// (d) prepare also reaps (and the fresh workspace is usable afterwards).
func TestReapPrepareKillsLeftoverRun(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	f.mode(t, "hold")
	w1 := f.startWrapper(t, f.runCmd(task), "p1")
	s1 := f.waitStarts(t, 1)[0]
	f.prepared(t)
	waitDead(t, "agent", s1.leader)
	waitDead(t, "agent child", s1.child)
	w1.waitDone(t, "old wrapper")
	if _, err := os.Stat(filepath.Join(f.ws(), ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.runPid(task)); !os.IsNotExist(err) {
		t.Fatal("run.pid survived prepare")
	}
}

// (e) tasks are independent.
func TestReapDifferentTasksDoNotAffectEachOther(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	if err := f.vm.Prepare(context.Background(), "8", f.repo, f.base); err != nil {
		t.Fatal(err)
	}
	f.mode(t, "hold")
	w8 := f.startWrapper(t, f.runCmd("8"), "p8")
	s8 := f.waitStarts(t, 1)[0]
	f.mode(t, "quick")
	if code, out := f.wrapperRun(f.runCmd(task), "p7"); code != 0 || strings.Contains(out, "reaped") {
		t.Fatalf("code %d out %q", code, out)
	}
	f.prepared(t) // prepare of task 7 as well
	if !alive(s8.leader) || !alive(s8.child) {
		t.Fatal("task 8's agent was disturbed by task 7")
	}
	if _, err := os.Stat(f.runPid("8")); err != nil {
		t.Fatalf("task 8's run.pid: %v", err)
	}
	f.release()
	w8.waitDone(t, "task 8 wrapper")
	if !strings.Contains(w8.out.String(), "rc=0") {
		t.Fatalf("task 8 output %q", w8.out.String())
	}
}

// (f) a normal run removes run.pid and the lock.
func TestReapNormalRunRemovesRunPid(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	if code, out := f.wrapperRun(f.runCmd(task), "p"); code != 0 || !strings.Contains(out, "rc=0") {
		t.Fatalf("code %d out %q", code, out)
	}
	for _, p := range []string{f.runPid(task), filepath.Join(f.root, task, "run.lock"), filepath.Join(f.root, task, "go")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}
}

// (g) orchestrator retry while the first run is alive and not stale: first reaped, second runs exactly once.
func TestReapConcurrentRetryRunsOnce(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	f.mode(t, "hold")
	w1 := f.startWrapper(t, f.runCmd(task), "p1")
	s1 := f.waitStarts(t, 1)[0]
	w2 := f.startWrapper(t, f.runCmd(task), "p2")
	all := f.waitStarts(t, 2)
	waitDead(t, "first agent", s1.leader)
	waitDead(t, "first agent child", s1.child)
	w1.waitDone(t, "first wrapper")
	s2 := all[1]
	if s2.leader == s1.leader || !alive(s2.leader) || !alive(s2.child) {
		t.Fatalf("second run not alive: %+v %+v", s1, s2)
	}
	b, _ := os.ReadFile(f.runPid(task))
	if first := strings.Fields(string(b))[0]; first != strconv.Itoa(s2.leader) {
		t.Fatalf("run.pid should name the second run: %q", b)
	}
	f.release()
	w2.waitDone(t, "second wrapper")
	if !strings.Contains(w2.out.String(), "rc=0") || !strings.Contains(w2.out.String(), "reaped previous agent run") {
		t.Fatalf("second output %q", w2.out.String())
	}
	if n := len(f.starts()); n != 2 {
		t.Fatalf("expected exactly 2 agent starts, got %d", n)
	}
	if _, err := os.Stat(f.runPid(task)); !os.IsNotExist(err) {
		t.Fatal("run.pid left behind")
	}
}

// (h) a live lock holder that is not reapable makes run refuse with exit code 5.
func TestReapRunRefusesWhenLockHeldByLiveProcess(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	holder := foreignSleep(t)
	lock := filepath.Join(f.root, task, "run.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(lock, "owner"), strconv.Itoa(holder)+"\n"+psField(t, "lstart", holder)+"\n")
	code, out := f.wrapperRun(f.runCmd(task), "p")
	if code != 5 || !strings.Contains(out, "still active") {
		t.Fatalf("code %d out %q", code, out)
	}
	if !alive(holder) {
		t.Fatal("lock holder must not be killed")
	}
	if n := len(f.starts()); n != 0 {
		t.Fatal("agent must not have started")
	}
}

// a stale lock (dead owner) does not block.
func TestReapStaleLockIsTakenOver(t *testing.T) {
	f := newReapFixture(t)
	f.prepared(t)
	c := exec.Command("true")
	_ = c.Run()
	lock := filepath.Join(f.root, task, "run.lock")
	_ = os.Mkdir(lock, 0o700)
	write(t, filepath.Join(lock, "owner"), strconv.Itoa(c.Process.Pid)+"\nMon Jan  1 00:00:00 2001\n")
	if code, out := f.wrapperRun(f.runCmd(task), "p"); code != 0 || !strings.Contains(out, "rc=0") {
		t.Fatalf("code %d out %q", code, out)
	}
}
