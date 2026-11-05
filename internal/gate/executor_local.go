package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"aigate/internal/gitx"
	"aigate/internal/runner"
)

// LocalExecutor runs checks on this machine, each in an isolated detached
// worktree of the requested commit.
type LocalExecutor struct {
	repo string
}

// Prepare records the repository; worktrees are created per RunCheck.
func (e *LocalExecutor) Prepare(_ context.Context, repo string, _ []string) error {
	e.repo = repo
	return nil
}

func (e *LocalExecutor) Close() error { return nil }

// RunCheck runs req.Setup then req.Argv in a worktree of req.SHA.
func (e *LocalExecutor) RunCheck(ctx context.Context, req CheckRequest) (CheckOutput, error) {
	if e.repo == "" {
		return CheckOutput{}, errors.New("local executor: Prepare was not called")
	}
	var res CheckOutput
	err := gitx.WithWorktree(e.repo, req.SHA, func(dir string) error {
		if len(req.Setup) > 0 {
			so := runner.Run(ctx, runner.Spec{Dir: dir, Argv: req.Setup, Timeout: req.Timeout})
			res.Seconds += so.Seconds
			if so.TimedOut || so.RC != 0 {
				res.RC, res.TimedOut, res.SetupFailed, res.Tail = so.RC, so.TimedOut, true, so.Tail
				return nil
			}
		}
		tmp, err := os.MkdirTemp("", "aigate-run-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		spec := runner.Spec{Dir: dir, Argv: req.Argv, Env: req.Env, Timeout: req.Timeout}
		out := filepath.Join(tmp, "out")
		switch req.Adapter {
		case "junit":
			spec.Env = append(append([]string{}, req.Env...), "AIGATE_JUNIT="+out)
		case "gotest-json":
			spec.StdoutFile = out
		case "exit-code":
		default:
			return fmt.Errorf("check %q: unknown adapter %q", req.CheckID, req.Adapter)
		}
		o := runner.Run(ctx, spec)
		res.RC, res.TimedOut, res.Tail = o.RC, o.TimedOut, o.Tail
		res.Seconds += o.Seconds
		if o.TimedOut || o.RC == 127 { // timed out, or command could not be started
			return nil
		}
		switch req.Adapter {
		case "junit":
			res.JUnit, err = readBounded(out)
		case "gotest-json":
			res.GoTestJSON, err = readBounded(out)
		}
		return err
	})
	if err != nil {
		return CheckOutput{}, err
	}
	return res, nil
}

// readBounded returns the file's bytes, nil when it does not exist, and an
// error when it is larger than maxResultBytes.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResultBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxResultBytes {
		return nil, fmt.Errorf("check output exceeds %d bytes", maxResultBytes)
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}
