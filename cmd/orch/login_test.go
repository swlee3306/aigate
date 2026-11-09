package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/api"
)

// None of these tests may use t.Parallel: they swap signCmd.

type signCall struct {
	key, ns string
	msg     string
}

// fakeSig is what the signCmd seam returns: tied to namespace and message.
func fakeSig(ns, msg string) string {
	s := sha256.Sum256([]byte(ns + "\x00" + msg))
	return "FAKE-SIG-" + hex.EncodeToString(s[:])
}

// useFakeSign swaps signCmd for a recorder producing fakeSig.
func useFakeSign(t *testing.T) *[]signCall {
	t.Helper()
	var calls []signCall
	old := signCmd
	signCmd = func(keyPath, namespace string, message []byte) (string, error) {
		calls = append(calls, signCall{keyPath, namespace, string(message)})
		return fakeSig(namespace, string(message)), nil
	}
	t.Cleanup(func() { signCmd = old })
	return &calls
}

// fakeOrch implements the login routes and GET /api/v1/tasks of orchd.
type fakeOrch struct {
	mu         sync.Mutex
	srv        *httptest.Server
	dumps      []string // every request, headers and body
	challenges int
	tokens     int
	taskHits   int
	valid      map[string]bool // live tokens
	pending    map[string]string
	always401  bool
	// message, when set, replaces the challenge message (hostile server).
	message func(signer string) string
	// tokenErr, when set, is the token endpoint's error answer.
	tokenErr string
	// verify, when set, checks the signature instead of fakeSig.
	verify func(msg, sig string) bool
}

func newFakeOrch(t *testing.T) *fakeOrch {
	t.Helper()
	f := &fakeOrch{valid: map[string]bool{}, pending: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func envelope(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (f *fakeOrch) serve(w http.ResponseWriter, r *http.Request) {
	dump, _ := httputil.DumpRequest(r, true)
	body := []byte{}
	if i := strings.Index(string(dump), "\r\n\r\n"); i >= 0 {
		body = dump[i+4:]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dumps = append(f.dumps, string(dump))
	switch r.Method + " " + r.URL.Path {
	case "POST /api/v1/auth/challenge":
		f.challenges++
		if r.Header.Get("Authorization") != "" {
			envelope(w, 400, "bad_request", "auth header on login route")
			return
		}
		var req struct{ Signer string }
		if json.Unmarshal(body, &req) != nil || req.Signer == "" {
			envelope(w, 400, "bad_request", "bad")
			return
		}
		id := fmt.Sprintf("%064x", f.challenges)
		msg := api.LoginMessage(req.Signer, strings.Repeat("ab", 32), time.Now().UTC().Truncate(time.Second))
		if f.message != nil {
			msg = f.message(req.Signer)
		}
		f.pending[id] = msg
		json.NewEncoder(w).Encode(map[string]string{"challenge_id": id, "message": msg,
			"expires": time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)})
	case "POST /api/v1/auth/token":
		f.tokens++
		if f.tokenErr != "" {
			envelope(w, 401, f.tokenErr, "\x1b[2J<img onerror=x> server text")
			return
		}
		var req struct {
			ChallengeID string `json:"challenge_id"`
			Sig         string `json:"sig"`
		}
		_ = json.Unmarshal(body, &req)
		msg, ok := f.pending[req.ChallengeID]
		delete(f.pending, req.ChallengeID)
		good := ok && req.Sig == fakeSig(signing.NamespaceLogin, msg)
		if f.verify != nil {
			good = ok && f.verify(msg, req.Sig)
		}
		if !good {
			envelope(w, 401, "bad_signature", "bad")
			return
		}
		tok := fmt.Sprintf("tok-%d", f.tokens)
		f.valid[tok] = true
		json.NewEncoder(w).Encode(map[string]string{"token": tok,
			"expires": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)})
	case "GET /api/v1/tasks":
		f.taskHits++
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.always401 || !f.valid[tok] {
			envelope(w, 401, "unauthenticated", "인증이 필요합니다")
			return
		}
		w.Write([]byte(`[]`))
	default:
		envelope(w, 404, "not_found", "nope")
	}
}

func (f *fakeOrch) counts() (challenges, tokens, tasks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.challenges, f.tokens, f.taskHits
}

// keyFile makes a stand-in private key file (never read by the CLI).
func keyFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id_test")
	if err := os.WriteFile(p, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readTokFile(t *testing.T, p string) tokenFileData {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var d tokenFileData
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("token file %q: %v", b, err)
	}
	return d
}

func writeTokJSON(t *testing.T, p string, d tokenFileData) {
	t.Helper()
	b, _ := json.Marshal(d)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o600)
}

func TestCLILoginStoresTokenMode0600(t *testing.T) {
	calls := useFakeSign(t)
	f := newFakeOrch(t)
	key := keyFile(t)
	dir := filepath.Join(t.TempDir(), "cfg", "orch") // created by login
	tf := filepath.Join(dir, "token")
	code, out, errS := runArgs("login", "--server", f.srv.URL, "--token-file", tf, "--signer", "alice", "--sign-key", key)
	if code != 0 {
		t.Fatalf("login %d %q %q", code, out, errS)
	}
	if !strings.Contains(out, "로그인: 서명자 alice") {
		t.Fatalf("out %q", out)
	}
	fi, err := os.Lstat(tf)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v %v", fi, err)
	}
	if di, err := os.Stat(dir); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v %v", di, err)
	}
	d := readTokFile(t, tf)
	if d.Token != "tok-1" || d.Signer != "alice" || d.Version != 1 {
		t.Fatalf("data %+v", d)
	}
	if exp, err := time.Parse(time.RFC3339, d.Expires); err != nil || time.Until(exp) < 14*time.Minute {
		t.Fatalf("expires %q %v", d.Expires, err)
	}
	if len(*calls) != 1 || (*calls)[0].ns != signing.NamespaceLogin || (*calls)[0].key != key ||
		!strings.HasPrefix((*calls)[0].msg, "aigate-login-v1\nsigner: alice\n") {
		t.Fatalf("sign calls %+v", *calls)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files %v", ents)
	}
	// The stored token is used as is.
	if code, out, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "list"); code != 0 || !strings.Contains(out, "작업이 없습니다") {
		t.Fatalf("list %d %q %q", code, out, errS)
	}
	if c, _, _ := f.counts(); c != 1 || len(*calls) != 1 {
		t.Fatalf("logged in again: %d challenges", c)
	}

	// A wider-mode file is replaced by a 0600 one (rename, new inode).
	if err := os.Chmod(tf, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errS := runArgs("login", "--server", f.srv.URL, "--token-file", tf, "--signer", "alice", "--sign-key", key); code != 0 {
		t.Fatalf("relogin %d %q", code, errS)
	}
	if fi, _ := os.Lstat(tf); fi.Mode().Perm() != 0o600 || readTokFile(t, tf).Token != "tok-2" {
		t.Fatalf("relogin file %v", fi.Mode())
	}

	// A symlink at the token path is refused; its target is untouched.
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("keep"), 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	code, _, errS = runArgs("login", "--server", f.srv.URL, "--token-file", link, "--signer", "alice", "--sign-key", key)
	if code == 0 || !strings.Contains(errS, "심볼릭 링크") {
		t.Fatalf("symlink %d %q", code, errS)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("target changed: %q", b)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}

	// login needs --server and a signer, and an existing key file.
	if code, _, errS := runArgs("login", "--token-file", tf, "--signer", "alice"); code == 0 || !strings.Contains(errS, "--server") {
		t.Fatalf("no server %d %q", code, errS)
	}
	t.Setenv("AIGATE_SIGNER", "")
	if code, _, errS := runArgs("login", "--server", f.srv.URL, "--token-file", filepath.Join(t.TempDir(), "t"), "--sign-key", key); code == 0 || !strings.Contains(errS, "서명자") {
		t.Fatalf("no signer %d %q", code, errS)
	}
	if code, _, errS := runArgs("login", "--server", f.srv.URL, "--token-file", tf, "--signer", "alice", "--sign-key", key+".none"); code == 0 || !strings.Contains(errS, "키 파일") {
		t.Fatalf("no key %d %q", code, errS)
	}
}

func TestRemoteClientAutoLoginWhenTokenExpired(t *testing.T) {
	key := keyFile(t)
	t.Setenv("AIGATE_SIGNER", "")

	t.Run("server-401-relogin-retry-once", func(t *testing.T) {
		calls := useFakeSign(t)
		f := newFakeOrch(t)
		tf := filepath.Join(t.TempDir(), "token")
		// Locally still valid; the server no longer knows it.
		writeTokJSON(t, tf, tokenFileData{Version: 1, Token: "stale", Signer: "alice", SignKey: key,
			Expires: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)})
		code, out, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "list")
		if code != 0 || !strings.Contains(out, "작업이 없습니다") {
			t.Fatalf("list %d %q %q", code, out, errS)
		}
		if c, tk, hits := f.counts(); c != 1 || tk != 1 || hits != 2 {
			t.Fatalf("challenges %d tokens %d task hits %d", c, tk, hits)
		}
		if len(*calls) != 1 || (*calls)[0].ns != signing.NamespaceLogin || (*calls)[0].key != key {
			t.Fatalf("sign calls %+v", *calls)
		}
		if !strings.Contains(errS, "로그인: 서명자 alice") {
			t.Fatalf("stderr %q", errS)
		}
		if d := readTokFile(t, tf); d.Token != "tok-1" || d.Signer != "alice" || d.SignKey != key {
			t.Fatalf("token file %+v", d)
		}
	})

	t.Run("locally-expired-logs-in-first", func(t *testing.T) {
		useFakeSign(t)
		f := newFakeOrch(t)
		tf := filepath.Join(t.TempDir(), "token")
		// Inside the 30 s skew margin counts as expired.
		writeTokJSON(t, tf, tokenFileData{Version: 1, Token: "old", Signer: "alice", SignKey: key,
			Expires: time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339)})
		if code, _, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "list"); code != 0 {
			t.Fatalf("list %d %q", code, errS)
		}
		if c, _, hits := f.counts(); c != 1 || hits != 1 {
			t.Fatalf("challenges %d task hits %d", c, hits)
		}
	})

	t.Run("missing-file-uses-flags", func(t *testing.T) {
		calls := useFakeSign(t)
		f := newFakeOrch(t)
		tf := filepath.Join(t.TempDir(), "token")
		if code, _, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "--signer", "bob", "--sign-key", key, "list"); code != 0 {
			t.Fatalf("list %d %q", code, errS)
		}
		if len(*calls) != 1 || !strings.Contains((*calls)[0].msg, "signer: bob\n") {
			t.Fatalf("calls %+v", *calls)
		}
		if fi, err := os.Lstat(tf); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file %v %v", fi, err)
		}
	})

	t.Run("always-401-never-loops", func(t *testing.T) {
		useFakeSign(t)
		f := newFakeOrch(t)
		f.always401 = true
		tf := filepath.Join(t.TempDir(), "token")
		writeTokJSON(t, tf, tokenFileData{Version: 1, Token: "stale", Signer: "alice", SignKey: key,
			Expires: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)})
		code, _, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "list")
		if code == 0 {
			t.Fatal("succeeded")
		}
		if c, tk, hits := f.counts(); c != 1 || tk != 1 || hits != 2 {
			t.Fatalf("challenges %d tokens %d task hits %d (%q)", c, tk, hits, errS)
		}
	})

	t.Run("login-failure-stops", func(t *testing.T) {
		useFakeSign(t)
		f := newFakeOrch(t)
		f.tokenErr = "bad_signature"
		tf := filepath.Join(t.TempDir(), "token")
		writeTokJSON(t, tf, tokenFileData{Version: 1, Token: "stale", Signer: "alice", SignKey: key,
			Expires: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)})
		code, _, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "list")
		if c, tk, hits := f.counts(); code == 0 || c != 1 || tk != 1 || hits != 1 {
			t.Fatalf("code %d challenges %d tokens %d hits %d", code, c, tk, hits)
		}
		if strings.Contains(errS, "\x1b") || strings.Contains(errS, "<img") || !strings.Contains(errS, "로그인 서명") {
			t.Fatalf("stderr %q", errS)
		}
	})

	t.Run("bad-permissions-no-autologin", func(t *testing.T) {
		calls := useFakeSign(t)
		f := newFakeOrch(t)
		tf := filepath.Join(t.TempDir(), "token")
		writeTokJSON(t, tf, tokenFileData{Version: 1, Token: "x", Signer: "alice",
			Expires: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
		os.Chmod(tf, 0o644)
		if code, _, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "list"); code == 0 || !strings.Contains(errS, "chmod 600") {
			t.Fatalf("%d %q", code, errS)
		}
		if c, _, hits := f.counts(); c != 0 || hits != 0 || len(*calls) != 0 {
			t.Fatalf("requests sent: %d %d", c, hits)
		}
	})
}

func TestRemoteClientNeverSendsKeyMaterial(t *testing.T) {
	useFakeSign(t)
	f := newFakeOrch(t)
	dir := filepath.Join(t.TempDir(), "KEYPATH-MARKER")
	os.MkdirAll(dir, 0o700)
	key := filepath.Join(dir, "id_ed25519")
	secret := "-----BEGIN OPENSSH PRIVATE KEY-----\nPRIVATE-KEY-MARKER\n-----END OPENSSH PRIVATE KEY-----\n"
	if err := os.WriteFile(key, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	tf := filepath.Join(t.TempDir(), "token")
	if code, _, errS := runArgs("login", "--server", f.srv.URL, "--token-file", tf, "--signer", "alice", "--sign-key", key); code != 0 {
		t.Fatalf("login %d %q", code, errS)
	}
	// Auto-login path too: the token expires locally.
	d := readTokFile(t, tf)
	d.Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	writeTokJSON(t, tf, d)
	if code, _, errS := runArgs("--server", f.srv.URL, "--token-file", tf, "list"); code != 0 {
		t.Fatalf("list %d %q", code, errS)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dumps) < 5 {
		t.Fatalf("only %d requests", len(f.dumps))
	}
	for _, dump := range f.dumps {
		for _, bad := range []string{"KEYPATH-MARKER", "PRIVATE-KEY-MARKER", "OPENSSH", "id_ed25519", "sign_key"} {
			if strings.Contains(dump, bad) {
				t.Fatalf("request carries %q:\n%s", bad, dump)
			}
		}
	}
}

func TestCLILoginRefusesHostileMessage(t *testing.T) {
	key := keyFile(t)
	now := time.Now().UTC().Truncate(time.Second)
	nonce := strings.Repeat("ab", 32)
	approval := "aigate-approval-v1\nkind: protected_modified\nrepo: 0123456789ab\ntask: 1\nbase: " +
		strings.Repeat("a", 40) + "\nhash: " + strings.Repeat("b", 64) + "\nissued: " + now.Format(time.RFC3339) + "\n"
	cases := map[string]func(string) string{
		"approval message": func(string) string { return approval },
		"other signer":     func(string) string { return api.LoginMessage("mallory", nonce, now) },
		"stale issued":     func(s string) string { return api.LoginMessage(s, nonce, now.Add(-time.Hour)) },
		"future issued":    func(s string) string { return api.LoginMessage(s, nonce, now.Add(time.Hour)) },
		"extra line":       func(s string) string { return api.LoginMessage(s, nonce, now) + "kind: protected_modified\n" },
		"no final newline": func(s string) string { return strings.TrimSuffix(api.LoginMessage(s, nonce, now), "\n") },
		"short nonce":      func(s string) string { return api.LoginMessage(s, "abcd", now) },
		"upper nonce":      func(s string) string { return api.LoginMessage(s, strings.ToUpper(nonce), now) },
		"control chars":    func(s string) string { return "\x1b[2J" + api.LoginMessage(s, nonce, now) },
		"offset time": func(s string) string {
			return strings.Replace(api.LoginMessage(s, nonce, now), "Z\n", "+00:00\n", 1)
		},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			calls := useFakeSign(t)
			f := newFakeOrch(t)
			f.message = msg
			tf := filepath.Join(t.TempDir(), "token")
			code, out, errS := runArgs("login", "--server", f.srv.URL, "--token-file", tf, "--signer", "alice", "--sign-key", key)
			if code == 0 || len(*calls) != 0 {
				t.Fatalf("signed a hostile message: %d %q %q", code, out, errS)
			}
			if strings.Contains(out+errS, "\x1b") {
				t.Fatalf("raw control chars printed: %q", out+errS)
			}
			if _, err := os.Lstat(tf); !os.IsNotExist(err) {
				t.Fatalf("token file written: %v", err)
			}
		})
	}
}

func TestCLILoginErrorsFixedMessages(t *testing.T) {
	key := keyFile(t)
	for code, want := range loginRefusals {
		t.Run(code, func(t *testing.T) {
			useFakeSign(t)
			f := newFakeOrch(t)
			f.tokenErr = code
			rc, _, errS := runArgs("login", "--server", f.srv.URL, "--token-file", filepath.Join(t.TempDir(), "t"), "--signer", "alice", "--sign-key", key)
			if rc == 0 || !strings.Contains(errS, want) || strings.Contains(errS, "server text") || strings.Contains(errS, "\x1b") {
				t.Fatalf("%d %q", rc, errS)
			}
		})
	}
	for _, c := range []string{"bad_challenge", "bad_signature", "busy", "signers_unavailable"} {
		if loginRefusals[c] == "" {
			t.Errorf("no fixed text for %s", c)
		}
	}
	useFakeSign(t)
	f := newFakeOrch(t)
	f.tokenErr = "weird_code"
	rc, _, errS := runArgs("login", "--server", f.srv.URL, "--token-file", filepath.Join(t.TempDir(), "t"), "--signer", "alice", "--sign-key", key)
	if rc == 0 || strings.Contains(errS, "weird_code") || strings.Contains(errS, "server text") || !strings.Contains(errS, "HTTP 401") {
		t.Fatalf("unknown code %d %q", rc, errS)
	}
}

// The default signCmd (real ssh-keygen) produces a login signature the
// server-side verifier accepts in the login namespace only.
func TestCLILoginRealSSHKeygen(t *testing.T) {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen이 없습니다")
	}
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "", "-C", "t", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	f := newFakeOrch(t)
	var approvalOK bool
	f.verify = func(msg, sig string) bool {
		approvalOK = signing.VerifySSHSig(strings.TrimSpace(string(pub)), signing.NamespaceApproval, []byte(msg), sig) == nil
		return signing.VerifySSHSig(strings.TrimSpace(string(pub)), signing.NamespaceLogin, []byte(msg), sig) == nil
	}
	tf := filepath.Join(t.TempDir(), "token")
	code, out, errS := runArgs("login", "--server", f.srv.URL, "--token-file", tf, "--signer", "alice", "--sign-key", key)
	if code != 0 {
		t.Fatalf("login %d %q %q", code, out, errS)
	}
	if approvalOK {
		t.Fatal("login signature verified in the approval namespace")
	}
	if readTokFile(t, tf).Token != "tok-1" {
		t.Fatal("no token")
	}
}

func TestTokenFileFormats(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	writeTokJSON(t, p, tokenFileData{Version: 1, Token: "abc", Expires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if tok, err := fileToken(p)(); err != nil || tok != "abc" {
		t.Fatalf("json %q %v", tok, err)
	}
	for name, content := range map[string]string{
		"expired":     `{"version":1,"token":"abc","expires":"2000-01-01T00:00:00Z"}`,
		"no expiry":   `{"version":1,"token":"abc"}`,
		"bad json":    `{"version":1,`,
		"empty token": `{"version":1,"token":"","expires":"2999-01-01T00:00:00Z"}`,
		"bad token":   `{"version":1,"token":"a b","expires":"2999-01-01T00:00:00Z"}`,
		"version 2":   `{"version":2,"token":"abc","expires":"2999-01-01T00:00:00Z"}`,
	} {
		os.WriteFile(p, []byte(content), 0o600)
		_, err := fileToken(p)()
		if err == nil || !isNoUsableToken(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Legacy plain token files still work.
	os.WriteFile(p, []byte("legacy-token\n"), 0o600)
	if tok, err := fileToken(p)(); err != nil || tok != "legacy-token" {
		t.Fatalf("legacy %q %v", tok, err)
	}
	if _, err := fileToken(filepath.Join(dir, "none"))(); !isNoUsableToken(err) {
		t.Fatalf("missing: %v", err)
	}
	_ = io.Discard
}
