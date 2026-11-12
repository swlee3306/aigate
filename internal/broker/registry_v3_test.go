package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const v2Sample = `{"version":2,"providers":[` +
	`{"id":"gl","kind":"gitlab","base_url":"http://100.64.0.10:18082","ssh_url":"ssh://git@100.64.0.10:22","token_ref":"gitlab-admin"}` +
	`],"projects":[` +
	`{"id":"app-one","path":"/srv/app-one","kind":"registered","ai_context":true,"provider":"gl","gitlab":{"project":"team/sub/app-one","project_id":21},"auto_merge":{"enabled":false}},` +
	`{"id":"legacy-a","path":"/srv/legacy-a","kind":"legacy","ai_context":false,"provider":null,"gitlab":{"project":"grp/proj","project_id":null},"auto_merge":{"enabled":false}},` +
	`{"id":"plain","path":"/srv/plain","kind":"registered","ai_context":false,"provider":null,"gitlab":null,"auto_merge":{"enabled":false}},` +
	`{"id":"nolink","path":"/srv/nolink","kind":"registered","ai_context":false,"provider":"gl","gitlab":null,"auto_merge":{"enabled":false}}` +
	`],"auto_merge_global":{"enabled":false,"max_per_day":9},"orphans":[` +
	`{"provider":"gl","project":"team/half","project_id":33,"step":"protect","at":"2026-11-10T00:00:00Z"},` +
	`{"provider":"gone","project":"team/x","project_id":null,"step":"project","at":"2026-11-11T01:02:03Z"}]}` + "\n"

// The v3 file the v2 sample must turn into: same values, link key renamed.
const v3Want = `{"version":3,"providers":[` +
	`{"id":"gl","kind":"gitlab","base_url":"http://100.64.0.10:18082","ssh_url":"ssh://git@100.64.0.10:22","token_ref":"gitlab-admin"}` +
	`],"projects":[` +
	`{"id":"app-one","path":"/srv/app-one","kind":"registered","ai_context":true,"provider":"gl","remote":{"path":"team/sub/app-one","id":21},"auto_merge":{"enabled":false}},` +
	`{"id":"legacy-a","path":"/srv/legacy-a","kind":"legacy","ai_context":false,"provider":null,"remote":{"path":"grp/proj","id":null},"auto_merge":{"enabled":false}},` +
	`{"id":"plain","path":"/srv/plain","kind":"registered","ai_context":false,"provider":null,"remote":null,"auto_merge":{"enabled":false}},` +
	`{"id":"nolink","path":"/srv/nolink","kind":"registered","ai_context":false,"provider":"gl","remote":null,"auto_merge":{"enabled":false}}` +
	`],"auto_merge_global":{"enabled":false,"max_per_day":9},"orphans":[` +
	`{"provider":"gl","project":"team/half","project_id":33,"step":"protect","at":"2026-11-10T00:00:00Z","origin":"created"},` +
	`{"provider":"gone","project":"team/x","project_id":null,"step":"project","at":"2026-11-11T01:02:03Z","origin":"created"}]}` + "\n"

func TestRegistryV2MigratesToV3Atomically(t *testing.T) {
	t.Run("content unchanged, mode 0600, idempotent", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v2Sample, 0o600)
		st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/ignored"}})
		reg := st.Snapshot()
		if reg.Version != 4 || len(reg.Providers) != 1 || len(reg.Projects) != 4 || len(reg.Orphans) != 2 || reg.AutoMergeGlobal.MaxPerDay != 9 {
			t.Fatalf("shape %+v", reg)
		}
		a := reg.Projects[0]
		if a.Provider == nil || *a.Provider != "gl" || a.Remote == nil || a.Remote.Path != "team/sub/app-one" || a.Remote.ID == nil || *a.Remote.ID != 21 || !a.AIContext {
			t.Fatalf("linked project changed: %+v", a)
		}
		if l := reg.Projects[1]; l.Provider != nil || l.Remote == nil || l.Remote.Path != "grp/proj" || l.Remote.ID != nil {
			t.Fatalf("legacy link changed: %+v", l)
		}
		if reg.Projects[2].Remote != nil || reg.Projects[3].Remote != nil || reg.Projects[3].Provider == nil {
			t.Fatal("unlinked projects changed")
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v", fi, err)
		}
		data := readFile(t, p)
		if string(data) != v4Want {
			t.Fatalf("file differs from the expected v4:\n%s\n%s", data, v4Want)
		}
		if left := tempLeftovers(t, dir); len(left) != 0 {
			t.Fatalf("temp files left: %v", left)
		}
		// providers, orphans and settings are byte-identical to the v2 file
		var o2, o3 map[string]json.RawMessage
		json.Unmarshal([]byte(v2Sample), &o2)
		json.Unmarshal(data, &o3)
		// (orphan records gain only the origin "created", the meaning of every older record)
		o3["orphans"] = json.RawMessage(strings.ReplaceAll(string(o3["orphans"]), `,"origin":"created"`, ""))
		for _, k := range []string{"providers", "orphans", "auto_merge_global"} {
			if string(o2[k]) != string(o3[k]) {
				t.Fatalf("%s changed: %s vs %s", k, o2[k], o3[k])
			}
		}
		// the legacy mapping and the W2b helpers behave as before
		if g, ok := st.GitLabProject("/srv/legacy-a"); !ok || g != "grp/proj" {
			t.Fatalf("GitLabProject %q %v", g, ok)
		}
		if _, ok := st.GitLabProject("/srv/app-one"); ok {
			t.Fatal("a provider-linked project must not take the legacy create_mr path")
		}
		if got := st.GitLabPaths(); len(got) != 1 || got[0] != "/srv/legacy-a" {
			t.Fatalf("GitLabPaths %v", got)
		}
		// opening again changes nothing
		before := readFile(t, p)
		mustOpen(t, dir, LegacySource{})
		if !bytes.Equal(before, readFile(t, p)) {
			t.Fatal("second open rewrote the file")
		}
	})
	t.Run("write failure leaves v2 untouched", func(t *testing.T) {
		for _, stage := range []string{"temp-written", "synced", "dir-synced"} {
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, v2Sample, 0o600)
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
			if stage == "dir-synced" { // the rename already happened: v3 on disk, retry is a plain open
				after := readFile(t, p)
				if string(after) != v4Want {
					t.Fatalf("dir-synced: %s", after)
				}
				mustOpen(t, dir, LegacySource{})
				if !bytes.Equal(after, readFile(t, p)) {
					t.Fatal("dir-synced: retry changed the file")
				}
				continue
			}
			if string(readFile(t, p)) != v2Sample {
				t.Fatalf("%s: v2 file changed", stage)
			}
			if left := tempLeftovers(t, dir); len(left) != 0 {
				t.Fatalf("%s: temp files left: %v", stage, left)
			}
			if st := mustOpen(t, dir, LegacySource{}); st.Snapshot().Version != 4 || string(readFile(t, p)) != v4Want {
				t.Fatalf("%s: retry did not migrate", stage)
			}
		}
	})
	t.Run("unwritable directory is a start failure and leaves v2", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory modes")
		}
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v2Sample, 0o600)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		st, err := OpenRegistry(dir, LegacySource{}, regOpts())
		if err == nil || errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("want a start failure, got %v (%v)", err, st)
		}
		os.Chmod(dir, 0o700)
		if string(readFile(t, p)) != v2Sample {
			t.Fatal("v2 file changed")
		}
	})
	t.Run("v1 file goes straight to v3", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v1Sample, 0o600)
		st := mustOpen(t, dir, LegacySource{})
		if st.Snapshot().Version != 4 || !strings.Contains(string(readFile(t, p)), `"version":4`) || strings.Contains(string(readFile(t, p)), `"gitlab"`) {
			t.Fatalf("%s", readFile(t, p))
		}
	})
	t.Run("broken or unowned v2 is never migrated or overwritten", func(t *testing.T) {
		const tail = `"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`
		prov := `{"id":"gl","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t"}`
		ghprov := `{"id":"gh","kind":"github","base_url":"https://api.github.com","ssh_url":"ssh://git@github.com","token_ref":"t"}`
		cases := map[string]string{
			"github provider in a v2 file":  `{"version":2,"providers":[` + ghprov + `],"projects":[],` + tail,
			"v2 with a remote key":          `{"version":2,"providers":[` + prov + `],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":"gl","remote":{"path":"g/a","id":1},"auto_merge":{"enabled":false}}],` + tail,
			"v2 project_id without prov":    `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"gitlab":{"project":"g/a","project_id":5},"auto_merge":{"enabled":false}}],` + tail,
			"v2 project_id zero":            `{"version":2,"providers":[` + prov + `],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":"gl","gitlab":{"project":"g/a","project_id":0},"auto_merge":{"enabled":false}}],` + tail,
			"v2 gitlab dotdot":              `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"gitlab":{"project":"a/../b","project_id":null},"auto_merge":{"enabled":false}}],` + tail,
			"v2 gitlab unknown key":         `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"gitlab":{"project":"g/a","project_id":null,"x":1},"auto_merge":{"enabled":false}}],` + tail,
			"v2 unknown provider reference": `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":"nope","gitlab":null,"auto_merge":{"enabled":false}}],` + tail,
			"v2 auto merge on":              `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"gitlab":null,"auto_merge":{"enabled":true}}],` + tail,
			"v2 duplicate id":               `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"gitlab":null,"auto_merge":{"enabled":false}},{"id":"a","path":"/x/b","kind":"k","ai_context":false,"provider":null,"gitlab":null,"auto_merge":{"enabled":false}}],` + tail,
			"v2 trailing data":              `{"version":2,"providers":[],"projects":[],` + tail + `{}`,
			"v2 orphans missing is ok key":  `{"version":2,"providers":[],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[{"provider":"gl","project":"../x","project_id":null,"step":"project","at":"2026-11-10T00:00:00Z"}]}`,
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
			})
		}
		for name, c := range map[string]struct {
			mode  os.FileMode
			owner func(uint32) bool
		}{
			"mode 0644":     {0o644, nil},
			"mode 0660":     {0o660, nil},
			"foreign owner": {0o600, func(uint32) bool { return false }},
		} {
			t.Run(name, func(t *testing.T) {
				dir := stateDirFor(t)
				p := writeRegFile(t, dir, v2Sample, c.mode)
				o := regOpts()
				if c.owner != nil {
					o.fileOwnerOK = c.owner
				}
				st, err := OpenRegistry(dir, LegacySource{}, o)
				if !errors.Is(err, ErrRegistryBroken) || st.Err() == nil {
					t.Fatalf("not refused: %v", err)
				}
				if string(readFile(t, p)) != v2Sample {
					t.Fatal("file was modified")
				}
			})
		}
	})
	t.Run("status check never migrates", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v2Sample, 0o600)
		if exists, err := RegistryStatus(dir, regOpts()); !exists || err != nil {
			t.Fatalf("%v %v", exists, err)
		}
		if string(readFile(t, p)) != v2Sample {
			t.Fatal("RegistryStatus wrote the file")
		}
	})
}

// A W3c broker (registry version 2) must treat a v3 file as broken and serve
// nothing: rolling the binary back needs the pre-migration registry backup.
// This pins the constants and replays the W3c strict loader on a real v3 file.
func TestOldBrokerRefusesV3(t *testing.T) {
	if registryVersion != 4 || registryVersionV3 != 3 || registryVersionV2 != 2 || registryVersionV1 != 1 {
		t.Fatalf("version constants moved: current=%d v3=%d v2=%d v1=%d", registryVersion, registryVersionV3, registryVersionV2, registryVersionV1)
	}
	if emptyRegistry().Version != 4 {
		t.Fatal("fresh registry is not v4")
	}
	// W3c loader, copied: strict decode into the v2 shape, then version == 2.
	type w3cProject struct {
		ID        string          `json:"id"`
		Path      string          `json:"path"`
		Kind      string          `json:"kind"`
		AIContext bool            `json:"ai_context"`
		Provider  *string         `json:"provider"`
		GitLab    json.RawMessage `json:"gitlab"`
		AutoMerge AutoMerge       `json:"auto_merge"`
	}
	type w3cRegistry struct {
		Version         int           `json:"version"`
		Providers       []Provider    `json:"providers"`
		Projects        []w3cProject  `json:"projects"`
		AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
		Orphans         []Orphan      `json:"orphans"`
	}
	oldAccepts := func(b []byte) bool {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var r w3cRegistry
		return dec.Decode(&r) == nil && r.Version == 2
	}
	if !oldAccepts([]byte(v2Sample)) {
		t.Fatal("replica of the W3c loader rejects a v2 file (the check below would mean nothing)")
	}
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/a"}})
	data := readFile(t, filepath.Join(dir, RegistryFileName))
	if oldAccepts(data) {
		t.Fatalf("a v3 file would be accepted by W3c code: %s", data)
	}
	// with a project that carries the new link key the strict decoder refuses even a forced version 2
	if err := st.Update(func(r *Registry) error {
		r.Providers = []Provider{testProvider("gl")}
		r.Projects = append(r.Projects, Project{Origin: OriginUnknown, ID: "linked", Path: "/srv/linked", Kind: "registered", Provider: strp("gl"), Remote: &RemoteLink{Path: "team/linked"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data = readFile(t, filepath.Join(dir, RegistryFileName))
	var f map[string]any
	json.Unmarshal(data, &f)
	if f["version"] != float64(4) {
		t.Fatalf("file version %v", f["version"])
	}
	f["version"] = 2
	forced, _ := json.Marshal(f)
	if oldAccepts(forced) {
		t.Fatal("the v3 link key would pass the W3c strict decoder")
	}
	// and a v3 file that no project uses the key in still carries version 3
	var empty map[string]any
	json.Unmarshal(mustMarshalRegistry(emptyRegistry()), &empty)
	if empty["version"] != float64(4) {
		t.Fatal("empty registry version")
	}
}

func TestRegistryV3Rules(t *testing.T) {
	ghp := Provider{ID: "gh", Kind: "github", BaseURL: "https://api.github.com", SSHURL: "ssh://git@github.com", TokenRef: "github-main"}
	id := int64(4242)
	zero := int64(0)
	t.Run("github provider and remote persist and reload", func(t *testing.T) {
		dir := stateDirFor(t)
		st := mustOpen(t, dir, LegacySource{})
		err := st.Update(func(r *Registry) error {
			r.Providers = []Provider{testProvider("gl"), ghp}
			r.Projects = []Project{
				{Origin: OriginUnknown, ID: "hello", Path: "/srv/hello", Kind: "registered", Provider: strp("gh"), Remote: &RemoteLink{Path: "Octo-Cat/Hello_World.v2", ID: &id}},
				{Origin: OriginUnknown, ID: "noid", Path: "/srv/noid", Kind: "registered", Provider: strp("gh"), Remote: &RemoteLink{Path: "octo/noid"}},
			}
			r.AddOrphan(Orphan{Provider: "gh", Project: "Octo-Cat/half-made", Step: "protect", At: "2026-11-12T00:00:00Z"}) // GitHub owner keeps case
			return nil
		})
		if err != nil {
			t.Fatalf("valid v3 registry refused: %v", err)
		}
		got := mustOpen(t, dir, LegacySource{}).Snapshot()
		if len(got.Providers) != 2 || got.Providers[1] != ghp || got.Projects[0].Remote == nil || got.Projects[0].Remote.Path != "Octo-Cat/Hello_World.v2" ||
			got.Projects[0].Remote.ID == nil || *got.Projects[0].Remote.ID != 4242 || len(got.Orphans) != 1 {
			t.Fatalf("reloaded %+v", got)
		}
		// a github remote never feeds the legacy create_mr path
		if _, ok := mustOpen(t, dir, LegacySource{}).GitLabProject("/srv/hello"); ok {
			t.Fatal("github remote reached the W2b helper")
		}
	})
	t.Run("kind enum", func(t *testing.T) {
		for _, kind := range []string{"bitbucket", "", "GitHub", "gitea"} {
			st := mustOpen(t, stateDirFor(t), LegacySource{})
			p := ghp
			p.Kind = kind
			if err := st.Update(func(r *Registry) error { r.Providers = []Provider{p}; return nil }); err == nil {
				t.Errorf("kind %q accepted", kind)
			}
		}
	})
	bad := map[string]func(*Registry){
		"remote id without provider": func(r *Registry) {
			r.Projects = []Project{{Origin: OriginUnknown, ID: "a", Path: "/srv/a", Kind: "k", Remote: &RemoteLink{Path: "o/r", ID: &id}}}
		},
		"remote id zero": func(r *Registry) {
			r.Providers = []Provider{ghp}
			r.Projects = []Project{{Origin: OriginUnknown, ID: "a", Path: "/srv/a", Kind: "k", Provider: strp("gh"), Remote: &RemoteLink{Path: "o/r", ID: &zero}}}
		},
		"remote dotdot": func(r *Registry) {
			r.Providers = []Provider{ghp}
			r.Projects = []Project{{Origin: OriginUnknown, ID: "a", Path: "/srv/a", Kind: "k", Provider: strp("gh"), Remote: &RemoteLink{Path: "o/../r"}}}
		},
		"remote percent": func(r *Registry) {
			r.Providers = []Provider{ghp}
			r.Projects = []Project{{Origin: OriginUnknown, ID: "a", Path: "/srv/a", Kind: "k", Provider: strp("gh"), Remote: &RemoteLink{Path: "o/r%2f"}}}
		},
		"remote one segment": func(r *Registry) {
			r.Providers = []Provider{ghp}
			r.Projects = []Project{{Origin: OriginUnknown, ID: "a", Path: "/srv/a", Kind: "k", Provider: strp("gh"), Remote: &RemoteLink{Path: "solo"}}}
		},
		"remote empty": func(r *Registry) {
			r.Providers = []Provider{ghp}
			r.Projects = []Project{{Origin: OriginUnknown, ID: "a", Path: "/srv/a", Kind: "k", Provider: strp("gh"), Remote: &RemoteLink{}}}
		},
		"orphan bad project": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gh", Project: "o/../x", Step: "protect", At: "2026-11-12T00:00:00Z"}}
		},
		"orphan percent": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gh", Project: "o/r%2f", Step: "protect", At: "2026-11-12T00:00:00Z"}}
		},
		"orphan single segment": func(r *Registry) {
			r.Orphans = []Orphan{{Provider: "gh", Project: "solo", Step: "protect", At: "2026-11-12T00:00:00Z"}}
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
			if !bytes.Equal(before, readFile(t, filepath.Join(dir, RegistryFileName))) {
				t.Fatal("invalid update changed state")
			}
		})
	}
	t.Run("an existing v3 file with a bad remote is broken", func(t *testing.T) {
		const tail = `"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`
		content := `{"version":3,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"remote":{"path":"a/../b","id":null},"auto_merge":{"enabled":false}}],` + tail
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, content, 0o600)
		if _, err := OpenRegistry(dir, LegacySource{}, regOpts()); !errors.Is(err, ErrRegistryBroken) || string(readFile(t, p)) != content {
			t.Fatalf("%v", err)
		}
		// the old key is not accepted in a v3 file
		content = strings.Replace(content, `"remote":{"path":"a/../b","id":null}`, `"gitlab":{"project":"a/b","project_id":null}`, 1)
		writeRegFile(t, dir, content, 0o600)
		if _, err := OpenRegistry(dir, LegacySource{}, regOpts()); !errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("gitlab key in v3: %v", err)
		}
	})
}

func TestRegistryRemotePathRulesForProviderLinkedProjects(t *testing.T) {
	ghp := Provider{ID: "gh", Kind: "github", BaseURL: "https://api.github.com", SSHURL: "ssh://git@github.com", TokenRef: "github-main"}
	seg := func(n int) string { return strings.Repeat("a", n) }
	try := func(prov *string, path string) error {
		st := mustOpen(t, stateDirFor(t), LegacySource{})
		return st.Update(func(r *Registry) error {
			r.Providers = []Provider{ghp}
			r.Projects = []Project{{Origin: OriginUnknown, ID: "a", Path: "/srv/a", Kind: "k", Provider: prov, Remote: &RemoteLink{Path: path}}}
			return nil
		})
	}
	for _, p := range []string{"o/r", "a/b/c/d/e", "Octo/Hello.v2", seg(100) + "/" + seg(100) + "/" + seg(53)} {
		if err := try(strp("gh"), p); err != nil {
			t.Errorf("provider-linked %q refused: %v", p, err)
		}
	}
	for _, p := range []string{"o/r.git", "o/r.git/x", "a/b/c/d/e/f", "o/r%2f", "o/./r", seg(100) + "/" + seg(100) + "/" + seg(54), "o/" + seg(101), "o/r s", "o/ré"} {
		if err := try(strp("gh"), p); err == nil {
			t.Errorf("provider-linked %q accepted", p)
		}
	}
	// legacy (no provider) keeps its loose rule but shares the 255-byte cap
	if err := try(nil, "grp/sub/proj"); err != nil {
		t.Errorf("legacy refused: %v", err)
	}
	if err := try(nil, seg(100)+"/"+seg(100)+"/"+seg(54)); err == nil {
		t.Error("legacy path over 255 bytes accepted")
	}
}

func TestV2ToV3WritesBackupOnceNeverOverwritten(t *testing.T) {
	dir := stateDirFor(t)
	p := writeRegFile(t, dir, v2Sample, 0o600)
	bak := p + ".v2.bak"
	st := mustOpen(t, dir, LegacySource{})
	data, err := os.ReadFile(bak)
	if err != nil || string(data) != v2Sample {
		t.Fatalf("backup missing or different: %v", err)
	}
	if fi, _ := os.Stat(bak); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
	if string(readFile(t, p)) != v4Want {
		t.Fatal("registry not migrated")
	}
	// later writes and starts never touch the backup
	if err := st.Update(func(r *Registry) error { r.AutoMergeGlobal.MaxPerDay = 11; return nil }); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, dir, LegacySource{})
	if got, _ := os.ReadFile(bak); string(got) != v2Sample {
		t.Fatal("backup changed")
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	// an existing backup is never overwritten, even by another v2 file
	dir2 := stateDirFor(t)
	p2 := writeRegFile(t, dir2, v2Sample, 0o600)
	if err := os.WriteFile(p2+".v2.bak", []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, dir2, LegacySource{})
	if got, _ := os.ReadFile(p2 + ".v2.bak"); string(got) != "keep me" {
		t.Fatal("existing backup overwritten")
	}
	// no backup when nothing is migrated (fresh and already-v3 registries), and none for a broken file
	dir3 := stateDirFor(t)
	mustOpen(t, dir3, LegacySource{})
	mustOpen(t, dir3, LegacySource{})
	dir4 := stateDirFor(t)
	writeRegFile(t, dir4, strings.Replace(v2Sample, `"version":2`, `"version":2,"x":1`, 1), 0o600)
	OpenRegistry(dir4, LegacySource{}, regOpts())
	for _, d := range []string{dir3, dir4} {
		if m, _ := filepath.Glob(filepath.Join(d, "*.bak")); len(m) != 0 {
			t.Fatalf("unexpected backup %v", m)
		}
	}
	// a backup that cannot be written is a start failure and leaves the v2 file alone
	if os.Geteuid() != 0 {
		dir5 := stateDirFor(t)
		p5 := writeRegFile(t, dir5, v2Sample, 0o600)
		os.Chmod(dir5, 0o500)
		defer os.Chmod(dir5, 0o700)
		if _, err := OpenRegistry(dir5, LegacySource{}, regOpts()); err == nil || errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("want a start failure, got %v", err)
		}
		os.Chmod(dir5, 0o700)
		if string(readFile(t, p5)) != v2Sample {
			t.Fatal("v2 file changed")
		}
	}
}
