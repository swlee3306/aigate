package broker

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// W3h (H3): adversarial tests for the broker's side of binding a key detach to
// the signed key hash of the registration (registry v5). Only properties the H1
// tests imply but never run over the whole matrix are here; the rest is mapped
// in docs/33.

// The registry row's hash changes AFTER the shape check passed and BEFORE the
// locked re-check (the race seam is the broker clock: s.Now is read exactly
// once, inside verifySignedShape, after shape() and before the request ID is
// marked). The locked re-check must refuse with deploy_key_mismatch: no provider
// is contacted (no key list, no DELETE) and the row stays. The request ID IS
// consumed at that point (verifySignedShape marks it before the handler takes
// its locks; by design, "From here on the request ID is consumed"): restoring
// the hash and resending the same signed request answers request_reused.
// Without the re-check's comparison (mutant MF) the DELETE goes out.
func TestKeyHashChangedBetweenShapeAndLockIsRefusedW3h(t *testing.T) {
	for _, op := range []string{"remove_project", "remove_deploy_key"} {
		for _, id := range []string{"demo-gh", "demo-gl"} {
			d := newDel(t)
			hashedRows(d)
			listAll(d)
			p := d.proj(id)
			swapped := false
			var once sync.Once
			d.s.Now = func() time.Time {
				once.Do(func() {
					// another registration of the same remote with another key line
					d.putHashed(id, *p.Provider, p.Remote.Path, *p.Remote.ID, p.Origin, delKeyC)
					swapped = true
				})
				return d.now
			}
			a := d.rmDetachArgs(id, false, delKeyB)
			if op == "remove_deploy_key" {
				a = d.keyArgs(id, delKeyB)
			}
			d.wantCode(op, a, "deploy_key_mismatch")
			if !swapped {
				t.Fatalf("%s %s: the seam did not run (the check above means nothing)", op, id)
			}
			d.noHits(op + " " + id + " after the swap")
			if !d.rowExists(id) || *d.proj(id).Remote.KeySHA256 != hashOfLine(delKeyC) {
				t.Fatalf("%s %s: the row changed: %+v", op, id, d.proj(id).Remote)
			}
			// the ID was consumed (after the shape stage): with the original hash
			// back, the same signed request is a reuse, still with no provider contact
			d.putHashed(id, *p.Provider, p.Remote.Path, *p.Remote.ID, p.Origin, delKeyB)
			d.wantCode(op, a, "request_reused")
			d.noHits(op + " " + id + " resend")
			// a fresh signature with the registered line works (control: the
			// setup could take the key off)
			fresh := d.rmDetachArgs(id, false, delKeyB)
			if op == "remove_deploy_key" {
				fresh = d.keyArgs(id, delKeyB)
			}
			d.wantCode(op, fresh, "")
			if d.gh.deletes()+d.gl.deletes() != 1 {
				t.Fatalf("%s %s control: %v %v", op, id, d.gh.all(), d.gl.all())
			}
		}
	}
}

// Refusal-code priority when a row both lost its remote number / path (remote
// changed) and carries a hash the line does not match: remote_changed wins, on
// both ops, before the signature and without burning the ID. (orchd answers the
// same: TestRemoteChangedOutranksKeyMismatchW3h in internal/orch/api.)
func TestRemoteChangedOutranksKeyMismatchW3h(t *testing.T) {
	for _, hashed := range []bool{true, false} {
		for _, id := range []string{"demo-gh", "demo-gl"} {
			d := newDel(t)
			listAll(d)
			p := d.proj(id)
			p.Remote.ID = nil // a remote without its number: remote_changed
			p.Remote.KeySHA256 = nil
			if hashed {
				h := hashOfLine(delKeyB)
				p.Remote.KeySHA256 = &h
			}
			d.putRaw(p)
			for _, line := range []string{delKeyC, delKeyB + " planted-comment"} {
				d.refusedNoBurn("remove_project", d.rmDetachArgs(id, false, line), "remote_changed")
				d.refusedNoBurn("remove_deploy_key", d.keyArgs(id, line), "remote_changed")
			}
		}
		// remove_deploy_key with another remote_path than the row's: remote_changed too
		d := newDel(t)
		listAll(d)
		if !hashed {
			d.put("demo-gl", "dgl", "team/demo", 21, OriginCreated)
		}
		a := d.signedArgs(d.alice, "remove_deploy_key", "demo-gl", map[string]any{"remote_path": "team/other"})
		a["deploy_key"] = delKeyC
		d.refusedNoBurn("remove_deploy_key", a, "remote_changed")
	}
}

// The broker's key-line rule over the vector table shared with orchd
// (testdata/keyline_vectors_w3h.json; orchd reads the same file in
// TestKeyLineRuleVectorsAgreeWithBrokerW3h): kind x hash present x hash equal.
// Both sides pin the same expected answer, so they cannot disagree: orchd never
// offers a detach the broker refuses, and never hides one it would take.
func TestKeyLineRuleVectorsW3h(t *testing.T) {
	vs := loadKeyLineVectors(t, "testdata/keyline_vectors_w3h.json")
	for _, v := range vs {
		rid := int64(5)
		prov := "p"
		p := Project{ID: "x", Provider: &prov, Remote: &RemoteLink{Path: "team/x", ID: &rid}}
		if v.Stored != "" {
			s := v.Stored
			p.Remote.KeySHA256 = &s
		}
		got := keyLineCode(Provider{ID: prov, Kind: v.Kind}, p, v.Line) == ""
		if got != v.OK {
			t.Errorf("%s stored=%.8q line=%.40q: broker says %v, table %v (%s)", v.Kind, v.Stored, v.Line, got, v.OK, v.Why)
		}
		if code := keyLineCode(Provider{ID: prov, Kind: v.Kind}, p, v.Line); code != "" && code != "deploy_key_mismatch" {
			t.Errorf("unexpected code %q", code)
		}
	}
}

type keyLineVector struct {
	Kind   string `json:"kind"`
	Stored string `json:"stored"`
	Line   string `json:"line"`
	OK     bool   `json:"ok"`
	Why    string `json:"why"`
}

// loadKeyLineVectors reads the shared table and checks it covers, for GitHub,
// GitLab and an unknown kind, a row without a hash, a hash that matches and a
// hash that does not.
func loadKeyLineVectors(t *testing.T, path string) []keyLineVector {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vs []keyLineVector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range vs {
		c := "absent"
		if v.Stored != "" {
			c = "mismatch"
			if v.Stored == hashOfLine(v.Line) {
				c = "equal"
			}
		}
		seen[v.Kind+"/"+c] = true
	}
	for _, k := range []string{"github", "gitlab", "gitea"} {
		for _, c := range []string{"absent", "equal", "mismatch"} {
			if !seen[k+"/"+c] {
				t.Fatalf("the vector table lacks %s/%s", k, c)
			}
		}
	}
	return vs
}

// A GitLab project registered BEFORE the upgrade: its v4 row migrates to v5
// without a hash, and from then on a key detach of it is refused before the
// signature (deploy_key_mismatch, ID not burnt, no provider contact) whatever
// line orchd sends, while removing it without the detach still works. A GitHub
// row migrated the same way keeps the W3g rule. (Runbook 18 3-3.)
func TestRowMigratedFromV4CannotDetachOnGitLabW3h(t *testing.T) {
	d := newDel(t)
	listAll(d)
	// a v4 file holding the two rows as a W3g broker wrote them
	path := d.s.Registry.path
	cur := readFileOrEmpty(path)
	var f map[string]any
	if err := json.Unmarshal([]byte(cur), &f); err != nil {
		t.Fatal(err)
	}
	for _, row := range f["projects"].([]any) {
		if rem, ok := row.(map[string]any)["remote"].(map[string]any); ok {
			delete(rem, "key_sha256")
		}
	}
	f["version"] = 4
	v4, _ := json.Marshal(f)
	dir := filepath.Dir(path)
	if err := os.WriteFile(path, v4, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(path + ".v4.bak")
	st, err := OpenRegistry(dir, LegacySource{}, regOpts())
	if err != nil || st.Err() != nil {
		t.Fatalf("migration: %v", err)
	}
	if got, _ := os.ReadFile(path + ".v4.bak"); !bytes.Equal(got, v4) {
		t.Fatalf("backup: %s", got)
	}
	d.s.Registry = st
	if p := d.proj("demo-gl"); p.Remote == nil || p.Remote.KeySHA256 != nil || st.Snapshot().Version != 5 {
		t.Fatalf("migrated row %+v", p.Remote)
	}
	for _, line := range []string{delKeyB, delKeyC} {
		d.refusedNoBurn("remove_project", d.rmDetachArgs("demo-gl", false, line), "deploy_key_mismatch")
		d.refusedNoBurn("remove_deploy_key", d.keyArgs("demo-gl", line), "deploy_key_mismatch")
	}
	// GitHub (W3g rule): the registered key is found by title and body
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "")
	if d.gh.deletes() != 1 || d.gl.deletes() != 0 {
		t.Fatalf("github detach: %v %v", d.gh.all(), d.gl.all())
	}
	// the GitLab row goes without the detach (its key is then removed by hand)
	before := len(d.gl.all())
	d.wantCode("remove_project", d.rmArgs("demo-gl", false), "")
	if d.rowExists("demo-gl") || len(d.gl.all()) != before {
		t.Fatal("remove without detach")
	}
}

// rollbackScript is the runbook's registry edit for a rollback to W3g (docs/10
// 18장 단계 4, kept in sync by hand): drop every remote.key_sha256 and set
// version 4, atomically, keeping owner and mode. The doc runs it as root with
// `python3 - <registry.json>` and the script on stdin.
const rollbackScript = `import json, os, sys
p = sys.argv[1]
with open(p, encoding="utf-8") as f:
    r = json.load(f)
if r.get("version") != 5:
    sys.exit("not a version 5 registry: nothing changed")
n = 0
for x in r["projects"]:
    rem = x.get("remote")
    if isinstance(rem, dict) and "key_sha256" in rem:
        del rem["key_sha256"]
        n += 1
r["version"] = 4
st = os.stat(p)
tmp = p + ".rollback-tmp"
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as f:
    json.dump(r, f, ensure_ascii=False, separators=(",", ":"))
    f.write("\n")
    f.flush()
    os.fsync(f.fileno())
os.chown(tmp, st.st_uid, st.st_gid)
os.replace(tmp, p)
dfd = os.open(os.path.dirname(p) or ".", os.O_RDONLY)
os.fsync(dfd)
os.close(dfd)
print("removed key_sha256 from", n, "rows; version 4")
`

// The runbook's rollback edit (instead of restoring registry.json.v4.bak, which
// would also undo every registry change made after the upgrade) turns a W3h
// registry into one the W3g strict loader accepts: no key_sha256 anywhere,
// version 4, every other member unchanged; a W3h broker migrates it to v5 again
// (re-upgrade). Rows created/linked after the upgrade stay; their GitLab
// detach is then refused (no hash) like any older row.
func TestRunbookRollbackEditMakesAW3gRegistryW3h(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	d := newDel(t)
	hashedRows(d) // two rows with key_sha256 (made after the upgrade)
	path := d.s.Registry.path
	before := readFileOrEmpty(path)
	if strings.Count(before, "key_sha256") != 2 {
		t.Fatalf("fixture: %s", before)
	}
	work := stateDirFor(t)
	reg := writeRegFile(t, work, before, 0o600)
	run := func() string {
		cmd := exec.Command(py, "-I", "-", reg)
		cmd.Stdin = strings.NewReader(rollbackScript)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script: %v %s", err, out)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "removed key_sha256 from 2 rows; version 4") {
		t.Fatalf("script said %q", out)
	}
	after := readFile(t, reg)
	if fi, _ := os.Stat(reg); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if strings.Contains(string(after), "key_sha256") || !w3gLoaderAccepts(after) {
		t.Fatalf("not a W3g registry: %s", after)
	}
	// every other member is unchanged
	var a, b map[string]any
	json.Unmarshal([]byte(before), &b)
	json.Unmarshal(after, &a)
	for _, row := range b["projects"].([]any) {
		if rem, ok := row.(map[string]any)["remote"].(map[string]any); ok {
			delete(rem, "key_sha256")
		}
	}
	b["version"] = float64(4)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("the edit changed more than the hash and the version:\n%s\n%s", before, after)
	}
	// a second run refuses (version 4 now) and changes nothing
	cmd := exec.Command(py, "-I", "-", reg)
	cmd.Stdin = strings.NewReader(rollbackScript)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "nothing changed") {
		t.Fatalf("second run: %v %s", err, out)
	}
	if !bytes.Equal(readFile(t, reg), after) {
		t.Fatal("second run changed the file")
	}
	// re-upgrade: a W3h broker migrates it to v5 again, rows unchanged, no hash
	st, err := OpenRegistry(work, LegacySource{}, regOpts())
	if err != nil || st.Err() != nil {
		t.Fatalf("re-upgrade: %v", err)
	}
	snap := st.Snapshot()
	if snap.Version != 5 || len(snap.Projects) != len(d.s.Registry.Snapshot().Projects) {
		t.Fatalf("re-upgrade: %+v", snap)
	}
	for _, p := range snap.Projects {
		if p.Remote != nil && p.Remote.KeySHA256 != nil {
			t.Fatalf("%s kept a hash", p.ID)
		}
	}
	if got, _ := os.ReadFile(reg + ".v4.bak"); !bytes.Equal(got, after) {
		t.Fatal("re-upgrade backup is not the rolled back file")
	}
	// a v5 registry without any hash: the same edit only changes the version
	reg = writeRegFile(t, stateDirFor(t), v5Of(v4Rich), 0o600)
	if out := run(); !strings.Contains(out, "removed key_sha256 from 0 rows; version 4") {
		t.Fatalf("script said %q", out)
	}
	var x, y any
	json.Unmarshal(readFile(t, reg), &x)
	json.Unmarshal([]byte(v4Rich), &y)
	if !reflect.DeepEqual(x, y) || !w3gLoaderAccepts(readFile(t, reg)) {
		t.Fatalf("hash-free edit: %s", readFile(t, reg))
	}
}

// w3gLoaderAccepts replays the W3g broker's strict loader (registry version 4,
// remote {path,id}), like TestOldBrokerRefusesV5.
func w3gLoaderAccepts(b []byte) bool {
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
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r w3gRegistry
	return dec.Decode(&r) == nil && r.Version == 4
}
