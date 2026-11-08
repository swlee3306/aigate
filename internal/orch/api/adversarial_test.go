package api

// Adversarial suite (Task 26, E1) for the HTTP listener: everything goes
// through the daemon's real loopback listener and middleware chain unless a
// test says it targets one middleware with a stub handler. Each test keeps an
// accepted control next to its refusals.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"aigate/internal/approval/signing"
	"aigate/internal/gitx"
)

// Review point 5 (CSRF): cross-origin requests, the opaque "null" origin, a
// cookie with neither Origin nor Sec-Fetch-Site, and a missing, wrong or
// foreign CSRF header are all refused, and none of them changes anything.
func TestCSRFCrossOriginRejected(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	tok, csrf := w.newSession(t, "phone")
	_, otherCSRF := w.newSession(t, "laptop") // a different session's token
	body := w.submitBody("p")
	base := hdr{"Content-Type": "application/json", "Cookie": cookie(tok), "Origin": webOrigin, CSRFHeader: csrf}
	without := func(h hdr, keys ...string) hdr {
		out := with(h, nil)
		for _, k := range keys {
			delete(out, k)
		}
		return out
	}
	originCases := map[string]hdr{
		"cross-origin Origin":         with(base, hdr{"Origin": "https://evil.example"}),
		"null Origin":                 with(base, hdr{"Origin": "null"}),
		"http scheme of the console":  with(base, hdr{"Origin": "http://" + webHost}),
		"sibling host":                with(base, hdr{"Origin": "https://evil." + webHost}),
		"no Origin, cookie only":      without(base, "Origin"),
		"no Origin, cross-site fetch": with(without(base, "Origin"), hdr{"Sec-Fetch-Site": "cross-site"}),
	}
	csrfCases := map[string]hdr{
		"missing CSRF header":       without(base, CSRFHeader),
		"empty CSRF header":         with(base, hdr{CSRFHeader: ""}),
		"wrong CSRF header":         with(base, hdr{CSRFHeader: "csrf-wrong"}),
		"another session's CSRF":    with(base, hdr{CSRFHeader: otherCSRF + "-x"}),
		"CSRF value with a suffix":  with(base, hdr{CSRFHeader: csrf + "x"}),
		"session id as CSRF header": with(base, hdr{CSRFHeader: tok}),
	}
	writes := []struct{ method, path, body string }{
		{"POST", "/api/v1/tasks", body},
		{"POST", "/api/v1/tasks/1/cancel", ""},
		{"POST", "/api/v1/approvals/1/grant", `{"signer":"phone","alg":"webauthn-es256","sig":"x","issued":"2026-11-09T12:00:00Z"}`},
		{"POST", "/api/v1/auth/logout", ""},
	}
	for name, h := range originCases {
		for _, wr := range writes {
			code, _, b := w.web(t, wr.method, wr.path, wr.body, h)
			if code != http.StatusForbidden || v1Code(b) != "bad_origin" || strings.Contains(string(b), "evil") {
				t.Fatalf("%s %s %s: %d %s", name, wr.method, wr.path, code, b)
			}
		}
	}
	for name, h := range csrfCases {
		for _, wr := range writes {
			code, _, b := w.web(t, wr.method, wr.path, wr.body, h)
			if code != http.StatusForbidden || v1Code(b) != "csrf_failed" {
				t.Fatalf("%s %s %s: %d %s", name, wr.method, wr.path, code, b)
			}
		}
	}
	// Two CSRF headers, one of them right, are refused.
	req, _ := http.NewRequest("POST", "http://"+w.addr+"/api/v1/tasks", strings.NewReader(body))
	req.Host = webHost
	for k, v := range without(base, CSRFHeader) {
		req.Header.Set(k, v)
	}
	req.Header.Add(CSRFHeader, csrf)
	req.Header.Add(CSRFHeader, "csrf-wrong")
	resp, err := w.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || v1Code(b) != "csrf_failed" {
		t.Fatalf("two CSRF headers: %d %s", resp.StatusCode, b)
	}
	// GET /auth/session rotates the CSRF token, so it is origin-checked too.
	for _, h := range []hdr{{"Origin": "https://evil.example"}, {"Origin": "null"}, {}} {
		if code, _, b := w.web(t, "GET", "/api/v1/auth/session", "", with(hdr{"Cookie": cookie(tok)}, h)); code != http.StatusForbidden {
			t.Fatalf("session rotation with %v: %d %s", h, code, b)
		}
	}
	if n := w.taskCount(t); n != 0 {
		t.Fatalf("refused requests created %d tasks", n)
	}
	// None of the refused logouts ended the session: the control write works
	// with the original CSRF token.
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", body, base); code != http.StatusCreated {
		t.Fatalf("control: %d %s", code, b)
	}
}

// Review point 5 (DNS rebinding): a Host that is not the console's name is
// refused before any handler runs, including writes with a valid session.
//
// The Host check ignores the port by design (Task 21: tailscale serve
// forwards the browser's Host, with or without :443), so "port mismatch" is
// covered as a foreign host on the listener's own port, and the console name
// on another port is accepted (asserted below so a change is noticed).
func TestHostRebindingRejected(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	tok, csrf := w.newSession(t, "phone")
	port := w.addr[strings.LastIndex(w.addr, ":")+1:]
	write := hdr{"Content-Type": "application/json", "Cookie": cookie(tok), "Origin": webOrigin, CSRFHeader: csrf}
	for _, host := range []string{
		"evil.example",
		"evil.example:" + port,
		"127.0.0.1",
		"127.0.0.1:" + port,
		"[::1]:" + port,
		"localhost:" + port,
		"0.0.0.0",
		webHost + ".",         // trailing dot: a different name to the allowlist
		webHost + ".:" + port, // ... with the port
		"evil." + webHost,     // subdomain
		webHost + ".evil.example",
		"xn--console-example-test",
	} {
		for _, c := range []struct {
			method, path, body string
			h                  hdr
		}{
			{"GET", "/api/v1/healthz", "", nil},
			{"GET", "/api/v1/tasks", "", hdr{"Cookie": cookie(tok)}},
			{"POST", "/api/v1/tasks", w.submitBody("p"), write},
			{"POST", "/api/v1/auth/challenge", `{"signer":"alice"}`, jsonCT},
			{"GET", "/", "", nil},
		} {
			code, _, b := w.web(t, c.method, c.path, c.body, with(c.h, hdr{"Host": host}))
			if code != http.StatusMisdirectedRequest || strings.Contains(string(b), host) {
				t.Fatalf("Host %q %s %s: %d %s", host, c.method, c.path, code, b)
			}
		}
	}
	// Two Host headers: Go's server refuses the request itself (400).
	head := fmt.Sprintf("GET /api/v1/healthz HTTP/1.1\r\nHost: %s\r\nHost: evil.example\r\nConnection: close\r\n", webHost)
	if code, _, b := rawRequest(t, "tcp", w.addr, head, nil); code != http.StatusBadRequest {
		t.Fatalf("duplicate Host: %d %s", code, b)
	}
	// An absolute request URI names the host the request is for; a foreign one
	// is refused even with an allowed Host header.
	head = fmt.Sprintf("GET http://evil.example/api/v1/healthz HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", webHost)
	if code, _, b := rawRequest(t, "tcp", w.addr, head, nil); code != http.StatusMisdirectedRequest && code != http.StatusBadRequest {
		t.Fatalf("absolute URI with a foreign host: %d %s", code, b)
	}
	// Malformed Host values never pass.
	for _, host := range []string{webHost + "@evil.example", "evil.example#" + webHost, webHost + "/x"} {
		head := fmt.Sprintf("GET /api/v1/healthz HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", host)
		if code, _, b := rawRequest(t, "tcp", w.addr, head, nil); code != http.StatusMisdirectedRequest && code != http.StatusBadRequest {
			t.Fatalf("Host %q: %d %s", host, code, b)
		}
	}
	if n := w.taskCount(t); n != 0 {
		t.Fatalf("%d tasks created through a foreign Host", n)
	}
	// Controls: the console name, any case, with or without a port.
	for _, host := range []string{webHost, strings.ToUpper(webHost), webHost + ":443", webHost + ":" + port, webHost + ":9999"} {
		if code, _, b := w.web(t, "GET", "/api/v1/healthz", "", hdr{"Host": host}); code != http.StatusOK {
			t.Fatalf("control Host %q: %d %s", host, code, b)
		}
	}
	if code, _, b := w.web(t, "POST", "/api/v1/tasks", w.submitBody("p"), write); code != http.StatusCreated {
		t.Fatalf("control write: %d %s", code, b)
	}
}

// Bodies over 1 MiB are refused on both listeners (declared or chunked); a
// huge protected diff is cut server-side to 256 KiB with truncated=true while
// the hash stays the full one; a change with many files is served whole.
func TestBodyAndDiffCaps(t *testing.T) {
	w := newWebEnv(t)
	w.d.pauseWorkers()
	bearer := w.newToken(t, "alice")
	big := []byte(strings.Repeat("a", MaxBodyBytes+1))
	declared := fmt.Sprintf("POST /api/v1/tasks HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n", webHost, bearer, len(big))
	chunkedBody := append([]byte(fmt.Sprintf("%x\r\n", len(big))), big...)
	chunkedBody = append(chunkedBody, "\r\n0\r\n\r\n"...)
	chunked := fmt.Sprintf("POST /api/v1/tasks HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n"+
		"Transfer-Encoding: chunked\r\nConnection: close\r\n", webHost, bearer)
	sockDeclared := fmt.Sprintf("POST /api/v1/tasks HTTP/1.1\r\nHost: orchd\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n", len(big))
	sockChunked := "POST /api/v1/tasks HTTP/1.1\r\nHost: orchd\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n"
	for _, c := range []struct {
		name, network, addr, head string
		body                      []byte
	}{
		{"http declared", "tcp", w.addr, declared, nil},
		{"http chunked", "tcp", w.addr, chunked, chunkedBody},
		{"socket declared", "unix", w.sock, sockDeclared, nil},
		{"socket chunked", "unix", w.sock, sockChunked, chunkedBody},
	} {
		code, _, b := rawRequest(t, c.network, c.addr, c.head, c.body)
		if code != http.StatusRequestEntityTooLarge || v1Code(b) != "body_too_large" {
			t.Fatalf("%s: %d %.200s", c.name, code, b)
		}
	}
	if n := w.taskCount(t); n != 0 {
		t.Fatalf("%d tasks from oversized bodies", n)
	}

	// Huge protected diff, read over HTTP with the token.
	huge := strings.Repeat("0123456789abcdef한\n", 40_000) // ~800 KiB of hashed text
	id, _, _ := diffTask(t, w.env, func() { putFile(t, w.repo, "tests/spec.txt", huge) })
	code, _, b := w.web(t, "GET", "/api/v1/tasks/"+strconv.FormatInt(id, 10)+"/diff", "", hdr{"Authorization": "Bearer " + bearer})
	var v DiffView
	if code != http.StatusOK || json.Unmarshal(b, &v) != nil {
		t.Fatalf("diff: %d %.200s", code, b)
	}
	if !v.Truncated || len(v.HashedText) > maxDiffBytes || len(v.HashedText) < maxDiffBytes-4 || !utf8.ValidString(v.HashedText) {
		t.Fatalf("cut: truncated=%v len=%d", v.Truncated, len(v.HashedText))
	}
	if len(b) > maxDiffBytes+64<<10 {
		t.Fatalf("response is %d bytes", len(b))
	}
	if v.Hash == "" || v.Hash == gitx.HashOf(v.HashedText) {
		t.Fatal("hash must be the full diff's, not the cut text's")
	}
	if len(v.ProtectedFiles) != 1 || v.ProtectedFiles[0] != "tests/spec.txt" {
		t.Fatalf("protected %q", v.ProtectedFiles)
	}

	// Many files in one change: every file is listed, and every modified
	// protected file is listed as needing approval (new files under tests/**
	// are not "modified protected" for the gate, so the guarded ones exist in
	// the base and are rewritten).
	const plain, guarded = 1500, 300
	for i := 0; i < guarded; i++ {
		putFile(t, w.repo, fmt.Sprintf("tests/many/g%04d.txt", i), "y\n")
	}
	id2, _, _ := diffTask(t, w.env, func() {
		for i := 0; i < plain; i++ {
			putFile(t, w.repo, fmt.Sprintf("src/many/f%04d.txt", i), "x\n")
		}
		for i := 0; i < guarded; i++ {
			putFile(t, w.repo, fmt.Sprintf("tests/many/g%04d.txt", i), "z\n")
		}
	})
	code, _, b = w.web(t, "GET", "/api/v1/tasks/"+strconv.FormatInt(id2, 10)+"/diff", "", hdr{"Authorization": "Bearer " + bearer})
	var v2 DiffView
	if code != http.StatusOK || json.Unmarshal(b, &v2) != nil {
		t.Fatalf("many-files diff: %d %.200s", code, b)
	}
	if len(v2.Files) != plain+guarded || len(v2.ProtectedFiles) != guarded {
		t.Fatalf("files %d (want %d), protected %d (want %d)", len(v2.Files), plain+guarded, len(v2.ProtectedFiles), guarded)
	}
}

// Task 21 minor 1: limitBody wraps the body in MaxBytesReader, so a chunked
// body (no declared length) cannot exceed the cap inside a handler either.
func TestLimitBodyWrapsChunkedBody(t *testing.T) {
	var calls int
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		n, err := io.Copy(io.Discard, r.Body)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			fmt.Fprintf(w, "limit %d", mbe.Limit)
			return
		}
		fmt.Fprintf(w, "read %d", n)
	})
	srv := httptest.NewServer(limitBody(MaxBodyBytes, stub))
	defer srv.Close()
	post := func(r io.Reader, size int64) (int, string) {
		t.Helper()
		req, err := http.NewRequest("POST", srv.URL+"/x", r)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = size // -1: unknown, sent chunked
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// MultiReader hides the length, so the client sends the body chunked.
	chunk := func(n int) io.Reader { return io.MultiReader(strings.NewReader(strings.Repeat("a", n))) }
	if code, b := post(chunk(MaxBodyBytes+1), -1); code != http.StatusRequestEntityTooLarge || b != fmt.Sprintf("limit %d", MaxBodyBytes) {
		t.Fatalf("chunked over the cap: %d %q", code, b)
	}
	if code, b := post(chunk(4*MaxBodyBytes), -1); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked far over the cap: %d %q", code, b)
	}
	if code, b := post(chunk(MaxBodyBytes), -1); code != http.StatusOK || b != fmt.Sprintf("read %d", MaxBodyBytes) {
		t.Fatalf("chunked at the cap: %d %q", code, b)
	}
	// A declared length over the cap never reaches the handler.
	before := calls
	if code, _ := post(strings.NewReader(strings.Repeat("a", MaxBodyBytes+1)), MaxBodyBytes+1); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared over the cap: %d", code)
	}
	if calls != before {
		t.Fatal("handler ran for a declared oversized body")
	}
}

// Task 21 minor 2: without Origin, exactly one Sec-Fetch-Site value is
// accepted; a repeated header (even with an allowed value) is refused.
func TestSecFetchSiteSingleValue(t *testing.T) {
	h := originCheck(webOrigin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	do := func(vals ...string) int {
		r := httptest.NewRequest("POST", "/api/v1/tasks", strings.NewReader("{}"))
		for _, v := range vals {
			r.Header.Add("Sec-Fetch-Site", v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for _, vals := range [][]string{
		{"same-origin", "cross-site"},
		{"cross-site", "same-origin"},
		{"same-origin", "same-origin"},
		{"none", "none"},
		{"cross-site"},
		{"same-site"},
		{""},
		{"Same-Origin"},
	} {
		if code := do(vals...); code != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site %q: %d", vals, code)
		}
	}
	for _, v := range []string{"same-origin", "none"} {
		if code := do(v); code != http.StatusNoContent {
			t.Fatalf("control %q: %d", v, code)
		}
	}
}

// CF18 / Task 20 M1: activePasskey re-checks the signer type, so an id that
// names an ssh (or unknown-type) signer is never a passkey, whatever its
// other fields hold. Registry data is built in memory (unvalidated) on
// purpose: LoadRegistry would refuse most of these.
func TestActivePasskeyTypeRecheck(t *testing.T) {
	reg := signing.Registry{Signers: []signing.Signer{
		{ID: "phone", Type: signing.TypeWebAuthnES256, CredentialID: "Y3JlZA", PublicKey: "x", RPID: webHost, Origin: webOrigin},
		{ID: "mac", Type: signing.TypeSSHEd25519, PublicKey: "ssh-ed25519 AAAA"},
		{ID: "mixed", Type: signing.TypeSSHEd25519, CredentialID: "Y3JlZA", RPID: webHost, Origin: webOrigin},
		{ID: "untyped", Type: "", CredentialID: "Y3JlZA"},
		{ID: "upper", Type: "WEBAUTHN-ES256", CredentialID: "Y3JlZA"},
	}}
	if s, ok := activePasskey(reg, "phone"); !ok || s.ID != "phone" {
		t.Fatalf("phone: %+v %v", s, ok)
	}
	for _, id := range []string{"mac", "mixed", "untyped", "upper", "nobody", ""} {
		if s, ok := activePasskey(reg, id); ok {
			t.Fatalf("%q accepted as a passkey: %+v", id, s)
		}
	}
	if _, ok := activePasskey(signing.Registry{}, "phone"); ok {
		t.Fatal("empty registry")
	}
}
