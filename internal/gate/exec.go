package gate

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"aigate/internal/adapter"
	"aigate/internal/config"
	"aigate/internal/report"
	"aigate/internal/rules"
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

// runSide runs one check (setup first) at sha through ex and parses its
// result bytes. Everything ex returns is untrusted and bounded here.
func runSide(ctx context.Context, ex Executor, sha string, c config.Check) (rules.RunInfo, report.RunSummary, error) {
	argv, ad, err := resolveCommand(c)
	if err != nil {
		return rules.RunInfo{}, report.RunSummary{}, err
	}
	o, err := ex.RunCheck(ctx, CheckRequest{SHA: sha, CheckID: c.ID, Argv: argv, Setup: c.Setup, Adapter: ad, Timeout: c.Timeout})
	if err == nil {
		err = checkBounds(o)
	}
	if err != nil {
		return rules.RunInfo{}, report.RunSummary{}, fmt.Errorf("check %q at %s: %w", c.ID, short(sha), err)
	}
	// A cancelled run (Ctrl-C, SIGTERM) is never a verdict: the runner killed
	// the process group, so its results say nothing about the code.
	if err := ctx.Err(); err != nil {
		return rules.RunInfo{}, report.RunSummary{}, fmt.Errorf("interrupted during check %q at %s: %w", c.ID, short(sha), err)
	}
	tail := o.Tail
	if len(tail) > maxTailBytes {
		tail = tail[len(tail)-maxTailBytes:]
	}
	info := rules.RunInfo{RC: o.RC, TimedOut: o.TimedOut, Tail: cleanTail(tail)}
	if !o.SetupFailed && !o.TimedOut && o.RC != 127 {
		var p adapter.Parsed
		switch ad {
		case "junit":
			p = adapter.ParseJUnit(o.JUnit)
		case "gotest-json":
			p = adapter.ParseGoTestJSON(o.GoTestJSON)
		default:
			p = adapter.ExitCode(c.ID, o.RC)
		}
		info.Results, info.ParseFailed = p.Results, p.ParseFailed
	}
	return info, summary(info, o.Seconds), nil
}

// checkBounds rejects an Executor result carrying more result bytes than the
// gate accepts.
func checkBounds(o CheckOutput) error {
	if len(o.JUnit) > maxResultBytes || len(o.GoTestJSON) > maxResultBytes {
		return fmt.Errorf("check output exceeds %d bytes", maxResultBytes)
	}
	return nil
}

const tailRunes = 300

var (
	// ANSI CSI sequences, OSC sequences (BEL or ST terminated) and any other
	// two-byte escape.
	ansiRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b.?`)
	// NAME=value / NAME: value where NAME mentions a key, token, secret or password.
	secretAssignRe = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:key|token|secret|passwd|password|pwd)[a-z0-9_.-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s"',;]+)`)
	bearerRe       = regexp.MustCompile(`(?i)\b(bearer)\s+[a-z0-9._~+/=-]+`)
	urlCredRe      = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
)

// maskSecrets replaces credential-looking values with ***.
func maskSecrets(s string) string {
	s = urlCredRe.ReplaceAllString(s, "${1}***@")
	s = bearerRe.ReplaceAllString(s, "${1} ***")
	return secretAssignRe.ReplaceAllString(s, "${1}${2}***")
}

// cleanTail makes runner output safe to embed in a violation detail: ANSI
// escapes stripped, valid UTF-8, credential-looking values masked (before
// cutting, so a cut cannot expose part of a secret), whitespace collapsed, at
// most the last tailRunes runes (cut on a rune boundary).
func cleanTail(s string) string {
	s = ansiRe.ReplaceAllString(strings.ToValidUTF8(s, ""), "")
	s = strings.Join(strings.Fields(maskSecrets(s)), " ")
	if r := []rune(s); len(r) > tailRunes {
		s = string(r[len(r)-tailRunes:])
	}
	return s
}

// hasCause reports whether a run had no usable results or an error, i.e.
// whether its output tail helps explain a violation.
func hasCause(i rules.RunInfo) bool {
	if i.TimedOut || i.ParseFailed || len(i.Results) == 0 {
		return true
	}
	for _, st := range i.Results {
		if st == rules.Error {
			return true
		}
	}
	return false
}

// withCause appends the output tail of the relevant side to the detail of
// violations whose rule it can explain. Rule names are not changed.
func withCause(v rules.Violation, base, head rules.RunInfo) rules.Violation {
	side := head
	switch v.Rule {
	case "base_unreliable", "base_not_green":
		side = base
	case "no_tests_ran", "parse_failed", "timeout", "setup_or_collection_error":
	case "runner_error": // results exist but do not explain the exit status
		if side.Tail != "" {
			v.Detail += " | 출력 끝: " + side.Tail
		}
		return v
	default:
		return v
	}
	if side.Tail != "" && hasCause(side) {
		v.Detail += " | 출력 끝: " + side.Tail
	}
	return v
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
