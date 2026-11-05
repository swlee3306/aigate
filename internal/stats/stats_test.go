package stats

import (
	"testing"
	"time"

	"aigate/internal/ledger"
)

func TestClassFilter(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	mk := func(class, cat string, rules ...any) ledger.Event {
		if rules == nil {
			rules = []any{}
		}
		return ledger.Event{TS: now.Format(time.RFC3339), Type: "run_result", Repo: "r",
			Payload: map[string]any{"task_class": class, "category": cat, "rules": rules}}
	}
	evs := []ledger.Event{mk("a", "ok"), mk("a", "behavior_failure", "regression"), mk("b", "ok"),
		{TS: now.Format(time.RFC3339), Type: "approval_granted", Repo: "r", Payload: map[string]any{}}}
	if o := Aggregate(evs, "r", now, 1, ""); o.Totals.Runs != 3 || o.Totals.Granted != 1 {
		t.Fatalf("%+v", o.Totals)
	}
	if o := Aggregate(evs, "", now, 1, "a"); o.Totals.Runs != 2 || o.Totals.OK != 1 || o.Totals.Behavior != 1 || o.Totals.Granted != 0 {
		t.Fatalf("%+v", o.Totals)
	}
	if o := Aggregate(evs, "other", now, 1, ""); o.Totals.Runs != 0 {
		t.Fatalf("repo filter: %+v", o.Totals)
	}
}
