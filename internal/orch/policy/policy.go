package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"aigate/internal/ledger"
)

// Level is an autonomy level, L0 (a human decides everything) to L3.
type Level int

// The levels. Their meaning is documented in package autonomy.
const (
	L0 Level = iota
	L1
	L2
	L3
)

func (l Level) String() string {
	if l < L0 || l > L3 {
		return fmt.Sprintf("L?(%d)", int(l))
	}
	return fmt.Sprintf("L%d", int(l))
}

// ParseLevel parses "L0".."L3" (exactly, upper case).
func ParseLevel(s string) (Level, error) {
	switch s {
	case "L0":
		return L0, nil
	case "L1":
		return L1, nil
	case "L2":
		return L2, nil
	case "L3":
		return L3, nil
	}
	return 0, fmt.Errorf("undefined level %q (use L0, L1, L2 or L3)", s)
}

// KnownKinds are the only action kinds an allowlist may name.
var KnownKinds = []string{"push_branch", "draft_pr", "notify"}

// MaxFileBytes caps the size of policy.yml.
const MaxFileBytes = 64 << 10

// Limits are the resource limits of the policy. A zero field is "not set":
// the daemon's own default (or flag) applies.
type Limits struct {
	AgentTimeout  time.Duration
	MaxConcurrent int
	MaxCommits    int
	MaxFetchBytes int64
}

// Policy is a validated policy.yml.
type Policy struct {
	Version      int
	DefaultLevel Level
	Classes      map[string]Level
	Allowlist    []string // sorted, unique
	Limits       Limits
	RepoRoots    []string // sorted, unique, absolute and clean
	path         string
	hash         string
}

// Hash is the sha256 of the canonical content: the same for any key order,
// whitespace or comments, and different for any change of meaning.
func (p Policy) Hash() string { return p.hash }

// Path is the file the policy was loaded from.
func (p Policy) Path() string { return p.path }

// StatePath is where demotions are kept: policy.state.json next to the policy.
func (p Policy) StatePath() string {
	return filepath.Join(filepath.Dir(p.path), "policy.state.json")
}

// LevelFor is the level the file grants class (default_level if unlisted).
// The effective level can be lower because of a demotion (see package autonomy).
func (p Policy) LevelFor(class string) Level {
	if l, ok := p.Classes[class]; ok {
		return l
	}
	return p.DefaultLevel
}

// AllowList returns the allowlist as the publisher wants it.
func (p Policy) AllowList() Allowlist { return NewAllowlist(p.Allowlist...) }

type rawPolicy struct {
	Version      *int                `yaml:"version"`
	Classes      map[string]rawClass `yaml:"classes"`
	DefaultLevel *string             `yaml:"default_level"`
	Allowlist    []string            `yaml:"allowlist"`
	Limits       *rawLimits          `yaml:"limits"`
	RepoRoots    []string            `yaml:"repo_roots"`
}

type rawClass struct {
	Level *string `yaml:"level"`
}

type rawLimits struct {
	AgentTimeout  *string `yaml:"agent_timeout"`
	MaxConcurrent *int    `yaml:"max_concurrent"`
	MaxCommits    *int    `yaml:"max_commits"`
	MaxFetchBytes *int64  `yaml:"max_fetch_bytes"`
}

var classNameRe = regexp.MustCompile(`^[a-z0-9_가-힣-]{1,32}$`)

// Load reads and strictly validates the policy file at path. It refuses a file
// that is not a regular file owned by the current user, or that is group- or
// world-writable (the file controls what runs unattended), unknown keys,
// duplicate keys or classes, undefined levels, bad or non-positive limits,
// and allowlist entries the hard floor denies or this build does not know.
func Load(path string) (Policy, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Policy{}, fmt.Errorf("policy: cannot open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return Policy{}, fmt.Errorf("policy: %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return Policy{}, fmt.Errorf("policy: %s has mode %04o; it must not be group- or world-writable (chmod 0644 or 0600)", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() && st.Uid != 0 {
		return Policy{}, fmt.Errorf("policy: %s is not owned by the current user or root", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return Policy{}, fmt.Errorf("policy: cannot read %s: %w", path, err)
	}
	if len(b) > MaxFileBytes {
		return Policy{}, fmt.Errorf("policy: %s is larger than %d bytes", path, MaxFileBytes)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Policy{}, err
	}
	p, err := Parse(b)
	if err != nil {
		return Policy{}, fmt.Errorf("policy: %s: %w", path, err)
	}
	p.path = abs
	return p, nil
}

// Parse validates policy text (no file checks) and returns the policy.
func Parse(b []byte) (Policy, error) {
	var raw rawPolicy
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Policy{}, errors.New("empty policy (need at least version: 1)")
		}
		return Policy{}, err
	}
	var extra rawPolicy
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("more than one YAML document")
	}
	if raw.Version == nil || *raw.Version != 1 {
		return Policy{}, errors.New("version must be 1")
	}
	p := Policy{Version: 1, Classes: map[string]Level{}}
	if raw.DefaultLevel != nil {
		l, err := ParseLevel(*raw.DefaultLevel)
		if err != nil {
			return Policy{}, fmt.Errorf("default_level: %w", err)
		}
		p.DefaultLevel = l
	}
	for name, c := range raw.Classes {
		if !classNameRe.MatchString(name) {
			return Policy{}, fmt.Errorf("class name %q is invalid", name)
		}
		if c.Level == nil {
			return Policy{}, fmt.Errorf("class %q: level is required", name)
		}
		l, err := ParseLevel(*c.Level)
		if err != nil {
			return Policy{}, fmt.Errorf("class %q: %w", name, err)
		}
		p.Classes[name] = l
	}
	seen := map[string]bool{}
	for _, k := range raw.Allowlist {
		if err := Floor(k); err != nil {
			return Policy{}, fmt.Errorf("allowlist entry %q is below the hard floor and can never be allowed: %v", k, err)
		}
		known := false
		for _, kk := range KnownKinds {
			known = known || kk == k
		}
		if !known {
			return Policy{}, fmt.Errorf("allowlist entry %q is not a known action kind (known: %v)", k, KnownKinds)
		}
		if seen[k] {
			return Policy{}, fmt.Errorf("allowlist entry %q is listed twice", k)
		}
		seen[k] = true
		p.Allowlist = append(p.Allowlist, k)
	}
	sort.Strings(p.Allowlist)
	if l := raw.Limits; l != nil {
		if l.AgentTimeout != nil {
			d, err := time.ParseDuration(*l.AgentTimeout)
			if err != nil {
				return Policy{}, fmt.Errorf("limits.agent_timeout: %v", err)
			}
			if d <= 0 {
				return Policy{}, errors.New("limits.agent_timeout must be positive")
			}
			p.Limits.AgentTimeout = d
		}
		if v := l.MaxConcurrent; v != nil {
			if *v <= 0 {
				return Policy{}, errors.New("limits.max_concurrent must be positive")
			}
			p.Limits.MaxConcurrent = *v
		}
		if v := l.MaxCommits; v != nil {
			if *v <= 0 {
				return Policy{}, errors.New("limits.max_commits must be positive")
			}
			p.Limits.MaxCommits = *v
		}
		if v := l.MaxFetchBytes; v != nil {
			if *v <= 0 {
				return Policy{}, errors.New("limits.max_fetch_bytes must be positive")
			}
			p.Limits.MaxFetchBytes = *v
		}
	}
	rs := map[string]bool{}
	for _, r := range raw.RepoRoots {
		if !filepath.IsAbs(r) || filepath.Clean(r) != r {
			return Policy{}, fmt.Errorf("repo_roots entry %q must be an absolute clean path", r)
		}
		if rs[r] {
			return Policy{}, fmt.Errorf("repo_roots entry %q is listed twice", r)
		}
		rs[r] = true
		p.RepoRoots = append(p.RepoRoots, r)
	}
	sort.Strings(p.RepoRoots)
	p.hash = p.canonicalHash()
	return p, nil
}

func (p Policy) canonicalHash() string {
	classes := map[string]string{}
	for k, v := range p.Classes {
		classes[k] = v.String()
	}
	canon := struct {
		Version      int               `json:"version"`
		DefaultLevel string            `json:"default_level"`
		Classes      map[string]string `json:"classes"`
		Allowlist    []string          `json:"allowlist"`
		AgentTimeout int64             `json:"agent_timeout_ns"`
		MaxConc      int               `json:"max_concurrent"`
		MaxCommits   int               `json:"max_commits"`
		MaxFetch     int64             `json:"max_fetch_bytes"`
		RepoRoots    []string          `json:"repo_roots"`
	}{p.Version, p.DefaultLevel.String(), classes, nonNil(p.Allowlist), int64(p.Limits.AgentTimeout),
		p.Limits.MaxConcurrent, p.Limits.MaxCommits, p.Limits.MaxFetchBytes, nonNil(p.RepoRoots)}
	b, _ := json.Marshal(canon) // map keys are sorted by encoding/json
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// OrchRepo is the "repo" of ledger events about the orchestrator itself.
const OrchRepo = "orchd"

// OrchLedgerPath is the ledger for policy and autonomy events under a gate home.
func OrchLedgerPath(gateHome string) string {
	return filepath.Join(gateHome, ".aigate", "ledger", OrchRepo+".jsonl")
}

// Live holds the policy orchd runs under and re-reads it on demand (at the
// start of each task). A change of hash is recorded once in the ledger. A file
// that no longer loads is an error: orchd does not fall back to an older,
// possibly more permissive policy.
type Live struct {
	path, ledger string
	now          func() time.Time
	mu           sync.Mutex
	cur          Policy
	startup      Policy
}

// NewLive loads the policy and records its hash in the ledger at ledgerPath.
func NewLive(path, ledgerPath string) (*Live, error) {
	p, err := Load(path)
	if err != nil {
		return nil, err
	}
	l := &Live{path: path, ledger: ledgerPath, now: time.Now, cur: p, startup: p}
	// A restart under the same policy is not a change.
	if evs, err := ledger.Events(ledgerPath); err == nil {
		last := ""
		for _, e := range evs {
			if e.Type == "policy_changed" {
				last, _ = e.Payload["new_hash"].(string)
			}
		}
		if last == p.hash {
			return l, nil
		}
		if last != "" {
			if err := l.record(last, p, nil); err != nil {
				return nil, err
			}
			return l, nil
		}
	}
	if err := l.record("", p, nil); err != nil {
		return nil, err
	}
	return l, nil
}

// Current is the last successfully loaded policy.
func (l *Live) Current() Policy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}

// Reload re-reads the file. If the hash changed the new policy becomes current
// and one ledger event is written.
func (l *Live) Reload() (Policy, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, err := Load(l.path)
	if err != nil {
		return Policy{}, err
	}
	if p.hash == l.cur.hash {
		return l.cur, nil
	}
	var restart []string
	if p.Limits != l.startup.Limits {
		restart = append(restart, "limits")
	}
	if fmt.Sprint(p.RepoRoots) != fmt.Sprint(l.startup.RepoRoots) {
		restart = append(restart, "repo_roots")
	}
	if err := l.record(l.cur.hash, p, restart); err != nil {
		return Policy{}, err
	}
	l.cur = p
	return p, nil
}

func (l *Live) record(old string, p Policy, restart []string) error {
	if l.ledger == "" {
		return nil
	}
	payload := map[string]any{"old_hash": old, "new_hash": p.hash, "path": p.path}
	if len(restart) > 0 {
		payload["restart_required"] = restart
	}
	if err := ledger.Append(l.ledger, "policy_changed", OrchRepo, payload, l.now().UTC()); err != nil {
		return fmt.Errorf("policy: cannot record the policy hash in the ledger: %w", err)
	}
	return nil
}
