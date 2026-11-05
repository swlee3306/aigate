package gate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"aigate/internal/config"
	"aigate/internal/rules"
)

func localFor(t *testing.T, repo string, shas ...string) Executor {
	t.Helper()
	ex := &LocalExecutor{}
	if err := ex.Prepare(context.Background(), repo, shas); err != nil {
		t.Fatal(err)
	}
	return ex
}

// fakeExecutor returns a canned output.
type fakeExecutor struct {
	out CheckOutput
	err error
}

func (f fakeExecutor) Prepare(context.Context, string, []string) error { return nil }
func (f fakeExecutor) RunCheck(context.Context, CheckRequest) (CheckOutput, error) {
	return f.out, f.err
}
func (f fakeExecutor) Close() error { return nil }

// TestLocalExecutorMatchesLegacy pins the observable result (RunInfo, RC,
// counts) of one check run for inputs that exercised every branch of the
// pre-Executor runSide. The expectations were captured from that code.
func TestLocalExecutorMatchesLegacy(t *testing.T) {
	repo := newRepo(t)
	write(t, repo, "f", "x")
	sha := commit(t, repo, "c")
	junit := `printf '<testsuite name="s"><testcase classname="c" name="ok"/><testcase classname="c" name="bad"><failure/></testcase></testsuite>' > "$AIGATE_JUNIT"; echo tail-junit; exit 1`
	gojson := `printf '%s\n' '{"Action":"run","Package":"p","Test":"TestA"}' '{"Action":"pass","Package":"p","Test":"TestA"}' '{"Action":"run","Package":"p","Test":"TestB"}' '{"Action":"fail","Package":"p","Test":"TestB"}'; exit 1`
	cases := []struct {
		name string
		c    config.Check
		want rules.RunInfo
	}{
		{"junit", config.Check{ID: "j", Command: []string{"sh", "-c", junit}, Adapter: "junit"},
			rules.RunInfo{RC: 1, Tail: "tail-junit", Results: rules.Results{"c::ok": rules.Passed, "c::bad": rules.Failed}}},
		{"junit missing file", config.Check{ID: "j", Command: []string{"sh", "-c", "echo hi"}, Adapter: "junit"},
			rules.RunInfo{RC: 0, Tail: "hi", Results: rules.Results{}, ParseFailed: true}},
		{"gotest-json", config.Check{ID: "g", Command: []string{"sh", "-c", gojson}, Adapter: "gotest-json"},
			rules.RunInfo{RC: 1, Tail: `{"Action":"run","Package":"p","Test":"TestA"} {"Action":"pass","Package":"p","Test":"TestA"} {"Action":"run","Package":"p","Test":"TestB"} {"Action":"fail","Package":"p","Test":"TestB"}`,
				Results: rules.Results{"p/TestA": rules.Passed, "p/TestB": rules.Failed}}},
		{"exit-code", config.Check{ID: "e", Command: []string{"sh", "-c", "echo boom; exit 3"}, Adapter: "exit-code"},
			rules.RunInfo{RC: 3, Tail: "boom", Results: rules.Results{"e": rules.Failed}}},
		{"setup fails", config.Check{ID: "s", Setup: []string{"sh", "-c", "echo nosetup; exit 4"}, Command: []string{"true"}, Adapter: "exit-code"},
			rules.RunInfo{RC: 4, Tail: "nosetup"}},
		{"command not found", config.Check{ID: "n", Command: []string{"/nonexistent/aigate-cmd"}, Adapter: "exit-code"},
			rules.RunInfo{RC: 127, Tail: "runner: fork/exec /nonexistent/aigate-cmd: no such file or directory"}},
		{"timeout", config.Check{ID: "t", Command: []string{"sleep", "30"}, Adapter: "exit-code", Timeout: 200 * time.Millisecond},
			rules.RunInfo{RC: 124, TimedOut: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, sum, err := runSide(context.Background(), localFor(t, repo, sha), sha, tc.c)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(info, tc.want) {
				t.Fatalf("info = %#v\nwant   %#v", info, tc.want)
			}
			if sum.RC != tc.want.RC {
				t.Fatalf("summary RC = %d, want %d", sum.RC, tc.want.RC)
			}
		})
	}
	t.Run("unknown adapter", func(t *testing.T) {
		_, _, err := runSide(context.Background(), localFor(t, repo, sha), sha, config.Check{ID: "u", Command: []string{"true"}, Adapter: "zzz"})
		if err == nil || !strings.Contains(err.Error(), `unknown adapter "zzz"`) {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestRunCheckBoundsOutput: result bytes beyond 8 MiB are an error (from the
// local executor and from the gate for any Executor), never truncated or ok;
// the tail cap (1500 bytes) sits below cleanTail's 300-rune cut and is not observable here.
func TestRunCheckBoundsOutput(t *testing.T) {
	repo := newRepo(t)
	write(t, repo, "f", "x")
	sha := commit(t, repo, "c")
	ctx := context.Background()
	big := `head -c 9437184 /dev/zero > "$AIGATE_JUNIT"`
	for _, ad := range []string{"junit", "gotest-json"} {
		cmd := big
		if ad == "gotest-json" {
			cmd = `head -c 9437184 /dev/zero`
		}
		c := config.Check{ID: "b", Command: []string{"sh", "-c", cmd}, Adapter: ad}
		info, _, err := runSide(ctx, localFor(t, repo, sha), sha, c)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("%s: err = %v, info = %+v", ad, err, info)
		}
	}
	over := make([]byte, 8<<20+1)
	for _, o := range []CheckOutput{{JUnit: over}, {GoTestJSON: over}} {
		_, _, err := runSide(ctx, fakeExecutor{out: o}, sha, config.Check{ID: "b", Command: []string{"true"}, Adapter: "junit"})
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("fake: err = %v", err)
		}
	}
	// exactly at the cap is accepted
	atCap := make([]byte, 8<<20)
	if _, _, err := runSide(ctx, fakeExecutor{out: CheckOutput{JUnit: atCap}}, sha, config.Check{ID: "b", Command: []string{"true"}, Adapter: "junit"}); err != nil {
		t.Fatalf("at cap: %v", err)
	}
	// executor errors surface as errors
	if _, _, err := runSide(ctx, fakeExecutor{err: errors.New("ssh down")}, sha, config.Check{ID: "b", Command: []string{"true"}, Adapter: "junit"}); err == nil {
		t.Fatal("executor error was swallowed")
	}
}
