package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const v1Sample = `{"version":1,"providers":[],"projects":[` +
	`{"id":"repo-a","path":"/srv/repo-a","kind":"legacy","ai_context":true,"provider":null,"gitlab":{"project":"grp/proj","project_id":null},"auto_merge":{"enabled":false}},` +
	`{"id":"plain","path":"/srv/plain","kind":"registered","ai_context":false,"provider":null,"gitlab":null,"auto_merge":{"enabled":false}}` +
	`],"auto_merge_global":{"enabled":false,"max_per_day":7}}` + "\n"

func writeRegFile(t *testing.T, dir, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, RegistryFileName)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".state-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func testProvider(id string) Provider {
	return Provider{ID: id, Kind: "gitlab", BaseURL: "http://100.64.0.10:18082", SSHURL: "ssh://git@100.64.0.10:22", TokenRef: "gitlab-admin"}
}

func strp(s string) *string { return &s }

func TestRegistryV1MigratesToV2Atomically(t *testing.T) {
	t.Run("content preserved, mode 0600, idempotent", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v1Sample, 0o600)
		// legacy source must be ignored
		st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/ignored"}})
		reg := st.Snapshot()
		if reg.Version != 2 || len(reg.Providers) != 0 || len(reg.Orphans) != 0 || reg.AutoMergeGlobal.MaxPerDay != 7 || len(reg.Projects) != 2 {
			t.Fatalf("shape %+v", reg)
		}
		a, b := reg.Projects[0], reg.Projects[1]
		if a.ID != "repo-a" || a.Path != "/srv/repo-a" || a.Kind != "legacy" || !a.AIContext || a.Provider != nil ||
			a.GitLab == nil || a.GitLab.Project != "grp/proj" || a.GitLab.ProjectID != nil || a.AutoMerge.Enabled {
			t.Fatalf("project a changed: %+v", a)
		}
		if b.ID != "plain" || b.Kind != "registered" || b.AIContext || b.GitLab != nil || b.Provider != nil {
			t.Fatalf("project b changed: %+v", b)
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v", fi, err)
		}
		data := readFile(t, p)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["version"]) != "2" || string(raw["providers"]) != "[]" || string(raw["orphans"]) != "[]" {
			t.Fatalf("file not v2: %s", data)
		}
		// the project entries serialize exactly as they were in v1
		var v1raw map[string]json.RawMessage
		json.Unmarshal([]byte(v1Sample), &v1raw)
		if string(raw["projects"]) != string(v1raw["projects"]) || string(raw["auto_merge_global"]) != string(v1raw["auto_merge_global"]) {
			t.Fatalf("content changed:\n%s\n%s", raw["projects"], v1raw["projects"])
		}
		if left := tempLeftovers(t, dir); len(left) != 0 {
			t.Fatalf("temp files left: %v", left)
		}
		// opening again changes nothing
		st2 := mustOpen(t, dir, LegacySource{})
		if !bytes.Equal(data, readFile(t, p)) || len(st2.Snapshot().Projects) != 2 {
			t.Fatal("second open rewrote the file")
		}
	})
	t.Run("write failure leaves v1 untouched", func(t *testing.T) {
		for _, stage := range []string{"temp-written", "synced"} {
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, v1Sample, 0o600)
			o := regOpts()
			o.failAt = func(s string) error {
				if s == stage {
					return errors.New("boom")
				}
				return nil
			}
			st, err := OpenRegistry(dir, LegacySource{}, o)
			if err == nil || errors.Is(err, ErrRegistryBroken) {
				t.Fatalf("%s: want a start failure, got %v (store %v)", stage, err, st)
			}
			if string(readFile(t, p)) != v1Sample {
				t.Fatalf("%s: v1 file changed", stage)
			}
			if left := tempLeftovers(t, dir); len(left) != 0 {
				t.Fatalf("%s: temp files left: %v", stage, left)
			}
			// a later run without the fault migrates normally
			if st := mustOpen(t, dir, LegacySource{}); st.Snapshot().Version != 2 {
				t.Fatalf("%s: retry did not migrate", stage)
			}
		}
	})
	t.Run("broken v1 or unknown versions are never migrated or overwritten", func(t *testing.T) {
		good := `"providers":[],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5}`
		cases := map[string]string{
			"v1 with orphans":       `{"version":1,` + good + `,"orphans":[]}`,
			"v1 with provider set":  `{"version":1,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":"gl","gitlab":null,"auto_merge":{"enabled":false}}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`,
			"v1 with providers":     `{"version":1,"providers":[{"id":"gl","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t"}],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5}}`,
			"v1 with project_id":    `{"version":1,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"gitlab":{"project":"g/p","project_id":5},"auto_merge":{"enabled":false}}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`,
			"v1 unknown key":        `{"version":1,"extra":1,` + good + `}`,
			"v1 auto merge on":      `{"version":1,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","auto_merge":{"enabled":true}}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`,
			"v1 duplicate id":       `{"version":1,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k"},{"id":"a","path":"/x/b","kind":"k"}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`,
			"v1 truncated":          `{"version":1,"providers":[],"proj`,
			"version 0":             `{"version":0,` + good + `,"orphans":[]}`,
			"version 3":             `{"version":3,` + good + `,"orphans":[]}`,
			"version -1":            `{"version":-1,` + good + `,"orphans":[]}`,
			"version missing":       `{` + good + `,"orphans":[]}`,
			"version string":        `{"version":"2",` + good + `,"orphans":[]}`,
			"version float":         `{"version":2.0,` + good + `,"orphans":[]}`,
			"v2 unknown key":        `{"version":2,"extra":1,` + good + `,"orphans":[]}`,
			"v2 trailing data":      `{"version":2,` + good + `,"orphans":[]}{}`,
			"v2 provider unknown":   `{"version":2,"providers":[{"id":"gl","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t","extra":1}],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`,
			"v2 orphan unknown key": `{"version":2,"providers":[],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[{"provider":"gl","project":"g/p","project_id":null,"step":"commit","at":"2026-11-10T00:00:00Z","x":1}]}`,
		}
		for name, content := range cases {
			t.Run(name, func(t *testing.T) {
				dir := stateDirFor(t)
				p := writeRegFile(t, dir, content, 0o600)
				st, err := OpenRegistry(dir, LegacySource{ContextRepos: []string{"/srv/legacy"}}, regOpts())
				if !errors.Is(err, ErrRegistryBroken) || st.Err() == nil {
					t.Fatalf("not refused: %v", err)
				}
				if string(readFile(t, p)) != content {
					t.Fatal("broken file was modified")
				}
				if exists, serr := RegistryStatus(dir, regOpts()); !exists || !errors.Is(serr, ErrRegistryBroken) {
					t.Fatalf("status: %v %v", exists, serr)
				}
			})
		}
	})
	t.Run("status check never migrates", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v1Sample, 0o600)
		if exists, err := RegistryStatus(dir, regOpts()); !exists || err != nil {
			t.Fatalf("%v %v", exists, err)
		}
		if string(readFile(t, p)) != v1Sample {
			t.Fatal("RegistryStatus wrote the file")
		}
	})
	t.Run("wrong mode v1 is broken, not migrated", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v1Sample, 0o644)
		if _, err := OpenRegistry(dir, LegacySource{}, regOpts()); !errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("%v", err)
		}
		if string(readFile(t, p)) != v1Sample {
			t.Fatal("modified")
		}
	})
}

func TestRegistryV2ValidationRules(t *testing.T) {
	id := int64(7)
	zero := int64(0)
	neg := int64(-3)
	base := func() []Provider { return []Provider{testProvider("gl")} }
	proj := func(gl *GitLabLink, prov *string) Project {
		return Project{ID: "app", Path: "/srv/app", Kind: "registered", Provider: prov, GitLab: gl}
	}
	linked := &GitLabLink{Project: "team/app", ProjectID: &id}
	mut := func(f func(*Provider)) []Provider { p := testProvider("gl"); f(&p); return []Provider{p} }
	many := func(n int) []Provider {
		var out []Provider
		for i := 0; i < n; i++ {
			out = append(out, testProvider(fmt.Sprintf("p%02d", i)))
		}
		return out
	}
	bad := map[string]func(*Registry){
		"empty provider id":         func(r *Registry) { r.Providers = mut(func(p *Provider) { p.ID = "" }) },
		"uppercase provider id":     func(r *Registry) { r.Providers = mut(func(p *Provider) { p.ID = "GL" }) },
		"41-char provider id":       func(r *Registry) { r.Providers = mut(func(p *Provider) { p.ID = "a" + strings.Repeat("b", 40) }) },
		"slash in provider id":      func(r *Registry) { r.Providers = mut(func(p *Provider) { p.ID = "a/b" }) },
		"kind github":               func(r *Registry) { r.Providers = mut(func(p *Provider) { p.Kind = "github" }) },
		"kind empty":                func(r *Registry) { r.Providers = mut(func(p *Provider) { p.Kind = "" }) },
		"base_url userinfo":         func(r *Registry) { r.Providers = mut(func(p *Provider) { p.BaseURL = "http://u:p@h" }) },
		"base_url path":             func(r *Registry) { r.Providers = mut(func(p *Provider) { p.BaseURL = "http://h/api/v4" }) },
		"base_url query":            func(r *Registry) { r.Providers = mut(func(p *Provider) { p.BaseURL = "http://h?x=1" }) },
		"base_url fragment":         func(r *Registry) { r.Providers = mut(func(p *Provider) { p.BaseURL = "http://h#f" }) },
		"base_url scheme":           func(r *Registry) { r.Providers = mut(func(p *Provider) { p.BaseURL = "ftp://h" }) },
		"base_url empty":            func(r *Registry) { r.Providers = mut(func(p *Provider) { p.BaseURL = "" }) },
		"ssh_url wrong user":        func(r *Registry) { r.Providers = mut(func(p *Provider) { p.SSHURL = "ssh://root@h" }) },
		"ssh_url path":              func(r *Registry) { r.Providers = mut(func(p *Provider) { p.SSHURL = "ssh://git@h/x.git" }) },
		"ssh_url empty":             func(r *Registry) { r.Providers = mut(func(p *Provider) { p.SSHURL = "" }) },
		"token_ref uppercase":       func(r *Registry) { r.Providers = mut(func(p *Provider) { p.TokenRef = "Tok" }) },
		"token_ref empty":           func(r *Registry) { r.Providers = mut(func(p *Provider) { p.TokenRef = "" }) },
		"token_ref slash":           func(r *Registry) { r.Providers = mut(func(p *Provider) { p.TokenRef = "a/b" }) },
		"token_ref 41 chars":        func(r *Registry) { r.Providers = mut(func(p *Provider) { p.TokenRef = "a" + strings.Repeat("b", 40) }) },
		"duplicate provider":        func(r *Registry) { r.Providers = []Provider{testProvider("gl"), testProvider("gl")} },
		"too many providers":        func(r *Registry) { r.Providers = many(maxProviders + 1) },
		"project refs unknown prov": func(r *Registry) { r.Providers = base(); r.Projects = []Project{proj(nil, strp("other"))} },
		"project refs when none":    func(r *Registry) { r.Projects = []Project{proj(nil, strp("gl"))} },
		"project provider bad id":   func(r *Registry) { r.Providers = base(); r.Projects = []Project{proj(nil, strp("GL"))} },
		"project provider empty":    func(r *Registry) { r.Providers = base(); r.Projects = []Project{proj(nil, strp(""))} },
		"project_id without prov":   func(r *Registry) { r.Projects = []Project{proj(linked, nil)} },
		"project_id zero": func(r *Registry) {
			r.Providers = base()
			r.Projects = []Project{proj(&GitLabLink{Project: "team/app", ProjectID: &zero}, strp("gl"))}
		},
		"project_id negative": func(r *Registry) {
			r.Providers = base()
			r.Projects = []Project{proj(&GitLabLink{Project: "team/app", ProjectID: &neg}, strp("gl"))}
		},
		"gitlab dotdot": func(r *Registry) {
			r.Providers = base()
			r.Projects = []Project{proj(&GitLabLink{Project: "a/../b"}, strp("gl"))}
		},
		"auto merge on w/ provider": func(r *Registry) {
			r.Providers = base()
			p := proj(linked, strp("gl"))
			p.AutoMerge.Enabled = true
			r.Projects = []Project{p}
		},
		"global auto merge": func(r *Registry) { r.AutoMergeGlobal.Enabled = true },
		"wrong version":     func(r *Registry) { r.Version = 1 },
		"too many orphans":  func(r *Registry) { r.Orphans = makeOrphans(maxOrphans + 1) },
		"orphan bad step": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gl", Project: "team/app", Step: "Commit!", At: "2026-11-10T00:00:00Z"}}
		},
		"orphan empty step": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gl", Project: "team/app", Step: "", At: "2026-11-10T00:00:00Z"}}
		},
		"orphan bad provider": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "GL", Project: "team/app", Step: "commit", At: "2026-11-10T00:00:00Z"}}
		},
		"orphan bad project": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gl", Project: "../x", Step: "commit", At: "2026-11-10T00:00:00Z"}}
		},
		"orphan bad at": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gl", Project: "team/app", Step: "commit", At: "yesterday"}}
		},
		"orphan offset at": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gl", Project: "team/app", Step: "commit", At: "2026-11-10T09:00:00+09:00"}}
		},
		"orphan project_id 0": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gl", Project: "team/app", ProjectID: &zero, Step: "commit", At: "2026-11-10T00:00:00Z"}}
		},
	}
	for name, f := range bad {
		t.Run(name, func(t *testing.T) {
			dir := stateDirFor(t)
			st := mustOpen(t, dir, LegacySource{})
			before := readFile(t, filepath.Join(dir, RegistryFileName))
			if err := st.Update(func(r *Registry) error { f(r); return nil }); err == nil {
				t.Fatal("invalid registry accepted")
			}
			if !bytes.Equal(before, readFile(t, filepath.Join(dir, RegistryFileName))) || len(st.Snapshot().Providers) != 0 || len(st.Snapshot().Projects) != 0 {
				t.Fatal("invalid update changed state")
			}
		})
	}
	t.Run("valid v2 registry persists and reloads", func(t *testing.T) {
		dir := stateDirFor(t)
		st := mustOpen(t, dir, LegacySource{})
		err := st.Update(func(r *Registry) error {
			r.Providers = []Provider{testProvider("gl"), testProvider("gl-two")}
			r.Projects = []Project{
				proj(linked, strp("gl")),
				{ID: "legacy-one", Path: "/srv/legacy-one", Kind: "legacy", GitLab: &GitLabLink{Project: "grp/legacy"}}, // legacy link: no provider, no project_id
				{ID: "plain", Path: "/srv/plain", Kind: "registered"},
			}
			r.AddOrphan(Orphan{Provider: "gone", Project: "team/xx", Step: "commit", At: "2026-11-10T00:00:00Z"}) // provider may be deleted later
			return nil
		})
		if err != nil {
			t.Fatalf("valid registry refused: %v", err)
		}
		st2 := mustOpen(t, dir, LegacySource{})
		got := st2.Snapshot()
		if len(got.Providers) != 2 || got.Providers[0] != testProvider("gl") || len(got.Projects) != 3 || len(got.Orphans) != 1 {
			t.Fatalf("reloaded %+v", got)
		}
		p0 := got.Projects[0]
		if p0.Provider == nil || *p0.Provider != "gl" || p0.GitLab == nil || p0.GitLab.ProjectID == nil || *p0.GitLab.ProjectID != 7 {
			t.Fatalf("project link %+v", p0)
		}
		// the snapshot is a deep copy
		*got.Projects[0].Provider = "zz"
		*got.Projects[0].GitLab.ProjectID = 99
		got.Providers[0].ID = "zz"
		got.Orphans[0].Step = "zz"
		again := st2.Snapshot()
		if *again.Projects[0].Provider != "gl" || *again.Projects[0].GitLab.ProjectID != 7 || again.Providers[0].ID != "gl" || again.Orphans[0].Step != "commit" {
			t.Fatal("snapshot aliases live state")
		}
		// removing a referenced provider is refused (delete_provider's provider_in_use rests on this)
		before := readFile(t, filepath.Join(dir, RegistryFileName))
		if err := st2.Update(func(r *Registry) error { r.Providers = r.Providers[1:]; return nil }); err == nil {
			t.Fatal("provider removed while referenced")
		}
		if !bytes.Equal(before, readFile(t, filepath.Join(dir, RegistryFileName))) {
			t.Fatal("state changed")
		}
		// ...but an unreferenced one can go
		if err := st2.Update(func(r *Registry) error { r.Providers = r.Providers[:1]; return nil }); err != nil {
			t.Fatalf("unreferenced provider removal: %v", err)
		}
	})
	t.Run("max providers accepted", func(t *testing.T) {
		st := mustOpen(t, stateDirFor(t), LegacySource{})
		if err := st.Update(func(r *Registry) error { r.Providers = many(maxProviders); return nil }); err != nil {
			t.Fatalf("%d providers: %v", maxProviders, err)
		}
	})
	t.Run("registry file stays under the size cap with maxima", func(t *testing.T) {
		r := emptyRegistry()
		r.Providers = many(maxProviders)
		r.Orphans = makeOrphans(maxOrphans)
		if len(mustMarshalRegistry(r)) > 64*1024 {
			t.Fatalf("providers+orphans alone take %d bytes", len(mustMarshalRegistry(r)))
		}
	})
}

func makeOrphans(n int) []Orphan {
	out := make([]Orphan, n)
	for i := range out {
		out[i] = Orphan{Provider: "gl", Project: fmt.Sprintf("team/app-%03d", i), Step: "commit", At: "2026-11-10T00:00:00Z"}
	}
	return out
}

func TestOrphansBounded(t *testing.T) {
	// Overflow policy (pinned): the registry never holds more than 100
	// orphans; AddOrphan appends the new record and drops the OLDEST ones.
	// Validation refuses a registry that already holds more than 100, so no
	// other writer can exceed the cap either.
	if maxOrphans != 100 {
		t.Fatalf("maxOrphans = %d", maxOrphans)
	}
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{})
	for i := 0; i < 105; i++ {
		i := i
		err := st.Update(func(r *Registry) error {
			pid := int64(1000 + i)
			r.AddOrphan(Orphan{Provider: "gl", Project: fmt.Sprintf("team/app-%03d", i), ProjectID: &pid, Step: "deploy_key", At: "2026-11-10T00:00:00Z"})
			return nil
		})
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	got := st.Snapshot().Orphans
	if len(got) != 100 {
		t.Fatalf("len %d", len(got))
	}
	if got[0].Project != "team/app-005" || got[99].Project != "team/app-104" {
		t.Fatalf("not the newest 100 in order: first %s last %s", got[0].Project, got[99].Project)
	}
	// persisted the same way
	st2 := mustOpen(t, dir, LegacySource{})
	re := st2.Snapshot().Orphans
	if len(re) != 100 || re[0].Project != "team/app-005" || re[99].ProjectID == nil || *re[99].ProjectID != 1104 {
		t.Fatalf("reload: %d %+v", len(re), re[0])
	}
	// an exact-cap file loads; one over is broken
	var f map[string]any
	json.Unmarshal(readFile(t, filepath.Join(dir, RegistryFileName)), &f)
	f["orphans"] = append(f["orphans"].([]any), map[string]any{"provider": "gl", "project": "team/xx", "project_id": nil, "step": "commit", "at": "2026-11-10T00:00:00Z"})
	over, _ := json.Marshal(f)
	dir2 := stateDirFor(t)
	writeRegFile(t, dir2, string(over), 0o600)
	if _, err := OpenRegistry(dir2, LegacySource{}, regOpts()); !errors.Is(err, ErrRegistryBroken) {
		t.Fatalf("101 orphans on disk: %v", err)
	}
	// a plain Update that exceeds the cap is refused, state unchanged
	before := readFile(t, filepath.Join(dir, RegistryFileName))
	if err := st2.Update(func(r *Registry) error {
		r.Orphans = append(r.Orphans, Orphan{Provider: "gl", Project: "team/xx", Step: "commit", At: "2026-11-10T00:00:00Z"})
		return nil
	}); err == nil {
		t.Fatal("direct append past the cap accepted")
	}
	if !bytes.Equal(before, readFile(t, filepath.Join(dir, RegistryFileName))) {
		t.Fatal("state changed")
	}
	// records hold values only: exactly these keys, nothing token- or body-like
	b, _ := json.Marshal(Orphan{Provider: "gl", Project: "team/xx", Step: "commit", At: "2026-11-10T00:00:00Z"})
	var keys map[string]any
	json.Unmarshal(b, &keys)
	if len(keys) != 5 {
		t.Fatalf("orphan keys %v", keys)
	}
	for _, k := range []string{"provider", "project", "project_id", "step", "at"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing key %s", k)
		}
	}
	// AddOrphan on an empty list and on a nil list
	var r Registry
	r.AddOrphan(Orphan{Provider: "gl", Project: "team/xx", Step: "commit", At: "2026-11-10T00:00:00Z"})
	if len(r.Orphans) != 1 {
		t.Fatal("AddOrphan on nil list")
	}
}

// W3b and older brokers only accept "version":1 and refuse unknown keys. This
// test pins the version constants and replays the W3b load rules on a v2 file,
// so the "roll back the registry together with the broker" runbook warning
// stays true: changing registryVersion back to 1, or letting v2 look like v1,
// fails here.
func TestOldBrokerVersionRefusesV2(t *testing.T) {
	if registryVersion != 2 || registryVersionV1 != 1 {
		t.Fatalf("version constants moved: current=%d previous=%d", registryVersion, registryVersionV1)
	}
	if got := emptyRegistry().Version; got != 2 {
		t.Fatalf("fresh registry version %d", got)
	}
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/a"}})
	if err := st.Update(func(r *Registry) error { r.Providers = []Provider{testProvider("gl")}; return nil }); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, filepath.Join(dir, RegistryFileName))

	// W3b loader, copied: strict decode into the v1 shape, then version == 1.
	type w3bProject struct {
		ID        string          `json:"id"`
		Path      string          `json:"path"`
		Kind      string          `json:"kind"`
		AIContext bool            `json:"ai_context"`
		Provider  *struct{}       `json:"provider"`
		GitLab    json.RawMessage `json:"gitlab"`
		AutoMerge AutoMerge       `json:"auto_merge"`
	}
	type w3bRegistry struct {
		Version         int           `json:"version"`
		Providers       []struct{}    `json:"providers"`
		Projects        []w3bProject  `json:"projects"`
		AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
	}
	oldAccepts := func(b []byte) bool {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var r w3bRegistry
		return dec.Decode(&r) == nil && r.Version == 1 && len(r.Providers) == 0
	}
	if oldAccepts(data) {
		t.Fatalf("a v2 file would be accepted by W3b code: %s", data)
	}
	var f map[string]any
	json.Unmarshal(data, &f)
	if f["version"] != float64(2) {
		t.Fatalf("file version %v", f["version"])
	}
	// the replica does accept a genuine v1 file (so the check above is meaningful)
	if !oldAccepts([]byte(v1Sample)) {
		t.Fatal("replica of the W3b loader rejects a v1 file")
	}
	// and even with the version forced back to 1, the v2-only keys make it fail
	delete(f, "version")
	f["version"] = 1
	forced, _ := json.Marshal(f)
	if oldAccepts(forced) {
		t.Fatal("v2-only keys would pass the W3b strict decoder")
	}
}
