package broker

// Adversarial suite for W3a (Task 6) on the broker side: the registry write
// ops (register_project, set_flag) and the registry itself. Everything runs on
// temp dirs with per-test generated signers and fake upstreams; nothing here
// touches a real key, server or network. Every refusal test keeps an accepted
// control next to it (the same request, made honest, succeeds), so a test can
// not pass just because everything is refused.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// --- helpers ---

func (w *wenv) usedFile() []byte {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.e.Dir, "state", UsedRequestsFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		w.t.Fatal(err)
	}
	return b
}

// wire sends one request over the broker socket (peer check included).
func (w *wenv) wire(op string, args any) reply {
	w.t.Helper()
	r, _ := call(w.t, w.sock, line(w.t, rpc(op, args)))
	return r
}

func (w *wenv) wantWire(op string, args any, ok bool, code string) {
	w.t.Helper()
	r := w.wire(op, args)
	if r.OK != ok || r.Code != code {
		w.t.Fatalf("%s: ok=%v code=%q, want ok=%v code=%q", op, r.OK, r.Code, ok, code)
	}
}

func copyArgs(base map[string]any, f func(map[string]any)) map[string]any {
	c := map[string]any{}
	for k, v := range base {
		c[k] = v
	}
	f(c)
	return c
}

// stateSnapshot is the bytes of the registry and the used-request file.
type stateSnapshot struct{ reg, used []byte }

func (w *wenv) snap() stateSnapshot { return stateSnapshot{w.regFile(), w.usedFile()} }

func (w *wenv) mustEqual(before stateSnapshot, what string) {
	w.t.Helper()
	if !bytes.Equal(before.reg, w.regFile()) {
		w.t.Fatalf("%s: the registry changed", what)
	}
	if !bytes.Equal(before.used, w.usedFile()) {
		w.t.Fatalf("%s: a request ID was consumed", what)
	}
}

func (w *wenv) noTempFiles() {
	w.t.Helper()
	ents, err := os.ReadDir(filepath.Join(w.e.Dir, "state"))
	if err != nil {
		w.t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".state-") {
			w.t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// webSignDigest signs a WebAuthn assertion whose challenge is digest.
func webSignDigest(t *testing.T, key *ecdsa.PrivateKey, id string, digest [32]byte) signing.Signature {
	t.Helper()
	rpHash := sha256.Sum256([]byte(testRPID))
	authData := append(append([]byte{}, rpHash[:]...), 0x05)
	authData = binary.BigEndian.AppendUint32(authData, 1)
	cd, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(digest[:]), "origin": testOrigin})
	if err != nil {
		t.Fatal(err)
	}
	cdHash := sha256.Sum256(cd)
	sum := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	der, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing.Signature{Signer: id, Alg: "webauthn-es256", Sig: b64u(der),
		WebAuthn: &signing.WebAuthnData{AuthenticatorData: b64u(authData), ClientDataJSON: b64u(cd)}}
}

func approvalMessage(issued time.Time) signing.Message {
	return signing.Message{Kind: signing.KindProtectedModified, Repo: strings.Repeat("a", 12), Task: 7,
		Base: strings.Repeat("b", 40), Hash: strings.Repeat("c", 64), Issued: issued}
}

// --- tests ---

func TestRegistryNeverWrittenWithoutValidSignature(t *testing.T) {
	w := newWEnv(t)
	bob := newWebTestSigner(t, "bob")
	w.writeSigners(w.alice, bob)
	if _, e := w.do("register_project", w.register("proj-a", w.repo("proj-a"))); e != nil {
		t.Fatal(e)
	}
	reg := w.signedArgs(w.alice, "register_project", "proj-b", regFields(w.repo("proj-b"), "default", true))
	sf := w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", false))
	other := w.signedArgs(w.alice, "register_project", "proj-z", regFields(w.repo("proj-z"), "default", true))
	badNS := w.signedArgsWith(w.alice, "register_project", "proj-b", regFields(w.repo("proj-b"), "default", true),
		registrymsg.NewRequestID(), w.now, signing.NamespaceApproval)
	stale := w.signedArgsWith(w.alice, "register_project", "proj-b", regFields(w.repo("proj-b"), "default", true),
		registrymsg.NewRequestID(), w.now.Add(-6*time.Minute), signing.NamespaceRegistry)
	future := w.signedArgsWith(w.alice, "register_project", "proj-b", regFields(w.repo("proj-b"), "default", true),
		registrymsg.NewRequestID(), w.now.Add(6*time.Minute), signing.NamespaceRegistry)
	flip := func(a map[string]any) {
		s := a["signature"].(signing.Signature)
		raw := []byte(s.Sig)
		i := len(raw) / 2
		if raw[i] == 'A' {
			raw[i] = 'B'
		} else {
			raw[i] = 'A'
		}
		s.Sig = string(raw)
		a["signature"] = s
	}
	type c struct {
		name string
		op   string
		args map[string]any
		code string
		pre  func() // runs before the request (and is undone by undo)
		undo func()
	}
	cases := []c{
		{name: "no signature", op: "register_project", args: copyArgs(reg, func(a map[string]any) { delete(a, "signature") }), code: "not_signed"},
		{name: "null signature", op: "set_flag", args: copyArgs(sf, func(a map[string]any) { a["signature"] = nil }), code: "not_signed"},
		{name: "flipped signature", op: "register_project", args: copyArgs(reg, flip), code: "bad_signature"},
		{name: "flipped signature set_flag", op: "set_flag", args: copyArgs(sf, flip), code: "bad_signature"},
		{name: "approval namespace", op: "register_project", args: badNS, code: "bad_signature"},
		{name: "unknown signer", op: "register_project", args: copyArgs(reg, func(a map[string]any) {
			s := a["signature"].(signing.Signature)
			s.Signer = "mallory"
			a["signature"] = s
		}), code: "unknown_signer"},
		{name: "signer id of another key", op: "register_project", args: copyArgs(reg, func(a map[string]any) {
			s := a["signature"].(signing.Signature)
			s.Signer = "bob" // alice's ssh signature under bob's (webauthn) entry
			a["signature"] = s
		}), code: "bad_signature"},
		{name: "stale", op: "register_project", args: stale, code: "stale_signature"},
		{name: "from the future", op: "register_project", args: future, code: "stale_signature"},
		{name: "no change", op: "register_project", args: copyArgs(reg, func(a map[string]any) { delete(a, "change") }), code: "change_mismatch"},
		{name: "wrong change", op: "register_project", args: copyArgs(reg, func(a map[string]any) { a["change"] = strings.Repeat("0", 64) }), code: "bad_signature"},
		{name: "other content", op: "register_project", args: copyArgs(reg, func(a map[string]any) { a["kind"] = "other" }), code: "change_mismatch"},
		{name: "signature of another request", op: "register_project", args: copyArgs(reg, func(a map[string]any) {
			a["signature"] = other["signature"]
		}), code: "bad_signature"},
		{name: "extra field", op: "register_project", args: copyArgs(reg, func(a map[string]any) { a["extra"] = true }), code: "bad_request"},
		{name: "unsupported flag", op: "set_flag", args: copyArgs(sf, func(a map[string]any) { a["flag"] = "auto_merge" }), code: "not_supported"},
		{name: "signers file missing", op: "register_project", args: reg, code: "signers_unavailable",
			pre: func() { os.Remove(w.signers) }, undo: func() { w.writeSigners(w.alice, bob) }},
		{name: "signers file broken", op: "register_project", args: reg, code: "signers_unavailable",
			pre: func() { os.WriteFile(w.signers, []byte("{"), 0o644) }, undo: func() { w.writeSigners(w.alice, bob) }},
		{name: "signers file group-writable", op: "register_project", args: reg, code: "signers_unavailable",
			pre: func() { os.Chmod(w.signers, 0o664) }, undo: func() { os.Chmod(w.signers, 0o644) }},
		{name: "signers empty", op: "register_project", args: reg, code: "unknown_signer",
			pre: func() { w.writeSigners() }, undo: func() { w.writeSigners(w.alice, bob) }},
	}
	before := w.snap()
	for _, tc := range cases {
		if tc.pre != nil {
			tc.pre()
		}
		r := w.wire(tc.op, tc.args)
		if tc.undo != nil {
			tc.undo()
		}
		if r.OK || r.Code != tc.code {
			t.Fatalf("%s: ok=%v code=%q, want %q", tc.name, r.OK, r.Code, tc.code)
		}
		w.mustEqual(before, tc.name)
	}
	// Garbage on the wire changes nothing either.
	for _, raw := range []string{"", "garbage", `{"v":1,"op":"register_project"}`, `{"v":1,"op":"register_project","args":null}`,
		`{"v":1,"op":"set_flag","args":[]}`, `{"v":2,"op":"register_project","args":{}}`} {
		roundTrip(t, w.sock, []byte(raw+"\n"))
		w.mustEqual(before, "wire "+raw)
	}
	big := append([]byte(`{"v":1,"op":"register_project","args":{"id":"`), bytes.Repeat([]byte("a"), MaxRequestBytes+10)...)
	roundTrip(t, w.sock, append(big, '\n'))
	w.mustEqual(before, "oversize")
	w.noTempFiles()
	// Controls: the very same honest requests are accepted afterwards (no ID was burned).
	w.wantWire("register_project", reg, true, "")
	w.wantWire("set_flag", sf, true, "")
	if p, ok := w.find("proj-b"); !ok || p.Path != filepath.Join(w.root, "proj-b") {
		t.Fatalf("proj-b not registered: %+v", w.projects())
	}
	if p, _ := w.find("proj-a"); p.AIContext {
		t.Fatal("set_flag did not apply")
	}
}

func TestForeignUIDCannotWrite(t *testing.T) {
	var uid atomic.Uint32
	var peerErr atomic.Bool
	uid.Store(4242)
	w := newWEnvMod(t, func(s *Server) {
		s.PeerUID = func(net.Conn) (uint32, error) {
			if peerErr.Load() {
				return 0, net.ErrClosed
			}
			return uid.Load(), nil
		}
		s.AllowUID = func(u uint32) bool { return u == 1 }
	})
	reg := w.signedArgs(w.alice, "register_project", "proj-a", regFields(w.repo("proj-a"), "default", true))
	sf := w.signedArgs(w.alice, "set_flag", "repo-a", flagFields("ai_context", false))
	before := w.snap()
	// Fully valid, fresh, correctly signed requests from a caller that is not on the list.
	for _, op := range []string{"register_project", "set_flag", "projects", "status"} {
		for _, a := range []any{reg, sf, nil, "junk"} {
			r := w.wire(op, a)
			if r.OK || r.Code != "forbidden" {
				t.Fatalf("%s from a foreign uid: ok=%v code=%q", op, r.OK, r.Code)
			}
		}
	}
	if r, out := call(t, w.sock, "garbage"); r.OK || r.Code != "forbidden" {
		t.Fatalf("garbage: %s", out)
	}
	w.mustEqual(before, "foreign uid")
	// The uid error path (credentials unreadable) is refused too.
	peerErr.Store(true)
	w.wantWire("register_project", reg, false, "forbidden")
	w.mustEqual(before, "peer credential error")
	// Control: an allowed caller makes the same requests succeed, so the foreign
	// caller burned no request ID and the requests were valid.
	peerErr.Store(false)
	uid.Store(1)
	w.wantWire("register_project", reg, true, "")
	w.wantWire("set_flag", sf, true, "")
}

func TestOrchdIdentityCannotReadOrWriteRegistryFile(t *testing.T) {
	// orchd runs as another account: it must neither read nor write the state
	// files. macOS cannot switch uid here, so the property is asserted as owner
	// and mode of everything the broker writes, plus the broker's own refusal to
	// trust anything looser or foreign-owned.
	w := newWEnv(t)
	if _, e := w.do("register_project", w.register("proj-a", w.repo("proj-a"))); e != nil {
		t.Fatal(e)
	}
	state := filepath.Join(w.e.Dir, "state")
	me := uint32(os.Getuid())
	check := func(p string, wantMode os.FileMode) {
		t.Helper()
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != wantMode || !(fi.Mode().IsRegular() || fi.IsDir()) {
			t.Fatalf("%s: mode %v", p, fi.Mode())
		}
		if uidOf(t, fi) != me {
			t.Fatalf("%s: wrong owner", p)
		}
	}
	check(state, 0o700)
	check(filepath.Join(state, RegistryFileName), 0o600)
	check(filepath.Join(state, UsedRequestsFileName), 0o600)
	ents, _ := os.ReadDir(state)
	for _, e := range ents {
		fi, _ := e.Info()
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is readable by group/other: %v", e.Name(), fi.Mode())
		}
	}
	// A loosened state dir or file, or a foreign owner, makes the registry
	// untrusted (fail closed), never silently readable.
	for name, mut := range map[string]func(){
		"dir 0750":      func() { os.Chmod(state, 0o750) },
		"dir 0755":      func() { os.Chmod(state, 0o755) },
		"file 0640":     func() { os.Chmod(filepath.Join(state, RegistryFileName), 0o640) },
		"file 0644":     func() { os.Chmod(filepath.Join(state, RegistryFileName), 0o644) },
		"file 0602":     func() { os.Chmod(filepath.Join(state, RegistryFileName), 0o602) },
		"file 0400":     func() { os.Chmod(filepath.Join(state, RegistryFileName), 0o400) },
		"file symlink":  func() { linkInstead(t, filepath.Join(state, RegistryFileName)) },
		"used file 664": func() { os.Chmod(filepath.Join(state, UsedRequestsFileName), 0o664) },
	} {
		w2 := newWEnv(t)
		st2 := filepath.Join(w2.e.Dir, "state")
		if _, e := w2.do("register_project", w2.register("proj-a", w2.repo("proj-a"))); e != nil {
			t.Fatal(e)
		}
		state = st2
		mut()
		s, err := New(w2.e.load(t), Options{StateDir: st2, KeyOwnerOK: func(uint32) bool { return true }})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.HasPrefix(name, "used") {
			if s.Used == nil || s.Used.Mark(registrymsg.NewRequestID(), w2.now) == nil {
				t.Fatalf("%s: used-request file trusted", name)
			}
			continue
		}
		if s.Registry.Err() == nil {
			t.Fatalf("%s: registry trusted", name)
		}
		if r := s.Registry.Update(func(*Registry) error { return nil }); r == nil {
			t.Fatalf("%s: write allowed on an untrusted registry", name)
		}
	}
	// Foreign-owned file (seam): never read.
	w3 := newWEnv(t)
	w3.do("register_project", w3.register("proj-a", w3.repo("proj-a")))
	st3 := filepath.Join(w3.e.Dir, "state")
	st, err := OpenRegistry(st3, LegacySource{}, RegistryOptions{fileOwnerOK: func(uint32) bool { return false }})
	if err == nil || st.Err() == nil {
		t.Fatal("a file owned by someone else was trusted")
	}
	if len(st.Snapshot().Projects) != 0 {
		t.Fatal("a foreign-owned registry was read")
	}
}

func TestReplayAfterRestartRefused(t *testing.T) {
	w := newWEnv(t)
	reg := w.register("proj-a", w.repo("proj-a"))
	w.wantWire("register_project", reg, true, "")
	sf := w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", false))
	w.wantWire("set_flag", sf, true, "")
	after := w.snap()
	// Same bytes again, on the running broker and after a restart; and the
	// project removed from the file changes nothing about the used ID.
	w.wantWire("register_project", reg, false, "request_reused")
	w.wantWire("set_flag", sf, false, "request_reused")
	s2 := w.restart(nil)
	for _, c := range []struct {
		op   string
		args map[string]any
	}{{"register_project", reg}, {"set_flag", sf}} {
		raw, _ := json.Marshal(c.args)
		if _, e := s2.Handlers[c.op](t.Context(), raw); codeOf(e) != "request_reused" {
			t.Fatalf("%s after restart: %v", c.op, e)
		}
	}
	// The same signature under a different action keeps failing, the ID stays spent.
	again := copyArgs(sf, func(a map[string]any) { a["value"] = true })
	raw, _ := json.Marshal(again)
	if _, e := s2.Handlers["set_flag"](t.Context(), raw); e == nil {
		t.Fatal("a replay with other content was accepted")
	}
	w.mustEqual(after, "replay")
	// Control: a fresh ID works on the restarted broker.
	raw, _ = json.Marshal(w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", true)))
	if _, e := s2.Handlers["set_flag"](t.Context(), raw); e != nil {
		t.Fatal(e)
	}
}

func TestSignatureOverDifferentChangeRefused(t *testing.T) {
	for _, who := range []func(*testing.T) testSigner{
		func(t *testing.T) testSigner { return newSSHTestSigner(t, "alice") },
		func(t *testing.T) testSigner { return newWebTestSigner(t, "alice") },
	} {
		w := newWEnv(t)
		a := who(t)
		w.writeSigners(a)
		p := w.repo("proj-a")
		// alice signed "register proj-a with AI context OFF" ...
		signed := w.signedArgs(a, "register_project", "proj-a", regFields(p, "default", false))
		// ... and the attacker flips it to ON, keeping signature, change and ID.
		flipped := copyArgs(signed, func(m map[string]any) { m["ai_context"] = true })
		before := w.snap()
		w.wantWire("register_project", flipped, false, "change_mismatch")
		// Or recomputes the change for the new content and keeps the old signature.
		h2, _ := registrymsg.ChangeHash("register_project", "proj-a", regFields(p, "default", true))
		recomputed := copyArgs(flipped, func(m map[string]any) { m["change"] = h2 })
		w.wantWire("register_project", recomputed, false, "bad_signature")
		// A valid signature of another project's registration, re-targeted.
		other := w.signedArgs(a, "register_project", "proj-b", regFields(w.repo("proj-b"), "default", false))
		retarget := copyArgs(other, func(m map[string]any) { m["id"] = "proj-a"; m["path"] = p })
		w.wantWire("register_project", retarget, false, "bad_signature") // the signed subject differs
		// The same for set_flag.
		w.wantWire("set_flag", copyArgs(w.signedArgs(a, "set_flag", "repo-a", flagFields("ai_context", false)),
			func(m map[string]any) { m["value"] = true }), false, "change_mismatch")
		w.mustEqual(before, "different change")
		// Control: exactly what alice signed (same request ID) is accepted.
		w.wantWire("register_project", signed, true, "")
		if pr, _ := w.find("proj-a"); pr.AIContext {
			t.Fatal("registered with the changed content")
		}
	}
}

func TestSSHNamespaceMismatchRefused(t *testing.T) {
	w := newWEnv(t)
	p := w.repo("proj-a")
	before := w.snap()
	for _, ns := range []string{signing.NamespaceApproval, signing.NamespaceLogin, "file", "git", "aigate-registry ", "AIGATE-REGISTRY", ""} {
		args := w.signedArgsWith(w.alice, "register_project", "proj-a", regFields(p, "default", true),
			registrymsg.NewRequestID(), w.now, ns)
		w.wantWire("register_project", args, false, "bad_signature")
		sf := w.signedArgsWith(w.alice, "set_flag", "repo-a", flagFields("ai_context", false),
			registrymsg.NewRequestID(), w.now, ns)
		w.wantWire("set_flag", sf, false, "bad_signature")
	}
	w.mustEqual(before, "wrong namespaces")
	w.wantWire("register_project", w.signedArgs(w.alice, "register_project", "proj-a", regFields(p, "default", true)), true, "")
}

func TestApprovalMessageCannotAuthorizeRegistryChange(t *testing.T) {
	approvalSig := func(who testSigner, m signing.Message) signing.Signature {
		if who.ssh != nil {
			return signing.Signature{Signer: who.id, Alg: "ssh-ed25519", Sig: sshsigArmored(t, who.ssh, signing.NamespaceApproval, []byte(m.Text()))}
		}
		return webSignDigest(t, who.web, who.id, m.Digest())
	}
	for _, mk := range []func() testSigner{
		func() testSigner { return newSSHTestSigner(t, "alice") },
		func() testSigner { return newWebTestSigner(t, "alice") },
	} {
		w := newWEnv(t)
		a := mk()
		w.writeSigners(a)
		p := w.repo("proj-a")
		before := w.snap()
		// Forward: a genuine, fresh approval signature (the kind a person gives to
		// approve a task) presented as the signature of a registry change.
		appr := approvalSig(a, approvalMessage(w.now))
		args := w.signedArgs(a, "register_project", "proj-a", regFields(p, "default", true))
		args["signature"] = appr
		w.wantWire("register_project", args, false, "bad_signature")
		sf := w.signedArgs(a, "set_flag", "repo-a", flagFields("ai_context", false))
		sf["signature"] = appr
		w.wantWire("set_flag", sf, false, "bad_signature")
		w.mustEqual(before, "approval signature as registry signature")

		// Reverse: a genuine registry signature is no approval.
		rm := signing.RegistryMessage{Action: "register_project", Subject: "proj-a",
			Change: strings.Repeat("d", 64), Request: registrymsg.NewRequestID(), Issued: w.now}
		var rsig signing.Signature
		if a.ssh != nil {
			rsig = a.sign(t, rm, signing.NamespaceRegistry)
		} else {
			rsig = a.sign(t, rm, "")
		}
		reg, err := signing.LoadRegistry(w.signers, signing.DefaultOptions)
		if err != nil {
			t.Fatal(err)
		}
		if err := signing.VerifyRegistry(reg, rm, rsig, signing.VerifyOptions{Now: w.now}); err != nil {
			t.Fatalf("control: registry signature does not verify: %v", err)
		}
		if err := signing.Verify(reg, approvalMessage(w.now), rsig, signing.VerifyOptions{Now: w.now}); err == nil {
			t.Fatal("a registry signature verified as an approval")
		}
		if err := signing.Verify(reg, approvalMessage(w.now), appr, signing.VerifyOptions{Now: w.now}); err != nil {
			t.Fatalf("control: approval signature does not verify: %v", err)
		}
		// Control: the honest registry request still works.
		w.wantWire("register_project", w.signedArgs(a, "register_project", "proj-a", regFields(p, "default", true)), true, "")
	}
}

func TestSignerRemovedTakesEffectImmediately(t *testing.T) {
	w := newWEnv(t)
	bob := newWebTestSigner(t, "bob")
	w.writeSigners(w.alice, bob)
	w.wantWire("register_project", w.signedArgs(bob, "register_project", "proj-a", regFields(w.repo("proj-a"), "default", false)), true, "")
	w.wantWire("set_flag", w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", true)), true, "")
	// bob leaves the list while the broker keeps running.
	w.writeSigners(w.alice)
	before := w.snap()
	pending := w.signedArgs(bob, "set_flag", "proj-a", flagFields("ai_context", false))
	w.wantWire("set_flag", pending, false, "unknown_signer")
	w.wantWire("register_project", w.signedArgs(bob, "register_project", "proj-b", regFields(w.repo("proj-b"), "default", false)), false, "unknown_signer")
	w.mustEqual(before, "removed signer")
	// alice is unaffected, and bob is back as soon as he is listed again.
	w.wantWire("register_project", w.signedArgs(w.alice, "register_project", "proj-b", regFields(w.repo("proj-b"), "default", false)), true, "")
	w.writeSigners(w.alice, bob)
	w.wantWire("set_flag", pending, true, "") // the refusal burned no ID
	// An emptied list refuses everybody.
	w.writeSigners()
	w.wantWire("set_flag", w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", true)), false, "unknown_signer")
}

func TestLegacyMigrationOnlyWhenRegistryAbsent(t *testing.T) {
	e := newTestEnv(t)
	state := filepath.Join(e.Dir, "state")
	repoA := filepath.Join(e.Dir, "repo-a")
	repoB := filepath.Join(e.Dir, "repo-b")
	start := func() *Server {
		t.Helper()
		s, err := New(e.load(t), Options{StateDir: state, KeyOwnerOK: func(uint32) bool { return true }})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	paths := func(s *Server) []string { return s.Registry.AIContextPaths() }
	// 1. absent -> migrated from the legacy entries.
	s := start()
	if got := paths(s); len(got) != 1 || got[0] != repoA {
		t.Fatalf("migration: %v", got)
	}
	first := readFile(t, filepath.Join(state, RegistryFileName))
	// 2. present -> new legacy entries are ignored, even after a restart.
	e.set(t, []string{"context_repos"}, []string{repoA, repoB})
	e.set(t, []string{"gitlab", "projects"}, map[string]any{repoA: "grp/proj", repoB: "grp/other"})
	s = start()
	if got := paths(s); len(got) != 1 || got[0] != repoA {
		t.Fatalf("legacy entries leaked in: %v", got)
	}
	if _, ok := s.Registry.GitLabProject(repoB); ok {
		t.Fatal("legacy gitlab mapping leaked in")
	}
	if !bytes.Equal(first, readFile(t, filepath.Join(state, RegistryFileName))) {
		t.Fatal("the registry was rewritten at start")
	}
	// 3. a project switched off in the registry stays off though legacy lists it.
	if err := s.Registry.Update(func(r *Registry) error { r.Projects[0].AIContext = false; return nil }); err != nil {
		t.Fatal(err)
	}
	s = start()
	if got := paths(s); len(got) != 0 {
		t.Fatalf("legacy config switched AI context back on: %v", got)
	}
	// 4. broken -> NOT migrated and not touched.
	os.WriteFile(filepath.Join(state, RegistryFileName), []byte("{broken"), 0o600)
	s = start()
	if s.Registry.Err() == nil || len(paths(s)) != 0 {
		t.Fatal("a broken registry was treated as absent")
	}
	if string(readFile(t, filepath.Join(state, RegistryFileName))) != "{broken" {
		t.Fatal("a broken registry was rewritten")
	}
	// 5. only when the file is really gone is the legacy config read again.
	os.Remove(filepath.Join(state, RegistryFileName))
	s = start()
	if got := paths(s); len(got) != 2 {
		t.Fatalf("second migration: %v", got)
	}
}

func TestBrokenRegistryDoesNotWidenAIContext(t *testing.T) {
	oa := newFakeHTTP(t, okHandler(goodPlan, 10, 10))
	for name, content := range map[string][]byte{
		"garbage":       []byte("not json"),
		"empty":         {},
		"unknown key":   []byte(`{"version":1,"providers":[],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5},"extra":1}`),
		"auto merge on": []byte(`{"version":1,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":true,"provider":null,"gitlab":null,"auto_merge":{"enabled":true}}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`),
	} {
		e := newTestEnv(t)
		// Legacy config lists repo-a: a fallback to it would widen the AI context.
		state := filepath.Join(e.Dir, "state")
		os.MkdirAll(state, 0o700)
		os.WriteFile(filepath.Join(state, RegistryFileName), content, 0o600)
		s, _ := e.newServer(t, func(s *Server) { s.OpenAI.BaseURL = oa.URL })
		if s.Registry.Err() == nil {
			t.Fatalf("%s: registry should be broken", name)
		}
		if got := s.Registry.AIContextPaths(); len(got) != 0 {
			t.Fatalf("%s: AI context paths %v", name, got)
		}
		sock := e.Raw["socket"].(string)
		repo := filepath.Join(e.Dir, "repo-a")
		if r, out := call(t, sock, `{"v":1,"op":"status"}`); r.OK || r.Code != "registry_unavailable" || strings.Contains(string(out), repo) {
			t.Fatalf("%s: status: %s", name, out)
		}
		if r, out := call(t, sock, `{"v":1,"op":"projects"}`); r.OK || r.Code != "registry_unavailable" {
			t.Fatalf("%s: projects: %s", name, out)
		}
		args := map[string]any{"repo": repo, "requirement": "x", "files": []map[string]string{{"path": "a.go", "content": "x"}}, "selection": "sel-abc123"}
		if r, out := call(t, sock, line(t, rpc("refine", args))); r.OK || r.Code != "registry_unavailable" {
			t.Fatalf("%s: refine: %s", name, out)
		}
		if got, _ := os.ReadFile(filepath.Join(state, RegistryFileName)); !bytes.Equal(got, content) {
			t.Fatalf("%s: the broken file was modified", name)
		}
	}
	if oa.Hits() != 0 {
		t.Fatalf("the AI endpoint was reached %d times with a broken registry", oa.Hits())
	}
}

func TestStatusAndProjectsNeverContainSecrets(t *testing.T) {
	w := newWEnv(t)
	// A project registered through the signed path, one with GitLab, one migrated.
	w.wantWire("register_project", w.register("proj-a", w.repo("proj-a")), true, "")
	w.wantWire("set_flag", w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", false)), true, "")
	secrets := []string{testOpenAIKey, testGitLabToken, "Bearer", "PRIVATE-TOKEN", "BEGIN", w.alice.line,
		w.e.KeyFile, w.e.TokFile, w.signers, "signature"}
	r1, out1 := call(t, w.sock, `{"v":1,"op":"status"}`)
	r2, out2 := call(t, w.sock, `{"v":1,"op":"projects"}`)
	if !r1.OK || !r2.OK {
		t.Fatalf("%s %s", out1, out2)
	}
	for _, out := range [][]byte{out1, out2} {
		for _, s := range secrets {
			if strings.Contains(string(out), s) {
				t.Fatalf("%q in a read reply: %s", s, out)
			}
		}
	}
	// The project objects have exactly the documented keys, nothing else.
	var res struct {
		Projects []map[string]json.RawMessage `json:"projects"`
		Provs    []any                        `json:"providers"`
		Global   map[string]json.RawMessage   `json:"auto_merge_global"`
	}
	if err := json.Unmarshal(r2.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Projects) < 2 {
		t.Fatalf("projects: %s", r2.Result)
	}
	for _, p := range res.Projects {
		keys := []string{}
		for k := range p {
			keys = append(keys, k)
		}
		if len(keys) != 7 {
			t.Fatalf("project keys %v", keys)
		}
		for _, k := range []string{"id", "path", "kind", "ai_context", "provider", "remote", "auto_merge"} {
			if _, ok := p[k]; !ok {
				t.Fatalf("project lacks %s: %v", k, keys)
			}
		}
		if g := string(p["remote"]); g != "null" {
			var gm map[string]json.RawMessage
			if json.Unmarshal(p["remote"], &gm) != nil || len(gm) != 2 {
				t.Fatalf("remote object %s", g)
			}
		}
	}
	// The failure replies and the log carry no secret either.
	for _, a := range []map[string]any{
		w.signedArgs(w.alice, "register_project", "proj-x", regFields(w.repo("proj-x"), "default", true)),
	} {
		bad := copyArgs(a, func(m map[string]any) { delete(m, "signature") })
		_, out := call(t, w.sock, line(t, rpc("register_project", bad)))
		for _, s := range secrets[:4] {
			if strings.Contains(string(out), s) {
				t.Fatalf("%q in an error reply: %s", s, out)
			}
		}
	}
	for _, s := range secrets[:5] {
		if strings.Contains(w.logs.String(), s) {
			t.Fatalf("%q in the log", s)
		}
	}
	if !strings.Contains(w.logs.String(), "op=projects ok") {
		t.Fatal("the log scan saw no op lines")
	}
}

func uidOf(t *testing.T, fi os.FileInfo) uint32 {
	t.Helper()
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no stat")
	}
	return st.Uid
}

// linkInstead replaces the file at p with a symlink to a copy of it.
func linkInstead(t *testing.T, p string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	target := p + ".real"
	if err := os.WriteFile(target, b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func TestWriteOpContractChangeRequiredIDKeptAndExactKeys(t *testing.T) {
	// A3 contract over the wire: `change` is required, keys are exact-case and
	// unique, and none of these refusals spends the request ID.
	w := newWEnv(t)
	p := w.repo("proj-a")
	good := w.signedArgs(w.alice, "register_project", "proj-a", regFields(p, "default", true))
	before := w.snap()
	w.wantWire("register_project", copyArgs(good, func(a map[string]any) { delete(a, "change") }), false, "change_mismatch")
	w.wantWire("register_project", copyArgs(good, func(a map[string]any) { a["change"] = "" }), false, "change_mismatch")
	for _, k := range []string{"path", "kind", "ai_context", "change", "request", "issued", "signature", "id"} {
		up := copyArgs(good, func(a map[string]any) {
			a[strings.ToUpper(k[:1])+k[1:]] = a[k]
			delete(a, k)
		})
		w.wantWire("register_project", up, false, "bad_request")
	}
	raw, _ := json.Marshal(good)
	dup := strings.Replace(string(raw), `{`, `{"path":"/elsewhere",`, 1)
	if r, out := call(t, w.sock, `{"v":1,"op":"register_project","args":`+dup+`}`); r.OK || r.Code != "bad_request" {
		t.Fatalf("duplicate key: %s", out)
	}
	sigText, _ := json.Marshal(good["signature"])
	dupSig := strings.Replace(string(raw), string(sigText), strings.Replace(string(sigText), `{`, `{"signer":"mallory",`, 1), 1)
	if r, out := call(t, w.sock, `{"v":1,"op":"register_project","args":`+dupSig+`}`); r.OK || r.Code != "bad_request" {
		t.Fatalf("duplicate key in the signature: %s", out)
	}
	w.mustEqual(before, "contract refusals")
	w.wantWire("register_project", good, true, "") // the same request ID, still unspent
}
