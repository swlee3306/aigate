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
)

// statsNow is a variable so tests can fix the clock (the --days window); a
// test over real events fixes it to the last event's time.
var statsNow = time.Now

// statsRow is one day (or the total) of `aigate stats`.
type statsRow struct {
	Runs           int `json:"runs"`
	OK             int `json:"ok"`
	Behavior       int `json:"behavior_failure"`
	Incomplete     int `json:"verification_incomplete"`
	Weakened       int `json:"check_weakened"`
	NeededApproval int `json:"needed_approval"`
	Granted        int `json:"approvals_granted"`
	// Refused counts approval_refused (safety-scan refusals) whose run needed
	// approval (payload needed_approval true, or absent in legacy events). A
	// human answering anything but "yes" is not recorded at all.
	Refused int `json:"approvals_refused"`
}

func (r *statsRow) add(o statsRow) {
	r.Runs += o.Runs
	r.OK += o.OK
	r.Behavior += o.Behavior
	r.Incomplete += o.Incomplete
	r.Weakened += o.Weakened
	r.NeededApproval += o.NeededApproval
	r.Granted += o.Granted
	r.Refused += o.Refused
}

type statsDay struct {
	Date string `json:"date"`
	statsRow
}

type statsOut struct {
	Repo   string     `json:"repo"`
	Days   int        `json:"days"`
	From   string     `json:"from"`
	To     string     `json:"to"`
	PerDay []statsDay `json:"per_day"`
	Totals statsRow   `json:"totals"`
	// Ratio is approvals_granted / runs (null when there are no runs).
	Ratio *float64 `json:"approval_ratio"`
	// NeededRatio is needed_approval / runs (null when there are no runs).
	NeededRatio *float64 `json:"approval_needed_ratio"`
	// Rules counts, per violation rule, the run_result events in the window
	// whose "rules" list contains it.
	Rules map[string]int `json:"rules"`
}

// hasRule reports whether a run_result payload lists rule in "rules".
func hasRule(p map[string]any, rule string) bool {
	l, _ := p["rules"].([]any)
	for _, v := range l {
		if s, _ := v.(string); s == rule {
			return true
		}
	}
	return false
}

// aggregate counts the events of repo id per UTC day over the window of
// `days` days ending today (UTC): [today-(days-1), today].
func aggregate(evs []ledger.Event, id string, now time.Time, days int) statsOut {
	today := now.UTC().Truncate(24 * time.Hour)
	from := today.AddDate(0, 0, -(days - 1))
	out := statsOut{Repo: id, Days: days, From: from.Format("2006-01-02"), To: today.Format("2006-01-02"), PerDay: []statsDay{}, Rules: map[string]int{}}
	byDay := map[string]*statsRow{}
	for _, ev := range evs {
		if ev.Repo != id {
			continue
		}
		ts, err := time.Parse(time.RFC3339, ev.TS)
		if err != nil {
			continue // Verify passed, so the line is genuine; an odd ts is not a day
		}
		day := ts.UTC().Truncate(24 * time.Hour)
		if day.Before(from) || day.After(today) {
			continue
		}
		var r statsRow
		switch ev.Type {
		case "run_result":
			r.Runs = 1
			switch c, _ := ev.Payload["category"].(string); c {
			case "ok":
				r.OK = 1
			case "behavior_failure":
				r.Behavior = 1
			case "verification_incomplete":
				r.Incomplete = 1
			case "check_weakened":
				r.Weakened = 1
			}
			if hasRule(ev.Payload, "protected_modified") {
				r.NeededApproval = 1
			}
			l, _ := ev.Payload["rules"].([]any)
			seen := map[string]bool{}
			for _, v := range l {
				if s, _ := v.(string); s != "" && !seen[s] {
					seen[s] = true
					out.Rules[s]++
				}
			}
		case "approval_granted":
			r.Granted = 1
		case "approval_refused":
			// legacy events (before fix wave 4) have no needed_approval: count
			if needed, ok := ev.Payload["needed_approval"].(bool); !ok || needed {
				r.Refused = 1
			}
		default:
			continue
		}
		k := day.Format("2006-01-02")
		if byDay[k] == nil {
			byDay[k] = &statsRow{}
		}
		byDay[k].add(r)
		out.Totals.add(r)
	}
	keys := make([]string, 0, len(byDay))
	for k := range byDay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.PerDay = append(out.PerDay, statsDay{Date: k, statsRow: *byDay[k]})
	}
	if out.Totals.Runs > 0 {
		v := float64(out.Totals.Granted) / float64(out.Totals.Runs)
		out.Ratio = &v
		n := float64(out.Totals.NeededApproval) / float64(out.Totals.Runs)
		out.NeededRatio = &n
	}
	return out
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
		row(d.Date, d.statsRow)
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
