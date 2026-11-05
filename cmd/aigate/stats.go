package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/stats"
)

// statsNow is a variable so tests can fix the clock (the --days window); a
// test over real events fixes it to the last event's time.
var statsNow = time.Now

type statsRow = stats.Row

func aggregate(evs []ledger.Event, id string, now time.Time, days int) stats.Out {
	return stats.Aggregate(evs, id, now, days, "")
}

// sortedRules returns the rule names by count (descending), then name.
func sortedRules(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

func cmdStats(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("stats", stderr)
	repo := fs.String("repo", ".", "repository directory")
	days := fs.Int("days", 14, "window in UTC days ending today")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if c, done := parse(fs, args); done {
		return c
	}
	if *days < 1 {
		fmt.Fprintf(stderr, "aigate: --days must be at least 1\n%s", usage)
		return exitGateError
	}
	id, _, err := gate.RepoID(*repo)
	if err != nil {
		return fail(stderr, err)
	}
	home, err := gate.Home("")
	if err != nil {
		return fail(stderr, err)
	}
	path := filepath.Clean(gate.LedgerPath(home, id))
	if n, err := ledger.Verify(path); err != nil {
		fmt.Fprintf(stdout, "ledger FAIL after %d valid events: %v (통계를 내지 않습니다)\n", n, err)
		return 1
	}
	evs, err := ledger.Events(path)
	if err != nil {
		fmt.Fprintf(stdout, "ledger FAIL: %v\n", err)
		return 1
	}
	s := aggregate(evs, id, statsNow(), *days)
	if *asJSON {
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}
	if len(s.PerDay) == 0 {
		fmt.Fprintln(stdout, "no events")
		return 0
	}
	fmt.Fprintf(stdout, "repo %s, %s ~ %s (UTC, %d일)\n", id, s.From, s.To, s.Days)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "날짜\truns\tok\tbehavior\tincomplete\tweakened\t승인필요\t승인수\t안전검사 거부")
	row := func(label string, r statsRow) {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", label, r.Runs, r.OK, r.Behavior, r.Incomplete, r.Weakened, r.NeededApproval, r.Granted, r.Refused)
	}
	for _, d := range s.PerDay {
		row(d.Date, d.Row)
	}
	row("합계", s.Totals)
	tw.Flush()
	if s.Ratio != nil {
		fmt.Fprintf(stdout, "approvals_granted / runs = %d/%d = %.2f\n", s.Totals.Granted, s.Totals.Runs, *s.Ratio)
		fmt.Fprintf(stdout, "필요율 needed_approval / runs = %d/%d = %.2f\n", s.Totals.NeededApproval, s.Totals.Runs, *s.NeededRatio)
	} else {
		fmt.Fprintf(stdout, "approvals_granted / runs = %d/0 = - (run 없음)\n", s.Totals.Granted)
		fmt.Fprintf(stdout, "필요율 needed_approval / runs = %d/0 = - (run 없음)\n", s.Totals.NeededApproval)
	}
	fmt.Fprintln(stdout, "주: runs는 모든 실행 수(서로 다른 head 수가 아님). 안전검사 거부는 승인이 필요했던 diff의 approve 거부만 센다. 사람이 'no'로 답한 것은 기록되지 않는다. 이력은 키 없는 해시 체인이라 숫자는 참고용이다.")
	if len(s.Rules) > 0 {
		fmt.Fprintln(stdout, "규칙별 횟수")
		tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "규칙\t횟수")
		for _, k := range sortedRules(s.Rules) {
			fmt.Fprintf(tw, "%s\t%d\n", k, s.Rules[k])
		}
		tw.Flush()
	}
	return 0
}
