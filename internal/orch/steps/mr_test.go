package steps

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
	"aigate/internal/report"
	"aigate/internal/rules"
)

// fakeMR stands in for the broker: it records every call and answers as told.
type fakeMR struct {
	mu     sync.Mutex
	tasks  []store.Task
	actors []string
	res    MRResult
	err    error
	// block makes the call wait for its context (a hung broker).
	block bool
	panic bool
}

func (f *fakeMR) CreateMR(ctx context.Context, t store.Task) (MRResult, error) {
	f.mu.Lock()
	f.tasks = append(f.tasks, t)
	f.actors = append(f.actors, store.ActorFrom(ctx))
	res, err, block, pn := f.res, f.err, f.block, f.panic
	f.mu.Unlock()
	if pn {
		panic("boom with a secret glpat-XYZ")
	}
	if block {
		<-ctx.Done()
		return MRResult{}, ctx.Err()
	}
	return res, err
}

func (f *fakeMR) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tasks)
}

const okURL = "https://gitlab.example/grp/proj/-/merge_requests/7"

func okMR() *fakeMR { return &fakeMR{res: MRResult{IID: 7, URL: okURL, Created: true}} }

func withMR(m MRCreator) func(*Config, *env) {
	return func(c *Config, _ *env) { c.MR = m; c.Publisher = &fakePub{} }
}

// sql runs a statement on a second connection to the task database (tests
// bend a task's state this way; the store has no such door).
func (e *env) sql(q string, args ...any) {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.st.Path()+"?_pragma=busy_timeout(10000)")
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) ledgerMR(t *testing.T) []ledger.Event {
	t.Helper()
	evs, err := ledger.Events(policy.OrchLedgerPath(e.p.cfg.GateHome))
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Event
	for _, ev := range evs {
		if ev.Type == "merge_request" {
			out = append(out, ev)
		}
	}
	return out
}

func TestPublishDoneThenMRCreated(t *testing.T) {
	mr := okMR()
	e := newEnv(t, withMR(mr))
	tk := e.drain()
	if tk.State != machine.StateDone {
		t.Fatalf("state %s", tk.State)
	}
	if tk.MRState != store.MRCreated || tk.MRURL != okURL || tk.MRIID != 7 || tk.Branch != "orch/1" || tk.MRCode != "" {
		t.Fatalf("mr fields: %+v", tk)
	}
	if mr.calls() != 1 {
		t.Fatalf("calls %d", mr.calls())
	}
	var last store.Event
	for _, ev := range e.events() {
		last = ev
	}
	if last.Note != "mr_created" || last.To != machine.StateDone || last.Actor != store.ActorSystem {
		t.Fatalf("last event %+v", last)
	}
	l := e.ledgerMR(t)
	if len(l) != 1 {
		t.Fatalf("ledger merge_request events %d", len(l))
	}
	p := l[0].Payload
	if fmt.Sprint(p["task"]) != "1" || p["branch"] != "orch/1" || fmt.Sprint(p["iid"]) != "7" || p["state"] != "created" || p["code"] != "" {
		t.Fatalf("payload %+v", p)
	}
	if _, err := ledger.Verify(policy.OrchLedgerPath(e.p.cfg.GateHome)); err != nil {
		t.Fatal(err)
	}
}

func TestMRFailureNeverChangesDone(t *testing.T) {
	cases := []struct {
		name string
		mr   *fakeMR
		code string
		slow bool
	}{
		{"upstream", &fakeMR{err: &MRError{Code: "upstream_error"}}, "upstream_error", false},
		{"rejected", &fakeMR{err: &MRError{Code: "rate_limited"}}, "rate_limited", false},
		{"unavailable", &fakeMR{err: &MRError{Code: "broker_unavailable"}}, "broker_unavailable", false},
		{"hostile code", &fakeMR{err: &MRError{Code: "Bad Code: glpat-SECRET\n"}}, "upstream_error", false},
		{"plain error", &fakeMR{err: errors.New("dial tcp 10.0.0.1: token glpat-SECRET")}, "upstream_error", false},
		{"timeout", &fakeMR{block: true}, "timeout", true},
		{"panic", &fakeMR{panic: true}, "internal_error", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, withMR(c.mr))
			e.p.mrTimeout = 150 * time.Millisecond
			start := time.Now()
			tk := e.drain()
			if time.Since(start) > 5*time.Second {
				t.Fatalf("took %v", time.Since(start))
			}
			if tk.State != machine.StateDone || tk.ErrorText != "" {
				t.Fatalf("task not done: %s %q", tk.State, tk.ErrorText)
			}
			if tk.MRState != store.MRFailed || tk.MRCode != c.code || tk.MRURL != "" || tk.MRIID != 0 {
				t.Fatalf("mr fields: %+v", tk)
			}
			for _, ev := range e.events() {
				if strings.Contains(ev.Note, "SECRET") || strings.Contains(ev.Note, "glpat") {
					t.Fatalf("event leaks: %+v", ev)
				}
			}
			for _, ev := range e.ledgerMR(t) {
				if ev.Payload["state"] != "failed" || ev.Payload["code"] != c.code {
					t.Fatalf("ledger %+v", ev.Payload)
				}
			}
			if len(e.ledgerMR(t)) != 1 {
				t.Fatal("no ledger record of the failure")
			}
		})
	}
}

func TestMRSkippedWhenNotConfiguredOrNotMapped(t *testing.T) {
	// Not configured: no broker at all.
	e := newEnv(t, func(c *Config, _ *env) { c.Publisher = &fakePub{} })
	tk := e.drain()
	if tk.State != machine.StateDone || tk.MRState != store.MRSkipped || tk.MRCode != "" || tk.Branch != "" {
		t.Fatalf("unconfigured: %+v", tk)
	}
	n := 0
	for range e.events() {
		n++
	}
	if n != 8 { // queued .. done, no extra history row for a skip
		t.Fatalf("events %d", n)
	}
	// Configured but the repository is not mapped (the broker's fixed answer).
	mr := &fakeMR{err: &MRError{Code: "repo_not_mapped"}}
	e2 := newEnv(t, withMR(mr))
	tk = e2.drain()
	if tk.State != machine.StateDone || tk.MRState != store.MRSkipped || tk.MRCode != "" || mr.calls() != 1 {
		t.Fatalf("unmapped: %+v calls %d", tk, mr.calls())
	}
}

func TestMRHookRunsOnceAndOnlyAfterSuccessfulPublish(t *testing.T) {
	// Publish fails (infrastructure) -> failed, never done: no MR call.
	mr := okMR()
	e := newEnv(t, func(c *Config, _ *env) { c.MR = mr; c.Publisher = &fakePub{err: errors.New("push down")} })
	tk := e.drain()
	if tk.State != machine.StateFailed || mr.calls() != 0 || tk.MRState != "" {
		t.Fatalf("publish failure: %s calls %d mr %q", tk.State, mr.calls(), tk.MRState)
	}
	// Verdict failure.
	mr = okMR()
	e = newEnv(t, func(c *Config, _ *env) {
		c.MR = mr
		c.Gate = func(_ context.Context, o gate.Options) (report.Report, error) {
			return report.Report{SchemaVersion: 1, Category: rules.CategoryBehavior, Repo: o.Repo, Base: o.Base, Head: o.Head}, nil
		}
	})
	tk = e.drain()
	if tk.State == machine.StateDone || mr.calls() != 0 {
		t.Fatalf("verdict failure: %s calls %d", tk.State, mr.calls())
	}
	// Cancelled before it reached publishing.
	mr = okMR()
	e = newEnv(t, withMR(mr))
	if err := e.m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Cancel(context.Background(), e.id); err != nil {
		t.Fatal(err)
	}
	tk = e.drain()
	if tk.State != machine.StateCancelled || mr.calls() != 0 {
		t.Fatalf("cancel: %s calls %d", tk.State, mr.calls())
	}
	// Publishing fails once, the machine retries the step and the task ends
	// done: exactly one MR. Then a second trip through publishing (a crash
	// recovery, or any republish) creates no second one.
	mr = okMR()
	pub := &flakyPub{failFirst: 1}
	e = newEnv(t, func(c *Config, _ *env) { c.MR = mr; c.Publisher = pub })
	tk = e.drain()
	if tk.State != machine.StateDone || tk.MRState != store.MRCreated || mr.calls() != 1 || pub.n != 2 {
		t.Fatalf("flaky publish: %s mr %q calls %d publishes %d", tk.State, tk.MRState, mr.calls(), pub.n)
	}
	e.sql(`UPDATE tasks SET state='publishing', running=0 WHERE id=?`, e.id)
	tk = e.drain()
	if tk.State != machine.StateDone || tk.MRState != store.MRCreated || mr.calls() != 1 || tk.MRIID != 7 {
		t.Fatalf("republished: %s mr %q calls %d", tk.State, tk.MRState, mr.calls())
	}
	if n := len(e.ledgerMR(t)); n != 1 {
		t.Fatalf("ledger merge_request events %d", n)
	}
	// Same for a failed first attempt: a republish does not retry on its own.
	bad := &fakeMR{err: &MRError{Code: "upstream_error"}}
	e = newEnv(t, withMR(bad))
	e.drain()
	e.sql(`UPDATE tasks SET state='publishing', running=0 WHERE id=?`, e.id)
	tk = e.drain()
	if tk.MRState != store.MRFailed || bad.calls() != 1 {
		t.Fatalf("failed then republished: %q calls %d", tk.MRState, bad.calls())
	}
}

type flakyPub struct {
	mu        sync.Mutex
	failFirst int
	n         int
}

func (f *flakyPub) Publish(context.Context, store.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n <= f.failFirst {
		return errors.New("push down")
	}
	return nil
}

func TestMRURLFromBrokerIsStoredVerbatimButOnlyHTTPish(t *testing.T) {
	good := []string{"http://10.0.0.5:8929/g/p/-/merge_requests/3", "https://gitlab.example/g/p/-/merge_requests/3?x=1#y"}
	for _, u := range good {
		e := newEnv(t, withMR(&fakeMR{res: MRResult{IID: 3, URL: u, Created: true}}))
		tk := e.drain()
		if tk.MRState != store.MRCreated || tk.MRURL != u {
			t.Fatalf("%q: %+v", u, tk)
		}
	}
	bad := []string{"javascript:alert(1)", "ftp://x/y", "//evil/x", "/relative", "", "https:///nohost", "https://x/ y",
		"https://x/\n", "data:text/html,x", "HTTP-ish", "https://x/" + strings.Repeat("a", 3000), "https://u:p@x/y", "https://x/\x00"}
	for _, u := range bad {
		e := newEnv(t, withMR(&fakeMR{res: MRResult{IID: 3, URL: u, Created: true}}))
		tk := e.drain()
		if tk.State != machine.StateDone || tk.MRState != store.MRFailed || tk.MRCode != "bad_url" || tk.MRURL != "" || tk.MRIID != 0 {
			t.Fatalf("%q stored: %+v", u, tk)
		}
	}
	// A non-positive iid is not a created MR either.
	e := newEnv(t, withMR(&fakeMR{res: MRResult{IID: 0, URL: okURL, Created: true}}))
	if tk := e.drain(); tk.MRState != store.MRFailed || tk.MRCode != "upstream_error" || tk.MRURL != "" {
		t.Fatalf("iid 0: %+v", tk)
	}
}

func TestMRActorsAndLedger(t *testing.T) {
	mr := &fakeMR{err: &MRError{Code: "upstream_error"}}
	e := newEnv(t, withMR(mr))
	e.drain()
	// The hook's event is the system's.
	evs := e.events()
	last := evs[len(evs)-1]
	if last.Note != "mr_failed: upstream_error" || last.Actor != store.ActorSystem {
		t.Fatalf("hook event %+v", last)
	}
	if mr.actors[0] != store.ActorSystem {
		t.Fatalf("hook ctx actor %q", mr.actors[0])
	}
	// The retry endpoint's event carries the requesting actor.
	mr.mu.Lock()
	mr.err, mr.res = nil, MRResult{IID: 9, URL: okURL, Created: true}
	mr.mu.Unlock()
	tk, err := e.p.RetryMR(store.WithActor(context.Background(), "session:mac-passkey"), e.id)
	if err != nil {
		t.Fatal(err)
	}
	if tk.MRState != store.MRCreated || tk.MRIID != 9 {
		t.Fatalf("retry result %+v", tk)
	}
	evs = e.events()
	last = evs[len(evs)-1]
	if last.Note != "mr_created" || last.Actor != "session:mac-passkey" || mr.actors[1] != "session:mac-passkey" {
		t.Fatalf("retry event %+v actors %v", last, mr.actors)
	}
	l := e.ledgerMR(t)
	if len(l) != 2 || l[0].Payload["actor"] != "system" || l[1].Payload["actor"] != "session:mac-passkey" ||
		l[0].Payload["state"] != "failed" || l[0].Payload["code"] != "upstream_error" || l[1].Payload["state"] != "created" {
		t.Fatalf("ledger %+v", l)
	}
	if _, err := ledger.Verify(policy.OrchLedgerPath(e.p.cfg.GateHome)); err != nil {
		t.Fatal(err)
	}
	// Created already: a second retry is refused without calling the broker.
	if _, err := e.p.RetryMR(context.Background(), e.id); !errors.Is(err, ErrMRAlreadyCreated) || mr.calls() != 2 {
		t.Fatalf("second retry: %v calls %d", err, mr.calls())
	}
}

func TestRetryMRRules(t *testing.T) {
	// Not configured, not done, never attempted: not applicable.
	e := newEnv(t, func(c *Config, _ *env) { c.Publisher = &fakePub{} })
	e.drain() // skipped, no broker
	if _, err := e.p.RetryMR(context.Background(), e.id); !errors.Is(err, ErrMRNotApplicable) {
		t.Fatalf("no broker: %v", err)
	}
	mr := okMR()
	e = newEnv(t, withMR(mr))
	if _, err := e.p.RetryMR(context.Background(), e.id); !errors.Is(err, ErrMRNotApplicable) { // queued
		t.Fatalf("not done: %v", err)
	}
	if _, err := e.p.RetryMR(context.Background(), 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// A done task with a skipped MR: the broker may know the repo now.
	e.drain()
	e.sql(`UPDATE tasks SET mr_state='skipped', mr_iid=NULL, mr_url=NULL, branch=NULL WHERE id=?`, e.id)
	if tk, err := e.p.RetryMR(context.Background(), e.id); err != nil || tk.MRState != store.MRCreated {
		t.Fatalf("skipped retry: %+v %v", tk, err)
	}
	// Still unmapped: stays skipped, 409-class error.
	un := &fakeMR{err: &MRError{Code: "repo_not_mapped"}}
	e = newEnv(t, withMR(un))
	e.drain()
	if _, err := e.p.RetryMR(context.Background(), e.id); !errors.Is(err, ErrMRNotApplicable) {
		t.Fatalf("unmapped retry: %v", err)
	}
	// Broker failure: the typed error carries only the fixed code, state failed.
	un.mu.Lock()
	un.err = &MRError{Code: "bad_url"}
	un.mu.Unlock()
	e.sql(`UPDATE tasks SET mr_state='failed', mr_code='upstream_error' WHERE id=?`, e.id)
	tk, err := e.p.RetryMR(context.Background(), e.id)
	var me *MRError
	if !errors.As(err, &me) || me.Code != "bad_url" || tk.MRState != store.MRFailed || tk.MRCode != "bad_url" {
		t.Fatalf("failed retry: %+v %v", tk, err)
	}
}

// brokerFake is a unix-socket broker answering create_mr; it records the args.
func brokerFake(t *testing.T, reply string) (string, func() []map[string]any) {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "mrb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	sock := filepath.Join(d, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var got []map[string]any
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadBytes('\n')
				var req struct {
					Op   string         `json:"op"`
					Args map[string]any `json:"args"`
				}
				json.Unmarshal(line, &req)
				mu.Lock()
				if req.Op == "create_mr" {
					got = append(got, req.Args)
				}
				mu.Unlock()
				c.Write([]byte(reply + "\n"))
			}()
		}
	}()
	return sock, func() []map[string]any { mu.Lock(); defer mu.Unlock(); return append([]map[string]any(nil), got...) }
}

func TestBrokerMRSendsExactArgs(t *testing.T) {
	sock, got := brokerFake(t, `{"ok":true,"result":{"iid":12,"url":"http://gl/g/p/-/merge_requests/12","created":true}}`)
	prompt := "Fix the login bug\r\nsecond line with detail\r\n\r\n" + strings.Repeat("x", 600)
	c := &BrokerMR{Client: &broker.Client{Socket: sock}}
	tk := store.Task{ID: 42, Repo: "/srv/repo", Prompt: prompt, HeadSHA: fakeHead, Verdict: "ok"}
	res, err := c.CreateMR(store.WithActor(context.Background(), "session:mac-passkey"), tk)
	if err != nil || res.IID != 12 || res.URL != "http://gl/g/p/-/merge_requests/12" || !res.Created {
		t.Fatalf("%+v %v", res, err)
	}
	args := got()
	if len(args) != 1 {
		t.Fatalf("calls %d", len(args))
	}
	a := args[0]
	if a["repo"] != "/srv/repo" || a["branch"] != "orch/42" || a["task"] != float64(42) ||
		a["actor"] != "session:mac-passkey" || a["verdict"] != "ok" || a["head"] != fakeHead {
		t.Fatalf("args %+v", a)
	}
	if len(a) != 8 {
		t.Fatalf("extra keys: %+v", a)
	}
	hint, sum := a["title_hint"].(string), a["summary"].(string)
	if hint != "Fix the login bug" || strings.ContainsAny(hint+sum, "\r\n") || len([]rune(sum)) > 400 || len([]rune(hint)) > 120 || sum == "" {
		t.Fatalf("hint %q summary %q", hint, sum)
	}
	// The system actor becomes kind:id; a long first line is capped.
	tk.Prompt = strings.Repeat("é", 500)
	if _, err := c.CreateMR(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	a = got()[1]
	if a["actor"] != "system:orchd" || len([]rune(a["title_hint"].(string))) != 120 {
		t.Fatalf("args %+v", a)
	}
	// A head that is not 40 hex is refused before any call.
	tk.HeadSHA = "abc"
	if _, err := c.CreateMR(context.Background(), tk); err == nil || len(got()) != 2 {
		t.Fatalf("bad head: %v calls %d", err, len(got()))
	}
}

func TestBrokerMRMapsCodes(t *testing.T) {
	cases := map[string]string{
		`{"ok":false,"code":"repo_not_mapped","message":"glpat-SECRET"}`: "repo_not_mapped",
		`{"ok":false,"code":"bad_url","message":"x"}`:                    "bad_url",
		`{"ok":false,"code":"Evil Code","message":"x"}`:                  "broker_unavailable",
		`garbage`: "broker_unavailable",
	}
	for reply, want := range cases {
		sock, _ := brokerFake(t, reply)
		c := &BrokerMR{Client: &broker.Client{Socket: sock}}
		_, err := c.CreateMR(context.Background(), store.Task{ID: 1, Repo: "/r", Prompt: "p", HeadSHA: fakeHead, Verdict: "ok"})
		var me *MRError
		if !errors.As(err, &me) || me.Code != want || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("%s -> %v", reply, err)
		}
	}
	// repo_not_mapped is what makes the hook record skipped.
	if (&MRError{Code: "repo_not_mapped"}).Error() == "" {
		t.Fatal("empty error text")
	}
}
