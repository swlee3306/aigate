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

// v4Want is the v4 file the v3 sample (v3Want) must turn into (and the v2 and
// v1 samples too, straight through): same values, every project gets its
// origin. The legacy-kind project becomes "legacy", everything else "unknown".
var v4Want = func() string {
	s := strings.Replace(v3Want, `"version":3`, `"version":4`, 1)
	s = strings.ReplaceAll(s, `"kind":"registered","ai_context":true,"provider":"gl","remote":{"path":"team/sub/app-one","id":21},"auto_merge":{"enabled":false}}`,
		`"kind":"registered","ai_context":true,"provider":"gl","remote":{"path":"team/sub/app-one","id":21},"auto_merge":{"enabled":false},"origin":"unknown"}`)
	s = strings.ReplaceAll(s, `"remote":{"path":"grp/proj","id":null},"auto_merge":{"enabled":false}}`, `"remote":{"path":"grp/proj","id":null},"auto_merge":{"enabled":false},"origin":"legacy"}`)
	s = strings.ReplaceAll(s, `"remote":null,"auto_merge":{"enabled":false}}`, `"remote":null,"auto_merge":{"enabled":false},"origin":"unknown"}`)
	return s
}()

func TestRegistryV3MigratesToV4Atomically(t *testing.T) {
	if strings.Count(v4Want, `"origin":"`) != 4+2 { // four projects, two orphans carry their own origin
		t.Fatalf("v4Want is not what the test thinks: %s", v4Want)
	}
	t.Run("content unchanged, origin unknown/legacy, mode 0600, idempotent", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v3Want, 0o600)
		st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/ignored"}})
		reg := st.Snapshot()
		if reg.Version != 5 || len(reg.Providers) != 1 || len(reg.Projects) != 4 || len(reg.Orphans) != 2 || reg.AutoMergeGlobal.MaxPerDay != 9 {
			t.Fatalf("shape %+v", reg)
		}
		wantOrigin := map[string]string{"app-one": OriginUnknown, "legacy-a": OriginLegacy, "plain": OriginUnknown, "nolink": OriginUnknown}
		for _, pr := range reg.Projects {
			if pr.Origin != wantOrigin[pr.ID] || pr.RemoteDeletedAt != nil {
				t.Errorf("%s: origin %q remote_deleted_at %v", pr.ID, pr.Origin, pr.RemoteDeletedAt)
			}
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v", fi, err)
		}
		data := readFile(t, p)
		if string(data) != v5Want {
			t.Fatalf("file differs from the expected v5:\n%s\n%s", data, v5Want)
		}
		// everything except the version number and the new origin key is byte-identical to the v3 file
		stripped := strings.NewReplacer(`,"origin":"unknown"}`, `}`, `,"origin":"legacy"}`, `}`).Replace(string(data))
		// (the orphan records' own "origin":"created" is part of the v3 file: the replacer only matched project tails)
		if strings.Replace(stripped, `"version":5`, `"version":3`, 1) != v3Want {
			t.Fatalf("migration changed more than version and origin:\n%s\n%s", stripped, v3Want)
		}
		// project rows keep their ai_context, links and provider (never widened or narrowed)
		var a, b struct {
			Projects []map[string]any `json:"projects"`
		}
		json.Unmarshal([]byte(v3Want), &a)
		json.Unmarshal(data, &b)
		for i := range a.Projects {
			for k, v := range a.Projects[i] {
				if jsonOf(t, v) != jsonOf(t, b.Projects[i][k]) {
					t.Errorf("project %d key %s: %v -> %v", i, k, v, b.Projects[i][k])
				}
			}
			if len(b.Projects[i]) != len(a.Projects[i])+1 {
				t.Errorf("project %d gained keys other than origin: %v", i, b.Projects[i])
			}
		}
		if left := tempLeftovers(t, dir); len(left) != 0 {
			t.Fatalf("temp files left: %v", left)
		}
		if g, ok := st.GitLabProject("/srv/legacy-a"); !ok || g != "grp/proj" {
			t.Fatalf("GitLabProject %q %v", g, ok)
		}
		if got := st.AIContextPaths(); len(got) != 1 || got[0] != "/srv/app-one" {
			t.Fatalf("AI context paths %v", got)
		}
		before := readFile(t, p)
		mustOpen(t, dir, LegacySource{})
		if !bytes.Equal(before, readFile(t, p)) {
			t.Fatal("second open rewrote the file")
		}
	})
	t.Run("write failure leaves v3 untouched", func(t *testing.T) {
		for _, stage := range []string{"temp-written", "synced", "dir-synced"} {
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, v3Want, 0o600)
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
			if stage == "dir-synced" { // the rename already happened: v4 on disk, retry is a plain open
				after := readFile(t, p)
				if string(after) != v5Want {
					t.Fatalf("dir-synced: %s", after)
				}
				mustOpen(t, dir, LegacySource{})
				if !bytes.Equal(after, readFile(t, p)) {
					t.Fatal("dir-synced: retry changed the file")
				}
				continue
			}
			if string(readFile(t, p)) != v3Want {
				t.Fatalf("%s: v3 file changed", stage)
			}
			if left := tempLeftovers(t, dir); len(left) != 0 {
				t.Fatalf("%s: temp files left: %v", stage, left)
			}
			if st := mustOpen(t, dir, LegacySource{}); st.Snapshot().Version != 5 || string(readFile(t, p)) != v5Want {
				t.Fatalf("%s: retry did not migrate", stage)
			}
		}
	})
	t.Run("unwritable directory is a start failure and leaves v3", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory modes")
		}
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v3Want, 0o600)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		st, err := OpenRegistry(dir, LegacySource{}, regOpts())
		if err == nil || errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("want a start failure, got %v (%v)", err, st)
		}
		os.Chmod(dir, 0o700)
		if string(readFile(t, p)) != v3Want {
			t.Fatal("v3 file changed")
		}
	})
	t.Run("v1 and v2 files go straight to v4 with their own backup only", func(t *testing.T) {
		for name, c := range map[string]struct{ sample, bak string }{"v1": {v1Sample, ".v1.bak"}, "v2": {v2Sample, ".v2.bak"}} {
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, c.sample, 0o600)
			st := mustOpen(t, dir, LegacySource{})
			if st.Snapshot().Version != 5 || !strings.Contains(string(readFile(t, p)), `"version":5`) || strings.Contains(string(readFile(t, p)), `"gitlab":`) {
				t.Fatalf("%s: %s", name, readFile(t, p))
			}
			for _, pr := range st.Snapshot().Projects {
				if want := originForKind(pr.Kind); pr.Origin != want {
					t.Errorf("%s: %s origin %q want %q", name, pr.ID, pr.Origin, want)
				}
			}
			if got, err := os.ReadFile(p + c.bak); err != nil || string(got) != c.sample {
				t.Errorf("%s: backup missing or different: %v", name, err)
			}
			if _, err := os.Stat(p + ".v3.bak"); !os.IsNotExist(err) {
				t.Errorf("%s: a v3 backup appeared for a file that was never v3", name)
			}
		}
		// v2 reaches the same bytes as the v3 route
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v2Sample, 0o600)
		mustOpen(t, dir, LegacySource{})
		if string(readFile(t, p)) != v5Want {
			t.Fatalf("v2 -> v4 differs from v3 -> v4:\n%s", readFile(t, p))
		}
	})
	t.Run("broken or unowned v3 is never migrated or overwritten", func(t *testing.T) {
		const tail = `"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`
		proj := func(extra string) string {
			return `{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"remote":null,"auto_merge":{"enabled":false}` + extra + `}`
		}
		cases := map[string]string{
			"unknown key":            `{"version":3,"providers":[],"projects":[],"extra":1,` + tail,
			"project unknown key":    `{"version":3,"providers":[],"projects":[` + proj(`,"extra":1`) + `],` + tail,
			"v4 key origin in a v3":  `{"version":3,"providers":[],"projects":[` + proj(`,"origin":"unknown"`) + `],` + tail,
			"v4 key remote_deleted":  `{"version":3,"providers":[],"projects":[` + proj(`,"remote_deleted_at":"2026-11-12T00:00:00Z"`) + `],` + tail,
			"auto merge on":          `{"version":3,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"remote":null,"auto_merge":{"enabled":true}}],` + tail,
			"duplicate id":           `{"version":3,"providers":[],"projects":[` + proj(``) + `,` + proj(``) + `],` + tail,
			"trailing data":          `{"version":3,"providers":[],"projects":[],` + tail + `{}`,
			"truncated":              `{"version":3,"providers":[],"projects":[`,
			"version as string":      `{"version":"3","providers":[],"projects":[],` + tail,
			"mixed-case Version":     `{"version":3,"Version":4,"providers":[],"projects":[],` + tail,
			"unknown version 6":      `{"version":6,"providers":[],"projects":[],` + tail,
			"unknown version 0":      `{"version":0,"providers":[],"projects":[],` + tail,
			"remote id w/o provider": `{"version":3,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"remote":{"path":"o/r","id":5},"auto_merge":{"enabled":false}}],` + tail,
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
				if m, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(m) != 0 {
					t.Fatalf("backup written for a broken file: %v", m)
				}
			})
		}
		for name, c := range map[string]struct {
			mode  os.FileMode
			owner func(uint32) bool
		}{"mode 0644": {0o644, nil}, "mode 0660": {0o660, nil}, "foreign owner": {0o600, func(uint32) bool { return false }}} {
			t.Run(name, func(t *testing.T) {
				dir := stateDirFor(t)
				p := writeRegFile(t, dir, v3Want, c.mode)
				o := regOpts()
				if c.owner != nil {
					o.fileOwnerOK = c.owner
				}
				st, err := OpenRegistry(dir, LegacySource{}, o)
				if !errors.Is(err, ErrRegistryBroken) || st.Err() == nil {
					t.Fatalf("not refused: %v", err)
				}
				if string(readFile(t, p)) != v3Want {
					t.Fatal("file was modified")
				}
				if m, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(m) != 0 {
					t.Fatalf("backup written: %v", m)
				}
			})
		}
	})
	t.Run("status check never migrates", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v3Want, 0o600)
		if exists, err := RegistryStatus(dir, regOpts()); !exists || err != nil {
			t.Fatalf("%v %v", exists, err)
		}
		if string(readFile(t, p)) != v3Want {
			t.Fatal("RegistryStatus wrote the file")
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(m) != 0 {
			t.Fatalf("RegistryStatus wrote a backup: %v", m)
		}
	})
}

func TestV3ToV4WritesBackupOnceNeverOverwritten(t *testing.T) {
	dir := stateDirFor(t)
	p := writeRegFile(t, dir, v3Want, 0o600)
	bak := p + ".v3.bak"
	st := mustOpen(t, dir, LegacySource{})
	data, err := os.ReadFile(bak)
	if err != nil || string(data) != v3Want {
		t.Fatalf("backup missing or different: %v", err)
	}
	if fi, _ := os.Stat(bak); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
	if string(readFile(t, p)) != v5Want {
		t.Fatal("registry not migrated")
	}
	// later writes and starts never touch the backup
	if err := st.Update(func(r *Registry) error { r.AutoMergeGlobal.MaxPerDay = 11; return nil }); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, dir, LegacySource{})
	if got, _ := os.ReadFile(bak); string(got) != v3Want {
		t.Fatal("backup changed")
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	// an existing backup is never overwritten, even by another v3 file
	dir2 := stateDirFor(t)
	p2 := writeRegFile(t, dir2, v3Want, 0o600)
	if err := os.WriteFile(p2+".v3.bak", []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, dir2, LegacySource{})
	if got, _ := os.ReadFile(p2 + ".v3.bak"); string(got) != "keep me" {
		t.Fatal("existing backup overwritten")
	}
	// the backup exists BEFORE the first rewrite: a failure at the first write stage finds it already there
	dir3 := stateDirFor(t)
	p3 := writeRegFile(t, dir3, v3Want, 0o600)
	o := regOpts()
	sawBackup := false
	o.failAt = func(stage string) error {
		if got, err := os.ReadFile(p3 + ".v3.bak"); err == nil && string(got) == v3Want {
			sawBackup = true
		}
		return errors.New("boom")
	}
	if _, err := OpenRegistry(dir3, LegacySource{}, o); err == nil || !sawBackup {
		t.Fatalf("backup was not on disk before the rewrite started: err=%v saw=%v", err, sawBackup)
	}
	// no backup when nothing is migrated (fresh and already-v4 registries) and none for a broken file
	dir4 := stateDirFor(t)
	mustOpen(t, dir4, LegacySource{})
	mustOpen(t, dir4, LegacySource{})
	dir5 := stateDirFor(t)
	writeRegFile(t, dir5, strings.Replace(v3Want, `"version":3`, `"version":3,"x":1`, 1), 0o600)
	OpenRegistry(dir5, LegacySource{}, regOpts())
	for _, d := range []string{dir4, dir5} {
		if m, _ := filepath.Glob(filepath.Join(d, "*.bak")); len(m) != 0 {
			t.Fatalf("unexpected backup %v", m)
		}
	}
	// a backup that cannot be written is a start failure and leaves the v3 file alone
	if os.Geteuid() != 0 {
		dir6 := stateDirFor(t)
		p6 := writeRegFile(t, dir6, v3Want, 0o600)
		os.Chmod(dir6, 0o500)
		defer os.Chmod(dir6, 0o700)
		if _, err := OpenRegistry(dir6, LegacySource{}, regOpts()); err == nil || errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("want a start failure, got %v", err)
		}
		os.Chmod(dir6, 0o700)
		if string(readFile(t, p6)) != v3Want {
			t.Fatal("v3 file changed")
		}
	}
}

// A W3e broker (registry version 3) must treat a v4 file as broken and serve
// nothing: rolling the binary back needs registry.json.v3.bak. This pins the
// constants and replays a copy of the W3e strict loader on a real v4 file.
func TestOldBrokerRefusesV4(t *testing.T) {
	if registryVersion != 5 || registryVersionV3 != 3 || registryVersionV2 != 2 || registryVersionV1 != 1 {
		t.Fatalf("version constants moved: current=%d v3=%d v2=%d v1=%d", registryVersion, registryVersionV3, registryVersionV2, registryVersionV1)
	}
	if emptyRegistry().Version != 5 {
		t.Fatal("fresh registry is not v4")
	}
	// W3e loader, copied: strict decode into the v3 shape (no origin), then version == 3.
	type w3eProject struct {
		ID        string      `json:"id"`
		Path      string      `json:"path"`
		Kind      string      `json:"kind"`
		AIContext bool        `json:"ai_context"`
		Provider  *string     `json:"provider"`
		Remote    *RemoteLink `json:"remote"`
		AutoMerge AutoMerge   `json:"auto_merge"`
	}
	type w3eRegistry struct {
		Version         int           `json:"version"`
		Providers       []Provider    `json:"providers"`
		Projects        []w3eProject  `json:"projects"`
		AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
		Orphans         []Orphan      `json:"orphans"`
	}
	oldAccepts := func(b []byte) bool {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var r w3eRegistry
		return dec.Decode(&r) == nil && r.Version == 3
	}
	if !oldAccepts([]byte(v3Want)) {
		t.Fatal("replica of the W3e loader rejects a v3 file (the check below would mean nothing)")
	}
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/a"}})
	data := readFile(t, filepath.Join(dir, RegistryFileName))
	if oldAccepts(data) {
		t.Fatalf("a v4 file would be accepted by W3e code: %s", data)
	}
	// with a project in it, forcing the version back to 3 still fails on the new key
	var f map[string]any
	json.Unmarshal(data, &f)
	if f["version"] != float64(5) {
		t.Fatalf("file version %v", f["version"])
	}
	if ps, _ := f["projects"].([]any); len(ps) != 1 {
		t.Fatalf("expected the migrated legacy project: %v", f["projects"])
	}
	f["version"] = 3
	forced, _ := json.Marshal(f)
	if oldAccepts(forced) {
		t.Fatal("the v4 origin key would pass the W3e strict decoder")
	}
	_ = st
}

func TestRegistryV4StrictParser(t *testing.T) {
	const tail = `"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`
	row := func(kind, origin, extra string) string {
		o := ""
		if origin != "" {
			o = `,"origin":` + origin
		}
		return `{"id":"a","path":"/x/a","kind":"` + kind + `","ai_context":false,"provider":null,"remote":null,"auto_merge":{"enabled":false}` + o + extra + `}`
	}
	file := func(rows ...string) string {
		return `{"version":4,"providers":[],"projects":[` + strings.Join(rows, ",") + `],` + tail
	}
	// origin created now requires a provider (F2 fix round): those rows carry one
	provJSON := `{"id":"gl","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t"}`
	fileP := func(rows ...string) string {
		return strings.Replace(file(rows...), `"providers":[]`, `"providers":[`+provJSON+`]`, 1)
	}
	withProv := func(r string) string { return strings.Replace(r, `"provider":null`, `"provider":"gl"`, 1) }
	ok := map[string]string{
		"created":         fileP(withProv(row("registered", `"created"`, ``))),
		"linked":          file(row("registered", `"linked"`, ``)),
		"unknown":         file(row("registered", `"unknown"`, ``)),
		"legacy":          file(row("legacy", `"legacy"`, ``)),
		"remote_deleted":  fileP(withProv(row("registered", `"created"`, `,"remote_deleted_at":"2026-11-12T01:02:03Z"`))),
		"no projects":     file(),
		"empty origin ok": "", // placeholder replaced below
	}
	delete(ok, "empty origin ok")
	for name, content := range ok {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, content, 0o600)
		st, err := OpenRegistry(dir, LegacySource{}, regOpts())
		if err != nil || st.Err() != nil {
			t.Errorf("%s refused: %v", name, err)
			continue
		}
		// W3h: a valid v4 file is migrated to v5 with nothing but the version changed
		if string(readFile(t, p)) != v5Of(content)+"\n" { // (the broker writes a final newline)
			t.Errorf("%s: a valid v4 file was not migrated as it is", name)
		}
	}
	bad := map[string]string{
		"origin missing":                  file(row("registered", ``, ``)),
		"origin null":                     file(row("registered", `null`, ``)),
		"origin empty":                    file(row("registered", `""`, ``)),
		"origin adopted":                  file(row("registered", `"adopted"`, ``)),
		"origin upper":                    file(row("registered", `"Created"`, ``)),
		"origin number":                   file(row("registered", `1`, ``)),
		"legacy kind with origin created": file(row("legacy", `"created"`, ``)),
		"legacy origin with registered":   file(row("registered", `"legacy"`, ``)),
		"unknown key":                     file(row("registered", `"unknown"`, `,"x":1`)),
		"remote_deleted bad format":       file(row("registered", `"created"`, `,"remote_deleted_at":"2026-11-12"`)),
		"remote_deleted with offset":      file(row("registered", `"created"`, `,"remote_deleted_at":"2026-11-12T01:02:03+09:00"`)),
		"remote_deleted fractional":       file(row("registered", `"created"`, `,"remote_deleted_at":"2026-11-12T01:02:03.5Z"`)),
		"remote_deleted empty":            file(row("registered", `"created"`, `,"remote_deleted_at":""`)),
		"remote_deleted number":           file(row("registered", `"created"`, `,"remote_deleted_at":5`)),
		"remote_deleted origin linked":    file(row("registered", `"linked"`, `,"remote_deleted_at":"2026-11-12T01:02:03Z"`)),
		"remote_deleted origin unknown":   file(row("registered", `"unknown"`, `,"remote_deleted_at":"2026-11-12T01:02:03Z"`)),
		"remote_deleted while remote set": `{"version":4,"providers":[{"id":"gl","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t"}],"projects":[{"id":"a","path":"/x/a","kind":"registered","ai_context":false,"provider":"gl","remote":{"path":"g/a","id":5},"auto_merge":{"enabled":false},"origin":"created","remote_deleted_at":"2026-11-12T01:02:03Z"}],` + tail,
		"v3 style gitlab key in v4":       strings.Replace(file(row("registered", `"unknown"`, ``)), `"remote":null`, `"gitlab":null`, 1),
		"trailing data":                   file(row("registered", `"unknown"`, ``)) + `{}`,
		"auto merge on":                   strings.Replace(file(row("registered", `"unknown"`, ``)), `"auto_merge":{"enabled":false}`, `"auto_merge":{"enabled":true}`, 1),
		"version 6":                       strings.Replace(file(), `"version":4`, `"version":6`, 1),
		"version as float":                strings.Replace(file(), `"version":4`, `"version":4.0`, 1),
		"two version keys":                strings.Replace(file(), `"version":4`, `"version":4,"version":4`, 1),
	}
	for name, content := range bad {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, content, 0o600)
		st, err := OpenRegistry(dir, LegacySource{ContextRepos: []string{"/srv/legacy"}}, regOpts())
		if !errors.Is(err, ErrRegistryBroken) || st.Err() == nil {
			t.Errorf("%s: not refused: %v", name, err)
			continue
		}
		if string(readFile(t, p)) != content {
			t.Errorf("%s: broken file was modified", name)
		}
	}
	// the same rules apply to every write
	st := mustOpen(t, stateDirFor(t), LegacySource{})
	created := OriginCreated
	_ = created
	stamp := "2026-11-12T01:02:03Z"
	badUpdates := map[string]func(*Registry){
		"origin empty": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k"}}
		},
		"origin bogus": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: "adopted"}}
		},
		"legacy origin non-legacy kind": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginLegacy}}
		},
		"legacy kind non-legacy origin": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: legacyKind, Origin: OriginUnknown}}
		},
		"remote_deleted for linked": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginLinked, RemoteDeletedAt: &stamp}}
		},
		"remote_deleted bad stamp": func(r *Registry) {
			bad := "yesterday"
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginCreated, RemoteDeletedAt: &bad}}
		},
	}
	for name, f := range badUpdates {
		if err := st.Update(func(r *Registry) error { f(r); return nil }); err == nil {
			t.Errorf("update %q accepted", name)
		}
	}
	if err := st.Update(func(r *Registry) error {
		prov := "gl"
		r.Providers = []Provider{{ID: "gl", Kind: "gitlab", BaseURL: "http://h", SSHURL: "ssh://git@h", TokenRef: "t"}}
		r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginCreated, Provider: &prov, RemoteDeletedAt: &stamp}}
		return nil
	}); err != nil {
		t.Fatalf("valid remote_deleted_at refused: %v", err)
	}
	// a Snapshot is a deep copy: editing RemoteDeletedAt in it does not touch the store
	snap := st.Snapshot()
	*snap.Projects[0].RemoteDeletedAt = "x"
	if got := st.Snapshot().Projects[0].RemoteDeletedAt; got == nil || *got != "2026-11-12T01:02:03Z" {
		t.Fatal("snapshot shares RemoteDeletedAt with the store")
	}
}

func TestNewProjectsRecordOrigin(t *testing.T) {
	e := newE3(t)
	e.existing()
	origin := func(id string) string {
		p, ok := e.find(id)
		if !ok {
			t.Fatalf("project %s not in the registry", id)
		}
		return p.Origin
	}
	// create_project (GitHub and GitLab): created
	if _, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil)); oe != nil {
		t.Fatalf("github create: %v", oe.Code)
	}
	if got := origin(c3Name); got != OriginCreated {
		t.Fatalf("github create_project origin %q", got)
	}
	if _, oe := e.do("create_project", e.projectArgs("demo-gl", nil)); oe != nil {
		t.Fatalf("gitlab create: %v", oe.Code)
	}
	if got := origin("demo-gl"); got != OriginCreated {
		t.Fatalf("gitlab create_project origin %q", got)
	}
	// link_project (GitHub and GitLab): linked
	if _, oe := e.do("link_project", e.linkArgs("linked-gh", "gh", "alice/existing", false, nil)); oe != nil {
		t.Fatalf("link gh: %v", oe.Code)
	}
	if _, oe := e.do("link_project", e.linkArgs("linked-gl", "gl", "team/exist", false, nil)); oe != nil {
		t.Fatalf("link gl: %v", oe.Code)
	}
	if origin("linked-gh") != OriginLinked || origin("linked-gl") != OriginLinked {
		t.Fatalf("link_project origins %q %q", origin("linked-gh"), origin("linked-gl"))
	}
	// register_project: nothing proves who made the repository -> unknown (legacy kind -> legacy)
	reg := func(id, kind string) *OpError {
		_, oe := e.do("register_project", e.signedArgs(e.alice, "register_project", id, map[string]any{"path": e.root + "/" + id, "kind": kind, "ai_context": false}))
		return oe
	}
	if oe := reg("reg-plain", "registered"); oe != nil {
		t.Fatalf("register: %v", oe.Code)
	}
	if oe := reg("reg-legacy", legacyKind); oe != nil {
		t.Fatalf("register legacy kind: %v", oe.Code)
	}
	if origin("reg-plain") != OriginUnknown || origin("reg-legacy") != OriginLegacy {
		t.Fatalf("register_project origins %q %q", origin("reg-plain"), origin("reg-legacy"))
	}
	// attach_remote never changes an existing project's origin (nor anything but provider and remote)
	if oe := e.s.Registry.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: "old-legacy", Path: e.root + "/old-legacy", Kind: legacyKind, Origin: OriginLegacy})
		return nil
	}); oe != nil {
		t.Fatal(oe)
	}
	for i, id := range []string{"reg-plain", "old-legacy"} {
		repo := []string{"alice/att-one", "alice/att-two"}[i]
		e.gh.mu.Lock()
		e.gh.repos[repo] = repo
		e.gh.mu.Unlock()
		before, _ := e.find(id)
		args := e.attachArgs(id, "gh", repo, nil)
		if _, oe := e.do("attach_remote", args); oe != nil {
			t.Fatalf("attach %s: %v", id, oe.Code)
		}
		after, _ := e.find(id)
		if after.Origin != before.Origin || after.Origin == OriginLinked || after.Kind != before.Kind {
			t.Fatalf("attach changed origin or kind of %s: %+v -> %+v", id, before, after)
		}
	}
	// the broker's read op carries the origin of every row
	res, oe := e.do("projects", map[string]any{})
	if oe != nil {
		t.Fatal(oe)
	}
	var out struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal([]byte(jsonOf(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range out.Projects {
		got[p["id"].(string)], _ = p["origin"].(string)
	}
	for id, want := range map[string]string{c3Name: OriginCreated, "demo-gl": OriginCreated, "linked-gh": OriginLinked, "linked-gl": OriginLinked, "reg-plain": OriginUnknown, "old-legacy": OriginLegacy} {
		if got[id] != want {
			t.Errorf("projects read: %s origin %q want %q", id, got[id], want)
		}
	}
	// the legacy migration of broker.json entries: legacy
	l := mustOpen(t, stateDirFor(t), LegacySource{ContextRepos: []string{"/srv/repo-a"}, GitLabProjects: map[string]string{"/srv/gl-only": "grp/p"}})
	for _, p := range l.Snapshot().Projects {
		if p.Origin != OriginLegacy {
			t.Errorf("migrated broker.json project %s origin %q", p.ID, p.Origin)
		}
	}
}
