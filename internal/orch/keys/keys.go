// Package keys makes and keeps the per-repository deploy keys of GitHub
// projects (W3e). GitHub refuses the same deploy key on a second repository, so
// every GitHub project gets its own ed25519 key pair, made here with the Go
// standard library (no ssh-keygen, no external program).
//
// Life cycle. A proposal makes the pair in a staging directory
// <root>/keys-staging/<random>/ (0700) and hands out only the public line (its
// SHA-256 is signed). Staging touches neither the registry nor the provider. An
// apply looks the pair up by the signed hash (Staged), forwards the public
// line, and only when the project is registered moves the directory to
// <root>/keys/<project id>/ with a no-replace rename (Promote). A refused apply
// deletes it (Discard). Staged pairs expire after StagingTTL and are swept
// lazily (SweepExpired) and, for everything, at orchd start (SweepAll). The
// sweeps only remove real directories with the staging name shape that belong
// to the running user; nothing is followed through a symlink.
//
// The private key is written once, by Stage, and is never read again by this
// package: the ssh command line names its path (PrivatePath only inspects the
// file's metadata). It is in no return value, error or log line.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"aigate/internal/orch/fsx"
	"aigate/internal/registrymsg"
)

const (
	// PrivateName and PublicName are the file names inside keys/<id>/ and in a
	// staging directory.
	PrivateName = "id_ed25519"
	PublicName  = "id_ed25519.pub"
	// StagingTTL is how long a staged key can still be applied.
	StagingTTL = 15 * time.Minute
	// MaxStaged bounds the staged keys that exist at once.
	MaxStaged = 64
	// HoldTTL bounds how long an apply may hold a staged key: a hold older than
	// this (a hung or crashed apply) no longer protects the entry, so held
	// entries cannot fill MaxStaged for good.
	HoldTTL = 10 * time.Minute
	// MaxRetained bounds the keys kept past their expiry because the outcome of
	// the apply that used them is unknown.
	MaxRetained = 16

	stagingDirName = "keys-staging"
	keysDirName    = "keys"
	pubComment     = "orchd-deploy"
	maxPubBytes    = 2048
	maxPrivBytes   = 16 << 10
)

// Fixed errors: none carries a path, key bytes or provider text.
var (
	ErrBadRoot  = errors.New("keys: the key directory is missing or not private to this user")
	ErrTooMany  = errors.New("keys: too many staged keys")
	ErrStaging  = errors.New("keys: could not stage a key")
	ErrNotFound = errors.New("keys: no such staged key")
	ErrExpired  = errors.New("keys: the staged key expired")
	ErrMismatch = errors.New("keys: the staged public key is not the signed one")
	ErrExists   = errors.New("keys: a key directory of that name exists already")
	ErrHeld     = errors.New("keys: the staged key is being applied already")
	ErrBadID    = errors.New("keys: not a project id")
	ErrUnusable = errors.New("keys: the key file is missing or not usable")
)

var stagingNameRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Key is what a staged pair shows the outside: the public line (the exact text
// the broker is given and whose SHA-256 is signed), its ssh-keygen style
// fingerprint, and the hash.
type Key struct {
	Public      string
	Fingerprint string
	SHA256      string
}

// Hash is the signed value for a public line: lowercase hex SHA-256 of its bytes.
func Hash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

type entry struct {
	dir    string
	info   os.FileInfo
	at     time.Time
	held   bool // an apply is using it: sweeps leave it alone (until HoldTTL)
	heldAt time.Time
	// retained: the key went to the broker and the outcome is unknown, so the
	// registry may hold a project that needs it. It does not expire; Reconcile
	// (promote if registered, else discard after StagingTTL) decides later.
	retained bool
	id, prov string
}

// isHeld reports a live hold (not older than HoldTTL).
func (e *entry) isHeld(now time.Time) bool { return e.held && now.Sub(e.heldAt) < HoldTTL }

// Store is the key area under Root (the directory that holds orchd's ssh
// files, e.g. /home/orch/orchd/ssh). Its staging index lives in memory: a
// restart forgets every staged key (SweepAll deletes them).
type Store struct {
	Root string
	// Now is the clock (nil = time.Now).
	Now func() time.Time

	mu     sync.Mutex
	staged map[string]*entry // signed hash -> staged directory
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// KeysDir is the directory of the promoted keys.
func (s *Store) KeysDir() string { return filepath.Join(s.Root, keysDirName) }

func (s *Store) stagingDir() string { return filepath.Join(s.Root, stagingDirName) }

func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// privateDir checks that p is a real directory (not a symlink), ours, and not
// reachable by group or others.
func privateDir(p string) error {
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 || !ownedByUs(fi) {
		return ErrBadRoot
	}
	return nil
}

// ensureDir makes sure the 0700 directory p exists (creating it when absent)
// and is private. Its parent must exist already.
func ensureDir(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return ErrBadRoot
		}
	}
	return privateDir(p)
}

// Stage makes a NEW key pair in keys-staging/<random>/ and returns its public
// parts. Every call makes a new key: a key that may already be attached to a
// repository is never offered again.
func (s *Store) Stage() (Key, error) {
	if s == nil || s.Root == "" || !filepath.IsAbs(s.Root) {
		return Key{}, ErrBadRoot
	}
	if fi, err := os.Lstat(s.Root); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return Key{}, ErrBadRoot
	}
	if err := ensureDir(s.stagingDir()); err != nil {
		return Key{}, err
	}
	s.SweepExpired()
	s.mu.Lock()
	n := len(s.staged)
	s.mu.Unlock()
	if n >= MaxStaged {
		return Key{}, ErrTooMany
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, ErrStaging
	}
	block, err := ssh.MarshalPrivateKey(priv, pubComment)
	clear(priv)
	if err != nil {
		return Key{}, ErrStaging
	}
	pemBytes := pem.EncodeToMemory(block)
	defer clear(pemBytes)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return Key{}, ErrStaging
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + pubComment
	k := Key{Public: line, Fingerprint: ssh.FingerprintSHA256(sshPub), SHA256: Hash(line)}

	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return Key{}, ErrStaging
	}
	dir := filepath.Join(s.stagingDir(), hex.EncodeToString(rnd[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Key{}, ErrStaging
	}
	info, err := os.Lstat(dir)
	if err != nil {
		os.Remove(dir)
		return Key{}, ErrStaging
	}
	fail := func() (Key, error) {
		if fi, err := os.Lstat(dir); err == nil && fi.IsDir() && os.SameFile(fi, info) {
			os.RemoveAll(dir)
		}
		return Key{}, ErrStaging
	}
	if err := writeNew(filepath.Join(dir, PrivateName), pemBytes, 0o600); err != nil {
		return fail()
	}
	if err := writeNew(filepath.Join(dir, PublicName), []byte(line+"\n"), 0o644); err != nil {
		return fail()
	}
	s.mu.Lock()
	if s.staged == nil {
		s.staged = map[string]*entry{}
	}
	s.staged[k.SHA256] = &entry{dir: dir, info: info, at: s.now()}
	s.mu.Unlock()
	return k, nil
}

// writeNew creates path (it must not exist; a symlink there fails too) with
// exactly mode and writes data. It never reads.
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil { // the umask may only have tightened it
		f.Close()
		return err
	}
	return f.Close()
}

// ReadPublic reads a public key file strictly (a regular file, one line,
// ssh-ed25519, no options, comment of at most 100 characters without spaces)
// and returns its canonical parts. It never opens a private key.
func ReadPublic(path string) (Key, error) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxPubBytes {
		return Key{}, ErrUnusable
	}
	f, err := os.Open(path)
	if err != nil {
		return Key{}, ErrUnusable
	}
	defer f.Close()
	if fi2, err := f.Stat(); err != nil || !os.SameFile(fi, fi2) {
		return Key{}, ErrUnusable
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPubBytes+1))
	if err != nil || len(data) > maxPubBytes {
		return Key{}, ErrUnusable
	}
	line := strings.TrimSuffix(string(data), "\n")
	if line == "" || strings.ContainsAny(line, "\r\n\x00") || !strings.HasPrefix(line, "ssh-ed25519 ") {
		return Key{}, ErrUnusable
	}
	pk, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) != 0 || len(rest) != 0 || pk.Type() != ssh.KeyAlgoED25519 || len(comment) > 100 || strings.ContainsAny(comment, " \t") {
		return Key{}, ErrUnusable
	}
	canon := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if comment != "" {
		canon += " " + comment
	}
	return Key{Public: canon, Fingerprint: ssh.FingerprintSHA256(pk), SHA256: Hash(canon)}, nil
}

// Staged looks the staged key up by the SIGNED hash (never by anything that
// names a path), checks it has not expired, re-reads its public file and
// compares the hash again in constant time. On success the key is held:
// sweeps leave it alone until Promote, Discard or Release, and a second apply of
// the same key (a double submit) is refused with ErrHeld while it is held, so
// one apply can never delete or move the key another one is still using. An expired key is
// deleted and answers ErrExpired; an unknown hash (swept, orchd restarted)
// answers ErrNotFound; a public file that is no longer the signed key answers
// ErrMismatch.
func (s *Store) Staged(sha string) (Key, error) {
	s.SweepExpired()
	now := s.now()
	s.mu.Lock()
	e := s.staged[sha]
	if e == nil {
		s.mu.Unlock()
		return Key{}, ErrNotFound
	}
	if e.isHeld(now) {
		s.mu.Unlock()
		return Key{}, ErrHeld
	}
	if !e.retained && now.Sub(e.at) >= StagingTTL {
		delete(s.staged, sha)
		s.mu.Unlock()
		removeOwn(e)
		return Key{}, ErrExpired
	}
	e.held, e.heldAt = true, now
	dir, info := e.dir, e.info
	s.mu.Unlock()

	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() || !os.SameFile(fi, info) {
		s.Discard(sha)
		return Key{}, ErrNotFound
	}
	k, err := ReadPublic(filepath.Join(dir, PublicName))
	if err != nil || subtle.ConstantTimeCompare([]byte(k.SHA256), []byte(sha)) != 1 {
		s.Release(sha)
		return Key{}, ErrMismatch
	}
	return k, nil
}

// Release lets sweeps consider the staged key again (the apply it was held for
// did not reach the provider, so the same signed change may still be applied).
func (s *Store) Release(sha string) {
	s.mu.Lock()
	if e := s.staged[sha]; e != nil {
		e.held = false
	}
	s.mu.Unlock()
}

// Discard deletes a staged key (only the directory this store made).
func (s *Store) Discard(sha string) {
	s.mu.Lock()
	e := s.staged[sha]
	delete(s.staged, sha)
	s.mu.Unlock()
	if e != nil {
		removeOwn(e)
	}
}

func removeOwn(e *entry) {
	if fi, err := os.Lstat(e.dir); err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 && os.SameFile(fi, e.info) {
		_ = os.RemoveAll(e.dir)
	}
}

// Promote moves the held staged key to keys/<id>/ with a no-replace rename:
// the private key file is the same file before and after, an existing
// keys/<id> (a symlink included) is never replaced (ErrExists).
func (s *Store) Promote(sha, id string) error {
	if !registrymsg.ValidSubject(id) {
		return ErrBadID
	}
	s.mu.Lock()
	e := s.staged[sha]
	s.mu.Unlock()
	if e == nil {
		return ErrNotFound
	}
	if err := ensureDir(s.KeysDir()); err != nil {
		return err
	}
	if fi, err := os.Lstat(e.dir); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || !os.SameFile(fi, e.info) {
		return ErrNotFound // not the directory this store made
	}
	if err := fsx.RenameNoReplace(e.dir, filepath.Join(s.KeysDir(), id)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return ErrStaging
	}
	s.mu.Lock()
	delete(s.staged, sha)
	s.mu.Unlock()
	return nil
}

// Exists reports whether keys/<id> is taken (as anything).
func (s *Store) Exists(id string) bool {
	s.SweepExpired()
	if !registrymsg.ValidSubject(id) {
		return true // never a name this store would use: treat as taken
	}
	_, err := os.Lstat(filepath.Join(s.KeysDir(), id))
	return err == nil
}

// SweepExpired deletes the staged keys older than StagingTTL that no apply
// holds. It returns how many it removed.
func (s *Store) SweepExpired() int {
	now := s.now()
	var dead []*entry
	s.mu.Lock()
	for sha, e := range s.staged {
		if !e.isHeld(now) && !e.retained && now.Sub(e.at) >= StagingTTL {
			dead = append(dead, e)
			delete(s.staged, sha)
		}
	}
	s.mu.Unlock()
	for _, e := range dead {
		removeOwn(e)
	}
	return len(dead)
}

// Retain marks a staged key as kept past its expiry (the outcome of the apply
// that sent it is unknown) and releases its hold. false when MaxRetained keys
// are kept already: the caller then leaves it to expire.
func (s *Store) Retain(sha, id, prov string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.staged[sha]
	if e == nil {
		return false
	}
	if !e.retained {
		n := 0
		for _, o := range s.staged {
			if o.retained {
				n++
			}
		}
		if n >= MaxRetained {
			e.held = false
			return false
		}
	}
	e.retained, e.held, e.id, e.prov = true, false, id, prov
	return true
}

// Retained describes a retained key for Reconcile.
type Retained struct {
	SHA, ID, Provider string
	Age               time.Duration
}

// RetainedKeys lists the retained keys that no apply holds.
func (s *Store) RetainedKeys() []Retained {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Retained
	for sha, e := range s.staged {
		if e.retained && !e.isHeld(now) {
			out = append(out, Retained{SHA: sha, ID: e.id, Provider: e.prov, Age: now.Sub(e.at)})
		}
	}
	return out
}

// SweepAll removes every staging directory (orchd start: nothing staged
// survives a restart). Only real directories with a staging name shape that
// belong to this user are removed: files, symlinks (whatever they point at),
// other names and nested contents of other entries stay.
func (s *Store) SweepAll() int {
	if s == nil || s.Root == "" {
		return 0
	}
	dir := s.stagingDir()
	if privateDir(dir) != nil {
		return 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, de := range ents {
		if !stagingNameRE.MatchString(de.Name()) {
			continue
		}
		p := filepath.Join(dir, de.Name())
		fi, err := os.Lstat(p)
		di, derr := de.Info()
		if err != nil || derr != nil || !os.SameFile(fi, di) || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || !ownedByUs(fi) {
			continue
		}
		if os.RemoveAll(p) == nil {
			n++
		}
	}
	s.mu.Lock()
	s.staged = nil
	s.mu.Unlock()
	return n
}

// UsableFile reports whether path is a private key file this process may name
// in an ssh command: a regular file (not a symlink), owned by the running
// user, readable by the owner and by nobody else (0600 or 0400), of plausible
// size. It only inspects metadata: the file is not opened.
func UsableFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o177 != 0 || fi.Mode().Perm()&0o400 == 0 ||
		fi.Size() == 0 || fi.Size() > maxPrivBytes || !ownedByUs(fi) {
		return ErrUnusable
	}
	return nil
}

// PrivatePath returns keysDir/<id>/id_ed25519 for a project id after checking
// the whole chain by metadata: keysDir and keysDir/<id> are real private
// directories of this user and the key file passes UsableFile. The path is
// built from the id alone (a registry value that passed the project id rule),
// never from anything else.
func PrivatePath(keysDir, id string) (string, error) {
	if !registrymsg.ValidSubject(id) || keysDir == "" || !filepath.IsAbs(keysDir) || filepath.Clean(keysDir) != keysDir {
		return "", ErrBadID
	}
	if privateDir(keysDir) != nil || privateDir(filepath.Join(keysDir, id)) != nil {
		return "", ErrUnusable
	}
	p := filepath.Join(keysDir, id, PrivateName)
	if err := UsableFile(p); err != nil {
		return "", err
	}
	return p, nil
}

// ProjectPublic reads the public key of a project's own key folder,
// keysDir/<id>/id_ed25519.pub, after checking the chain by metadata (keysDir
// and keysDir/<id> are real private directories of this user). The private
// key is never opened. It is how a deploy key is identified for removal.
func ProjectPublic(keysDir, id string) (Key, error) {
	if !registrymsg.ValidSubject(id) || keysDir == "" || !filepath.IsAbs(keysDir) || filepath.Clean(keysDir) != keysDir {
		return Key{}, ErrBadID
	}
	if privateDir(keysDir) != nil || privateDir(filepath.Join(keysDir, id)) != nil {
		return Key{}, ErrUnusable
	}
	return ReadPublic(filepath.Join(keysDir, id, PublicName))
}
