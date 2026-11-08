package api

import (
	"bufio"
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
	"sync"
	"testing"
	"time"

	"aigate/internal/orch/store"
)

const (
	webHost   = "console.example.test"
	webOrigin = "https://console.example.test"
)

// withHTTP turns the loopback HTTP listener on (ephemeral port).
func withHTTP(c *Config) {
	c.HTTPListen = "127.0.0.1:0"
	c.RPID, c.Origin = webHost, webOrigin
	c.AllowedHosts = []string{webHost}
}

// webEnv is a daemon with its real HTTP listener; requests carry the allowed
// Host unless a test overrides it with hdr{"Host": ...}.
type webEnv struct {
	*env
	addr string
	hc   *http.Client
}

func newWebEnv(t *testing.T, opts ...func(*Config)) *webEnv {
	t.Helper()
	e := newEnv(t, noSteps{}, 1, append([]func(*Config){withHTTP}, opts...)...)
	a := e.d.HTTPAddr()
	if a == nil {
		t.Fatal("HTTP listener not bound")
	}
	return &webEnv{env: e, addr: a.String(), hc: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
}

func (w *webEnv) req(t *testing.T, method, path string, body io.Reader, h hdr) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+w.addr+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = webHost
	for k, v := range h {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (w *webEnv) web(t *testing.T, method, path, body string, h hdr) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	return w.req(t, method, path, rd, h)
}

// with returns base plus extra (extra wins).
func with(base hdr, extra hdr) hdr {
	out := hdr{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

var jsonCT = hdr{"Content-Type": "application/json"}

func TestSecurityHeadersPresent(t *testing.T) {
	w := newWebEnv(t)
	want := map[string]string{
		"Content-Security-Policy":      ContentSecurityPolicy,
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           PermissionsPolicy,
		"Strict-Transport-Security":    "max-age=31536000",
	}
	// The design's CSP core is kept.
	for _, part := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(ContentSecurityPolicy, part) {
			t.Fatalf("CSP lacks %q", part)
		}
	}
	if strings.Contains(PermissionsPolicy, "publickey-credentials") {
		t.Fatal("Permissions-Policy must not block passkeys")
	}
	for _, c := range []struct {
		name, method, path string
		h                  hdr
		code               int
	}{
		{"ok", "GET", "/api/v1/healthz", nil, 200},
		{"401", "GET", "/api/v1/tasks", nil, 401},
		{"legacy 401", "GET", "/tasks", nil, 401},
		{"421", "GET", "/api/v1/healthz", hdr{"Host": "evil.example"}, 421},
		{"405", "OPTIONS", "/api/v1/tasks", nil, 405},
	} {
		code, h, b := w.web(t, c.method, c.path, "", c.h)
		if code != c.code {
			t.Fatalf("%s: %d %s", c.name, code, b)
		}
		for k, v := range want {
			if got := h.Values(k); len(got) != 1 || got[0] != v {
				t.Fatalf("%s: %s = %q, want %q", c.name, k, got, v)
			}
		}
	}
	// The socket path is unchanged: no browser headers there.
	_, h, _ := w.sockDo(t, "GET", "/api/v1/healthz", "", nil)
	if h.Get("Content-Security-Policy") != "" {
		t.Fatal("socket responses got browser headers")
	}
}

func TestHostMismatchRefused(t *testing.T) {
	w := newWebEnv(t)
	for _, host := range []string{"evil.example", "console.example.test.evil.example", "127.0.0.1", "localhost",
		"xconsole.example.test", "console.example.tes", "127.0.0.1:" + strings.Split(w.addr, ":")[1]} {
		code, _, b := w.web(t, "GET", "/api/v1/healthz", "", hdr{"Host": host})
		if code != http.StatusMisdirectedRequest || v1Code(b) != "bad_host" || strings.Contains(string(b), host) {
			t.Fatalf("Host %q: %d %s", host, code, b)
		}
	}
	// A rebinding page posting to the login endpoint is refused before it runs.
	code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", `{"signer":"alice"}`,
		with(jsonCT, hdr{"Host": "evil.example", "Origin": "http://evil.example"}))
	if code != http.StatusMisdirectedRequest {
		t.Fatalf("login with foreign host: %d %s", code, b)
	}
	if n, _ := w.st.CountChallenges(context.Background(), ChallengePurposeLogin); n != 0 {
		t.Fatalf("%d challenges created", n)
	}
	// Legacy paths get the flat error shape.
	if code, _, b := w.web(t, "GET", "/tasks", "", hdr{"Host": "evil.example"}); code != 421 || flatCode(b) != "bad_host" {
		t.Fatalf("legacy: %d %s", code, b)
	}
	// Case and port do not matter.
	for _, host := range []string{webHost, "CONSOLE.Example.TEST", webHost + ":443", webHost + ":8808"} {
		if code, _, b := w.web(t, "GET", "/api/v1/healthz", "", hdr{"Host": host}); code != 200 {
			t.Fatalf("Host %q: %d %s", host, code, b)
		}
	}
	// HTTP/1.0 without a Host header.
	c, err := net.Dial("tcp", w.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET /api/v1/healthz HTTP/1.0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("no Host: %d", resp.StatusCode)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	tok, csrf := w.newSession(t, "phone")
	sess := hdr{"Content-Type": "application/json", "Cookie": cookie(tok), CSRFHeader: csrf}
	body := w.submitBody("p")
	for _, h := range []hdr{
		{"Origin": "https://evil.example"},
		{"Origin": "null"},
		{"Origin": "http://console.example.test"},
		{"Origin": webOrigin + ":443"},
		{"Origin": webOrigin + "/"},
		{"Origin": "https://console.example.test.evil.example"},
		{"Origin": "https://CONSOLE.example.test"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "https://evil.example", "Sec-Fetch-Site": "same-origin"},
		{}, // the cookie alone, no Origin and no Sec-Fetch-Site
	} {
		code, _, b := w.web(t, "POST", "/api/v1/tasks", body, with(sess, h))
		if code != http.StatusForbidden || v1Code(b) != "bad_origin" || strings.Contains(string(b), "evil") {
			t.Fatalf("%v: %d %s", h, code, b)
		}
	}
	if code, _, b := w.web(t, "POST", "/tasks", body, with(sess, hdr{"Origin": "https://evil.example"})); code != 403 || flatCode(b) != "bad_origin" {
		t.Fatalf("legacy: %d %s", code, b)
	}
	// Two Origin headers are refused even if one matches.
	req, _ := http.NewRequest("POST", "http://"+w.addr+"/api/v1/tasks", strings.NewReader(body))
	req.Host = webHost
	for k, v := range sess {
		req.Header.Set(k, v)
	}
	req.Header.Add("Origin", webOrigin)
	req.Header.Add("Origin", "https://evil.example")
	if resp, err := w.hc.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("two Origins: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	if n := w.taskCount(t); n != 0 {
		t.Fatalf("refused requests created %d tasks", n)
	}
	// Controls: the console's own origin, or a same-origin fetch without Origin.
	for _, h := range []hdr{{"Origin": webOrigin}, {"Sec-Fetch-Site": "same-origin"}} {
		if code, _, b := w.web(t, "POST", "/api/v1/tasks", body, with(sess, h)); code != http.StatusCreated {
			t.Fatalf("%v: %d %s", h, code, b)
		}
	}
	// A bearer token (the CLI) needs no Origin, but a wrong one still fails.
	bearer := hdr{"Content-Type": "application/json", "Authorization": "Bearer " + w.newToken(t, "alice")}
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", body, bearer); code != http.StatusCreated {
		t.Fatalf("bearer: %d %s", code, b)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", body, with(bearer, hdr{"Origin": "https://evil.example"})); code != 403 {
		t.Fatalf("bearer + foreign origin: %d %s", code, b)
	}
	// Unauthenticated login POSTs: a foreign page is refused before a
	// challenge is made; the CLI's `orch login` (no Origin, no cookie) works.
	login := `{"signer":"alice"}`
	if code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", login, with(jsonCT, hdr{"Origin": "https://evil.example"})); code != 403 || v1Code(b) != "bad_origin" {
		t.Fatalf("foreign login: %d %s", code, b)
	}
	if n, _ := w.st.CountChallenges(context.Background(), ChallengePurposeLogin); n != 0 {
		t.Fatalf("%d challenges", n)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", login, jsonCT); code != 200 {
		t.Fatalf("CLI-shaped login: %d %s", code, b)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", login, with(jsonCT, hdr{"Origin": webOrigin})); code != 200 {
		t.Fatalf("console login: %d %s", code, b)
	}
}

// CF16: GET /auth/session rotates the CSRF token, so it is origin-checked.
func TestOriginCheckedOnAuthSession(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	tok, csrf := w.newSession(t, "phone")
	c := hdr{"Cookie": cookie(tok)}
	for _, m := range []string{"GET", "HEAD"} {
		for _, h := range []hdr{{"Origin": "https://evil.example"}, {"Sec-Fetch-Site": "cross-site"}, {"Sec-Fetch-Site": "same-site"}, {}} {
			code, _, b := w.web(t, m, "/api/v1/auth/session", "", with(c, h))
			if code != http.StatusForbidden || (m == "GET" && v1Code(b) != "bad_origin") {
				t.Fatalf("%s %v: %d %s", m, h, code, b)
			}
		}
	}
	// The refused calls did not rotate the token: the old one still writes.
	write := with(c, hdr{"Content-Type": "application/json", "Origin": webOrigin, CSRFHeader: csrf})
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", w.submitBody("p"), write); code != http.StatusCreated {
		t.Fatalf("old csrf after refused rotations: %d %s", code, b)
	}
	// Same-origin is allowed and rotates.
	code, _, b := w.web(t, "GET", "/api/v1/auth/session", "", with(c, hdr{"Sec-Fetch-Site": "same-origin"}))
	var r struct{ CSRF string }
	if code != 200 || json.Unmarshal(b, &r) != nil || r.CSRF == "" {
		t.Fatalf("same-origin session: %d %s", code, b)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", w.submitBody("p"), write); code != 403 || v1Code(b) != "csrf_failed" {
		t.Fatalf("old csrf after rotation: %d %s", code, b)
	}
	// Ordinary reads are not origin-checked (SameSite=Strict keeps the cookie
	// off cross-site requests anyway).
	if code, _, b := w.web(t, "GET", "/api/v1/tasks", "", with(c, hdr{"Origin": "https://evil.example"})); code != 200 {
		t.Fatalf("read: %d %s", code, b)
	}
}

// rawRequest writes head (request line and headers, without the final CRLF)
// and body on a fresh connection and reads the response. The body is written
// concurrently and write errors are ignored: a server that refuses early may
// close the connection before the client has sent everything.
func rawRequest(t *testing.T, network, addr, head string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	go func() {
		_, _ = io.WriteString(c, head+"\r\n")
		_, _ = c.Write(body)
	}()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func TestBodyTooLarge413(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	tok := w.newToken(t, "alice")
	big := []byte(strings.Repeat("a", MaxBodyBytes+1))
	declared := func(path string, n int) string {
		return fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n", path, webHost, tok, n)
	}
	// A declared length over the cap is refused before the body is read
	// (only the headers are sent here).
	for _, p := range []string{"/api/v1/tasks", "/api/v1/auth/challenge", "/api/v1/approvals/1/grant"} {
		code, _, b := rawRequest(t, "tcp", w.addr, declared(p, len(big)), nil)
		if code != http.StatusRequestEntityTooLarge || v1Code(b) != "body_too_large" {
			t.Fatalf("%s: %d %s", p, code, b)
		}
	}
	if code, _, b := rawRequest(t, "tcp", w.addr, declared("/tasks", len(big)), nil); code != 413 || flatCode(b) != "body_too_large" {
		t.Fatalf("legacy: %d %s", code, b)
	}
	// A chunked body (no declared length) is cut by MaxBytesReader.
	chunked := fmt.Sprintf("POST /api/v1/tasks HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n"+
		"Transfer-Encoding: chunked\r\nConnection: close\r\n", webHost, tok)
	body := append([]byte(fmt.Sprintf("%x\r\n", len(big))), big...)
	body = append(body, "\r\n0\r\n\r\n"...)
	if code, _, b := rawRequest(t, "tcp", w.addr, chunked, body); code != http.StatusRequestEntityTooLarge || v1Code(b) != "body_too_large" {
		t.Fatalf("chunked: %d %s", code, b)
	}
	// Exactly the cap is read (and then fails as bad JSON, not as too large).
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", string(big[1:]), hdr{"Authorization": "Bearer " + tok, "Content-Type": "application/json"}); code != http.StatusBadRequest {
		t.Fatalf("at the cap: %d %s", code, b)
	}
	// The socket has the same cap.
	sockReq := fmt.Sprintf("POST /api/v1/tasks HTTP/1.1\r\nHost: orchd\r\nContent-Length: %d\r\nConnection: close\r\n", len(big))
	if code, _, b := rawRequest(t, "unix", w.sock, sockReq, nil); code != 413 || v1Code(b) != "body_too_large" {
		t.Fatalf("socket: %d %s", code, b)
	}
	if n := w.taskCount(t); n != 0 {
		t.Fatalf("%d tasks", n)
	}
}

func TestJSONOnly415(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	auth := hdr{"Authorization": "Bearer " + w.newToken(t, "alice")}
	body := w.submitBody("p")
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x",
		"application/json; charset=latin1", "application/json; foo=bar", "application/jsonp", "text/json"} {
		h := with(auth, nil)
		if ct != "" {
			h["Content-Type"] = ct
		}
		code, _, b := w.web(t, "POST", "/api/v1/tasks", body, h)
		if code != http.StatusUnsupportedMediaType || v1Code(b) != "unsupported_media_type" {
			t.Fatalf("%q: %d %s", ct, code, b)
		}
	}
	// A cross-site form cannot reach the login endpoints either.
	if code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", "signer=alice",
		hdr{"Content-Type": "application/x-www-form-urlencoded"}); code != 415 {
		t.Fatalf("form login: %d %s", code, b)
	}
	if n := w.taskCount(t); n != 0 {
		t.Fatalf("%d tasks", n)
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON; Charset=UTF-8"} {
		if code, _, b := w.web(t, "POST", "/api/v1/tasks", body, with(auth, hdr{"Content-Type": ct})); code != http.StatusCreated {
			t.Fatalf("%q: %d %s", ct, code, b)
		}
	}
	// An empty action POST needs no Content-Type (logout).
	tok, csrf := w.newSession(t, "phone")
	if code, _, b := w.web(t, "POST", "/api/v1/auth/logout", "", hdr{"Cookie": cookie(tok), CSRFHeader: csrf, "Origin": webOrigin}); code != 200 {
		t.Fatalf("logout: %d %s", code, b)
	}
	// The socket keeps accepting what it accepted before.
	if code, _, b := w.sockDo(t, "POST", "/api/v1/tasks", body, hdr{"Content-Type": "text/plain"}); code != http.StatusCreated {
		t.Fatalf("socket: %d %s", code, b)
	}
}

func TestRateLimit429(t *testing.T) {
	w := newWebEnv(t)
	alice := with(jsonCT, hdr{TailscaleUserHeader: "alice@example.com"})
	login := `{"signer":"alice"}`
	for i := 0; i < DefaultAuthRatePerKey; i++ {
		if code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", login, alice); code != 200 {
			t.Fatalf("request %d: %d %s", i, code, b)
		}
	}
	code, h, b := w.web(t, "POST", "/api/v1/auth/challenge", login, alice)
	if code != http.StatusTooManyRequests || v1Code(b) != "rate_limited" {
		t.Fatalf("over the limit: %d %s", code, b)
	}
	if ra, err := strconv.Atoi(h.Get("Retry-After")); err != nil || ra < 1 || ra > 60 {
		t.Fatalf("Retry-After %q", h.Get("Retry-After"))
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Fatal("429 without no-store")
	}
	// Every auth route shares the user's bucket.
	if code, _, _ := w.web(t, "GET", "/api/v1/auth/signers", "", alice); code != 429 {
		t.Fatalf("signers: %d", code)
	}
	// Another tailnet user, and a caller without the header, are separate.
	if code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", login, with(jsonCT, hdr{TailscaleUserHeader: "bob@example.com"})); code != 200 {
		t.Fatalf("bob: %d %s", code, b)
	}
	if code, _, b := w.web(t, "POST", "/api/v1/auth/challenge", login, jsonCT); code != 200 {
		t.Fatalf("no header: %d %s", code, b)
	}
	// Non-auth routes and the socket are not limited.
	for i := 0; i < 2*DefaultAuthRatePerKey; i++ {
		if code, _, _ := w.web(t, "GET", "/api/v1/healthz", "", alice); code != 200 {
			t.Fatalf("healthz %d: %d", i, code)
		}
		if code, b := w.do(t, "POST", "/api/v1/auth/challenge", login); code != 200 {
			t.Fatalf("socket %d: %d %s", i, code, b)
		}
	}
}

func TestRateKey(t *testing.T) {
	for _, c := range []struct {
		remote, user, want string
	}{
		{"127.0.0.1:5555", "alice@example.com", "user:alice@example.com"},
		{"[::1]:5555", "alice@example.com", "user:alice@example.com"},
		{"127.0.0.1:5555", "", "addr:127.0.0.1"},
		{"10.0.0.5:5555", "alice@example.com", "addr:10.0.0.5"}, // header trusted from loopback only
		{"127.0.0.1:5555", strings.Repeat("a", 300), "addr:127.0.0.1"},
	} {
		r := httptest.NewRequest("POST", "/api/v1/auth/challenge", nil)
		r.RemoteAddr = c.remote
		if c.user != "" {
			r.Header.Set(TailscaleUserHeader, c.user)
		}
		if got := rateKey(r); got != c.want {
			t.Fatalf("%v: %q", c, got)
		}
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Add(TailscaleUserHeader, "a")
	r.Header.Add(TailscaleUserHeader, "b")
	if got := rateKey(r); got != "addr:127.0.0.1" {
		t.Fatalf("two headers: %q", got)
	}
}

func TestRateLimiterPerKeyAndGlobal(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newRateLimiter(10, 25, 100)
	l.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("a %d refused", i)
		}
	}
	ok, wait := l.allow("a")
	if ok || wait < 5*time.Second || wait > 6*time.Second {
		t.Fatalf("a over: %v %v", ok, wait)
	}
	for i := 0; i < 10; i++ {
		if ok, _ := l.allow("b"); !ok {
			t.Fatalf("b %d refused: keys are isolated", i)
		}
	}
	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("c"); !ok {
			t.Fatalf("c %d refused", i)
		}
	}
	// 25 taken in total: the global ceiling refuses c although c has tokens.
	if ok, wait := l.allow("c"); ok || wait <= 0 || wait > 3*time.Second {
		t.Fatalf("global: %v %v", ok, wait)
	}
	if ok, _ := l.allow("fresh"); ok {
		t.Fatal("a fresh key passed the global ceiling")
	}
	now = now.Add(6 * time.Second) // one token per key, 2.5 global
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("a not refilled")
	}
	if ok, _ := l.allow("a"); ok {
		t.Fatal("a refilled twice")
	}
}

func TestRateLimiterBounded(t *testing.T) {
	l := newRateLimiter(1, 1_000_000, 8)
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 1000; i++ {
		l.allow(fmt.Sprint("k", i))
		if n := l.size(); n > 8 || n != l.lru.Len() {
			t.Fatalf("after %d keys: map %d lru %d", i+1, n, l.lru.Len())
		}
	}
	// The most recent keys are the ones kept.
	if ok, _ := l.allow("k999"); ok {
		t.Fatal("recent key forgotten")
	}
	if _, kept := l.keys["k0"]; kept {
		t.Fatal("oldest key kept")
	}
}

func TestNonLoopbackBindRefused(t *testing.T) {
	old := lookupIPAddr
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "localhost":
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("::1")}}, nil
		case "mixed.test":
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("10.0.0.7")}}, nil
		case "public.test":
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		case "zoned.test":
			return []net.IPAddr{{IP: net.ParseIP("::1"), Zone: "lo0"}}, nil
		case "empty.test":
			return nil, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { lookupIPAddr = old })
	for listen, want := range map[string]string{
		"127.0.0.1:8808": "127.0.0.1:8808",
		"127.1.2.3:1":    "127.1.2.3:1",
		"[::1]:8808":     "[::1]:8808",
		"localhost:8808": "127.0.0.1:8808",
		"127.0.0.1:0":    "127.0.0.1:0",
	} {
		got, err := ResolveLoopbackListen(context.Background(), listen)
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", listen, got, err)
		}
	}
	for _, listen := range []string{":8808", "0.0.0.0:8808", "[::]:8808", "10.0.0.5:8808", "192.168.0.1:80",
		"100.64.0.1:8808", "[fe80::1]:80", "[::ffff:10.0.0.1]:80", "mixed.test:80", "public.test:80", "zoned.test:80",
		"empty.test:80", "nxdomain.test:80", "127.0.0.1", "127.0.0.1:99999", "127.0.0.1:http", "127.0.0.1:-1",
		"127.0.0.1:08808", "", "[::1%lo0]:80"} {
		if got, err := ResolveLoopbackListen(context.Background(), listen); err == nil {
			t.Fatalf("%q accepted as %q", listen, got)
		}
	}
	// The daemon refuses to be built with such an address.
	st, err := store.Open(filepath.Join(shortDir(t), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := Config{Socket: "/tmp/never.sock"}
	withHTTP(&cfg)
	cfg.HTTPListen = "0.0.0.0:0"
	if _, err := NewDaemon(cfg, st, noSteps{}); err == nil {
		t.Fatal("NewDaemon accepted 0.0.0.0")
	}
}

func TestHTTPConfigValidation(t *testing.T) {
	ctx := context.Background()
	if bind, _, err := ValidateHTTPConfig(ctx, "", nil, "", ""); err != nil || bind != "" {
		t.Fatalf("off: %q %v", bind, err)
	}
	bind, hosts, err := ValidateHTTPConfig(ctx, "127.0.0.1:8808", []string{"Console.Example.Test", "127.0.0.1:8808"}, webHost, webOrigin)
	if err != nil || bind != "127.0.0.1:8808" || !hosts[webHost] || !hosts["127.0.0.1"] || len(hosts) != 2 {
		t.Fatalf("ok: %q %v %v", bind, hosts, err)
	}
	for name, c := range map[string]struct {
		hosts        []string
		rpID, origin string
	}{
		"no origin":            {[]string{webHost}, "", ""},
		"no hosts":             {nil, webHost, webOrigin},
		"origin host unlisted": {[]string{"other.example.test"}, webHost, webOrigin},
		"scheme in host":       {[]string{"https://" + webHost}, webHost, webOrigin},
		"path in host":         {[]string{webHost + "/x"}, webHost, webOrigin},
		"empty host":           {[]string{""}, webHost, webOrigin},
		"space in host":        {[]string{webHost, "a b"}, webHost, webOrigin},
		"bad origin":           {[]string{webHost}, webHost, "https://evil.example"},
	} {
		if _, _, err := ValidateHTTPConfig(ctx, "127.0.0.1:8808", c.hosts, c.rpID, c.origin); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The HTTP request path never yields a socket principal, whatever headers it
// forges: the principal comes from the connection type (ConnContext).
func TestHTTPForgedHeadersNeverSocketPrincipal(t *testing.T) {
	w := newWebEnv(t)
	forged := hdr{"Content-Type": "application/json", "Origin": webOrigin,
		"X-Forwarded-For": "127.0.0.1", "X-Real-IP": "127.0.0.1", "Forwarded": "for=127.0.0.1",
		TailscaleUserHeader: "root", "X-Orch-UID": "0", "X-Peer-UID": strconv.Itoa(os.Getuid()), "X-Orch-Principal": "socket"}
	code, h, b := w.web(t, "POST", "/api/v1/admin/enroll", "{}", forged)
	if code != http.StatusUnauthorized || v1Code(b) != "unauthenticated" || h.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("enroll without credentials: %d %s", code, b)
	}
	withTok := with(forged, hdr{"Authorization": "Bearer " + w.newToken(t, "alice")})
	if code, _, b := w.web(t, "POST", "/api/v1/admin/enroll", "{}", withTok); code != http.StatusForbidden || v1Code(b) != "socket_only" {
		t.Fatalf("enroll with a token: %d %s", code, b)
	}
	code, _, b = w.web(t, "GET", "/api/v1/auth/session", "", withTok)
	var s struct{ Principal, Kind string }
	if code != 200 || json.Unmarshal(b, &s) != nil || s.Kind != KindToken || s.Principal != "alice" {
		t.Fatalf("session over HTTP: %d %s", code, b)
	}
	// Control: the socket is a socket principal.
	code, b = w.do(t, "GET", "/api/v1/auth/session", "")
	if code != 200 || json.Unmarshal(b, &s) != nil || s.Kind != KindSocket {
		t.Fatalf("socket session: %d %s", code, b)
	}
}

func TestNoCORSHeaders(t *testing.T) {
	w := newWebEnv(t)
	pre := hdr{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST",
		"Access-Control-Request-Headers": "content-type, x-orch-csrf"}
	check := func(what string, h http.Header) {
		t.Helper()
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Fatalf("%s: CORS header %s", what, k)
			}
		}
	}
	for _, p := range []string{"/api/v1/tasks", "/api/v1/auth/challenge", "/api/v1/auth/webauthn/login/finish", "/tasks"} {
		code, h, b := w.web(t, "OPTIONS", p, "", pre)
		if code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
			t.Fatalf("preflight %s: %d %s", p, code, b)
		}
		check("preflight "+p, h)
	}
	_, h, _ := w.web(t, "GET", "/api/v1/healthz", "", hdr{"Origin": "https://evil.example"})
	check("GET", h)
	_, h, _ = w.web(t, "POST", "/api/v1/auth/challenge", `{"signer":"a"}`, with(jsonCT, hdr{"Origin": webOrigin}))
	check("POST", h)
}

func TestAPIResponsesNoStore(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	bearer := "Bearer " + w.newToken(t, "alice")
	for _, c := range []struct {
		name, method, path, body string
		h                        hdr
		code                     int
	}{
		{"healthz", "GET", "/api/v1/healthz", "", nil, 200},
		{"tasks", "GET", "/api/v1/tasks", "", hdr{"Authorization": bearer}, 200},
		{"legacy", "GET", "/tasks", "", hdr{"Authorization": bearer}, 200},
		{"401", "GET", "/api/v1/tasks", "", nil, 401},
		{"404", "GET", "/api/v1/nope", "", hdr{"Authorization": bearer}, 404},
		{"421", "GET", "/api/v1/tasks", "", hdr{"Host": "evil.example"}, 421},
		{"403", "POST", "/api/v1/tasks", "{}", hdr{"Origin": "https://evil.example", "Content-Type": "application/json"}, 403},
		{"405", "OPTIONS", "/api/v1/tasks", "", nil, 405},
		{"415", "POST", "/api/v1/tasks", "{}", hdr{"Authorization": bearer, "Content-Type": "text/plain"}, 415},
		{"challenge", "POST", "/api/v1/auth/challenge", `{"signer":"a"}`, jsonCT, 200},
	} {
		code, h, b := w.web(t, c.method, c.path, c.body, c.h)
		if code != c.code {
			t.Fatalf("%s: %d %s", c.name, code, b)
		}
		if got := h.Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
			t.Fatalf("%s: Cache-Control %q", c.name, got)
		}
	}
	head := fmt.Sprintf("POST /api/v1/tasks HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\nConnection: close\r\n", webHost, MaxBodyBytes+1)
	if code, h, _ := rawRequest(t, "tcp", w.addr, head, nil); code != 413 || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("413: %d %q", code, h.Get("Cache-Control"))
	}
	if _, h, _ := w.sockDo(t, "GET", "/api/v1/tasks", "", nil); h.Get("Cache-Control") != "no-store" {
		t.Fatal("socket response cacheable")
	}
}

func TestWWWAuthenticateOn401(t *testing.T) {
	w := newWebEnv(t)
	for _, c := range []struct {
		path string
		h    hdr
	}{
		{"/api/v1/tasks", nil},
		{"/tasks", nil},
		{"/api/v1/tasks", hdr{"Authorization": "Bearer nope"}},
		{"/api/v1/tasks", hdr{"Cookie": cookie("nope")}},
	} {
		code, h, b := w.web(t, "GET", c.path, "", c.h)
		if code != 401 || h.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s %v: %d %q %s", c.path, c.h, code, h.Get("WWW-Authenticate"), b)
		}
	}
	if _, h, _ := w.web(t, "GET", "/api/v1/healthz", "", nil); h.Get("WWW-Authenticate") != "" {
		t.Fatal("WWW-Authenticate on 200")
	}
}

func TestSessionCookieName(t *testing.T) {
	if SessionCookieName != "__Host-orch_session" {
		t.Fatalf("cookie name %q", SessionCookieName)
	}
	// The __Host- rules: Secure, Path=/, no Domain.
	rec := httptest.NewRecorder()
	setSessionCookie(rec, "v", 60)
	sc := rec.Header().Get("Set-Cookie")
	if !strings.HasPrefix(sc, "__Host-orch_session=v;") || !strings.Contains(sc, "; Path=/;") ||
		!strings.Contains(sc, "Secure") || strings.Contains(strings.ToLower(sc), "domain") {
		t.Fatalf("Set-Cookie %q", sc)
	}
}

// --session-ttl reaches both the new session's expiry and the sliding touch.
func TestSessionTTLWired(t *testing.T) {
	st, err := store.Open(filepath.Join(shortDir(t), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, bad := range []time.Duration{time.Minute, 25 * time.Hour, -time.Hour} {
		if _, err := NewDaemon(Config{Socket: "/tmp/never.sock", SessionTTL: bad}, st, noSteps{}); err == nil {
			t.Fatalf("session ttl %v accepted", bad)
		}
	}
	for ttl, want := range map[time.Duration]time.Duration{0: SessionIdleTTL, 30 * time.Minute: 30 * time.Minute} {
		d, err := NewDaemon(Config{Socket: "/tmp/never.sock", SessionTTL: ttl}, st, noSteps{})
		if err != nil {
			t.Fatal(err)
		}
		sa, ok := d.handler.auth.(*StoreAuthenticator)
		if d.handler.sessionTTL != want || !ok || sa.IdleTTL != want {
			t.Fatalf("ttl %v: handler %v auth %+v", ttl, d.handler.sessionTTL, d.handler.auth)
		}
	}

	f := newWAFixture(t)
	const ttl = 30 * time.Minute
	f.e.d.handler.sessionTTL = ttl
	f.e.d.handler.auth = &StoreAuthenticator{St: f.e.st, Now: f.clock.now, IdleTTL: ttl}
	s := f.login(t)
	exp, err := time.Parse(time.RFC3339, s.expires)
	if err != nil || exp.Sub(f.clock.now()) > ttl+time.Second || exp.Sub(f.clock.now()) < ttl-time.Minute {
		t.Fatalf("expires %q", s.expires)
	}
	read := func() int {
		code, _, _ := httpDo(t, f.srv, "GET", "/api/v1/tasks", "", hdr{"Cookie": cookie(s.token)})
		return code
	}
	for i := 0; i < 3; i++ { // each read slides the expiry by ttl
		f.clock.advance(20 * time.Minute)
		if code := read(); code != 200 {
			t.Fatalf("read %d: %d", i, code)
		}
	}
	f.clock.advance(ttl + time.Second)
	if code := read(); code != 401 {
		t.Fatalf("idle past ttl: %d", code)
	}
}

func TestPurgeExpiredLoop(t *testing.T) {
	dir := shortDir(t)
	st, err := store.Open(filepath.Join(dir, "db", "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	expired := func(id string) {
		t.Helper()
		if err := st.SaveChallenge(ctx, store.Challenge{ID: id, Purpose: "test", Data: "{}", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	expired("before-start")
	d, err := NewDaemon(Config{Socket: filepath.Join(dir, "o.sock"), Concurrency: 1}, st, noSteps{})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls int
	var removed int64
	d.purgeEvery = 10 * time.Millisecond
	d.purgeHook = func(n int64, err error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		removed += n
	}
	snapshot := func() (int, int64) { mu.Lock(); defer mu.Unlock(); return calls, removed }
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- d.Run(rctx) }()
	<-d.Ready()
	// Once at startup, before the listener answered.
	if c, n := snapshot(); c < 1 || n < 1 {
		t.Fatalf("startup purge: %d calls, %d rows", c, n)
	}
	// Then periodically.
	expired("while-running")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, n := snapshot(); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic purge never removed the second row")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return: purge loop not stopped")
	}
	c1, _ := snapshot()
	time.Sleep(20 * d.purgeEvery)
	if c2, _ := snapshot(); c2 != c1 {
		t.Fatalf("purge kept running after shutdown: %d -> %d calls", c1, c2)
	}
}

func TestShutdownClosesBothListeners(t *testing.T) {
	dir := shortDir(t)
	st, err := store.Open(filepath.Join(dir, "db", "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sock := filepath.Join(dir, "o.sock")
	cfg := Config{Socket: sock, Concurrency: 1}
	withHTTP(&cfg)
	d, err := NewDaemon(cfg, st, noSteps{})
	if err != nil {
		t.Fatal(err)
	}
	d.shutdownTimeout = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	addr := d.HTTPAddr().String()
	w := &webEnv{env: &env{sock: sock, client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}}, addr: addr, hc: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
	if code, _, b := w.web(t, "GET", "/api/v1/healthz", "", nil); code != 200 {
		t.Fatalf("http before: %d %s", code, b)
	}
	if code, b := w.do(t, "GET", "/api/v1/healthz", ""); code != 200 {
		t.Fatalf("socket before: %d %s", code, b)
	}
	// An in-flight request that never finishes its headers.
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fmt.Fprintf(slow, "GET /api/v1/healthz HTTP/1.1\r\nHost: %s\r\n", webHost)
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("shutdown took %v (deadline 300ms)", el)
	}
	// The stuck connection was closed by the server, not left open.
	slow.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := slow.Read(make([]byte, 1)); err == nil {
		t.Fatal("in-flight connection still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("in-flight connection still open (read timed out)")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("HTTP listener still accepts")
	}
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		t.Fatal("socket still accepts")
	}
}
