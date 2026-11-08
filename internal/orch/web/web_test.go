package web

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// staticFiles returns every embedded file by its path under static/.
func staticFiles(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		out[strings.TrimPrefix(p, "static/")] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWebAssetsEmbedded(t *testing.T) {
	files := staticFiles(t)
	for _, need := range []string{"index.html", "app.js", "api.js", "render.js", "router.js", "style.css", "views/tasks.js", "views/task.js", "views/diff.js", "views/approvals.js", "views/login.js", "webauthn.js"} {
		if len(files[need]) == 0 {
			t.Errorf("static/%s is missing or empty", need)
		}
	}
	stray := regexp.MustCompile(`(^|/)[.~]|\.(map|bak|orig|swp|tmp|test\.js)$|(^|/)(package\.json|node_modules)`)
	for name := range files {
		if stray.MatchString(name) {
			t.Errorf("stray file embedded: static/%s", name)
		}
		if _, ok := contentTypes[strings.ToLower(path.Ext(name))]; !ok {
			t.Errorf("static/%s has no known content type", name)
		}
	}
	// the served table covers exactly the embedded files
	assets, err := load(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != len(files) {
		t.Errorf("served %d assets, embedded %d files", len(assets), len(files))
	}

	html := string(files["index.html"])
	if !strings.Contains(html, `lang="ko"`) {
		t.Error("index.html lacks lang=\"ko\"")
	}
	if !strings.Contains(html, `class="skip"`) {
		t.Error("index.html lacks the skip link")
	}
	for _, l := range []string{"<main", "<nav", "<header", "<aside"} {
		if !strings.Contains(html, l) {
			t.Errorf("index.html lacks landmark %s", l)
		}
	}
	scriptRe := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	scripts := scriptRe.FindAllStringSubmatch(html, -1)
	if len(scripts) == 0 {
		t.Error("index.html loads no script")
	}
	srcRe := regexp.MustCompile(`(?i)\bsrc\s*=\s*"([^"]*)"`)
	for _, m := range scripts {
		if strings.TrimSpace(m[2]) != "" {
			t.Errorf("inline script body in index.html: %q", m[2])
		}
		src := srcRe.FindStringSubmatch(m[1])
		if src == nil {
			t.Errorf("script without src: %q", m[1])
			continue
		}
		if !strings.Contains(m[1], `type="module"`) {
			t.Errorf("script %q is not a module", src[1])
		}
	}
	for _, bad := range []struct{ re, what string }{
		{`(?i)<style\b`, "inline <style>"},
		{`(?i)\sstyle\s*=`, "style= attribute"},
		{`(?i)\son[a-z]+\s*=`, "on* handler attribute"},
		{`(?i)javascript:`, "javascript: URL"},
		{`(?i)(src|href|action)\s*=\s*"(https?:)?//`, "external URL"},
		{`(?i)<(iframe|object|embed|base|form)\b`, "forbidden element"},
	} {
		if regexp.MustCompile(bad.re).MatchString(html) {
			t.Errorf("index.html contains %s", bad.what)
		}
	}
	// every same-origin src/href points at an existing asset or a hash
	refRe := regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*"([^"]*)"`)
	for _, m := range refRe.FindAllStringSubmatch(html, -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "#") {
			continue
		}
		if !strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "//") {
			t.Errorf("index.html reference %q is not same-origin absolute", ref)
			continue
		}
		if _, ok := files[strings.TrimPrefix(ref, "/")]; !ok {
			t.Errorf("index.html references %q which is not embedded", ref)
		}
	}
	// static JS imports resolve to embedded files
	impRe := regexp.MustCompile(`(?m)^\s*(?:import|export)\b[^'"\n]*?from\s+'([^']+)'`)
	for name, b := range files {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		for _, m := range impRe.FindAllStringSubmatch(string(b), -1) {
			target := path.Join(path.Dir(name), m[1])
			if _, ok := files[target]; !ok {
				t.Errorf("static/%s imports %q: static/%s is not embedded", name, m[1], target)
			}
		}
	}
}

// TestNoForbiddenSinksInStaticJS keeps HTML-parsing, dynamic-code and
// storage APIs out of every shipped script: agent text must only ever reach
// the page as text nodes, and the CSRF token must never touch storage.
func TestNoForbiddenSinksInStaticJS(t *testing.T) {
	forbidden := []string{
		"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment",
		"DOMParser", "srcdoc", "eval(", "new Function", "Function(", `setTimeout("`, `setTimeout('`, "setTimeout(`",
		`setInterval("`, `setInterval('`, "setInterval(`", "javascript:", "localStorage", "sessionStorage",
		"document.cookie", "indexedDB", ".style.cssText", `setAttribute('style'`, `setAttribute("style"`, "importScripts",
	}
	n := 0
	for name, b := range staticFiles(t) {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		n++
		for _, f := range forbidden {
			if bytes.Contains(b, []byte(f)) {
				t.Errorf("static/%s contains forbidden %q", name, f)
			}
		}
		// the timer APIs may only be given functions: setTimeout(run, ...)
		if regexp.MustCompile(`set(Timeout|Interval)\(\s*(["'` + "`" + `])`).Match(b) {
			t.Errorf("static/%s passes a string to a timer", name)
		}
	}
	if n < 8 {
		t.Fatalf("only %d scripts scanned", n)
	}
}

// TestSigningCodeNeverReadsClock: the signed `issued` is the server's
// detail.issued; the signing and approval code must not read the clock.
func TestSigningCodeNeverReadsClock(t *testing.T) {
	files := staticFiles(t)
	for _, name := range []string{"webauthn.js", "views/approvals.js"} {
		b, ok := files[name]
		if !ok {
			t.Fatalf("static/%s missing", name)
		}
		for _, f := range []string{"Date.now", "new Date", "Date(", "performance.now"} {
			if bytes.Contains(b, []byte(f)) {
				t.Errorf("static/%s contains %q", name, f)
			}
		}
	}
}

func TestHandlerTypesAndCaching(t *testing.T) {
	h := Handler()
	cases := []struct{ path, ctype string }{
		{"/", "text/html; charset=utf-8"},
		{"/index.html", "text/html; charset=utf-8"},
		{"/app.js", "text/javascript; charset=utf-8"},
		{"/render.js", "text/javascript; charset=utf-8"},
		{"/style.css", "text/css; charset=utf-8"},
	}
	for _, c := range cases {
		rec := get(t, h, "GET", c.path)
		if rec.Code != 200 {
			t.Errorf("%s: %d", c.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ctype)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q", c.path, got)
		}
		if strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s: immutable caching on an unhashed file", c.path)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("%s: no ETag", c.path)
		}
	}
	// revalidation
	rec := get(t, h, "GET", "/app.js")
	req := httptest.NewRequest("GET", "/app.js", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d, want 304", rec2.Code)
	}
}

func TestSPAFallback(t *testing.T) {
	h := Handler()
	index := get(t, h, "GET", "/").Body.String()
	for _, p := range []string{"/tasks", "/tasks/38", "/approvals", "/nope/deeper/path", "/views/", "/app.js.map", "/static/app.js"} {
		rec := get(t, h, "GET", p)
		if rec.Code != 200 || rec.Body.String() != index {
			t.Errorf("%s: %d, want index.html with 200", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", p, ct)
		}
	}
}

func TestHEADAndMethods(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/app.js", "/some/route"} {
		rec := get(t, h, "HEAD", p)
		if rec.Code != 200 || rec.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d with %d body bytes", p, rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("Content-Type") == "" || rec.Header().Get("Content-Length") == "" {
			t.Errorf("HEAD %s lacks Content-Type/Content-Length: %v", p, rec.Header())
		}
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		rec := get(t, h, m, "/")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /: %d, want 405", m, rec.Code)
		}
		if rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s /: Allow %q", m, rec.Header().Get("Allow"))
		}
		if m != "HEAD" && strings.Contains(rec.Body.String(), "<!doctype") {
			t.Errorf("%s / served the page", m)
		}
	}
}

func TestPathTraversalSafe(t *testing.T) {
	h := Handler()
	index := get(t, h, "GET", "/").Body.String()
	// A file that exists outside static/ and must never be served.
	secret, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{
		"/../web.go", "/%2e%2e/web.go", "/%2E%2E/%2E%2E/web.go", "//../web.go", "/./../web.go",
		"/static/../web.go", "/..%2fweb.go", "/%2e%2e%2fweb.go", "/app.js/../../web.go", "//web.go",
		"/..\\web.go", "/%5c..%5cweb.go", "/static/web.go", "/web.go", "/../../../../etc/passwd",
		"/%00", "/app.js%00.html", "/.git/config", "/..;/web.go", "///app.js/..//..//web.go",
	}
	for _, p := range targets {
		req := httptest.NewRequest("GET", "/", nil)
		req.URL.Path = ""
		u := httptest.NewRequest("GET", p, nil)
		req.URL = u.URL
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Body)
		if bytes.Contains(body, secret[:64]) || bytes.Contains(body, []byte("root:")) {
			t.Errorf("%s leaked a file outside static/", p)
		}
		switch rec.Code {
		case 200:
			// an existing asset ("//app.js" cleans to /app.js) or the SPA index
			clean := path.Clean("/" + u.URL.Path)
			want := index
			if a, err := staticFS.ReadFile("static" + clean); err == nil && clean != "/" {
				want = string(a)
			}
			if string(body) != want {
				t.Errorf("%s: 200 with an unexpected body (%d bytes)", p, len(body))
			}
		default:
			t.Errorf("%s: status %d, want 200 (index or asset)", p, rec.Code)
		}
	}
}

func TestLoadRefusesUnknownType(t *testing.T) {
	m := fstestMap{"static/x.exe": "MZ"}
	if _, err := load(m.fs(), "static"); err == nil {
		t.Error("an embedded file of unknown type must be refused")
	}
}

// TestWebJS runs the browser-side tests with node's built-in runner (no npm,
// no network). Skipped when node is not installed.
func TestWebJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	files, err := filepath.Glob(filepath.Join("test", "*.test.js"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no JS tests found (%v)", err)
	}
	ver, _ := exec.Command(node, "--version").Output()
	t.Logf("node %s", strings.TrimSpace(string(ver)))
	// Files are passed explicitly: newer node versions treat a directory
	// argument as a module path, not as a directory to scan.
	cmd := exec.Command(node, append([]string{"--test"}, files...)...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test failed: %v\n%s", err, out)
	}
	t.Logf("%s", lastLines(string(out), 8))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
