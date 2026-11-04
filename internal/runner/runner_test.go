package runner

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sh(script string) []string { return []string{"sh", "-c", script} }

func processGone(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("pid parse: %v", err)
	}
	return pid
}

func TestTimeoutKillsChildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	start := time.Now()
	out := Run(context.Background(), Spec{
		Dir:     dir,
		Argv:    sh("sleep 30 & echo $! > " + pidFile + "; wait"),
		Timeout: time.Second,
	})
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took too long: %v", el)
	}
	if !out.TimedOut || out.RC != 124 {
		t.Fatalf("want TimedOut RC=124, got %+v", out)
	}
	pid := readPid(t, pidFile)
	if !processGone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("grandchild %d still alive", pid)
	}
}

func TestTimeoutWithOutputHoldingGrandchild(t *testing.T) {
	// grandchild inherits stdout pipe; Run must still return promptly.
	out := Run(context.Background(), Spec{
		Argv:    sh("(sleep 30; echo late) & echo started; wait"),
		Timeout: 500 * time.Millisecond,
	})
	if !out.TimedOut || !strings.Contains(out.Tail, "started") {
		t.Fatalf("got %+v", out)
	}
}

func TestTailLimited(t *testing.T) {
	start := time.Now()
	out := Run(context.Background(), Spec{
		Argv:    sh("head -c 5000000 /dev/zero | tr '\\0' 'x'; echo END"),
		Timeout: 30 * time.Second,
	})
	if out.RC != 0 {
		t.Fatalf("rc=%d", out.RC)
	}
	if len(out.Tail) > 1500 {
		t.Fatalf("tail len %d", len(out.Tail))
	}
	if !strings.HasSuffix(strings.TrimSpace(out.Tail), "END") {
		t.Fatalf("tail should keep the end: %q", out.Tail[max(0, len(out.Tail)-20):])
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("too slow: %v", el)
	}
}

func TestTailCombinesStderr(t *testing.T) {
	out := Run(context.Background(), Spec{Argv: sh("echo out; echo err 1>&2; exit 3")})
	if out.RC != 3 || !strings.Contains(out.Tail, "out") || !strings.Contains(out.Tail, "err") {
		t.Fatalf("got %+v", out)
	}
}

func TestEnvPassed(t *testing.T) {
	out := Run(context.Background(), Spec{
		Argv: sh("printf %s \"$AIGATE_JUNIT\""),
		Env:  []string{"AIGATE_JUNIT=/tmp/x.xml"},
	})
	if out.RC != 0 || out.Tail != "/tmp/x.xml" {
		t.Fatalf("got %+v", out)
	}
}

func TestMissingBinary(t *testing.T) {
	out := Run(context.Background(), Spec{Argv: []string{"/nonexistent/definitely-not-here"}})
	if out.RC != 127 || out.TimedOut || out.Tail == "" {
		t.Fatalf("got %+v", out)
	}
	if !strings.Contains(out.Tail, "definitely-not-here") {
		t.Fatalf("tail lacks error text: %q", out.Tail)
	}
}

func TestEmptyArgv(t *testing.T) {
	out := Run(context.Background(), Spec{})
	if out.RC != 127 {
		t.Fatalf("got %+v", out)
	}
}

func TestStdoutFileGetsFullStdoutOnly(t *testing.T) {
	f := filepath.Join(t.TempDir(), "stdout.log")
	out := Run(context.Background(), Spec{
		Argv:       sh("seq 1 100000; echo ERRTEXT 1>&2"),
		StdoutFile: f,
		Timeout:    30 * time.Second,
	})
	if out.RC != 0 {
		t.Fatalf("rc=%d", out.RC)
	}
	if len(out.Tail) > 1500 {
		t.Fatalf("tail len %d", len(out.Tail))
	}
	fh, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	n := 0
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		n++
		if strings.Contains(sc.Text(), "ERRTEXT") {
			t.Fatal("stderr leaked into StdoutFile")
		}
	}
	if n != 100000 {
		t.Fatalf("lines=%d", n)
	}
}

func TestJunitSubstitution(t *testing.T) {
	out := Run(context.Background(), Spec{
		Argv: []string{"printf", "%s", "--x=${AIGATE_JUNIT}"},
		Env:  []string{"AIGATE_JUNIT=/tmp/j.xml"},
	})
	if out.Tail != "--x=/tmp/j.xml" {
		t.Fatalf("got %q", out.Tail)
	}
	out = Run(context.Background(), Spec{
		Argv: []string{"printf", "%s|%s", "JEST_JUNIT_OUTPUT_FILE=${AIGATE_JUNIT}", "$AIGATE_JUNIT"},
		Env:  []string{"AIGATE_JUNIT=/tmp/j.xml"},
	})
	if out.Tail != "JEST_JUNIT_OUTPUT_FILE=/tmp/j.xml|/tmp/j.xml" {
		t.Fatalf("got %q", out.Tail)
	}
}

func TestOtherDollarTextUntouched(t *testing.T) {
	out := Run(context.Background(), Spec{
		Argv: sh("echo $HOME"),
		Env:  []string{"AIGATE_JUNIT=/tmp/j.xml"},
	})
	if strings.TrimSpace(out.Tail) != os.Getenv("HOME") {
		t.Fatalf("shell should expand HOME itself: %q", out.Tail)
	}
	out = Run(context.Background(), Spec{
		Argv: []string{"printf", "%s|%s|%s", "${FOO}", "$AIGATE_JUNIT_OTHER", "${AIGATE_JUNIT}x"},
		Env:  []string{"AIGATE_JUNIT=/j"},
	})
	if out.Tail != "${FOO}|$AIGATE_JUNIT_OTHER|/jx" {
		t.Fatalf("got %q", out.Tail)
	}
}

func TestJunitUnsetLeftAsIs(t *testing.T) {
	out := Run(context.Background(), Spec{Argv: []string{"printf", "%s", "${AIGATE_JUNIT}"}})
	if out.Tail != "${AIGATE_JUNIT}" {
		t.Fatalf("got %q", out.Tail)
	}
}

func TestDirAndSeconds(t *testing.T) {
	d := t.TempDir()
	out := Run(context.Background(), Spec{Dir: d, Argv: []string{"pwd", "-P"}})
	want, _ := filepath.EvalSymlinks(d)
	if strings.TrimSpace(out.Tail) != want || out.Seconds < 0 {
		t.Fatalf("got %+v want %s", out, want)
	}
}

func TestContextCancelKillsGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	out := Run(ctx, Spec{Argv: sh("sleep 30 & wait"), Timeout: time.Minute})
	if time.Since(start) > 3*time.Second || out.RC == 0 || out.TimedOut {
		t.Fatalf("got %+v after %v", out, time.Since(start))
	}
}
