package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/api"
	"aigate/internal/orch/store"
)

// enrollDaemon runs a real orchd on a unix socket with (or without)
// passkey settings and returns the socket path.
func enrollDaemon(t *testing.T, rpID, origin string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "orch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, _ = filepath.EvalSymlinks(dir)
	st, err := store.Open(filepath.Join(dir, "db", "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sock := filepath.Join(dir, "s")
	d, err := api.NewDaemon(api.Config{Socket: sock, RPID: rpID, Origin: origin,
		PendingFile: filepath.Join(dir, "pending.json")}, st, noSteps{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	t.Cleanup(func() { cancel(); <-done })
	return sock
}

func runCLI(args ...string) (int, string, string) {
	var o, e bytes.Buffer
	code := run(args, &o, &e)
	return code, o.String(), e.String()
}

var printedCode = regexp.MustCompile(`패스키 등록 코드: ([0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4})\n`)

func TestAdminEnrollPrintsCode(t *testing.T) {
	sock := enrollDaemon(t, "console.example.test", "https://console.example.test")
	code, out, errS := runCLI("--socket", sock, "admin", "enroll", "--id", "phone", "--label", "iPhone")
	m := printedCode.FindStringSubmatch(out)
	if code != 0 || m == nil || !strings.Contains(out, "phone") || !strings.Contains(out, "iPhone") ||
		!strings.Contains(out, "signers add --pending phone --fingerprint") || !strings.Contains(out, "signers pending") {
		t.Fatalf("enroll %d %q %q", code, out, errS)
	}
	// The printed code starts a registration (unauthenticated route, here
	// reached over the same socket), once.
	c := newClient(sock)
	if _, err := c.call("POST", "/auth/webauthn/register/begin", map[string]string{"code": m[1]}); err != nil {
		t.Fatalf("begin with printed code: %v", err)
	}
	if _, err := c.call("POST", "/auth/webauthn/register/begin", map[string]string{"code": m[1]}); err == nil {
		t.Fatal("code reused")
	}
	// A malformed id is refused with a fixed text.
	if code, _, errS = runCLI("--socket", sock, "admin", "enroll", "--id", "nope!"); code != 1 || !strings.Contains(errS, "형식") {
		t.Fatalf("bad id %d %q", code, errS)
	}
}

func TestAdminEnrollSocketOnly(t *testing.T) {
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	code, out, errS := runCLI("--server", "http://"+ln.Addr().String(), "admin", "enroll")
	if code != 2 || out != "" || !strings.Contains(errS, "소켓") || hits.Load() != 0 {
		t.Fatalf("--server: %d %q %q hits=%d", code, out, errS, hits.Load())
	}
}

func TestAdminEnrollRefusalsFixed(t *testing.T) {
	// A daemon without passkey settings: fixed text.
	sock := enrollDaemon(t, "", "")
	code, out, errS := runCLI("--socket", sock, "admin", "enroll")
	if code != 1 || out != "" || !strings.Contains(errS, "--rp-id") {
		t.Fatalf("unconfigured %d %q %q", code, out, errS)
	}
	// A hostile server's message is never printed; an unknown code is not
	// printed either; a malformed code in a 2xx answer is not shown.
	dir, _ := os.MkdirTemp("/tmp", "orch")
	t.Cleanup(func() { os.RemoveAll(dir) })
	fake := filepath.Join(dir, "f")
	ln, err := net.Listen("unix", fake)
	if err != nil {
		t.Fatal(err)
	}
	var reply string
	var status int
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(reply))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{403, `{"error":{"code":"socket_only","message":"<img onerror=EVIL>\u001b[2J"}}`, "소켓으로만"},
		{409, `{"error":{"code":"EVIL_code","message":"EVIL"}}`, "HTTP 409"},
		{201, `{"code":"EVIL\u001b[2J","id":"phone","label":"x","expires":"2026-11-09T00:00:00Z"}`, "응답이 올바르지 않습니다"},
		{201, `{"code":"ABCD-EFGH","id":"EVIL\n","label":"x","expires":"2026-11-09T00:00:00Z"}`, "응답이 올바르지 않습니다"},
	} {
		status, reply = tc.status, tc.body
		code, out, errS := runCLI("--socket", fake, "admin", "enroll")
		if code != 1 || !strings.Contains(errS, tc.want) || strings.Contains(out+errS, "EVIL") || strings.Contains(out+errS, "\x1b") {
			t.Fatalf("%s: %d %q %q", tc.body, code, out, errS)
		}
	}
	// A hostile label is escaped.
	status, reply = 201, `{"code":"ABCD-EFGH","id":"phone","label":"a\u001b[2Jb","expires":"2026-11-09T00:00:00Z"}`
	if code, out, _ := runCLI("--socket", fake, "admin", "enroll"); code != 0 || strings.Contains(out, "\x1b") {
		t.Fatalf("label: %d %q", code, out)
	}
}

// The daemon's pending file (api.AppendPending) is exactly what `orch admin
// signers pending` / `add --pending` read (Task 15's pendingCredential), and
// an entry registered under an https origin is activatable as is.
func TestPendingFileFromDaemonReadable(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signers.pending.json")
	in := api.PendingCredential{ID: "phone", Label: "iPhone", CredentialID: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 16)),
		PublicKey: base64.RawURLEncoding.EncodeToString(pt), RPID: "orch-vm.tailnet.ts.net",
		Origin: "https://orch-vm.tailnet.ts.net", Created: time.Date(2026, 11, 9, 1, 2, 3, 0, time.UTC)}
	if err := api.AppendPending(path, in, api.MaxPendingCredentials); err != nil {
		t.Fatal(err)
	}
	ps, err := readPending(path)
	if err != nil || len(ps) != 1 {
		t.Fatalf("readPending %+v %v", ps, err)
	}
	p := ps[0]
	if p.ID != in.ID || p.Label != in.Label || p.CredentialID != in.CredentialID || p.PublicKey != in.PublicKey ||
		p.RPID != in.RPID || p.Origin != in.Origin || !p.Created.Equal(in.Created) {
		t.Fatalf("round trip %+v", p)
	}
	s := p.signer()
	s.AddedAt = time.Now().UTC().Truncate(time.Second)
	if err := signing.ValidateSigner(s); err != nil {
		t.Fatalf("not activatable: %v", err)
	}
	if _, err := signing.Fingerprint(s); err != nil {
		t.Fatal(err)
	}
	code, out, errS := runCLI("admin", "signers", "pending", "--pending-file", path)
	if code != 0 || !strings.Contains(out, "phone") || strings.Contains(out, "형식 오류") {
		t.Fatalf("pending list %d %q %q", code, out, errS)
	}
}
