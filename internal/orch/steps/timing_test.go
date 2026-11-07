package steps

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/gate"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
	"aigate/internal/report"
	"aigate/internal/rules"
)

func TestFmtDur(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0.0s"}, {400 * ms, "0.4s"}, {1200 * ms, "1.2s"}, {9900 * ms, "9.9s"},
		{9960 * ms, "10s"}, {10 * time.Second, "10s"}, {52 * time.Second, "52s"},
		{52600 * ms, "53s"}, {145 * time.Second, "145s"},
	}
	for _, c := range cases {
		if got := fmtDur(c.d); got != c.want {
			t.Errorf("fmtDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// reportingTest is a fakeTest that also reports restore phases.
type reportingTest struct {
	fakeTest
	mu     sync.Mutex
	rec    vm.RestoreRecord
	has    bool
	phases []vm.Phase
	clock  *fakeClock
	cost   time.Duration
}

func (r *reportingTest) Restore(ctx context.Context) error {
	r.clock.advance(r.cost)
	err := r.fakeTest.Restore(ctx)
	r.mu.Lock()
	r.rec, r.has = vm.RestoreRecord{Phases: r.phases}, true
	r.mu.Unlock()
	return err
}
func (r *reportingTest) LastRestore() (vm.RestoreRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec, r.has
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func okPhases() []vm.Phase {
	return []vm.Phase{
		{Name: "pre_prune", Dur: 4 * time.Second}, {Name: "delete_server", Dur: 10 * time.Second},
		{Name: "create_volume", Dur: 6 * time.Second}, {Name: "create_server", Dur: 11 * time.Second},
		{Name: "wait_ssh", Dur: 21 * time.Second}, {Name: "post_prune", Dur: 400 * time.Millisecond},
	}
}

const wantTiming = " | restore 52s (pre_prune 4.0s, delete_server 10s, create_volume 6.0s, create_server 11s, wait_ssh 21s, post_prune 0.4s) | gate 145s (base 60s, head 80s)"

func timedEnv(t *testing.T, mod func(*Config, *env), cat rules.Category) (*env, *reportingTest) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)}
	rt := &reportingTest{clock: clk, cost: 52 * time.Second, phases: okPhases()}
	var e *env
	e = newEnv(t, func(c *Config, en *env) {
		c.Test = rt
		c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
			clk.advance(145 * time.Second)
			var r report.Report
			if cat == rules.CategoryWeakened {
				r = weakened(o, "protected_modified")
			} else if cat == rules.CategoryBehavior {
				r = weakened(o, "behavior_change")
				r.Category = rules.CategoryBehavior
			} else {
				r = report.Report{SchemaVersion: 1, Category: rules.CategoryOK, Repo: o.Repo, Base: o.Base, Head: o.Head}
			}
			r.Checks = []report.CheckResult{
				{ID: "a", Base: report.RunSummary{Seconds: 20}, Head: report.RunSummary{Seconds: 30}},
				{ID: "b", Base: report.RunSummary{Seconds: 40}, Head: report.RunSummary{Seconds: 50}},
			}
			return r, nil
		}
		if mod != nil {
			mod(c, en)
		}
	})
	e.p.now = clk.now
	return e, rt
}

func TestVerifyNoteHasRestoreAndGateTimings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cat   rules.Category
		to    store.State
		start string
	}{
		{"passed", rules.CategoryOK, machine.StatePassed, "verdict ok"},
		{"needs_approval", rules.CategoryWeakened, machine.StateNeedsApproval, "check_weakened: protected files changed"},
		{"failed", rules.CategoryBehavior, machine.StateFailed, "behavior_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := timedEnv(t, nil, tc.cat)
			e.drain()
			note := e.noteFor(tc.to)
			if !strings.HasPrefix(note, tc.start) || !strings.HasSuffix(note, wantTiming) {
				t.Fatalf("note %q", note)
			}
			if strings.Contains(note, "\n") {
				t.Fatalf("multi-line note %q", note)
			}
			if tc.to == machine.StateFailed {
				e.neverPassed()
			}
		})
	}
}

func TestVerifyNoteWithoutRestoreWhenRestoreEachJobOff(t *testing.T) {
	e, rt := timedEnv(t, func(c *Config, _ *env) { c.RestoreEachJob = false }, rules.CategoryOK)
	e.drain()
	if rt.restores != 0 {
		t.Fatal("restored")
	}
	if got := e.noteFor(machine.StatePassed); got != "verdict ok | gate 145s (base 60s, head 80s)" {
		t.Fatalf("note %q", got)
	}
}

func TestVerifyNoteWithNonReportingDriver(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *env) {
		c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
			return report.Report{SchemaVersion: 1, Category: rules.CategoryOK, Repo: o.Repo, Base: o.Base, Head: o.Head,
				Checks: []report.CheckResult{{ID: "a", Base: report.RunSummary{Seconds: 1.2}, Head: report.RunSummary{Seconds: 0.4}}}}, nil
		}
	})
	clk := &fakeClock{t: time.Unix(0, 0)}
	e.p.now = func() time.Time { return clk.now() }
	e.drain()
	got := e.noteFor(machine.StatePassed)
	if strings.Contains(got, "restore") || !strings.HasPrefix(got, "verdict ok | gate 0.0s (base 1.2s, head 0.4s)") {
		t.Fatalf("note %q", got)
	}
}

func TestRestoreFailureErrorCarriesPhases(t *testing.T) {
	e, rt := timedEnv(t, nil, rules.CategoryOK)
	rt.restoreErr = errors.New("create volume: quota")
	rt.phases = []vm.Phase{{Name: "delete_server", Dur: 10 * time.Second},
		{Name: "create_volume", Dur: 3 * time.Second, Err: "create volume: quota"}}
	tk := e.drain()
	if tk.State != machine.StateFailed && tk.ErrorText == "" {
		t.Fatalf("state %s", tk.State)
	}
	want := "restore test vm: create volume: quota (phases: delete_server 10s, create_volume FAILED 3.0s"
	all := tk.ErrorText
	for _, ev := range e.events() {
		all += "|" + ev.Note
	}
	if !strings.Contains(all, "(phases: delete_server 10s, create_volume FAILED 3.0s)") || !strings.Contains(all, "restore test vm: create volume: quota") {
		t.Fatalf("%s\nerror %q\nevents %+v", want, tk.ErrorText, e.events())
	}
	e.neverPassed()
}

func TestTimingSummaryCappedAt300Bytes(t *testing.T) {
	var ps []vm.Phase
	for i := 0; i < 40; i++ {
		ps = append(ps, vm.Phase{Name: "phase_with_a_long_name_é", Dur: time.Duration(i) * time.Second})
	}
	got := timingSummary(&vm.RestoreRecord{Phases: ps,
		PruneWarnings: []string{strings.Repeat("warning ", 500)}}, time.Minute, time.Minute, 1, 2)
	if len(got) > 300 || strings.Contains(got, "\n") || !strings.HasSuffix(got, "…") {
		t.Fatalf("len %d: %q", len(got), got)
	}
	if !strings.HasPrefix(got, " | restore ") {
		t.Fatalf("%q", got)
	}
}
