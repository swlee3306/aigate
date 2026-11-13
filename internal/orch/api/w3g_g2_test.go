package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/policy"
)

// W3g (G2): remove_project may detach the deploy key in the same signature
// (signed detach_key, orchd supplies the key line from the project's key folder
// as an unsigned argument), and delete_remote_repo with also_local removes the
// registry row at the broker, so orchd removes the local files after one
// signature.

func rmDetach(detach bool) map[string]any {
	return map[string]any{"scope": "local", "legacy_ack": false, "detach_key": detach}
}

// dropRow removes a registry row from the fake broker (what the W3g broker does
// on an ok remove_project / also_local answer).
func (d *delEnv) dropRow(id string) {
	d.wb.mu.Lock()
	defer d.wb.mu.Unlock()
	var keep []map[string]any
	for _, p := range d.wb.projects {
		if p["id"] != id {
			keep = append(keep, p)
		}
	}
	d.wb.projects = keep
}

func (d *delEnv) hasRow(id string) bool {
	d.wb.mu.Lock()
	defer d.wb.mu.Unlock()
	for _, p := range d.wb.projects {
		if p["id"] == id {
			return true
		}
	}
	return false
}

func (d *delEnv) resetDeleteLimits() {
	h := d.d.handler
	h.removeLimit, h.deleteRemoteLimit = newActorLimiter(100, time.Hour), newActorLimiter(100, time.Hour)
	h.removeKeyLimit, h.orphanLimit = newActorLimiter(100, time.Hour), newActorLimiter(100, time.Hour)
	h.w3cProposeLimit = newActorLimiter(1000, time.Hour)
}

func pubFP(t *testing.T, path string) (line, fp string) {
	t.Helper()
	line, fp, ok := readDeployKey(path)
	if !ok {
		t.Fatalf("cannot read %s", path)
	}
	return line, fp
}

func TestRemoveProposeReadsKeyLineOnlyFromKeyFolder(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	line, fp := pubFP(t, filepath.Join(e.keysDir(), "app-one", "id_ed25519.pub"))
	// a .pub elsewhere (inside the clone, next to the folder) is never read
	other, _ := sshPubLine(t, "planted")
	writeFile(t, filepath.Join(path, "id_ed25519.pub"), other+"\n")
	writeFile(t, filepath.Join(path, ".git", "id_ed25519.pub"), other+"\n")

	c, b := e.proposeDel(t, "remove_project", "app-one", rmDetach(true))
	if c != 200 {
		t.Fatalf("propose: %d %s", c, b)
	}
	var resp map[string]any
	_ = json.Unmarshal(b, &resp)
	if resp["detach_key_available"] != true || resp["deploy_key_fingerprint"] != fp || resp["deploy_key_title"] != "orchd app-one" || resp["deploy_key_shared"] != false {
		t.Fatalf("propose extras: %s", b)
	}
	if strings.Contains(string(b), strings.Fields(line)[1]) || strings.Contains(string(b), strings.Fields(other)[1]) {
		t.Fatalf("a key body is in the proposal: %s", b)
	}
	var p w3cProposal
	_ = json.Unmarshal(b, &p)
	if _, has := p.Fields["deploy_key"]; has {
		t.Fatal("deploy_key must not be a signed field")
	}
	if fmt.Sprint(anyKeys(p.Fields)) != "[detach_key legacy_ack scope]" || p.Fields["detach_key"] != true {
		t.Fatalf("signed fields: %v", p.Fields)
	}
	// the key line can never come from the request: not as a field, not at the top level
	bad := rmDetach(true)
	bad["deploy_key"] = other
	if c, b := e.proposeDel(t, "remove_project", "app-one", bad); c != 400 || v1Code(b) != "bad_request" {
		t.Fatalf("deploy_key as a field: %d %s", c, b)
	}
	if c, b := e.sockJSON(t, "POST", delPaths["remove_project"]+"/propose", jsonBody(map[string]any{"action": "remove_project", "subject": "app-one", "fields": rmDetach(true), "deploy_key": other})); c != 400 {
		t.Fatalf("deploy_key at the top level of propose: %d %s", c, b)
	}
	ab := e.applyBody(p, webauthnSig)
	ab["deploy_key"] = other
	if c, b := e.sockJSON(t, "POST", delPaths["remove_project"]+"/apply", jsonBody(ab)); c != 400 {
		t.Fatalf("deploy_key at the top level of apply: %d %s", c, b)
	}
	if n := len(e.writes()); n != 0 {
		t.Fatalf("the broker was asked %d times", n)
	}
	if c, b := e.applyDel(t, p); c != 200 {
		t.Fatalf("apply: %d %s", c, b)
	}
	if args := lastCall(t, e.w3cEnv, "remove_project"); args["deploy_key"] != line {
		t.Fatalf("the key line sent is not the key folder's: %v", args["deploy_key"])
	}
	// GitHub never falls back to the shared key: without a key folder the line is unavailable
	e.addDel(t, delProject{id: "gh-two", origin: "linked", prov: "gh", remote: "Octo-Cat/gh-two", remoteID: 124})
	if c, b := e.proposeDel(t, "remove_project", "gh-two", rmDetach(true)); c != 409 || v1Code(b) != "key_unavailable_for_removal" {
		t.Fatalf("no key folder: %d %s", c, b)
	}
	// a symlinked key folder (pointing at a real key) is not the key folder
	real := t.TempDir()
	os.Chmod(real, 0o700)
	writeFile(t, filepath.Join(real, "id_ed25519.pub"), other+"\n")
	if err := os.Symlink(real, filepath.Join(e.keysDir(), "gh-two")); err != nil {
		t.Fatal(err)
	}
	if c, b := e.proposeDel(t, "remove_project", "gh-two", rmDetach(true)); c != 409 || v1Code(b) != "key_unavailable_for_removal" {
		t.Fatalf("symlinked key folder: %d %s", c, b)
	}
	// GitLab: the shared key's .pub (G-D5), marked shared
	e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 23})
	pg := e.mustPropose(t, "remove_project", "gl-one", rmDetach(true))
	_, gb := e.proposeDel(t, "remove_project", "gl-one", rmDetach(true))
	var gresp map[string]any
	_ = json.Unmarshal(gb, &gresp)
	if gresp["deploy_key_fingerprint"] != e.pubFP || gresp["deploy_key_shared"] != true {
		t.Fatalf("gitlab propose: %s", gb)
	}
	if c, b := e.applyDel(t, pg); c != 200 {
		t.Fatalf("gitlab apply: %d %s", c, b)
	}
	if args := lastCall(t, e.w3cEnv, "remove_project"); args["deploy_key"] != e.pubLine {
		t.Fatalf("gitlab key line: %v", args["deploy_key"])
	}
}

func anyKeys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestDetachKeyUnavailableRefusedBeforeSignature(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "no-key", origin: "created", prov: "gh", remote: "Octo-Cat/no-key", remoteID: 1})
	badKey := e.addDel(t, delProject{id: "bad-key", origin: "created", prov: "gh", remote: "Octo-Cat/bad-key", remoteID: 2, withKey: true})
	_ = badKey
	writeFile(t, filepath.Join(e.keysDir(), "bad-key", "id_ed25519.pub"), "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ not-ed25519\n")
	e.addDel(t, delProject{id: "bare-one", origin: "created"})
	e.addDel(t, delProject{id: "gone-one", origin: "created", prov: "gh", deletedAt: delStamp, withKey: true})
	e.addDel(t, delProject{id: "legacy-one", origin: "legacy", remote: "team/legacy-one", remoteID: 9, withKey: true})
	e.addDel(t, delProject{id: "no-number", origin: "linked", prov: "gh", remote: "Octo-Cat/no-number", withKey: true})
	for _, c := range []struct {
		id, code string
		status   int
		fields   map[string]any
	}{
		{"no-key", "key_unavailable_for_removal", 409, rmDetach(true)},
		{"bad-key", "key_unavailable_for_removal", 409, rmDetach(true)},
		// G-D3: detach is meaningless without provider+remote, or with the remote already deleted
		{"bare-one", "bad_request", 400, rmDetach(true)},
		{"gone-one", "bad_request", 400, rmDetach(true)},
		{"legacy-one", "bad_request", 400, map[string]any{"scope": "local", "legacy_ack": true, "detach_key": true}},
		// the broker would answer remote_changed (no repository number): said before the signature
		{"no-number", "remote_changed", 409, rmDetach(true)},
	} {
		code, b := e.proposeDel(t, "remove_project", c.id, c.fields)
		if code != c.status || v1Code(b) != c.code {
			t.Errorf("%s: %d %s (want %d %s)", c.id, code, b, c.status, c.code)
		}
	}
	// with detach_key off the same rows are removable (the key, if any, stays at the provider)
	for _, id := range []string{"no-key", "bare-one", "gone-one", "no-number"} {
		if c, b := e.proposeDel(t, "remove_project", id, rmDetach(false)); c != 200 {
			t.Errorf("%s detach off: %d %s", id, c, b)
		}
	}
	// the key line disappears between propose and apply: refused before anything is sent
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 3, withKey: true})
	p := e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
	if err := os.Remove(filepath.Join(e.keysDir(), "app-one", "id_ed25519.pub")); err != nil {
		t.Fatal(err)
	}
	if c, b := e.applyDel(t, p); c != 409 || v1Code(b) != "key_unavailable_for_removal" {
		t.Fatalf("apply without the key line: %d %s", c, b)
	}
	if n := len(e.writes()); n != 0 {
		t.Fatalf("the broker was asked %d times", n)
	}
	if !exists(filepath.Join(e.root, "app-one", ".git")) || !exists(filepath.Join(e.keysDir(), "app-one", "id_ed25519")) {
		t.Fatal("local files were touched")
	}
	if led := e.ledgerPayloads(t, "project_remove"); len(led) != 0 {
		t.Fatalf("a refusal before the broker is not a ledger event: %v", led)
	}
}

func TestKeyLineReadBeforeLocalRemoval(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	line, _ := pubFP(t, filepath.Join(keyDir, "id_ed25519.pub"))
	var seen []string
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			var m map[string]any
			_ = json.Unmarshal(a, &m)
			seen = append(seen, fmt.Sprint(m["deploy_key"] == line, exists(keyDir), exists(path)))
		}
		return ""
	})
	p := e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
	c, b := e.applyDel(t, p)
	if c != 200 {
		t.Fatalf("apply: %d %s", c, b)
	}
	// at the broker: the line was already read and the key folder and clone were still there
	if fmt.Sprint(seen) != "[true true true]" {
		t.Fatalf("at the broker: %v", seen)
	}
	if exists(keyDir) || exists(path) {
		t.Fatal("the local files were not removed after the ok answer")
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	loc, _ := out["local"].(map[string]any)
	if out["removed"] != true || out["detach_key"] != true || out["key_state"] != "removed" || loc["clone"] != "removed" || loc["key"] != "removed" {
		t.Fatalf("%s", b)
	}
	// the broker refuses the key removal: the row, the clone and the key folder all stay
	e.setOverride(nil)
	path2 := e.addDel(t, delProject{id: "app-two", origin: "created", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	for _, code := range []string{"remove_refused", "delete_partial"} {
		e.setOverride(func(op string, a json.RawMessage) string {
			if op == "remove_project" {
				return errReply(code)
			}
			return ""
		})
		p2 := e.mustPropose(t, "remove_project", "app-two", rmDetach(true))
		c, b := e.applyDel(t, p2)
		if c < 400 || v1Code(b) != code {
			t.Fatalf("%s: %d %s", code, c, b)
		}
		// the uncertain detach talks about the key, not about a remote repository deletion
		if code == "delete_partial" && (!strings.Contains(string(b), "배포 키 떼기") || strings.Contains(string(b), "원격 저장소 삭제")) {
			t.Fatalf("delete_partial sentence: %s", b)
		}
		if !exists(filepath.Join(path2, ".git")) || !exists(filepath.Join(e.keysDir(), "app-two", "id_ed25519.pub")) {
			t.Fatalf("%s: local files removed", code)
		}
	}
}

func TestApplyForwardsDetachKeyAndKeyLine(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	line, _ := pubFP(t, filepath.Join(keyDir, "id_ed25519.pub"))
	// answers that do not say how the key ended are an unknown outcome: nothing local is touched
	for _, reply := range []string{
		`{"ok":true,"result":{"id":"app-one","removed":true}}`,
		`{"ok":true,"result":{"id":"app-one","removed":true,"key_state":"maybe"}}`,
		`{"ok":true,"result":{"id":"app-one","removed":true,"key_state":""}}`,
		`{"ok":true,"result":{"id":"app-one","removed":true,"key_state":true}}`,
		`{"ok":true,"result":{"id":"app-one","removed":false,"key_state":"removed"}}`,
	} {
		e.resetDeleteLimits()
		e.setOverride(func(op string, a json.RawMessage) string {
			if op == "remove_project" {
				return reply
			}
			return ""
		})
		p := e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
		if c, b := e.applyDel(t, p); c != 504 || v1Code(b) != "outcome_unknown" {
			t.Errorf("%s: %d %s", reply, c, b)
		}
		if !exists(filepath.Join(path, ".git")) || !exists(keyDir) {
			t.Fatalf("%s: local files removed", reply)
		}
	}
	// detach_key off: a reply that talks about a key was not asked for
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return `{"ok":true,"result":{"id":"app-one","removed":true,"key_state":"removed"}}`
		}
		return ""
	})
	p := e.mustPropose(t, "remove_project", "app-one", rmDetach(false))
	if c, b := e.applyDel(t, p); c != 504 || v1Code(b) != "outcome_unknown" || !exists(path) {
		t.Fatalf("key_state without detach: %d %s", c, b)
	}
	// detach off: the args carry detach_key false and NO key line
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return errReply("remove_refused")
		}
		return ""
	})
	p = e.mustPropose(t, "remove_project", "app-one", rmDetach(false))
	e.applyDel(t, p)
	args := lastCall(t, e.w3cEnv, "remove_project")
	if _, has := args["deploy_key"]; has || args["detach_key"] != false {
		t.Fatalf("detach off args: %v", args)
	}
	// detach on: every signed field plus the key line, verbatim
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return `{"ok":true,"result":{"id":"app-one","removed":true,"key_state":"already_gone"}}`
		}
		return ""
	})
	p = e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
	c, b := e.applyDel(t, p)
	if c != 200 || !strings.Contains(string(b), `"key_state":"already_gone"`) {
		t.Fatalf("already_gone: %d %s", c, b)
	}
	args = lastCall(t, e.w3cEnv, "remove_project")
	want := []string{"change", "deploy_key", "detach_key", "id", "issued", "legacy_ack", "request", "scope", "signature"}
	if fmt.Sprint(anyKeys(args)) != fmt.Sprint(want) {
		t.Fatalf("arg keys %v", anyKeys(args))
	}
	if args["deploy_key"] != line || args["detach_key"] != true || args["scope"] != "local" || args["legacy_ack"] != false || args["id"] != "app-one" || args["change"] != p.Change {
		t.Fatalf("args %v", args)
	}
	if exists(path) || exists(keyDir) {
		t.Fatal("files stay after an ok already_gone answer")
	}
}

func TestPreviewDetachKeyAvailableAndFingerprint(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "gh-key", origin: "linked", prov: "gh", remote: "Octo-Cat/gh-key", remoteID: 1, withKey: true})
	_, fp := pubFP(t, filepath.Join(e.keysDir(), "gh-key", "id_ed25519.pub"))
	e.addDel(t, delProject{id: "gh-nokey", origin: "linked", prov: "gh", remote: "Octo-Cat/gh-nokey", remoteID: 2})
	e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 3})
	e.addDel(t, delProject{id: "bare-one", origin: "unknown"})
	e.addDel(t, delProject{id: "gone-one", origin: "created", prov: "gh", deletedAt: delStamp, withKey: true})
	e.addDel(t, delProject{id: "legacy-one", origin: "legacy", remote: "team/legacy-one", remoteID: 9})
	type want struct {
		avail, mayRemain, shared bool
		fp, title                any
	}
	for id, w := range map[string]want{
		"gh-key":     {true, true, false, fp, "orchd gh-key"},
		"gh-nokey":   {false, true, false, nil, nil},
		"gl-one":     {true, true, true, e.pubFP, "orchd gl-one"},
		"bare-one":   {false, false, false, nil, nil},
		"gone-one":   {false, false, false, nil, nil},
		"legacy-one": {false, true, false, nil, nil},
	} {
		c, b := e.sockJSON(t, "GET", "/api/v1/projects/"+id+"/removal-preview", "")
		var pv map[string]any
		_ = json.Unmarshal(b, &pv)
		if c != 200 || pv["detach_key_available"] != w.avail || pv["remote_key_may_remain"] != w.mayRemain || pv["deploy_key_shared"] != w.shared ||
			pv["deploy_key_fingerprint"] != w.fp || pv["deploy_key_title"] != w.title {
			t.Errorf("%s: %d %s", id, c, b)
		}
		for _, k := range []string{"detach_key_available", "deploy_key_fingerprint", "deploy_key_title", "deploy_key_shared"} {
			if _, has := pv[k]; !has {
				t.Errorf("%s: no %s", id, k)
			}
		}
		if strings.Contains(string(b), "ssh-ed25519") {
			t.Errorf("%s: a key body in the preview", id)
		}
	}
	// the signing preview of a detach names the repository and the fingerprint
	p := e.mustPropose(t, "remove_project", "gh-key", rmDetach(true))
	found := false
	for _, l := range p.Preview {
		if strings.Contains(l, "Octo-Cat/gh-key") && strings.Contains(l, fp) {
			found = true
		}
	}
	if !found {
		t.Fatalf("preview: %q", p.Preview)
	}
	// with detach off the W3f lines are unchanged (no key line)
	q := e.mustPropose(t, "remove_project", "gh-key", rmDetach(false))
	if strings.Contains(strings.Join(q.Preview, "\n"), fp) || len(q.Preview) != len(p.Preview)-1 {
		t.Fatalf("detach off preview: %q", q.Preview)
	}
}

func TestAlsoLocalRemovesLocalAfterBrokerRowRemoval(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	// answers that do not say the row was removed (incl. a W3f broker's) are an unknown outcome
	for _, res := range []string{
		`{"id":"app-one","remote_state":"deleted","remote_deleted_at":"` + delStamp + `"}`,
		`{"id":"app-one","remote_state":"deleted","remote_deleted_at":"` + delStamp + `","registry_removed":false}`,
		`{"id":"app-one","remote_state":"deleted","remote_deleted_at":"` + delStamp + `","registry_removed":"true"}`,
	} {
		e.resetDeleteLimits()
		e.setOverride(func(op string, a json.RawMessage) string {
			if op == "delete_remote_repo" {
				return `{"ok":true,"result":` + res + `}`
			}
			return ""
		})
		p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
		if c, b := e.applyDel(t, p); c != 504 || v1Code(b) != "outcome_unknown" {
			t.Errorf("%s: %d %s", res, c, b)
		}
		if !exists(filepath.Join(path, ".git")) || !exists(keyDir) {
			t.Fatalf("%s: local files removed", res)
		}
	}
	// the ok answer: the broker removed the row, then orchd removes the files
	e.resetDeleteLimits()
	var atBroker []bool
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "delete_remote_repo" {
			atBroker = append(atBroker, exists(path), exists(keyDir))
			e.dropRow("app-one")
		}
		return ""
	})
	before := e.changeCount()
	p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
	c, b := e.applyDel(t, p)
	if c != 200 {
		t.Fatalf("apply: %d %s", c, b)
	}
	if fmt.Sprint(atBroker) != "[true true]" {
		t.Fatalf("files at the broker call: %v", atBroker)
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	loc, _ := out["local"].(map[string]any)
	if out["registry"] != "removed" || out["also_local"] != true || out["remote_state"] != "deleted" || loc["clone"] != "removed" || loc["key"] != "removed" {
		t.Fatalf("%s", b)
	}
	if exists(path) || exists(keyDir) || e.hasRow("app-one") {
		t.Fatal("files or row left")
	}
	if e.changeCount() <= before {
		t.Fatal("the resolver cache was not told")
	}
	// one broker write, no follow-up remove_project
	if n := len(e.wb.opsNamed("remove_project")); n != 0 {
		t.Fatalf("a second write was sent: %d", n)
	}
	// also_local=false is W3f: the row stays (registry kept) and a registry_removed answer is not what was asked
	e.setOverride(nil)
	p2 := e.addDel(t, delProject{id: "app-two", origin: "created", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	q := e.mustPropose(t, "delete_remote_repo", "app-two", drFields("Octo-Cat/app-two", 124, "app-two", false))
	if c, b := e.applyDel(t, q); c != 200 || !strings.Contains(string(b), `"registry":"kept"`) || strings.Contains(string(b), `"local"`) || !exists(p2) {
		t.Fatalf("also_local=false: %d %s", c, b)
	}
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "delete_remote_repo" {
			return `{"ok":true,"result":{"id":"app-two","remote_state":"deleted","remote_deleted_at":"` + delStamp + `","registry_removed":true}}`
		}
		return ""
	})
	q = e.mustPropose(t, "delete_remote_repo", "app-two", drFields("Octo-Cat/app-two", 124, "app-two", false))
	if c, b := e.applyDel(t, q); c != 504 || v1Code(b) != "outcome_unknown" || !exists(p2) {
		t.Fatalf("registry_removed without also_local: %d %s", c, b)
	}
	// the preview no longer says the row stays after also_local
	e.setOverride(nil)
	e.addDel(t, delProject{id: "app-three", origin: "created", prov: "gh", remote: "Octo-Cat/app-three", remoteID: 125})
	r := e.mustPropose(t, "delete_remote_repo", "app-three", drFields("Octo-Cat/app-three", 125, "app-three", true))
	all := strings.Join(r.Preview, "\n")
	if strings.Contains(all, "'원격 없음' 상태로 남습니다") || strings.Contains(all, "한 번 더 필요") || !strings.Contains(all, "프로젝트 등록도 같은 서명으로 지웁니다") {
		t.Fatalf("also_local preview: %q", r.Preview)
	}
	// ... and without also_local it still says it does
	r = e.mustPropose(t, "delete_remote_repo", "app-three", drFields("Octo-Cat/app-three", 125, "app-three", false))
	if !strings.Contains(strings.Join(r.Preview, "\n"), "'원격 없음' 상태가 됩니다") {
		t.Fatalf("remote-only preview: %q", r.Preview)
	}
}

func TestAlsoLocalFailureLeavesUnregisteredFolderWithExactItems(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "delete_remote_repo" {
			e.dropRow("app-one")
		}
		return ""
	})
	p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
	if err := os.Chmod(e.root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.root, 0o700) })
	c, b := e.applyDel(t, p)
	d := detailOf(b)
	if c != 502 || v1Code(b) != "delete_partial_local" {
		t.Fatalf("%d %s", c, b)
	}
	rem, _ := d["remaining"].([]any)
	if fmt.Sprint(rem) != "[clone]" || d["clone"] != "kept_in_place" || d["key"] != "removed" || d["registry_removed"] != true ||
		d["remote_deleted"] != true || d["reason"] != "remove_failed" || len(d) != 6 {
		t.Fatalf("detail %v", d)
	}
	// what is left: an unregistered clone (the row is gone at the broker), no key folder
	if !exists(filepath.Join(path, ".git")) || exists(keyDir) || e.hasRow("app-one") {
		t.Fatal("state after the failure")
	}
	if len(e.wb.opsNamed("delete_remote_repo")) != 1 || len(e.wb.opsNamed("remove_project")) != 0 {
		t.Fatal("a retry or a second write was sent")
	}
	led := e.ledgerPayloads(t, "remote_delete")
	if len(led) != 2 || led[0]["ok"] != true || led[1]["ok"] != false || led[1]["code"] != "delete_partial_local" {
		t.Fatalf("ledger %v", led)
	}
	// recovery (G-D4): register the path again, then a signed remove_project (detach off: the remote is gone)
	os.Chmod(e.root, 0o700)
	e.setOverride(nil)
	e.addDel(t, delProject{id: "app-one", origin: "unknown", noClone: true})
	q := e.mustPropose(t, "remove_project", "app-one", rmDetach(false))
	if c, b := e.applyDel(t, q); c != 200 || exists(path) {
		t.Fatalf("recovery: %d %s", c, b)
	}
}

func TestLimitsDetachKey(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	// detach_key is a JSON bool and required
	for _, v := range []any{"true", 1, nil, "false", 0, []any{true}} {
		f := rmDetach(false)
		f["detach_key"] = v
		if c, b := e.proposeDel(t, "remove_project", "app-one", f); c != 400 || v1Code(b) != "bad_request" {
			t.Errorf("detach_key %#v: %d %s", v, c, b)
		}
	}
	f := rmDetach(false)
	delete(f, "detach_key")
	if c, b := e.proposeDel(t, "remove_project", "app-one", f); c != 400 {
		t.Errorf("without detach_key: %d %s", c, b)
	}
	// a string smuggled into a signed apply body is refused too
	p := e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
	ab := e.applyBody(p, webauthnSig)
	ab["fields"].(map[string]any)["detach_key"] = "true"
	if c, b := e.sockJSON(t, "POST", delPaths["remove_project"]+"/apply", jsonBody(ab)); c != 400 {
		t.Errorf("string in apply: %d %s", c, b)
	}
	if n := len(e.writes()); n != 0 {
		t.Fatalf("the broker was asked %d times", n)
	}
	// both buckets: the local-removal one and the key-removal one (G-D5)
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return errReply("remove_refused") // a known refusal keeps the slots
		}
		return ""
	})
	h := e.d.handler
	h.removeLimit, h.removeKeyLimit = newActorLimiter(3, time.Hour), newActorLimiter(1, time.Hour)
	apply := func(detach bool) (int, []byte) {
		return e.applyDel(t, e.mustPropose(t, "remove_project", "app-one", rmDetach(detach)))
	}
	if c, b := apply(true); c != 502 {
		t.Fatalf("first detach: %d %s", c, b)
	}
	n := len(e.wb.opsNamed("remove_project"))
	if c, b := apply(true); c != 429 || v1Code(b) != "project_rate_limited" || len(e.wb.opsNamed("remove_project")) != n {
		t.Fatalf("second detach (key bucket empty): %d %s", c, b)
	}
	// the local slot taken by the refused second detach was given back: two plain removals still fit
	for i := 0; i < 2; i++ {
		if c, b := apply(false); c != 502 {
			t.Fatalf("plain removal %d: %d %s", i, c, b)
		}
	}
	if c, b := apply(false); c != 429 {
		t.Fatalf("local bucket: %d %s", c, b)
	}
	// a plain removal never spends the key bucket; a detach that reached nobody gives both back
	h.removeLimit, h.removeKeyLimit = newActorLimiter(1, time.Hour), newActorLimiter(1, time.Hour)
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return errReply("busy")
		}
		return ""
	})
	for i := 0; i < 3; i++ {
		if c, b := apply(true); c != 409 || v1Code(b) != "busy" {
			t.Fatalf("busy %d: %d %s", i, c, b)
		}
	}
	h.removeLimit = newActorLimiter(5, time.Hour)
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return errReply("remove_refused")
		}
		return ""
	})
	for i := 0; i < 2; i++ {
		apply(false)
	}
	if c, b := apply(true); c != 502 {
		t.Fatalf("the key bucket was spent by plain removals: %d %s", c, b)
	}
	if RemoveProjectPerActorHour != 10 || RemoveKeyPerActorHour != 10 {
		t.Fatal("limits changed")
	}
}

func TestLedgerNoSecretsW3g(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	e.addDel(t, delProject{id: "app-two", origin: "created", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	e.addDel(t, delProject{id: "app-three", origin: "linked", prov: "gh", remote: "Octo-Cat/app-three", remoteID: 125, withKey: true})
	e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 126})
	var lines []string
	for _, id := range []string{"app-one", "app-two", "app-three"} {
		l, _ := pubFP(t, filepath.Join(e.keysDir(), id, "id_ed25519.pub"))
		lines = append(lines, l)
	}
	lines = append(lines, e.pubLine)
	do := func(action, subject string, fields map[string]any) {
		e.resetDeleteLimits()
		e.applyDel(t, e.mustPropose(t, action, subject, fields))
	}
	// refused, unknown, ok detach; ok also_local; gitlab detach
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return errReply("remove_refused")
		}
		return ""
	})
	do("remove_project", "app-three", rmDetach(true))
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return `{"ok":true,"result":{"id":"app-three","removed":true}}`
		}
		return ""
	})
	do("remove_project", "app-three", rmDetach(true))
	e.setOverride(nil)
	do("remove_project", "app-three", rmDetach(true))
	do("delete_remote_repo", "app-two", drFields("Octo-Cat/app-two", 124, "app-two", true))
	do("remove_project", "gl-one", rmDetach(true))
	evs := e.delLedger(t)
	if len(evs) != 5 {
		t.Fatalf("ledger events: %v", evs)
	}
	for _, p := range evs {
		delete(p, "_type")
		if ledgerKeys(p) != "action,actor,code,ok,subject" {
			t.Errorf("ledger keys %v", ledgerKeys(p))
		}
	}
	codes := []string{}
	for _, p := range evs {
		codes = append(codes, fmt.Sprint(p["subject"], ":", p["code"]))
	}
	sort.Strings(codes)
	if fmt.Sprint(codes) != "[app-three:ok app-three:outcome_unknown app-three:remove_refused app-two:ok gl-one:ok]" {
		t.Fatalf("codes %v", codes)
	}
	raw, err := os.ReadFile(policy.OrchLedgerPath(e.home))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{e.root, e.keysDir(), "PRIVATE-KEY", "webauthn", "signature", "Octo-Cat", "team/gl-one", "SHA256:", "orchd app"}
	for _, l := range lines {
		secrets = append(secrets, l, strings.Fields(l)[1])
	}
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Errorf("the ledger holds %q", s)
		}
		if strings.Contains(logs.String(), s) {
			t.Errorf("the log holds %q", s)
		}
	}
}

func TestOrchdWorksWithBrokerDownW3g(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	p := e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
	q := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
	e.d.handler.broker = &broker.Client{Socket: filepath.Join(shortDir(t), "none.sock")}
	for _, a := range []struct{ method, path, body string }{
		{"POST", delPaths["remove_project"] + "/propose", jsonBody(map[string]any{"action": "remove_project", "subject": "app-one", "fields": rmDetach(true)})},
		{"POST", delPaths["remove_project"] + "/apply", jsonBody(e.applyBody(p, webauthnSig))},
		{"POST", delPaths["delete_remote_repo"] + "/apply", jsonBody(e.applyBody(q, webauthnSig))},
		{"GET", "/api/v1/projects/app-one/removal-preview", ""},
	} {
		c, b := e.sockJSON(t, a.method, a.path, a.body)
		if c != 503 || v1Code(b) != "broker_unavailable" {
			t.Errorf("%s %s: %d %s", a.method, a.path, c, b)
		}
	}
	if !exists(filepath.Join(path, ".git")) || !exists(filepath.Join(e.keysDir(), "app-one", "id_ed25519")) {
		t.Fatal("files removed with the broker down")
	}
	if c, _ := e.sockJSON(t, "GET", "/api/v1/tasks", ""); c != 200 {
		t.Fatalf("tasks: %d", c)
	}
}

// Mixed versions: a W3f broker does not know detach_key (bad_request for the
// unknown argument) or the op (unknown_op): a clear "unavailable" refusal,
// nothing deleted; its also_local answer lacks registry_removed: unknown
// outcome, nothing local deleted.
func TestMixedVersionOldBrokerW3g(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	for _, code := range []string{"unknown_op", "bad_request", "not_implemented"} {
		for _, detach := range []bool{true, false} {
			e.resetDeleteLimits()
			e.setOverride(func(op string, a json.RawMessage) string {
				if op == "remove_project" {
					return `{"ok":false,"code":"` + code + `","message":"RAW unknown field detach_key"}`
				}
				return ""
			})
			p := e.mustPropose(t, "remove_project", "app-one", rmDetach(detach))
			c, b := e.applyDel(t, p)
			if c != 503 || v1Code(b) != "broker_unavailable" || !strings.Contains(string(b), "아무것도 지우지 않았습니다") || strings.Contains(string(b), "RAW") {
				t.Errorf("%s detach=%v: %d %s", code, detach, c, b)
			}
			if !exists(filepath.Join(path, ".git")) || !exists(keyDir) {
				t.Fatalf("%s: files removed", code)
			}
		}
	}
	// the W3f also_local answer (row kept, no registry_removed): nothing local is removed
	e.resetDeleteLimits()
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "delete_remote_repo" {
			return `{"ok":true,"result":{"id":"app-one","remote_state":"deleted","remote_deleted_at":"` + delStamp + `"}}`
		}
		return ""
	})
	q := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
	if c, b := e.applyDel(t, q); c != 504 || v1Code(b) != "outcome_unknown" || !exists(filepath.Join(path, ".git")) || !exists(keyDir) {
		t.Fatalf("W3f also_local answer: %d %s", c, b)
	}
}

// G2 review round 1.

func TestDetachedThenLocalFailureReportsKeyStateAndLedger(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	for _, ks := range []string{"removed", "already_gone"} {
		e.resetDeleteLimits()
		e.setOverride(func(op string, a json.RawMessage) string {
			if op == "remove_project" {
				return `{"ok":true,"result":{"id":"app-one","removed":true,"key_state":"` + ks + `"}}`
			}
			return ""
		})
		p := e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
		if err := os.Chmod(e.root, 0o500); err != nil {
			t.Fatal(err)
		}
		c, b := e.applyDel(t, p)
		os.Chmod(e.root, 0o700)
		d := detailOf(b)
		if c != 502 || v1Code(b) != "delete_partial_local" || d["key_state"] != ks || d["key_detached"] != true || d["registry_removed"] != true ||
			d["remote_deleted"] != false || d["clone"] != "kept_in_place" || d["reason"] != "remove_failed" {
			t.Fatalf("%s: %d %s", ks, c, b)
		}
		if !strings.Contains(string(b), "배포 키는 제공자에서 이미 떼어졌고") || !strings.Contains(string(b), "등록되지 않은 상태") {
			t.Fatalf("%s: sentence %s", ks, b)
		}
		if !exists(filepath.Join(path, ".git")) {
			t.Fatal("the clone should remain")
		}
		// the key folder went in the first round; recreate it for the second
		if !exists(filepath.Join(e.keysDir(), "app-one")) {
			e.addKeyDir(t, "app-one")
		}
	}
	led := e.ledgerPayloads(t, "project_remove")
	var got []string
	for _, l := range led {
		got = append(got, fmt.Sprint(l["ok"], ":", l["code"]))
	}
	if fmt.Sprint(got) != "[true:ok false:delete_partial_local true:ok false:delete_partial_local]" {
		t.Fatalf("ledger %v", got)
	}
	// without detach the W3f shape stays (no key members)
	e.resetDeleteLimits()
	e.setOverride(nil)
	p := e.mustPropose(t, "remove_project", "app-one", rmDetach(false))
	os.Chmod(e.root, 0o500)
	c, b := e.applyDel(t, p)
	os.Chmod(e.root, 0o700)
	d := detailOf(b)
	if _, has := d["key_state"]; c != 502 || has || len(d) != 6 {
		t.Fatalf("detach off: %d %s", c, b)
	}
}

func TestReviewSentencesW3g(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "no-key", origin: "created", prov: "gh", remote: "Octo-Cat/no-key", remoteID: 1})
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 2, withKey: true})
	// the unavailable key line on remove_project says the option can be turned off
	if c, b := e.proposeDel(t, "remove_project", "no-key", rmDetach(true)); c != 409 || v1Code(b) != "key_unavailable_for_removal" || !strings.Contains(string(b), "배포 키도 함께 떼기") {
		t.Fatalf("unavailable: %d %s", c, b)
	}
	// remove_deploy_key keeps the W3f sentence
	if c, b := e.proposeDel(t, "remove_deploy_key", "no-key", map[string]any{"remote_path": "Octo-Cat/no-key"}); c != 409 || strings.Contains(string(b), "함께 떼기") {
		t.Fatalf("remove_deploy_key: %d %s", c, b)
	}
	// unknown outcome: with detach the sentence says the key may be gone; without it, the W3f sentence
	for _, detach := range []bool{true, false} {
		for _, reply := range []string{"DROP", `{"ok":true,"result":{"id":"other","removed":true}}`} {
			e.resetDeleteLimits()
			e.setOverride(func(op string, a json.RawMessage) string {
				if op == "remove_project" {
					return reply
				}
				return ""
			})
			c, b := e.applyDel(t, e.mustPropose(t, "remove_project", "app-one", rmDetach(detach)))
			mentions := strings.Contains(string(b), "이미 떼어졌을 수 있습니다")
			if c != 504 || v1Code(b) != "outcome_unknown" || mentions != detach {
				t.Errorf("detach=%v %s: %d %s", detach, reply, c, b)
			}
		}
	}
	// broker bad_request on remove_project: unavailable + "state may have changed"
	e.resetDeleteLimits()
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return errReply("bad_request")
		}
		return ""
	})
	if c, b := e.applyDel(t, e.mustPropose(t, "remove_project", "app-one", rmDetach(false))); c != 503 || !strings.Contains(string(b), "그사이 프로젝트 상태가 바뀌었을 수 있습니다") {
		t.Fatalf("bad_request: %d %s", c, b)
	}
}

// (D) the other deletion actions keep their mapping of a broker bad_request.
func TestBadRequestOnOtherDeleteActionsUnchanged(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	e.addOrphan("gl", "team/half-made")
	for _, in := range []struct {
		action, subject string
		fields          map[string]any
	}{
		{"delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false)},
		{"delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true)},
		{"remove_deploy_key", "app-one", map[string]any{"remote_path": "Octo-Cat/app-one"}},
		{"clear_orphan", "gl", map[string]any{"project": "team/half-made"}},
	} {
		e.resetDeleteLimits()
		e.setOverride(func(op string, a json.RawMessage) string {
			if op == in.action {
				return errReply("bad_request")
			}
			return ""
		})
		c, b := e.applyDel(t, e.mustPropose(t, in.action, in.subject, in.fields))
		if c != 400 || v1Code(b) != "bad_request" {
			t.Errorf("%s: %d %s", in.action, c, b)
		}
	}
}

// (G) a proposal without detach carries only detach_key_available.
func TestProposeWithoutDetachHasNoKeyIdentity(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 3})
	for _, id := range []string{"app-one", "gl-one"} {
		c, b := e.proposeDel(t, "remove_project", id, rmDetach(false))
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if c != 200 || m["detach_key_available"] != true {
			t.Fatalf("%s: %d %s", id, c, b)
		}
		for _, k := range []string{"deploy_key_title", "deploy_key_fingerprint", "deploy_key_shared"} {
			if _, has := m[k]; has {
				t.Errorf("%s: %s present without detach_key", id, k)
			}
		}
	}
}
