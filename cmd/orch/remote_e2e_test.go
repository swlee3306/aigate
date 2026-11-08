package main

// Task 21 concern 5 / Task 26: the remote CLI (newRemoteClient with token
// login through the signCmd seam) driving a real Daemon over its loopback
// HTTP listener and full middleware chain (Host, Origin, body cap, JSON-only,
// auth rate limit), against a temp signers registry. It lives in cmd/orch
// because the client, run and signCmd are package main.
//
// Not t.Parallel: it changes the process-wide registry path and options and
// the signCmd seam.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/orch/api"
	"aigate/internal/orch/store"
)

// useTempRegistry points the process-wide registry at a trusted-looking temp
// layout (owned by the test user, trusted through OwnerOK) holding signers.
func useTempRegistry(t *testing.T, signers ...signing.Signer) {
	t.Helper()
	oldPath, oldOpts := signing.RegistryPath(), signing.DefaultOptions
	t.Cleanup(func() {
		signing.SetRegistryPath(oldPath)
		signing.DefaultOptions = oldOpts
	})
	dir := filepath.Join(t.TempDir(), "etc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"version": 1, "signers": signers})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "signers.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	signing.SetRegistryPath(p)
	signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == uint32(os.Getuid()) }}
}

func TestRemoteCLIThroughHTTPListener(t *testing.T) {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen이 없습니다")
	}
	keyDir := t.TempDir()
	key := filepath.Join(keyDir, "id_ed25519")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	useTempRegistry(t, signing.Signer{ID: "alice", Type: signing.TypeSSHEd25519, Label: "mac",
		PublicKey: strings.TrimSpace(string(pub)), AddedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)})

	// The real signer (ssh-keygen -Y sign) behind the seam, recording what it
	// is asked to sign.
	type call struct{ ns, msg string }
	var (
		mu    sync.Mutex
		calls []call
	)
	realSign := signCmd
	signCmd = func(keyPath, namespace string, message []byte) (string, error) {
		mu.Lock()
		calls = append(calls, call{namespace, string(message)})
		mu.Unlock()
		return realSign(keyPath, namespace, message)
	}
	t.Cleanup(func() { signCmd = realSign })

	const host = "console.example.test"
	e2eAPIConfig = func(c *api.Config) {
		c.HTTPListen = "127.0.0.1:0"
		c.RPID, c.Origin = host, "https://"+host
		// The CLI reaches the listener as http://127.0.0.1:port, so that is
		// the Host it sends.
		c.AllowedHosts = []string{host, "127.0.0.1"}
	}
	t.Cleanup(func() { e2eAPIConfig = nil })
	e := newE2E(t, map[string]string{"tests/spec.txt": "line1\nline2\n"}, nil)
	if e.httpAddr == "" {
		t.Fatal("HTTP listener not bound")
	}
	tokenFile := filepath.Join(t.TempDir(), "orch", "token") // absent: the first call logs in
	remote := func(in string, args ...string) (int, string, string) {
		t.Helper()
		old := approveIn
		approveIn = strings.NewReader(in)
		defer func() { approveIn = old }()
		var o, er bytes.Buffer
		full := append([]string{"--server", "http://" + e.httpAddr, "--token-file", tokenFile,
			"--sign-key", key, "--signer", "alice"}, args...)
		code := run(full, &o, &er)
		return code, o.String(), er.String()
	}

	// submit: logs in first (no token yet), then posts over HTTP.
	code, out, errS := remote("", "--json", "submit", "--repo", e.repo, "--base", "main", "--prompt", "x", "--class", "docs", "--agent", "claude")
	var tv api.TaskView
	if code != 0 || json.Unmarshal([]byte(out), &tv) != nil || tv.ID == 0 {
		t.Fatalf("remote submit: %d %q %q", code, out, errS)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0].ns != signing.NamespaceLogin || !strings.HasPrefix(calls[0].msg, "aigate-login-v1\n") {
		t.Fatalf("login signing: %+v", calls)
	}
	mu.Unlock()
	if fi, err := os.Stat(tokenFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	e.waitState(tv.ID, store.StateNeedsApproval)

	// list and approvals with the stored token (no second login).
	if code, out, errS := remote("", "list"); code != 0 || !strings.Contains(out, strconv.FormatInt(tv.ID, 10)) {
		t.Fatalf("remote list: %d %q %q", code, out, errS)
	}
	code, out, errS = remote("", "--json", "approvals")
	var items []api.ApprovalView
	if code != 0 || json.Unmarshal([]byte(out), &items) != nil || len(items) != 1 || items[0].Kind != store.KindProtectedModified {
		t.Fatalf("remote approvals: %d %q %q", code, out, errS)
	}

	// approve: signed mode, so the CLI checks the message and the diff hash,
	// asks for "yes", signs in the approval namespace and posts the grant.
	fakeTTY(t)
	code, out, errS = remote("yes\n", "approve", strconv.FormatInt(items[0].ID, 10))
	if code != 0 {
		t.Fatalf("remote approve: %d\nout %s\nerr %s", code, out, errS)
	}
	for _, want := range []string{"BEGIN HASHED DIFF", "+line2", "aigate-approval-v1", "hash: " + items[0].Hash} {
		if !strings.Contains(out, want) {
			t.Errorf("approve output lacks %q:\n%s", want, out)
		}
	}
	mu.Lock()
	if len(calls) != 2 || calls[1].ns != signing.NamespaceApproval || !strings.HasPrefix(calls[1].msg, "aigate-approval-v1\n") ||
		!strings.Contains(calls[1].msg, "hash: "+items[0].Hash+"\n") {
		t.Fatalf("approval signing: %+v", calls)
	}
	mu.Unlock()

	// The daemon wrote a signed grant that verifies against the registry, and
	// the real gate accepted it.
	done := e.waitState(tv.ID, store.StateDone)
	if done.Verdict != "ok" {
		t.Fatalf("verdict %q", done.Verdict)
	}
	raw, err := os.ReadFile(approvalsFile(e))
	if err != nil || !bytes.Contains(raw, []byte(`"signer": "alice"`)) || !bytes.Contains(raw, []byte(`"sig": "-----BEGIN SSH SIGNATURE-----`)) {
		t.Fatalf("approvals file: %v\n%s", err, raw)
	}
	rid, _, err := gate.RepoID(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if !(approval.Store{Path: approvalsFile(e)}).Valid(rid, e.base, items[0].Hash) {
		t.Fatal("stored signed grant does not verify")
	}
	if signed, err := approval.SignedMode(); !signed || err != nil {
		t.Fatalf("signed mode: %v %v", signed, err)
	}
}
