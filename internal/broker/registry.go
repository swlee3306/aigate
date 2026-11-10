package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The project registry (decision D1) is owned by the broker: one JSON file in
// the 0700 state directory, 0600, written atomically. The broker is the only
// writer, and this file holds the only write code (writeAtomic is also used by
// the used-request store).
//
// Fail closed: only a truly missing file starts a fresh registry (and migrates
// the legacy broker.json entries once). A file that is broken, unknown-keyed,
// of another version, not owned by the broker account or not 0600 is never
// treated as "absent": the broker keeps running but every read and write that
// depends on the registry answers registry_unavailable.

const (
	// RegistryFileName and UsedRequestsFileName live in the state directory.
	RegistryFileName     = "registry.json"
	UsedRequestsFileName = "used-requests.json"

	registryVersion = 1
	maxStateFile    = 1 << 20
	maxProjects     = 500
	maxPathBytes    = 4096
)

var (
	// ErrRegistryBroken: the registry file exists but cannot be trusted.
	ErrRegistryBroken = errors.New("프로젝트 등록부를 사용할 수 없습니다")
	// ErrMigrationConflict: legacy entries cannot be turned into a valid registry.
	ErrMigrationConflict = errors.New("옛 설정을 등록부로 옮길 수 없습니다(프로젝트 id 충돌 또는 형식 오류)")

	projectIDRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	projectKindRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// legacyKind is the policy class name given to projects migrated from
// broker.json (the old config knows no classes).
const legacyKind = "legacy"

// Registry is the D1 shape.
type Registry struct {
	Version         int           `json:"version"`
	Providers       []struct{}    `json:"providers"`
	Projects        []Project     `json:"projects"`
	AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
}

// Project is one registered repository. Provider is a placeholder that W3a
// keeps null; GitLab carries the project path used by create_mr.
type Project struct {
	ID        string      `json:"id"`
	Path      string      `json:"path"`
	Kind      string      `json:"kind"`
	AIContext bool        `json:"ai_context"`
	Provider  *struct{}   `json:"provider"`
	GitLab    *GitLabLink `json:"gitlab"`
	AutoMerge AutoMerge   `json:"auto_merge"`
}

// GitLabLink is the project's GitLab name; ProjectID is a W3b+ placeholder.
type GitLabLink struct {
	Project   string `json:"project"`
	ProjectID *int64 `json:"project_id"`
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
			reg, err := parseRegistry(data)
			if err != nil {
				st.broken = true
				return st, ErrRegistryBroken
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
		if p.Path == path && p.GitLab != nil {
			return p.GitLab.Project, true
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
		if p.GitLab != nil {
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
		if _, perr := parseRegistry(data); perr != nil {
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
	return Registry{Version: registryVersion, Providers: []struct{}{}, Projects: []Project{},
		AutoMergeGlobal: AutoMergeGlob{Enabled: false, MaxPerDay: 5}}
}

func cloneRegistry(r Registry) Registry {
	c := r
	c.Providers = append([]struct{}{}, r.Providers...)
	c.Projects = make([]Project, len(r.Projects))
	for i, p := range r.Projects {
		if p.GitLab != nil {
			g := *p.GitLab
			if g.ProjectID != nil {
				id := *g.ProjectID
				g.ProjectID = &id
			}
			p.GitLab = &g
		}
		if p.Provider != nil {
			p.Provider = &struct{}{}
		}
		c.Projects[i] = p
	}
	return c
}

func mustMarshalRegistry(r Registry) []byte {
	if r.Providers == nil {
		r.Providers = []struct{}{}
	}
	if r.Projects == nil {
		r.Projects = []Project{}
	}
	b, err := json.Marshal(r)
	if err != nil { // plain structs: cannot happen
		panic("registry marshal")
	}
	return append(b, '\n')
}

func parseRegistry(data []byte) (Registry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Registry
	if err := dec.Decode(&r); err != nil {
		return Registry{}, ErrRegistryBroken
	}
	if _, err := dec.Token(); err != io.EOF {
		return Registry{}, ErrRegistryBroken
	}
	if r.Providers == nil {
		r.Providers = []struct{}{}
	}
	if r.Projects == nil {
		r.Projects = []Project{}
	}
	if err := validateRegistry(&r); err != nil {
		return Registry{}, ErrRegistryBroken
	}
	return r, nil
}

// Validation errors are fixed sentences without values.
var (
	errRegVersion  = errors.New("등록부 버전이 올바르지 않습니다")
	errRegProject  = errors.New("등록부의 프로젝트 항목이 올바르지 않습니다")
	errRegDupID    = errors.New("등록부에 같은 프로젝트 id가 중복되어 있습니다")
	errRegDupPath  = errors.New("등록부에 같은 경로가 중복되어 있습니다")
	errRegAutoOff  = errors.New("자동 병합은 이 버전에서 켤 수 없습니다")
	errRegTooBig   = errors.New("등록부가 너무 커서 저장할 수 없습니다")
	errRegProvider = errors.New("제공자(provider) 항목은 이 버전에서 비어 있어야 합니다")
)

func validateRegistry(r *Registry) error {
	if r.Version != registryVersion {
		return errRegVersion
	}
	if len(r.Providers) != 0 {
		return errRegProvider
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
		if p.Provider != nil {
			return errRegProvider
		}
		if p.AutoMerge.Enabled {
			return errRegAutoOff
		}
		if p.GitLab != nil {
			g := p.GitLab
			if !projectRE.MatchString(g.Project) || strings.Contains("/"+g.Project+"/", "/../") ||
				strings.Contains("/"+g.Project+"/", "/./") || g.ProjectID != nil {
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
		reg.Projects[i].GitLab = &GitLabLink{Project: l.GitLabProjects[p]}
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
