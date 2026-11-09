package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
)

type mrFake struct {
	mu    sync.Mutex
	calls int
	res   steps.MRResult
	err   error
}

func (f *mrFake) CreateMR(_ context.Context, t store.Task) (steps.MRResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.res, f.err
}

func (f *mrFake) set(res steps.MRResult, err error) {
	f.mu.Lock()
	f.res, f.err = res, err
	f.mu.Unlock()
}

// mrProv is a StepProvider that forwards RetryMR to a real steps.Provider
// built on the env's own store (made after the env, see newMREnv).
type mrProv struct {
	noSteps
	p *steps.Provider
}

func (m *mrProv) RetryMR(ctx context.Context, id int64) (store.Task, error) {
	return m.p.RetryMR(ctx, id)
}

type mrEnv struct {
	*env
	fake *mrFake
	prov *mrProv
}

func newMREnv(t *testing.T, withMR bool) *mrEnv {
	t.Helper()
	prov := &mrProv{}
	e := newEnv(t, prov, 1)
	e.d.pauseWorkers()
	cfg := steps.DefaultConfig()
	cfg.Agent, cfg.Test = nopAgent{}, nopTest{}
	cfg.ReportDir, cfg.GateHome = t.TempDir(), t.TempDir()
	fake := &mrFake{res: steps.MRResult{IID: 7, URL: "https://gl.example/g/p/-/merge_requests/7", Created: true}}
	if withMR {
		cfg.MR = fake
	}
	p, err := steps.NewProvider(e.st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	prov.p = p
	return &mrEnv{env: e, fake: fake, prov: prov}
}

// doneTask makes a done task with the given merge request state.
func (e *mrEnv) doneTask(t *testing.T, mrState, mrCode string) int64 {
	t.Helper()
	id, err := e.st.CreateTask(context.Background(), store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET state='done', head_sha=?, verdict='ok', mr_state=NULLIF(?,''), mr_code=NULLIF(?,'') WHERE id=?`,
		strings.Repeat("a", 40), mrState, mrCode, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func mrPath(id int64) string { return fmt.Sprintf("/api/v1/tasks/%d/mr", id) }

func TestMRRetryEndpointRules(t *testing.T) {
	e := newMREnv(t, true)
	post := func(id int64) (int, []byte) {
		c, _, b := e.sockDo(t, "POST", mrPath(id), "", nil)
		return c, b
	}
	// Not applicable: unknown task, queued task, done without any attempt.
	if c, b := post(999); c != 404 {
		t.Fatalf("unknown: %d %s", c, b)
	}
	queued, _ := e.st.CreateTask(context.Background(), store.Task{Repo: e.repo, Base: e.sha, Prompt: "p"})
	if c, b := post(queued); c != 409 || v1Code(b) != "mr_not_applicable" {
		t.Fatalf("queued: %d %s", c, b)
	}
	fresh := e.doneTask(t, "", "")
	if c, b := post(fresh); c != 409 || v1Code(b) != "mr_not_applicable" {
		t.Fatalf("never attempted: %d %s", c, b)
	}
	if e.fake.calls != 0 {
		t.Fatalf("broker called for refused requests: %d", e.fake.calls)
	}
	// Broker failure: 502 upstream_error with only the fixed code, state failed.
	failed := e.doneTask(t, "failed", "upstream_error")
	e.fake.set(steps.MRResult{}, &steps.MRError{Code: "rate_limited"})
	c, b := post(failed)
	if c != 502 || v1Code(b) != "upstream_error" || strings.Contains(string(b), "glpat") {
		t.Fatalf("broker failure: %d %s", c, b)
	}
	if tk, _ := e.st.GetTask(context.Background(), failed); tk.MRState != store.MRFailed || tk.MRCode != "rate_limited" || tk.State != store.State("done") {
		t.Fatalf("after failure: %+v", tk)
	}
	// Success: 200 TaskView with the MR fields.
	e.fake.set(steps.MRResult{IID: 7, URL: "https://gl.example/g/p/-/merge_requests/7", Created: true}, nil)
	c, b = post(failed)
	var v TaskView
	if c != 200 || json.Unmarshal(b, &v) != nil || v.MRState != "created" || v.MRIID != 7 || v.MRURL == "" || v.Branch != fmt.Sprintf("orch/%d", failed) || v.State != "done" {
		t.Fatalf("success: %d %s", c, b)
	}
	// Already created: 409 and the broker is not called again.
	n := e.fake.calls
	if c, b := post(failed); c != 409 || v1Code(b) != "mr_already_created" || e.fake.calls != n {
		t.Fatalf("again: %d %s calls %d->%d", c, b, n, e.fake.calls)
	}
	// Skipped task whose repository is still not mapped: 409 not applicable.
	skipped := e.doneTask(t, "skipped", "")
	e.fake.set(steps.MRResult{}, &steps.MRError{Code: "repo_not_mapped"})
	if c, b := post(skipped); c != 409 || v1Code(b) != "mr_not_applicable" {
		t.Fatalf("unmapped: %d %s", c, b)
	}
	// No broker configured at all: not applicable, and no panic.
	e2 := newMREnv(t, false)
	id := e2.doneTask(t, "skipped", "")
	if c, _, b := e2.sockDo(t, "POST", mrPath(id), "", nil); c != 409 || v1Code(b) != "mr_not_applicable" {
		t.Fatalf("no broker: %d %s", c, b)
	}
	// A provider without the capability behaves the same.
	e3 := newEnv(t, noSteps{}, 1)
	if c, _, b := e3.sockDo(t, "POST", mrPath(1), "", nil); c != 409 || v1Code(b) != "mr_not_applicable" {
		t.Fatalf("no provider support: %d %s", c, b)
	}
}

func TestMRRetryRequiresWriteTier(t *testing.T) {
	e := newMREnv(t, true)
	id := e.doneTask(t, "failed", "upstream_error")
	srv := e.httpServer(t)
	path := mrPath(id)
	if code, _, b := httpDo(t, srv, "POST", path, "", nil); code != 401 {
		t.Errorf("anonymous: %d %s", code, b)
	}
	tok, csrf := e.newSession(t, "alice")
	if code, _, b := httpDo(t, srv, "POST", path, "", hdr{"Cookie": cookie(tok)}); code != 403 {
		t.Errorf("session without CSRF: %d %s", code, b)
	}
	if code, _, b := httpDo(t, srv, "POST", path, "", hdr{"Cookie": cookie(tok), CSRFHeader: "wrong"}); code != 403 {
		t.Errorf("wrong CSRF: %d %s", code, b)
	}
	if e.fake.calls != 0 {
		t.Fatalf("refused requests reached the broker: %d", e.fake.calls)
	}
	if code, _, b := e.sockDo(t, "GET", path, "", nil); code != 405 && code != 404 {
		t.Errorf("GET: %d %s", code, b)
	}
	// With the CSRF header it goes through, and the event names the session.
	code, _, b := httpDo(t, srv, "POST", path, "", hdr{"Cookie": cookie(tok), CSRFHeader: csrf})
	if code != 200 {
		t.Fatalf("session + CSRF: %d %s", code, b)
	}
	evs, _ := e.st.Events(context.Background(), id)
	last := evs[len(evs)-1]
	if last.Note != "mr_created" || last.Actor != "session:alice" {
		t.Fatalf("event %+v", last)
	}
	if e.fake.calls != 1 {
		t.Fatalf("calls %d", e.fake.calls)
	}
}

func TestTaskViewCarriesMRFields(t *testing.T) {
	e := newMREnv(t, true)
	id := e.doneTask(t, "", "")
	get := func() map[string]any {
		c, _, b := e.sockDo(t, "GET", fmt.Sprintf("/api/v1/tasks/%d", id), "", nil)
		if c != 200 {
			t.Fatalf("get %d %s", c, b)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	// Nothing recorded: the fields are absent (omitempty), never null garbage.
	m := get()
	for _, k := range []string{"branch", "mr_url", "mr_iid", "mr_state", "mr_code"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%s present before any MR: %v", k, m)
		}
	}
	if err := e.st.SetMR(context.Background(), id, "orch/1", 7, "https://gl.example/g/p/-/merge_requests/7", store.MRCreated, ""); err != nil {
		t.Fatal(err)
	}
	m = get()
	if m["branch"] != "orch/1" || m["mr_state"] != "created" || m["mr_iid"] != float64(7) || m["mr_url"] != "https://gl.example/g/p/-/merge_requests/7" {
		t.Fatalf("created view %v", m)
	}
	if _, ok := m["mr_code"]; ok {
		t.Fatalf("mr_code on a created MR: %v", m)
	}
	id2 := e.doneTask(t, "", "")
	if err := e.st.SetMR(context.Background(), id2, "orch/2", 0, "", store.MRFailed, "bad_url"); err != nil {
		t.Fatal(err)
	}
	c, _, b := e.sockDo(t, "GET", fmt.Sprintf("/api/v1/tasks/%d", id2), "", nil)
	var v TaskView
	if c != 200 || json.Unmarshal(b, &v) != nil || v.MRState != "failed" || v.MRCode != "bad_url" || v.MRURL != "" || v.MRIID != 0 {
		t.Fatalf("failed view %d %s", c, b)
	}
	// The list carries them too.
	c, _, b = e.sockDo(t, "GET", "/api/v1/tasks", "", nil)
	if c != 200 || !strings.Contains(string(b), `"mr_state":"failed"`) || !strings.Contains(string(b), `"mr_state":"created"`) {
		t.Fatalf("list %d %s", c, b)
	}
	_ = errors.New
	_ = http.StatusOK
}
