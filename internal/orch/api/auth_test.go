package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/store"
)

// usePeerUID swaps the peer-credential seam for the test. It must run before
// newEnv (starting the daemon goroutine orders the write before every read)
// and only in sequential tests.
func usePeerUID(t *testing.T, f func(net.Conn) (uint32, error)) {
	t.Helper()
	old := peerUID
	peerUID = f
	t.Cleanup(func() { peerUID = old })
}

// httpServer serves the daemon's handler over TCP with the production
// connection hook, the way the HTTP listener (Task 21) will.
func (e *env) httpServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(e.d.handler)
	srv.Config.ConnContext = ConnContext
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

type hdr map[string]string

func httpDo(t *testing.T, srv *httptest.Server, method, path, body string, h hdr) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// sockDo is e.do with request headers and the response headers returned.
func (e *env) sockDo(t *testing.T, method, path, body string, h hdr) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://orchd"+path, strings.NewReader(body))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func v1Code(b []byte) string {
	var m struct {
		Error struct{ Code, Message string }
	}
	_ = json.Unmarshal(b, &m)
	return m.Error.Code
}

func flatCode(b []byte) string {
	var m struct{ Code string }
	_ = json.Unmarshal(b, &m)
	return m.Code
}

func (e *env) newSession(t *testing.T, principal string) (token, csrf string) {
	t.Helper()
	token, csrf = "sess-"+principal+"-"+strconv.FormatInt(time.Now().UnixNano(), 36), "csrf-"+principal
	if _, err := e.st.CreateSession(context.Background(), token, csrf, principal, KindSession, time.Hour); err != nil {
		t.Fatal(err)
	}
	return token, csrf
}

func (e *env) newToken(t *testing.T, principal string) string {
	t.Helper()
	token := "tok-" + principal + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := e.st.CreateSession(context.Background(), token, "", principal, KindToken, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	return token
}

func cookie(token string) string { return SessionCookieName + "=" + token }

func (e *env) taskCount(t *testing.T) int {
	t.Helper()
	ts, err := e.st.ListTasks(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(ts)
}

func (e *env) submitBody(prompt string) string {
	b, _ := json.Marshal(map[string]string{"repo": e.repo, "base": "HEAD", "prompt": prompt, "class": "docs", "agent": "claude"})
	return string(b)
}

func TestSocketPrincipalAllowedUID(t *testing.T) {
	// Real peer credentials, default allowlist (the daemon's own uid).
	e := newEnv(t, noSteps{}, 1)
	if code, b := e.do(t, "GET", "/api/v1/tasks", ""); code != 200 {
		t.Fatalf("same-uid read: %d %s", code, b)
	}
	if code, b := e.submit(t, e.repo, "HEAD", "p", "docs", "claude"); code != http.StatusCreated {
		t.Fatalf("same-uid write: %d %s", code, b)
	}

	// An explicit allowlist that names the caller's uid.
	e2 := newEnv(t, noSteps{}, 1, func(c *Config) { c.SocketUIDs = []uint32{uint32(os.Getuid())} })
	if code, b := e2.do(t, "GET", "/api/v1/tasks", ""); code != 200 {
		t.Fatalf("listed uid: %d %s", code, b)
	}
}

func TestSocketPrincipalAllowedUIDSeam(t *testing.T) {
	usePeerUID(t, func(net.Conn) (uint32, error) { return 4242, nil })
	e := newEnv(t, noSteps{}, 1, func(c *Config) { c.SocketUIDs = []uint32{7, 4242} })
	if code, b := e.do(t, "GET", "/api/v1/tasks", ""); code != 200 {
		t.Fatalf("allowed uid via seam: %d %s", code, b)
	}
}

func TestPeerUIDRealSocket(t *testing.T) {
	dir := shortDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); got <- c }()
	c, err := net.Dial("unix", filepath.Join(dir, "p.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc := <-got
	if sc == nil {
		t.Fatal("accept failed")
	}
	defer sc.Close()
	uid, err := peerCredUID(sc)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Fatalf("peer uid %d %v, want %d", uid, err, os.Getuid())
	}
	// A TCP connection has no peer credentials.
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	go func() { c, _ := tl.Accept(); got <- c }()
	tc, err := net.Dial("tcp", tl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if s := <-got; s != nil {
		defer s.Close()
		if _, err := peerCredUID(s); err == nil {
			t.Fatal("tcp conn gave a peer uid")
		}
	}
}

func TestSocketForeignUIDRefused(t *testing.T) {
	foreign := uint32(os.Getuid()) + 1
	usePeerUID(t, func(net.Conn) (uint32, error) { return foreign, nil })
	e := newEnv(t, noSteps{}, 1)
	for _, p := range []string{"/api/v1/tasks", "/api/v1/approvals", "/api/v1/nonexistent"} {
		code, b := e.do(t, "GET", p, "")
		if code != http.StatusForbidden || v1Code(b) != "forbidden_uid" {
			t.Fatalf("GET %s: %d %s", p, code, b)
		}
	}
	// Legacy alias paths are guarded too (flat error shape).
	code, b := e.do(t, "GET", "/tasks", "")
	if code != http.StatusForbidden || flatCode(b) != "forbidden_uid" {
		t.Fatalf("legacy GET: %d %s", code, b)
	}
	code, b = e.submit(t, e.repo, "HEAD", "p", "docs", "claude")
	if code != http.StatusForbidden {
		t.Fatalf("legacy submit: %d %s", code, b)
	}
	code, b = e.do(t, "POST", "/api/v1/tasks", e.submitBody("p"))
	if code != http.StatusForbidden || v1Code(b) != "forbidden_uid" {
		t.Fatalf("v1 submit: %d %s", code, b)
	}
	if n := e.taskCount(t); n != 0 {
		t.Fatalf("a refused peer created %d tasks", n)
	}
}

func TestSocketPeerCredFailureRefused(t *testing.T) {
	usePeerUID(t, func(net.Conn) (uint32, error) { return 0, errors.New("no creds") })
	e := newEnv(t, noSteps{}, 1, func(c *Config) { c.SocketUIDs = []uint32{0, uint32(os.Getuid())} })
	if code, b := e.do(t, "GET", "/api/v1/tasks", ""); code != http.StatusForbidden || v1Code(b) != "forbidden_uid" {
		t.Fatalf("peercred failure: %d %s", code, b)
	}
}

func TestSocketForeignUIDRefusedRealCreds(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, func(c *Config) { c.SocketUIDs = []uint32{uint32(os.Getuid()) + 1} })
	if code, b := e.do(t, "GET", "/api/v1/tasks", ""); code != http.StatusForbidden || v1Code(b) != "forbidden_uid" {
		t.Fatalf("unlisted real uid: %d %s", code, b)
	}
}

func TestHTTPNoCredentials401(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	srv := e.httpServer(t)
	for _, p := range []string{"/api/v1/tasks", "/api/v1/stages", "/api/v1/nonexistent"} {
		code, _, b := httpDo(t, srv, "GET", p, "", nil)
		if code != http.StatusUnauthorized || v1Code(b) != "unauthenticated" {
			t.Fatalf("GET %s: %d %s", p, code, b)
		}
	}
	code, _, b := httpDo(t, srv, "GET", "/tasks", "", nil)
	if code != http.StatusUnauthorized || flatCode(b) != "unauthenticated" {
		t.Fatalf("legacy: %d %s", code, b)
	}
	code, _, b = httpDo(t, srv, "POST", "/api/v1/tasks", e.submitBody("p"), nil)
	if code != http.StatusUnauthorized || e.taskCount(t) != 0 {
		t.Fatalf("POST: %d %s", code, b)
	}
	// Bad credentials are refused with a fixed message that never echoes them (CF1).
	const secret = "s3cr3t-<script>-value"
	for _, h := range []hdr{
		{"Authorization": "Bearer " + secret},
		{"Authorization": "Basic " + secret},
		{"Authorization": secret},
		{"Cookie": cookie(secret)},
	} {
		code, _, b := httpDo(t, srv, "GET", "/api/v1/tasks", "", h)
		if code != http.StatusUnauthorized || v1Code(b) != "unauthenticated" || strings.Contains(string(b), "s3cr3t") {
			t.Fatalf("%v: %d %s", h, code, b)
		}
	}
	// A request without connection information (no listener hook) is treated
	// as untrusted HTTP: it never becomes a socket principal.
	rec := httptest.NewRecorder()
	e.d.handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/tasks", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no conn info: %d %s", rec.Code, rec.Body)
	}
}

func TestHealthzOpen(t *testing.T) {
	usePeerUID(t, func(net.Conn) (uint32, error) { return uint32(os.Getuid()) + 1, nil })
	e := newEnv(t, noSteps{}, 1)
	srv := e.httpServer(t)
	check := func(where string, code int, b []byte) {
		t.Helper()
		var m map[string]any
		if code != 200 || json.Unmarshal(b, &m) != nil || len(m) != 1 || m["ok"] != true {
			t.Fatalf("%s: %d %s", where, code, b)
		}
	}
	code, _, b := httpDo(t, srv, "GET", "/api/v1/healthz", "", nil)
	check("http", code, b)
	code, b = e.do(t, "GET", "/api/v1/healthz", "") // foreign uid: still open
	check("socket", code, b)
	// Only GET is open.
	if code, _, b := httpDo(t, srv, "POST", "/api/v1/healthz", "{}", nil); code != http.StatusUnauthorized {
		t.Fatalf("POST healthz: %d %s", code, b)
	}
}

func TestIdempotentSubmitReturnsSameTask(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	body := e.submitBody("idem")
	h := hdr{"Idempotency-Key": "key-1"}
	code, rh, first := e.sockDo(t, "POST", "/api/v1/tasks", body, h)
	if code != http.StatusCreated || rh.Get("Idempotent-Replay") != "" {
		t.Fatalf("first: %d %v %s", code, rh, first)
	}
	code, rh, second := e.sockDo(t, "POST", "/api/v1/tasks", body, h)
	if code != http.StatusOK || rh.Get("Idempotent-Replay") != "true" || string(second) != string(first) {
		t.Fatalf("replay: %d %v\n%s\n%s", code, rh, second, first)
	}
	if n := e.taskCount(t); n != 1 {
		t.Fatalf("%d tasks", n)
	}
	// Without a key every submit is a new task.
	if code, _, _ := e.sockDo(t, "POST", "/api/v1/tasks", body, nil); code != http.StatusCreated || e.taskCount(t) != 2 {
		t.Fatalf("keyless: %d", code)
	}
	// The legacy alias honours the key too.
	code, _, _ = e.sockDo(t, "POST", "/tasks", body, h)
	if code != http.StatusOK || e.taskCount(t) != 2 {
		t.Fatalf("legacy replay: %d %d", code, e.taskCount(t))
	}
}

func TestIdempotencyKeyDifferentBodyConflict409(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	h := hdr{"Idempotency-Key": "key-2"}
	if code, _, b := e.sockDo(t, "POST", "/api/v1/tasks", e.submitBody("one"), h); code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, b)
	}
	code, _, b := e.sockDo(t, "POST", "/api/v1/tasks", e.submitBody("two"), h)
	if code != http.StatusConflict || v1Code(b) != "idempotency_conflict" {
		t.Fatalf("conflict: %d %s", code, b)
	}
	if n := e.taskCount(t); n != 1 {
		t.Fatalf("%d tasks", n)
	}
}

func TestIdempotencyKeyMalformed400(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	body := e.submitBody("p")
	for _, k := range []string{strings.Repeat("a", 129), "tab\there", "é-key"} {
		code, _, b := e.sockDo(t, "POST", "/api/v1/tasks", body, hdr{"Idempotency-Key": k})
		if code != http.StatusBadRequest || v1Code(b) != "bad_idempotency_key" || strings.Contains(string(b), k) {
			t.Fatalf("key %q: %d %s", k, code, b)
		}
	}
	// Two keys are ambiguous.
	req, _ := http.NewRequest("POST", "http://orchd/api/v1/tasks", strings.NewReader(body))
	req.Header.Add("Idempotency-Key", "a")
	req.Header.Add("Idempotency-Key", "b")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || v1Code(b) != "bad_idempotency_key" {
		t.Fatalf("two keys: %d %s", resp.StatusCode, b)
	}
	if n := e.taskCount(t); n != 0 {
		t.Fatalf("%d tasks", n)
	}
	// 128 printable bytes (space included) are fine.
	if code, _, b := e.sockDo(t, "POST", "/api/v1/tasks", body, hdr{"Idempotency-Key": "a b" + strings.Repeat("~", 125)}); code != http.StatusCreated {
		t.Fatalf("max key: %d %s", code, b)
	}
}

func TestIdempotencyKeyScopedPerPrincipal(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	srv := e.httpServer(t)
	alice, bob := e.newToken(t, "alice"), e.newToken(t, "bob")
	body := e.submitBody("scoped")
	code, _, a := httpDo(t, srv, "POST", "/api/v1/tasks", body, hdr{"Authorization": "Bearer " + alice, "Idempotency-Key": "shared"})
	if code != http.StatusCreated {
		t.Fatalf("alice: %d %s", code, a)
	}
	// Bob's identical key and body is his own: a new task, not alice's replay.
	code, rh, b := httpDo(t, srv, "POST", "/api/v1/tasks", body, hdr{"Authorization": "Bearer " + bob, "Idempotency-Key": "shared"})
	if code != http.StatusCreated || rh.Get("Idempotent-Replay") != "" || string(a) == string(b) {
		t.Fatalf("bob: %d %s", code, b)
	}
	// Bob's different body under alice's key is not a conflict with alice.
	code, _, b = httpDo(t, srv, "POST", "/api/v1/tasks", e.submitBody("other"), hdr{"Authorization": "Bearer " + bob, "Idempotency-Key": "alice-only"})
	if code != http.StatusCreated {
		t.Fatalf("bob other: %d %s", code, b)
	}
	// The socket principal has its own scope as well.
	code, _, b = e.sockDo(t, "POST", "/api/v1/tasks", body, hdr{"Idempotency-Key": "shared"})
	if code != http.StatusCreated {
		t.Fatalf("socket: %d %s", code, b)
	}
	if n := e.taskCount(t); n != 4 {
		t.Fatalf("%d tasks", n)
	}
}

func TestWriteTierSessionCSRF(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	srv := e.httpServer(t)
	tok, csrf := e.newSession(t, "alice")
	body := e.submitBody("p")
	// Reads need only the session.
	if code, _, b := httpDo(t, srv, "GET", "/api/v1/tasks", "", hdr{"Cookie": cookie(tok)}); code != 200 {
		t.Fatalf("read: %d %s", code, b)
	}
	for name, h := range map[string]hdr{
		"missing": {"Cookie": cookie(tok)},
		"wrong":   {"Cookie": cookie(tok), CSRFHeader: csrf + "x"},
		"empty":   {"Cookie": cookie(tok), CSRFHeader: ""},
	} {
		code, _, b := httpDo(t, srv, "POST", "/api/v1/tasks", body, h)
		if code != http.StatusForbidden || v1Code(b) != "csrf_failed" || strings.Contains(string(b), csrf) {
			t.Fatalf("%s csrf: %d %s", name, code, b)
		}
	}
	if n := e.taskCount(t); n != 0 {
		t.Fatalf("refused writes created %d tasks", n)
	}
	code, _, b := httpDo(t, srv, "POST", "/api/v1/tasks", body, hdr{"Cookie": cookie(tok), CSRFHeader: csrf})
	if code != http.StatusCreated {
		t.Fatalf("valid csrf: %d %s", code, b)
	}
}

func TestWriteTierCoversEveryMutatingRoute(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	srv := e.httpServer(t)
	tok, _ := e.newSession(t, "alice")
	for _, p := range []string{
		"/api/v1/tasks", "/api/v1/tasks/1/cancel", "/api/v1/tasks/1/retry",
		"/api/v1/approvals/1/reverify", "/api/v1/approvals/1/grant", "/api/v1/admin/enroll",
		"/api/v1/auth/logout",
		"/tasks", "/tasks/1/cancel", "/tasks/1/retry", "/approvals/1/reverify",
	} {
		code, _, b := httpDo(t, srv, "POST", p, "{}", hdr{"Cookie": cookie(tok)})
		if code != http.StatusForbidden || (v1Code(b) != "csrf_failed" && flatCode(b) != "csrf_failed") {
			t.Fatalf("POST %s without CSRF: %d %s", p, code, b)
		}
	}
}

func TestTokenPrincipalWritesWithoutCSRF(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	srv := e.httpServer(t)
	tok := e.newToken(t, "cli")
	code, _, b := httpDo(t, srv, "POST", "/api/v1/tasks", e.submitBody("p"), hdr{"Authorization": "bearer " + tok})
	if code != http.StatusCreated {
		t.Fatalf("token write: %d %s", code, b)
	}
}

func TestCredentialKindBoundToChannel(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	srv := e.httpServer(t)
	stok, _ := e.newSession(t, "alice")
	btok := e.newToken(t, "alice")
	if code, _, b := httpDo(t, srv, "GET", "/api/v1/tasks", "", hdr{"Cookie": cookie(btok)}); code != http.StatusUnauthorized {
		t.Fatalf("bearer token as cookie: %d %s", code, b)
	}
	if code, _, b := httpDo(t, srv, "GET", "/api/v1/tasks", "", hdr{"Authorization": "Bearer " + stok}); code != http.StatusUnauthorized {
		t.Fatalf("session as bearer: %d %s", code, b)
	}
	// A malformed Authorization header does not fall back to the cookie.
	if code, _, b := httpDo(t, srv, "GET", "/api/v1/tasks", "", hdr{"Authorization": "Basic x", "Cookie": cookie(stok)}); code != http.StatusUnauthorized {
		t.Fatalf("bad authz + cookie: %d %s", code, b)
	}
}

func TestSessionExpiredOrOverLifetime401(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	srv := e.httpServer(t)
	ctx := context.Background()
	// Idle-expired: the store no longer returns it.
	if _, err := e.st.CreateSession(ctx, "short", "c", "alice", KindSession, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if code, _, b := httpDo(t, srv, "GET", "/api/v1/tasks", "", hdr{"Cookie": cookie("short")}); code != http.StatusUnauthorized {
		t.Fatalf("expired: %d %s", code, b)
	}

	// Over the absolute cap although the sliding expiry is still ahead.
	if _, err := e.st.CreateSession(ctx, "long", "c", "alice", KindSession, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	a := &StoreAuthenticator{St: e.st}
	if p, s, ok := a.Session(ctx, "long"); !ok || p.Kind != KindSession || p.ID != "alice" || s == nil {
		t.Fatalf("fresh session refused: %+v %v", p, ok)
	}
	a.Now = func() time.Time { return time.Now().Add(SessionMaxLifetime + time.Minute) }
	if _, _, ok := a.Session(ctx, "long"); ok {
		t.Fatal("session over the absolute lifetime accepted")
	}
	if _, err := e.st.GetSession(ctx, "long"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("over-lifetime session not deleted: %v", err)
	}

	// Touching never extends past the cap. One clock for the store and the
	// authenticator (CF11): at +23h a touch without the cap would store
	// +31h. TestSessionIdleAndAbsoluteExpiry covers the same end to end.
	clk := &testClock{}
	e.st.SetClock(clk.now)
	defer e.st.SetClock(nil)
	a.Now = clk.now
	if _, err := e.st.CreateSession(ctx, "near", "c", "alice", KindSession, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	clk.set(SessionMaxLifetime - time.Hour)
	if _, _, ok := a.Session(ctx, "near"); !ok {
		t.Fatal("near-cap session refused")
	}
	s, err := e.st.GetSession(ctx, "near")
	if err != nil {
		t.Fatal(err)
	}
	if lim := s.CreatedAt.Add(SessionMaxLifetime); s.ExpiresAt.After(lim) || s.ExpiresAt.Before(lim.Add(-time.Second)) {
		t.Fatalf("expiry %v, want the cap %v", s.ExpiresAt, lim)
	}
	e.st.SetClock(nil)

	// Bearer tokens have their own absolute cap.
	if _, err := e.st.CreateSession(ctx, "tok", "", "alice", KindToken, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	a.Now = nil
	if p, ok := a.Bearer(ctx, "tok"); !ok || p.Kind != KindToken {
		t.Fatalf("fresh token refused: %+v", p)
	}
	a.Now = func() time.Time { return time.Now().Add(TokenMaxLifetime + time.Minute) }
	if _, ok := a.Bearer(ctx, "tok"); ok {
		t.Fatal("token over its lifetime accepted")
	}
}

func TestGrantLedgerViaSession(t *testing.T) {
	f := newGrantFixture(t)
	srv := f.e.httpServer(t)
	tok, csrf := f.e.newSession(t, "alice")
	_, aid, _ := f.protectedTask(t, "line1\nVIA\n")
	issued := nowSec()
	m := f.message(t, aid, issued)
	b, _ := json.Marshal(f.body(f.alice.signText(t, m.Text()), issued))
	path := fmt.Sprintf("/api/v1/approvals/%d/grant", aid)
	// The session alone (no CSRF) cannot reach the grant.
	if code, _, rb := httpDo(t, srv, "POST", path, string(b), hdr{"Cookie": cookie(tok)}); code != http.StatusForbidden {
		t.Fatalf("grant without csrf: %d %s", code, rb)
	}
	if code, _, rb := httpDo(t, srv, "POST", path, string(b), hdr{"Cookie": cookie(tok), CSRFHeader: csrf}); code != 200 {
		t.Fatalf("grant: %d %s", code, rb)
	}
	evs, err := ledger.Events(gate.LedgerPath(f.home, f.rid))
	if err != nil || len(evs) != 1 {
		t.Fatalf("ledger %v %+v", err, evs)
	}
	if p := evs[0].Payload; p["via"] != KindSession || p["signer"] != "alice" || p["alg"] != signing.TypeSSHEd25519 {
		t.Fatalf("event %+v", evs[0])
	}
}
