// Package runner executes one check command with a timeout, killing the whole
// process group on expiry and keeping only a bounded tail of the output.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tailMax   = 1500
	waitDelay = 2 * time.Second
)

// Spec describes one command invocation.
type Spec struct {
	Dir     string
	Argv    []string
	Env     []string // extra KEY=VALUE entries appended to the inherited environment
	Timeout time.Duration
	// StdoutFile, when non-empty, receives the child's full stdout (not stderr).
	StdoutFile string
}

// Out is the result of a run. RC is 124 on timeout, 127 if the command could
// not be started, 128+signal if killed by a signal.
type Out struct {
	RC       int
	TimedOut bool
	Seconds  float64
	Tail     string // last <=1500 bytes of combined stdout+stderr
}

// tailBuf is a goroutine-safe ring buffer keeping the last tailMax bytes.
type tailBuf struct {
	mu   sync.Mutex
	buf  [tailMax]byte
	pos  int // next write index
	full bool
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) >= tailMax {
		copy(t.buf[:], p[len(p)-tailMax:])
		t.pos, t.full = 0, true
		return n, nil
	}
	for len(p) > 0 {
		c := copy(t.buf[t.pos:], p)
		p = p[c:]
		t.pos += c
		if t.pos == tailMax {
			t.pos, t.full = 0, true
		}
	}
	return n, nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.full {
		return string(t.buf[:t.pos])
	}
	return string(t.buf[t.pos:]) + string(t.buf[:t.pos])
}

var junitRef = regexp.MustCompile(`\$\{AIGATE_JUNIT\}|\$AIGATE_JUNIT\b`)

func substituteJunit(argv, env []string) []string {
	val, ok := "", false
	for _, e := range env {
		if v, found := strings.CutPrefix(e, "AIGATE_JUNIT="); found {
			val, ok = v, true
		}
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		if ok {
			a = junitRef.ReplaceAllLiteralString(a, val)
		}
		out[i] = a
	}
	return out
}

func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

// Run executes s. It never panics and never blocks past Timeout (+ a short
// grace period for pipe draining).
func Run(ctx context.Context, s Spec) Out {
	start := time.Now()
	finish := func(o Out) Out { o.Seconds = time.Since(start).Seconds(); return o }
	fail := func(msg string) Out { return finish(Out{RC: 127, Tail: msg}) }

	if len(s.Argv) == 0 {
		return fail("runner: empty argv")
	}
	argv := substituteJunit(s.Argv, s.Env)

	rctx := ctx
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		killGroup(cmd.Process.Pid)
		return nil
	}
	cmd.WaitDelay = waitDelay

	tail := &tailBuf{}
	if s.StdoutFile != "" {
		f, err := os.Create(s.StdoutFile)
		if err != nil {
			return fail("runner: " + err.Error())
		}
		defer f.Close()
		cmd.Stdout = io.MultiWriter(f, tail)
		cmd.Stderr = tail
	} else {
		cmd.Stdout = tail
		cmd.Stderr = tail
	}

	if err := cmd.Start(); err != nil {
		return fail("runner: " + err.Error())
	}
	pid := cmd.Process.Pid
	err := cmd.Wait()
	killGroup(pid) // reap stragglers (background grandchildren) on every exit path

	timedOut := s.Timeout > 0 && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	o := Out{Tail: tail.String()}
	switch {
	case timedOut:
		o.RC, o.TimedOut = 124, true
	case ctx.Err() != nil:
		o.RC = 130
	case err == nil:
		o.RC = 0
	default:
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			o.RC = exitCode(ee.ProcessState)
		case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
			// The process itself exited; only a background grandchild kept
			// the pipes open. Report the real exit status.
			o.RC = exitCode(cmd.ProcessState)
		default:
			o.RC = 1
			o.Tail += fmt.Sprintf("\nrunner: %v", err)
			if len(o.Tail) > tailMax {
				o.Tail = o.Tail[len(o.Tail)-tailMax:]
			}
		}
	}
	return finish(o)
}

func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
