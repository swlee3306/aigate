package broker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"

	"golang.org/x/crypto/ssh"
)

// --- test-only signers (no real keys: everything is generated per test) ---

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func sshWire(b []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)
}

// sshsigArmored builds an SSHSIG (sha512) by key over msg in namespace ns.
func sshsigArmored(t *testing.T, s ssh.Signer, ns string, msg []byte) string {
	t.Helper()
	h := sha512.Sum512(msg)
	signed := []byte("SSHSIG")
	signed = append(signed, sshWire([]byte(ns))...)
	signed = append(signed, sshWire(nil)...)
	signed = append(signed, sshWire([]byte("sha512"))...)
	signed = append(signed, sshWire(h[:])...)
	sig, err := s.Sign(rand.Reader, signed)
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("SSHSIG")
	blob = binary.BigEndian.AppendUint32(blob, 1)
	blob = append(blob, sshWire(s.PublicKey().Marshal())...)
	blob = append(blob, sshWire([]byte(ns))...)
	blob = append(blob, sshWire(nil)...)
	blob = append(blob, sshWire([]byte("sha512"))...)
	blob = append(blob, sshWire(append(sshWire([]byte(sig.Format)), sshWire(sig.Blob)...))...)
	enc := base64.StdEncoding.EncodeToString(blob)
	var sb strings.Builder
	sb.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		sb.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	sb.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return sb.String()
}

type testSigner struct {
	id   string
	ssh  ssh.Signer // ssh-ed25519
	web  *ecdsa.PrivateKey
	line string
}

func newSSHTestSigner(t *testing.T, id string) testSigner {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{id: id, ssh: s, line: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))}
}

func newWebTestSigner(t *testing.T, id string) testSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{id: id, web: k}
}

const (
	testRPID   = "console.example.com"
	testOrigin = "https://console.example.com:8443"
)

func (s testSigner) entry(t *testing.T) map[string]any {
	t.Helper()
	if s.ssh != nil {
		return map[string]any{"id": s.id, "type": "ssh-ed25519", "label": "L", "public_key": s.line, "added_at": "2026-11-09T01:02:03Z"}
	}
	pub, err := s.web.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": s.id, "type": "webauthn-es256", "label": "phone", "public_key": b64u(pub),
		"credential_id": b64u([]byte("cred-1")), "rp_id": testRPID, "origin": testOrigin, "added_at": "2026-11-09T01:02:03Z"}
}

// sign signs a registry message; ns is the SSH namespace (tests of the wrong
// namespace pass another one).
func (s testSigner) sign(t *testing.T, msg signing.RegistryMessage, ns string) signing.Signature {
	t.Helper()
	if s.ssh != nil {
		return signing.Signature{Signer: s.id, Alg: "ssh-ed25519", Sig: sshsigArmored(t, s.ssh, ns, []byte(msg.Text()))}
	}
	d := msg.Digest()
	rpHash := sha256.Sum256([]byte(testRPID))
	authData := append(append([]byte{}, rpHash[:]...), 0x05)
	authData = binary.BigEndian.AppendUint32(authData, 1)
	cd, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": b64u(d[:]), "origin": testOrigin})
	if err != nil {
		t.Fatal(err)
	}
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	der, err := ecdsa.SignASN1(rand.Reader, s.web, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing.Signature{Signer: s.id, Alg: "webauthn-es256", Sig: b64u(der),
		WebAuthn: &signing.WebAuthnData{AuthenticatorData: b64u(authData), ClientDataJSON: b64u(cd)}}
}

// --- environment ---

var wNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type wenv struct {
	t       *testing.T
	e       *testEnv
	s       *Server
	sock    string
	root    string
	signers string // signers.json path
	alice   testSigner
	now     time.Time
	logs    *syncBuf
}

// newWEnv starts a broker with repo_roots, a signers file holding alice, and a
// fixed broker clock. The test config's migrated "repo-a" project exists.
func newWEnv(t *testing.T) *wenv { return newWEnvMod(t, nil) }

// newWEnvMod is newWEnv with an extra change to the server before it listens
// (test seams such as PeerUID/AllowUID).
func newWEnvMod(t *testing.T, mod func(*Server)) *wenv {
	t.Helper()
	e := newTestEnv(t)
	root := filepath.Join(e.Dir, "roots")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	e.set(t, []string{"repo_roots"}, []string{root})
	w := &wenv{t: t, e: e, root: root, now: wNow, alice: newSSHTestSigner(t, "alice")}
	sdir := filepath.Join(e.Dir, "etc")
	if err := os.Mkdir(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	w.signers = filepath.Join(sdir, "signers.json")
	oldPath, oldOpts := signing.RegistryPath(), signing.DefaultOptions
	signing.SetRegistryPath(w.signers)
	signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == uint32(os.Getuid()) }}
	t.Cleanup(func() { signing.SetRegistryPath(oldPath); signing.DefaultOptions = oldOpts })
	w.writeSigners(w.alice)
	w.s, w.logs = e.newServer(t, func(s *Server) {
		s.Now = func() time.Time { return w.now }
		if mod != nil {
			mod(s)
		}
	})
	w.sock = e.Raw["socket"].(string)
	return w
}

func (w *wenv) writeSigners(ss ...testSigner) {
	w.t.Helper()
	var es []map[string]any
	for _, s := range ss {
		es = append(es, s.entry(w.t))
	}
	b, err := json.Marshal(map[string]any{"version": 1, "signers": es})
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(w.signers, b, 0o644); err != nil {
		w.t.Fatal(err)
	}
	os.Chmod(w.signers, 0o644)
}

func (w *wenv) repo(name string) string {
	w.t.Helper()
	p := filepath.Join(w.root, name)
	if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *wenv) regFile() []byte {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.e.Dir, "state", RegistryFileName))
	if err != nil {
		w.t.Fatal(err)
	}
	return b
}

// signedArgs builds fully valid args for an op, signed by who at w.now.
func (w *wenv) signedArgs(who testSigner, action, subject string, fields map[string]any) map[string]any {
	w.t.Helper()
	return w.signedArgsWith(who, action, subject, fields, registrymsg.NewRequestID(), w.now, signing.NamespaceRegistry)
}

func (w *wenv) signedArgsWith(who testSigner, action, subject string, fields map[string]any, request string, issued time.Time, ns string) map[string]any {
	w.t.Helper()
	h, err := registrymsg.ChangeHash(action, subject, fields)
	if err != nil {
		w.t.Fatal(err)
	}
	msg := signing.RegistryMessage{Action: action, Subject: subject, Change: h, Request: request, Issued: issued}
	args := map[string]any{"id": subject, "change": h, "request": request, "issued": issued.Format("2006-01-02T15:04:05Z"),
		"signature": who.sign(w.t, msg, ns)}
	for k, v := range fields {
		args[k] = v
	}
	return args
}

func regFields(path, kind string, ai bool) map[string]any {
	return map[string]any{"path": path, "kind": kind, "ai_context": ai}
}

func flagFields(flag string, v bool) map[string]any {
	return map[string]any{"flag": flag, "value": v}
}

func (w *wenv) do(op string, args map[string]any) (any, *OpError) {
	w.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.s.Handlers[op](context.Background(), raw)
}

func codeOf(e *OpError) string {
	if e == nil {
		return ""
	}
	return e.Code
}

func (w *wenv) wantCode(op string, args map[string]any, want string) {
	w.t.Helper()
	_, e := w.do(op, args)
	if codeOf(e) != want {
		w.t.Fatalf("%s: code = %q, want %q", op, codeOf(e), want)
	}
}

func (w *wenv) register(id, path string) map[string]any {
	return w.signedArgs(w.alice, "register_project", id, regFields(path, "default", true))
}

func (w *wenv) projects() []Project {
	w.t.Helper()
	return w.s.Registry.Snapshot().Projects
}

func (w *wenv) find(id string) (Project, bool) {
	for _, p := range w.projects() {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

// --- tests ---

func TestProjectsReadIsOpenAndSecretFree(t *testing.T) {
	w := newWEnv(t)
	r, raw := call(t, w.sock, `{"v":1,"op":"projects"}`)
	if !r.OK {
		t.Fatalf("projects failed: %s", raw)
	}
	var res struct {
		Projects  []Project     `json:"projects"`
		Providers []any         `json:"providers"`
		Global    AutoMergeGlob `json:"auto_merge_global"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Projects) != 1 || res.Projects[0].ID != "repo-a" || res.Providers == nil || len(res.Providers) != 0 {
		t.Fatalf("unexpected result: %s", r.Result)
	}
	if !strings.Contains(string(r.Result), `"providers":[]`) {
		t.Fatalf("providers must be an empty array: %s", r.Result)
	}
	for _, secret := range []string{testOpenAIKey, testGitLabToken, w.e.KeyFile, w.e.TokFile, "signature", "request"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("reply leaks %q: %s", secret, raw)
		}
	}
	// read needs no signature and no args, and does not touch the registry
	before := w.regFile()
	call(t, w.sock, `{"v":1,"op":"projects","args":{}}`)
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("projects changed the registry")
	}
	// a broken registry fails closed
	w2 := newWEnv(t)
	if err := os.WriteFile(filepath.Join(w2.e.Dir, "state", RegistryFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := w2.restart(nil)
	if _, e := s2.Handlers["projects"](context.Background(), nil); codeOf(e) != "registry_unavailable" {
		t.Fatalf("broken registry: %v", e)
	}
}

// restart builds another server over the same state dir (no socket).
func (w *wenv) restart(mod func(*Server)) *Server {
	w.t.Helper()
	s, err := New(w.e.load(w.t), Options{StateDir: filepath.Join(w.e.Dir, "state"), KeyOwnerOK: func(uint32) bool { return true }})
	if err != nil {
		w.t.Fatal(err)
	}
	s.Now = func() time.Time { return w.now }
	if mod != nil {
		mod(s)
	}
	return s
}

func TestRegisterRequiresValidSignature(t *testing.T) {
	w := newWEnv(t)
	p := w.repo("proj-a")
	before := w.regFile()
	good := func() map[string]any { return w.register("proj-a", p) }

	unsigned := good()
	delete(unsigned, "signature")
	w.wantCode("register_project", unsigned, "not_signed")
	nullsig := good()
	nullsig["signature"] = nil
	w.wantCode("register_project", nullsig, "not_signed")

	ghost := newSSHTestSigner(t, "ghost")
	w.wantCode("register_project", w.signedArgs(ghost, "register_project", "proj-a", regFields(p, "default", true)), "unknown_signer")

	// right signer id, but a signature made for another purpose (approval namespace)
	w.wantCode("register_project", w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true),
		registrymsg.NewRequestID(), w.now, signing.NamespaceApproval), "bad_signature")
	// right signer id, but another key's signature
	fake := w.alice
	fake.ssh = newSSHTestSigner(t, "alice").ssh
	w.wantCode("register_project", w.signedArgs(fake, "register_project", "proj-a", regFields(p, "default", true)), "bad_signature")

	for _, d := range []time.Duration{-6 * time.Minute, 6 * time.Minute} {
		w.wantCode("register_project", w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true),
			registrymsg.NewRequestID(), w.now.Add(d), signing.NamespaceRegistry), "stale_signature")
	}

	// signers file absent / broken -> signers_unavailable
	os.Remove(w.signers)
	w.wantCode("register_project", good(), "signers_unavailable")
	os.WriteFile(w.signers, []byte("{not json"), 0o644)
	w.wantCode("register_project", good(), "signers_unavailable")
	os.WriteFile(w.signers, []byte(`{"version":1,"signers":[]}`), 0o644)
	w.wantCode("register_project", good(), "unknown_signer")
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed after failed registrations")
	}

	// both signer kinds succeed (and the response is the updated project)
	bob := newWebTestSigner(t, "bob")
	w.writeSigners(w.alice, bob)
	res, e := w.do("register_project", good())
	if e != nil {
		t.Fatalf("ssh register: %v", e)
	}
	if pr, ok := res.(Project); !ok || pr.ID != "proj-a" || pr.Path != p || !pr.AIContext || pr.AutoMerge.Enabled {
		t.Fatalf("response: %#v", res)
	}
	p2 := w.repo("proj-b")
	if _, e := w.do("register_project", w.signedArgs(bob, "register_project", "proj-b", regFields(p2, "default", false))); e != nil {
		t.Fatalf("webauthn register: %v", e)
	}
	if pb, ok := w.find("proj-b"); !ok || pb.AIContext {
		t.Fatalf("proj-b: %#v", pb)
	}
}

func TestSignerRemovalTakesEffectImmediately(t *testing.T) {
	w := newWEnv(t)
	p := w.repo("proj-a")
	bob := newSSHTestSigner(t, "bob")
	w.writeSigners(w.alice, bob)
	if _, e := w.do("register_project", w.signedArgs(bob, "register_project", "proj-a", regFields(p, "default", true))); e != nil {
		t.Fatalf("bob before removal: %v", e)
	}
	w.writeSigners(w.alice) // root removes bob
	p2 := w.repo("proj-b")
	w.wantCode("register_project", w.signedArgs(bob, "register_project", "proj-b", regFields(p2, "default", true)), "unknown_signer")
	if _, ok := w.find("proj-b"); ok {
		t.Fatal("removed signer still registered a project")
	}
}

func TestRegisterChangeMismatchRefused(t *testing.T) {
	w := newWEnv(t)
	signedPath := w.repo("proj-a")
	other := w.repo("proj-x")
	before := w.regFile()
	signed := regFields(signedPath, "default", false)
	h, _ := registrymsg.ChangeHash("register_project", "proj-a", signed)
	for name, tamper := range map[string]func(map[string]any){
		"path":       func(a map[string]any) { a["path"] = other },
		"kind":       func(a map[string]any) { a["kind"] = "registered" },
		"ai_context": func(a map[string]any) { a["ai_context"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			args := w.signedArgs(w.alice, "register_project", "proj-a", signed)
			args["change"] = h // the signed change hash travels with the request
			tamper(args)
			w.wantCode("register_project", args, "change_mismatch")
			if !bytes.Equal(before, w.regFile()) {
				t.Fatal("registry changed")
			}
		})
	}
	// `change` is required: without it the request is refused before the
	// signers are read or the request ID is spent
	reqID := registrymsg.NewRequestID()
	nochange := w.signedArgsWith(w.alice, "register_project", "proj-a", signed, reqID, w.now, signing.NamespaceRegistry)
	delete(nochange, "change")
	w.wantCode("register_project", nochange, "change_mismatch")
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed")
	}
	honest := w.signedArgsWith(w.alice, "register_project", "proj-a", signed, reqID, w.now, signing.NamespaceRegistry)
	if _, e := w.do("register_project", honest); e != nil {
		t.Fatalf("same request ID after a refused no-change request: %v", e)
	}
	sfReq := registrymsg.NewRequestID()
	nosf := w.signedArgsWith(w.alice, "set_flag", "proj-a", flagFields("ai_context", true), sfReq, w.now, signing.NamespaceRegistry)
	delete(nosf, "change")
	afterReg := w.regFile()
	w.wantCode("set_flag", nosf, "change_mismatch")
	if !bytes.Equal(afterReg, w.regFile()) {
		t.Fatal("registry changed")
	}
	if _, e := w.do("set_flag", w.signedArgsWith(w.alice, "set_flag", "proj-a", flagFields("ai_context", true), sfReq, w.now, signing.NamespaceRegistry)); e != nil {
		t.Fatalf("set_flag same request ID after refusal: %v", e)
	}
	// not_signed still comes first
	both := w.signedArgs(w.alice, "register_project", "proj-z", regFields(other, "default", true))
	delete(both, "change")
	delete(both, "signature")
	w.wantCode("register_project", both, "not_signed")
	// set_flag tampered value (signed true, sent false) is refused; flag stays on
	sf := w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", true))
	sf["value"] = false
	w.wantCode("set_flag", sf, "change_mismatch")
	if pr, _ := w.find("proj-a"); !pr.AIContext {
		t.Fatal("set_flag applied a tampered value")
	}
}

func TestRegisterRequestIDOnce(t *testing.T) {
	w := newWEnv(t)
	p, p2 := w.repo("proj-a"), w.repo("proj-b")
	req := registrymsg.NewRequestID()
	a := w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true), req, w.now, signing.NamespaceRegistry)
	if _, e := w.do("register_project", a); e != nil {
		t.Fatalf("first: %v", e)
	}
	// the very same request again, and another valid signature reusing the ID
	w.wantCode("register_project", a, "request_reused")
	b := w.signedArgsWith(w.alice, "register_project", "proj-b", regFields(p2, "default", true), req, w.now, signing.NamespaceRegistry)
	w.wantCode("register_project", b, "request_reused")
	if _, ok := w.find("proj-b"); ok {
		t.Fatal("reused request ID registered a project")
	}
	// survives a broker restart
	s2 := w.restart(nil)
	raw, _ := json.Marshal(b)
	if _, e := s2.Handlers["register_project"](context.Background(), raw); codeOf(e) != "request_reused" {
		t.Fatalf("after restart: %v", e)
	}
	// set_flag shares the same ID space
	sf := w.signedArgsWith(w.alice, "set_flag", "proj-a", flagFields("ai_context", false), req, w.now, signing.NamespaceRegistry)
	w.wantCode("set_flag", sf, "request_reused")
	// the used-ID time is the broker clock: an hour later the ID is free again
	w.now = w.now.Add(61 * time.Minute)
	c := w.signedArgsWith(w.alice, "register_project", "proj-b", regFields(p2, "default", true), req, w.now, signing.NamespaceRegistry)
	if _, e := w.do("register_project", c); e != nil {
		t.Fatalf("after an hour: %v", e)
	}
}

func TestInvalidSignatureDoesNotConsumeRequestID(t *testing.T) {
	w := newWEnv(t)
	p := w.repo("proj-a")
	req := registrymsg.NewRequestID()
	bad := w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true), req, w.now, signing.NamespaceApproval)
	w.wantCode("register_project", bad, "bad_signature")
	tampered := w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true), req, w.now, signing.NamespaceRegistry)
	tampered["kind"] = "other"
	tampered["change"], _ = registrymsg.ChangeHash("register_project", "proj-a", regFields(p, "default", true))
	w.wantCode("register_project", tampered, "change_mismatch") // hash mismatch is also before Mark
	good := w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true), req, w.now, signing.NamespaceRegistry)
	if _, e := w.do("register_project", good); e != nil {
		t.Fatalf("honest request after failed attempts: %v", e)
	}
}

func TestValidSignatureConsumesRequestIDEvenWhenLaterCheckFails(t *testing.T) {
	w := newWEnv(t)
	req := registrymsg.NewRequestID()
	bad := w.signedArgsWith(w.alice, "register_project", "proj-a", regFields("/nowhere/proj-a", "default", true), req, w.now, signing.NamespaceRegistry)
	w.wantCode("register_project", bad, "path_not_allowed")
	w.wantCode("register_project", bad, "request_reused") // the same signature cannot be retried
}

func TestRegisterPathRules(t *testing.T) {
	w := newWEnv(t)
	good := w.repo("proj-a")
	real := w.repo("real")
	os.Symlink(real, filepath.Join(w.root, "link"))
	os.MkdirAll(filepath.Join(w.root, "nogit"), 0o755)
	deep := filepath.Join(w.root, "grp", "deep")
	os.MkdirAll(filepath.Join(deep, ".git"), 0o755)
	os.MkdirAll(filepath.Join(w.root, ".git"), 0o755) // the root itself looks like a repo
	outside := filepath.Join(w.e.Dir, "elsewhere")
	os.MkdirAll(filepath.Join(outside, ".git"), 0o755)
	os.WriteFile(filepath.Join(w.root, "afile"), []byte("x"), 0o644)
	before := w.regFile()
	cases := map[string]string{
		"dotdot":         w.root + "/grp/../proj-a",
		"dotdot suffix":  good + "/..",
		"double slash":   w.root + "//proj-a",
		"trailing slash": good + "/",
		"dot":            w.root + "/./proj-a",
		"relative":       "proj-a",
		"outside roots":  outside,
		"root itself":    w.root,
		"root parent":    filepath.Dir(w.root),
		"two levels":     deep,
		"too long":       filepath.Join(w.root, strings.Repeat("a", 4100)),
		"filesystem /":   "/",
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			w.wantCode("register_project", w.register("proj-z", p), "path_not_allowed")
		})
	}
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed")
	}
	// The broker is syntactic only (it cannot read /home/orch/repos by design):
	// what only the filesystem can tell (symlink, no .git, missing, plain file)
	// is orchd's job and the broker accepts it. Pinned on purpose.
	for name, p := range map[string]string{
		"symlink":    filepath.Join(w.root, "link"),
		"no .git":    filepath.Join(w.root, "nogit"),
		"missing":    filepath.Join(w.root, "absent"),
		"plain file": filepath.Join(w.root, "afile"),
	} {
		if _, e := w.do("register_project", w.register("fs-"+strings.ReplaceAll(strings.ReplaceAll(name, " ", "-"), ".", ""), p)); e != nil {
			t.Fatalf("%s: the broker must not judge the filesystem: %v", name, e)
		}
	}
	if _, e := w.do("register_project", w.register("proj-ok", good)); e != nil {
		t.Fatalf("a good path must work: %v", e)
	}
}

func TestRegisterPathRefusedWithoutRepoRoots(t *testing.T) {
	w := newWEnv(t)
	w.e.set(t, []string{"repo_roots"}, nil)
	s := w.restart(nil)
	p := w.repo("proj-a")
	raw, _ := json.Marshal(w.register("proj-a", p))
	if _, e := s.Handlers["register_project"](context.Background(), raw); codeOf(e) != "path_not_allowed" {
		t.Fatalf("no repo_roots: %v", e)
	}
}

func TestRegisterDuplicateIDOrPath(t *testing.T) {
	w := newWEnv(t)
	p := w.repo("proj-a")
	if _, e := w.do("register_project", w.register("proj-a", p)); e != nil {
		t.Fatal(e)
	}
	q := w.repo("proj-b")
	before := w.regFile()
	w.wantCode("register_project", w.register("proj-a", q), "project_exists")                                  // same id, other path
	w.wantCode("register_project", w.register("proj-b", p), "project_exists")                                  // same path, other id
	w.wantCode("register_project", w.register("repo-a", q), "project_exists")                                  // id of the migrated project
	w.wantCode("register_project", w.register("proj-b", filepath.Join(w.e.Dir, "repo-a")), "path_not_allowed") // migrated path is outside roots
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("existing entries changed")
	}
	if pr, _ := w.find("proj-a"); pr.Path != p || !pr.AIContext {
		t.Fatalf("existing entry changed: %#v", pr)
	}
}

func TestSetFlagAIContextOnlyAndSigned(t *testing.T) {
	w := newWEnv(t)
	flag := func(id string, v bool) map[string]any {
		return w.signedArgs(w.alice, "set_flag", id, flagFields("ai_context", v))
	}
	unsigned := flag("repo-a", false)
	delete(unsigned, "signature")
	w.wantCode("set_flag", unsigned, "not_signed")
	ghost := newSSHTestSigner(t, "ghost")
	w.wantCode("set_flag", w.signedArgs(ghost, "set_flag", "repo-a", flagFields("ai_context", false)), "unknown_signer")
	w.wantCode("set_flag", w.signedArgsWith(w.alice, "set_flag", "repo-a", flagFields("ai_context", false),
		registrymsg.NewRequestID(), w.now, signing.NamespaceApproval), "bad_signature")
	w.wantCode("set_flag", w.signedArgsWith(w.alice, "set_flag", "repo-a", flagFields("ai_context", false),
		registrymsg.NewRequestID(), w.now.Add(-10*time.Minute), signing.NamespaceRegistry), "stale_signature")
	if pr, _ := w.find("repo-a"); !pr.AIContext {
		t.Fatal("a refused request changed the flag")
	}

	res, e := w.do("set_flag", flag("repo-a", false))
	if e != nil {
		t.Fatalf("turn off: %v", e)
	}
	if pr, ok := res.(Project); !ok || pr.AIContext || pr.ID != "repo-a" {
		t.Fatalf("response: %#v", res)
	}
	if len(w.s.Registry.AIContextPaths()) != 0 {
		t.Fatal("status context_repos still lists the project")
	}
	if _, e := w.do("set_flag", flag("repo-a", true)); e != nil {
		t.Fatalf("turn on: %v", e)
	}
	if pr, _ := w.find("repo-a"); !pr.AIContext || pr.AutoMerge.Enabled {
		t.Fatalf("after on: %#v", pr)
	}

	// anything else is not supported in W3a, signed or not, true or false
	before := w.regFile()
	for _, f := range []string{"auto_merge", "auto_merge_global", "enabled", "provider"} {
		for _, v := range []bool{true, false} {
			w.wantCode("set_flag", w.signedArgs(w.alice, "set_flag", "repo-a", flagFields(f, v)), "not_supported")
		}
	}
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed by an unsupported flag")
	}
	if g := w.s.Registry.Snapshot().AutoMergeGlobal; g.Enabled {
		t.Fatal("global switch turned on")
	}
}

func TestSetFlagUnknownProject(t *testing.T) {
	w := newWEnv(t)
	before := w.regFile()
	w.wantCode("set_flag", w.signedArgs(w.alice, "set_flag", "nope", flagFields("ai_context", true)), "project_unknown")
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed")
	}
}

func TestWriteOpsNeverChangeRegistryOnAnyFailure(t *testing.T) {
	w := newWEnv(t)
	p := w.repo("proj-a")
	if _, e := w.do("register_project", w.register("proj-a", p)); e != nil {
		t.Fatal(e)
	}
	before := w.regFile()
	used := w.register("proj-b", w.repo("proj-b"))
	if _, e := w.do("register_project", used); e != nil {
		t.Fatal(e)
	}
	before = w.regFile()
	mut := func(base map[string]any, f func(map[string]any)) map[string]any {
		c := map[string]any{}
		for k, v := range base {
			c[k] = v
		}
		f(c)
		return c
	}
	reg := w.register("proj-c", w.repo("proj-c"))
	sf := w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", false))
	type c struct {
		op   string
		args map[string]any
		code string
	}
	cases := []c{
		{"register_project", mut(reg, func(a map[string]any) { delete(a, "signature") }), "not_signed"},
		{"register_project", mut(reg, func(a map[string]any) { delete(a, "change") }), "change_mismatch"},
		{"set_flag", mut(sf, func(a map[string]any) { delete(a, "change") }), "change_mismatch"},
		{"register_project", mut(reg, func(a map[string]any) { a["kind"] = "zzz" }), "change_mismatch"},
		{"register_project", mut(reg, func(a map[string]any) { a["extra"] = 1 }), "bad_request"},
		{"register_project", mut(reg, func(a map[string]any) { a["id"] = "Bad_ID" }), "bad_request"},
		{"register_project", mut(reg, func(a map[string]any) { a["issued"] = "yesterday" }), "bad_request"},
		{"register_project", mut(reg, func(a map[string]any) { a["request"] = "short" }), "bad_request"},
		{"register_project", mut(reg, func(a map[string]any) { a["change"] = "xyz" }), "bad_request"},
		{"register_project", mut(reg, func(a map[string]any) { delete(a, "ai_context") }), "bad_request"},
		{"register_project", mut(reg, func(a map[string]any) { a["path"] = "../x" }), "change_mismatch"},
		{"register_project", used, "request_reused"},
		{"register_project", w.register("proj-a", w.repo("proj-d")), "project_exists"},
		{"set_flag", mut(sf, func(a map[string]any) { a["value"] = true }), "change_mismatch"},
		{"set_flag", mut(sf, func(a map[string]any) { a["flag"] = "auto_merge" }), "not_supported"},
		{"set_flag", mut(sf, func(a map[string]any) { a["id"] = "proj-b" }), "bad_signature"},
		{"set_flag", mut(sf, func(a map[string]any) { a["signature"] = "x" }), "bad_request"},
		{"set_flag", w.signedArgs(w.alice, "set_flag", "ghost-project", flagFields("ai_context", true)), "project_unknown"},
		{"register_project", mut(reg, func(a map[string]any) {
			a["signature"] = map[string]any{"signer": "alice", "alg": "ssh-ed25519", "sig": "x", "bogus": 1}
		}), "bad_request"},
	}
	for i, tc := range cases {
		_, e := w.do(tc.op, tc.args)
		if codeOf(e) != tc.code {
			t.Fatalf("case %d (%s): code %q, want %q", i, tc.op, codeOf(e), tc.code)
		}
		if !bytes.Equal(before, w.regFile()) {
			t.Fatalf("case %d (%s) changed the registry", i, tc.op)
		}
	}
	// malformed raw args
	for _, raw := range []string{``, `null`, `[]`, `"x"`, `{"id":`, `{"id":"a"} {"id":"b"}`} {
		_, e := w.s.Handlers["register_project"](context.Background(), json.RawMessage(raw))
		if codeOf(e) != "bad_request" {
			t.Fatalf("raw %q: %v", raw, e)
		}
		_, e = w.s.Handlers["set_flag"](context.Background(), json.RawMessage(raw))
		if codeOf(e) != "bad_request" {
			t.Fatalf("set_flag raw %q: %v", raw, e)
		}
	}
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed")
	}
}

var newCodes = []string{"not_signed", "bad_signature", "unknown_signer", "stale_signature", "signers_unavailable",
	"request_reused", "change_mismatch", "project_exists", "project_unknown", "path_not_allowed",
	"registry_unavailable", "not_supported"}

func TestFixedMessagesRegisteredForNewCodes(t *testing.T) {
	seen := map[string]string{}
	for _, c := range newCodes {
		m, ok := fixedMessages[c]
		if !ok || strings.TrimSpace(m) == "" {
			t.Fatalf("code %s has no fixed message", c)
		}
		if prev, dup := seen[m]; dup {
			t.Fatalf("codes %s and %s share a sentence", prev, c)
		}
		seen[m] = c
	}
	// over the wire a failing op answers with exactly the fixed sentence
	w := newWEnv(t)
	r, _ := call(t, w.sock, `{"v":1,"op":"register_project","args":{"id":"proj-a","path":"/x/REFLECT-ME","kind":"default","ai_context":true,"request":"`+
		registrymsg.NewRequestID()+`","issued":"2026-11-09T12:00:00Z"}}`)
	if r.OK || r.Code != "not_signed" || r.Message != fixedMessages["not_signed"] || strings.Contains(r.Message, "REFLECT") {
		t.Fatalf("reply: %+v", r)
	}
}

func TestOpAllowlistIncludesOnlyNewOps(t *testing.T) {
	w := newWEnv(t)
	var got []string
	for k := range w.s.Handlers {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"create_mr", "delete_token", "projects", "refine", "register_project", "set_flag", "set_token", "status", "tokens"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ops = %v, want %v", got, want)
	}
	for _, op := range []string{"delete_project", "unregister_project", "remove_project",
		"set_provider", "set_auto_merge", "update_project", "delete", "register"} {
		r, _ := call(t, w.sock, `{"v":1,"op":"`+op+`","args":{}}`)
		if r.OK || r.Code != "unknown_op" {
			t.Fatalf("op %s: %+v", op, r)
		}
	}
	// the new ops are reachable
	for _, op := range []string{"projects", "register_project", "set_flag"} {
		r, _ := call(t, w.sock, `{"v":1,"op":"`+op+`","args":{}}`)
		if r.Code == "unknown_op" {
			t.Fatalf("op %s is not registered", op)
		}
	}
}

func TestWriteOpsRefuseWhenRegistryBrokenOrUsedFileBroken(t *testing.T) {
	// used-requests file broken: a validly signed request is refused, nothing changes
	w := newWEnv(t)
	p := w.repo("proj-a")
	before := w.regFile()
	if err := os.WriteFile(filepath.Join(w.e.Dir, "state", UsedRequestsFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := w.restart(nil)
	for _, tc := range []struct {
		op   string
		args map[string]any
	}{
		{"register_project", w.register("proj-a", p)},
		{"set_flag", w.signedArgs(w.alice, "set_flag", "repo-a", flagFields("ai_context", false))},
	} {
		raw, _ := json.Marshal(tc.args)
		if _, e := s.Handlers[tc.op](context.Background(), raw); codeOf(e) != "registry_unavailable" {
			t.Fatalf("%s with broken used file: %v", tc.op, e)
		}
	}
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed")
	}

	// registry broken: both writes and the read are refused, the file is not touched
	w2 := newWEnv(t)
	p2 := w2.repo("proj-a")
	reg := filepath.Join(w2.e.Dir, "state", RegistryFileName)
	os.WriteFile(reg, []byte("{broken"), 0o600)
	s2 := w2.restart(nil)
	for _, tc := range []struct {
		op   string
		args map[string]any
	}{
		{"register_project", w2.register("proj-a", p2)},
		{"set_flag", w2.signedArgs(w2.alice, "set_flag", "repo-a", flagFields("ai_context", false))},
		{"projects", nil},
	} {
		raw, _ := json.Marshal(tc.args)
		if _, e := s2.Handlers[tc.op](context.Background(), raw); codeOf(e) != "registry_unavailable" {
			t.Fatalf("%s with broken registry: %v", tc.op, e)
		}
	}
	if b, _ := os.ReadFile(reg); string(b) != "{broken" {
		t.Fatalf("broken registry file was touched: %q", b)
	}
}

func TestConcurrentRegisterSameIDCreatesOnce(t *testing.T) {
	w := newWEnv(t)
	const n = 12
	paths := make([]string, n)
	args := make([]map[string]any, n)
	for i := range args {
		paths[i] = w.repo(fmt.Sprintf("dir-%d", i))
		args[i] = w.register("same", paths[i])
	}
	codes := make([]string, n)
	var wg sync.WaitGroup
	for i := range args {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(args[i])
			_, e := w.s.Handlers["register_project"](context.Background(), raw)
			codes[i] = codeOf(e)
		}()
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case "":
			ok++
		case "project_exists":
		default:
			t.Fatalf("unexpected code %q", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d registrations succeeded: %v", ok, codes)
	}
	count := 0
	for _, p := range w.projects() {
		if p.ID == "same" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("registry has %d entries", count)
	}
}

func TestRegisterUsesBrokerClockForUsedRequests(t *testing.T) {
	// the signed `issued` is 4 minutes old; the used-ID store must stamp the
	// broker's clock, so the entry expires one hour after the broker saw it
	w := newWEnv(t)
	p := w.repo("proj-a")
	req := registrymsg.NewRequestID()
	a := w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true), req, w.now.Add(-4*time.Minute), signing.NamespaceRegistry)
	if _, e := w.do("register_project", a); e != nil {
		t.Fatal(e)
	}
	b, err := os.ReadFile(filepath.Join(w.e.Dir, "state", UsedRequestsFileName))
	if err != nil {
		t.Fatal(err)
	}
	var f usedFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if f.Entries[req] != w.now.Unix() {
		t.Fatalf("stamp %d, want broker time %d", f.Entries[req], w.now.Unix())
	}
}

// Every refusal that happens before the request ID is spent leaves it usable.
func TestFailedRequestsDoNotConsumeRequestID(t *testing.T) {
	w := newWEnv(t)
	ghost := newSSHTestSigner(t, "ghost")
	type fc struct {
		code  string
		build func(id, path, req string) map[string]any
		prep  func() func()
	}
	valid := func(who testSigner, id, path, req string, issued time.Time, ns string) map[string]any {
		return w.signedArgsWith(who, "register_project", id, regFields(path, "default", true), req, issued, ns)
	}
	cases := []fc{
		{code: "unknown_signer", build: func(id, p, r string) map[string]any { return valid(ghost, id, p, r, w.now, signing.NamespaceRegistry) }},
		{code: "stale_signature", build: func(id, p, r string) map[string]any {
			return valid(w.alice, id, p, r, w.now.Add(-time.Hour), signing.NamespaceRegistry)
		}},
		{code: "signers_unavailable", build: func(id, p, r string) map[string]any {
			return valid(w.alice, id, p, r, w.now, signing.NamespaceRegistry)
		},
			prep: func() func() {
				os.Remove(w.signers)
				return func() { w.writeSigners(w.alice) }
			}},
		{code: "not_signed", build: func(id, p, r string) map[string]any {
			a := valid(w.alice, id, p, r, w.now, signing.NamespaceRegistry)
			delete(a, "signature")
			return a
		}},
		{code: "bad_request", build: func(id, p, r string) map[string]any {
			a := valid(w.alice, id, p, r, w.now, signing.NamespaceRegistry)
			a["extra"] = true
			return a
		}},
		{code: "bad_signature", build: func(id, p, r string) map[string]any {
			return valid(w.alice, id, p, r, w.now, signing.NamespaceApproval)
		}},
		{code: "change_mismatch", build: func(id, p, r string) map[string]any {
			a := valid(w.alice, id, p, r, w.now, signing.NamespaceRegistry)
			delete(a, "change")
			return a
		}},
	}
	for i, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			id := fmt.Sprintf("proj-%d", i)
			p := w.repo(id)
			req := registrymsg.NewRequestID()
			restore := func() {}
			if tc.prep != nil {
				restore = tc.prep()
			}
			w.wantCode("register_project", tc.build(id, p, req), tc.code)
			restore()
			if _, e := w.do("register_project", valid(w.alice, id, p, req, w.now, signing.NamespaceRegistry)); e != nil {
				t.Fatalf("honest request with the same ID after %s: %v", tc.code, e)
			}
		})
	}
}

func TestArgsKeysAreExactCaseAndUnique(t *testing.T) {
	w := newWEnv(t)
	p := w.repo("proj-a")
	before := w.regFile()
	rawArgs := func() string {
		b, _ := json.Marshal(w.register("proj-a", p))
		return string(b)
	}
	do := func(op, raw string) string {
		_, e := w.s.Handlers[op](context.Background(), json.RawMessage(raw))
		return codeOf(e)
	}
	cases := map[string]string{
		"Path":          strings.Replace(rawArgs(), `"path"`, `"Path"`, 1),
		"Signature":     strings.Replace(rawArgs(), `"signature"`, `"Signature"`, 1),
		"Signer":        strings.Replace(rawArgs(), `"signer"`, `"Signer"`, 1),
		"Sig":           strings.Replace(rawArgs(), `"sig"`, `"SIG"`, 1),
		"path dup":      strings.TrimSuffix(rawArgs(), "}") + `,"path":"` + p + `"}`,
		"Path dup":      strings.TrimSuffix(rawArgs(), "}") + `,"Path":"` + p + `"}`,
		"request dup":   strings.TrimSuffix(rawArgs(), "}") + `,"request":"` + strings.Repeat("a", 32) + `"}`,
		"signer dup":    strings.Replace(rawArgs(), `"signer":"alice"`, `"signer":"alice","signer":"alice"`, 1),
		"AI_context":    strings.Replace(rawArgs(), `"ai_context"`, `"AI_Context"`, 1),
		"not an obj":    `[1]`,
		"nested sigstr": strings.Replace(rawArgs(), `"signature":{`, `"signature":{"Alg":"x",`, 1),
	}
	for name, raw := range cases {
		if c := do("register_project", raw); c != "bad_request" {
			t.Fatalf("%s: code %q", name, c)
		}
	}
	sraw, _ := json.Marshal(w.signedArgs(w.alice, "set_flag", "repo-a", flagFields("ai_context", false)))
	if c := do("set_flag", strings.Replace(string(sraw), `"flag"`, `"Flag"`, 1)); c != "bad_request" {
		t.Fatalf("set_flag Flag: %q", c)
	}
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed")
	}
	// the exact-case request still works
	if c := do("register_project", rawArgs()); c != "" {
		t.Fatalf("honest: %q", c)
	}
}

func TestRegisterGitEntryRules(t *testing.T) {
	// The broker no longer looks at .git at all: every shape is accepted.
	w := newWEnv(t)
	mk := func(name string) string {
		p := filepath.Join(w.root, name)
		os.MkdirAll(p, 0o755)
		return p
	}
	link := mk("gitlink")
	os.Symlink(w.repo("target"), filepath.Join(link, ".git"))
	file := mk("gitfile")
	os.WriteFile(filepath.Join(file, ".git"), []byte("gitdir: ../elsewhere\n"), 0o644)
	for id, p := range map[string]string{"proj-link": link, "proj-file": file, "proj-dir": w.repo("gitdir")} {
		if _, e := w.do("register_project", w.register(id, p)); e != nil {
			t.Fatalf("%s: %v", id, e)
		}
	}
}

func TestRegisterDoesNoFilesystemAccess(t *testing.T) {
	// A valid signature for a path that exists nowhere, under a root that exists
	// nowhere: accepted, so the broker did not stat anything.
	w := newWEnv(t)
	ghostRoot := "/nonexistent-aigate-root-w3a"
	w.e.set(t, []string{"repo_roots"}, []string{ghostRoot})
	s := w.restart(nil)
	args := w.register("ghost", ghostRoot+"/ghost")
	raw, _ := json.Marshal(args)
	if _, e := s.Handlers["register_project"](context.Background(), raw); e != nil {
		t.Fatalf("register under a non-existent root: %v", e)
	}
	if _, err := os.Lstat(ghostRoot); err == nil {
		t.Fatal("the broker created the path")
	}
	// Still syntactic: not one level below the root, or the root itself.
	for _, p := range []string{ghostRoot, ghostRoot + "/a/b", "/nonexistent-aigate-root-w3b/x"} {
		raw, _ := json.Marshal(w.register("g2", p))
		if _, e := s.Handlers["register_project"](context.Background(), raw); codeOf(e) != "path_not_allowed" {
			t.Fatalf("%s: %v", p, e)
		}
	}
}

func TestWriteOpsHaveExplicitTimeouts(t *testing.T) {
	for _, op := range []string{"projects", "register_project", "set_flag"} {
		if d, ok := defaultOpTimeouts[op]; !ok || d != 10*time.Second {
			t.Fatalf("op %s timeout = %v, %v", op, d, ok)
		}
	}
}

func TestSignatureNotReusableForOtherActionOrSubject(t *testing.T) {
	w := newWEnv(t)
	pa, pb := w.repo("proj-a"), w.repo("proj-b")
	before := w.regFile()
	// a register_project signature replayed as set_flag
	reg := w.register("repo-a", pa)
	sf := map[string]any{"id": "repo-a", "flag": "ai_context", "value": false, "request": reg["request"], "issued": reg["issued"], "signature": reg["signature"]}
	sf["change"], _ = registrymsg.ChangeHash("set_flag", "repo-a", flagFields("ai_context", false))
	w.wantCode("set_flag", sf, "bad_signature")
	// a signature over subject A used for project B
	a := w.register("proj-a", pa)
	a["id"] = "proj-b"
	a["path"] = pb
	a["change"], _ = registrymsg.ChangeHash("register_project", "proj-b", regFields(pb, "default", true))
	w.wantCode("register_project", a, "bad_signature")
	// ... and the same with the signed change hash left in place
	a2 := w.register("proj-a", pa)
	a2["id"] = "proj-b"
	w.wantCode("register_project", a2, "bad_signature")
	if !bytes.Equal(before, w.regFile()) {
		t.Fatal("registry changed")
	}
}

func TestConcurrentIdenticalRequestsRegisterOnce(t *testing.T) {
	w := newWEnv(t)
	args := w.register("same", w.repo("only"))
	raw, _ := json.Marshal(args)
	const n = 12
	codes := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := w.s.Handlers["register_project"](context.Background(), raw)
			codes[i] = codeOf(e)
		}()
	}
	wg.Wait()
	ok, reused := 0, 0
	for _, c := range codes {
		switch c {
		case "":
			ok++
		case "request_reused":
			reused++
		default:
			t.Fatalf("unexpected code %q", c)
		}
	}
	if ok != 1 || reused != n-1 {
		t.Fatalf("ok=%d reused=%d: %v", ok, reused, codes)
	}
	count := 0
	for _, p := range w.projects() {
		if p.ID == "same" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d projects", count)
	}
}
