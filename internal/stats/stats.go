// Package stats computes the per-day and per-class run counts that
// `aigate stats` prints and `orch propose` bases its suggestions on.
package stats

import (
	"sort"
	"time"

	"aigate/internal/ledger"
)

// Row is one day (or the total) of `aigate stats`.
type Row struct {
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

func (r *Row) add(o Row) {
	r.Runs += o.Runs
	r.OK += o.OK
	r.Behavior += o.Behavior
	r.Incomplete += o.Incomplete
	r.Weakened += o.Weakened
	r.NeededApproval += o.NeededApproval
	r.Granted += o.Granted
	r.Refused += o.Refused
}

type Day struct {
	Date string `json:"date"`
	Row
}

type Out struct {
	Repo   string `json:"repo"`
	Days   int    `json:"days"`
	From   string `json:"from"`
	To     string `json:"to"`
	PerDay []Day  `json:"per_day"`
	Totals Row    `json:"totals"`
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

// Aggregate counts the events of repo id per UTC day over the window of
// `days` days ending today (UTC): [today-(days-1), today].
func Aggregate(evs []ledger.Event, id string, now time.Time, days int, class string) Out {
	today := now.UTC().Truncate(24 * time.Hour)
	from := today.AddDate(0, 0, -(days - 1))
	out := Out{Repo: id, Days: days, From: from.Format("2006-01-02"), To: today.Format("2006-01-02"), PerDay: []Day{}, Rules: map[string]int{}}
	byDay := map[string]*Row{}
	for _, ev := range evs {
		if id != "" && ev.Repo != id {
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
		var r Row
		switch ev.Type {
		case "run_result":
			if c, _ := ev.Payload["task_class"].(string); class != "" && c != class {
				continue
			}
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
			if class != "" {
				continue // approval events carry no class
			}
			r.Granted = 1
		case "approval_refused":
			if class != "" {
				continue
			}
			// legacy events (before fix wave 4) have no needed_approval: count
			if needed, ok := ev.Payload["needed_approval"].(bool); !ok || needed {
				r.Refused = 1
			}
		default:
			continue
		}
		k := day.Format("2006-01-02")
		if byDay[k] == nil {
			byDay[k] = &Row{}
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
		out.PerDay = append(out.PerDay, Day{Date: k, Row: *byDay[k]})
	}
	if out.Totals.Runs > 0 {
		v := float64(out.Totals.Granted) / float64(out.Totals.Runs)
		out.Ratio = &v
		n := float64(out.Totals.NeededApproval) / float64(out.Totals.Runs)
		out.NeededRatio = &n
	}
	return out
}
