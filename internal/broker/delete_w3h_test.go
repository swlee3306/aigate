package broker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/registrymsg"
)

// W3h (H-D2, H-D3): the signed deploy_key_sha256 of the registration is stored
// on the row as remote.key_sha256, and a detach (remove_project detach_key=true,
// remove_deploy_key) is refused in the shape stage (no ID burnt, no provider
// contact) unless sha256(the unsigned deploy_key line) equals it. A row without
// it: GitHub keeps the W3g rule (title AND body), GitLab is refused.

// putHashed stores id like put, with the hash of line as key_sha256 (a row a
// W3h broker registered with that key line).
func (d *delEnv) putHashed(id, prov, remote string, rid int64, origin, line string) {
	d.t.Helper()
	h := hashOfLine(line)
	d.putRaw(Project{ID: id, Path: d.root + "/" + id, Kind: "registered", Provider: &prov, Remote: &RemoteLink{Path: remote, ID: &rid, KeySHA256: &h}, Origin: origin})
}

// hashedRows replaces demo-gh and demo-gl by rows registered with delKeyB.
func hashedRows(d *delEnv) {
	d.putHashed("demo-gh", "dgh", "alice/Demo", 77, OriginCreated, delKeyB)
	d.putHashed("demo-gl", "dgl", "team/demo", 21, OriginCreated, delKeyB)
}

// foreignLines are lines orchd could send that are not the registered one:
// other keys, a key not listed at all, and the registered key's BODY with a
// different comment (same body, different line: the hash covers the whole line).
var foreignLines = []string{
	delKeyA, delKeyC, delKeyD,
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKeyEKeyEKeyEKeyEKeyEKeyEKeyEKeyEKeyEKeyE",
	delKeyB + " planted-comment",
	delKeyC + " orchd-demo-gl",
}

// both providers list our key AND the other keys (a corrupted orchd's targets)
func listAll(d *delEnv) {
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"2", "orchd demo-gh", delKeyB}, [3]string{"3", "my laptop", delKeyC}, [3]string{"4", "ci", delKeyD})
	d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd demo-gl", delKeyB}, [3]string{"10", "my laptop", delKeyC}, [3]string{"11", "ci", delKeyD})
}

func TestCreateLinkAttachStoreTheSignedKeyHash(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.gl.existP["team/other"] = true
	want := c3Hash(c3Key())
	if want != hashOfLine(c3Key()) || len(want) != 64 {
		t.Fatalf("fixture hash %q", want)
	}
	// register two plain projects to attach to
	for _, id := range []string{"att-gh", "att-gl"} {
		if _, oe := e.do("register_project", e.signedArgs(e.alice, "register_project", id, map[string]any{"path": e.root + "/" + id, "kind": "registered", "ai_context": false})); oe != nil {
			t.Fatalf("register %s: %v", id, oe.Code)
		}
	}
	e.gh.mu.Lock()
	e.gh.repos["alice/att-one"] = "alice/att-one"
	e.gh.mu.Unlock()
	steps := []struct {
		name, op, id string
		args         map[string]any
	}{
		{"create github", "create_project", c3Name, e.ghCreate(c3Name, "alice", "private", nil)},
		{"create gitlab", "create_project", "demo-gl", e.projectArgs("demo-gl", nil)},
		{"link github (new row)", "link_project", "linked-gh", e.linkArgs("linked-gh", "gh", "alice/existing", false, nil)},
		{"link gitlab (new row)", "link_project", "linked-gl", e.linkArgs("linked-gl", "gl", "team/exist", false, nil)},
		{"attach github (existing row)", "attach_remote", "att-gh", e.attachArgs("att-gh", "gh", "alice/att-one", nil)},
		{"attach gitlab (existing row)", "attach_remote", "att-gl", e.attachArgs("att-gl", "gl", "team/other", nil)},
	}
	for _, s := range steps {
		res, oe := e.do(s.op, s.args)
		if oe != nil {
			t.Fatalf("%s: %v", s.name, oe.Code)
		}
		p, ok := e.find(s.id)
		if !ok || p.Remote == nil || p.Remote.KeySHA256 == nil || *p.Remote.KeySHA256 != want {
			t.Fatalf("%s: the row does not carry the signed hash: %+v", s.name, p.Remote)
		}
		// the answer's project carries it too
		if !strings.Contains(jsonOf(t, res), `"key_sha256":"`+want+`"`) {
			t.Fatalf("%s: answer %s", s.name, jsonOf(t, res))
		}
	}
	// the file and the projects read carry it, for every registered row, and a reload keeps it
	data := readFileOrEmpty(e.s.Registry.path)
	if n := strings.Count(data, `"key_sha256":"`+want+`"`); n != len(steps) {
		t.Fatalf("file holds the hash %d times, want %d: %s", n, len(steps), data)
	}
	res, oe := e.do("projects", map[string]any{})
	if oe != nil {
		t.Fatal(oe.Code)
	}
	var view struct {
		Projects []struct {
			ID     string `json:"id"`
			Remote *struct {
				KeySHA256 string `json:"key_sha256"`
			} `json:"remote"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(jsonOf(t, res)), &view); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, p := range view.Projects {
		if p.Remote != nil && p.Remote.KeySHA256 == want {
			seen++
		}
	}
	if seen != len(steps) {
		t.Fatalf("projects view: %d rows carry the hash", seen)
	}
	st := mustOpen(t, filepath.Dir(e.s.Registry.path), LegacySource{})
	for _, s := range steps {
		if p, ok := findProject(ptrReg(st.Snapshot()), s.id); !ok || p.Remote.KeySHA256 == nil || *p.Remote.KeySHA256 != want {
			t.Fatalf("%s: reload lost the hash", s.name)
		}
	}
}

func ptrReg(r Registry) *Registry { return &r }

func TestDetachRefusedWhenKeyLineHashDiffers(t *testing.T) {
	for _, id := range []string{"demo-gh", "demo-gl"} {
		for _, line := range foreignLines {
			d := newDel(t)
			hashedRows(d)
			listAll(d)
			d.refusedNoBurn("remove_project", d.rmDetachArgs(id, false, line), "deploy_key_mismatch")
			if !d.rowExists(id) {
				t.Fatalf("%s %q: row gone", id, line)
			}
			// decided BEFORE the signature: a broken signature is still answered deploy_key_mismatch
			bad := d.rmDetachArgs(id, false, line)
			bad["signature"] = d.rmDetachArgs("other", false, line)["signature"]
			d.refusedNoBurn("remove_project", bad, "deploy_key_mismatch")
		}
		// control: the registered line takes our key off and unregisters
		d := newDel(t)
		hashedRows(d)
		listAll(d)
		d.wantCode("remove_project", d.rmDetachArgs(id, false, delKeyB), "")
		if d.rowExists(id) || d.gh.deletes()+d.gl.deletes() != 1 {
			t.Fatalf("control %s: %v %v", id, d.gh.all(), d.gl.all())
		}
		// without detach the key line plays no part (and must be absent, as in W3g)
		d2 := newDel(t)
		hashedRows(d2)
		d2.wantCode("remove_project", d2.rmArgs(id, false), "")
		d2.noHits("remove_project without detach")
	}
}

func TestRemoveDeployKeyRefusedWhenKeyLineHashDiffers(t *testing.T) {
	for _, id := range []string{"demo-gh", "demo-gl"} {
		for _, line := range foreignLines {
			d := newDel(t)
			hashedRows(d)
			listAll(d)
			d.refusedNoBurn("remove_deploy_key", d.keyArgs(id, line), "deploy_key_mismatch")
			bad := d.keyArgs(id, line)
			bad["signature"] = d.rmDetachArgs(id, false, line)["signature"] // a signature of another message
			d.refusedNoBurn("remove_deploy_key", bad, "deploy_key_mismatch")
		}
		d := newDel(t)
		hashedRows(d)
		listAll(d)
		d.wantCode("remove_deploy_key", d.keyArgs(id, delKeyB), "")
		if d.gh.deletes()+d.gl.deletes() != 1 || !d.rowExists(id) {
			t.Fatalf("control %s: %v %v", id, d.gh.all(), d.gl.all())
		}
	}
}

func TestGitLabRowWithoutHashRefusesDetach(t *testing.T) {
	// demo-gl without a hash (a row registered before W3h / migrated from v4)
	for _, line := range append([]string{delKeyB}, foreignLines...) {
		d := newDel(t)
		d.put("demo-gl", "dgl", "team/demo", 21, OriginCreated)
		listAll(d)
		if d.proj("demo-gl").Remote.KeySHA256 != nil {
			t.Fatal("fixture: demo-gl carries a hash")
		}
		d.refusedNoBurn("remove_project", d.rmDetachArgs("demo-gl", false, line), "deploy_key_mismatch")
		d.refusedNoBurn("remove_deploy_key", d.keyArgs("demo-gl", line), "deploy_key_mismatch")
		if !d.rowExists("demo-gl") {
			t.Fatal("row gone")
		}
	}
	// the documented way out: remove without detaching (the key is then taken off by hand)
	d := newDel(t)
	d.put("demo-gl", "dgl", "team/demo", 21, OriginCreated)
	d.wantCode("remove_project", d.rmArgs("demo-gl", false), "")
	d.noHits("remove without detach")
	// a linked/unknown-origin GitLab row is refused the same way
	d2 := newDel(t)
	listAll(d2)
	d2.put("demo-gl", "dgl", "team/demo", 21, OriginLinked)
	d2.refusedNoBurn("remove_project", d2.rmDetachArgs("demo-gl", false, delKeyB), "deploy_key_mismatch")
}

func TestGitHubRowWithoutHashKeepsW3gRule(t *testing.T) {
	// newDel's demo-gh has no hash: title "orchd demo-gh" AND body decide, as in W3g
	d := newDel(t)
	w3gKeys(d)
	if d.proj("demo-gh").Remote.KeySHA256 != nil {
		t.Fatal("fixture: demo-gh carries a hash")
	}
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "")
	if got := d.gh.all(); fmt.Sprint(got) != fmt.Sprint([]string{ghRepoGet, ghKeysPath, "DELETE /repos/alice/Demo/keys/2"}) {
		t.Fatalf("requests %v", got)
	}
	// a foreign line: the W3g look-alike refusal after the provider read (ID burnt), no DELETE
	d2 := newDel(t)
	listAll(d2)
	a := d2.rmDetachArgs("demo-gh", false, delKeyC)
	d2.wantCode("remove_project", a, "remove_refused")
	if d2.gh.deletes() != 0 || !d2.rowExists("demo-gh") {
		t.Fatalf("W3g rule changed: %v", d2.gh.all())
	}
	d2.wantCode("remove_project", a, "request_reused")
	// remove_deploy_key likewise
	d3 := newDel(t)
	listAll(d3)
	d3.wantCode("remove_deploy_key", d3.keyArgs("demo-gh", delKeyB), "")
	if fmt.Sprint(d3.gh.all()[len(d3.gh.all())-1]) != "DELETE /repos/alice/Demo/keys/2" {
		t.Fatalf("requests %v", d3.gh.all())
	}
	d4 := newDel(t)
	listAll(d4)
	d4.wantCode("remove_deploy_key", d4.keyArgs("demo-gh", delKeyC), "remove_refused")
	if d4.gh.deletes() != 0 {
		t.Fatalf("W3g rule changed: %v", d4.gh.all())
	}
}

func TestRemoteClearedDropsHash(t *testing.T) {
	d := newDel(t)
	hashedRows(d)
	h := hashOfLine(delKeyB)
	path := d.s.Registry.path
	if strings.Count(readFileOrEmpty(path), h) != 2 {
		t.Fatalf("fixture: %s", readFileOrEmpty(path))
	}
	// delete_remote_repo without also_local: remote null + remote_deleted_at, no hash left anywhere
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "")
	p := d.proj("demo-gh")
	if p.Remote != nil || p.RemoteDeletedAt == nil {
		t.Fatalf("row %+v", p)
	}
	if strings.Count(readFileOrEmpty(path), h) != 1 { // only demo-gl's
		t.Fatalf("the hash outlived the remote: %s", readFileOrEmpty(path))
	}
	res, oe := d.do("projects", map[string]any{})
	if oe != nil {
		t.Fatal(oe.Code)
	}
	if strings.Count(jsonOf(t, res), h) != 1 {
		t.Fatalf("projects view: %s", jsonOf(t, res))
	}
	// with also_local the row (and the hash) goes
	d.wantCode("delete_remote_repo", d.delArgs("demo-gl", func(f map[string]any) { f["also_local"] = true }), "")
	if strings.Contains(readFileOrEmpty(path), h) || strings.Contains(readFileOrEmpty(path), "key_sha256") {
		t.Fatalf("hash left: %s", readFileOrEmpty(path))
	}
	// clear_orphan touches orphan records only (they have no hash): file stays hash-free
	if _, err := d.s.Registry.AddOrphan(Orphan{Provider: "dgl", Project: "team/half", Step: "protect", At: "2026-11-10T00:00:00Z", Origin: OriginLinked}); err != nil {
		t.Fatal(err)
	}
	d.wantCode("clear_orphan", d.signedArgs(d.alice, registrymsg.ActionClearOrphan, "dgl", map[string]any{"project": "team/half"}), "")
	if strings.Contains(readFileOrEmpty(path), "key_sha256") {
		t.Fatalf("hash left: %s", readFileOrEmpty(path))
	}
	// the hash lives inside remote: there is no row-level key to leave behind
	if b, _ := os.ReadFile(path); strings.Contains(string(b), `"remote":null,"key_sha256"`) {
		t.Fatal("hash outside remote")
	}
}

// W3h replacement of TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g:
// the residual pinned there (on GitLab the unsigned line alone chose WHICH key
// of the signed project was taken off) is closed. Every line other than the
// registered one is refused before the signature with deploy_key_mismatch, on
// GitHub and GitLab rows that carry the hash, and on a GitLab row without it;
// nothing reaches a provider and the ID is not burnt.
func TestUnsignedKeyLineWideningNowRefusedW3h(t *testing.T) {
	type probe struct {
		id     string
		hashed bool
	}
	for _, p := range []probe{{"demo-gh", true}, {"demo-gl", true}, {"demo-gl", false}} {
		for _, line := range foreignLines {
			d := newDel(t)
			if p.hashed {
				hashedRows(d)
			}
			listAll(d)
			for _, op := range []string{"remove_project", "remove_deploy_key"} {
				a := d.rmDetachArgs(p.id, false, line)
				if op == "remove_deploy_key" {
					a = d.keyArgs(p.id, line)
				}
				d.refusedNoBurn(op, a, "deploy_key_mismatch")
			}
			if d.gh.deletes()+d.gl.deletes() != 0 || !d.rowExists(p.id) {
				t.Fatalf("%+v %q: %v %v", p, line, d.gh.all(), d.gl.all())
			}
		}
	}
	// control: the registered line takes exactly our key off, on both providers
	for _, c := range []struct{ id, del string }{{"demo-gh", "DELETE /repos/alice/Demo/keys/2"}, {"demo-gl", "DELETE /api/v4/projects/21/deploy_keys/8"}} {
		d := newDel(t)
		hashedRows(d)
		listAll(d)
		d.wantCode("remove_project", d.rmDetachArgs(c.id, false, delKeyB), "")
		all := append(d.gh.all(), d.gl.all()...)
		if all[len(all)-1] != c.del || d.rowExists(c.id) || d.gh.deletes()+d.gl.deletes() != 1 {
			t.Fatalf("control %s: %v", c.id, all)
		}
	}
}
