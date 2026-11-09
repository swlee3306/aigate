package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
)

// principalCaller sends requests to an env as one kind of principal.
type principalCaller struct {
	name string
	// want is the actor string the API must record for this principal.
	want string
	// bind returns the request function for e (it may start an HTTP server
	// or create a credential in e's store).
	bind func(t *testing.T, e *env) func(method, path, body string) (int, []byte)
}

func socketActor() string { return "socket:uid:" + strconv.Itoa(os.Getuid()) }

func principalCallers() []principalCaller {
	return []principalCaller{
		{"socket", socketActor(), func(t *testing.T, e *env) func(string, string, string) (int, []byte) {
			return func(m, p, b string) (int, []byte) { c, _, rb := e.sockDo(t, m, p, b, nil); return c, rb }
		}},
		{"session", "session:alice", func(t *testing.T, e *env) func(string, string, string) (int, []byte) {
			srv := e.httpServer(t)
			tok, csrf := e.newSession(t, "alice")
			return func(m, p, b string) (int, []byte) {
				c, _, rb := httpDo(t, srv, m, p, b, hdr{"Cookie": cookie(tok), CSRFHeader: csrf})
				return c, rb
			}
		}},
		{"token", "token:mac-ssh", func(t *testing.T, e *env) func(string, string, string) (int, []byte) {
			srv := e.httpServer(t)
			tok := e.newToken(t, "mac-ssh")
			return func(m, p, b string) (int, []byte) {
				c, _, rb := httpDo(t, srv, m, p, b, hdr{"Authorization": "Bearer " + tok})
				return c, rb
			}
		}},
	}
}

func lastEventActor(t *testing.T, e *env, id int64) string {
	t.Helper()
	evs, err := e.st.Events(context.Background(), id)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v %v", evs, err)
	}
	return evs[len(evs)-1].Actor
}

// Every write handler that records a task event records the request's
// principal as its actor (socket, session and token principals), and the task
// view shows it.
func TestEveryWriteHandlerPassesPrincipal(t *testing.T) {
	for _, pc := range principalCallers() {
		t.Run(pc.name, func(t *testing.T) {
			e := newEnv(t, noSteps{}, 1)
			e.d.pauseWorkers()
			call := pc.bind(t, e)
			check := func(what string, code int, b []byte, wantCode int, id int64) {
				t.Helper()
				if code != wantCode {
					t.Fatalf("%s: %d %s", what, code, b)
				}
				var v TaskView
				if err := json.Unmarshal(b, &v); err != nil || v.Actor != pc.want {
					t.Fatalf("%s: view actor %q (want %q) %v %s", what, v.Actor, pc.want, err, b)
				}
				if a := lastEventActor(t, e, id); a != pc.want {
					t.Fatalf("%s: event actor %q, want %q", what, a, pc.want)
				}
			}

			// submit (v1 and the flat route)
			for _, path := range []string{"/api/v1/tasks", "/tasks"} {
				code, b := call("POST", path, e.submitBody("p"))
				var v TaskView
				_ = json.Unmarshal(b, &v)
				check("submit "+path, code, b, http.StatusCreated, v.ID)
			}

			// cancel
			id, err := e.st.CreateTask(context.Background(), store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "docs", Agent: "claude"})
			if err != nil {
				t.Fatal(err)
			}
			code, b := call("POST", "/api/v1/tasks/"+itoa(id)+"/cancel", "")
			check("cancel", code, b, 200, id)

			// retry
			id = failedTask(t, e, store.StateVerifying, true)
			code, b = call("POST", "/api/v1/tasks/"+itoa(id)+"/retry", "")
			check("retry", code, b, 200, id)

			// reverify (legacy grant in the approvals file)
			tid, aid, home := parked(t, e, store.KindProtectedModified, "hash1")
			grant(t, e, home, e.sha, "hash1")
			code, b = call("POST", "/api/v1/approvals/"+itoa(aid)+"/reverify", "")
			check("reverify", code, b, 200, tid)
		})
	}
	// grant (signed): its own fixture, in signed-only mode
	for _, pc := range principalCallers() {
		t.Run(pc.name+"/grant", func(t *testing.T) {
			f := newGrantFixture(t)
			call := pc.bind(t, f.e)
			tid, aid, _ := f.protectedTask(t, "line1\nACTOR\n")
			issued := nowSec()
			m := f.message(t, aid, issued)
			body, _ := json.Marshal(f.body(f.alice.signText(t, m.Text()), issued))
			code, b := call("POST", "/api/v1/approvals/"+itoa(aid)+"/grant", string(body))
			if code != 200 {
				t.Fatalf("grant %d %s", code, b)
			}
			var v TaskView
			if err := json.Unmarshal(b, &v); err != nil || v.Actor != pc.want {
				t.Fatalf("grant view actor %q (want %q) %s", v.Actor, pc.want, b)
			}
			if a := lastEventActor(t, f.e, tid); a != pc.want {
				t.Fatalf("grant event actor %q, want %q", a, pc.want)
			}
		})
	}
}

type nopAgent struct{}

func (nopAgent) Prepare(context.Context, string, string, string) error { return nil }
func (nopAgent) Run(context.Context, string, string, string, time.Duration) (vm.RunResult, error) {
	return vm.RunResult{}, errors.New("not used")
}
func (nopAgent) Collect(context.Context, string, string, string) (string, int, error) {
	return "", 0, errors.New("not used")
}

type nopTest struct{}

func (nopTest) Restore(context.Context) error { return nil }
func (nopTest) Executor() (gate.Executor, error) {
	return nil, errors.New("not used")
}

// A submit through the API appends task_submitted to the orchd ledger with
// the submitter as actor and via; an idempotent replay appends nothing.
func TestLedgerSubmittedEventHasActor(t *testing.T) {
	home := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := steps.DefaultConfig()
	cfg.Agent, cfg.Test = nopAgent{}, nopTest{}
	cfg.ReportDir, cfg.GateHome = t.TempDir(), home
	p, err := steps.NewProvider(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The env opens its own store; the provider only needs one to read tasks
	// for task_transition, which this test does not look at.
	e := newEnv(t, p, 1, func(c *Config) { c.GateHome = home })
	e.d.pauseWorkers()
	srv := e.httpServer(t)
	tok := e.newToken(t, "mac-ssh")
	code, _, b := httpDo(t, srv, "POST", "/api/v1/tasks", e.submitBody("p1"),
		hdr{"Authorization": "Bearer " + tok, "Idempotency-Key": "k1"})
	if code != http.StatusCreated {
		t.Fatalf("submit %d %s", code, b)
	}
	if code, _, b := httpDo(t, srv, "POST", "/api/v1/tasks", e.submitBody("p1"),
		hdr{"Authorization": "Bearer " + tok, "Idempotency-Key": "k1"}); code != 200 {
		t.Fatalf("replay %d %s", code, b)
	}
	if code, _, b := e.sockDo(t, "POST", "/api/v1/tasks", e.submitBody("p2"), nil); code != http.StatusCreated {
		t.Fatalf("socket submit %d %s", code, b)
	}
	lp := policy.OrchLedgerPath(home)
	evs, err := ledger.Events(lp)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range evs {
		if ev.Type != "task_submitted" {
			continue
		}
		pl := ev.Payload
		if ev.Repo != policy.OrchRepo || pl["repo"] != e.repo || pl["class"] != "docs" || pl["agent"] != "claude" || pl["via"] != pl["actor"] {
			t.Fatalf("event %+v", ev)
		}
		got = append(got, fmt.Sprint(pl["task"], "@", pl["actor"]))
	}
	want := []string{"1@token:mac-ssh", "2@" + socketActor()}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("task_submitted %v, want %v", got, want)
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

// approval_granted's via is the principal's kind:id (socket:uid:<n>,
// session:<signer>), with the same value as actor; signer and alg stay.
func TestApprovalGrantedViaIsKindAndID(t *testing.T) {
	for _, pc := range principalCallers() {
		t.Run(pc.name, func(t *testing.T) {
			f := newGrantFixture(t)
			call := pc.bind(t, f.e)
			_, aid, _ := f.protectedTask(t, "line1\nVIA2\n")
			issued := nowSec()
			m := f.message(t, aid, issued)
			body, _ := json.Marshal(f.body(f.alice.signText(t, m.Text()), issued))
			if code, b := call("POST", "/api/v1/approvals/"+itoa(aid)+"/grant", string(body)); code != 200 {
				t.Fatalf("grant %d %s", code, b)
			}
			evs, err := ledger.Events(gate.LedgerPath(f.home, f.rid))
			if err != nil || len(evs) != 1 {
				t.Fatalf("ledger %v %+v", err, evs)
			}
			pl := evs[0].Payload
			if evs[0].Type != "approval_granted" || pl["via"] != pc.want || pl["actor"] != pc.want ||
				pl["signer"] != "alice" || pl["alg"] != signing.TypeSSHEd25519 {
				t.Fatalf("event %+v", evs[0])
			}
		})
	}
}
