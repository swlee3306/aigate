package api

// Adversarial suite for W2a (Task 6): submit, cancel/retry, the actor, the
// queue limit, /repos and ops.cloud, attacked through the daemon's real
// listeners (the loopback HTTP listener with its whole middleware chain, and
// the unix socket) unless a test says otherwise. Every test keeps an accepted
// control next to its refusals and checks that a refusal changed nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
)

// try is one HTTP request that never calls t.Fatal, so goroutines can use it;
// the caller checks err after Wait.
func try(hc *http.Client, method, url, host string, body []byte, h hdr) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// sessionHdr is what the console sends for a write: cookie, Origin, CSRF.
func sessionHdr(tok, csrf string) hdr {
	return hdr{"Content-Type": "application/json", "Cookie": cookie(tok), "Origin": webOrigin, CSRFHeader: csrf}
}

// taskSnap is everything a cancel or retry could change on a task.
type taskSnap struct {
	State   store.State
	Updated time.Time
	Att     int
	Retries int
	Events  int
	Actor   string
}

func snapAll(t *testing.T, e *env) map[int64]taskSnap {
	t.Helper()
	ts, err := e.st.ListTasks(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]taskSnap{}
	for _, x := range ts {
		evs, err := e.st.Events(context.Background(), x.ID)
		if err != nil {
			t.Fatal(err)
		}
		out[x.ID] = taskSnap{x.State, x.UpdatedAt, x.Attempts, x.Retries, len(evs), x.LastActor}
	}
	return out
}

// sameExcept fails when any task other than the ones in changed differs
// between before and after (or a task appeared or vanished).
func sameExcept(t *testing.T, what string, before, after map[int64]taskSnap, changed ...int64) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s: %d tasks before, %d after", what, len(before), len(after))
	}
	skip := map[int64]bool{}
	for _, id := range changed {
		skip[id] = true
	}
	for id, b := range before {
		if skip[id] {
			continue
		}
		if a, ok := after[id]; !ok || a != b {
			t.Fatalf("%s: task %d changed: %+v -> %+v", what, id, b, a)
		}
	}
}

// Submit, cancel and retry on the HTTP listener: no credentials is 401; a
// session without CSRF, with a wrong CSRF or without an allowed Origin is
// 403; an unknown or garbage credential is 401. None of them creates or
// changes a task. Controls: the same three writes with a full session.
func TestSubmitNeedsAuthAndCSRF(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	q := queuedTask(t, w.env)
	f := failedTask(t, w.env, store.StateVerifying, true)
	tok, csrf := w.newSession(t, "alice")
	good := sessionHdr(tok, csrf)
	writes := []struct{ name, path, body string }{
		{"submit", "/api/v1/tasks", w.submitBody("p")},
		{"cancel", "/api/v1/tasks/" + itoa(q) + "/cancel", ""},
		{"retry", "/api/v1/tasks/" + itoa(f) + "/retry", ""},
	}
	cases := []struct {
		name   string
		h      hdr
		status int
		codes  []string // accepted v1 codes (legacy route answers flat)
	}{
		{"no credentials", jsonCT, 401, []string{"unauthenticated"}},
		{"unknown session cookie", with(good, hdr{"Cookie": cookie("sess-nobody")}), 401, []string{"unauthenticated"}},
		{"garbage bearer", with(jsonCT, hdr{"Authorization": "Bearer x"}), 401, []string{"unauthenticated"}},
		{"basic auth", with(jsonCT, hdr{"Authorization": "Basic YWxpY2U6YWxpY2U="}), 401, []string{"unauthenticated"}},
		{"session as bearer", with(jsonCT, hdr{"Authorization": "Bearer " + tok}), 401, []string{"unauthenticated"}},
		{"cookie, no CSRF", with(jsonCT, hdr{"Cookie": cookie(tok), "Origin": webOrigin}), 403, []string{"csrf_failed"}},
		{"cookie, wrong CSRF", with(good, hdr{CSRFHeader: "csrf-x"}), 403, []string{"csrf_failed"}},
		{"cookie, foreign Origin", with(good, hdr{"Origin": "https://evil.example"}), 403, []string{"bad_origin"}},
	}
	before := snapAll(t, w.env)
	for _, c := range cases {
		for _, wr := range writes {
			code, _, b := w.web(t, "POST", wr.path, wr.body, c.h)
			got := v1Code(b)
			if got == "" {
				got = flatCode(b)
			}
			ok := code == c.status
			if ok && got != "" {
				ok = false
				for _, want := range c.codes {
					ok = ok || got == want
				}
			}
			if !ok {
				t.Fatalf("%s / %s: %d %s", c.name, wr.name, code, b)
			}
		}
	}
	sameExcept(t, "refused writes", before, snapAll(t, w.env))
	// The legacy flat route is not mounted on the HTTP listener at all.
	if code, _, b := w.web(t, "POST", "/tasks", w.submitBody("p"), good); code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
		t.Fatalf("legacy route over HTTP: %d %s", code, b)
	}
	sameExcept(t, "legacy route", before, snapAll(t, w.env))
	// Controls: the same writes with a full session succeed.
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/tasks", w.submitBody("p"), 201},
		{"/api/v1/tasks/" + itoa(q) + "/cancel", "", 200},
		{"/api/v1/tasks/" + itoa(f) + "/retry", "", 200},
	} {
		if code, _, b := w.web(t, "POST", c.path, c.body, good); code != c.want {
			t.Fatalf("control %s: %d %s", c.path, code, b)
		}
	}
}

// Hostile submit fields are refused with a fixed message and create nothing.
func TestSubmitHostileFields(t *testing.T) {
	w := newWebEnv(t, withPolicyClasses(map[string]string{"docs": "L2", "test": "L1"}))
	w.d.pauseWorkers()
	outerSHA, x, xSHA := outerSetup(t, w.env) // the directory around the root is a repo; X is outside
	bearer := hdr{"Content-Type": "application/json", "Authorization": "Bearer " + w.newToken(t, "alice")}

	link := filepath.Join(w.root, "link-out")
	if err := os.Symlink(x, link); err != nil {
		t.Fatal(err)
	}
	hollow := filepath.Join(w.root, "hollow") // empty .git: git would walk up to the outer repo
	if err := os.MkdirAll(filepath.Join(hollow, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := func(repo, base, prompt, class string) string {
		b, _ := json.Marshal(map[string]string{"repo": repo, "base": base, "prompt": prompt, "class": class, "agent": "claude"})
		return string(b)
	}
	exact := strings.Repeat("a", MaxPromptBytes)
	cases := []struct {
		name   string
		raw    string
		status int
		code   string
	}{
		{"dotdot escape", body(w.root+"/../elsewhere/x", "HEAD", "p", "docs"), 400, "bad_request"},
		{"dotdot inside", body(w.repo+"/../proj", "HEAD", "p", "docs"), 400, "bad_request"},
		{"absolute outside", body(x, "HEAD", "p", "docs"), 403, "forbidden"},
		{"relative path", body("proj", "HEAD", "p", "docs"), 400, "bad_request"},
		{"symlink out of the roots", body(link, "HEAD", "p", "docs"), 403, "forbidden"},
		{"empty .git walks up (R8)", body(hollow, "HEAD", "p", "docs"), 400, "bad_request"},
		{"subdirectory of a repo", body(filepath.Join(w.repo, "sub"), "HEAD", "p", "docs"), 400, "bad_request"},
		{"base --upload-pack", body(w.repo, "--upload-pack=x", "p", "docs"), 400, "bad_request"},
		{"base -x", body(w.repo, "-x", "p", "docs"), 400, "bad_request"},
		{"base with a space option", body(w.repo, "HEAD --upload-pack=x", "p", "docs"), 400, "bad_request"},
		{"base with NUL", body(w.repo, "HEAD\x00", "p", "docs"), 400, "bad_request"},
		{"prompt 64 KiB + 1", body(w.repo, "HEAD", exact+"b", "docs"), 400, "bad_request"},
		{"prompt with NUL", body(w.repo, "HEAD", "a\x00b", "docs"), 400, "bad_request"},
		{"prompt with raw invalid UTF-8", `{"repo":"` + w.repo + `","base":"HEAD","prompt":"a` + "\xff\xfe" + `b","class":"docs","agent":"claude"}`, 400, "bad_request"},
		{"class 33 chars", body(w.repo, "HEAD", "p", strings.Repeat("c", 33)), 400, "bad_request"},
		{"class 32 chars, not in policy", body(w.repo, "HEAD", "p", strings.Repeat("c", 32)), 400, "unknown_class"},
		{"class not in policy", body(w.repo, "HEAD", "p", "deploy"), 400, "unknown_class"},
		{"class with newline", body(w.repo, "HEAD", "p", "docs\n"), 400, "bad_request"},
		{"class uppercase", body(w.repo, "HEAD", "p", "DOCS"), 400, "bad_request"},
		{"agent not allowed", `{"repo":"` + w.repo + `","base":"HEAD","prompt":"p","class":"docs","agent":"sh"}`, 400, "bad_request"},
		{"unknown field", `{"repo":"` + w.repo + `","base":"HEAD","prompt":"p","class":"docs","agent":"claude","x":1}`, 400, "bad_request"},
	}
	for _, c := range cases {
		code, _, b := w.web(t, "POST", "/api/v1/tasks", c.raw, bearer)
		if code != c.status || v1Code(b) != c.code {
			t.Fatalf("%s: %d %.300s", c.name, code, b)
		}
		for _, s := range []string{outerSHA, xSHA, "outersecretbranch", "xsecretbranch", "elsewhere"} {
			if strings.Contains(string(b), s) {
				t.Fatalf("%s: response leaks %q: %s", c.name, s, b)
			}
		}
	}
	if n := w.taskCount(t); n != 0 {
		t.Fatalf("hostile fields created %d tasks", n)
	}
	// Controls: exactly 64 KiB is accepted, and so is a 32-character class
	// the policy lists.
	code, _, b := w.web(t, "POST", "/api/v1/tasks", body(w.repo, "HEAD", exact, "docs"), bearer)
	var v TaskView
	if code != 201 || json.Unmarshal(b, &v) != nil || len(v.Prompt) != MaxPromptBytes {
		t.Fatalf("64 KiB prompt: %d %.200s", code, b)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", body(w.repo, "HEAD", "p", "test"), bearer); code != 201 {
		t.Fatalf("control: %d %s", code, b)
	}
	// An escaped lone surrogate is valid JSON; Go decodes it to U+FFFD, so the
	// stored prompt is valid UTF-8 without the surrogate (recorded behaviour,
	// not a refusal).
	code, _, b = w.web(t, "POST", "/api/v1/tasks", `{"repo":"`+w.repo+`","base":"HEAD","prompt":"a\ud800b","class":"docs","agent":"claude"}`, bearer)
	if code != 201 || json.Unmarshal(b, &v) != nil || v.Prompt != "a�b" {
		t.Fatalf("lone surrogate escape: %d %s", code, b)
	}
}

// adversaryEnv is a web env whose steps provider is a real steps.Provider
// writing the orchd ledger under home (workers paused: no step runs).
func adversaryEnv(t *testing.T, opts ...func(*Config)) (*webEnv, string) {
	t.Helper()
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
	all := append([]func(*Config){withHTTP, func(c *Config) { c.GateHome = home }}, opts...)
	e := newEnv(t, p, 1, all...)
	e.d.pauseWorkers()
	return &webEnv{env: e, addr: e.d.HTTPAddr().String(), hc: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}, home
}

// No header or body field a client sends changes the recorded actor: the
// actor comes only from the authenticated principal. Checked in the stored
// event, the task view and the orchd ledger (task_submitted, task_transition).
func TestForgedActorHeaderIgnored(t *testing.T) {
	w, home := adversaryEnv(t)
	tok, csrf := w.newSession(t, "alice")
	bearer := w.newToken(t, "mac-ssh")
	forged := hdr{
		"X-Actor":              "socket:uid:0",
		"X-Orch-Actor":         "system",
		"X-Forwarded-User":     "root",
		"X-Remote-User":        "root",
		"Remote-User":          "root",
		"X-Webauth-User":       "root",
		"Tailscale-User-Login": "admin@evil.example",
		"Tailscale-User-Name":  "admin",
		"X-Principal":          "token:root",
	}
	viaHTTP := func(m, path, body string, h hdr) (int, []byte) { c, _, b := w.web(t, m, path, body, h); return c, b }
	viaSocket := func(m, path, body string, h hdr) (int, []byte) { c, _, b := w.sockDo(t, m, path, body, h); return c, b }
	sock := socketActor() // the real socket principal: socket:uid:<this process's uid>
	principals := []struct {
		name, want string
		h          hdr
		send       func(method, path, body string, h hdr) (int, []byte)
	}{
		{"session", "session:alice", sessionHdr(tok, csrf), viaHTTP},
		{"token", "token:mac-ssh", hdr{"Content-Type": "application/json", "Authorization": "Bearer " + bearer}, viaHTTP},
		// A uid-allowlisted socket user must not be able to forge the actor either.
		{"socket", sock, hdr{"Content-Type": "application/json"}, viaSocket},
	}
	for _, pc := range principals {
		h := with(pc.h, forged)
		code, b := pc.send("POST", "/api/v1/tasks", w.submitBody("p-"+pc.name), h)
		var v TaskView
		if code != 201 || json.Unmarshal(b, &v) != nil || v.Actor != pc.want {
			t.Fatalf("%s submit: %d %s", pc.name, code, b)
		}
		if a := lastEventActor(t, w.env, v.ID); a != pc.want {
			t.Fatalf("%s submit event actor %q", pc.name, a)
		}
		// cancel with the same forged headers
		code, b = pc.send("POST", "/api/v1/tasks/"+itoa(v.ID)+"/cancel", "", h)
		if code != 200 || json.Unmarshal(b, &v) != nil || v.Actor != pc.want {
			t.Fatalf("%s cancel: %d %s", pc.name, code, b)
		}
		if a := lastEventActor(t, w.env, v.ID); a != pc.want {
			t.Fatalf("%s cancel event actor %q", pc.name, a)
		}
		// retry with the same forged headers
		f := failedTask(t, w.env, store.StateVerifying, true)
		code, b = pc.send("POST", "/api/v1/tasks/"+itoa(f)+"/retry", "", h)
		if code != 200 || json.Unmarshal(b, &v) != nil || v.Actor != pc.want {
			t.Fatalf("%s retry: %d %s", pc.name, code, b)
		}
		if a := lastEventActor(t, w.env, f); a != pc.want {
			t.Fatalf("%s retry event actor %q", pc.name, a)
		}
		// An "actor" field in the body is not ignored silently: the decoder
		// refuses unknown fields, so the submit is a 400 and creates nothing.
		n := w.taskCount(t)
		for _, extra := range []string{`"actor":"system"`, `"Actor":"socket:uid:0"`, `"via":"socket:uid:0"`, `"principal":"root"`} {
			raw := strings.TrimSuffix(w.submitBody("p"), "}") + "," + extra + "}"
			code, b := pc.send("POST", "/api/v1/tasks", raw, h)
			if code != 400 || v1Code(b) != "bad_request" {
				t.Fatalf("%s body %s: %d %s", pc.name, extra, code, b)
			}
		}
		if w.taskCount(t) != n {
			t.Fatalf("%s: a body with an actor field created a task", pc.name)
		}
	}
	// The ledger: every task_submitted and task_transition names the real
	// principal; no forged value appears anywhere in the file.
	lp := policy.OrchLedgerPath(home)
	raw, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"socket:uid:0", "admin", "evil", "token:root", `"actor":"system"`} {
		if s == sock {
			continue // only if the test itself runs as root
		}
		if strings.Contains(string(raw), s) {
			t.Fatalf("ledger contains forged %q", s)
		}
	}
	evs, err := ledger.Events(lp)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, ev := range evs {
		pl := ev.Payload
		a, _ := pl["actor"].(string)
		if a != "session:alice" && a != "token:mac-ssh" && a != sock {
			t.Fatalf("ledger %s actor %q: %+v", ev.Type, a, pl)
		}
		if pl["via"] != a {
			t.Fatalf("ledger %s via %v != actor %q", ev.Type, pl["via"], a)
		}
		seen[ev.Type+"@"+a]++
	}
	for _, want := range []string{"task_submitted@session:alice", "task_submitted@token:mac-ssh", "task_submitted@" + sock,
		"task_transition@session:alice", "task_transition@token:mac-ssh", "task_transition@" + sock} {
		if seen[want] == 0 {
			t.Fatalf("ledger has no %s: %v", want, seen)
		}
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

// Four principals flood the queue at once with keyed and unkeyed submits. The
// limit is global (all principals share it): exactly QueueLimit tasks are
// created, every other answer is 429 queue_full, nothing else. Cancelling a
// queued task makes room for exactly one more, from any principal.
func TestQueueFloodBoundedAcrossPrincipals(t *testing.T) {
	const limit, per = 5, 10
	w := newWebEnv(t, withQueueLimit(limit))
	w.d.pauseWorkers()
	tokA, csrfA := w.newSession(t, "alice")
	tokB, csrfB := w.newSession(t, "bob")
	bearer := w.newToken(t, "mac-ssh")
	httpURL := "http://" + w.addr + "/api/v1/tasks"
	sock := w.client
	senders := []struct {
		name string
		do   func(i int, body []byte) (int, []byte, error)
	}{
		{"alice", func(i int, b []byte) (int, []byte, error) {
			h := sessionHdr(tokA, csrfA)
			if i%2 == 0 {
				h["Idempotency-Key"] = fmt.Sprintf("alice-%d", i)
			}
			return try(w.hc, "POST", httpURL, webHost, b, h)
		}},
		{"bob", func(i int, b []byte) (int, []byte, error) {
			return try(w.hc, "POST", httpURL, webHost, b, sessionHdr(tokB, csrfB))
		}},
		{"token", func(i int, b []byte) (int, []byte, error) {
			return try(w.hc, "POST", httpURL, webHost, b, hdr{"Content-Type": "application/json", "Authorization": "Bearer " + bearer,
				"Idempotency-Key": fmt.Sprintf("tok-%d", i)})
		}},
		{"socket", func(i int, b []byte) (int, []byte, error) {
			return try(sock, "POST", "http://orchd/api/v1/tasks", "", b, nil)
		}},
	}
	type res struct {
		who  string
		code int
		body []byte
		err  error
	}
	results := make(chan res, len(senders)*per)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, s := range senders {
		for i := 0; i < per; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code, b, err := s.do(i, []byte(w.submitBody(fmt.Sprintf("%s-%d", s.name, i))))
				results <- res{s.name, code, b, err}
			}()
		}
	}
	close(start)
	wg.Wait()
	close(results)
	created, full := 0, 0
	for r := range results {
		switch {
		case r.err != nil:
			t.Fatalf("%s: %v", r.who, r.err)
		case r.code == 201:
			created++
		case r.code == 429 && v1Code(r.body) == "queue_full":
			full++
		default:
			t.Fatalf("%s: %d %s", r.who, r.code, r.body)
		}
	}
	if created != limit || full != len(senders)*per-limit {
		t.Fatalf("created %d (want %d), queue_full %d", created, limit, full)
	}
	if n, err := w.st.CountQueued(context.Background()); err != nil || n != limit {
		t.Fatalf("queued %d %v", n, err)
	}
	if n := w.taskCount(t); n != limit {
		t.Fatalf("%d tasks stored", n)
	}
	// Room for exactly one more after a cancel.
	ts, _ := w.st.ListTasks(context.Background(), 10)
	if code, _, b := w.web(t, "POST", "/api/v1/tasks/"+itoa(ts[0].ID)+"/cancel", "", sessionHdr(tokB, csrfB)); code != 200 {
		t.Fatalf("cancel: %d %s", code, b)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", w.submitBody("after"), sessionHdr(tokA, csrfA)); code != 201 {
		t.Fatalf("after cancel: %d %s", code, b)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", w.submitBody("after2"), sessionHdr(tokB, csrfB)); code != 429 || v1Code(b) != "queue_full" {
		t.Fatalf("over the limit again: %d %s", code, b)
	}
}

// Cancel and retry touch only the task they name: every other task keeps its
// state, timestamps, counters, events and actor. Refused and malformed calls
// change nothing at all.
func TestCancelRetryOtherTasksUntouched(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	tok, csrf := w.newSession(t, "alice")
	h := sessionHdr(tok, csrf)
	q1, q2 := queuedTask(t, w.env), queuedTask(t, w.env)
	f1, f2 := failedTask(t, w.env, store.StateVerifying, true), failedTask(t, w.env, store.StateCollecting, true)
	fv := failedTask(t, w.env, store.StateVerifying, false) // verdict failure: not retryable
	run := mkTask(t, w.env, store.StatePreparing, store.StateAgentRunning)
	done := mkTask(t, w.env, store.StatePreparing, store.StateAgentRunning, store.StateCollecting, store.StateVerifying,
		store.StatePassed, store.StatePublishing, store.StateDone)
	post := func(path string) (int, []byte) {
		code, _, b := w.web(t, "POST", path, "", h)
		return code, b
	}

	// Refused and malformed calls change nothing.
	before := snapAll(t, w.env)
	for _, c := range []struct {
		path   string
		status int
		code   string
	}{
		{"/api/v1/tasks/" + itoa(done) + "/cancel", 409, "already_final"},
		{"/api/v1/tasks/" + itoa(fv) + "/cancel", 409, "already_final"},
		{"/api/v1/tasks/" + itoa(f2) + "/cancel", 409, "already_final"}, // failed is final for cancel, even when retryable
		{"/api/v1/tasks/" + itoa(q1) + "/retry", 409, "not_retryable"},
		{"/api/v1/tasks/" + itoa(fv) + "/retry", 409, "not_retryable"},
		{"/api/v1/tasks/" + itoa(run) + "/retry", 409, "not_retryable"},
		{"/api/v1/tasks/99999/cancel", 404, "not_found"},
		{"/api/v1/tasks/99999/retry", 404, "not_found"},
		{"/api/v1/tasks/0/cancel", 400, "bad_request"},
		{"/api/v1/tasks/-1/cancel", 400, "bad_request"},
		{"/api/v1/tasks/abc/retry", 400, "bad_request"},
		{"/api/v1/tasks/1.5/cancel", 400, "bad_request"},
		{"/api/v1/tasks/1e3/cancel", 400, "bad_request"},
		{"/api/v1/tasks/" + itoa(q1) + "," + itoa(q2) + "/cancel", 400, "bad_request"},
		{"/api/v1/tasks/9223372036854775808/cancel", 400, "bad_request"},
	} {
		if code, b := post(c.path); code != c.status || v1Code(b) != c.code {
			t.Fatalf("%s: %d %s", c.path, code, b)
		}
	}
	sameExcept(t, "refused calls", before, snapAll(t, w.env))

	// A cancel and a retry each change only their own task.
	before = snapAll(t, w.env)
	if code, b := post("/api/v1/tasks/" + itoa(q1) + "/cancel"); code != 200 {
		t.Fatalf("cancel: %d %s", code, b)
	}
	if code, b := post("/api/v1/tasks/" + itoa(f1) + "/retry"); code != 200 {
		t.Fatalf("retry: %d %s", code, b)
	}
	after := snapAll(t, w.env)
	sameExcept(t, "cancel+retry", before, after, q1, f1)
	if after[q1].State != store.StateCancelled || after[f1].State != store.StateVerifying ||
		after[q1].Actor != "session:alice" || after[f1].Actor != "session:alice" {
		t.Fatalf("targets: %+v %+v", after[q1], after[f1])
	}
	// The running task can be cancelled, and that too touches only it.
	before = after
	if code, b := post("/api/v1/tasks/" + itoa(run) + "/cancel"); code != 200 {
		t.Fatalf("cancel running: %d %s", code, b)
	}
	sameExcept(t, "cancel running", before, snapAll(t, w.env), run)
}

// /repos never lists or describes anything outside the allowed roots,
// whatever the tree under a root holds and whatever the query says.
func TestReposCannotEscapeRoots(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	tok, _ := w.newSession(t, "alice")
	outerSHA, x, xSHA := outerSetup(t, w.env)
	sibling := w.root + "-evil" // shares the root's name as a prefix
	mkRepo(t, filepath.Join(sibling, "s"))
	git(t, filepath.Join(sibling, "s"), "commit", "-q", "--allow-empty", "-m", "sibling") // a SHA unlike the root's repos
	sibSHA := git(t, filepath.Join(sibling, "s"), "rev-parse", "HEAD")
	git(t, filepath.Join(sibling, "s"), "branch", "siblingsecretbranch")
	mustLink := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(w.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	mustLink(x, "to-x")                             // symlink to a repo outside
	mustLink("/", "to-slash")                       // symlink to /
	mustLink("..", "to-parent")                     // relative symlink to the parent (the outer repo)
	mustLink(filepath.Join(sibling, "s"), "to-sib") // symlink to the prefix sibling
	mustLink(filepath.Join(w.root, "loop"), "loop") // symlink loop
	os.MkdirAll(filepath.Join(w.root, "hollow", ".git"), 0o755)
	gf := filepath.Join(w.root, "gitfile-out")
	os.MkdirAll(gf, 0o755)
	os.WriteFile(filepath.Join(gf, ".git"), []byte("gitdir: "+filepath.Join(x, ".git")+"\n"), 0o644)
	gfRel := filepath.Join(w.root, "gitfile-rel")
	os.MkdirAll(gfRel, 0o755)
	os.WriteFile(filepath.Join(gfRel, ".git"), []byte("gitdir: ../../elsewhere/x/.git\n"), 0o644)
	nested := filepath.Join(w.root, "a", "b", "deep") // depth 3: below the walk depth
	mkRepo(t, nested)

	secrets := []string{outerSHA, xSHA, sibSHA, "outersecretbranch", "xsecretbranch", "siblingsecretbranch",
		"elsewhere", "-evil", "to-x", "to-slash", "to-parent", "to-sib", "hollow", "gitfile", "deep"}
	for _, q := range []string{"", "?root=/", "?path=..", "?root=" + sibling, "?depth=10", "?roots=" + filepath.Dir(w.root)} {
		code, _, b := w.web(t, "GET", "/api/v1/repos"+q, "", hdr{"Cookie": cookie(tok)})
		if code != 200 {
			t.Fatalf("%q: %d %s", q, code, b)
		}
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Fatalf("%q: response contains %q: %s", q, s, b)
			}
		}
		var rs []RepoView
		if err := json.Unmarshal(b, &rs); err != nil || len(rs) != 1 || rs[0].Path != w.repo {
			t.Fatalf("%q: listed %s", q, b)
		}
	}
	// Without a session /repos is 401 and says nothing about paths.
	code, _, b := w.web(t, "GET", "/api/v1/repos", "", nil)
	if code != 401 || strings.Contains(string(b), w.root) {
		t.Fatalf("no session: %d %s", code, b)
	}
}

// leakyReader is a vm.Cloud + vm.CloudReader whose errors and outputs carry
// text that must never reach a client (host names, a password, CLI noise).
type leakyReader struct {
	quota, status string
	quotaErr      error
	statusErr     error
}

const leakText = "keystone.secret.example:5000 password=hunter2 Traceback"

func (leakyReader) DeleteServer(context.Context, string) error                     { return nil }
func (leakyReader) CreateVolumeFromSnapshot(context.Context, string, string) error { return nil }
func (leakyReader) CreateServer(context.Context, string, string) error             { return nil }
func (leakyReader) WaitSSH(context.Context, string) error                          { return nil }
func (r leakyReader) QuotaUsage(context.Context) (string, error)                   { return r.quota, r.quotaErr }
func (r leakyReader) ServerStatus(context.Context, string) (string, error) {
	return r.status, r.statusErr
}
func (leakyReader) CountRestoreVolumes(context.Context) (int, error) { return 1, nil }

// naiveCloud passes every vm.CloudInfo field through unchanged (the worst
// possible adapter), so the test sees what vm and the API themselves let out.
func naiveCloud(tv *vm.TestVM) func(context.Context) CloudView {
	return func(ctx context.Context) CloudView {
		in := tv.CloudStatus(ctx)
		lim := func(l vm.CloudLimit) CloudLimit {
			return CloudLimit{Limit: l.Limit, InUse: l.InUse, Reserved: l.Reserved, Unlimited: l.Unlimited}
		}
		return CloudView{OK: in.OK, Code: in.Code, RestoreVolumes: in.RestoreVolumes, CheckedAt: in.CheckedAt,
			Quota:  &QuotaView{Gigabytes: lim(in.Gigabytes), Volumes: lim(in.Volumes)},
			TestVM: &VMState{Name: in.VMName, Status: in.VMStatus}}
	}
}

const goodQuota = `[{"Resource":"gigabytes","In Use":10,"Reserved":0,"Limit":300},{"Resource":"volumes","In Use":2,"Reserved":0,"Limit":-1}]`

// ops.cloud never carries cloud error text, CLI output or a supplier's free
// text: failure codes are one of two fixed values, the VM status is a plain
// status word or "unknown", and a failed cloud has no quota or VM block.
func TestOpsNeverLeaksCloudErrorText(t *testing.T) {
	leak := errors.New(leakText)
	readers := map[string]leakyReader{
		"quota error":         {quotaErr: leak, status: "ACTIVE"},
		"quota not JSON":      {quota: leakText, status: "ACTIVE"},
		"server error":        {quota: goodQuota, statusErr: leak},
		"status is CLI noise": {quota: goodQuota, status: "ERROR: " + leakText},
		"status multi-line":   {quota: goodQuota, status: "ACTIVE\n" + leakText},
	}
	suppliers := map[string]func(context.Context) CloudView{
		"code with text": func(context.Context) CloudView { return CloudView{Code: "cloud_unavailable: " + leakText} },
		"ok with code text": func(context.Context) CloudView {
			return CloudView{OK: true, Code: leakText, Quota: &QuotaView{}, TestVM: &VMState{Name: "test-vm", Status: "ACTIVE"}}
		},
		"status text": func(context.Context) CloudView {
			return CloudView{OK: true, Quota: &QuotaView{}, TestVM: &VMState{Name: "test-vm", Status: leakText}}
		},
		"name text": func(context.Context) CloudView {
			return CloudView{OK: true, Quota: &QuotaView{}, TestVM: &VMState{Name: leakText, Status: "ACTIVE"}}
		},
		"failure with blocks": func(context.Context) CloudView {
			return CloudView{Code: CloudTimeout, Quota: &QuotaView{}, TestVM: &VMState{Name: "test-vm", Status: leakText}}
		},
	}
	for name, r := range readers {
		tv := &vm.TestVM{Name: "test-vm", Cloud: r}
		suppliers["vm/"+name] = naiveCloud(tv)
	}
	for name, sup := range suppliers {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, noSteps{}, 1, func(c *Config) {
				c.Ops = func() OpsView { return OpsView{Build: "rev1", Started: time.Now()} }
				c.CloudStatus = sup
			})
			code, b := e.do(t, "GET", "/api/v1/ops", "")
			if code != 200 {
				t.Fatalf("%d %s", code, b)
			}
			for _, s := range []string{"keystone", "hunter2", "Traceback", "password", "ERROR:"} {
				if strings.Contains(string(b), s) {
					t.Fatalf("ops leaks %q: %s", s, b)
				}
			}
			var v OpsView
			if err := json.Unmarshal(b, &v); err != nil || v.Cloud == nil || v.Build != "rev1" {
				t.Fatalf("%v %s", err, b)
			}
			c := v.Cloud
			switch {
			case c.OK:
				if c.Code != "" || c.TestVM == nil || !(c.TestVM.Status == "unknown" || vmStatusRe.MatchString(c.TestVM.Status)) {
					t.Fatalf("ok view %+v", c)
				}
			default:
				if (c.Code != CloudUnavailable && c.Code != CloudTimeout) || c.Quota != nil || c.TestVM != nil {
					t.Fatalf("failure view %+v", c)
				}
			}
		})
	}
}

// startDaemonOn runs a daemon (socket and HTTP listener) on the database at
// dbPath and returns it with a stop function that shuts it down completely
// and closes the store, like a process exit.
func startDaemonOn(t *testing.T, dbPath, sock, root string) (*Daemon, *store.Store, *http.Client, func()) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Socket: sock, Concurrency: 1, AllowedRepos: []string{root}}
	withHTTP(&cfg)
	d, err := NewDaemon(cfg, st, noSteps{})
	if err != nil {
		t.Fatal(err)
	}
	d.pauseWorkers()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	select {
	case <-d.Ready():
	case err := <-done:
		t.Fatalf("daemon exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
		st.Close()
	}
	t.Cleanup(stop)
	return d, st, client, stop
}

// A client whose submit response was lost (the daemon went down after the
// commit) resends with the same Idempotency-Key to the restarted daemon and
// gets the SAME task back (200, Idempotent-Replay), not a second task. The
// key stays scoped to its principal, and a changed body is a conflict.
func TestIdempotentSubmitAcrossRestartWindow(t *testing.T) {
	dir := shortDir(t)
	root := filepath.Join(dir, "repos")
	repo := filepath.Join(root, "proj")
	mkRepo(t, repo)
	dbPath, sock := filepath.Join(dir, "db", "o.db"), filepath.Join(dir, "o.sock")
	body := func(p string) []byte {
		b, _ := json.Marshal(map[string]string{"repo": repo, "base": "HEAD", "prompt": p, "class": "docs", "agent": "claude"})
		return b
	}
	sockSubmit := func(c *http.Client, key string, b []byte) (int, http.Header, []byte) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://orchd/api/v1/tasks", strings.NewReader(string(b)))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, rb
	}
	id := func(b []byte) int64 {
		var v TaskView
		_ = json.Unmarshal(b, &v)
		return v.ID
	}

	// First daemon: a keyed submit from the socket and one from a token.
	_, st1, c1, stop1 := startDaemonOn(t, dbPath, sock, root)
	bearer := "tok-restart-" + fmt.Sprint(time.Now().UnixNano())
	if _, err := st1.CreateSession(context.Background(), bearer, "", "mac-ssh", KindToken, time.Hour); err != nil {
		t.Fatal(err)
	}
	code, _, b := sockSubmit(c1, "K-restart-1", body("same"))
	if code != 201 {
		t.Fatalf("first submit: %d %s", code, b)
	}
	first := id(b)
	stop1() // the response "was lost": the daemon is gone

	// Restarted daemon on the same database.
	d2, st2, c2, _ := startDaemonOn(t, dbPath, sock, root)
	code, h, b := sockSubmit(c2, "K-restart-1", body("same"))
	if code != 200 || h.Get("Idempotent-Replay") != "true" || id(b) != first {
		t.Fatalf("resend after restart: %d %v %s (want task %d)", code, h, b, first)
	}
	// Same key, different body: conflict, nothing created.
	code, _, b = sockSubmit(c2, "K-restart-1", body("changed"))
	if code != 409 || v1Code(b) != "idempotency_conflict" {
		t.Fatalf("changed body: %d %s", code, b)
	}
	ts, err := st2.ListTasks(context.Background(), 100)
	if err != nil || len(ts) != 1 {
		t.Fatalf("tasks after restart: %d %v", len(ts), err)
	}
	// The same key from another principal (a token over HTTP) is its own
	// submit: keys never cross principals.
	hc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	code, rb, err := try(hc, "POST", "http://"+d2.HTTPAddr().String()+"/api/v1/tasks", webHost, body("same"),
		hdr{"Content-Type": "application/json", "Authorization": "Bearer " + bearer, "Idempotency-Key": "K-restart-1"})
	if err != nil || code != 201 || id(rb) == first {
		t.Fatalf("other principal, same key: %d %s %v", code, rb, err)
	}
	// And that principal's resend replays its own task.
	code, rb2, err := try(hc, "POST", "http://"+d2.HTTPAddr().String()+"/api/v1/tasks", webHost, body("same"),
		hdr{"Content-Type": "application/json", "Authorization": "Bearer " + bearer, "Idempotency-Key": "K-restart-1"})
	if err != nil || code != 200 || id(rb2) != id(rb) {
		t.Fatalf("token resend: %d %s %v", code, rb2, err)
	}
	if ts, _ := st2.ListTasks(context.Background(), 100); len(ts) != 2 {
		t.Fatalf("%d tasks, want 2", len(ts))
	}
}
