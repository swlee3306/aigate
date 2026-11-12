package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func anyOwner(uint32) bool { return true }

func regOpts() RegistryOptions { return RegistryOptions{OwnerOK: anyOwner} }

// stateDirFor returns a fresh 0700 state dir path (not yet created).
func stateDirFor(t *testing.T) string { return filepath.Join(shortTempDir(t), "state") }

func legacyFor(dir string) LegacySource {
	return LegacySource{
		ContextRepos:   []string{dir + "/repo-a", dir + "/Repo_B"},
		GitLabProjects: map[string]string{dir + "/repo-a": "grp/proj", dir + "/gl-only": "grp/other"},
	}
}

func mustOpen(t *testing.T, dir string, l LegacySource) *RegistryStore {
	t.Helper()
	st, err := OpenRegistry(dir, l, regOpts())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	return st
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRegistryFreshStartMigratesLegacyConfig(t *testing.T) {
	dir := stateDirFor(t)
	l := legacyFor("/srv")
	st := mustOpen(t, dir, l)
	reg := st.Snapshot()
	if reg.Version != 3 || len(reg.Providers) != 0 || reg.AutoMergeGlobal.Enabled || reg.AutoMergeGlobal.MaxPerDay != 5 {
		t.Fatalf("shape %+v", reg)
	}
	got := st.AIContextPaths()
	if strings.Join(got, ",") != strings.Join(l.ContextRepos, ",") {
		t.Fatalf("ai_context paths %v want %v", got, l.ContextRepos)
	}
	if p, ok := st.GitLabProject("/srv/repo-a"); !ok || p != "grp/proj" {
		t.Fatalf("gitlab project %q %v", p, ok)
	}
	// a mapped repo missing from context_repos is added with ai_context:false
	if p, ok := st.GitLabProject("/srv/gl-only"); !ok || p != "grp/other" {
		t.Fatalf("gl-only %q %v", p, ok)
	}
	for _, p := range reg.Projects {
		if p.Path == "/srv/gl-only" && p.AIContext {
			t.Fatal("gitlab-only project must not get ai_context")
		}
		if p.Provider != nil || p.AutoMerge.Enabled || (p.Remote != nil && p.Remote.ID != nil) {
			t.Fatalf("placeholders must be empty: %+v", p)
		}
	}
	ids := map[string]string{}
	for _, p := range reg.Projects {
		ids[p.Path] = p.ID
	}
	if ids["/srv/Repo_B"] != "repo-b" || ids["/srv/repo-a"] != "repo-a" {
		t.Fatalf("ids %v", ids)
	}
	// persisted: a second open (no legacy at all) sees the same registry
	st2 := mustOpen(t, dir, LegacySource{})
	if strings.Join(st2.AIContextPaths(), ",") != strings.Join(l.ContextRepos, ",") {
		t.Fatal("migration was not persisted")
	}
	// the file is the D1 shape
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(readFile(t, filepath.Join(dir, RegistryFileName)), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "providers", "projects", "auto_merge_global"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("file lacks %s", k)
		}
	}
	if string(raw["providers"]) != "[]" {
		t.Fatalf("providers %s", raw["providers"])
	}
}

func TestRegistryMigrationRefusesIDClash(t *testing.T) {
	dir := stateDirFor(t)
	_, err := OpenRegistry(dir, LegacySource{ContextRepos: []string{"/a/Web", "/b/web"}}, regOpts())
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("err %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, RegistryFileName)); serr == nil {
		t.Fatal("a registry was written despite the clash")
	}
}

func TestLegacyIDDerivation(t *testing.T) {
	for in, want := range map[string]string{
		"/x/My_Repo.v2": "my-repo-v2", "/x/123abc": "p-123abc", "/x/---": "p", "/x/Repo": "repo",
		"/x/" + strings.Repeat("a", 60): strings.Repeat("a", 40),
	} {
		if got := legacyID(in); got != want || !projectIDRE.MatchString(got) {
			t.Errorf("legacyID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRegistryExistingWinsOverLegacy(t *testing.T) {
	dir := stateDirFor(t)
	mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/one"}})
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/two", "/srv/three"}, GitLabProjects: map[string]string{"/srv/two": "g/p"}})
	if got := st.AIContextPaths(); len(got) != 1 || got[0] != "/srv/one" {
		t.Fatalf("legacy changes leaked in: %v", got)
	}
	if _, ok := st.GitLabProject("/srv/two"); ok {
		t.Fatal("legacy gitlab mapping leaked in")
	}
}

func TestRegistryBrokenIsFailClosedNotMigrated(t *testing.T) {
	good := string(mustMarshalRegistry(emptyRegistry()))
	cases := map[string]struct {
		content string
		mode    os.FileMode
		owner   func(uint32) bool
		symlink bool
	}{
		"broken json":      {content: `{"version":1,`, mode: 0o600},
		"empty file":       {content: ``, mode: 0o600},
		"unknown key":      {content: strings.Replace(good, `"version":3`, `"version":3,"extra":true`, 1), mode: 0o600},
		"version mismatch": {content: strings.Replace(good, `"version":3`, `"version":4`, 1), mode: 0o600},
		"trailing data":    {content: good + `{}`, mode: 0o600},
		"duplicate id":     {content: `{"version":1,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k"},{"id":"a","path":"/x/b","kind":"k"}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`, mode: 0o600},
		"auto merge on":    {content: `{"version":1,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","auto_merge":{"enabled":true}}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`, mode: 0o600},
		"mode 0644":        {content: good, mode: 0o644},
		"mode 0660":        {content: good, mode: 0o660},
		"foreign owner":    {content: good, mode: 0o600, owner: func(uint32) bool { return false }},
		"symlink":          {content: good, mode: 0o600, symlink: true},
	}
	legacy := LegacySource{ContextRepos: []string{"/srv/wide-open"}, GitLabProjects: map[string]string{"/srv/wide-open": "g/p"}}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := stateDirFor(t)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, RegistryFileName)
			if c.symlink {
				target := filepath.Join(dir, "real.json")
				os.WriteFile(target, []byte(c.content), c.mode)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte(c.content), c.mode); err != nil {
					t.Fatal(err)
				}
				os.Chmod(path, c.mode)
			}
			before := readFile(t, path)
			o := regOpts()
			if c.owner != nil {
				o.OwnerOK = c.owner
			}
			st, err := OpenRegistry(dir, legacy, o)
			if !errors.Is(err, ErrRegistryBroken) || st == nil {
				t.Fatalf("err=%v store=%v", err, st)
			}
			if st.Err() == nil || len(st.AIContextPaths()) != 0 || len(st.Snapshot().Projects) != 0 {
				t.Fatal("broken registry served data (fell back to legacy?)")
			}
			if _, ok := st.GitLabProject("/srv/wide-open"); ok {
				t.Fatal("gitlab mapping served from a broken registry")
			}
			if uerr := st.Update(func(r *Registry) error { return nil }); !errors.Is(uerr, ErrRegistryBroken) {
				t.Fatalf("Update on broken registry: %v", uerr)
			}
			if !bytes.Equal(before, readFile(t, path)) {
				t.Fatal("broken registry file was modified")
			}
		})
	}
}

func TestRegistryBadStateDirIsBroken(t *testing.T) {
	dir := stateDirFor(t)
	os.MkdirAll(dir, 0o755)
	os.Chmod(dir, 0o755)
	st, err := OpenRegistry(dir, LegacySource{ContextRepos: []string{"/srv/a"}}, regOpts())
	if !errors.Is(err, ErrRegistryBroken) || len(st.AIContextPaths()) != 0 {
		t.Fatalf("err=%v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, RegistryFileName)); serr == nil {
		t.Fatal("registry created in an untrusted state dir")
	}
}

func TestRegistryAtomicWriteSurvivesCrash(t *testing.T) {
	dir := stateDirFor(t)
	l := LegacySource{ContextRepos: []string{"/srv/a"}}
	st := mustOpen(t, dir, l)
	path := filepath.Join(dir, RegistryFileName)
	before := readFile(t, path)
	snap := st.Snapshot()
	add := func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: "new", Path: "/srv/new", Kind: "k", AIContext: true})
		return nil
	}
	for _, stage := range []string{"temp-written", "synced"} {
		st.opts.failAt = func(s string) error {
			if s == stage {
				return errors.New("injected")
			}
			return nil
		}
		if err := st.Update(add); err == nil {
			t.Fatalf("%s: update succeeded despite injected failure", stage)
		}
		if !bytes.Equal(before, readFile(t, path)) {
			t.Fatalf("%s: file changed", stage)
		}
		if got := st.Snapshot(); len(got.Projects) != len(snap.Projects) {
			t.Fatalf("%s: memory not rolled back: %+v", stage, got.Projects)
		}
		if len(st.AIContextPaths()) != 1 {
			t.Fatalf("%s: AIContextPaths shows the failed change", stage)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != RegistryFileName {
				t.Fatalf("%s: leftover %s", stage, e.Name())
			}
		}
	}
	st.opts.failAt = nil
	if err := st.Update(add); err != nil {
		t.Fatalf("update after failures: %v", err)
	}
	// and a restart sees the committed change only
	if got := mustOpen(t, dir, LegacySource{}).AIContextPaths(); len(got) != 2 {
		t.Fatalf("after restart: %v", got)
	}
}

func TestRegistryUpdateErrorKeepsStateAndFile(t *testing.T) {
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/a"}})
	before := readFile(t, filepath.Join(dir, RegistryFileName))
	boom := errors.New("callback")
	if err := st.Update(func(r *Registry) error { r.Projects[0].AIContext = false; return boom }); err != boom {
		t.Fatalf("err %v", err)
	}
	if len(st.AIContextPaths()) != 1 || !bytes.Equal(before, readFile(t, filepath.Join(dir, RegistryFileName))) {
		t.Fatal("failed callback leaked into state")
	}
}

func TestRegistryRejectsInvalidProjects(t *testing.T) {
	base := Project{ID: "ok", Path: "/srv/ok", Kind: "k"}
	gl := func(p string) *RemoteLink { return &RemoteLink{Path: p} }
	id := int64(1)
	cases := map[string]Project{
		"empty id":          {ID: "", Path: "/srv/x", Kind: "k"},
		"uppercase id":      {ID: "Abc", Path: "/srv/x", Kind: "k"},
		"digit-first id":    {ID: "1abc", Path: "/srv/x", Kind: "k"},
		"underscore id":     {ID: "a_b", Path: "/srv/x", Kind: "k"},
		"41-char id":        {ID: "a" + strings.Repeat("b", 40), Path: "/srv/x", Kind: "k"},
		"duplicate id":      {ID: "ok", Path: "/srv/x", Kind: "k"},
		"duplicate path":    {ID: "x", Path: "/srv/ok", Kind: "k"},
		"relative path":     {ID: "x", Path: "srv/x", Kind: "k"},
		"dotdot path":       {ID: "x", Path: "/srv/a/../x", Kind: "k"},
		"trailing slash":    {ID: "x", Path: "/srv/x/", Kind: "k"},
		"double slash":      {ID: "x", Path: "/srv//x", Kind: "k"},
		"root path":         {ID: "x", Path: "/", Kind: "k"},
		"newline in path":   {ID: "x", Path: "/srv/x\n", Kind: "k"},
		"bad kind":          {ID: "x", Path: "/srv/x", Kind: "Bad Kind"},
		"empty kind":        {ID: "x", Path: "/srv/x"},
		"auto merge on":     {ID: "x", Path: "/srv/x", Kind: "k", AutoMerge: AutoMerge{Enabled: true}},
		"provider set":      {ID: "x", Path: "/srv/x", Kind: "k", Provider: strp("gl")}, // no such provider in the registry
		"gitlab dotdot":     {ID: "x", Path: "/srv/x", Kind: "k", Remote: gl("a/../b")},
		"gitlab one part":   {ID: "x", Path: "/srv/x", Kind: "k", Remote: gl("solo")},
		"gitlab project id": {ID: "x", Path: "/srv/x", Kind: "k", Remote: &RemoteLink{Path: "a/b", ID: &id}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			dir := stateDirFor(t)
			st := mustOpen(t, dir, LegacySource{})
			if err := st.Update(func(r *Registry) error { r.Projects = append(r.Projects, base); return nil }); err != nil {
				t.Fatal(err)
			}
			before := readFile(t, filepath.Join(dir, RegistryFileName))
			if err := st.Update(func(r *Registry) error { r.Projects = append(r.Projects, p); return nil }); err == nil {
				t.Fatal("invalid project accepted")
			}
			if !bytes.Equal(before, readFile(t, filepath.Join(dir, RegistryFileName))) || len(st.Snapshot().Projects) != 1 {
				t.Fatal("invalid project changed state")
			}
		})
	}
	t.Run("global auto merge on", func(t *testing.T) {
		st := mustOpen(t, stateDirFor(t), LegacySource{})
		if err := st.Update(func(r *Registry) error { r.AutoMergeGlobal.Enabled = true; return nil }); err == nil {
			t.Fatal("global auto merge accepted")
		}
	})
	t.Run("invalid provider entry", func(t *testing.T) {
		st := mustOpen(t, stateDirFor(t), LegacySource{})
		if err := st.Update(func(r *Registry) error { r.Providers = append(r.Providers, Provider{}); return nil }); err == nil {
			t.Fatal("provider accepted")
		}
	})
	t.Run("valid project with gitlab", func(t *testing.T) {
		st := mustOpen(t, stateDirFor(t), LegacySource{})
		if err := st.Update(func(r *Registry) error {
			r.Projects = append(r.Projects, Project{ID: "good-1", Path: "/srv/good", Kind: "registered", AIContext: true, Remote: gl("grp/sub/proj")})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRegistryFileMode0600(t *testing.T) {
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/a"}})
	path := filepath.Join(dir, RegistryFileName)
	check := func(when string) {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: registry mode %v err %v", when, fi.Mode(), err)
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("%s: state dir mode %v", when, di.Mode().Perm())
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("%s: stray files %v", when, entries)
		}
	}
	check("after migration")
	if err := st.Update(func(r *Registry) error { r.Projects[0].AIContext = false; return nil }); err != nil {
		t.Fatal(err)
	}
	check("after update")
}

func TestRegistryConcurrentUpdates(t *testing.T) {
	st := mustOpen(t, stateDirFor(t), LegacySource{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := Project{ID: "p" + strings.Repeat("a", i), Path: "/srv/p" + strings.Repeat("a", i), Kind: "k"}
			if err := st.Update(func(r *Registry) error { r.Projects = append(r.Projects, p); return nil }); err != nil {
				t.Error(err)
			}
			st.Snapshot()
		}(i)
	}
	wg.Wait()
	if n := len(st.Snapshot().Projects); n != 20 {
		t.Fatalf("projects %d", n)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	st := mustOpen(t, stateDirFor(t), LegacySource{ContextRepos: []string{"/srv/a"}, GitLabProjects: map[string]string{"/srv/a": "g/p"}})
	s := st.Snapshot()
	s.Projects[0].AIContext = false
	s.Projects[0].Remote.Path = "evil/x"
	if len(st.AIContextPaths()) != 1 {
		t.Fatal("snapshot mutation reached the store")
	}
	if p, _ := st.GitLabProject("/srv/a"); p != "g/p" {
		t.Fatal("snapshot gitlab pointer is shared")
	}
}

func TestRegistryStatusIsReadOnly(t *testing.T) {
	dir := stateDirFor(t)
	if ex, err := RegistryStatus(dir, regOpts()); ex || err != nil {
		t.Fatalf("absent dir: %v %v", ex, err)
	}
	if _, serr := os.Stat(dir); serr == nil {
		t.Fatal("RegistryStatus created the dir")
	}
	mustOpen(t, dir, LegacySource{})
	if ex, err := RegistryStatus(dir, regOpts()); !ex || err != nil {
		t.Fatalf("present: %v %v", ex, err)
	}
	os.WriteFile(filepath.Join(dir, RegistryFileName), []byte("junk"), 0o600)
	if _, err := RegistryStatus(dir, regOpts()); !errors.Is(err, ErrRegistryBroken) {
		t.Fatalf("broken: %v", err)
	}
}

// --- wiring into the W2b ops ---

func TestRefineUsesRegistryAllowList(t *testing.T) {
	fake := newFakeHTTP(t, okHandler(goodPlan, 10, 10))
	r := newRefineEnv(t, fake, nil)
	if _, oerr := r.call(t, r.args()); oerr != nil {
		t.Fatalf("allowed repo refused: %s", oerr.Code)
	}
	hits := fake.Hits()
	// turn ai_context off in the registry: refine must refuse
	if err := r.srv.Registry.Update(func(g *Registry) error { g.Projects[0].AIContext = false; return nil }); err != nil {
		t.Fatal(err)
	}
	_, oerr := r.call(t, r.args())
	wantCode(t, oerr, "repo_not_allowed")
	if fake.Hits() != hits {
		t.Fatal("AI endpoint called for a repo whose ai_context is off")
	}
	// editing the legacy config value in memory changes nothing: the registry wins
	r.srv.Config.ContextRepos = []string{r.repo}
	_, oerr = r.call(t, r.args())
	wantCode(t, oerr, "repo_not_allowed")
	// and back on
	r.srv.Registry.Update(func(g *Registry) error { g.Projects[0].AIContext = true; return nil })
	if _, oerr := r.call(t, r.args()); oerr != nil {
		t.Fatalf("re-enabled repo refused: %s", oerr.Code)
	}
}

func TestCreateMRUsesRegistryGitLabProject(t *testing.T) {
	m := newMREnv(t, nil)
	if _, oerr := m.call(t, m.args()); oerr != nil {
		t.Fatalf("err %s", oerr.Code)
	}
	if ps := m.posts(); len(ps) != 1 || !strings.Contains(ps[0].Path, "/projects/grp%2Fproj/") {
		t.Fatalf("posts %+v", ps)
	}
	// the registry's GitLab name is the one used
	m.srv.Registry.Update(func(g *Registry) error { g.Projects[0].Remote.Path = "other/name"; return nil })
	m.srv.Config.GitLab.Projects = map[string]string{m.repo: "grp/proj"} // legacy value must be ignored
	m.gl.mu.Lock()
	m.gl.reqs = nil
	m.gl.mu.Unlock()
	if _, oerr := m.call(t, m.args()); oerr != nil {
		t.Fatalf("err %s", oerr.Code)
	}
	for _, r := range m.gl.Reqs() {
		if !strings.Contains(r.Path, "/projects/other%2Fname/") {
			t.Fatalf("request used %s", r.Path)
		}
	}
	// no mapping in the registry: repo_not_mapped, nothing sent
	m.srv.Registry.Update(func(g *Registry) error { g.Projects[0].Remote = nil; return nil })
	n := len(m.gl.Reqs())
	_, oerr := m.call(t, m.args())
	wantCode(t, oerr, "repo_not_mapped")
	if len(m.gl.Reqs()) != n {
		t.Fatal("GitLab contacted for an unmapped repo")
	}
}

func TestStatusContextReposFromRegistry(t *testing.T) {
	e := newTestEnv(t)
	s, _ := e.newServer(t, nil)
	sock := e.Raw["socket"].(string)
	read := func() (repos, mr []string) {
		r, out := call(t, sock, `{"v":1,"op":"status"}`)
		if !r.OK {
			t.Fatalf("status: %s", out)
		}
		var res struct {
			ContextRepos []string `json:"context_repos"`
			MRProjects   []string `json:"mr_projects"`
		}
		if err := json.Unmarshal(r.Result, &res); err != nil {
			t.Fatal(err)
		}
		return res.ContextRepos, res.MRProjects
	}
	repo := filepath.Join(e.Dir, "repo-a")
	if c, m := read(); len(c) != 1 || c[0] != repo || len(m) != 1 || m[0] != repo {
		t.Fatalf("after migration: %v %v", c, m)
	}
	s.Registry.Update(func(g *Registry) error {
		g.Projects[0].AIContext = false
		g.Projects = append(g.Projects, Project{ID: "added", Path: "/srv/added", Kind: "k", AIContext: true})
		return nil
	})
	if c, m := read(); len(c) != 1 || c[0] != "/srv/added" || len(m) != 1 || m[0] != repo {
		t.Fatalf("after registry change: %v %v", c, m)
	}
}

func TestBrokenRegistryMakesOpsUnavailableNotLegacy(t *testing.T) {
	fake := newFakeHTTP(t, okHandler(goodPlan, 10, 10))
	e := newTestEnv(t)
	state := filepath.Join(e.Dir, "state")
	os.MkdirAll(state, 0o700)
	os.WriteFile(filepath.Join(state, RegistryFileName), []byte("not json"), 0o600)
	s, logs := e.newServer(t, func(s *Server) { s.OpenAI.BaseURL = fake.URL })
	repo := filepath.Join(e.Dir, "repo-a")
	if s.Registry.Err() == nil {
		t.Fatal("registry should be broken")
	}
	sock := e.Raw["socket"].(string)
	if r, _ := call(t, sock, `{"v":1,"op":"status"}`); r.OK || r.Code != "registry_unavailable" {
		t.Fatalf("status: %+v", r)
	}
	ra, _ := json.Marshal(map[string]any{"repo": repo, "requirement": "x", "files": []any{}, "selection": "sel-abc123"})
	if _, oerr := s.Handlers["refine"](t.Context(), ra); oerr == nil || oerr.Code != "registry_unavailable" {
		t.Fatalf("refine: %+v", oerr)
	}
	ma, _ := json.Marshal(map[string]any{"repo": repo, "branch": "orch/1", "task": 1, "title_hint": "t", "summary": "s",
		"actor": "u", "verdict": "pass", "head": strings.Repeat("a", 40)})
	if _, oerr := s.Handlers["create_mr"](t.Context(), ma); oerr == nil || oerr.Code != "registry_unavailable" {
		t.Fatalf("create_mr: %+v", oerr)
	}
	if fake.Hits() != 0 {
		t.Fatal("AI endpoint reached with a broken registry")
	}
	if !strings.Contains(logs.String(), "등록부") {
		t.Fatal("no fixed log line about the registry")
	}
	if string(readFile(t, filepath.Join(state, RegistryFileName))) != "not json" {
		t.Fatal("broken registry file was rewritten")
	}
}

func TestConfigRepoRootsOptionalAndClean(t *testing.T) {
	e := newTestEnv(t)
	if cfg := e.load(t); len(cfg.RepoRoots) != 0 {
		t.Fatal("repo_roots must default to empty")
	}
	e.set(t, []string{"repo_roots"}, []string{"/home/orch/repos"})
	if cfg := e.load(t); len(cfg.RepoRoots) != 1 {
		t.Fatalf("%v", cfg.RepoRoots)
	}
	for _, bad := range [][]string{{"relative"}, {"/a/../b"}, {"/a/"}, {""}, {"/"}} {
		e.set(t, []string{"repo_roots"}, bad)
		if _, err := LoadConfigOpts(e.CfgPath, e.loadOpts()); err == nil {
			t.Fatalf("repo_roots %v accepted", bad)
		}
	}
}

// --- fix round 1 ---

func TestRegistryMigrationDoesNotClobberLateFile(t *testing.T) {
	legacy := LegacySource{ContextRepos: []string{"/srv/wide-open"}}
	good := mustMarshalRegistry(func() Registry {
		r := emptyRegistry()
		r.Projects = []Project{{ID: "late", Path: "/srv/late", Kind: "k"}}
		return r
	}())
	cases := map[string]struct {
		content string
		broken  bool
	}{
		"valid late file":  {content: string(good)},
		"broken late file": {content: "junk", broken: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := stateDirFor(t)
			path := filepath.Join(dir, RegistryFileName)
			o := regOpts()
			o.beforeCreate = func() { os.WriteFile(path, []byte(c.content), 0o600) }
			st, err := OpenRegistry(dir, legacy, o)
			if string(readFile(t, path)) != c.content {
				t.Fatal("late registry file was overwritten")
			}
			if c.broken {
				if !errors.Is(err, ErrRegistryBroken) || len(st.AIContextPaths()) != 0 {
					t.Fatalf("err=%v paths=%v", err, st.AIContextPaths())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p := st.Snapshot().Projects; len(p) != 1 || p[0].ID != "late" {
				t.Fatalf("projects %+v (legacy-derived?)", p)
			}
		})
	}
}

func TestRegistryConcurrentOpenCreatesExactlyOne(t *testing.T) {
	dir := stateDirFor(t)
	var wg sync.WaitGroup
	res := make([]*RegistryStore, 8)
	for i := range res {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := OpenRegistry(dir, LegacySource{ContextRepos: []string{"/srv/a"}}, regOpts())
			if err != nil {
				t.Error(err)
			}
			res[i] = st
		}(i)
	}
	wg.Wait()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir holds %v", entries)
	}
	for _, st := range res {
		if st == nil || len(st.Snapshot().Projects) != 1 {
			t.Fatal("an opener did not see the single registry")
		}
	}
}

func TestReadStateFileFailureBranches(t *testing.T) {
	dir := stateDirFor(t)
	path := filepath.Join(dir, RegistryFileName)
	good := string(mustMarshalRegistry(emptyRegistry()))
	open := func(o RegistryOptions) error {
		_, err := OpenRegistry(dir, LegacySource{ContextRepos: []string{"/srv/wide"}}, o)
		return err
	}
	os.MkdirAll(dir, 0o700)
	t.Run("file owner rejected, dir owner accepted", func(t *testing.T) {
		os.WriteFile(path, []byte(good), 0o600)
		o := regOpts()
		o.fileOwnerOK = func(uint32) bool { return false }
		if err := open(o); !errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("err %v", err)
		}
		os.Remove(path)
	})
	t.Run("unreadable (EACCES on lstat)", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores modes")
		}
		os.WriteFile(path, []byte(good), 0o600)
		os.Chmod(dir, 0o600)
		defer func() { os.Chmod(dir, 0o700); os.Remove(path) }()
		if err := open(regOpts()); !errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("directory in place of file", func(t *testing.T) {
		os.Mkdir(path, 0o700)
		defer os.Remove(path)
		if err := open(regOpts()); !errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("oversize file", func(t *testing.T) {
		os.WriteFile(path, bytes.Repeat([]byte(" "), maxStateFile+1), 0o600)
		defer os.Remove(path)
		if err := open(regOpts()); !errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestRegistryCaps(t *testing.T) {
	mk := func(i int, pathLen int) Project {
		n := strings.Repeat("a", pathLen-4) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))
		return Project{ID: "p" + strings.Repeat("x", 0) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)), Path: "/" + n, Kind: "k"}
	}
	t.Run("500 ok 501 refused", func(t *testing.T) {
		st := mustOpen(t, stateDirFor(t), LegacySource{})
		if err := st.Update(func(r *Registry) error {
			for i := 0; i < 500; i++ {
				r.Projects = append(r.Projects, mk(i, 20))
			}
			return nil
		}); err != nil {
			t.Fatalf("500: %v", err)
		}
		if err := st.Update(func(r *Registry) error { r.Projects = append(r.Projects, mk(500, 20)); return nil }); err == nil {
			t.Fatal("501 accepted")
		}
		if len(st.Snapshot().Projects) != 500 {
			t.Fatal("state changed")
		}
	})
	t.Run("path length", func(t *testing.T) {
		st := mustOpen(t, stateDirFor(t), LegacySource{})
		if err := st.Update(func(r *Registry) error { r.Projects = append(r.Projects, mk(0, 4096)); return nil }); err != nil {
			t.Fatalf("4096: %v", err)
		}
		if err := st.Update(func(r *Registry) error { r.Projects = append(r.Projects, mk(1, 4097)); return nil }); err == nil {
			t.Fatal("4097 accepted")
		}
	})
	t.Run("serialized size", func(t *testing.T) {
		dir := stateDirFor(t)
		st := mustOpen(t, dir, LegacySource{})
		before := readFile(t, filepath.Join(dir, RegistryFileName))
		err := st.Update(func(r *Registry) error {
			for i := 0; i < 300; i++ {
				r.Projects = append(r.Projects, mk(i, 4096))
			}
			return nil
		})
		if err == nil {
			t.Fatal("over-size registry accepted")
		}
		if !bytes.Equal(before, readFile(t, filepath.Join(dir, RegistryFileName))) || len(st.Snapshot().Projects) != 0 {
			t.Fatal("state changed")
		}
	})
}

func TestWriteAtomicDirSyncFailureKeepsMemoryEqualToDisk(t *testing.T) {
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{})
	st.opts.failAt = func(s string) error {
		if s == "dir-synced" {
			return errors.New("injected")
		}
		return nil
	}
	err := st.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: "a", Path: "/srv/a", Kind: "k"})
		return nil
	})
	if err == nil {
		t.Fatal("dir fsync failure not reported")
	}
	disk := mustOpen(t, dir, LegacySource{}).Snapshot()
	if len(disk.Projects) != 1 || len(st.Snapshot().Projects) != 1 {
		t.Fatalf("disk %d memory %d", len(disk.Projects), len(st.Snapshot().Projects))
	}
	// a failure before the rename still rolls back
	st.opts.failAt = func(s string) error {
		if s == "synced" {
			return errors.New("injected")
		}
		return nil
	}
	st.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: "b", Path: "/srv/b", Kind: "k"})
		return nil
	})
	if len(st.Snapshot().Projects) != 1 {
		t.Fatal("pre-rename failure not rolled back")
	}
}

func TestOpenRegistrySweepsStaleTempFiles(t *testing.T) {
	dir := stateDirFor(t)
	mustOpen(t, dir, LegacySource{})
	old := time.Now().Add(-time.Hour)
	stale := filepath.Join(dir, ".state-stale")
	fresh := filepath.Join(dir, ".state-fresh")
	other := filepath.Join(dir, "other.txt")
	outside := filepath.Join(shortTempDir(t), "victim")
	os.WriteFile(stale, []byte("x"), 0o600)
	os.WriteFile(fresh, []byte("x"), 0o600)
	os.WriteFile(other, []byte("x"), 0o600)
	os.WriteFile(outside, []byte("keep"), 0o600)
	os.Chtimes(stale, old, old)
	os.Chtimes(other, old, old)
	os.Chtimes(outside, old, old)
	link := filepath.Join(dir, ".state-link")
	os.Symlink(outside, link)
	os.Mkdir(filepath.Join(dir, ".state-dir"), 0o700)
	mustOpen(t, dir, LegacySource{})
	if _, err := os.Lstat(stale); err == nil {
		t.Fatal("stale temp not swept")
	}
	for _, p := range []string{fresh, other, link, filepath.Join(dir, ".state-dir"), outside, filepath.Join(dir, RegistryFileName)} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s wrongly removed", filepath.Base(p))
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatal("symlink target touched")
	}
}
