package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestStaticServedWithCSPAndTypes goes through the real HTTP listener stack:
// the console files carry the security headers and the right types, the Host
// check applies to them, and the unix socket never serves them.
func TestStaticServedWithCSPAndTypes(t *testing.T) {
	w := newWebEnv(t)
	for _, c := range []struct{ path, ctype string }{
		{"/", "text/html; charset=utf-8"},
		{"/index.html", "text/html; charset=utf-8"},
		{"/app.js", "text/javascript; charset=utf-8"},
		{"/api.js", "text/javascript; charset=utf-8"},
		{"/render.js", "text/javascript; charset=utf-8"},
		{"/style.css", "text/css; charset=utf-8"},
		{"/tasks/38", "text/html; charset=utf-8"}, // SPA route
	} {
		code, h, b := w.web(t, "GET", c.path, "", nil)
		if code != 200 || len(b) == 0 {
			t.Fatalf("%s: %d (%d bytes)", c.path, code, len(b))
		}
		if got := h.Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ctype)
		}
		if got := h.Values("Content-Security-Policy"); len(got) != 1 || got[0] != ContentSecurityPolicy {
			t.Errorf("%s: CSP %q", c.path, got)
		}
		for k, v := range map[string]string{
			"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
			"Cross-Origin-Resource-Policy": "same-origin", "Strict-Transport-Security": HSTSValue,
			"Cache-Control": "no-cache",
		} {
			if got := h.Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", c.path, k, got, v)
			}
		}
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header on a static file", c.path)
		}
	}
	// The CSP forbids what the page must not need: inline script and style.
	code, _, b := w.web(t, "GET", "/", "", nil)
	if code != 200 || !strings.Contains(string(b), `<script type="module" src="/app.js">`) {
		t.Fatalf("index does not load the app module: %d", code)
	}
	if strings.Contains(ContentSecurityPolicy, "unsafe-inline") || strings.Contains(ContentSecurityPolicy, "unsafe-eval") {
		t.Fatal("CSP allows inline/eval")
	}
	// HEAD works through the stack
	if code, _, b := w.web(t, "HEAD", "/app.js", "", nil); code != 200 || len(b) != 0 {
		t.Errorf("HEAD /app.js: %d, %d body bytes", code, len(b))
	}
	// the Host allowlist covers static files too (DNS rebinding)
	if code, _, _ := w.web(t, "GET", "/", "", hdr{"Host": "evil.example"}); code != http.StatusMisdirectedRequest {
		t.Errorf("static with a foreign Host: %d, want 421", code)
	}
	// Writes to a static path never reach the console or the API.
	if code, _, _ := w.web(t, "POST", "/", "{}", with(jsonCT, hdr{"Origin": webOrigin})); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d, want 405", code)
	}
	if code, _, _ := w.web(t, "POST", "/", "{}", with(jsonCT, hdr{"Origin": "https://evil.example"})); code != http.StatusForbidden {
		t.Errorf("cross-origin POST /: %d, want 403", code)
	}
	// The unix socket is not a web server.
	if code, _, b := w.sockDo(t, "GET", "/", "", nil); strings.Contains(string(b), "<!doctype html>") {
		t.Errorf("socket / served the console: %d", code)
	}
	if code, _, b := w.sockDo(t, "GET", "/app.js", "", nil); code == 200 || strings.Contains(string(b), "export") {
		t.Errorf("socket /app.js served the script: %d", code)
	}
}

var doctypeRe = regexp.MustCompile(`(?i)<!doctype|<html`)

// TestSPAFallbackNotForAPI: unknown /api paths keep the daemon's JSON errors
// and never fall back to index.html, with and without a credential.
func TestSPAFallbackNotForAPI(t *testing.T) {
	w := newWebEnv(t)
	bearer := hdr{"Authorization": "Bearer " + w.newToken(t, "alice")}
	for _, c := range []struct {
		name, path string
		h          hdr
		code       int
		errCode    string
	}{
		{"authenticated unknown", "/api/v1/nope", bearer, 404, "not_found"},
		{"authenticated deep", "/api/v1/tasks/1/nope/x", bearer, 404, "not_found"},
		{"anonymous unknown", "/api/v1/nope", nil, 401, "unauthenticated"},
		{"api root", "/api/", bearer, 404, ""},
		{"api bare", "/api", bearer, 404, ""},
		{"other version", "/api/v2/tasks", bearer, 404, ""},
		{"encoded slash", "/api%2Fv1/nope", bearer, 404, "not_found"}, // decodes to /api/v1/nope
	} {
		code, h, b := w.web(t, "GET", c.path, "", c.h)
		if code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, b, c.code)
		}
		if doctypeRe.Match(b) {
			t.Errorf("%s: API path answered with the console page", c.name)
		}
		if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type %q", c.name, ct)
		}
		if got := h.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q", c.name, got)
		}
		if strings.HasPrefix(c.path, "/api/v1/") {
			var env struct {
				Error struct{ Code, Message string }
			}
			if err := json.Unmarshal(b, &env); err != nil || env.Error.Code != c.errCode || env.Error.Message == "" {
				t.Errorf("%s: body %s is not the {error:{code,message}} envelope with code %q", c.name, b, c.errCode)
			}
		}
	}
	// A page route, by contrast, is the console.
	if code, h, b := w.web(t, "GET", "/approvals", "", nil); code != 200 || !doctypeRe.Match(b) || !strings.HasPrefix(h.Get("Content-Type"), "text/html") {
		t.Errorf("/approvals: %d %q", code, h.Get("Content-Type"))
	}
	// The API is still reachable and unaffected beside the console.
	if code, _, b := w.web(t, "GET", "/api/v1/healthz", "", nil); code != 200 || doctypeRe.Match(b) {
		t.Errorf("healthz: %d %s", code, b)
	}
}
