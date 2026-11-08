package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTok(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

func runArgs(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	code := run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestCLIAgainstHTTPServer(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	tf := writeTok(t, "s3cret-token\n", 0o600)
	code, out, errS := runArgs("--server", srv.URL, "--token-file", tf, "list")
	if code != 0 || !strings.Contains(out, "작업이 없습니다") {
		t.Fatalf("list %d %q %q", code, out, errS)
	}
	if gotPath != "/api/v1/tasks" {
		t.Fatalf("path %q", gotPath)
	}
	if gotAuth != "Bearer s3cret-token" {
		t.Fatalf("auth %q", gotAuth)
	}
}

func TestCLIErrorEnvelopeMessage(t *testing.T) {
	cases := map[string]string{
		"envelope": `{"error":{"code":"conflict","message":"이미 끝난 작업입니다"}}`,
		"flat":     `{"error":"이미 끝난 작업입니다"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(409)
				w.Write([]byte(body))
			}))
			defer srv.Close()
			tf := writeTok(t, "t", 0o600)
			code, _, errS := runArgs("--server", srv.URL, "--token-file", tf, "cancel", "1")
			if code != 1 || errS != "오류: 이미 끝난 작업입니다\n" {
				t.Fatalf("%d %q", code, errS)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer srv.Close()
	tf := writeTok(t, "t", 0o600)
	code, _, errS := runArgs("--server", srv.URL, "--token-file", tf, "cancel", "1")
	if code != 1 || !strings.Contains(errS, "HTTP 502") || strings.Contains(errS, "html") {
		t.Fatalf("non-json %d %q", code, errS)
	}
}

func TestRemoteClientRefusesInsecureServer(t *testing.T) {
	tok := func() (string, error) { return "t", nil }
	for _, s := range []string{"http://example.com", "http://127.0.0.1.evil.com", "ftp://x", "orch-vm", "http://localhost.evil.com"} {
		if _, err := newRemoteClient(s, tok).call("GET", "/tasks", nil); err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"https://orch.example.ts.net", "http://127.0.0.1:1234", "http://localhost:80"} {
		if c := newRemoteClient(s, tok); c.err != nil {
			t.Errorf("%s: %v", s, c.err)
		}
	}
}

func TestRemoteClientTimeoutAndNoInsecureTLS(t *testing.T) {
	c := newRemoteClient("https://orch.example.ts.net", func() (string, error) { return "t", nil })
	if c.hc.Timeout.Seconds() != 30 {
		t.Fatalf("timeout %v", c.hc.Timeout)
	}
	if tr, ok := c.hc.Transport.(*http.Transport); !ok || (tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify) {
		t.Fatalf("transport %#v", c.hc.Transport)
	}
}

func TestFileToken(t *testing.T) {
	tok, err := fileToken(writeTok(t, "  abc123\n", 0o600))()
	if err != nil || tok != "abc123" {
		t.Fatalf("%q %v", tok, err)
	}
	for _, mode := range []os.FileMode{0o644, 0o660, 0o400} {
		p := writeTok(t, "SECRETVALUE", mode)
		_, err := fileToken(p)()
		if err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), "SECRETVALUE") {
			t.Errorf("mode %o: %v", mode, err)
		}
	}
	if _, err := fileToken(filepath.Join(t.TempDir(), "none"))(); err == nil {
		t.Error("missing file accepted")
	}
	if _, err := fileToken(writeTok(t, " \n", 0o600))(); err == nil {
		t.Error("empty token accepted")
	}
}

func TestTokenErrorDoesNotLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") }))
	defer srv.Close()
	p := writeTok(t, "SECRETVALUE", 0o644)
	code, out, errS := runArgs("--server", srv.URL, "--token-file", p, "list")
	if code != 1 || strings.Contains(out+errS, "SECRETVALUE") || !strings.Contains(errS, p) {
		t.Fatalf("%d %q", code, errS)
	}
}

func TestServerAndSocketMutuallyExclusive(t *testing.T) {
	code, _, errS := runArgs("--server", "https://x.example", "--socket", "/tmp/s", "list")
	if code != 2 || !strings.Contains(errS, "함께") {
		t.Fatalf("%d %q", code, errS)
	}
}

func TestTokenSourceSeam(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	old := tokenSource
	defer func() { tokenSource = old }()
	tokenSource = func(string) func() (string, error) { return func() (string, error) { return "from-seam", nil } }
	if code, _, e := runArgs("--server", srv.URL, "list"); code != 0 || gotAuth != "Bearer from-seam" {
		t.Fatalf("%d %q %q", code, gotAuth, e)
	}
}

func TestSocketClientUsesV1Prefix(t *testing.T) {
	sock := stubServer(t) // serves only /api/v1/tasks...; legacy paths would 404
	var o, e bytes.Buffer
	if code := run([]string{"--socket", sock, "show", "7"}, &o, &e); code != 0 || !strings.Contains(o.String(), "작업 #7") {
		t.Fatalf("%d %q %q", code, o.String(), e.String())
	}
}
