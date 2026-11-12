package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"aigate/internal/registrymsg"
)

// The project registry (decision D1) is owned by the broker: one JSON file in
// the 0700 state directory, 0600, written atomically. The broker is the only
// writer, and this file holds the only write code (writeAtomic is also used by
// the used-request store).
//
// Fail closed: only a truly missing file starts a fresh registry (and migrates
// the legacy broker.json entries once). A file that is broken, unknown-keyed,
// of a version other than 1 or 2, not owned by the broker account or not 0600
// is never treated as "absent": the broker keeps running but every read and
// write that depends on the registry answers registry_unavailable.
//
// Versions. The file is version 2 (W3c: providers, project provider links and
// GitLab ids, orphan records). A valid version 1 file (W3a/W3b) is migrated to
// 2 when it is opened: same content, rewritten atomically as 0600 (temp file,
// fsync, rename); if the write fails the v1 file is untouched and the open is a
// start failure. A W3b broker accepts only version 1 and unknown keys are
// refused, so it treats a version 2 file as broken (fail closed): rolling the
// broker back means restoring the registry from the pre-migration backup.

const (
	// RegistryFileName and UsedRequestsFileName live in the state directory.
	RegistryFileName     = "registry.json"
	UsedRequestsFileName = "used-requests.json"

	// registryVersion is what this code writes. registryVersionV1 is the only
	// older version it still reads (and migrates). TestOldBrokerVersionRefusesV2
	// pins both.
	registryVersion = 3
	// registryVersionV2 is the W3c file shape: project links are
	// gitlab:{project,project_id}; read once and migrated to version 3.
	registryVersionV2 = 2
	registryVersionV1 = 1
	maxStateFile      = 1 << 20
	maxProjects       = 500
	maxPathBytes      = 4096
	maxProviders      = 20
	maxOrphans        = 100
)

var (
	// ErrRegistryBroken: the registry file exists but cannot be trusted.
	ErrRegistryBroken = errors.New("프로젝트 등록부를 사용할 수 없습니다")
	// ErrMigrationConflict: legacy entries cannot be turned into a valid registry.
	ErrMigrationConflict = errors.New("옛 설정을 등록부로 옮길 수 없습니다(프로젝트 id 충돌 또는 형식 오류)")

	projectIDRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	projectKindRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	orphanStepRE  = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)
)

// orphanTimeLayout is the only accepted form of Orphan.At (UTC, whole seconds).
const orphanTimeLayout = "2006-01-02T15:04:05Z"

// legacyKind is the policy class name given to projects migrated from
// broker.json (the old config knows no classes).
const legacyKind = "legacy"

// Registry is the D1 shape (version 3).
type Registry struct {
	Version         int           `json:"version"`
	Providers       []Provider    `json:"providers"`
	Projects        []Project     `json:"projects"`
	AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
	Orphans         []Orphan      `json:"orphans"`
}

// Provider is a configured Git provider: kind gitlab or github. TokenRef names
// a token of the same kind in the token store; the token value is never here.
type Provider struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	BaseURL  string `json:"base_url"`
	SSHURL   string `json:"ssh_url"`
	TokenRef string `json:"token_ref"`
}

// Orphan records a GitLab project that a failed create_project left behind
// (the broker never deletes it). Values only: no tokens, no response bodies.
// ProjectID is null when GitLab did not confirm the id. At is UTC
// "2006-01-02T15:04:05Z". Provider need not exist any more.
type Orphan struct {
	Provider  string `json:"provider"`
	Project   string `json:"project"`
	ProjectID *int64 `json:"project_id"`
	Step      string `json:"step"`
	At        string `json:"at"`
}

// orphanDroppedLog is the fixed log line written when the oldest orphan record
// is dropped to stay within maxOrphans (no values).
const orphanDroppedLog = "orphan record dropped: the list is at its limit of 100"

// AddOrphan appends o. The registry never holds more than maxOrphans (100)
// records: when the list overflows, the OLDEST records are dropped and dropped
// is true (callers tell the user).
func (r *Registry) AddOrphan(o Orphan) (dropped bool) {
	if o.ProjectID != nil {
		id := *o.ProjectID
		o.ProjectID = &id
	}
	r.Orphans = append(r.Orphans, o)
	if n := len(r.Orphans) - maxOrphans; n > 0 {
		r.Orphans = append([]Orphan(nil), r.Orphans[n:]...)
		return true
	}
	return false
}

// AddOrphan stores an orphan record (see Registry.AddOrphan) and persists it.
// dropped reports that the oldest record was discarded; that is also logged as
// one fixed line. If the write fails nothing changes and dropped is false.
func (s *RegistryStore) AddOrphan(o Orphan) (dropped bool, err error) {
	d := false
	if err = s.Update(func(r *Registry) error { d = r.AddOrphan(o); return nil }); err != nil {
		return false, err
	}
	if d {
		if s.opts.Logf != nil {
			s.opts.Logf("%s", orphanDroppedLog)
		} else {
			log.Print(orphanDroppedLog)
		}
	}
	return d, nil
}

// Project is one registered repository. Provider is a provider id or null (a
// project that has no provider, such as every project of a v1 registry). Remote
// carries the repository path (group/…/name for GitLab, owner/repo for GitHub)
// used by create_mr and the publish resolver; with a provider it may also carry
// the numeric repository id.
type Project struct {
	ID        string      `json:"id"`
	Path      string      `json:"path"`
	Kind      string      `json:"kind"`
	AIContext bool        `json:"ai_context"`
	Provider  *string     `json:"provider"`
	Remote    *RemoteLink `json:"remote"`
	AutoMerge AutoMerge   `json:"auto_merge"`
}

// RemoteLink is the project's remote repository: Path is group/…/name (GitLab)
// or owner/repo (GitHub); ID is the provider's numeric id (null for a link
// without a provider, such as a migrated broker.json entry, or when the
// provider did not confirm it).
type RemoteLink struct {
	Path string `json:"path"`
	ID   *int64 `json:"id"`
}

// gitlabLinkV2 is the link shape of version 1 and 2 files: gitlab:{project,project_id}.
type gitlabLinkV2 struct {
	Project   string `json:"project"`
	ProjectID *int64 `json:"project_id"`
}

func (g *gitlabLinkV2) toRemote() *RemoteLink {
	if g == nil {
		return nil
	}
	r := &RemoteLink{Path: g.Project}
	if g.ProjectID != nil {
		id := *g.ProjectID
		r.ID = &id
	}
	return r
}

// AutoMerge is per project; W3a keeps it off.
type AutoMerge struct {
	Enabled bool `json:"enabled"`
}

// AutoMergeGlob is the global switch; W3a keeps it off.
type AutoMergeGlob struct {
	Enabled   bool `json:"enabled"`
	MaxPerDay int  `json:"max_per_day"`
}

// LegacySource is what broker.json used to hold (read once, for migration).
type LegacySource struct {
	ContextRepos   []string
	GitLabProjects map[string]string
}

// RegistryOptions are the seams of the state stores. OwnerOK reports whether a
// uid may own the state directory and files (nil: the current effective uid).
type RegistryOptions struct {
	OwnerOK func(uint32) bool
	// failAt, when set, is called at the named write stage ("temp-written",
	// "synced"); a non-nil result aborts the write there. Tests only.
	failAt func(stage string) error
	// beforeCreate runs after OpenRegistry saw no registry file and before it
	// creates one (simulates a file appearing late). Tests only.
	beforeCreate func()
	// Logf receives fixed, value-free log lines (nil: the standard logger).
	Logf func(format string, a ...any)
	// fileOwnerOK, when set, replaces OwnerOK for state files (not the dir). Tests only.
	fileOwnerOK func(uint32) bool
}

func (o RegistryOptions) ownerOK() func(uint32) bool {
	if o.OwnerOK != nil {
		return o.OwnerOK
	}
	euid := uint32(os.Geteuid())
	return func(uid uint32) bool { return uid == euid }
}

func (o RegistryOptions) fileOwner() func(uint32) bool {
	if o.fileOwnerOK != nil {
		return o.fileOwnerOK
	}
	return o.ownerOK()
}

// RegistryStore is the in-memory registry plus its file. Safe for concurrent use.
type RegistryStore struct {
	mu     sync.RWMutex
	path   string
	opts   RegistryOptions
	reg    Registry
	broken bool
}

// OpenRegistry opens (or, when the file is truly absent, creates by migrating
// legacy) the registry in dir. A file that cannot be trusted yields a store
// that refuses everything together with ErrRegistryBroken; the legacy source is
// never consulted in that case. Any other error is a start failure.
func OpenRegistry(dir string, legacy LegacySource, o RegistryOptions) (*RegistryStore, error) {
	st := &RegistryStore{path: filepath.Join(dir, RegistryFileName), opts: o}
	if err := ensureStateDir(dir, o.ownerOK()); err != nil {
		if errors.Is(err, errStateUntrusted) {
			st.broken = true
			return st, ErrRegistryBroken
		}
		return nil, err
	}
	sweepStaleTemps(dir)
	for attempt := 0; attempt < 2; attempt++ {
		data, exists, err := readStateFile(st.path, o.fileOwner())
		if err != nil {
			st.broken = true
			return st, ErrRegistryBroken
		}
		if exists {
			reg, migrated, err := parseRegistry(data)
			if err != nil {
				st.broken = true
				return st, ErrRegistryBroken
			}
			if migrated {
				// v1/v2 -> v3: same content, atomic replace (never exclusive:
				// the old file is the one being replaced). Any error is a start
				// failure. Per stage: before the rename (temp write, fsync) the
				// v1 file is untouched; after the rename (only the directory
				// fsync can still fail) the file is already v2 and the next
				// start reads it as a normal v2 file. There is no lock between
				// reading v1 and renaming v2: stop the old broker first.
				if _, werr := writeAtomic(st.path, mustMarshalRegistry(reg), o.failAt, false); werr != nil {
					return nil, errors.New("프로젝트 등록부를 저장할 수 없습니다")
				}
			}
			st.reg = reg
			return st, nil
		}
		reg, err := migrateLegacy(legacy)
		if err != nil {
			return nil, err
		}
		if o.beforeCreate != nil {
			o.beforeCreate()
		}
		// Exclusive create: never replaces a file that appeared after the
		// absence check. If one did, judge it by the normal load rules.
		_, err = writeAtomic(st.path, mustMarshalRegistry(reg), o.failAt, true)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, errors.New("프로젝트 등록부를 저장할 수 없습니다")
		}
		st.reg = reg
		return st, nil
	}
	return nil, errors.New("프로젝트 등록부를 저장할 수 없습니다")
}

// Err is ErrRegistryBroken when the registry cannot be trusted, else nil.
func (s *RegistryStore) Err() error {
	if s == nil || s.broken {
		return ErrRegistryBroken
	}
	return nil
}

// Snapshot returns a deep copy; a broken registry yields an empty one.
func (s *RegistryStore) Snapshot() Registry {
	if s.Err() != nil {
		return emptyRegistry()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRegistry(s.reg)
}

// Update runs fn on a copy under the write lock, validates the result, writes
// it atomically and only then makes it the live state. If fn, validation or the
// write fails, the live state is unchanged (rolled back) and the file keeps its
// previous content.
func (s *RegistryStore) Update(fn func(*Registry) error) error {
	if err := s.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneRegistry(s.reg)
	if err := fn(&next); err != nil {
		return err
	}
	if err := validateRegistry(&next); err != nil {
		return err
	}
	data := mustMarshalRegistry(next)
	if len(data) > maxStateFile {
		return errRegTooBig
	}
	prev := s.reg
	s.reg = next
	// A panic inside the write (only test hooks can cause one) must not leave
	// the memory ahead of the file: roll back, then let the panic go on.
	defer func() {
		if p := recover(); p != nil {
			s.reg = prev
			panic(p)
		}
	}()
	renamed, err := writeAtomic(s.path, data, s.opts.failAt, false)
	if err != nil {
		// If the rename already happened (only the directory fsync failed) the
		// new content is on disk: keep memory equal to disk and still report
		// the failure. Otherwise the file is untouched: roll back.
		if !renamed {
			s.reg = prev
		}
		return errors.New("프로젝트 등록부를 저장할 수 없습니다")
	}
	return nil
}

// AIContextPaths lists the paths of projects with ai_context true.
func (s *RegistryStore) AIContextPaths() []string {
	out := []string{}
	if s.Err() != nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.reg.Projects {
		if p.AIContext {
			out = append(out, p.Path)
		}
	}
	return out
}

// GitLabProject returns the GitLab project path registered for a repo path.
func (s *RegistryStore) GitLabProject(path string) (string, bool) {
	if s.Err() != nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.reg.Projects {
		// A provider-linked project (W3c) never takes the W2b create_mr path
		// (it would use broker.json's legacy gitlab section): W3d moves it.
		if p.Path == path && p.Remote != nil && p.Provider == nil {
			return p.Remote.Path, true
		}
	}
	return "", false
}

// GitLabPaths lists the repo paths that have a GitLab project, sorted.
func (s *RegistryStore) GitLabPaths() []string {
	out := []string{}
	if s.Err() != nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.reg.Projects {
		if p.Remote != nil && p.Provider == nil { // same rule as GitLabProject
			out = append(out, p.Path)
		}
	}
	sort.Strings(out)
	return out
}

// RegistryStatus inspects the registry in dir without writing or creating
// anything (for orch-broker -check): exists reports a present file, err is
// ErrRegistryBroken when it cannot be trusted. Absent dir or file is not an error.
func RegistryStatus(dir string, o RegistryOptions) (exists bool, err error) {
	if _, serr := os.Lstat(dir); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			return false, nil
		}
		return false, ErrRegistryBroken
	}
	if err := checkStateDir(dir, o.ownerOK()); err != nil {
		return false, ErrRegistryBroken
	}
	data, exists, rerr := readStateFile(filepath.Join(dir, RegistryFileName), o.fileOwner())
	if rerr != nil {
		return false, ErrRegistryBroken
	}
	if exists {
		if _, _, perr := parseRegistry(data); perr != nil {
			return true, ErrRegistryBroken
		}
	}
	return exists, nil
}

// MigrationDryRun builds the registry the legacy entries of cfg would become,
// in memory only (for orch-broker -check). ErrMigrationConflict when they
// cannot be turned into a valid registry.
func MigrationDryRun(cfg Config) error {
	_, err := migrateLegacy(LegacySource{ContextRepos: cfg.ContextRepos, GitLabProjects: cfg.GitLab.Projects})
	return err
}

func emptyRegistry() Registry {
	return Registry{Version: registryVersion, Providers: []Provider{}, Projects: []Project{},
		AutoMergeGlobal: AutoMergeGlob{Enabled: false, MaxPerDay: 5}, Orphans: []Orphan{}}
}

func cloneRegistry(r Registry) Registry {
	c := r
	c.Providers = append([]Provider{}, r.Providers...)
	c.Orphans = make([]Orphan, len(r.Orphans))
	for i, o := range r.Orphans {
		if o.ProjectID != nil {
			id := *o.ProjectID
			o.ProjectID = &id
		}
		c.Orphans[i] = o
	}
	c.Projects = make([]Project, len(r.Projects))
	for i, p := range r.Projects {
		if p.Remote != nil {
			g := *p.Remote
			if g.ID != nil {
				id := *g.ID
				g.ID = &id
			}
			p.Remote = &g
		}
		if p.Provider != nil {
			v := *p.Provider
			p.Provider = &v
		}
		c.Projects[i] = p
	}
	return c
}

func mustMarshalRegistry(r Registry) []byte {
	if r.Providers == nil {
		r.Providers = []Provider{}
	}
	if r.Projects == nil {
		r.Projects = []Project{}
	}
	if r.Orphans == nil {
		r.Orphans = []Orphan{}
	}
	b, err := json.Marshal(r)
	if err != nil { // plain structs: cannot happen
		panic("registry marshal")
	}
	return append(b, '\n')
}

// registryV1 is the exact version 1 file shape (W3a/W3b): no orphans, empty
// providers, no provider links, no GitLab ids.
type registryV1 struct {
	Version         int           `json:"version"`
	Providers       []struct{}    `json:"providers"`
	Projects        []projectV1   `json:"projects"`
	AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
}

type projectV1 struct {
	ID        string        `json:"id"`
	Path      string        `json:"path"`
	Kind      string        `json:"kind"`
	AIContext bool          `json:"ai_context"`
	Provider  *struct{}     `json:"provider"`
	GitLab    *gitlabLinkV2 `json:"gitlab"`
	AutoMerge AutoMerge     `json:"auto_merge"`
}

// registryV2 is the exact version 2 file shape (W3c): gitlab links, provider
// kind gitlab only.
type registryV2 struct {
	Version         int           `json:"version"`
	Providers       []Provider    `json:"providers"`
	Projects        []projectV2   `json:"projects"`
	AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
	Orphans         []Orphan      `json:"orphans"`
}

type projectV2 struct {
	ID        string        `json:"id"`
	Path      string        `json:"path"`
	Kind      string        `json:"kind"`
	AIContext bool          `json:"ai_context"`
	Provider  *string       `json:"provider"`
	GitLab    *gitlabLinkV2 `json:"gitlab"`
	AutoMerge AutoMerge     `json:"auto_merge"`
}

// topLevelKeys returns the raw top-level values of a JSON object. It refuses
// anything that the strict struct decoder could read differently from the
// version dispatch: a repeated key, or two keys that differ only in case (Go's
// decoder matches struct fields case-insensitively), and trailing data.
func topLevelKeys(data []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	out := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		key, isStr := kt.(string)
		if err != nil || !isStr {
			return nil, false
		}
		folded := strings.ToLower(key)
		if seen[folded] {
			return nil, false
		}
		seen[folded] = true
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return nil, false
		}
		out[key] = raw
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return out, true
}

// parseRegistry decodes and validates a registry file. Only versions 1, 2 and
// 3 are known; all are decoded strictly (unknown keys, trailing data and wrong
// types are refused). migrated reports that data was a valid version 1 or 2
// file that was converted to the version 3 shape in memory (content and
// meaning unchanged): the caller rewrites it.
func parseRegistry(data []byte) (r Registry, migrated bool, err error) {
	head, ok := topLevelKeys(data)
	if !ok {
		return Registry{}, false, ErrRegistryBroken
	}
	strict := func(v any) bool {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if dec.Decode(v) != nil {
			return false
		}
		_, terr := dec.Token()
		return terr == io.EOF
	}
	switch string(head["version"]) {
	case "3":
		if !strict(&r) || r.Version != registryVersion {
			return Registry{}, false, ErrRegistryBroken
		}
		if r.Providers == nil {
			r.Providers = []Provider{}
		}
		if r.Projects == nil {
			r.Projects = []Project{}
		}
		if r.Orphans == nil {
			r.Orphans = []Orphan{}
		}
	case "2":
		var v2 registryV2
		if !strict(&v2) || v2.Version != registryVersionV2 {
			return Registry{}, false, ErrRegistryBroken
		}
		r = Registry{Version: registryVersion, Providers: v2.Providers, AutoMergeGlobal: v2.AutoMergeGlobal, Orphans: v2.Orphans}
		for _, p := range v2.Providers {
			if p.Kind != "gitlab" { // the only kind a version 2 file can hold
				return Registry{}, false, ErrRegistryBroken
			}
		}
		r.Projects = []Project{}
		for _, p := range v2.Projects {
			r.Projects = append(r.Projects, Project{ID: p.ID, Path: p.Path, Kind: p.Kind, AIContext: p.AIContext, Provider: p.Provider,
				Remote: p.GitLab.toRemote(), AutoMerge: p.AutoMerge})
		}
		if r.Providers == nil {
			r.Providers = []Provider{}
		}
		if r.Orphans == nil {
			r.Orphans = []Orphan{}
		}
		migrated = true
	case "1":
		var v1 registryV1
		if !strict(&v1) || v1.Version != registryVersionV1 || len(v1.Providers) != 0 {
			return Registry{}, false, ErrRegistryBroken
		}
		r = emptyRegistry()
		r.AutoMergeGlobal = v1.AutoMergeGlobal
		for _, p := range v1.Projects {
			if p.Provider != nil || (p.GitLab != nil && p.GitLab.ProjectID != nil) {
				return Registry{}, false, ErrRegistryBroken
			}
			r.Projects = append(r.Projects, Project{ID: p.ID, Path: p.Path, Kind: p.Kind, AIContext: p.AIContext, Remote: p.GitLab.toRemote(), AutoMerge: p.AutoMerge})
		}
		migrated = true
	default:
		return Registry{}, false, ErrRegistryBroken
	}
	if err := validateRegistry(&r); err != nil {
		return Registry{}, false, ErrRegistryBroken
	}
	return r, migrated, nil
}

// Validation errors are fixed sentences without values.
var (
	errRegVersion  = errors.New("등록부 버전이 올바르지 않습니다")
	errRegProject  = errors.New("등록부의 프로젝트 항목이 올바르지 않습니다")
	errRegDupID    = errors.New("등록부에 같은 프로젝트 id가 중복되어 있습니다")
	errRegDupPath  = errors.New("등록부에 같은 경로가 중복되어 있습니다")
	errRegAutoOff  = errors.New("자동 병합은 이 버전에서 켤 수 없습니다")
	errRegTooBig   = errors.New("등록부가 너무 커서 저장할 수 없습니다")
	errRegProvider = errors.New("제공자(provider) 항목이 올바르지 않습니다")
	errRegOrphan   = errors.New("등록부의 고아 기록이 올바르지 않습니다")
)

func validateRegistry(r *Registry) error {
	if r.Version != registryVersion {
		return errRegVersion
	}
	if len(r.Providers) > maxProviders || len(r.Orphans) > maxOrphans {
		return errRegProvider
	}
	providers := map[string]bool{}
	for _, p := range r.Providers {
		if !registrymsg.ValidProviderID(p.ID) || !registrymsg.ValidProviderKind(p.Kind) || !registrymsg.ValidBaseURL(p.BaseURL) ||
			!registrymsg.ValidSSHURL(p.SSHURL) || !registrymsg.ValidTokenRef(p.TokenRef) || providers[p.ID] {
			return errRegProvider
		}
		providers[p.ID] = true
	}
	for _, o := range r.Orphans {
		if !validOrphan(o) {
			return errRegOrphan
		}
	}
	if r.AutoMergeGlobal.Enabled {
		return errRegAutoOff
	}
	if r.AutoMergeGlobal.MaxPerDay < 1 || r.AutoMergeGlobal.MaxPerDay > 1000 {
		return errRegProject
	}
	if len(r.Projects) > maxProjects {
		return errRegProject
	}
	ids := map[string]bool{}
	paths := map[string]bool{}
	for _, p := range r.Projects {
		if len(p.Path) > maxPathBytes || !projectIDRE.MatchString(p.ID) || !projectKindRE.MatchString(p.Kind) || !cleanAbs(p.Path) {
			return errRegProject
		}
		if p.Provider != nil && !providers[*p.Provider] {
			return errRegProvider // unknown provider id (this also keeps a referenced provider from being removed)
		}
		if p.AutoMerge.Enabled {
			return errRegAutoOff
		}
		if p.Remote != nil {
			g := p.Remote
			if !projectRE.MatchString(g.Path) || strings.Contains("/"+g.Path+"/", "/../") ||
				strings.Contains("/"+g.Path+"/", "/./") {
				return errRegProject
			}
			// A repository id belongs to a provider. A link without a provider is
			// only the legacy form (migrated from broker.json or a v1 file).
			if g.ID != nil && (p.Provider == nil || *g.ID <= 0) {
				return errRegProject
			}
		}
		if ids[p.ID] {
			return errRegDupID
		}
		if paths[p.Path] {
			return errRegDupPath
		}
		ids[p.ID] = true
		paths[p.Path] = true
	}
	return nil
}

// validOrphan: values only, every field in a fixed shape. The provider id need
// not be configured any more (the record outlives set_provider/delete_provider).
func validOrphan(o Orphan) bool {
	if !registrymsg.ValidProviderID(o.Provider) || !orphanStepRE.MatchString(o.Step) {
		return false
	}
	// A GitLab project (group path + lowercase name, up to five segments) or a
	// GitHub/GitLab remote path as link_project and the GitHub create flow
	// write it (owner/repo, case kept).
	i := strings.LastIndexByte(o.Project, '/')
	gitlabShape := i >= 0 && registrymsg.ValidGroupPath(o.Project[:i]) && registrymsg.ValidName(o.Project[i+1:]) &&
		strings.Count(o.Project, "/")+1 <= registrymsg.MaxGroupDepth+1
	if !gitlabShape && !registrymsg.ValidRemotePath(o.Project) {
		return false
	}
	if o.ProjectID != nil && *o.ProjectID <= 0 {
		return false
	}
	t, err := time.Parse(orphanTimeLayout, o.At)
	return err == nil && t.Format(orphanTimeLayout) == o.At
}

func cleanAbs(p string) bool {
	return p != "/" && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsAny(p, "\x00\n\r")
}

// migrateLegacy builds the first registry from the old broker.json entries.
// Every context_repos entry becomes ai_context:true, every gitlab.projects
// entry its GitLab link (a mapped repo missing from context_repos is added with
// ai_context:false). An id clash refuses the migration.
func migrateLegacy(l LegacySource) (Registry, error) {
	reg := emptyRegistry()
	index := map[string]int{}
	add := func(path string) (int, error) {
		if i, ok := index[path]; ok {
			return i, nil
		}
		if !cleanAbs(path) {
			return 0, ErrMigrationConflict
		}
		reg.Projects = append(reg.Projects, Project{ID: legacyID(path), Path: path, Kind: legacyKind})
		index[path] = len(reg.Projects) - 1
		return index[path], nil
	}
	for _, p := range l.ContextRepos {
		i, err := add(p)
		if err != nil {
			return Registry{}, err
		}
		reg.Projects[i].AIContext = true
	}
	mapped := make([]string, 0, len(l.GitLabProjects))
	for p := range l.GitLabProjects {
		mapped = append(mapped, p)
	}
	sort.Strings(mapped)
	for _, p := range mapped {
		i, err := add(p)
		if err != nil {
			return Registry{}, err
		}
		reg.Projects[i].Remote = &RemoteLink{Path: l.GitLabProjects[p]}
	}
	if err := validateRegistry(&reg); err != nil {
		return Registry{}, ErrMigrationConflict
	}
	return reg, nil
}

// legacyID derives a project id from the last path element.
func legacyID(path string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(filepath.Base(path)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	s := strings.Trim(sb.String(), "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = strings.Trim("p-"+s, "-")
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// --- state directory and file trust, atomic write (shared by usedreq.go) ---

var errStateUntrusted = errors.New("상태 폴더 또는 파일을 신뢰할 수 없습니다")

// ensureStateDir creates the directory 0700 when it is missing and requires it
// to be a real directory owned by the broker account with no group/other access.
func ensureStateDir(dir string, ownerOK func(uint32) bool) error {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return errors.New("상태 폴더를 준비할 수 없습니다")
		}
	}
	return checkStateDir(dir, ownerOK)
}

func checkStateDir(dir string, ownerOK func(uint32) bool) error {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return errStateUntrusted
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !ownerOK(st.Uid) || fi.Mode().Perm()&0o077 != 0 {
		return errStateUntrusted
	}
	return nil
}

// readStateFile reads a state file. exists is false only when the file is
// truly missing; every other problem (not regular, symlink, wrong owner or
// mode, too large, unreadable, changed while reading) is an error.
func readStateFile(path string, ownerOK func(uint32) bool) (data []byte, exists bool, err error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, errStateUntrusted
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || !ownerOK(st.Uid) || fi.Mode().Perm() != 0o600 || fi.Size() > maxStateFile {
		return nil, true, errStateUntrusted
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, true, errStateUntrusted
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil || !os.SameFile(fi, fi2) {
		return nil, true, errStateUntrusted
	}
	data, err = io.ReadAll(io.LimitReader(f, maxStateFile+1))
	if err != nil || len(data) > maxStateFile {
		return nil, true, errStateUntrusted
	}
	return data, true, nil
}

// sweepStaleTemps removes leftover temp files of interrupted writes: plain
// files named .state-* older than a minute (younger ones may belong to a
// concurrent writer). Symlinks and directories are never touched. Errors are
// ignored.
func sweepStaleTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".state-") || !e.Type().IsRegular() {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Minute {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// writeAtomic is the only file-writing routine of the state stores: temp file
// in the same directory (0600), write, fsync, then either rename over the
// target or, with exclusive, os.Link (fails with os.ErrExist instead of
// replacing an existing target), then fsync of the directory. renamed reports
// that the target now holds the new content; on err with renamed false the old
// file is untouched and the temp file is removed. err with renamed true means
// only the directory fsync failed (content is on disk, durability unconfirmed).
func writeAtomic(path string, data []byte, failAt func(string) error, exclusive bool) (renamed bool, err error) {
	return writeAtomicMode(path, data, 0o600, failAt, exclusive)
}

// writeAtomicMode is writeAtomic with the final file mode chosen by the caller
// (secret files are 0400). The temp file gets that mode before any data is
// written; the open descriptor keeps write access.
func writeAtomicMode(path string, data []byte, mode os.FileMode, failAt func(string) error, exclusive bool) (renamed bool, err error) {
	stage := func(name string) error {
		if failAt != nil {
			return failAt(name)
		}
		return nil
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name) // after a successful rename the name is gone: harmless
	}()
	if err := tmp.Chmod(mode); err != nil {
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		return false, err
	}
	if err := stage("temp-written"); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := stage("synced"); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if exclusive {
		if err := os.Link(name, path); err != nil {
			return false, err
		}
	} else if err := os.Rename(name, path); err != nil {
		return false, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return true, errors.New("상태 폴더를 열 수 없습니다")
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return true, errors.New("상태 폴더를 동기화할 수 없습니다")
	}
	if err := stage("dir-synced"); err != nil {
		return true, err
	}
	return true, nil
}
