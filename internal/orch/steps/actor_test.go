package steps

import (
	"context"
	"testing"

	"aigate/internal/ledger"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
)

// The task_transition ledger payload names who made the transition: the
// caller's actor for a cancel, "system" for the worker's own steps; via is
// the same kind:id string.
func TestTransitionLedgerPayloadHasActor(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if err := e.m.Tick(ctx); err != nil { // queued -> preparing, by the worker
		t.Fatal(err)
	}
	if err := e.m.Cancel(store.WithActor(ctx, "session:mac-passkey"), e.id); err != nil {
		t.Fatal(err)
	}
	evs, err := ledger.Events(policy.OrchLedgerPath(e.p.cfg.GateHome))
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]any
	for _, ev := range evs {
		if ev.Type != "task_transition" {
			continue
		}
		got = append(got, [2]any{ev.Payload["actor"], ev.Payload["via"]})
		if ev.Payload["to"] == string(machine.StateCancelled) &&
			(ev.Payload["actor"] != "session:mac-passkey" || ev.Payload["via"] != "session:mac-passkey") {
			t.Fatalf("cancel event %+v", ev.Payload)
		}
	}
	want := [][2]any{{"system", "system"}, {"session:mac-passkey", "session:mac-passkey"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("actor/via %v, want %v", got, want)
	}
	if _, err := ledger.Verify(policy.OrchLedgerPath(e.p.cfg.GateHome)); err != nil {
		t.Fatal(err)
	}
}
