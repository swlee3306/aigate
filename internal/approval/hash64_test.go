package approval

import (
	"errors"
	"os"
	"testing"

	"aigate/internal/approval/signing"
)

// Since 2026-11-09 the diff hash is the full SHA-256 (64 hex). Entries that
// older builds wrote carry the 16-hex prefix. There is no migration: such an
// entry must load without error and never validate the new full hash (fail
// closed; the human approves again).

const (
	tFull  = "8082594eac4a51b773457d7e4c3597e97f8d8697a34585ff69d8acc085c69611"
	tShort = "8082594eac4a51b7" // tFull[:16], what an old build stored
)

// oldBuildFile writes an approvals.json as a pre-2026-11-09 build would have:
// a legacy entry and a signed entry, both with the 16-hex hash (the signed
// entry's message also says the short hash, and its signature is whatever the
// old build stored; it can never verify under the new grammar).
func oldBuildFile(t *testing.T, s Store) {
	t.Helper()
	oldMsg := "aigate-approval-v1\nkind: protected_modified\nrepo: " + tRepo + "\ntask: 7\nbase: " + tBase +
		"\nhash: " + tShort + "\nissued: 2026-11-08T12:00:00Z\n"
	writeRaw(t, s, rawFile{Version: 1, Approvals: []map[string]any{
		{"repo": tRepo, "base": tBase, "hash": tShort, "granted_at": "2026-11-08T12:00:00Z"},
		{"repo": tRepo, "base": tBase, "hash": tShort, "granted_at": "2026-11-08T12:00:01Z",
			"message": oldMsg, "signer": "alice", "alg": signing.TypeSSHEd25519, "sig": "c2ln"},
	}})
}

func TestOldShortHashEntryNeverValidatesLegacyMode(t *testing.T) {
	useRegistry(t, nil) // legacy mode
	s := newStore(t)
	oldBuildFile(t, s)
	if _, err := s.load(); err != nil {
		t.Fatalf("a file with 16-hex entries must load: %v", err)
	}
	if s.Valid(tRepo, tBase, tFull) {
		t.Fatal("a 16-hex legacy entry must not validate the full 64-hex hash")
	}
	// Re-approving with the new hash works and keeps the old entries intact.
	if err := s.Grant(tRepo, tBase, tFull, t0); err != nil {
		t.Fatal(err)
	}
	if !s.Valid(tRepo, tBase, tFull) {
		t.Fatal("re-approval with the full hash must validate")
	}
	if n := len(readRaw(t, s).Approvals); n != 3 {
		t.Fatalf("want the two old entries plus the new one, got %d", n)
	}
}

func TestOldShortHashEntryNeverValidatesSignedMode(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	useRegistry(t, []signing.Signer{alice.signer})
	s := newStore(t)
	oldBuildFile(t, s)
	for _, h := range []string{tFull, tShort} {
		if s.Valid(tRepo, tBase, h) {
			t.Fatalf("an old 16-hex entry validated %q in signed mode", h)
		}
	}
	msg := msgFor(tRepo, tBase, tFull)
	if err := s.GrantSigned(msg, alice.sign(t, msg), tNow, verifyOpts); err != nil {
		t.Fatal(err)
	}
	if !s.Valid(tRepo, tBase, tFull) {
		t.Fatal("a signed re-approval with the full hash must validate")
	}
}

// TestGrantSignedRefusesShortHash: a signed grant whose message carries a
// 16-hex hash is refused before any verification and writes nothing.
func TestGrantSignedRefusesShortHash(t *testing.T) {
	alice := newSSHSigner(t, "alice")
	useRegistry(t, []signing.Signer{alice.signer})
	s := newStore(t)
	good := msgFor(tRepo, tBase, tFull)
	sig := alice.sign(t, good)
	for _, kind := range []string{signing.KindProtectedModified, signing.KindPublishOutsideAllowlist} {
		short := msgFor(tRepo, tBase, tShort)
		short.Kind = kind
		err := s.GrantSigned(short, sig, tNow, verifyOpts)
		if !errors.Is(err, signing.ErrInvalidMessage) {
			t.Fatalf("%s: GrantSigned with a 16-hex hash: %v, want ErrInvalidMessage", kind, err)
		}
	}
	if _, err := os.Stat(s.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused grant must not create the file: %v", err)
	}
	if s.Valid(tRepo, tBase, tShort) || s.Valid(tRepo, tBase, tFull) {
		t.Fatal("nothing may validate after refused grants")
	}
}
