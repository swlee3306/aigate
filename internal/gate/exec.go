package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"aigate/internal/adapter"
	"aigate/internal/config"
	"aigate/internal/gitx"
	"aigate/internal/report"
	"aigate/internal/rules"
	"aigate/internal/runner"
)

// resolveCommand returns the argv and adapter name for a check.
func resolveCommand(c config.Check) ([]string, string, error) {
	if c.Preset != "" {
		p, ok := adapter.PresetFor(c.Preset)
		if !ok {
			return nil, "", fmt.Errorf("check %q: unknown preset %q", c.ID, c.Preset)
		}
		argv := append(append([]string{}, p.Command...), p.Neutralize...)
		return argv, p.Adapter, nil
	}
	if len(c.Command) == 0 {
		return nil, "", fmt.Errorf("check %q: no command", c.ID)
	}
	return append([]string{}, c.Command...), c.Adapter, nil
}

// runSide runs one check (setup first) in an isolated worktree of sha.
func runSide(ctx context.Context, repo, sha string, c config.Check) (rules.RunInfo, report.RunSummary, error) {
	argv, ad, err := resolveCommand(c)
	if err != nil {
		return rules.RunInfo{}, report.RunSummary{}, err
	}
	var info rules.RunInfo
	var seconds float64
	err = gitx.WithWorktree(repo, sha, func(dir string) error {
		if len(c.Setup) > 0 {
			so := runner.Run(ctx, runner.Spec{Dir: dir, Argv: c.Setup, Timeout: c.Timeout})
			seconds += so.Seconds
			if so.TimedOut || so.RC != 0 {
				info = rules.RunInfo{RC: so.RC, TimedOut: so.TimedOut}
				return nil
			}
		}
		tmp, err := os.MkdirTemp("", "aigate-run-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		spec := runner.Spec{Dir: dir, Argv: argv, Timeout: c.Timeout}
		out := filepath.Join(tmp, "out")
		switch ad {
		case "junit":
			spec.Env = []string{"AIGATE_JUNIT=" + out}
		case "gotest-json":
			spec.StdoutFile = out
		case "exit-code":
		default:
			return fmt.Errorf("check %q: unknown adapter %q", c.ID, ad)
		}
		o := runner.Run(ctx, spec)
		seconds += o.Seconds
		info = rules.RunInfo{RC: o.RC, TimedOut: o.TimedOut}
		if o.TimedOut || o.RC == 127 { // timed out, or command could not be started
			return nil
		}
		var p adapter.Parsed
		switch ad {
		case "junit":
			p = adapter.ParseJUnit(out)
		case "gotest-json":
			f, err := os.Open(out)
			if err != nil {
				p = adapter.Parsed{ParseFailed: true}
			} else {
				p = adapter.ParseGoTestJSON(f)
				f.Close()
			}
		default:
			p = adapter.ExitCode(c.ID, o.RC)
		}
		info.Results, info.ParseFailed = p.Results, p.ParseFailed
		return nil
	})
	if err != nil {
		return rules.RunInfo{}, report.RunSummary{}, fmt.Errorf("check %q at %s: %w", c.ID, short(sha), err)
	}
	return info, summary(info, seconds), nil
}

func summary(i rules.RunInfo, seconds float64) report.RunSummary {
	s := report.RunSummary{RC: i.RC, Seconds: seconds}
	for _, st := range i.Results {
		switch st {
		case rules.Passed:
			s.Counts.Passed++
		case rules.Failed:
			s.Counts.Failed++
		case rules.Error:
			s.Counts.Error++
		case rules.Skipped:
			s.Counts.Skipped++
		}
	}
	return s
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
