package approval

// Adversarial suite (Task 26, E1): an attacker who controls the approvals file
// (the orch user) or the signers registry layout must not be able to make an
// approval count. Every case asserts a refusal; each test also keeps a control
// that is accepted, so a test cannot pass by refusing everything.
//
// None of these tests may use t.Parallel: they change the process-wide
// registry path and options.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aigate/internal/approval/signing"
)

// signNS signs text with s's key in namespace ns (ssh-keygen -Y sign).
func (s sshSigner) signNS(t *testing.T, ns, text string) string {
	t.Helper()
	cmd := exec.Command(sshKeygen(t), "-Y", "sign", "-f", s.key, "-n", ns)
	cmd.Stdin = strings.NewReader(text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssh-keygen -Y sign -n %s: %v: %s", ns, err, stderr.String())
	}
	return stdout.String()
}

// sshEntry is a raw approvals-file entry for triple (repo, base, hash)
// carrying message and an ssh signature by signer.
func sshEntry(repo, base, hash, message, signer, sig string) map[string]any {
	return map[string]any{"repo": repo, "base": base, "hash": hash, "granted_at": "2026-11-09T12:00:00Z",
		"message": message, "signer": signer, "alg": signing.TypeSSHEd25519, "sig": sig}
}

// appendRaw adds entries to the approvals file as the orch user could.
func appendRaw(t *testing.T, s Store, entries ...map[string]any) {
	t.Helper()
	f := readRaw(t, s)
	f.Approvals = append(f.Approvals, entries...)
	writeRaw(t, s, f)
}

// setRaw replaces the approvals with entries.
func setRaw(t *testing.T, s Store, entries ...map[string]any) {
	t.Helper()
	writeRaw(t, s, rawFile{Version: 1, Approvals: entries})
}

// assertClosed checks the fail-closed state: no approval counts and no grant
// of either kind is accepted, the registry is reported unusable (signed mode,
// never legacy), and the approvals file is untouched.
func assertClosed(t *testing.T, s Store, alice sshSigner, what string) {
	t.Helper()
	before, _ := os.ReadFile(s.Path)
	if s.Valid(tRepo, tBase, tHashA) {
		t.Fatalf("%s: an approval counted", what)
	}
	if signed, err := SignedMode(); !signed || !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("%s: SignedMode = %v, %v (want signed, ErrRegistryUnavailable)", what, signed, err)
	}
	if err := s.Grant(tRepo, tBase, tHashB, tNow); err == nil {
		t.Fatalf("%s: legacy Grant accepted", what)
	}
	msgB := msgFor(tRepo, tBase, tHashB)
	if err := s.GrantSigned(msgB, alice.sign(t, msgB), tNow, verifyOpts); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("%s: GrantSigned = %v", what, err)
	}
	if after, _ := os.ReadFile(s.Path); !bytes.Equal(before, after) {
		t.Fatalf("%s: approvals file changed", what)
	}
}

// Review point 2: in signed mode, entries written straight into
// approvals.json by someone without a registered key never count.
func TestForgedApprovalsFileIgnored(t *testing.T) {
	s, alice, _, sigA := signedSetup(t)
	mallory := newSSHSigner(t, "mallory") // a real key, not in the registry
	phone := newFakeAuthenticator(t)      // a real authenticator, not in the registry
	msgB := msgFor(tRepo, tBase, tHashB)
	textB := msgB.Text()
	cases := map[string]map[string]any{
		"plain triple": {"repo": tRepo, "base": tBase, "hash": tHashB, "granted_at": "2026-11-09T12:00:00Z"},
		"garbage sig":  sshEntry(tRepo, tBase, tHashB, textB, "alice", "garbage"),
		"armored garbage sig": sshEntry(tRepo, tBase, tHashB, textB, "alice",
			"-----BEGIN SSH SIGNATURE-----\nU1NIU0lHAAAAAQ==\n-----END SSH SIGNATURE-----\n"),
		"signature for another triple": sshEntry(tRepo, tBase, tHashB, textB, "alice", sigA.Sig),
		"unregistered key under its own id": sshEntry(tRepo, tBase, tHashB, textB, "mallory",
			mallory.signNS(t, signing.NamespaceApproval, textB)),
		"unregistered key claiming a registered id": sshEntry(tRepo, tBase, tHashB, textB, "alice",
			mallory.signNS(t, signing.NamespaceApproval, textB)),
		"wrong namespace (login)": sshEntry(tRepo, tBase, tHashB, textB, "alice",
			alice.signNS(t, signing.NamespaceLogin, textB)),
		"wrong namespace (file)": sshEntry(tRepo, tBase, tHashB, textB, "alice", alice.signNS(t, "file", textB)),
		"alg not the signer's type": func() map[string]any {
			e := sshEntry(tRepo, tBase, tHashB, textB, "alice", alice.sign(t, msgB).Sig)
			e["alg"] = signing.TypeWebAuthnES256
			return e
		}(),
		"unregistered authenticator claiming a registered id": func() map[string]any {
			sig := phone.assert(t, "alice", msgB)
			return map[string]any{"repo": tRepo, "base": tBase, "hash": tHashB, "granted_at": "x",
				"message": textB, "signer": "alice", "alg": sig.Alg, "sig": sig.Sig,
				"webauthn": map[string]any{"authenticator_data": sig.WebAuthn.AuthenticatorData, "client_data_json": sig.WebAuthn.ClientDataJSON}}
		}(),
		"empty signer": sshEntry(tRepo, tBase, tHashB, textB, "", alice.sign(t, msgB).Sig),
	}
	// Control: alice's real signature for B in the same shape is accepted, so
	// the refusals below are about the forgery, not the entry layout.
	good := sshEntry(tRepo, tBase, tHashB, textB, "alice", alice.sign(t, msgB).Sig)
	valid := readRaw(t, s).Approvals[0]
	setRaw(t, s, valid, good)
	if !s.Valid(tRepo, tBase, tHashB) {
		t.Fatal("control: a genuinely signed hand-written entry must count")
	}
	for name, forged := range cases {
		t.Run(name, func(t *testing.T) {
			setRaw(t, s, valid, forged)
			if s.Valid(tRepo, tBase, tHashB) {
				t.Fatal("forged entry counted")
			}
			if !s.Valid(tRepo, tBase, tHashA) {
				t.Fatal("the genuine entry must still count")
			}
			// Many forged copies do not add up to an approval either.
			setRaw(t, s, valid, forged, forged, forged)
			if s.Valid(tRepo, tBase, tHashB) {
				t.Fatal("repeated forged entries counted")
			}
		})
	}
}

// Each line of the stored message, and each field of the plain triple, is
// covered: changing any of them invalidates the entry for every triple.
func TestEditedMessageOrHashRejected(t *testing.T) {
	s, _, msg, _ := signedSetup(t)
	orig := readRaw(t, s).Approvals[0]
	otherRepo, otherBase := "ba9876543210", strings.Repeat("d", 40)
	line := func(key, from, to string) string {
		return strings.Replace(msg.Text(), key+": "+from+"\n", key+": "+to+"\n", 1)
	}
	issued := msg.Issued.Format("2006-01-02T15:04:05Z")
	messageEdits := map[string]struct {
		text             string
		repo, base, hash string // the triple the edit points at
	}{
		"kind":       {line("kind", msg.Kind, signing.KindPublishOutsideAllowlist), tRepo, tBase, tHashA},
		"repo":       {line("repo", tRepo, otherRepo), otherRepo, tBase, tHashA},
		"task":       {line("task", "7", "8"), tRepo, tBase, tHashA},
		"base":       {line("base", tBase, otherBase), tRepo, otherBase, tHashA},
		"hash":       {line("hash", tHashA, tHashB), tRepo, tBase, tHashB},
		"issued":     {line("issued", issued, "2026-11-09T12:00:01Z"), tRepo, tBase, tHashA},
		"magic":      {strings.Replace(msg.Text(), "aigate-approval-v1", "aigate-approval-v2", 1), tRepo, tBase, tHashA},
		"extra line": {msg.Text() + "note: x\n", tRepo, tBase, tHashA},
		"crlf":       {strings.ReplaceAll(msg.Text(), "\n", "\r\n"), tRepo, tBase, tHashA},
	}
	for name, c := range messageEdits {
		t.Run("message "+name, func(t *testing.T) {
			// Only the message changed.
			e := clone(orig)
			e["message"] = c.text
			setRaw(t, s, e)
			if s.Valid(tRepo, tBase, tHashA) || s.Valid(c.repo, c.base, c.hash) {
				t.Fatal("entry with an edited message counted")
			}
			// The message and the plain triple re-pointed together.
			e["repo"], e["base"], e["hash"] = c.repo, c.base, c.hash
			setRaw(t, s, e)
			if s.Valid(c.repo, c.base, c.hash) || s.Valid(tRepo, tBase, tHashA) {
				t.Fatal("re-pointed entry counted")
			}
		})
	}
	// The plain triple edited, the signed message untouched: the entry no
	// longer matches its own message (this is the message-vs-query hash check).
	for key, val := range map[string]string{"repo": otherRepo, "base": otherBase, "hash": tHashB} {
		t.Run("plain "+key, func(t *testing.T) {
			e := clone(orig)
			e[key] = val
			setRaw(t, s, e)
			q := map[string]string{"repo": tRepo, "base": tBase, "hash": tHashA}
			q[key] = val
			if s.Valid(q["repo"], q["base"], q["hash"]) || s.Valid(tRepo, tBase, tHashA) {
				t.Fatalf("entry with edited plain %s counted", key)
			}
		})
	}
	setRaw(t, s, orig)
	if !s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("control: the original entry must count again")
	}
}

func clone(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Review point 1: a valid signed entry for item A, copied under another
// item's triple (other hash, other repo, other base), approves nothing new.
func TestReplayAcrossItemsRejected(t *testing.T) {
	s, alice, msgA, sigA := signedSetup(t)
	orig := readRaw(t, s).Approvals[0]
	targets := map[string][3]string{
		"other hash": {tRepo, tBase, tHashB},
		"other repo": {"ba9876543210", tBase, tHashA},
		"other base": {tRepo, strings.Repeat("d", 40), tHashA},
		"all three":  {"ba9876543210", strings.Repeat("d", 64), strings.Repeat("e", 64)},
	}
	for name, tg := range targets {
		t.Run(name, func(t *testing.T) {
			msgT := msgFor(tg[0], tg[1], tg[2])
			for variant, message := range map[string]string{
				"A's message kept": msgA.Text(),
				"target's message": msgT.Text(),
			} {
				replay := sshEntry(tg[0], tg[1], tg[2], message, sigA.Signer, sigA.Sig)
				setRaw(t, s, orig, replay)
				if s.Valid(tg[0], tg[1], tg[2]) {
					t.Fatalf("%s: A's signature approved %v", variant, tg)
				}
				if !s.Valid(tRepo, tBase, tHashA) {
					t.Fatalf("%s: A must stay valid", variant)
				}
			}
			if err := s.GrantSigned(msgT, sigA, tNow, verifyOpts); !errors.Is(err, signing.ErrBadSignature) {
				t.Fatalf("GrantSigned with A's signature: %v", err)
			}
			if s.Valid(tg[0], tg[1], tg[2]) {
				t.Fatal("refused grant left an approval")
			}
		})
	}
	// The same holds for a WebAuthn assertion: its challenge is A's digest.
	phone := newFakeAuthenticator(t)
	useRegistry(t, []signing.Signer{alice.signer, phone.signer(t, "phone")})
	w := newStore(t)
	if err := w.GrantSigned(msgA, phone.assert(t, "phone", msgA), tNow, verifyOpts); err != nil {
		t.Fatal(err)
	}
	wa := readRaw(t, w).Approvals[0]
	msgB := msgFor(tRepo, tBase, tHashB)
	replay := clone(wa)
	replay["hash"], replay["message"] = tHashB, msgB.Text()
	setRaw(t, w, wa, replay)
	if w.Valid(tRepo, tBase, tHashB) || !w.Valid(tRepo, tBase, tHashA) {
		t.Fatal("webauthn assertion for A approved B (or A lost)")
	}
	if err := w.GrantSigned(msgB, phone.assert(t, "phone", msgA), tNow, verifyOpts); !errors.Is(err, signing.ErrBadSignature) {
		t.Fatalf("GrantSigned with A's assertion: %v", err)
	}
}

// Review point 3: a registry in a directory the orch user owns could be
// deleted or swapped by orch, so it is not trusted.
//
// Limitation: run as one (non-root) test user, the directory and the file
// cannot have different owners. The production rule (owner uid 0 only) is
// exercised with everything owned by the test user, and "orch-writable" is
// also exercised through group/other write bits on the directory.
func TestRegistryInOrchOwnedDirRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root the temp directory is root-owned")
	}
	alice := newSSHSigner(t, "alice")
	t.Run("production owner rule, dir owned by the test user", func(t *testing.T) {
		useRegistry(t, []signing.Signer{alice.signer})
		s := newStore(t)
		signing.DefaultOptions = signing.RegistryOptions{} // uid 0 only, as in production
		assertClosed(t, s, alice, "registry file present")
		// orch deleting the file from its own directory must not switch
		// signing off.
		if err := os.Remove(signing.RegistryPath()); err != nil {
			t.Fatal(err)
		}
		assertClosed(t, s, alice, "registry file deleted")
	})
	for _, mode := range []os.FileMode{0o775, 0o757, 0o777, 0o1777} {
		t.Run(fmt.Sprintf("dir writable by others %04o", mode), func(t *testing.T) {
			s, _, _, _ := signedSetup(t)
			dir := filepath.Dir(signing.RegistryPath())
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o755) })
			assertClosedKeepsEntry(t, s, alice)
			if err := os.Remove(signing.RegistryPath()); err != nil {
				t.Fatal(err)
			}
			assertClosedKeepsEntry(t, s, alice)
		})
	}
	// Control: the same layout with the trusted owner and mode is signed mode.
	s, _, _, _ := signedSetup(t)
	if signed, err := SignedMode(); !signed || err != nil || !s.Valid(tRepo, tBase, tHashA) {
		t.Fatalf("control: %v %v", signed, err)
	}
}

// assertClosedKeepsEntry is assertClosed for a store that holds a genuine
// signed entry for tHashA.
func assertClosedKeepsEntry(t *testing.T, s Store, alice sshSigner) {
	t.Helper()
	assertClosed(t, s, alice, "untrusted registry directory")
}

func TestRegistryMode0666Refused(t *testing.T) {
	for _, mode := range []os.FileMode{0o666, 0o664, 0o646, 0o622, 0o660, 0o606} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			s, alice, _, _ := signedSetup(t)
			if err := os.Chmod(signing.RegistryPath(), mode); err != nil {
				t.Fatal(err)
			}
			assertClosed(t, s, alice, "loose registry file")
		})
	}
	for _, mode := range []os.FileMode{0o644, 0o600, 0o444, 0o400} {
		t.Run(fmt.Sprintf("control %04o", mode), func(t *testing.T) {
			s, _, _, _ := signedSetup(t)
			if err := os.Chmod(signing.RegistryPath(), mode); err != nil {
				t.Fatal(err)
			}
			if !s.Valid(tRepo, tBase, tHashA) {
				t.Fatalf("mode %04o must be accepted", mode)
			}
		})
	}
}

// Absence is the only state that means legacy mode. Every broken state of a
// registry that exists (or whose absence cannot be trusted) fails closed.
func TestRegistryDeletedFallsBackOnlyWhenAbsentNotWhenBroken(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	goodJSON, err := json.Marshal(map[string]any{"version": 1, "signers": []signing.Signer{alice.signer}})
	if err != nil {
		t.Fatal(err)
	}
	// legacyStore holds an unsigned (legacy) grant for tHashA.
	legacyStore := func(t *testing.T) Store {
		s := newStore(t)
		setRaw(t, s, map[string]any{"repo": tRepo, "base": tBase, "hash": tHashA, "granted_at": "2026-11-04T01:02:03Z"})
		return s
	}
	t.Run("absent in a trusted directory: legacy", func(t *testing.T) {
		useRegistry(t, nil)
		s := legacyStore(t)
		if signed, err := SignedMode(); signed || err != nil {
			t.Fatalf("SignedMode %v %v", signed, err)
		}
		if !s.Valid(tRepo, tBase, tHashA) {
			t.Fatal("legacy grant must count")
		}
		if err := s.Grant(tRepo, tBase, tHashB, tNow); err != nil || !s.Valid(tRepo, tBase, tHashB) {
			t.Fatalf("legacy Grant: %v", err)
		}
	})
	t.Run("absent with missing parents under a trusted ancestor: legacy", func(t *testing.T) {
		p := useRegistry(t, nil)
		signing.SetRegistryPath(filepath.Join(filepath.Dir(p), "missing", "deeper", "signers.json"))
		s := legacyStore(t)
		if signed, err := SignedMode(); signed || err != nil || !s.Valid(tRepo, tBase, tHashA) {
			t.Fatalf("SignedMode %v %v", signed, err)
		}
	})

	broken := map[string]func(t *testing.T, p string){
		"corrupt":   func(t *testing.T, p string) { mustWrite(t, p, []byte("{")) },
		"empty":     func(t *testing.T, p string) { mustWrite(t, p, nil) },
		"truncated": func(t *testing.T, p string) { mustWrite(t, p, goodJSON[:len(goodJSON)/2]) },
		"trailing":  func(t *testing.T, p string) { mustWrite(t, p, append(append([]byte{}, goodJSON...), "{}"...)) },
		"wrong version": func(t *testing.T, p string) {
			mustWrite(t, p, bytes.Replace(goodJSON, []byte(`"version":1`), []byte(`"version":2`), 1))
		},
		"invalid signer": func(t *testing.T, p string) {
			mustWrite(t, p, bytes.Replace(goodJSON, []byte(`"ssh-ed25519 `), []byte(`"ssh-ed25519 X`), 1))
		},
		"wrong owner": func(t *testing.T, p string) {
			signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uint32) bool { return false }}
		},
		"unreadable": func(t *testing.T, p string) {
			if os.Getuid() == 0 {
				t.Skip("root reads mode 0000 files")
			}
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
		},
		"directory instead of file": func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to a valid registry": func(t *testing.T, p string) {
			real := filepath.Join(filepath.Dir(p), "real.json")
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(filepath.Dir(p), "nowhere.json"), p); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
		},
		"absent in an untrusted directory": func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Dir(p), 0o777); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(filepath.Dir(p), 0o755) })
		},
		"absent with an untrusted directory owner": func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uint32) bool { return false }}
		},
		"absent under an untrusted nearest ancestor": func(t *testing.T, p string) {
			dir := filepath.Dir(p)
			if err := os.Chmod(dir, 0o777); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o755) })
			signing.SetRegistryPath(filepath.Join(dir, "missing", "signers.json"))
		},
		"parent is a symlink": func(t *testing.T, p string) {
			dir := filepath.Dir(p)
			link := filepath.Join(filepath.Dir(dir), "etc-link")
			if err := os.Symlink(dir, link); err != nil {
				t.Fatal(err)
			}
			signing.SetRegistryPath(filepath.Join(link, "signers.json"))
		},
	}
	for name, breakIt := range broken {
		t.Run(name, func(t *testing.T) {
			p := useRegistry(t, []signing.Signer{alice.signer})
			s := legacyStore(t)
			// A genuine signed entry too: neither kind may count.
			msgC := msgFor(tRepo, tBase, strings.Repeat("f", 16))
			if err := s.GrantSigned(msgC, alice.sign(t, msgC), tNow, verifyOpts); err != nil {
				t.Fatal(err)
			}
			breakIt(t, p)
			if s.Valid(tRepo, tBase, strings.Repeat("f", 16)) {
				t.Fatal("signed entry counted with a broken registry")
			}
			assertClosed(t, s, alice, name) // checks the legacy entry (tHashA) and both grants
		})
	}
}

func mustWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// GrantSigned enforces the ±5 minute issued window (both directions);
// approvals already stored do not expire.
func TestStaleIssuedRejected(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	phone := newFakeAuthenticator(t)
	useRegistry(t, []signing.Signer{alice.signer, phone.signer(t, "phone")})
	s := newStore(t)
	msg := msgFor(tRepo, tBase, tHashA)
	sig := alice.sign(t, msg)
	for name, now := range map[string]time.Time{
		"10 minutes later":   tNow.Add(10 * time.Minute),
		"5m01s later":        tNow.Add(5*time.Minute + time.Second),
		"10 minutes earlier": tNow.Add(-10 * time.Minute),
		"5m01s earlier":      tNow.Add(-5*time.Minute - time.Second),
		"a year later":       tNow.AddDate(1, 0, 0),
	} {
		if err := s.GrantSigned(msg, sig, now, verifyOpts); !errors.Is(err, signing.ErrStale) {
			t.Fatalf("%s: %v", name, err)
		}
		// Callers cannot opt out of freshness.
		if err := s.GrantSigned(msg, sig, now, signing.VerifyOptions{SkipFreshness: true, Now: tNow}); !errors.Is(err, signing.ErrStale) {
			t.Fatalf("%s with SkipFreshness: %v", name, err)
		}
		wmsg := msgFor(tRepo, tBase, tHashB)
		if err := s.GrantSigned(wmsg, phone.assert(t, "phone", wmsg), now, verifyOpts); !errors.Is(err, signing.ErrStale) {
			t.Fatalf("%s (webauthn): %v", name, err)
		}
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatal("stale grants wrote the approvals file")
	}
	// The window's edges are inside it.
	for _, now := range []time.Time{tNow.Add(5 * time.Minute), tNow.Add(-5 * time.Minute)} {
		if err := s.GrantSigned(msg, sig, now, verifyOpts); err != nil {
			t.Fatalf("edge %v: %v", now.Sub(tNow), err)
		}
	}
	// A stored entry whose message was issued years ago (or in the future)
	// still counts: approvals do not expire once written.
	for _, issued := range []time.Time{time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)} {
		m := msgFor(tRepo, tBase, strings.Repeat("9", 16))
		m.Issued = issued
		appendRaw(t, s, sshEntry(tRepo, tBase, m.Hash, m.Text(), "alice", alice.sign(t, m).Sig))
		if !s.Valid(tRepo, tBase, m.Hash) {
			t.Fatalf("stored entry issued %v must count", issued)
		}
		f := readRaw(t, s)
		f.Approvals = f.Approvals[:len(f.Approvals)-1]
		writeRaw(t, s, f)
	}
}

// Once the registry exists, unsigned entries (written earlier in legacy mode
// or added by hand) stop counting, and no new unsigned grant is accepted.
func TestUnsignedLegacyEntryAfterSignedModeRejected(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	p := useRegistry(t, nil)
	s := newStore(t)
	if err := s.Grant(tRepo, tBase, tHashA, t0); err != nil {
		t.Fatal(err)
	}
	if !s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("legacy grant must count in legacy mode")
	}
	writeRegistry(t, p, []signing.Signer{alice.signer})
	if s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("legacy entry counted in signed mode")
	}
	if err := s.Grant(tRepo, tBase, tHashB, tNow); !errors.Is(err, ErrSignatureRequired) {
		t.Fatalf("Grant in signed mode: %v", err)
	}
	// Partially signed entries are unsigned too.
	msgB := msgFor(tRepo, tBase, tHashB)
	sigB := alice.sign(t, msgB).Sig
	full := sshEntry(tRepo, tBase, tHashB, msgB.Text(), "alice", sigB)
	for _, drop := range []string{"message", "signer", "alg", "sig"} {
		e := clone(full)
		delete(e, drop)
		setRaw(t, s, map[string]any{"repo": tRepo, "base": tBase, "hash": tHashA, "granted_at": "x"}, e)
		if s.Valid(tRepo, tBase, tHashB) || s.Valid(tRepo, tBase, tHashA) {
			t.Fatalf("entry without %s counted", drop)
		}
	}
	// Upgrading the legacy entry with a real signature makes it count, in
	// place (one entry for the triple).
	setRaw(t, s, map[string]any{"repo": tRepo, "base": tBase, "hash": tHashA, "granted_at": "x"})
	msgA := msgFor(tRepo, tBase, tHashA)
	if err := s.GrantSigned(msgA, alice.sign(t, msgA), tNow, verifyOpts); err != nil {
		t.Fatal(err)
	}
	if f := readRaw(t, s); len(f.Approvals) != 1 || !s.Valid(tRepo, tBase, tHashA) {
		t.Fatalf("upgrade: %+v", f.Approvals)
	}
}

// Many concurrent grants of the same triple (each with its own valid
// signature) leave exactly one entry; concurrent grants of distinct triples
// all land.
func TestConcurrentGrantsOneEntry(t *testing.T) {
	phone := newFakeAuthenticator(t)
	useRegistry(t, []signing.Signer{phone.signer(t, "phone")})
	s := newStore(t)
	const same, distinct = 16, 16
	type job struct {
		msg signing.Message
		sig signing.Signature
	}
	var jobs []job
	for k := 0; k < same; k++ {
		m := msgFor(tRepo, tBase, tHashA)
		m.Issued = tNow.Add(time.Duration(k) * time.Second)
		jobs = append(jobs, job{m, phone.assert(t, "phone", m)})
	}
	for i := 0; i < distinct; i++ {
		m := msgFor(tRepo, tBase, fmt.Sprintf("%016x", 1000+i))
		jobs = append(jobs, job{m, phone.assert(t, "phone", m)})
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, len(jobs))
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			<-start
			errs <- s.GrantSigned(j.msg, j.sig, tNow, verifyOpts)
		}(j)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f := readRaw(t, s)
	count := map[string]int{}
	for _, e := range f.Approvals {
		count[e["hash"].(string)]++
	}
	if count[tHashA] != 1 {
		t.Fatalf("same triple stored %d times", count[tHashA])
	}
	if len(f.Approvals) != 1+distinct {
		t.Fatalf("want %d entries, got %d", 1+distinct, len(f.Approvals))
	}
	for i := 0; i < distinct; i++ {
		h := fmt.Sprintf("%016x", 1000+i)
		if count[h] != 1 || !s.Valid(tRepo, tBase, h) {
			t.Fatalf("triple %s: %d entries", h, count[h])
		}
	}
	if !s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("same triple not valid")
	}
}

// SSH signatures in the login namespace never verify as approvals and vice
// versa, over identical bytes.
func TestLoginVsApprovalNamespacesDisjoint(t *testing.T) {
	if signing.NamespaceLogin == signing.NamespaceApproval {
		t.Fatalf("login and approval namespaces are both %q", signing.NamespaceLogin)
	}
	alice := newSSHSigner(t, "alice")
	useRegistry(t, []signing.Signer{alice.signer})
	reg := signing.Registry{Signers: []signing.Signer{alice.signer}}
	s := newStore(t)
	msg := msgFor(tRepo, tBase, tHashA)
	text := msg.Text()
	loginText := "aigate-login-v1\nsigner: alice\nnonce: " + strings.Repeat("ab", 32) + "\nissued: 2026-11-09T12:00:00Z\n"

	// 1. A login signature over the approval message's exact bytes.
	loginSig := alice.signNS(t, signing.NamespaceLogin, text)
	if err := signing.Verify(reg, msg, signing.Signature{Signer: "alice", Alg: signing.TypeSSHEd25519, Sig: loginSig},
		signing.VerifyOptions{Now: tNow}); !errors.Is(err, signing.ErrBadSignature) {
		t.Fatalf("login signature verified as approval: %v", err)
	}
	if err := s.GrantSigned(msg, signing.Signature{Signer: "alice", Alg: signing.TypeSSHEd25519, Sig: loginSig}, tNow, verifyOpts); !errors.Is(err, signing.ErrBadSignature) {
		t.Fatalf("GrantSigned with a login signature: %v", err)
	}
	setRaw(t, s, sshEntry(tRepo, tBase, tHashA, text, "alice", loginSig))
	if s.Valid(tRepo, tBase, tHashA) {
		t.Fatal("stored login signature counted as approval")
	}
	// It is a genuine login signature (control).
	if err := signing.VerifySSHSig(alice.signer.PublicKey, signing.NamespaceLogin, []byte(text), loginSig); err != nil {
		t.Fatalf("control: login signature must verify as login: %v", err)
	}

	// 2. An approval signature never verifies as a login, over the same bytes
	// (both the approval message and a login message).
	for name, b := range map[string]string{"approval message": text, "login message": loginText} {
		approvalSig := alice.signNS(t, signing.NamespaceApproval, b)
		if err := signing.VerifySSHSig(alice.signer.PublicKey, signing.NamespaceLogin, []byte(b), approvalSig); err == nil {
			t.Fatalf("%s: approval signature verified as login", name)
		}
		if err := signing.VerifySSHSig(alice.signer.PublicKey, signing.NamespaceApproval, []byte(b), approvalSig); err != nil {
			t.Fatalf("%s: control: %v", name, err)
		}
	}
	// 3. Control: the approval-namespace signature is accepted as approval.
	if err := s.GrantSigned(msg, alice.sign(t, msg), tNow, verifyOpts); err != nil || !s.Valid(tRepo, tBase, tHashA) {
		t.Fatalf("control approval: %v", err)
	}
}
