package publish

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
)

// The publisher refuses to start a plan when any action would be refused: the
// allowed push_branch must NOT run before notify is found to be unlisted.
func TestPublishRefusesPartialPlan(t *testing.T) {
	fx := newFixture(t)
	n := &fakeNotifier{}
	p, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: fx.bare, Notifier: n})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Publish(context.Background(), store.Task{ID: 1, Repo: fx.repo, Base: fx.base, HeadSHA: fx.head})
	var na *NeedsApprovalError
	if !errors.As(err, &na) || na.Action.Kind != KindNotify {
		t.Fatalf("err = %v", err)
	}
	if r := refs(t, fx.bare); r != "" {
		t.Fatalf("a partial publication happened: %s", r)
	}
	if len(n.calls) != 0 {
		t.Fatal("notified")
	}
}

type recFn func(typ string, p map[string]any) error

func (f recFn) Record(typ string, p map[string]any) error { return f(typ, p) }

// publish_action is written before the push (the remote does not have the ref
// yet when it is recorded), publish_result after it; the ledger verifies.
func TestPublishActionRecordedBeforePush(t *testing.T) {
	fx := newFixture(t)
	lp := filepath.Join(t.TempDir(), "orchd.jsonl")
	rec := LedgerRecorder(lp)
	var seen []string
	var refsAtAction string
	p, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: fx.bare,
		Ledger: recFn(func(typ string, pl map[string]any) error {
			seen = append(seen, typ)
			if typ == "publish_action" {
				refsAtAction = refs(t, fx.bare)
			}
			return rec.Record(typ, pl)
		})})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), store.Task{ID: 3, Repo: fx.repo, Base: fx.base, HeadSHA: fx.head, PolicyHash: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "publish_action,publish_result" {
		t.Fatalf("order %v", seen)
	}
	if refsAtAction != "" {
		t.Fatalf("ref already pushed when publish_action was written: %s", refsAtAction)
	}
	if refs(t, fx.bare) == "" {
		t.Fatal("not pushed")
	}
	evs, err := ledger.Events(lp)
	if err != nil || len(evs) != 2 {
		t.Fatalf("%v %+v", err, evs)
	}
	if evs[0].Payload["kind"] != KindPushBranch || evs[0].Payload["head"] != fx.head ||
		evs[0].Payload["policy_hash"] != "abc123" || evs[0].Payload["target"] != fx.bare || evs[0].Repo != policy.OrchRepo {
		t.Fatalf("%+v", evs[0])
	}
	if evs[1].Payload["ok"] != true {
		t.Fatalf("%+v", evs[1])
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

// A ledger that cannot be written stops the action: it is never unrecorded.
func TestPublishNotRunWhenLedgerFails(t *testing.T) {
	fx := newFixture(t)
	p, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: fx.bare,
		Ledger: recFn(func(string, map[string]any) error { return errors.New("disk full") })})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), store.Task{ID: 3, Repo: fx.repo, Base: fx.base, HeadSHA: fx.head}); err == nil {
		t.Fatal("expected an error")
	}
	if r := refs(t, fx.bare); r != "" {
		t.Fatalf("pushed without a record: %s", r)
	}
}
