package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"aigate/internal/registrymsg"
)

// Token metadata (decision D2): <state>/tokens.json (0600) lists what the
// secret store holds, without any value. It follows the registry rules of W3a:
// absent means empty, anything else that is not a trusted, well-formed file
// makes the whole token store unavailable (fail closed, file untouched).
//
// A stored secret is usable only when metadata and a trusted file both exist.
// A file without metadata (orphan) is ignored and reported; metadata without a
// usable file is listed as unusable.

// TokensFileName is the metadata file inside the state directory.
const TokensFileName = "tokens.json"

// WarnDays is the expiry warning window (decision D6).
const WarnDays = 14

const maxTokenEntries = 200

var (
	// ErrTokensBroken: the metadata or the secrets directory cannot be trusted.
	ErrTokensBroken = errors.New("토큰 저장소를 사용할 수 없습니다")
	// ErrTokenUnknown: no such token to replace or delete.
	ErrTokenUnknown = errors.New("등록되지 않은 토큰입니다")
	// ErrTokenKindMismatch: the name is already used by another kind.
	ErrTokenKindMismatch = errors.New("같은 이름의 토큰이 다른 종류로 있습니다")
	// ErrOpenAISingleton: the openai kind has exactly one name, openai.
	ErrOpenAISingleton = errors.New("openai 종류는 이름이 openai인 것 하나뿐입니다")
	errTokenKind       = errors.New("토큰 종류가 올바르지 않습니다")
	errTokenExpires    = errors.New("만료일이 올바르지 않습니다")
	errTokenSave       = errors.New("토큰 정보를 저장할 수 없습니다")
	errTokenFull       = errors.New("토큰이 너무 많습니다")
)

// TokenEntry is one metadata record. It never holds the value.
type TokenEntry struct {
	Ref     string `json:"ref"`
	Kind    string `json:"kind"`
	Last4   string `json:"last4"`
	Expires string `json:"expires"`
	SetAt   string `json:"set_at"`
}

// TokenInfo is a TokenEntry as the broker reports it.
type TokenInfo struct {
	TokenEntry
	Usable   bool
	DaysLeft *int
}

type tokensFile struct {
	Version int          `json:"version"`
	Tokens  []TokenEntry `json:"tokens"`
}

// TokenMeta is tokens.json plus its in-memory copy. Writers hold TokenStore's lock.
type TokenMeta struct {
	path    string
	opts    RegistryOptions
	entries []TokenEntry
	broken  bool
}

// TokenStore is the secret store together with its metadata. Safe for
// concurrent use.
type TokenStore struct {
	mu      sync.RWMutex
	Secrets *SecretStore
	Meta    *TokenMeta

	// cache holds the currently usable secret values for log redaction. It has
	// its own mutex so the logger never waits for (or takes) mu, and nothing
	// logs while holding mu.
	// It is a pointer so that printing a TokenStore never walks into the values.
	cache *secretCache
}

// secretCache is the redaction value list with its own mutex.
type secretCache struct {
	mu sync.Mutex
	v  []string
}

func (*secretCache) String() string   { return "[secrets]" }
func (*secretCache) GoString() string { return "[secrets]" }

// OpenTokenStore opens (creating secrets/ 0700 when missing) the store under
// stateDir. A broken store is still returned, refusing everything, together
// with ErrTokensBroken; no file is changed in that case.
func OpenTokenStore(stateDir string, o RegistryOptions) (*TokenStore, error) {
	sec, serr := OpenSecretStore(stateDir, o)
	ts := &TokenStore{cache: &secretCache{}, Secrets: sec, Meta: &TokenMeta{path: filepath.Join(stateDir, TokensFileName), opts: o}}
	if serr != nil {
		ts.Meta.broken = true
		return ts, ErrTokensBroken
	}
	data, exists, err := readStateFile(ts.Meta.path, o.fileOwner())
	if err != nil {
		ts.Meta.broken = true
		return ts, ErrTokensBroken
	}
	if exists {
		entries, err := parseTokensFile(data)
		if err != nil {
			ts.Meta.broken = true
			return ts, ErrTokensBroken
		}
		ts.Meta.entries = entries
	}
	ts.refreshCache()
	return ts, nil
}

// Err is ErrTokensBroken when the store refuses everything.
func (t *TokenStore) Err() error {
	if t == nil || t.Meta.broken {
		return ErrTokensBroken
	}
	return nil
}

func validExpires(s string) bool {
	if s == "" {
		return true
	}
	if !registrymsgDate(s) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func registrymsgDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i, c := range []byte(s) {
		if i == 4 || i == 7 {
			if c != '-' {
				return false
			}
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validKindRef(ref, kind string) error {
	if !registrymsg.ValidTokenRef(ref) {
		return errSecretName
	}
	if !registrymsg.ValidTokenKind(kind) {
		return errTokenKind
	}
	if (kind == "openai") != (ref == "openai") {
		return ErrOpenAISingleton
	}
	return nil
}

func parseTokensFile(data []byte) ([]TokenEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f tokensFile
	if err := dec.Decode(&f); err != nil || f.Version != 1 {
		return nil, ErrTokensBroken
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrTokensBroken
	}
	if len(f.Tokens) > maxTokenEntries {
		return nil, ErrTokensBroken
	}
	seen := map[string]bool{}
	for _, e := range f.Tokens {
		if validKindRef(e.Ref, e.Kind) != nil || seen[e.Ref] || !validExpires(e.Expires) || len(e.Last4) > 4 || !printableASCII(e.Last4) {
			return nil, ErrTokensBroken
		}
		if _, err := time.Parse(time.RFC3339, e.SetAt); err != nil {
			return nil, ErrTokensBroken
		}
		seen[e.Ref] = true
	}
	if f.Tokens == nil {
		f.Tokens = []TokenEntry{}
	}
	return f.Tokens, nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func (t *TokenStore) find(ref string) int {
	for i, e := range t.Meta.entries {
		if e.Ref == ref {
			return i
		}
	}
	return -1
}

// writeMeta saves entries atomically. renamed reports that the file now holds
// them (even if only the directory fsync failed); memory then equals disk.
func (t *TokenStore) writeMeta(entries []TokenEntry, failAt func(string) error) (renamed bool, err error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Ref < entries[j].Ref })
	b, err := json.Marshal(tokensFile{Version: 1, Tokens: entries})
	if err != nil {
		return false, errTokenSave
	}
	renamed, err = writeAtomic(t.Meta.path, append(b, '\n'), failAt, false)
	if renamed {
		t.Meta.entries = entries
	}
	if err != nil {
		return renamed, errTokenSave
	}
	return renamed, nil
}

// refreshCache recomputes the redaction cache from the trusted files.
// Callers hold mu (any mode); it never logs.
func (t *TokenStore) refreshCache() {
	var out []string
	if !t.Meta.broken {
		for _, e := range t.Meta.entries {
			if v, ok := t.Secrets.Get(e.Ref); ok {
				out = append(out, v)
			}
		}
	}
	t.cache.mu.Lock()
	t.cache.v = out
	t.cache.mu.Unlock()
}

func (t *TokenStore) addToCache(v string) {
	t.cache.mu.Lock()
	t.cache.v = append(t.cache.v, v)
	t.cache.mu.Unlock()
}

// Set stores value for ref and records its metadata. The secret file is
// written first; if the metadata write then fails, the previous value (or
// absence) is restored, so a failed Set changes nothing visible. expires is ""
// or YYYY-MM-DD; whether it lies in the past is the caller's rule.
func (t *TokenStore) Set(ref, kind, expires, value string, now time.Time) error {
	if t == nil {
		return ErrTokensBroken
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Meta.broken {
		return ErrTokensBroken
	}
	if err := validKindRef(ref, kind); err != nil {
		return err
	}
	if !validExpires(expires) {
		return errTokenExpires
	}
	i := t.find(ref)
	if i >= 0 && t.Meta.entries[i].Kind != kind {
		return ErrTokenKindMismatch
	}
	if i < 0 && len(t.Meta.entries) >= maxTokenEntries {
		return errTokenFull
	}
	if value == "" {
		return errSecretValue
	}
	last4 := value
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	// Invariant: the file in secrets/ and its metadata agree about which
	// value is live, and an error return means the previous one still is.
	// Secret.Put restores the previous file itself when it fails after its
	// rename; the metadata is written second and, on failure, put back (also
	// when only its directory fsync failed) before the secret is restored.
	// Accepted limit: a crash between the secret rename and the metadata
	// write of a REPLACEMENT leaves the new value live with the old last4 and
	// expiry (the value works, the listing is stale until the next set).
	t.addToCache(value) // redact it from the first moment it can exist
	defer t.refreshCache()
	prev, hadPrev := t.Secrets.Get(ref)
	if err := t.Secrets.Put(ref, value); err != nil {
		return err
	}
	next := append([]TokenEntry(nil), t.Meta.entries...)
	e := TokenEntry{Ref: ref, Kind: kind, Last4: last4, Expires: expires, SetAt: now.UTC().Format(time.RFC3339)}
	if i >= 0 {
		next[i] = e
	} else {
		next = append(next, e)
	}
	oldEntries := append([]TokenEntry(nil), t.Meta.entries...)
	if renamed, err := t.writeMeta(next, t.Meta.opts.failAt); err != nil {
		if renamed {
			t.writeMeta(oldEntries, nil)
		}
		if hadPrev {
			t.Secrets.Put(ref, prev)
		} else {
			t.Secrets.Delete(ref)
		}
		return err
	}
	return nil
}

// Delete removes the metadata and the secret file of ref. It reports
// ErrTokenUnknown when neither existed.
func (t *TokenStore) Delete(ref string) error {
	if t == nil {
		return ErrTokensBroken
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Meta.broken {
		return ErrTokensBroken
	}
	if !registrymsg.ValidTokenRef(ref) {
		return errSecretName
	}
	i := t.find(ref)
	if i < 0 && t.Secrets.Lookup(ref) == SecretAbsent {
		return ErrTokenUnknown
	}
	// Metadata first: after a crash in between, the leftover is an orphan
	// file (ignored), never a usable secret the operator thinks is gone.
	if i >= 0 {
		old := append([]TokenEntry(nil), t.Meta.entries...)
		next := append(append([]TokenEntry(nil), t.Meta.entries[:i]...), t.Meta.entries[i+1:]...)
		if renamed, err := t.writeMeta(next, t.Meta.opts.failAt); err != nil {
			if renamed {
				t.writeMeta(append([]TokenEntry(nil), old...), nil)
			}
			return err
		}
	}
	defer t.refreshCache()
	return t.Secrets.Delete(ref)
}

// Value returns the stored secret of ref when the store is healthy, the
// metadata names it and its file is trusted.
func (t *TokenStore) Value(ref string) (string, bool) {
	if t == nil {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Meta.broken || t.find(ref) < 0 {
		return "", false
	}
	return t.Secrets.Get(ref)
}

// values returns the cached usable secret values (for log redaction). It
// takes only the cache mutex.
func (t *TokenStore) values() []string {
	if t == nil {
		return nil
	}
	t.cache.mu.Lock()
	defer t.cache.mu.Unlock()
	return append([]string(nil), t.cache.v...)
}

// List returns the metadata with usability and days left, sorted by ref.
func (t *TokenStore) List(now time.Time) ([]TokenInfo, error) {
	if t == nil {
		return nil, ErrTokensBroken
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Meta.broken {
		return nil, ErrTokensBroken
	}
	out := make([]TokenInfo, 0, len(t.Meta.entries))
	for _, e := range t.Meta.entries {
		out = append(out, TokenInfo{TokenEntry: e, Usable: t.Secrets.Lookup(e.Ref) == SecretOK, DaysLeft: DaysLeft(e.Expires, now)})
	}
	return out, nil
}

// Orphans lists secret files that have no metadata (never used).
func (t *TokenStore) Orphans() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Meta.broken {
		return nil
	}
	var out []string
	for _, r := range t.Secrets.Refs() {
		if t.find(r) < 0 {
			out = append(out, r)
		}
	}
	return out
}

// DaysLeft is the number of calendar days (UTC) from now's day to the expiry
// day: 0 on the day itself, negative after it. nil when expires is empty or
// not a date.
func DaysLeft(expires string, now time.Time) *int {
	if expires == "" || !validExpires(expires) {
		return nil
	}
	exp, err := time.Parse("2006-01-02", expires)
	if err != nil {
		return nil
	}
	n := now.UTC()
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	d := int(exp.Sub(today).Hours() / 24)
	return &d
}

// Expiring reports whether a token with this many days left is within the
// warning window or already expired.
func Expiring(daysLeft *int) bool { return daysLeft != nil && *daysLeft <= WarnDays }

// TokenStoreStatus inspects the token store read-only (for orch-broker
// -check): nothing is created or written. A missing secrets directory or
// tokens.json is fine. A directory or metadata that Open would call broken is
// ErrTokensBroken. warnings are fixed sentences (never a value or a path).
func TokenStoreStatus(stateDir string, o RegistryOptions) (warnings []string, err error) {
	if _, e := os.Lstat(stateDir); e != nil {
		return nil, nil
	}
	secDir := filepath.Join(stateDir, "secrets")
	sec := &SecretStore{Dir: secDir, opts: o}
	if _, e := os.Lstat(secDir); e == nil {
		if checkStateDir(secDir, o.ownerOK()) != nil {
			return nil, ErrTokensBroken
		}
	}
	data, exists, rerr := readStateFile(filepath.Join(stateDir, TokensFileName), o.fileOwner())
	if rerr != nil {
		return nil, ErrTokensBroken
	}
	var entries []TokenEntry
	if exists {
		if entries, rerr = parseTokensFile(data); rerr != nil {
			return nil, ErrTokensBroken
		}
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Ref] = true
		switch sec.Lookup(e.Ref) {
		case SecretAbsent:
			warnings = append(warnings, "경고: tokens.json에 있는 토큰의 비밀 파일이 없어 그 토큰은 쓸 수 없습니다")
		case SecretUntrusted:
			warnings = append(warnings, "경고: 비밀 파일의 소유자·모드(0400)가 올바르지 않아 그 토큰은 쓸 수 없습니다")
		}
	}
	for _, r := range sec.Refs() {
		if !have[r] {
			warnings = append(warnings, "경고: tokens.json에 없는 비밀 파일이 있어 무시됩니다")
		}
	}
	return warnings, nil
}

// String and GoString keep any formatting of a TokenStore from walking into
// its fields (the redaction cache holds values).
func (*TokenStore) String() string   { return "[tokenstore]" }
func (*TokenStore) GoString() string { return "[tokenstore]" }
