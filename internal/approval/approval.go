// Package approval stores hash-bound human approvals outside the repository.
// An approval is valid only for the exact (repo, base, hash) triple.
//
// Signed-only mode is on whenever the signers registry exists at
// signing.RegistryPath(): then only entries carrying a signature that verifies
// against the registry, over a message naming the same triple, are valid, and
// unsigned grants are refused. Without a registry (legacy mode) behavior is
// unchanged. Any registry problem other than absence fails closed.
package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"aigate/internal/approval/signing"
	"golang.org/x/term"
)

// ErrSignatureRequired is returned by Grant in signed-only mode.
var ErrSignatureRequired = errors.New("서명 필수 모드에서는 서명된 승인만 기록할 수 있습니다")

// ErrRegistryUnavailable wraps a registry load failure other than absence;
// signed approvals are then refused (fail closed). Callers must not show the
// wrapped detail to users (CF1).
var ErrRegistryUnavailable = errors.New("서명자 목록을 신뢰할 수 없어 승인을 처리할 수 없습니다")

// ErrNotSignedMode is returned by GrantSigned when no signers registry exists
// (legacy mode): signed approvals cannot be verified there.
var ErrNotSignedMode = errors.New("서명자 목록이 없어 서명 승인을 검증할 수 없습니다(서명 필수 모드가 아님)")

const fileVersion = 1

// isTerminal is a seam so the positive TTY path is testable.
var isTerminal = term.IsTerminal

// Store is a JSON approvals file at Path.
type Store struct{ Path string }

type entry struct {
	Repo      string `json:"repo"`
	Base      string `json:"base"`
	Hash      string `json:"hash"`
	GrantedAt string `json:"granted_at"`
	// Signed entries (file version stays 1; legacy readers ignore these).
	Message  string         `json:"message,omitempty"`
	Signer   string         `json:"signer,omitempty"`
	Alg      string         `json:"alg,omitempty"`
	Sig      string         `json:"sig,omitempty"`
	WebAuthn *webauthnEntry `json:"webauthn,omitempty"`
}

type webauthnEntry struct {
	AuthenticatorData string `json:"authenticator_data"`
	ClientDataJSON    string `json:"client_data_json"`
}

func (e entry) is(repo, base, hash string) bool {
	return e.Repo == repo && e.Base == base && e.Hash == hash
}

// signedValid reports whether e is a signed approval of (repo, base, hash)
// that verifies against reg. The entry's plain triple and the triple in its
// (strictly parsed, validated) message must both equal the queried one.
// Stored approvals do not expire, so freshness is skipped.
func (e entry) signedValid(reg signing.Registry, repo, base, hash string) bool {
	if e.Message == "" || e.Signer == "" || e.Alg == "" || e.Sig == "" || !e.is(repo, base, hash) {
		return false
	}
	msg, err := signing.ParseMessage(e.Message)
	if err != nil || msg.Validate() != nil {
		return false
	}
	if msg.Repo != repo || msg.Base != base || msg.Hash != hash {
		return false
	}
	sig := signing.Signature{Signer: e.Signer, Alg: e.Alg, Sig: e.Sig}
	if e.WebAuthn != nil {
		sig.WebAuthn = &signing.WebAuthnData{AuthenticatorData: e.WebAuthn.AuthenticatorData, ClientDataJSON: e.WebAuthn.ClientDataJSON}
	}
	return signing.Verify(reg, msg, sig, signing.VerifyOptions{SkipFreshness: true}) == nil
}

// registryMode loads the process-wide registry. signed is false only when the
// registry is absent (legacy mode); any other load error is returned and
// callers must fail closed.
func registryMode() (reg signing.Registry, signed bool, err error) {
	reg, err = signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)
	if errors.Is(err, signing.ErrRegistryAbsent) {
		return signing.Registry{}, false, nil
	}
	if err != nil {
		return signing.Registry{}, true, fmt.Errorf("%w: %w", ErrRegistryUnavailable, err)
	}
	return reg, true, nil
}

// SignedMode reports whether signed-only mode is on. signed is false only when
// the registry is absent; a registry that cannot be trusted reports signed ==
// true with an error wrapping ErrRegistryUnavailable (fail closed).
func SignedMode() (signed bool, err error) {
	_, signed, err = registryMode()
	return signed, err
}

type fileData struct {
	Version   int     `json:"version"`
	Approvals []entry `json:"approvals"`
}

// load reads the store. A missing file yields an empty store (nil error);
// corrupt content or an unknown version yields an error.
func (s Store) load() (fileData, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return fileData{Version: fileVersion}, nil
	}
	if err != nil {
		return fileData{}, err
	}
	var d fileData
	if err := json.Unmarshal(b, &d); err != nil {
		return fileData{}, fmt.Errorf("approvals file %s is corrupt: %w", s.Path, err)
	}
	if d.Version != fileVersion {
		return fileData{}, fmt.Errorf("approvals file %s has unsupported version %d", s.Path, d.Version)
	}
	return d, nil
}

// Valid reports whether (repoID, baseSHA, hash) was granted. It never panics
// and returns false for a missing, corrupt or unknown-version file. In
// signed-only mode only verifying signed entries count, and a registry that
// cannot be trusted makes every approval invalid.
func (s Store) Valid(repoID, baseSHA, hash string) bool {
	if repoID == "" || baseSHA == "" || hash == "" {
		return false
	}
	reg, signed, err := registryMode()
	if err != nil {
		return false
	}
	d, err := s.load()
	if err != nil {
		return false
	}
	for _, e := range d.Approvals {
		if signed {
			if e.signedValid(reg, repoID, baseSHA, hash) {
				return true
			}
			continue
		}
		if e.is(repoID, baseSHA, hash) {
			return true
		}
	}
	return false
}

// Grant records an unsigned approval (legacy mode only; signed-only mode
// returns ErrSignatureRequired). It is idempotent for an identical triple and
// refuses (leaving the file untouched) on empty arguments or a bad existing file.
func (s Store) Grant(repoID, baseSHA, hash string, now time.Time) error {
	if repoID == "" || baseSHA == "" || hash == "" {
		return errors.New("approval: repo, base and hash must be non-empty")
	}
	if _, signed, err := registryMode(); err != nil {
		return err
	} else if signed {
		return ErrSignatureRequired
	}
	return s.update(func(d *fileData) bool {
		for _, e := range d.Approvals {
			if e.is(repoID, baseSHA, hash) {
				return false
			}
		}
		d.Approvals = append(d.Approvals, entry{
			Repo: repoID, Base: baseSHA, Hash: hash,
			GrantedAt: now.UTC().Format(time.RFC3339),
		})
		return true
	})
}

// GrantSigned records a signed approval of (msg.Repo, msg.Base, msg.Hash). The
// registry must exist and be trusted, and sig must verify over msg with
// freshness enforced against now (o.Now and o.SkipFreshness are overridden;
// o.MaxAge applies). Verification failures are returned as the signing
// sentinels, wrapped without detail. Entries are keyed by the triple: an
// existing verifying entry is kept (idempotent); an unsigned or non-verifying
// entry for the triple is replaced in place.
func (s Store) GrantSigned(msg signing.Message, sig signing.Signature, now time.Time, o signing.VerifyOptions) error {
	if msg.Validate() != nil {
		return fmt.Errorf("approval: %w", signing.ErrInvalidMessage)
	}
	reg, signed, err := registryMode()
	if err != nil {
		return err
	}
	if !signed {
		return ErrNotSignedMode
	}
	o.Now, o.SkipFreshness = now, false
	if err := signing.Verify(reg, msg, sig, o); err != nil {
		return fmt.Errorf("approval: %w", err)
	}
	ne := entry{
		Repo: msg.Repo, Base: msg.Base, Hash: msg.Hash,
		GrantedAt: now.UTC().Format(time.RFC3339),
		Message:   msg.Text(), Signer: sig.Signer, Alg: sig.Alg, Sig: sig.Sig,
	}
	if sig.WebAuthn != nil {
		ne.WebAuthn = &webauthnEntry{AuthenticatorData: sig.WebAuthn.AuthenticatorData, ClientDataJSON: sig.WebAuthn.ClientDataJSON}
	}
	return s.update(func(d *fileData) bool {
		for i, e := range d.Approvals {
			if !e.is(msg.Repo, msg.Base, msg.Hash) {
				continue
			}
			if e.signedValid(reg, msg.Repo, msg.Base, msg.Hash) {
				return false
			}
			d.Approvals[i] = ne
			return true
		}
		d.Approvals = append(d.Approvals, ne)
		return true
	})
}

// update runs fn on the current file contents under the exclusive flock that
// every writer takes, and writes the result atomically if fn reports a change.
// A corrupt or unknown-version file is left untouched.
func (s Store) update(fn func(d *fileData) (changed bool)) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	d, err := s.load()
	if err != nil {
		return err
	}
	if !fn(&d) {
		return nil
	}
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.Path, append(out, '\n'))
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".approvals-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// RequireTTY returns an error unless stdin is a real terminal.
func RequireTTY(stdin *os.File) error {
	if stdin == nil || !isTerminal(int(stdin.Fd())) {
		return errors.New("approval requires an interactive terminal (stdin is not a TTY)")
	}
	return nil
}
