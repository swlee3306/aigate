package broker

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time      { return c.now }
func (c *fakeClock) Add(d time.Duration) { c.now = c.now.Add(d) }
func newClock() *fakeClock               { return &fakeClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)} }
func usd(v float64) int64                { return int64(v * 1e6) }

func TestHourlyLimitSlidingWindow(t *testing.T) {
	clk := newClock()
	l := NewHourlyLimiter(3, clk.Now)
	for i := 0; i < 3; i++ {
		if !l.Allow() {
			t.Fatalf("call %d refused", i)
		}
		clk.Add(10 * time.Minute) // t=0,10,20
	}
	if l.Allow() {
		t.Fatal("4th call inside the hour allowed")
	}
	if l.Remaining() != 0 {
		t.Fatalf("remaining %d", l.Remaining())
	}
	clk.Add(29*time.Minute + 59*time.Second) // t=59:59, first hit (t=0) still counts
	if l.Allow() {
		t.Fatal("allowed before the oldest hit expired")
	}
	clk.Add(2 * time.Second) // t=1:00:01 -> t=0 hit gone, t=10, t=20 remain
	if l.Remaining() != 1 {
		t.Fatalf("remaining %d, want 1", l.Remaining())
	}
	if !l.Allow() {
		t.Fatal("refused after the oldest hit expired")
	}
	// a refused call is not recorded
	if l.Allow() {
		t.Fatal("over limit allowed")
	}
	clk.Add(11 * time.Minute) // t=1:11:01 -> t=10 expired
	if !l.Allow() {
		t.Fatal("refused calls must not extend the window")
	}
}

func TestWeeklyBudgetPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	clk := newClock()
	b, err := OpenBudget(path, usd(25), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.Reserve(usd(10))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(id, usd(4)); err != nil {
		t.Fatal(err)
	}
	if got := b.RemainingMicros(); got != usd(21) {
		t.Fatalf("remaining %d", got)
	}
	// restart
	b2, err := OpenBudget(path, usd(25), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got := b2.RemainingMicros(); got != usd(21) {
		t.Fatalf("after restart remaining %d, want %d", got, usd(21))
	}
	if _, err := b2.Reserve(usd(21) + 1); err != ErrBudgetExceeded {
		t.Fatalf("over-budget reserve: %v", err)
	}
	// an unsettled reservation survives a crash as spend (conservative)
	if _, err := b2.Reserve(usd(20)); err != nil {
		t.Fatal(err)
	}
	b3, _ := OpenBudget(path, usd(25), clk.Now)
	if got := b3.RemainingMicros(); got != usd(1) {
		t.Fatalf("crash left remaining %d, want %d", got, usd(1))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("budget file mode %v", fi.Mode().Perm())
	}
	// no leftover temp files
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
}

func TestBudgetCorruptFileFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":     "not json",
		"truncated":   `{"version":1,"entries":[{"id":"a","at":"2026-11-`,
		"bad version": `{"version":9,"entries":[]}`,
		"negative":    `{"version":1,"entries":[{"id":"a","at":"2026-11-09T11:00:00Z","micro_usd":-5}]}`,
		"unknown key": `{"version":1,"entries":[],"extra":1}`,
		"bad time":    `{"version":1,"entries":[{"id":"a","at":"yesterday","micro_usd":5}]}`,
		"empty file":  ``,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget.json")
			os.WriteFile(path, []byte(content), 0o600)
			b, err := OpenBudget(path, usd(25), newClock().Now)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if !b.Exhausted() || b.RemainingMicros() != 0 {
				t.Fatal("corrupt budget not treated as exhausted")
			}
			if _, err := b.Reserve(1); err != ErrBudgetExceeded {
				t.Fatalf("reserve on corrupt budget: %v", err)
			}
			after, _ := os.ReadFile(path)
			if string(after) != content {
				t.Fatal("corrupt budget file was overwritten")
			}
		})
	}
	// an absent file is a fresh, empty budget
	b, err := OpenBudget(filepath.Join(t.TempDir(), "budget.json"), usd(25), newClock().Now)
	if err != nil || b.Exhausted() || b.RemainingMicros() != usd(25) {
		t.Fatalf("fresh budget wrong: %v", err)
	}
}

func TestRollingSevenDayWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	clk := newClock()
	b, _ := OpenBudget(path, usd(25), clk.Now)
	id, _ := b.Reserve(usd(20))
	b.Settle(id, usd(20))
	clk.Add(3 * 24 * time.Hour)
	id2, err := b.Reserve(usd(4))
	if err != nil {
		t.Fatal(err)
	}
	b.Settle(id2, usd(4))
	// t0+6d23h: the $20 entry is still inside the rolling window.
	clk.Add(3*24*time.Hour + 23*time.Hour) // t0 + 6d23h
	if got := b.RemainingMicros(); got != usd(1) {
		t.Fatalf("before expiry remaining %d, want %d", got, usd(1))
	}
	if _, err := b.Reserve(usd(2)); err != ErrBudgetExceeded {
		t.Fatalf("expected exceeded inside window, got %v", err)
	}
	// t0+7d+1s: the $20 entry is gone, the $4 entry (4d old) stays.
	clk.Add(time.Hour + time.Second)
	if got := b.RemainingMicros(); got != usd(21) {
		t.Fatalf("after expiry remaining %d, want %d", got, usd(21))
	}
	if _, err := b.Reserve(usd(21)); err != nil {
		t.Fatalf("reserve after expiry: %v", err)
	}
	// pruning is persisted: a restart sees only live entries
	b2, _ := OpenBudget(path, usd(25), clk.Now)
	if got := b2.RemainingMicros(); got != 0 {
		t.Fatalf("restart remaining %d", got)
	}
	clk.Add(4 * 24 * time.Hour) // the $4 entry is now 8 days old
	if got := b2.RemainingMicros(); got != usd(4) {
		t.Fatalf("remaining %d, want %d", got, usd(4))
	}
}

func TestBudgetConcurrentReservationsCannotOvershoot(t *testing.T) {
	b, _ := OpenBudget(filepath.Join(t.TempDir(), "budget.json"), usd(25), newClock().Now)
	done := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		go func() { _, err := b.Reserve(usd(5)); done <- err == nil }()
	}
	ok := 0
	for i := 0; i < 20; i++ {
		if <-done {
			ok++
		}
	}
	if ok != 5 {
		t.Fatalf("%d reservations of $5 granted against $25", ok)
	}
}

func TestCostMath(t *testing.T) {
	p := Pricing{InputPerMTok: 2.5, OutputPerMTok: 10}
	// 3000 bytes -> 1000 tokens * 2.5 + 4096 * 10 micro-USD
	if got, err := WorstCaseMicros(p, 3000, 4096); err != nil || got != 2500+40960 {
		t.Fatalf("worst case %d %v", got, err)
	}
	// ceil(input bytes / 3): 1001 tokens -> 2502.5 -> 2503
	if got, err := WorstCaseMicros(p, 3001, 0); err != nil || got != 2503 {
		t.Fatalf("ceil %d %v", got, err)
	}
	if got, err := ActualMicros(p, 1000, 100); err != nil || got != 3500 {
		t.Fatalf("actual %d %v", got, err)
	}
}

func TestCostRejectsOverflowAndNegative(t *testing.T) {
	p := Pricing{InputPerMTok: 2.5, OutputPerMTok: 10}
	if _, err := WorstCaseMicros(p, -1, 10); err == nil {
		t.Fatal("negative input bytes accepted")
	}
	if _, err := WorstCaseMicros(p, 10, -1); err == nil {
		t.Fatal("negative output tokens accepted")
	}
	if _, err := WorstCaseMicros(p, math.MaxInt64, 10); err == nil {
		t.Fatal("overflowing input bytes accepted")
	}
	if _, err := WorstCaseMicros(p, 10, math.MaxInt64); err == nil {
		t.Fatal("overflowing output tokens accepted")
	}
	if _, err := ActualMicros(p, -5, 1); err == nil {
		t.Fatal("negative usage accepted")
	}
	if _, err := ActualMicros(Pricing{InputPerMTok: 1e300, OutputPerMTok: 1}, 1<<60, 1); err == nil {
		t.Fatal("overflowing cost accepted")
	}
	b, _ := OpenBudget(filepath.Join(t.TempDir(), "budget.json"), usd(25), newClock().Now)
	if _, err := b.Reserve(-1); err == nil {
		t.Fatal("negative reservation accepted")
	}
	if b.RemainingMicros() != usd(25) {
		t.Fatal("rejected reservation changed the budget")
	}
}

func TestSettleNoUsageContract(t *testing.T) {
	p := Pricing{InputPerMTok: 2.5, OutputPerMTok: 10}
	newB := func() (*Budget, string) {
		b, _ := OpenBudget(filepath.Join(t.TempDir(), "budget.json"), usd(25), newClock().Now)
		id, err := b.Reserve(usd(10))
		if err != nil {
			t.Fatal(err)
		}
		return b, id
	}
	// negative is rejected, never "keep"
	b, id := newB()
	if err := b.Settle(id, -1); err == nil {
		t.Fatal("Settle(-1) accepted")
	}
	if b.RemainingMicros() != usd(15) {
		t.Fatal("rejected Settle changed spend")
	}
	// missing usage keeps the reservation
	for name, u := range map[string]Usage{"none": {}, "no output": {InputTokens: ptr(5)}, "no input": {OutputTokens: ptr(5)}} {
		b, id := newB()
		if err := b.SettleUsage(id, p, u); err != nil {
			t.Fatal(err)
		}
		if b.RemainingMicros() != usd(15) {
			t.Fatalf("%s: reservation not kept", name)
		}
	}
	// a reported zero is a real zero cost
	b, id = newB()
	if err := b.SettleUsage(id, p, Usage{InputTokens: ptr(0), OutputTokens: ptr(0)}); err != nil {
		t.Fatal(err)
	}
	if b.RemainingMicros() != usd(25) {
		t.Fatal("explicit zero usage not settled to zero")
	}
	// reported usage settles to its cost
	b, id = newB()
	b.SettleUsage(id, p, Usage{InputTokens: ptr(1000), OutputTokens: ptr(100)})
	if b.RemainingMicros() != usd(25)-3500 {
		t.Fatalf("remaining %d", b.RemainingMicros())
	}
}

func ptr(v int64) *int64 { return &v }

func TestPersistFailureRefusesWithoutGhostEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "state")
	b, err := OpenBudget(filepath.Join(dir, "budget.json"), usd(25), newClock().Now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	b.OnPersistError = func() { calls++ }
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if _, err := b.Reserve(usd(5)); err != ErrBudgetExceeded {
		t.Fatalf("reserve with unwritable state: %v", err)
	}
	if calls != 1 {
		t.Fatalf("persist error hook called %d times", calls)
	}
	if b.RemainingMicros() != usd(25) {
		t.Fatal("failed reservation left a ghost entry")
	}
	os.Chmod(dir, 0o700)
	if _, err := b.Reserve(usd(5)); err != nil {
		t.Fatalf("reserve after recovery: %v", err)
	}
}
