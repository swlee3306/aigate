package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/orch/api"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
)

const (
	tBase     = "0123456789abcdef0123456789abcdef01234567"
	tHead     = "89abcdef0123456789abcdef0123456789abcdef"
	tRepoPath = "/srv/repos/proj" // not a repository here: its id is the hash of the path
)

// tRepo is the repo id the server would put in the message for tRepoPath.
var tRepo = func() string {
	sum := sha256.Sum256([]byte(tRepoPath))
	return hex.EncodeToString(sum[:])[:12]
}()

// signedServer is a fake orchd for the signed flow: it serves one approval
// detail and records grant requests, answering them with grantStatus/grantBody.
type signedServer struct {
	mu          sync.Mutex
	detail      api.ApprovalDetail
	grants      []map[string]any
	grantStatus int
	grantBody   string
	reverified  bool
	srv         *httptest.Server
}

func newSignedServer(t *testing.T, d api.ApprovalDetail) *signedServer {
	t.Helper()
	s := &signedServer{detail: d, grantStatus: 200, grantBody: `{"id":7,"state":"verifying"}`}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/approvals/1":
			json.NewEncoder(w).Encode(s.detail)
		case r.Method == "POST" && r.URL.Path == "/api/v1/approvals/1/grant":
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			s.grants = append(s.grants, body)
			w.WriteHeader(s.grantStatus)
			w.Write([]byte(s.grantBody))
		case r.Method == "POST" && r.URL.Path == "/api/v1/approvals/1/reverify":
			s.reverified = true
			w.Write([]byte(`{"id":7,"state":"verifying"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *signedServer) grantCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.grants) }

// fakeSigner replaces signCmd and counts calls.
type fakeSigner struct {
	mu    sync.Mutex
	calls []string // messages signed
	ns    []string
}

func installFakeSigner(t *testing.T) *fakeSigner {
	t.Helper()
	f := &fakeSigner{}
	old := signCmd
	signCmd = func(keyPath, namespace string, message []byte) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, string(message))
		f.ns = append(f.ns, namespace)
		return "-----BEGIN SSH SIGNATURE-----\nFAKE\n-----END SSH SIGNATURE-----\n", nil
	}
	t.Cleanup(func() { signCmd = old })
	return f
}

func (f *fakeSigner) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// protectedDetail is a signed-mode protected_modified detail whose message,
// hash and hashed text agree.
func protectedDetail(t *testing.T, hashed string) api.ApprovalDetail {
	t.Helper()
	issued := time.Now().UTC().Truncate(time.Second)
	hash := gitx.HashOf(hashed)
	m := signing.Message{Kind: store.KindProtectedModified, Repo: tRepo, Task: 7, Base: tBase, Hash: hash, Issued: issued}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	d := api.ApprovalDetail{Repo: tRepoPath, TaskState: string(store.StateNeedsApproval)}
	d.ID, d.TaskID, d.Kind, d.Hash, d.Base, d.Head, d.Status = 1, 7, store.KindProtectedModified, hash, tBase, tHead, store.ApprovalPending
	d.Message, d.HashedText, d.ProtectedFiles = m.Text(), hashed, []string{"tests/spec.txt"}
	d.Meaning = "작업 #7의 보호 파일 1개의 수정을 허용합니다."
	d.RequiresSignature, d.Issued = true, issued.Format(time.RFC3339)
	return d
}

func publishDetail(t *testing.T) api.ApprovalDetail {
	t.Helper()
	issued := time.Now().UTC().Truncate(time.Second)
	hash := approvals.PublishHash(7, tHead, publish.KindPushBranch)
	m := signing.Message{Kind: store.KindPublishOutsideAllowlist, Repo: tRepo, Task: 7, Base: tBase, Hash: hash, Issued: issued}
	d := api.ApprovalDetail{Repo: tRepoPath, TaskState: string(store.StateNeedsApproval)}
	d.ID, d.TaskID, d.Kind, d.Hash, d.Base, d.Head, d.Status = 1, 7, store.KindPublishOutsideAllowlist, hash, tBase, tHead, store.ApprovalPending
	d.Message, d.ProtectedFiles = m.Text(), []string{}
	d.Meaning, d.Action = "작업 #7의 head 에 대해 허용 목록 밖의 게시 행위 하나, push 를 허용합니다.", publish.KindPushBranch
	d.RequiresSignature, d.Issued = true, issued.Format(time.RFC3339)
	return d
}

// signKey creates an (empty) key file: the fake signer never reads it, the
// CLI only checks it exists.
func signKey(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, []byte("not a real key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (s *signedServer) approve(t *testing.T, in string, extra ...string) (int, string, string) {
	t.Helper()
	old := approveIn
	approveIn = strings.NewReader(in)
	defer func() { approveIn = old }()
	tf := writeTok(t, "tok\n", 0o600)
	args := append([]string{"--server", s.srv.URL, "--token-file", tf, "approve"}, extra...)
	args = append(args, "1")
	var o, e bytes.Buffer
	code := run(args, &o, &e)
	return code, o.String(), e.String()
}

const sampleHashed = "diff --git a/tests/spec.txt b/tests/spec.txt\n--- a/tests/spec.txt\n+++ b/tests/spec.txt\n@@ -1 +1,2 @@\n line1\n+line2\n"

// detail -> TTY confirmation -> signature over the exact message -> grant.
func TestApproveSignedFlow(t *testing.T) {
	fakeTTY(t)
	key := signKey(t)
	t.Run("protected", func(t *testing.T) {
		f := installFakeSigner(t)
		d := protectedDetail(t, sampleHashed)
		d.Head = "\x1b[2J" + tHead // head is not in the message: a server could plant a control sequence
		s := newSignedServer(t, d)
		code, out, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if strings.Contains(out+errS, "\x1b") {
			t.Fatalf("raw control character printed: %q", out)
		}
		if code != 0 {
			t.Fatalf("approve %d out %q err %q", code, out, errS)
		}
		for _, want := range []string{"BEGIN HASHED DIFF", "+line2", "hash: " + d.Hash, "무엇을 허용하나요: " + d.Meaning, "aigate-approval-v1", "mac", "verifying", tRepoPath} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if f.count() != 1 || f.calls[0] != d.Message || f.ns[0] != signing.NamespaceApproval {
			t.Fatalf("signed %q ns %q", f.calls, f.ns)
		}
		if s.grantCount() != 1 {
			t.Fatalf("grants %d", s.grantCount())
		}
		g := s.grants[0]
		if g["signer"] != "mac" || g["alg"] != signing.TypeSSHEd25519 || g["issued"] != d.Issued || !strings.Contains(g["sig"].(string), "SSH SIGNATURE") {
			t.Fatalf("grant body %v", g)
		}
		if s.reverified {
			t.Fatal("signed flow called reverify: the grant endpoint already re-judges")
		}
		if strings.Contains(out, "approvals.json") {
			t.Fatal("signed flow wrote a local grant")
		}
	})
	t.Run("no-yes-no-signature", func(t *testing.T) {
		f := installFakeSigner(t)
		s := newSignedServer(t, protectedDetail(t, sampleHashed))
		for _, ans := range []string{"no\n", "\n", ""} {
			if code, _, _ := s.approve(t, ans, "--sign-key", key, "--signer", "mac"); code != 64 {
				t.Fatalf("answer %q: code %d", ans, code)
			}
		}
		if f.count() != 0 || s.grantCount() != 0 {
			t.Fatalf("signed %d times, %d grants without a yes", f.count(), s.grantCount())
		}
	})
	t.Run("publish-without-hashed-text", func(t *testing.T) {
		f := installFakeSigner(t)
		d := publishDetail(t)
		s := newSignedServer(t, d)
		t.Setenv("AIGATE_SIGNER", "backup")
		code, out, errS := s.approve(t, "yes\n", "--sign-key", key)
		if code != 0 {
			t.Fatalf("approve %d out %q err %q", code, out, errS)
		}
		if !strings.Contains(out, d.Meaning) || !strings.Contains(out, d.Hash) || !strings.Contains(out, "행위: "+publish.KindPushBranch) {
			t.Fatalf("meaning/hash not shown:\n%s", out)
		}
		// Task 15 minor: the server's meaning is labelled unverified for a
		// publish entry (the derived action above is the verified line).
		if !strings.Contains(out, "서버 설명(검증 안 됨): "+d.Meaning) || strings.Contains(out, "무엇을 허용하나요") {
			t.Fatalf("publish meaning not labelled as unverified:\n%s", out)
		}
		if f.count() != 1 || s.grantCount() != 1 || s.grants[0]["signer"] != "backup" {
			t.Fatalf("signed %d grants %v", f.count(), s.grants)
		}
	})
	t.Run("message-disagrees-with-entry", func(t *testing.T) {
		f := installFakeSigner(t)
		d := protectedDetail(t, sampleHashed)
		other := publishDetail(t) // a valid message for another hash
		d.Message = other.Message
		s := newSignedServer(t, d)
		code, _, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if code != 64 || f.count() != 0 || s.grantCount() != 0 || !strings.Contains(errS, "메시지") {
			t.Fatalf("code %d signed %d err %q", code, f.count(), errS)
		}
	})
	t.Run("not-grantable-now", func(t *testing.T) {
		f := installFakeSigner(t)
		d := protectedDetail(t, sampleHashed)
		d.Message, d.HashedText = "", ""
		s := newSignedServer(t, d)
		code, _, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if code != 64 || f.count() != 0 || s.grantCount() != 0 {
			t.Fatalf("code %d signed %d err %q", code, f.count(), errS)
		}
	})
	t.Run("publish-action-mismatch", func(t *testing.T) {
		f := installFakeSigner(t)
		d := publishDetail(t) // hash binds push_branch
		d.Action = publish.KindNotify
		d.Meaning = "알림 보내기(notify)를 허용합니다."
		s := newSignedServer(t, d)
		code, out, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if code != 64 || f.count() != 0 || s.grantCount() != 0 || !strings.Contains(errS, "게시 행위") {
			t.Fatalf("code %d signed %d out %q err %q", code, f.count(), out, errS)
		}
	})
	t.Run("publish-unknown-hash", func(t *testing.T) {
		f := installFakeSigner(t)
		d := publishDetail(t)
		d.Hash = strings.Repeat("ab", 32)
		m, _ := signing.ParseMessage(d.Message)
		m.Hash = d.Hash
		d.Message = m.Text()
		s := newSignedServer(t, d)
		code, _, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if code != 64 || f.count() != 0 || s.grantCount() != 0 {
			t.Fatalf("code %d signed %d err %q", code, f.count(), errS)
		}
	})
	t.Run("policy-change-action", func(t *testing.T) {
		f := installFakeSigner(t)
		d := publishDetail(t)
		d.PolicyHash = strings.Repeat("c", 64)
		d.Action = steps.PolicyChangeKind(d.PolicyHash)
		d.Hash = approvals.PublishHash(7, tHead, d.Action)
		m, _ := signing.ParseMessage(d.Message)
		m.Hash = d.Hash
		d.Message = m.Text()
		s := newSignedServer(t, d)
		if code, out, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac"); code != 0 || f.count() != 1 {
			t.Fatalf("code %d signed %d out %q err %q", code, f.count(), out, errS)
		}
	})
	t.Run("repo-id-mismatch", func(t *testing.T) {
		f := installFakeSigner(t)
		d := protectedDetail(t, sampleHashed)
		d.Repo = "/srv/repos/other" // the message's repo id is /srv/repos/proj's
		s := newSignedServer(t, d)
		code, _, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if code != 64 || f.count() != 0 || s.grantCount() != 0 || !strings.Contains(errS, "저장소") {
			t.Fatalf("code %d signed %d err %q", code, f.count(), errS)
		}
	})
	t.Run("repo-id-local-repository", func(t *testing.T) {
		f := installFakeSigner(t)
		repo, _ := filepath.EvalSymlinks(t.TempDir())
		git(t, repo, "init", "-q")
		rid, _, err := gate.RepoID(repo)
		if err != nil {
			t.Fatal(err)
		}
		d := protectedDetail(t, sampleHashed)
		m, _ := signing.ParseMessage(d.Message)
		m.Repo = rid
		d.Repo, d.Message = repo, m.Text()
		s := newSignedServer(t, d)
		code, out, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if code != 0 || f.count() != 1 || !strings.Contains(out, repo) {
			t.Fatalf("code %d signed %d out %q err %q", code, f.count(), out, errS)
		}
	})
	t.Run("no-signer-id", func(t *testing.T) {
		f := installFakeSigner(t)
		s := newSignedServer(t, protectedDetail(t, sampleHashed))
		t.Setenv("AIGATE_SIGNER", "")
		code, _, errS := s.approve(t, "yes\n", "--sign-key", key)
		if code == 0 || f.count() != 0 || !strings.Contains(errS, "--signer") {
			t.Fatalf("code %d signed %d err %q", code, f.count(), errS)
		}
	})
}

// The server's hashed text must hash to the entry's hash before anything is signed.
func TestApproveRefusesHashMismatch(t *testing.T) {
	fakeTTY(t)
	f := installFakeSigner(t)
	d := protectedDetail(t, sampleHashed)
	d.HashedText = strings.Replace(sampleHashed, "+line2", "+line3", 1) // shown text differs from what the hash covers
	s := newSignedServer(t, d)
	code, out, errS := s.approve(t, "yes\n", "--sign-key", signKey(t), "--signer", "mac")
	if code != 64 || f.count() != 0 || s.grantCount() != 0 {
		t.Fatalf("code %d signed %d grants %d", code, f.count(), s.grantCount())
	}
	if !strings.Contains(errS, "해시") || !strings.Contains(errS, short(d.Hash)) || !strings.Contains(errS, short(gitx.HashOf(d.HashedText))) {
		t.Fatalf("reason not printed: %q", errS)
	}
	if strings.Contains(out, "+line3") {
		t.Fatal("mismatching text was shown as the approved diff")
	}
}

// A truncated diff cannot be reviewed in full, so it is never signed.
func TestApproveRefusesTruncatedDiff(t *testing.T) {
	fakeTTY(t)
	f := installFakeSigner(t)
	d := protectedDetail(t, sampleHashed)
	d.Truncated = true // even though the text happens to hash correctly
	s := newSignedServer(t, d)
	code, _, errS := s.approve(t, "yes\n", "--sign-key", signKey(t), "--signer", "mac")
	if code != 64 || f.count() != 0 || s.grantCount() != 0 {
		t.Fatalf("code %d signed %d grants %d", code, f.count(), s.grantCount())
	}
	if !strings.Contains(errS, "잘렸") || !strings.Contains(errS, "git diff") {
		t.Fatalf("guidance not printed: %q", errS)
	}
}

// Without a terminal nothing is fetched, shown or signed, signed mode or not.
func TestApproveStillNeedsTTY(t *testing.T) {
	f := installFakeSigner(t)
	s := newSignedServer(t, protectedDetail(t, sampleHashed))
	code, out, errS := s.approve(t, "yes\n", "--sign-key", signKey(t), "--signer", "mac")
	if code != 64 || !strings.Contains(errS, "터미널") || strings.Contains(out, "BEGIN HASHED DIFF") {
		t.Fatalf("code %d out %q err %q", code, out, errS)
	}
	if f.count() != 0 || s.grantCount() != 0 {
		t.Fatalf("signed %d grants %d without a terminal", f.count(), s.grantCount())
	}
}

// Not in signed mode: the TTY flow writes the local grant exactly as before
// and never signs.
func TestLegacyApproveUnchangedWhenNotSigned(t *testing.T) {
	old := signCmd
	signCmd = func(string, string, []byte) (string, error) {
		t.Error("signCmd called in legacy mode")
		return "", nil
	}
	t.Cleanup(func() { signCmd = old })
	e := newE2E(t, map[string]string{"tests/spec.txt": "line1\nline2\n"}, nil)
	id := e.submit()
	e.waitState(id, store.StateNeedsApproval)
	fakeTTY(t)
	code, out, errS := e.orch(strings.NewReader("yes\n"), "approve", "1")
	if code != 0 {
		t.Fatalf("approve %d out %q err %q", code, out, errS)
	}
	for _, want := range []string{"BEGIN HASHED DIFF", "+line2", "approved.", "재검증"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if m := mode(t, approvalsFile(e)); m != 0o600 {
		t.Fatalf("approvals.json mode %04o", m)
	}
	e.waitState(id, store.StateDone)
}

// CF1: grant refusals print a fixed text per code; whatever message the
// server sends (it could echo input) is never printed, nor an unknown code.
func TestApproveGrantErrorsFixedMessages(t *testing.T) {
	fakeTTY(t)
	key := signKey(t)
	const hostile = "<img onerror=x>\x1b[2Jpwned"
	if len(grantRefusals) != 9 {
		t.Fatalf("grant refusal codes changed: %d", len(grantRefusals))
	}
	for code, want := range grantRefusals {
		t.Run(code, func(t *testing.T) {
			installFakeSigner(t)
			s := newSignedServer(t, protectedDetail(t, sampleHashed))
			s.grantStatus = 403
			s.grantBody = `{"error":{"code":"` + code + `","message":"` + strings.ReplaceAll(hostile, "\x1b", `\u001b`) + `"}}`
			rc, out, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
			if rc == 0 || !strings.Contains(errS, want) {
				t.Fatalf("rc %d err %q, want %q", rc, errS, want)
			}
			if strings.Contains(out+errS, "pwned") || strings.Contains(out+errS, "onerror") {
				t.Fatalf("server message echoed: %q %q", out, errS)
			}
		})
	}
	t.Run("unknown-code", func(t *testing.T) {
		installFakeSigner(t)
		s := newSignedServer(t, protectedDetail(t, sampleHashed))
		s.grantStatus = 418
		s.grantBody = `{"error":{"code":"evil_pwned_code","message":"pwned"}}`
		rc, out, errS := s.approve(t, "yes\n", "--sign-key", key, "--signer", "mac")
		if rc == 0 || strings.Contains(out+errS, "pwned") || !strings.Contains(errS, "418") {
			t.Fatalf("rc %d out %q err %q", rc, out, errS)
		}
	})
}

// The default signCmd runs ssh-keygen -Y sign; its output verifies.
func TestDefaultSignCmdSSHKeygen(t *testing.T) {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen이 없습니다")
	}
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	msg := []byte(protectedDetail(t, sampleHashed).Message)
	armored, err := signCmd(key, signing.NamespaceApproval, msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.VerifySSHSig(strings.TrimSpace(string(pub)), signing.NamespaceApproval, msg, armored); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if signing.VerifySSHSig(strings.TrimSpace(string(pub)), "aigate-login", msg, armored) == nil {
		t.Fatal("signature verified in another namespace")
	}
}

// M5: server error text is escaped before it reaches the terminal.
func TestServerErrorTextEscaped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"code":"internal","message":"bad\u001b[2Jthing"}}`))
	}))
	defer srv.Close()
	tf := writeTok(t, "tok\n", 0o600)
	fakeTTY(t)
	for _, args := range [][]string{{"list"}, {"approve", "1"}, {"reverify", "1"}} {
		var o, e bytes.Buffer
		code := run(append([]string{"--server", srv.URL, "--token-file", tf}, args...), &o, &e)
		if code == 0 || strings.Contains(o.String()+e.String(), "\x1b") || !strings.Contains(e.String(), `bad\x1b[2Jthing`) {
			t.Fatalf("%v: code %d out %q err %q", args, code, o.String(), e.String())
		}
	}
}
