package broker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W3h (H-D1): registry v5. RemoteLink gains an optional key_sha256 (the signed
// deploy_key_sha256 of the registration, lowercase hex, 64 characters); a v4
// file is rewritten as v5 with every row unchanged (no hash), after a once-only
// registry.json.v4.bak.

// v4Rich is a v4 file whose rows carry every v4 property a careless migration
// could lose: origins created/linked/unknown/legacy and a remote_deleted_at row.
const v4Rich = `{"version":4,"providers":[` +
	`{"id":"gl","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t"},` +
	`{"id":"gh","kind":"github","base_url":"https://api.github.com","ssh_url":"ssh://git@github.com","token_ref":"g"}` +
	`],"projects":[` +
	`{"id":"made","path":"/srv/made","kind":"registered","ai_context":true,"provider":"gl","remote":{"path":"team/made","id":21},"auto_merge":{"enabled":false},"origin":"created"},` +
	`{"id":"gone","path":"/srv/gone","kind":"registered","ai_context":false,"provider":"gh","remote":null,"auto_merge":{"enabled":false},"origin":"created","remote_deleted_at":"2026-11-12T01:02:03Z"},` +
	`{"id":"took","path":"/srv/took","kind":"registered","ai_context":false,"provider":"gh","remote":{"path":"alice/Took","id":9},"auto_merge":{"enabled":false},"origin":"linked"},` +
	`{"id":"plain","path":"/srv/plain","kind":"registered","ai_context":false,"provider":null,"remote":null,"auto_merge":{"enabled":false},"origin":"unknown"},` +
	`{"id":"old","path":"/srv/old","kind":"legacy","ai_context":false,"provider":null,"remote":{"path":"grp/proj","id":null},"auto_merge":{"enabled":false},"origin":"legacy"}` +
	`],"auto_merge_global":{"enabled":false,"max_per_day":9},"orphans":[` +
	`{"provider":"gl","project":"team/half","project_id":33,"step":"protect","at":"2026-11-10T00:00:00Z","origin":"linked"}]}` + "\n"

// v5Of is a v4 file as v5 must write it: only the version token differs.
func v5Of(v4 string) string { return strings.Replace(v4, `"version":4`, `"version":5`, 1) }

var v5Want = v5Of(v4Want)

func hashOfLine(line string) string {
	s := sha256.Sum256([]byte(line))
	return hex.EncodeToString(s[:])
}

func TestRegistryV4MigratesToV5Atomically(t *testing.T) {
	for name, sample := range map[string]string{"v4Want": v4Want, "v4Rich": v4Rich} {
		t.Run(name+": rows unchanged, only the version moves, idempotent", func(t *testing.T) {
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, sample, 0o600)
			st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/ignored"}})
			if got := string(readFile(t, p)); got != v5Of(sample) {
				t.Fatalf("file differs from the expected v5:\n%s\n%s", got, v5Of(sample))
			}
			if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("mode %v %v", fi, err)
			}
			// the in-memory rows equal the v4 rows decoded as they are (origins kept, no hash)
			var v4 Registry
			if err := json.Unmarshal([]byte(sample), &v4); err != nil {
				t.Fatal(err)
			}
			got := st.Snapshot()
			if got.Version != 5 || jsonOf(t, got.Projects) != jsonOf(t, v4.Projects) || jsonOf(t, got.Orphans) != jsonOf(t, v4.Orphans) {
				t.Fatalf("rows changed:\n%s\n%s", jsonOf(t, got.Projects), jsonOf(t, v4.Projects))
			}
			for _, pr := range got.Projects {
				if pr.Remote != nil && pr.Remote.KeySHA256 != nil {
					t.Fatalf("%s: the migration invented a key hash", pr.ID)
				}
			}
			if left := tempLeftovers(t, dir); len(left) != 0 {
				t.Fatalf("temp files left: %v", left)
			}
			before := readFile(t, p)
			mustOpen(t, dir, LegacySource{})
			if !bytes.Equal(before, readFile(t, p)) {
				t.Fatal("second open rewrote the file")
			}
		})
	}
	t.Run("write failure leaves v4 untouched, retry migrates", func(t *testing.T) {
		for _, stage := range []string{"temp-written", "synced", "dir-synced"} {
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, v4Rich, 0o600)
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
			if stage == "dir-synced" { // renamed already: v5 on disk, a retry is a plain open
				if string(readFile(t, p)) != v5Of(v4Rich) {
					t.Fatalf("dir-synced: %s", readFile(t, p))
				}
			} else {
				if string(readFile(t, p)) != v4Rich {
					t.Fatalf("%s: v4 file changed", stage)
				}
				if left := tempLeftovers(t, dir); len(left) != 0 {
					t.Fatalf("%s: temp files left: %v", stage, left)
				}
			}
			if st := mustOpen(t, dir, LegacySource{}); st.Snapshot().Version != 5 || string(readFile(t, p)) != v5Of(v4Rich) {
				t.Fatalf("%s: retry did not migrate", stage)
			}
			if got, _ := os.ReadFile(p + ".v4.bak"); string(got) != v4Rich {
				t.Fatalf("%s: backup %q", stage, got)
			}
		}
	})
	t.Run("older files go straight to v5 with their own backup only", func(t *testing.T) {
		for name, c := range map[string]struct{ sample, bak string }{"v1": {v1Sample, ".v1.bak"}, "v2": {v2Sample, ".v2.bak"}, "v3": {v3Want, ".v3.bak"}} {
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, c.sample, 0o600)
			if st := mustOpen(t, dir, LegacySource{}); st.Snapshot().Version != 5 {
				t.Fatalf("%s: version %d", name, st.Snapshot().Version)
			}
			if got, err := os.ReadFile(p + c.bak); err != nil || string(got) != c.sample {
				t.Errorf("%s: backup missing or different: %v", name, err)
			}
			if _, err := os.Stat(p + ".v4.bak"); !os.IsNotExist(err) {
				t.Errorf("%s: a v4 backup appeared for a file that was never v4", name)
			}
		}
	})
	t.Run("a v4 file is decoded in the v4 shape: key_sha256 in it is broken", func(t *testing.T) {
		h := `"key_sha256":"` + strings.Repeat("a", 64) + `"`
		bad := map[string]string{
			"v4 with key_sha256": strings.Replace(v4Rich, `"remote":{"path":"team/made","id":21}`, `"remote":{"path":"team/made","id":21,`+h+`}`, 1),
			"v3 with key_sha256": strings.Replace(v3Want, `"remote":{"path":"team/sub/app-one","id":21}`, `"remote":{"path":"team/sub/app-one","id":21,`+h+`}`, 1),
		}
		for name, content := range bad {
			if content == v4Rich || content == v3Want {
				t.Fatalf("%s: the replacement did not apply", name)
			}
			dir := stateDirFor(t)
			p := writeRegFile(t, dir, content, 0o600)
			st, err := OpenRegistry(dir, LegacySource{}, regOpts())
			if !errors.Is(err, ErrRegistryBroken) || st.Err() == nil {
				t.Errorf("%s: not refused: %v", name, err)
			}
			if string(readFile(t, p)) != content {
				t.Errorf("%s: file modified", name)
			}
			if m, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(m) != 0 {
				t.Errorf("%s: backup written for a broken file: %v", name, m)
			}
		}
	})
	t.Run("status check never migrates", func(t *testing.T) {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, v4Rich, 0o600)
		if exists, err := RegistryStatus(dir, regOpts()); !exists || err != nil {
			t.Fatalf("%v %v", exists, err)
		}
		if string(readFile(t, p)) != v4Rich {
			t.Fatal("RegistryStatus wrote the file")
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(m) != 0 {
			t.Fatalf("RegistryStatus wrote a backup: %v", m)
		}
	})
}

func TestV4ToV5WritesBackupOnceNeverOverwritten(t *testing.T) {
	dir := stateDirFor(t)
	p := writeRegFile(t, dir, v4Rich, 0o600)
	bak := p + ".v4.bak"
	st := mustOpen(t, dir, LegacySource{})
	if data, err := os.ReadFile(bak); err != nil || string(data) != v4Rich {
		t.Fatalf("backup missing or different: %v", err)
	}
	if fi, _ := os.Stat(bak); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
	// later writes and starts never touch the backup
	if err := st.Update(func(r *Registry) error { r.AutoMergeGlobal.MaxPerDay = 11; return nil }); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, dir, LegacySource{})
	if got, _ := os.ReadFile(bak); string(got) != v4Rich {
		t.Fatal("backup changed")
	}
	// an existing backup is never overwritten, even by another v4 file
	dir2 := stateDirFor(t)
	p2 := writeRegFile(t, dir2, v4Rich, 0o600)
	if err := os.WriteFile(p2+".v4.bak", []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, dir2, LegacySource{})
	if got, _ := os.ReadFile(p2 + ".v4.bak"); string(got) != "keep me" {
		t.Fatal("existing backup overwritten")
	}
	if string(readFile(t, p2)) != v5Of(v4Rich) {
		t.Fatal("registry not migrated next to an existing backup")
	}
	// the backup exists BEFORE the rewrite starts
	dir3 := stateDirFor(t)
	p3 := writeRegFile(t, dir3, v4Rich, 0o600)
	o := regOpts()
	saw := false
	o.failAt = func(string) error {
		if got, err := os.ReadFile(p3 + ".v4.bak"); err == nil && string(got) == v4Rich {
			saw = true
		}
		return errors.New("boom")
	}
	if _, err := OpenRegistry(dir3, LegacySource{}, o); err == nil || !saw {
		t.Fatalf("backup not on disk before the rewrite: err=%v saw=%v", err, saw)
	}
	// no backup for a fresh or an already-v5 registry
	dir4 := stateDirFor(t)
	mustOpen(t, dir4, LegacySource{})
	mustOpen(t, dir4, LegacySource{})
	if m, _ := filepath.Glob(filepath.Join(dir4, "*.bak")); len(m) != 0 {
		t.Fatalf("unexpected backup %v", m)
	}
	// a backup that cannot be written is a start failure and leaves the v4 file alone
	if os.Geteuid() != 0 {
		dir6 := stateDirFor(t)
		p6 := writeRegFile(t, dir6, v4Rich, 0o600)
		os.Chmod(dir6, 0o500)
		defer os.Chmod(dir6, 0o700)
		if _, err := OpenRegistry(dir6, LegacySource{}, regOpts()); err == nil || errors.Is(err, ErrRegistryBroken) {
			t.Fatalf("want a start failure, got %v", err)
		}
		os.Chmod(dir6, 0o700)
		if string(readFile(t, p6)) != v4Rich {
			t.Fatal("v4 file changed")
		}
	}
}

// A W3g broker (registry version 4) must refuse a v5 file: rolling the binary
// back needs registry.json.v4.bak. This pins the constants and replays a copy
// of the W3g strict loader (the v4 shape: remote is {path,id}).
func TestOldBrokerRefusesV5(t *testing.T) {
	if registryVersion != 5 || registryVersionV4 != 4 || registryVersionV3 != 3 || registryVersionV2 != 2 || registryVersionV1 != 1 {
		t.Fatalf("version constants moved: current=%d v4=%d v3=%d v2=%d v1=%d", registryVersion, registryVersionV4, registryVersionV3, registryVersionV2, registryVersionV1)
	}
	if emptyRegistry().Version != 5 {
		t.Fatal("fresh registry is not v5")
	}
	type w3gRemote struct {
		Path string `json:"path"`
		ID   *int64 `json:"id"`
	}
	type w3gProject struct {
		ID              string     `json:"id"`
		Path            string     `json:"path"`
		Kind            string     `json:"kind"`
		AIContext       bool       `json:"ai_context"`
		Provider        *string    `json:"provider"`
		Remote          *w3gRemote `json:"remote"`
		AutoMerge       AutoMerge  `json:"auto_merge"`
		Origin          string     `json:"origin"`
		RemoteDeletedAt *string    `json:"remote_deleted_at,omitempty"`
	}
	type w3gRegistry struct {
		Version         int           `json:"version"`
		Providers       []Provider    `json:"providers"`
		Projects        []w3gProject  `json:"projects"`
		AutoMergeGlobal AutoMergeGlob `json:"auto_merge_global"`
		Orphans         []Orphan      `json:"orphans"`
	}
	oldAccepts := func(b []byte) bool {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var r w3gRegistry
		return dec.Decode(&r) == nil && r.Version == 4
	}
	if !oldAccepts([]byte(v4Rich)) || !oldAccepts([]byte(v4Want)) {
		t.Fatal("replica of the W3g loader rejects a v4 file (the check below would mean nothing)")
	}
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/a"}})
	path := filepath.Join(dir, RegistryFileName)
	if oldAccepts(readFile(t, path)) {
		t.Fatalf("a v5 file would be accepted by W3g code: %s", readFile(t, path))
	}
	// with a row that carries the new key, even a version forced back to 4 is refused
	h := hashOfLine(goodKey)
	if err := st.Update(func(r *Registry) error {
		r.Providers = []Provider{testProvider("gl")}
		id := int64(7)
		r.Projects = append(r.Projects, Project{Origin: OriginLinked, ID: "hashed", Path: "/srv/hashed", Kind: "registered", Provider: strp("gl"),
			Remote: &RemoteLink{Path: "team/hashed", ID: &id, KeySHA256: &h}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, path)
	if !strings.Contains(string(data), `"key_sha256":"`+h+`"`) {
		t.Fatalf("the hash is not in the file: %s", data)
	}
	var f map[string]any
	json.Unmarshal(data, &f)
	if f["version"] != float64(5) {
		t.Fatalf("file version %v", f["version"])
	}
	f["version"] = 4
	forced, _ := json.Marshal(f)
	if oldAccepts(forced) {
		t.Fatal("the v5 key_sha256 would pass the W3g strict decoder")
	}
	// (by design: a v5 file WITHOUT any hash and its version edited back to 4 is a
	// valid v4 file; the documented rollback restores registry.json.v4.bak instead)
}

func TestRegistryV5Rules(t *testing.T) {
	const tail = `"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`
	provs := `[{"id":"gl","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t"}]`
	h := hashOfLine(goodKey)
	row := func(provider, remote string) string {
		return `{"id":"a","path":"/x/a","kind":"registered","ai_context":false,"provider":` + provider + `,"remote":` + remote + `,"auto_merge":{"enabled":false},"origin":"linked"}`
	}
	file := func(r string) string { return `{"version":5,"providers":` + provs + `,"projects":[` + r + `],` + tail }
	ok := map[string]string{
		"hash with provider and remote": file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":"`+h+`"}`)),
		"hash with remote id null":      file(row(`"gl"`, `{"path":"team/a","id":null,"key_sha256":"`+h+`"}`)),
		"no hash (older row)":           file(row(`"gl"`, `{"path":"team/a","id":5}`)),
		"no remote":                     file(row(`"gl"`, `null`)),
		"no provider no remote":         file(row(`null`, `null`)),
	}
	for name, content := range ok {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, content, 0o600)
		st, err := OpenRegistry(dir, LegacySource{}, regOpts())
		if err != nil || st.Err() != nil {
			t.Errorf("%s refused: %v", name, err)
			continue
		}
		if string(readFile(t, p)) != content {
			t.Errorf("%s: a valid v5 file was rewritten", name)
		}
	}
	bad := map[string]string{
		"hash without provider":   file(row(`null`, `{"path":"team/a","id":null,"key_sha256":"`+h+`"}`)),
		"hash upper case":         file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":"`+strings.ToUpper(h)+`"}`)),
		"hash 63 chars":           file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":"`+h[:63]+`"}`)),
		"hash 65 chars":           file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":"`+h+`0"}`)),
		"hash empty":              file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":""}`)),
		"hash not hex":            file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":"`+strings.Repeat("g", 64)+`"}`)),
		"hash with prefix":        file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":"sha256:`+h[:57]+`"}`)),
		"hash number":             file(row(`"gl"`, `{"path":"team/a","id":5,"key_sha256":5}`)),
		"hash on the row":         strings.Replace(file(row(`"gl"`, `{"path":"team/a","id":5}`)), `"origin":"linked"`, `"origin":"linked","key_sha256":"`+h+`"`, 1),
		"remote unknown key":      file(row(`"gl"`, `{"path":"team/a","id":5,"key":"x"}`)),
		"version 6":               strings.Replace(file(row(`"gl"`, `null`)), `"version":5`, `"version":6`, 1),
		"version as string":       strings.Replace(file(row(`"gl"`, `null`)), `"version":5`, `"version":"5"`, 1),
		"origin missing (v5 too)": strings.Replace(file(row(`"gl"`, `null`)), `,"origin":"linked"`, ``, 1),
	}
	for name, content := range bad {
		dir := stateDirFor(t)
		p := writeRegFile(t, dir, content, 0o600)
		st, err := OpenRegistry(dir, LegacySource{}, regOpts())
		if !errors.Is(err, ErrRegistryBroken) || st.Err() == nil {
			t.Errorf("%s: not refused: %v", name, err)
			continue
		}
		if string(readFile(t, p)) != content {
			t.Errorf("%s: broken file modified", name)
		}
	}
	// the same rules apply to every write
	st := mustOpen(t, stateDirFor(t), LegacySource{})
	id := int64(5)
	upper := strings.ToUpper(h)
	short := h[:10]
	empty := ""
	for name, f := range map[string]func(*Registry){
		"hash without provider": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginUnknown, Remote: &RemoteLink{Path: "team/a", KeySHA256: &h}}}
		},
		"hash upper": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginLinked, Provider: strp("gl"), Remote: &RemoteLink{Path: "team/a", ID: &id, KeySHA256: &upper}}}
		},
		"hash short": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginLinked, Provider: strp("gl"), Remote: &RemoteLink{Path: "team/a", ID: &id, KeySHA256: &short}}}
		},
		"hash empty": func(r *Registry) {
			r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginLinked, Provider: strp("gl"), Remote: &RemoteLink{Path: "team/a", ID: &id, KeySHA256: &empty}}}
		},
	} {
		if err := st.Update(func(r *Registry) error {
			r.Providers = []Provider{testProvider("gl")}
			f(r)
			return nil
		}); err == nil {
			t.Errorf("update %q accepted", name)
		}
	}
	if err := st.Update(func(r *Registry) error {
		r.Providers = []Provider{testProvider("gl")}
		r.Projects = []Project{{ID: "a", Path: "/srv/a", Kind: "k", Origin: OriginLinked, Provider: strp("gl"), Remote: &RemoteLink{Path: "team/a", ID: &id, KeySHA256: &h}}}
		return nil
	}); err != nil {
		t.Fatalf("a valid hash refused: %v", err)
	}
	// a Snapshot is a deep copy: editing the hash in it does not touch the store
	snap := st.Snapshot()
	*snap.Projects[0].Remote.KeySHA256 = "x"
	if got := st.Snapshot().Projects[0].Remote.KeySHA256; got == nil || *got != hashOfLine(goodKey) || h != hashOfLine(goodKey) {
		t.Fatal("snapshot shares key_sha256 with the store")
	}
	// the hash survives a reload byte for byte
	if !strings.Contains(string(mustMarshalRegistry(st.Snapshot())), `"remote":{"path":"team/a","id":5,"key_sha256":"`+h+`"}`) {
		t.Fatalf("wire shape: %s", mustMarshalRegistry(st.Snapshot()))
	}
	// a row without a hash has no key_sha256 key at all (not null, not "")
	if err := st.Update(func(r *Registry) error { r.Projects[0].Remote.KeySHA256 = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if b := string(mustMarshalRegistry(st.Snapshot())); strings.Contains(b, "key_sha256") {
		t.Fatalf("an absent hash is written: %s", b)
	}
}
