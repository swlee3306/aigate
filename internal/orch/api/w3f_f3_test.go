package api

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
)

// ---- harness ----

const delStamp = "2026-11-13T01:02:03Z"

var delPaths = map[string]string{
	"remove_project":     "/api/v1/projects/remove",
	"delete_remote_repo": "/api/v1/projects/delete-remote",
	"remove_deploy_key":  "/api/v1/projects/remove-key",
	"clear_orphan":       "/api/v1/orphans/clear",
}

// delEnv is a W3c/W3e environment whose fake broker answers the four deletion
// ops (an override can replace any answer) and whose registry rows carry origin.
type delEnv struct {
	*w3cEnv
	mu       sync.Mutex
	override func(op string, args json.RawMessage) string
}

func newDelEnv(t *testing.T, opts ...func(*Config)) *delEnv {
	t.Helper()
	d := &delEnv{w3cEnv: ghEnv(t, opts...)}
	d.wb.setReply(func(op string, a json.RawMessage) string {
		d.mu.Lock()
		f := d.override
		d.mu.Unlock()
		if f != nil {
			if r := f(op, a); r != "" {
				return r
			}
		}
		var m map[string]any
		_ = json.Unmarshal(a, &m)
		ok := func(v any) string { b, _ := json.Marshal(map[string]any{"ok": true, "result": v}); return string(b) }
		switch op {
		case "remove_project":
			return ok(map[string]any{"id": m["id"], "removed": true})
		case "delete_remote_repo":
			return ok(map[string]any{"id": m["id"], "remote_state": "deleted", "remote_deleted_at": delStamp})
		case "remove_deploy_key":
			return ok(map[string]any{"id": m["id"], "key_removed": true})
		case "clear_orphan":
			return ok(map[string]any{"provider": m["id"], "project": m["project"], "cleared": true})
		}
		return ""
	})
	return d
}

func (d *delEnv) setOverride(f func(op string, args json.RawMessage) string) {
	d.mu.Lock()
	d.override = f
	d.mu.Unlock()
}

func errReply(code string) string { return `{"ok":false,"code":"` + code + `"}` }

type delProject struct {
	id, origin, prov, remote string
	remoteID                 int64
	deletedAt                string
	noClone, withKey         bool
}

// addDel registers a project row (path under the repo root) and makes its clone
// (a directory with a .git directory) and, with withKey, its key folder.
func (d *delEnv) addDel(t *testing.T, p delProject) string {
	t.Helper()
	path := filepath.Join(d.root, p.id)
	row := map[string]any{"id": p.id, "path": path, "kind": "registered", "ai_context": false, "provider": nil, "remote": nil,
		"auto_merge": map[string]any{"enabled": false}, "origin": p.origin}
	if p.prov != "" {
		row["provider"] = p.prov
	}
	if p.remote != "" {
		var id any
		if p.remoteID != 0 {
			id = p.remoteID
		}
		row["remote"] = map[string]any{"path": p.remote, "id": id}
	}
	if p.deletedAt != "" {
		row["remote_deleted_at"] = p.deletedAt
	}
	d.wb.mu.Lock()
	d.wb.projects = append(d.wb.projects, row)
	d.wb.mu.Unlock()
	if !p.noClone {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(path, "README.md"), "hello")
	}
	if p.withKey {
		d.addKeyDir(t, p.id)
	}
	return path
}

func (d *delEnv) addKeyDir(t *testing.T, id string) (line string) {
	t.Helper()
	if err := os.MkdirAll(d.keysDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(d.keysDir(), 0o700)
	kd := filepath.Join(d.keysDir(), id)
	if err := os.MkdirAll(kd, 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ = sshPubLine(t, "orchd-deploy")
	if err := os.WriteFile(filepath.Join(kd, "id_ed25519"), []byte("-----PRIVATE-KEY-MATERIAL-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kd, "id_ed25519.pub"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return line
}

func (d *delEnv) addOrphan(provider, project string) {
	d.wb.mu.Lock()
	d.wb.orphans = append(d.wb.orphans, map[string]any{"provider": provider, "project": project, "project_id": 7, "step": "project_created", "at": delStamp, "origin": "created"})
	d.wb.mu.Unlock()
}

func (d *delEnv) proposeDel(t *testing.T, action, subject string, fields map[string]any) (int, []byte) {
	t.Helper()
	return d.sockJSON(t, "POST", delPaths[action]+"/propose", jsonBody(map[string]any{"action": action, "subject": subject, "fields": fields}))
}

func (d *delEnv) mustPropose(t *testing.T, action, subject string, fields map[string]any) w3cProposal {
	t.Helper()
	c, b := d.proposeDel(t, action, subject, fields)
	if c != 200 {
		t.Fatalf("propose %s: %d %s", action, c, b)
	}
	var p w3cProposal
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (d *delEnv) applyDel(t *testing.T, p w3cProposal) (int, []byte) {
	t.Helper()
	return d.sockJSON(t, "POST", delPaths[p.Action]+"/apply", jsonBody(d.applyBody(p, webauthnSig)))
}

func rmFields(ack bool) map[string]any { return map[string]any{"scope": "local", "legacy_ack": ack} }

func drFields(path string, id int64, confirm string, also bool) map[string]any {
	return map[string]any{"remote_path": path, "remote_id": fmt.Sprint(id), "confirm_name": confirm, "also_local": also}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func (d *delEnv) writes() []regCall {
	return d.wb.opsNamed("remove_project", "delete_remote_repo", "remove_deploy_key", "clear_orphan")
}

func (d *delEnv) delLedger(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, typ := range []string{"project_remove", "remote_delete", "key_remove", "orphan_clear"} {
		for _, p := range d.ledgerPayloads(t, typ) {
			p["_type"] = typ
			out = append(out, p)
		}
	}
	return out
}

func detailOf(b []byte) map[string]any {
	var m struct {
		Error struct {
			Detail map[string]any `json:"detail"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Error.Detail
}

func (d *delEnv) task(t *testing.T, repo string, state store.State) int64 {
	t.Helper()
	id, err := d.st.CreateTask(context.Background(), store.Task{Repo: repo, Base: "main", Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if state != store.StateQueued {
		if err := d.st.Transition(context.Background(), id, store.StateQueued, state, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// ---- tests ----

func TestProposeStoresNothingNoSideEffects(t *testing.T) {
	e := newDelEnv(t)
	pathA := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	e.addDel(t, delProject{id: "app-two", origin: "linked", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	e.addOrphan("gl", "team/half-made")
	cases := []struct {
		action, subject string
		fields          map[string]any
	}{
		{"remove_project", "app-one", rmFields(false)},
		{"delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true)},
		{"remove_deploy_key", "app-two", map[string]any{"remote_path": "Octo-Cat/app-two"}},
		{"clear_orphan", "gl", map[string]any{"project": "team/half-made"}},
	}
	before := fileTree(t, e.root, e.sshDir())
	seen := map[string]bool{}
	for _, c := range cases {
		p := e.mustPropose(t, c.action, c.subject, c.fields)
		if p.Message == "" || p.Change == "" || len(p.Request) != 32 || len(p.Preview) == 0 || p.Action != c.action {
			t.Fatalf("%s: incomplete proposal %+v", c.action, p)
		}
		if seen[p.Request] {
			t.Fatal("a request id was repeated")
		}
		seen[p.Request] = true
		// the browser gets the fields back unchanged (it recomputes the change itself)
		if fmt.Sprint(p.Fields) != fmt.Sprint(c.fields) {
			t.Fatalf("%s: fields changed: %v", c.action, p.Fields)
		}
	}
	for _, c := range e.wb.allOps() {
		if c != "projects" {
			t.Fatalf("a proposal sent %q to the broker", c)
		}
	}
	if len(e.writes()) != 0 {
		t.Fatal("a proposal sent a write")
	}
	if after := fileTree(t, e.root, e.sshDir()); after != before {
		t.Fatalf("a proposal changed files:\n%s\n---\n%s", before, after)
	}
	if !exists(pathA) || len(e.delLedger(t)) != 0 {
		t.Fatal("a proposal removed something or wrote the ledger")
	}
	if e.changeCount() != 0 {
		t.Fatal("a proposal told the resolver the registry changed")
	}
}

// fileTree lists every path (and mode) below the given directories.
func fileTree(t *testing.T, dirs ...string) string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		_ = filepath.Walk(d, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				out = append(out, fmt.Sprintf("%s %v", p, fi.Mode()))
			}
			return nil
		})
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func TestApplyRecomputesChange(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false))
	// the signer saw also_local=false; a request that says true is another change
	body := e.applyBody(p, webauthnSig)
	body["fields"].(map[string]any)["also_local"] = true
	if c, b := e.sockJSON(t, "POST", delPaths[p.Action]+"/apply", jsonBody(body)); c != 400 || v1Code(b) != "change_mismatch" {
		t.Fatalf("flipped also_local: %d %s", c, b)
	}
	// a claimed change that is not the hash of the fields
	body = e.applyBody(p, webauthnSig)
	body["change"] = strings.Repeat("0", 64)
	if c, b := e.sockJSON(t, "POST", delPaths[p.Action]+"/apply", jsonBody(body)); c != 400 || v1Code(b) != "change_mismatch" {
		t.Fatalf("wrong change: %d %s", c, b)
	}
	// another subject with the same fields
	body = e.applyBody(p, webauthnSig)
	body["subject"] = "app-two"
	if c, b := e.sockJSON(t, "POST", delPaths[p.Action]+"/apply", jsonBody(body)); c != 400 {
		t.Fatalf("other subject: %d %s", c, b)
	}
	// an action that does not belong to the endpoint
	body = e.applyBody(p, webauthnSig)
	body["action"] = "remove_project"
	if c, b := e.sockJSON(t, "POST", delPaths["delete_remote_repo"]+"/apply", jsonBody(body)); c != 400 {
		t.Fatalf("wrong action: %d %s", c, b)
	}
	// no signature
	if c, b := e.sockJSON(t, "POST", delPaths[p.Action]+"/apply", jsonBody(e.applyBody(p, nil))); c != 400 || v1Code(b) != "not_signed" {
		t.Fatalf("unsigned: %d %s", c, b)
	}
	if len(e.writes()) != 0 || !exists(path) {
		t.Fatal("a refused apply reached the broker or removed files")
	}
	// the genuine request is forwarded with exactly the signed values
	if c, b := e.applyDel(t, p); c != 200 {
		t.Fatalf("apply: %d %s", c, b)
	}
	args := lastCall(t, e.w3cEnv, "delete_remote_repo")
	want := map[string]any{"id": "app-one", "remote_path": "Octo-Cat/app-one", "remote_id": "123", "confirm_name": "app-one", "also_local": false, "change": p.Change, "request": p.Request, "issued": p.Issued}
	for k, v := range want {
		if fmt.Sprint(args[k]) != fmt.Sprint(v) {
			t.Errorf("arg %s = %v, want %v", k, args[k], v)
		}
	}
	if _, has := args["signature"]; !has || len(args) != len(want)+1 {
		t.Errorf("args: %v", args)
	}
	// the same signed change a second time: the apply runs again only as far as the broker says
	// (orchd keeps no state), and a file-less second apply does not touch anything else
	if !exists(path) {
		t.Fatal("also_local=false removed files")
	}
}

func TestRemoveLegacyOnlyWithLegacyAck(t *testing.T) {
	e := newDelEnv(t)
	legacy := e.addDel(t, delProject{id: "toy", origin: "legacy"})
	plain := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123})
	// legacy without the second confirmation: refused before a signature
	if c, b := e.proposeDel(t, "remove_project", "toy", rmFields(false)); c != 400 || v1Code(b) != "legacy_ack_required" {
		t.Fatalf("legacy without ack: %d %s", c, b)
	}
	// a non-legacy project with the acknowledgement: the broker refuses too, orchd earlier
	if c, b := e.proposeDel(t, "remove_project", "app-one", rmFields(true)); c != 400 || v1Code(b) != "bad_request" {
		t.Fatalf("non-legacy with ack: %d %s", c, b)
	}
	// legacy remote deletion is never offered
	for _, origin := range []string{"legacy"} {
		_ = origin
		if c, b := e.proposeDel(t, "delete_remote_repo", "toy", drFields("team/toy", 1, "toy", true)); c != 409 || v1Code(b) != "origin_unknown" {
			t.Fatalf("legacy remote delete: %d %s", c, b)
		}
	}
	// apply of a hand-made request without the ack never reaches the broker
	good := e.mustPropose(t, "remove_project", "toy", rmFields(true))
	bad := good
	bad.Fields = rmFields(false)
	h, _ := changeOf(bad)
	bad.Change = h
	if c, b := e.sockJSON(t, "POST", delPaths["remove_project"]+"/apply", jsonBody(e.applyBody(bad, webauthnSig))); c != 400 || v1Code(b) != "legacy_ack_required" {
		t.Fatalf("legacy apply without ack: %d %s", c, b)
	}
	if len(e.writes()) != 0 || !exists(legacy) {
		t.Fatal("refused legacy request reached the broker or removed files")
	}
	// with the signed ack the removal happens: broker first, then the folder
	if c, b := e.applyDel(t, good); c != 200 {
		t.Fatalf("legacy with ack: %d %s", c, b)
	}
	if args := lastCall(t, e.w3cEnv, "remove_project"); args["legacy_ack"] != true || args["scope"] != "local" {
		t.Fatalf("args: %v", args)
	}
	if exists(legacy) || !exists(plain) {
		t.Fatal("wrong folders removed")
	}
	// a legacy project's folder is not removed by any other endpoint
	e.addDel(t, delProject{id: "sample-app", origin: "legacy", prov: "gl", remote: "team/sample-collector", remoteID: 9, withKey: true})
	if c, b := e.proposeDel(t, "delete_remote_repo", "sample-app", drFields("team/sample-collector", 9, "sample-collector", true)); c != 409 {
		t.Fatalf("%d %s", c, b)
	}
}

func TestRemoveRefusesBusy(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	actions := func() map[string]map[string]any {
		return map[string]map[string]any{
			"remove_project":     rmFields(false),
			"delete_remote_repo": drFields("Octo-Cat/app-one", 123, "app-one", true),
			"remove_deploy_key":  {"remote_path": "Octo-Cat/app-one"},
		}
	}
	for _, st := range []store.State{store.StateQueued, store.StatePreparing, store.StateAgentRunning, store.StateCollecting, store.StateVerifying,
		store.StateNeedsApproval, store.StatePassed, store.StatePublishing, store.StateError} {
		id := e.task(t, path, st)
		for action, fields := range actions() {
			if c, b := e.proposeDel(t, action, "app-one", fields); c != 409 || v1Code(b) != "project_busy" {
				t.Fatalf("%s in state %s: propose %d %s", action, st, c, b)
			}
		}
		// the preview says so too
		c, b := e.sockJSON(t, "GET", "/api/v1/projects/app-one/removal-preview", "")
		var pv map[string]any
		_ = json.Unmarshal(b, &pv)
		if c != 200 || pv["busy"] != true {
			t.Fatalf("preview in state %s: %d %s", st, c, b)
		}
		if err := e.st.Transition(context.Background(), id, st, store.StateCancelled, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// a task of another repository, a nested path of this one, and finished tasks
	other := e.task(t, filepath.Join(e.root, "other"), store.StateQueued)
	_ = other
	if c, b := e.proposeDel(t, "remove_project", "app-one", rmFields(false)); c != 200 {
		t.Fatalf("only finished or foreign tasks: %d %s", c, b)
	}
	nested := e.task(t, filepath.Join(path, "sub"), store.StateQueued)
	_ = nested
	if c, b := e.proposeDel(t, "remove_project", "app-one", rmFields(false)); c != 409 {
		t.Fatalf("a task below the project path: %d %s", c, b)
	}
	e.st.Transition(context.Background(), nested, store.StateQueued, store.StateCancelled, "test")
	// apply: a task that appears after the proposal is refused before any signature use
	p := e.mustPropose(t, "remove_project", "app-one", rmFields(false))
	busy := e.task(t, path, store.StateAgentRunning)
	if c, b := e.applyDel(t, p); c != 409 || v1Code(b) != "project_busy" {
		t.Fatalf("apply busy: %d %s", c, b)
	}
	if len(e.writes()) != 0 || !exists(path) {
		t.Fatal("busy apply reached the broker or removed files")
	}
	// a task that appears BETWEEN the broker's answer and the local removal keeps the files
	e.st.Transition(context.Background(), busy, store.StateAgentRunning, store.StateCancelled, "test")
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			e.task(t, path, store.StateQueued)
		}
		return ""
	})
	c, b := e.applyDel(t, p)
	d := detailOf(b)
	if c != 502 || v1Code(b) != "delete_partial_local" || d["reason"] != "project_busy" || d["registry_removed"] != true {
		t.Fatalf("task appeared late: %d %s", c, b)
	}
	if !exists(filepath.Join(path, ".git")) || !exists(filepath.Join(e.keysDir(), "app-one", "id_ed25519")) {
		t.Fatal("files were removed although a task appeared")
	}
	if rem, _ := d["remaining"].([]any); len(rem) != 2 {
		t.Fatalf("remaining: %v", d["remaining"])
	}
}

func TestRemoveProjectRemovesCloneAndKeyAfterTheBroker(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	other := e.addDel(t, delProject{id: "app-two", origin: "linked", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	var atBroker struct{ clone, key bool }
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			atBroker.clone = exists(path)
			atBroker.key = exists(filepath.Join(e.keysDir(), "app-one"))
		}
		return ""
	})
	p := e.mustPropose(t, "remove_project", "app-one", rmFields(false))
	c, b := e.applyDel(t, p)
	if c != 200 {
		t.Fatalf("apply: %d %s", c, b)
	}
	if !atBroker.clone || !atBroker.key {
		t.Fatal("local files were already gone when the broker was asked")
	}
	var out struct {
		ID      string            `json:"id"`
		Removed bool              `json:"removed"`
		Local   map[string]string `json:"local"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID != "app-one" || !out.Removed || out.Local["clone"] != "removed" || out.Local["key"] != "removed" {
		t.Fatalf("%s", b)
	}
	if exists(path) || exists(filepath.Join(e.keysDir(), "app-one")) {
		t.Fatal("the project's files are still there")
	}
	if !exists(other) || !exists(filepath.Join(e.keysDir(), "app-two", "id_ed25519")) {
		t.Fatal("another project's files were touched")
	}
	for _, d := range []string{e.root, e.keysDir()} {
		for _, n := range dirNames(t, d) {
			if strings.HasPrefix(n, ".deleting-") {
				t.Fatalf("a staging directory was left in %s", d)
			}
		}
	}
	// the broker was called exactly once (no retry), and the resolver cache was told
	if len(e.wb.opsNamed("remove_project")) != 1 || e.changeCount() != 1 {
		t.Fatalf("calls %d changes %d", len(e.wb.opsNamed("remove_project")), e.changeCount())
	}
	// the clone already gone: a project without a folder is removed as well (idempotent)
	e.addDel(t, delProject{id: "app-three", origin: "created", prov: "gh", remote: "Octo-Cat/app-three", remoteID: 125, noClone: true})
	p = e.mustPropose(t, "remove_project", "app-three", rmFields(false))
	c, b = e.applyDel(t, p)
	if c != 200 || !strings.Contains(string(b), `"clone":"already_gone"`) {
		t.Fatalf("no folder: %d %s", c, b)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestRemoveRefusesUnsafeLocalFolderBeforeSignature(t *testing.T) {
	e := newDelEnv(t)
	victim := t.TempDir()
	writeFile(t, filepath.Join(victim, "precious"), "keep")
	os.MkdirAll(filepath.Join(victim, ".git"), 0o755)
	// 1. the project folder is a symlink to a directory that looks like a clone
	e.addDel(t, delProject{id: "linked-one", origin: "created", prov: "gh", remote: "Octo-Cat/linked-one", remoteID: 1, noClone: true})
	if err := os.Symlink(victim, filepath.Join(e.root, "linked-one")); err != nil {
		t.Fatal(err)
	}
	// 2. no .git directory
	p2 := e.addDel(t, delProject{id: "no-git", origin: "created", prov: "gh", remote: "Octo-Cat/no-git", remoteID: 2})
	os.RemoveAll(filepath.Join(p2, ".git"))
	// 3. the key folder is a symlink
	e.addDel(t, delProject{id: "key-link", origin: "created", prov: "gh", remote: "Octo-Cat/key-link", remoteID: 3})
	os.MkdirAll(e.keysDir(), 0o700)
	os.Symlink(victim, filepath.Join(e.keysDir(), "key-link"))
	// 4. the registry path is not the project's own folder under a root
	row := map[string]any{"id": "elsewhere", "path": filepath.Join(victim, "elsewhere"), "kind": "registered", "ai_context": false, "provider": nil, "remote": nil,
		"auto_merge": map[string]any{"enabled": false}, "origin": "unknown"}
	e.wb.mu.Lock()
	e.wb.projects = append(e.wb.projects, row)
	e.wb.mu.Unlock()
	for _, id := range []string{"linked-one", "no-git", "key-link", "elsewhere"} {
		if c, b := e.proposeDel(t, "remove_project", id, rmFields(false)); c != 409 || v1Code(b) != "local_unsafe" {
			t.Errorf("%s: %d %s", id, c, b)
		}
	}
	// also_local on a remote delete is checked BEFORE the remote is touched
	if c, b := e.proposeDel(t, "delete_remote_repo", "no-git", drFields("Octo-Cat/no-git", 2, "no-git", true)); c != 409 || v1Code(b) != "local_unsafe" {
		t.Errorf("also_local unsafe: %d %s", c, b)
	}
	// without also_local the remote delete is fine (no local action is planned)
	if c, b := e.proposeDel(t, "delete_remote_repo", "no-git", drFields("Octo-Cat/no-git", 2, "no-git", false)); c != 200 {
		t.Errorf("remote only: %d %s", c, b)
	}
	if len(e.writes()) != 0 || !exists(filepath.Join(victim, "precious")) || !exists(filepath.Join(victim, ".git")) {
		t.Fatal("something was written or removed")
	}
}

func TestOrderRemoteBeforeLocal(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	var stateAtBroker []bool
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "delete_remote_repo" {
			stateAtBroker = append(stateAtBroker, exists(path), exists(keyDir))
		}
		return ""
	})
	p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
	c, b := e.applyDel(t, p)
	if c != 200 {
		t.Fatalf("apply: %d %s", c, b)
	}
	if len(stateAtBroker) != 2 || !stateAtBroker[0] || !stateAtBroker[1] {
		t.Fatalf("the local files must still exist when the remote delete is requested: %v", stateAtBroker)
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	loc, _ := out["local"].(map[string]any)
	if out["remote_state"] != "deleted" || out["also_local"] != true || out["registry"] != "kept" || loc["clone"] != "removed" || loc["key"] != "removed" || out["remote_deleted_at"] != delStamp {
		t.Fatalf("%s", b)
	}
	if exists(path) || exists(keyDir) {
		t.Fatal("also_local did not remove the files after the remote delete")
	}
	// orchd never unregisters the row itself: the broker saw exactly one write, the delete
	var writes []string
	for _, op := range e.wb.allOps() {
		if op != "projects" {
			writes = append(writes, op)
		}
	}
	if fmt.Sprint(writes) != "[delete_remote_repo]" {
		t.Fatalf("broker calls: %v", e.wb.allOps())
	}
	// without also_local nothing local is touched
	p2 := e.addDel(t, delProject{id: "app-two", origin: "created", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	q := e.mustPropose(t, "delete_remote_repo", "app-two", drFields("Octo-Cat/app-two", 124, "app-two", false))
	if c, b := e.applyDel(t, q); c != 200 || strings.Contains(string(b), `"local"`) {
		t.Fatalf("remote only: %d %s", c, b)
	}
	if !exists(p2) || !exists(filepath.Join(e.keysDir(), "app-two")) {
		t.Fatal("the local files were removed without a signed also_local")
	}
	// the gate is the SIGNED field: a broker reply cannot turn it on
	e.setOverride(nil)
	p3 := e.addDel(t, delProject{id: "app-three", origin: "created", prov: "gh", remote: "Octo-Cat/app-three", remoteID: 125})
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "delete_remote_repo" {
			return `{"ok":true,"result":{"id":"app-three","remote_state":"deleted","remote_deleted_at":"` + delStamp + `","also_local":true,"remove_local":true}}`
		}
		return ""
	})
	q = e.mustPropose(t, "delete_remote_repo", "app-three", drFields("Octo-Cat/app-three", 125, "app-three", false))
	if c, b := e.applyDel(t, q); c != 200 || !exists(p3) {
		t.Fatalf("reply-driven removal: %d %s exists=%v", c, b, exists(p3))
	}
}

func TestLocalFilesNeverRemovedOnUnsignedOrUnknownOutcome(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	bogus := func(s string) func(op string, a json.RawMessage) string {
		return func(op string, a json.RawMessage) string {
			if op == "delete_remote_repo" || op == "remove_project" {
				return s
			}
			return ""
		}
	}
	type tc struct {
		name     string
		reply    string // "" = default, "DROP" = no answer
		status   int
		code     string
		diagnose string
	}
	cases := []tc{
		{"bad signature", errReply("bad_signature"), 403, "bad_signature", ""},
		{"remote changed", errReply("remote_changed"), 409, "remote_changed", ""},
		{"refused by provider", errReply("remove_refused"), 502, "remove_refused", ""},
		{"delete_partial", errReply("delete_partial"), 504, "delete_partial", ""},
		{"unknown code on a write", errReply("some_new_code"), 504, "outcome_unknown", ""},
		{"internal_error", errReply("internal_error"), 504, "outcome_unknown", ""},
		{"dropped connection", "DROP", 504, "outcome_unknown", ""},
		{"ok without result", `{"ok":true}`, 504, "outcome_unknown", ""},
		{"ok for another project", `{"ok":true,"result":{"id":"other","remote_state":"deleted","remote_deleted_at":"` + delStamp + `"}}`, 504, "outcome_unknown", ""},
		{"ok with a made-up state", `{"ok":true,"result":{"id":"app-one","remote_state":"maybe","remote_deleted_at":"` + delStamp + `"}}`, 504, "outcome_unknown", ""},
		{"ok with a bad stamp", `{"ok":true,"result":{"id":"app-one","remote_state":"deleted","remote_deleted_at":"yesterday"}}`, 504, "outcome_unknown", ""},
	}
	for _, c := range cases {
		e.setOverride(bogus(c.reply))
		p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
		before := len(e.wb.opsNamed("delete_remote_repo"))
		code, b := e.applyDel(t, p)
		if code != c.status || v1Code(b) != c.code {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
		if got := len(e.wb.opsNamed("delete_remote_repo")) - before; got != 1 {
			t.Errorf("%s: the broker was asked %d times (no automatic retry)", c.name, got)
		}
		if !exists(filepath.Join(path, ".git")) || !exists(keyDir) {
			t.Fatalf("%s: local files were removed", c.name)
		}
		// no raw text
		if strings.Contains(string(b), "some_new_code") || strings.Contains(string(b), "maybe") {
			t.Errorf("%s: raw broker text in the answer: %s", c.name, b)
		}
		// give the limiter room for the next case
		e.d.handler.deleteRemoteLimit = newActorLimiter(100, time.Hour)
	}
	// remove_project the same way
	for _, c := range cases[:4] {
		e.setOverride(bogus(c.reply))
		p := e.mustPropose(t, "remove_project", "app-one", rmFields(false))
		if code, b := e.applyDel(t, p); code < 400 {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
		if !exists(filepath.Join(path, ".git")) || !exists(keyDir) {
			t.Fatalf("remove_project %s: local files were removed", c.name)
		}
	}
	e.setOverride(bogus(`{"ok":true,"result":{"id":"app-one","removed":false}}`))
	p := e.mustPropose(t, "remove_project", "app-one", rmFields(false))
	if code, b := e.applyDel(t, p); code != 504 || v1Code(b) != "outcome_unknown" || !exists(path) {
		t.Fatalf("removed:false: %d %s", code, b)
	}
}

func TestDeletePartialShowsStateAndRetry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true))
	// the remote delete succeeds, then the folder cannot be moved aside
	if err := os.Chmod(e.root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.root, 0o700) })
	c, b := e.applyDel(t, p)
	d := detailOf(b)
	if c != 502 || v1Code(b) != "delete_partial_local" || d["remote_deleted"] != true || d["registry_removed"] != false {
		t.Fatalf("%d %s", c, b)
	}
	rem, _ := d["remaining"].([]any)
	if fmt.Sprint(rem) != "[clone key]" {
		t.Fatalf("remaining: %v", rem)
	}
	if !exists(filepath.Join(path, ".git")) || !exists(keyDir) {
		t.Fatal("files vanished in a failed removal")
	}
	if len(e.wb.opsNamed("delete_remote_repo")) != 1 {
		t.Fatal("the remote delete was retried")
	}
	// the ledger says what happened: the remote delete was ok, the local part was not
	led := e.ledgerPayloads(t, "remote_delete")
	if len(led) != 2 || led[0]["ok"] != true || led[0]["code"] != "ok" || led[1]["ok"] != false || led[1]["code"] != "delete_partial_local" {
		t.Fatalf("ledger: %v", led)
	}
	// the retry is a NEW signature on remove_project; the row (remote gone) is still there
	if err := os.Chmod(e.root, 0o700); err != nil {
		t.Fatal(err)
	}
	q := e.mustPropose(t, "remove_project", "app-one", rmFields(false))
	if q.Request == p.Request {
		t.Fatal("same request id")
	}
	c, b = e.applyDel(t, q)
	if c != 200 || exists(path) || exists(keyDir) {
		t.Fatalf("retry: %d %s", c, b)
	}
	// ... and when the first attempt had already removed the files, the second signed
	// remove_project is the idempotent unregister
	e.addDel(t, delProject{id: "app-two", origin: "created", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, noClone: true})
	q = e.mustPropose(t, "remove_project", "app-two", rmFields(false))
	if c, b := e.applyDel(t, q); c != 200 || !strings.Contains(string(b), "already_gone") {
		t.Fatalf("%d %s", c, b)
	}
}

func TestRemoveDeleteRemoteRefusalsBeforeSignature(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "made-one", origin: "created", prov: "gh", remote: "Octo-Cat/Made-One", remoteID: 11})
	e.addDel(t, delProject{id: "linked-one", origin: "linked", prov: "gh", remote: "Octo-Cat/linked-one", remoteID: 12})
	e.addDel(t, delProject{id: "unknown-one", origin: "unknown", prov: "gh", remote: "Octo-Cat/unknown-one", remoteID: 13})
	e.addDel(t, delProject{id: "no-remote", origin: "created", prov: "gh"})
	e.addDel(t, delProject{id: "gone-remote", origin: "created", prov: "gh", deletedAt: delStamp})
	e.addDel(t, delProject{id: "no-id", origin: "created", prov: "gh", remote: "Octo-Cat/no-id"})
	cases := []struct {
		name, subject string
		fields        map[string]any
		status        int
		code          string
	}{
		{"linked", "linked-one", drFields("Octo-Cat/linked-one", 12, "linked-one", false), 409, "not_created_by_tool"},
		{"unknown origin", "unknown-one", drFields("Octo-Cat/unknown-one", 13, "unknown-one", false), 409, "origin_unknown"},
		{"no remote", "no-remote", drFields("Octo-Cat/no-remote", 1, "no-remote", false), 409, "remote_changed"},
		{"already deleted", "gone-remote", drFields("Octo-Cat/gone-remote", 1, "gone-remote", false), 409, "remote_changed"},
		{"registry id missing", "no-id", drFields("Octo-Cat/no-id", 1, "no-id", false), 409, "remote_changed"},
		{"wrong remote id", "made-one", drFields("Octo-Cat/Made-One", 99, "Made-One", false), 409, "remote_changed"},
		{"wrong remote path", "made-one", drFields("Octo-Cat/other", 11, "other", false), 409, "remote_changed"},
		{"confirm wrong case", "made-one", drFields("Octo-Cat/Made-One", 11, "made-one", false), 400, "confirm_mismatch"},
		{"confirm other name", "made-one", drFields("Octo-Cat/Made-One", 11, "Made", false), 400, "confirm_mismatch"},
		{"unknown project", "nobody", drFields("Octo-Cat/Made-One", 11, "Made-One", false), 404, "project_unknown"},
	}
	for _, c := range cases {
		if code, b := e.proposeDel(t, "delete_remote_repo", c.subject, c.fields); code != c.status || v1Code(b) != c.code {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
	}
	// GitHub path case differences are the same repository; the answer for a good request is 200
	if code, b := e.proposeDel(t, "delete_remote_repo", "made-one", drFields("octo-cat/made-one", 11, "Made-One", false)); code != 200 {
		t.Errorf("github case-insensitive path: %d %s", code, b)
	}
	if len(e.writes()) != 0 {
		t.Fatal("a refusal reached the broker")
	}
}

func TestRemoveKeyFlow(t *testing.T) {
	e := newDelEnv(t)
	line := ""
	e.addDel(t, delProject{id: "gh-one", origin: "linked", prov: "gh", remote: "Octo-Cat/gh-one", remoteID: 21, withKey: true})
	line, _, _ = readDeployKey(filepath.Join(e.keysDir(), "gh-one", "id_ed25519.pub"))
	e.addDel(t, delProject{id: "gh-nokey", origin: "linked", prov: "gh", remote: "Octo-Cat/gh-nokey", remoteID: 22})
	e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 23})
	e.addDel(t, delProject{id: "bare-one", origin: "created"})
	// no key line: refused before any signature, with the by-hand sentence
	c, b := e.proposeDel(t, "remove_deploy_key", "gh-nokey", map[string]any{"remote_path": "Octo-Cat/gh-nokey"})
	if c != 409 || v1Code(b) != "key_unavailable_for_removal" || !strings.Contains(string(b), "직접 제거") {
		t.Fatalf("no key line: %d %s", c, b)
	}
	if c, b := e.proposeDel(t, "remove_deploy_key", "bare-one", map[string]any{"remote_path": "Octo-Cat/bare-one"}); c != 409 || v1Code(b) != "no_remote" {
		t.Fatalf("no remote: %d %s", c, b)
	}
	if c, b := e.proposeDel(t, "remove_deploy_key", "gh-one", map[string]any{"remote_path": "Octo-Cat/elsewhere"}); c != 409 || v1Code(b) != "remote_changed" {
		t.Fatalf("other path: %d %s", c, b)
	}
	// the key line comes from the project's own key folder and is an argument, not a signed field
	p := e.mustPropose(t, "remove_deploy_key", "gh-one", map[string]any{"remote_path": "Octo-Cat/gh-one"})
	if _, has := p.Fields["deploy_key"]; has {
		t.Fatal("deploy_key must not be a signed field")
	}
	if c, b := e.applyDel(t, p); c != 200 || !strings.Contains(string(b), `"key_removed":true`) {
		t.Fatalf("apply: %d %s", c, b)
	}
	args := lastCall(t, e.w3cEnv, "remove_deploy_key")
	if args["deploy_key"] != line || args["remote_path"] != "Octo-Cat/gh-one" || args["id"] != "gh-one" {
		t.Fatalf("args: %v", args)
	}
	// the key folder and the clone stay: only the provider's copy was removed
	if !exists(filepath.Join(e.keysDir(), "gh-one", "id_ed25519")) || !exists(filepath.Join(e.root, "gh-one", ".git")) {
		t.Fatal("remove_deploy_key removed local files")
	}
	// the key is re-read at apply: a key file replaced after the proposal changes what is sent
	line2, _ := sshPubLine(t, "orchd-deploy")
	writeFile(t, filepath.Join(e.keysDir(), "gh-one", "id_ed25519.pub"), line2+"\n")
	if c, b := e.applyDel(t, p); c != 200 {
		t.Fatalf("second apply: %d %s", c, b)
	}
	if args := lastCall(t, e.w3cEnv, "remove_deploy_key"); args["deploy_key"] != line2 {
		t.Fatalf("a stale key line was sent: %v", args)
	}
	// GitLab: the shared key's .pub
	pg := e.mustPropose(t, "remove_deploy_key", "gl-one", map[string]any{"remote_path": "team/gl-one"})
	if c, b := e.applyDel(t, pg); c != 200 {
		t.Fatalf("gitlab: %d %s", c, b)
	}
	if args := lastCall(t, e.w3cEnv, "remove_deploy_key"); args["deploy_key"] != e.pubLine {
		t.Fatalf("gitlab key: %v", args)
	}
	// a symlinked key folder gives no key line
	other := t.TempDir()
	os.RemoveAll(filepath.Join(e.keysDir(), "gh-nokey"))
	os.Symlink(other, filepath.Join(e.keysDir(), "gh-nokey"))
	if c, b := e.proposeDel(t, "remove_deploy_key", "gh-nokey", map[string]any{"remote_path": "Octo-Cat/gh-nokey"}); c != 409 || v1Code(b) != "key_unavailable_for_removal" {
		t.Fatalf("symlinked key folder: %d %s", c, b)
	}
}

func TestClearOrphanFlow(t *testing.T) {
	e := newDelEnv(t)
	e.addOrphan("gl", "team/half-made")
	if c, b := e.proposeDel(t, "clear_orphan", "gl", map[string]any{"project": "team/other"}); c != 404 || v1Code(b) != "orphan_unknown" {
		t.Fatalf("unknown orphan: %d %s", c, b)
	}
	if c, b := e.proposeDel(t, "clear_orphan", "gh", map[string]any{"project": "team/half-made"}); c != 404 || v1Code(b) != "orphan_unknown" {
		t.Fatalf("other provider: %d %s", c, b)
	}
	p := e.mustPropose(t, "clear_orphan", "gl", map[string]any{"project": "team/half-made"})
	if c, b := e.applyDel(t, p); c != 200 || !strings.Contains(string(b), `"cleared":true`) {
		t.Fatalf("%d %s", c, b)
	}
	if args := lastCall(t, e.w3cEnv, "clear_orphan"); args["id"] != "gl" || args["project"] != "team/half-made" {
		t.Fatalf("args: %v", args)
	}
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "clear_orphan" {
			return `{"ok":true,"result":{"provider":"gl","project":"team/else","cleared":true}}`
		}
		return ""
	})
	if c, b := e.applyDel(t, p); c != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("reply for another record: %d %s", c, b)
	}
}

func TestBrokerCodesMapToFixedSentences(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	e.addOrphan("gl", "team/half-made")
	type in struct {
		action, subject string
		fields          map[string]any
	}
	ins := []in{
		{"remove_project", "app-one", rmFields(false)},
		{"delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false)},
		{"remove_deploy_key", "app-one", map[string]any{"remote_path": "Octo-Cat/app-one"}},
		{"clear_orphan", "gl", map[string]any{"project": "team/half-made"}},
	}
	codes := map[string]struct {
		status int
		code   string
	}{
		"legacy_ack_required": {400, "legacy_ack_required"}, "origin_unknown": {409, "origin_unknown"}, "not_created_by_tool": {409, "not_created_by_tool"},
		"remote_changed": {409, "remote_changed"}, "confirm_mismatch": {400, "confirm_mismatch"}, "key_not_found": {404, "key_not_found"},
		"remove_refused": {502, "remove_refused"}, "orphan_unknown": {404, "orphan_unknown"}, "delete_partial": {504, "delete_partial"},
		"busy": {409, "busy"}, "rate_limited": {429, "delete_rate_limited"}, "provider_rate_limited": {429, "provider_rate_limited"},
		"provider_unreachable": {502, "provider_unreachable"}, "github_refused": {502, "github_refused"}, "gitlab_refused": {502, "gitlab_refused"},
		"token_ref_unknown": {400, "token_ref_unknown"}, "bad_url": {400, "bad_url"}, "provider_unknown": {404, "provider_unknown"},
		"project_unknown": {404, "project_unknown"}, "bad_signature": {403, "bad_signature"}, "unknown_signer": {403, "unknown_signer"},
		"stale_signature": {403, "stale_signature"}, "request_reused": {409, "request_reused"}, "not_signed": {400, "not_signed"},
		"change_mismatch": {400, "change_mismatch"}, "signers_unavailable": {503, "signers_unavailable"},
		"registry_unavailable": {504, "outcome_unknown"}, "timeout": {504, "outcome_unknown"}, "internal_error": {504, "outcome_unknown"},
		"brand_new_code": {504, "outcome_unknown"},
	}
	var names []string
	for k := range codes {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		want := codes[name]
		for _, i := range ins {
			e.d.handler.removeLimit, e.d.handler.deleteRemoteLimit = newActorLimiter(100, time.Hour), newActorLimiter(100, time.Hour)
			e.d.handler.removeKeyLimit, e.d.handler.orphanLimit = newActorLimiter(100, time.Hour), newActorLimiter(100, time.Hour)
			e.d.handler.w3cProposeLimit = newActorLimiter(1000, time.Hour)
			e.setOverride(func(op string, a json.RawMessage) string {
				if op == i.action {
					return `{"ok":false,"code":"` + name + `","message":"RAW-BROKER-TEXT /home/orch/secret"}`
				}
				return ""
			})
			p := e.mustPropose(t, i.action, i.subject, i.fields)
			c, b := e.applyDel(t, p)
			if c != want.status || v1Code(b) != want.code {
				t.Errorf("%s via %s: %d %s (want %d %s)", name, i.action, c, b, want.status, want.code)
			}
			if strings.Contains(string(b), "RAW-BROKER-TEXT") || strings.Contains(string(b), "/home/orch") {
				t.Errorf("%s via %s: raw broker text: %s", name, i.action, b)
			}
		}
	}
	// refund rule: busy/forbidden/unsent give the slot back, a known refusal keeps it
	e.d.handler.deleteRemoteLimit = newActorLimiter(DeleteRemotePerActorHour, time.Hour)
	for _, c := range []struct {
		code string
		kept bool
	}{{"busy", false}, {"forbidden", false}, {"remote_changed", true}} {
		e.d.handler.deleteRemoteLimit = newActorLimiter(1, time.Hour)
		e.setOverride(func(op string, a json.RawMessage) string {
			if op == "delete_remote_repo" {
				return errReply(c.code)
			}
			return ""
		})
		p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false))
		e.applyDel(t, p)
		p2 := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false))
		code, b := e.applyDel(t, p2)
		limited := code == 429 && v1Code(b) == "project_rate_limited"
		if limited != c.kept {
			t.Errorf("%s: slot kept=%v, want %v (%d %s)", c.code, limited, c.kept, code, b)
		}
	}
}

func TestDeleteLimits(t *testing.T) {
	if RemoveProjectPerActorHour != 10 || DeleteRemotePerActorHour != 3 || RemoveKeyPerActorHour != 10 || ClearOrphanPerActorHour != 20 {
		t.Fatal("limits changed")
	}
	e := newDelEnv(t)
	clk := &testClock{}
	e.d.handler.now = clk.now
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, noClone: true})
	for i := 0; i < ClearOrphanPerActorHour; i++ {
		e.addOrphan("gl", fmt.Sprintf("team/o%d", i))
	}
	e.addOrphan("gl", "team/over")
	// remote delete: 3 an hour (the broker's answer does not matter, a known refusal keeps the slot)
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "delete_remote_repo" {
			return errReply("remove_refused")
		}
		return ""
	})
	for i := 0; i < DeleteRemotePerActorHour; i++ {
		p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false))
		if c, b := e.applyDel(t, p); c != 502 {
			t.Fatalf("%d: %d %s", i, c, b)
		}
	}
	n := len(e.wb.opsNamed("delete_remote_repo"))
	p := e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false))
	if c, b := e.applyDel(t, p); c != 429 || v1Code(b) != "project_rate_limited" || len(e.wb.opsNamed("delete_remote_repo")) != n {
		t.Fatalf("4th remote delete: %d %s", c, b)
	}
	// a second actor has its own bucket
	other := e.tokenCallerW3c(t, "mac-ssh")
	if c, b := other("POST", delPaths["delete_remote_repo"]+"/apply", jsonBody(e.applyBody(p, webauthnSig))); c != 502 {
		t.Fatalf("second actor: %d %s", c, b)
	}
	// the local-removal bucket is independent of the remote one
	e.setOverride(nil)
	for i := 0; i < 1; i++ {
		q := e.mustPropose(t, "remove_project", "app-one", rmFields(false))
		if c, b := e.applyDel(t, q); c != 200 {
			t.Fatalf("remove_project: %d %s", c, b)
		}
	}
	// orphan clearing: 20 an hour
	for i := 0; i < ClearOrphanPerActorHour; i++ {
		q := e.mustPropose(t, "clear_orphan", "gl", map[string]any{"project": fmt.Sprintf("team/o%d", i)})
		if c, b := e.applyDel(t, q); c != 200 {
			t.Fatalf("orphan %d: %d %s", i, c, b)
		}
	}
	q := e.mustPropose(t, "clear_orphan", "gl", map[string]any{"project": "team/over"})
	if c, b := e.applyDel(t, q); c != 429 {
		t.Fatalf("21st orphan: %d %s", c, b)
	}
	clk.set(time.Hour + time.Minute)
	if c, b := e.applyDel(t, q); c != 200 {
		t.Fatalf("after the hour: %d %s", c, b)
	}
}

func TestUnpushedWorkWarning(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, noClone: true, withKey: true})
	bare := filepath.Join(shortDir(t), "origin.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, bare, "init", "-q", "--bare")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, path, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(path, "tracked.txt"), "one")
	git(t, path, "add", ".")
	git(t, path, "commit", "-q", "-m", "first")
	git(t, path, "remote", "add", "origin", bare)
	git(t, path, "push", "-q", "-u", "origin", "main")
	preview := func() map[string]any {
		c, b := e.sockJSON(t, "GET", "/api/v1/projects/app-one/removal-preview", "")
		if c != 200 {
			t.Fatalf("preview: %d %s", c, b)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		m["_raw"] = string(b)
		return m
	}
	m := preview()
	if m["unpushed_commits"] != float64(0) || m["changed_files"] != float64(0) || m["unpushed_known"] != true || m["changed_known"] != true ||
		m["folder_exists"] != true || m["is_git_repo"] != true || m["local_removable"] != true || m["key_dir_exists"] != true ||
		m["busy"] != false || m["origin"] != "created" || m["legacy"] != false || m["has_remote"] != true || m["remote_deleted_at"] != nil {
		t.Fatalf("clean clone: %v", m)
	}
	// 2 unpushed commits (one on a side branch), 2 changed files; names and messages are planted
	writeFile(t, filepath.Join(path, "PLANTED-FILE-NAME.txt"), "x")
	git(t, path, "add", ".")
	git(t, path, "commit", "-q", "-m", "PLANTED-COMMIT-MESSAGE")
	git(t, path, "checkout", "-q", "-b", "planted-branch-name")
	writeFile(t, filepath.Join(path, "side.txt"), "y")
	git(t, path, "add", ".")
	git(t, path, "commit", "-q", "-m", "side")
	writeFile(t, filepath.Join(path, "tracked.txt"), "changed")
	writeFile(t, filepath.Join(path, "untracked-planted.txt"), "u")
	m = preview()
	if m["unpushed_commits"] != float64(2) || m["changed_files"] != float64(2) {
		t.Fatalf("counts: %v", m)
	}
	if size, _ := m["size_bytes"].(float64); size <= 0 || m["size_known"] != true {
		t.Fatalf("size: %v", m)
	}
	raw := m["_raw"].(string)
	for _, secret := range []string{"PLANTED", "planted", "tracked.txt", "side.txt", "README", "origin.git", e.root, bare, "first"} {
		if strings.Contains(raw, secret) {
			t.Errorf("the preview leaks %q: %s", secret, raw)
		}
	}
	// only these keys exist, all counts or flags
	keys := []string{}
	for k := range m {
		if k != "_raw" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	want := "busy,busy_unknown,changed_files,changed_known,folder_exists,has_remote,id,is_git_repo,key_dir_exists,legacy,local_removable,origin,remote_deleted_at,size_bytes,size_known,static_remote,unpushed_commits,unpushed_known"
	if strings.Join(keys, ",") != want {
		t.Fatalf("preview keys:\n%s\nwant\n%s", strings.Join(keys, ","), want)
	}
	// a clone whose folder is a symlink: nothing is read through it
	e.addDel(t, delProject{id: "sym-one", origin: "unknown", noClone: true})
	victim := t.TempDir()
	os.MkdirAll(filepath.Join(victim, ".git"), 0o755)
	writeFile(t, filepath.Join(victim, "VICTIM-NAME"), "z")
	os.Symlink(victim, filepath.Join(e.root, "sym-one"))
	c, b := e.sockJSON(t, "GET", "/api/v1/projects/sym-one/removal-preview", "")
	var sm map[string]any
	_ = json.Unmarshal(b, &sm)
	if c != 200 || sm["local_removable"] != false || sm["is_git_repo"] != false || sm["size_known"] != false || strings.Contains(string(b), "VICTIM") {
		t.Fatalf("symlinked folder: %d %s", c, b)
	}
	// unknown project, bad id
	if c, b := e.sockJSON(t, "GET", "/api/v1/projects/nobody/removal-preview", ""); c != 404 || v1Code(b) != "project_unknown" {
		t.Fatalf("%d %s", c, b)
	}
	if c, _ := e.sockJSON(t, "GET", "/api/v1/projects/Bad..Id/removal-preview", ""); c != 400 {
		t.Fatalf("bad id: %d", c)
	}
}

func TestLedgerNoSecrets(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	e.addDel(t, delProject{id: "app-two", origin: "linked", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	e.addDel(t, delProject{id: "app-three", origin: "created", prov: "gh", remote: "Octo-Cat/app-three", remoteID: 125})
	e.addOrphan("gl", "team/half-made")
	keyLine, _, _ := readDeployKey(filepath.Join(e.keysDir(), "app-two", "id_ed25519.pub"))
	do := func(action, subject string, fields map[string]any) {
		p := e.mustPropose(t, action, subject, fields)
		e.applyDel(t, p)
	}
	do("remove_deploy_key", "app-two", map[string]any{"remote_path": "Octo-Cat/app-two"})
	do("clear_orphan", "gl", map[string]any{"project": "team/half-made"})
	do("delete_remote_repo", "app-three", drFields("Octo-Cat/app-three", 125, "app-three", true))
	e.setOverride(func(op string, a json.RawMessage) string {
		if op == "remove_project" {
			return errReply("busy")
		}
		return ""
	})
	do("remove_project", "app-one", rmFields(false))
	e.setOverride(nil)
	do("remove_project", "app-one", rmFields(false))
	evs := e.delLedger(t)
	if len(evs) != 5 {
		t.Fatalf("ledger events: %v", evs)
	}
	wantType := map[string]string{"remove_project": "project_remove", "delete_remote_repo": "remote_delete", "remove_deploy_key": "key_remove", "clear_orphan": "orphan_clear"}
	got := map[string]int{}
	for _, p := range evs {
		keys := []string{}
		for k := range p {
			if k != "_type" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "action,actor,code,ok,subject" {
			t.Errorf("ledger keys %v", keys)
		}
		act, _ := p["action"].(string)
		if wantType[act] != p["_type"] {
			t.Errorf("event %v for action %s", p["_type"], act)
		}
		got[act]++
	}
	if got["remove_project"] != 2 || got["delete_remote_repo"] != 1 || got["remove_deploy_key"] != 1 || got["clear_orphan"] != 1 {
		t.Fatalf("events per action: %v", got)
	}
	raw, err := os.ReadFile(policy.OrchLedgerPath(e.home))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{e.root, e.keysDir(), keyLine, strings.Fields(keyLine)[1], "PRIVATE-KEY", "webauthn", "signature", "Octo-Cat", "half-made", "authenticator"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the ledger holds %q", secret)
		}
	}
	// the ledger chain still verifies
	if _, err := ledger.Events(policy.OrchLedgerPath(e.home)); err != nil {
		t.Fatal(err)
	}
	// the code of a refusal is recorded, the outcome too
	var busy map[string]any
	for _, p := range evs {
		if p["code"] == "busy" {
			busy = p
		}
	}
	if busy == nil || busy["ok"] != false || busy["subject"] != "app-one" {
		t.Fatalf("refusal not recorded: %v", evs)
	}
}

func TestRequireWriteAndCSRFDelete(t *testing.T) {
	e := newDelEnv(t)
	srv := e.httpServer(t)
	tok, _ := e.newSession(t, "alice")
	token := e.newToken(t, "mac-ssh")
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, noClone: true})
	e.addDel(t, delProject{id: "app-two", origin: "linked", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	e.addOrphan("gl", "team/half-made")
	type target struct {
		action, subject string
		fields          map[string]any
	}
	targets := []target{
		{"remove_project", "app-one", rmFields(false)},
		{"delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", false)},
		{"remove_deploy_key", "app-two", map[string]any{"remote_path": "Octo-Cat/app-two"}},
		{"clear_orphan", "gl", map[string]any{"project": "team/half-made"}},
	}
	for _, tg := range targets {
		p := e.mustPropose(t, tg.action, tg.subject, tg.fields)
		for _, ep := range []struct{ path, body string }{
			{delPaths[tg.action] + "/propose", jsonBody(map[string]any{"action": tg.action, "subject": tg.subject, "fields": tg.fields})},
			{delPaths[tg.action] + "/apply", jsonBody(e.applyBody(p, webauthnSig))},
		} {
			before := len(e.wb.allOps())
			for name, h := range map[string]hdr{
				"no credentials":     {},
				"session no csrf":    {"Cookie": cookie(tok)},
				"session wrong csrf": {"Cookie": cookie(tok), CSRFHeader: "csrf-wrong"},
				"bad bearer":         {"Authorization": "Bearer nope"},
			} {
				code, _, b := httpDo(t, srv, "POST", ep.path, ep.body, h)
				want := 403
				if name == "no credentials" || name == "bad bearer" {
					want = 401
				}
				if code != want {
					t.Errorf("%s %s: %d %s", ep.path, name, code, b)
				}
			}
			if len(e.wb.allOps()) != before {
				t.Errorf("%s: reached the broker without authority", ep.path)
			}
			if code, _, b := httpDo(t, srv, "POST", ep.path, ep.body, hdr{"Authorization": "Bearer " + token}); code != 200 {
				t.Errorf("%s with a token: %d %s", ep.path, code, b)
			}
		}
	}
	// GET on the POST endpoints, and the preview needs authentication
	if code, _, _ := httpDo(t, srv, "GET", "/api/v1/projects/remove/apply", "", hdr{"Authorization": "Bearer " + token}); code != 405 && code != 404 {
		t.Errorf("GET on apply: %d", code)
	}
	if code, _, _ := httpDo(t, srv, "GET", "/api/v1/projects/app-one/removal-preview", "", hdr{}); code != 401 {
		t.Errorf("preview without credentials: %d", code)
	}
	if code, _, b := httpDo(t, srv, "GET", "/api/v1/projects/app-one/removal-preview", "", hdr{"Authorization": "Bearer " + token}); code != 200 {
		t.Errorf("preview with a token: %d %s", code, b)
	}
}

func TestOrchdWorksWithBrokerDownW3f(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	p := e.mustPropose(t, "remove_project", "app-one", rmFields(false))
	e.d.handler.broker = &broker.Client{Socket: filepath.Join(shortDir(t), "none.sock")}
	for _, a := range []struct{ method, path, body string }{
		{"POST", delPaths["remove_project"] + "/propose", jsonBody(map[string]any{"action": "remove_project", "subject": "app-one", "fields": rmFields(false)})},
		{"POST", delPaths["remove_project"] + "/apply", jsonBody(e.applyBody(p, webauthnSig))},
		{"POST", delPaths["clear_orphan"] + "/propose", jsonBody(map[string]any{"action": "clear_orphan", "subject": "gl", "fields": map[string]any{"project": "team/x"}})},
		{"GET", "/api/v1/projects/app-one/removal-preview", ""},
	} {
		c, b := e.sockJSON(t, a.method, a.path, a.body)
		if c != 503 || v1Code(b) != "broker_unavailable" {
			t.Errorf("%s %s: %d %s", a.method, a.path, c, b)
		}
	}
	if !exists(path) {
		t.Fatal("files removed with the broker down")
	}
	// the rest of orchd keeps answering
	if c, _ := e.sockJSON(t, "GET", "/api/v1/tasks", ""); c != 200 {
		t.Fatalf("tasks: %d", c)
	}
	// no broker configured at all
	e.d.handler.broker = nil
	if c, b := e.sockJSON(t, "GET", "/api/v1/projects/app-one/removal-preview", ""); c != 503 {
		t.Fatalf("no client: %d %s", c, b)
	}
}

func TestStartupSweepRemovesOnlyOwnDeletingDirs(t *testing.T) {
	var roots string
	var planted []string
	e := newDelEnv(t, func(c *Config) {
		roots = c.AllowedRepos[0]
		os.MkdirAll(roots, 0o755)
		own := filepath.Join(roots, ".deleting-0123456789abcdef")
		os.MkdirAll(filepath.Join(own, "sub"), 0o755)
		os.WriteFile(filepath.Join(own, "sub", "f"), []byte("x"), 0o644)
		foreign := filepath.Join(roots, ".deleting-notmine")
		os.MkdirAll(foreign, 0o755)
		planted = []string{own, foreign}
	})
	_ = e
	if exists(planted[0]) {
		t.Fatal("a leftover .deleting directory survived the start")
	}
	if !exists(planted[1]) {
		t.Fatal("a directory with another name shape was removed")
	}
}

// ---- static pins ----

// The local removal functions are called from removeLocal only, and removeLocal
// only from finishDelete (which runs after the broker's ok to a signed change).
func TestLocalRemovalOnlyInTheSignedTail(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string][]string{}
	for _, pkg := range pkgs {
		for fname, f := range pkg.Files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						if id, ok := fun.X.(*ast.Ident); ok && id.Name == "remove" && (fun.Sel.Name == "Clone" || fun.Sel.Name == "KeyDir") {
							callers[fun.Sel.Name] = append(callers[fun.Sel.Name], fd.Name.Name+"@"+filepath.Base(fname))
						}
						if fun.Sel.Name == "removeLocal" {
							callers["removeLocal"] = append(callers["removeLocal"], fd.Name.Name+"@"+filepath.Base(fname))
						}
					}
					return true
				})
			}
		}
	}
	for _, name := range []string{"Clone", "KeyDir"} {
		if fmt.Sprint(callers[name]) != "[removeLocal@delete_project.go]" {
			t.Errorf("remove.%s is called from %v", name, callers[name])
		}
	}
	got := callers["removeLocal"]
	sort.Strings(got)
	if fmt.Sprint(got) != "[finishDelete@delete_project.go finishDelete@delete_project.go]" {
		t.Errorf("removeLocal is called from %v", got)
	}
	// nothing else in the package deletes files or directories
	for _, pkg := range pkgs {
		for fname, f := range pkg.Files {
			b, _ := os.ReadFile(fname)
			_ = f
			src := string(b)
			for _, bad := range []string{"os.RemoveAll(", "os.Remove("} {
				if strings.Contains(src, bad) {
					t.Logf("note: %s uses %s", filepath.Base(fname), bad)
				}
			}
		}
	}
}

// remote_deleted_at rides in the project view (exact stamp only); an odd stamp
// makes the whole list broker_error like any other implausible entry.
func TestProjectViewCarriesRemoteDeletedAt(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", deletedAt: delStamp, noClone: true})
	c, b := e.sockJSON(t, "GET", "/api/v1/projects/registry", "")
	if c != 200 || !strings.Contains(string(b), `"remote_deleted_at":"`+delStamp+`"`) || !strings.Contains(string(b), `"gitlab":null`) {
		t.Fatalf("%d %s", c, b)
	}
	e.addDel(t, delProject{id: "app-two", origin: "created", prov: "gh", deletedAt: "yesterday", noClone: true})
	if c, b := e.sockJSON(t, "GET", "/api/v1/projects/registry", ""); c != 502 {
		t.Fatalf("odd stamp: %d %s", c, b)
	}
}
