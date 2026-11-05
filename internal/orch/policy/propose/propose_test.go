package propose

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
)

var clock = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

type led struct {
	t    *testing.T
	path string
	n    int
}

func newLed(t *testing.T) *led { return &led{t: t, path: filepath.Join(t.TempDir(), "l.jsonl")} }

func (l *led) at() time.Time {
	l.n++
	return clock.Add(-time.Hour * 24).Add(time.Duration(l.n) * time.Minute)
}

func (l *led) run(class, cat string, rules ...string) {
	l.t.Helper()
	if rules == nil {
		rules = []string{}
	}
	rs := make([]any, len(rules))
	for i, r := range rules {
		rs[i] = r
	}
	if err := ledger.Append(l.path, "run_result", "r1", map[string]any{
		"category": cat, "task_class": class, "rules": rs}, l.at()); err != nil {
		l.t.Fatal(err)
	}
}

func (l *led) oks(class string, n int) {
	for i := 0; i < n; i++ {
		l.run(class, "ok")
	}
}

func one(t *testing.T, l *led, opts ...Option) Proposal {
	t.Helper()
	opts = append([]Option{WithConfig(Config{Now: func() time.Time { return clock }})}, opts...)
	ps, err := Load(l.path, 14, opts...)
	if err != nil || len(ps) != 1 {
		t.Fatalf("%v %v", ps, err)
	}
	return ps[0]
}

func TestThresholdBoundaries(t *testing.T) {
	l := newLed(t)
	l.oks("docs", 9)
	if p := one(t, l); p.Eligible || p.ConsecutiveOK != 9 {
		t.Fatalf("9 ok must not be enough: %+v", p)
	}
	l.oks("docs", 1)
	p := one(t, l)
	if !p.Eligible || p.From != policy.L0 || p.To != policy.L1 || p.ConsecutiveOK != 10 || p.Runs != 10 || p.FromLevel != "L0" || p.ToLevel != "L1" {
		t.Fatalf("10 ok must be enough: %+v", p)
	}
}

func TestRegressionBlocksAndStreakResets(t *testing.T) {
	l := newLed(t)
	l.oks("docs", 12)
	l.run("docs", "behavior_failure", "regression")
	p := one(t, l)
	if p.Eligible || p.Regressions != 1 || p.ConsecutiveOK != 0 {
		t.Fatalf("%+v", p)
	}
	l.oks("docs", 15) // streak recovers, but the regression is still inside the window
	if p := one(t, l); p.Eligible || p.ConsecutiveOK != 15 || p.Regressions != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestApprovalRateBoundary(t *testing.T) {
	// total runs: needed approvals first, then ok runs.
	mk := func(needed, ok int) Proposal {
		l := newLed(t)
		for i := 0; i < needed; i++ {
			l.run("docs", "check_weakened", "protected_modified")
		}
		l.oks("docs", ok)
		return one(t, l, WithConfig(Config{MinConsecutiveOK: 7}))
	}
	if p := mk(3, 10); !p.Eligible { // 3/13 = 23%
		t.Fatalf("23%% should be fine: %+v", p)
	}
	if p := mk(3, 7); p.Eligible || p.ApprovalRate != 0.3 { // exactly 30%
		t.Fatalf("exactly 30%% is not below 30%%: %+v", p)
	}
	if p := mk(2, 8); !p.Eligible || p.Regressions != 0 { // 20%; approvals are not regressions
		t.Fatalf("20%% must be eligible: %+v", p)
	}
	l := newLed(t)
	l.run("docs", "check_weakened", "test_missing") // another weakening is a regression
	l.oks("docs", 10)
	if p := one(t, l); p.Eligible || p.Regressions != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestLevelsAndL3Rules(t *testing.T) {
	at := func(lvl policy.Level) Option { return WithLevels(func(string) policy.Level { return lvl }) }
	l := newLed(t)
	l.oks("docs", 19)
	if p := one(t, l, at(policy.L1)); !p.Eligible || p.To != policy.L2 {
		t.Fatalf("L1 -> L2: %+v", p)
	}
	if p := one(t, l, at(policy.L2)); p.Eligible {
		t.Fatalf("L2 -> L3 needs 20 runs, has 19: %+v", p)
	}
	l.oks("docs", 1)
	if p := one(t, l, at(policy.L2)); !p.Eligible || p.To != policy.L3 {
		t.Fatalf("L2 -> L3 with 20 runs: %+v", p)
	}
	// Never from L3, and never more than one step (L0 with 100 ok -> L1 only).
	if p := one(t, l, at(policy.L3)); p.Eligible || p.To != policy.L3 {
		t.Fatalf("%+v", p)
	}
	big := newLed(t)
	big.oks("docs", 100)
	if p := one(t, big, at(policy.L0)); p.To != policy.L1 {
		t.Fatalf("%+v", p)
	}
	if p := one(t, big, at(policy.L1)); p.To != policy.L2 {
		t.Fatalf("L1 must never be suggested L3: %+v", p)
	}
}

func TestDemotionInWindowBlocks(t *testing.T) {
	l := newLed(t)
	l.oks("docs", 25)
	if err := ledger.Append(l.path, "autonomy_demote", "r1", map[string]any{
		"class": "docs", "from": "L2", "to": "L1", "reason": "x", "task": 1}, l.at()); err != nil {
		t.Fatal(err)
	}
	p := one(t, l, WithLevels(func(string) policy.Level { return policy.L2 }))
	if p.Eligible || p.Demotions != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestWindowAndClassIsolation(t *testing.T) {
	l := newLed(t)
	l.oks("docs", 10)
	l.oks("bug", 3)
	ps, err := Load(l.path, 14, WithConfig(Config{Now: func() time.Time { return clock }}))
	if err != nil || len(ps) != 2 {
		t.Fatalf("%v %v", ps, err)
	}
	if ps[0].Class != "bug" || ps[0].Eligible || ps[1].Class != "docs" || !ps[1].Eligible {
		t.Fatalf("%+v", ps)
	}
}

func TestDoesNotModifyPolicyFile(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "policy.yml")
	if err := os.WriteFile(pf, []byte("version: 1\nclasses:\n  docs: {level: L0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(pf, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(pf)
	fi0, _ := os.Stat(pf)
	entries0, _ := os.ReadDir(dir)

	l := newLed(t)
	l.oks("docs", 30)
	pol, err := policy.Load(pf)
	if err != nil {
		t.Fatal(err)
	}
	p := one(t, l, WithLevels(pol.LevelFor))
	if !p.Eligible {
		t.Fatalf("setup: %+v", p)
	}
	_ = FromLedger(l.path, 14)

	after, _ := os.ReadFile(pf)
	fi1, _ := os.Stat(pf)
	if string(before) != string(after) || !fi0.ModTime().Equal(fi1.ModTime()) || fi0.Mode() != fi1.Mode() {
		t.Fatal("propose changed the policy file")
	}
	if entries1, _ := os.ReadDir(dir); len(entries1) != len(entries0) {
		t.Fatal("propose created files next to the policy")
	}
}

func TestBrokenLedgerGivesNothing(t *testing.T) {
	l := newLed(t)
	l.oks("docs", 12)
	b, _ := os.ReadFile(l.path)
	os.WriteFile(l.path, append(b[:len(b)-30], []byte("garbage\n")...), 0o600)
	if ps := FromLedger(l.path, 14); ps != nil {
		t.Fatalf("%v", ps)
	}
	if _, err := Load(l.path, 14); err == nil {
		t.Fatal("verification failure must be an error")
	}
	if _, err := Load(l.path, 0); err == nil {
		t.Fatal("days 0")
	}
}

func TestEventsOutsideWindowIgnored(t *testing.T) {
	l := newLed(t)
	if err := ledger.Append(l.path, "run_result", "r1", map[string]any{"category": "behavior_failure", "task_class": "docs", "rules": []any{"regression"}},
		clock.AddDate(0, 0, -30)); err != nil {
		t.Fatal(err)
	}
	l.oks("docs", 10)
	p := one(t, l)
	if !p.Eligible || p.Regressions != 0 || p.Runs != 10 {
		t.Fatalf("%+v", p)
	}
}
