package gate

import (
	"context"
	"time"
)

const (
	// maxResultBytes caps the JUnit and go-test-JSON bytes accepted from an
	// Executor. Output coming back from an Executor is untrusted: exceeding
	// the cap is an error, never a silent truncation and never a verdict.
	maxResultBytes = 8 << 20
	// maxTailBytes caps the output tail accepted from an Executor.
	maxTailBytes = 1500
)

// CheckRequest is one check to run at one commit: Setup (if any) first, then
// Argv, both in a clean checkout of SHA.
type CheckRequest struct {
	SHA     string
	CheckID string
	Argv    []string
	Setup   []string
	Env     []string // extra KEY=VALUE entries for the check command
	Adapter string   // "junit", "gotest-json" or "exit-code"
	Timeout time.Duration
}

// CheckOutput is what an Executor reports back. JUnit and GoTestJSON hold the
// adapter's raw result bytes; nil means the check produced no result file.
// SetupFailed means Setup did not succeed: RC, TimedOut and Tail then describe
// the setup command and no result bytes exist.
type CheckOutput struct {
	RC          int
	TimedOut    bool
	SetupFailed bool
	Tail        string
	Seconds     float64
	JUnit       []byte
	GoTestJSON  []byte
}

// Executor runs checks somewhere (locally, or on a remote machine). An error
// from any method is an infrastructure failure, never a check verdict.
type Executor interface {
	// Prepare makes repo's commits shas runnable by RunCheck.
	Prepare(ctx context.Context, repo string, shas []string) error
	RunCheck(ctx context.Context, req CheckRequest) (CheckOutput, error)
	Close() error
}
