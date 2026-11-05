// Package propose suggests autonomy promotions from the ledger. It only reads:
// it never writes policy.yml (or anything else). A human applies a suggestion
// by editing the policy file.
//
// Per class (the task_class of run_result events in the last N UTC days) it
// reports the number of runs, the trailing streak of consecutive ok runs, the
// share of runs that needed an approval (protected_modified) and the
// regressions (behavior failures and weakened checks that were not just a
// protected-file approval). The counts come from internal/stats, the same code
// as `aigate stats`.
//
// A class is suggested one step up only if ALL hold (defaults, see
// DefaultConfig, overridable with WithConfig):
//
//	L0 -> L1, L1 -> L2: at least MinConsecutiveOK (10) consecutive ok runs, no
//	  regression in the window, approval-needed rate below MaxApprovalRate (30%).
//	L2 -> L3 additionally: at least L3MinRuns (20) runs.
//	  and no autonomy_demote event for the class in the window.
//
// Never a jump of more than one level, never from L3, and a class that is
// not at L2 is never suggested L3.
package propose

import (
	"fmt"
	"sort"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/stats"
)

// Config holds the thresholds.
type Config struct {
	MinConsecutiveOK int     // default 10
	MaxApprovalRate  float64 // exclusive upper bound, default 0.30
	L3MinRuns        int     // default 20
	Now              func() time.Time
}

// DefaultConfig returns the documented thresholds.
func DefaultConfig() Config {
	return Config{MinConsecutiveOK: 10, MaxApprovalRate: 0.30, L3MinRuns: 20, Now: time.Now}
}

// Proposal is the picture of one class. Eligible says whether the thresholds
// are met; only then From and To differ.
type Proposal struct {
	Class         string       `json:"class"`
	From          policy.Level `json:"-"`
	To            policy.Level `json:"-"`
	FromLevel     string       `json:"from"`
	ToLevel       string       `json:"to"`
	Eligible      bool         `json:"eligible"`
	Runs          int          `json:"runs"`
	ConsecutiveOK int          `json:"consecutive_ok"`
	ApprovalRate  float64      `json:"approval_needed_rate"`
	Regressions   int          `json:"regressions"`
	Demotions     int          `json:"demotions"`
	Reason        string       `json:"reason"`
}

// Option adjusts FromLedger.
type Option func(*options)

type options struct {
	cfg    Config
	levels func(class string) policy.Level
}

// WithConfig overrides the thresholds (zero fields keep their default).
func WithConfig(c Config) Option {
	return func(o *options) {
		if c.MinConsecutiveOK != 0 {
			o.cfg.MinConsecutiveOK = c.MinConsecutiveOK
		}
		if c.MaxApprovalRate != 0 {
			o.cfg.MaxApprovalRate = c.MaxApprovalRate
		}
		if c.L3MinRuns != 0 {
			o.cfg.L3MinRuns = c.L3MinRuns
		}
		if c.Now != nil {
			o.cfg.Now = c.Now
		}
	}
}

// WithLevels tells propose the current (effective) level of each class
// (default: every class is L0).
func WithLevels(f func(class string) policy.Level) Option {
	return func(o *options) { o.levels = f }
}

// FromLedger is Load without the error: an unreadable or unverifiable ledger
// yields no proposals.
func FromLedger(ledgerPath string, days int, opts ...Option) []Proposal {
	ps, err := Load(ledgerPath, days, opts...)
	if err != nil {
		return nil
	}
	return ps
}

// Load verifies the ledger, then returns one Proposal per class seen in the
// window, sorted by class.
func Load(ledgerPath string, days int, opts ...Option) ([]Proposal, error) {
	if days < 1 {
		return nil, fmt.Errorf("propose: days must be at least 1")
	}
	o := options{cfg: DefaultConfig(), levels: func(string) policy.Level { return policy.L0 }}
	for _, f := range opts {
		f(&o)
	}
	if n, err := ledger.Verify(ledgerPath); err != nil {
		return nil, fmt.Errorf("propose: ledger fails verification after %d valid events: %w", n, err)
	}
	evs, err := ledger.Events(ledgerPath)
	if err != nil {
		return nil, err
	}
	now := o.cfg.Now()
	today := now.UTC().Truncate(24 * time.Hour)
	from := today.AddDate(0, 0, -(days - 1))
	inWindow := func(e ledger.Event) bool {
		ts, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			return false
		}
		d := ts.UTC().Truncate(24 * time.Hour)
		return !d.Before(from) && !d.After(today)
	}

	streak := map[string]int{}
	demotions := map[string]int{}
	var classes []string
	seen := map[string]bool{}
	for _, e := range evs {
		if !inWindow(e) {
			continue
		}
		c, _ := e.Payload["task_class"].(string)
		switch e.Type {
		case "run_result":
			if !seen[c] {
				seen[c] = true
				classes = append(classes, c)
			}
			if cat, _ := e.Payload["category"].(string); cat == "ok" {
				streak[c]++
			} else {
				streak[c] = 0
			}
		case "autonomy_demote":
			dc, _ := e.Payload["class"].(string)
			demotions[dc]++
		}
	}
	sort.Strings(classes)
	var out []Proposal
	for _, c := range classes {
		t := stats.Aggregate(evs, "", now, days, c).Totals
		p := Proposal{Class: c, Runs: t.Runs, ConsecutiveOK: streak[c], Demotions: demotions[c]}
		if t.Runs > 0 {
			p.ApprovalRate = float64(t.NeededApproval) / float64(t.Runs)
		}
		p.Regressions = t.Behavior + max(t.Weakened-t.NeededApproval, 0)
		p.From = o.levels(c)
		p.To = p.From
		decide(&p, o.cfg)
		p.FromLevel, p.ToLevel = p.From.String(), p.To.String()
		out = append(out, p)
	}
	return out, nil
}

func decide(p *Proposal, cfg Config) {
	switch {
	case p.From >= policy.L3:
		p.Reason = "already at the highest level"
	case p.Demotions > 0:
		p.Reason = fmt.Sprintf("%d demotion(s) in the window", p.Demotions)
	case p.Regressions > 0:
		p.Reason = fmt.Sprintf("%d regression(s) in the window", p.Regressions)
	case p.ConsecutiveOK < cfg.MinConsecutiveOK:
		p.Reason = fmt.Sprintf("%d consecutive ok, need %d", p.ConsecutiveOK, cfg.MinConsecutiveOK)
	case p.ApprovalRate >= cfg.MaxApprovalRate:
		p.Reason = fmt.Sprintf("approval-needed rate %.0f%% is not below %.0f%%", 100*p.ApprovalRate, 100*cfg.MaxApprovalRate)
	case p.From == policy.L2 && p.Runs < cfg.L3MinRuns:
		p.Reason = fmt.Sprintf("%d runs, L3 needs %d", p.Runs, cfg.L3MinRuns)
	default:
		p.Eligible = true
		p.To = p.From + 1
		p.Reason = fmt.Sprintf("class %s could move from %s to %s", p.Class, p.From, p.To)
	}
}
