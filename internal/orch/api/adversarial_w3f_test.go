package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W3f (F5): adversarial tests for the orchd side of project deletion. They run
// the real handler with the F3 kit (fake broker, real folders under a temp root).
// Properties already proved by a named F3 test are cited in docs/29, not repeated.

func setKind(e *delEnv, id, kind string) {
	e.wb.mu.Lock()
	defer e.wb.mu.Unlock()
	for _, row := range e.wb.projects {
		if row["id"] == id {
			row["kind"] = kind
		}
	}
}

// No request that is not a complete, well-formed, signed deletion of the right
// shape ever removes a file or reaches a broker write: no signature, a changed
// field, another subject, a proposal of one action applied at the endpoint of
// every other action (all 12 ordered pairs), hostile ids, wrong methods (there is
// no HTTP DELETE route anywhere), GET on every apply path. The legacy project
// (with its key folder) and a created project stay byte-for-byte as they were.
func TestNoUnsignedOrMisdirectedRequestRemovesAnythingW3f(t *testing.T) {
	e := newDelEnv(t)
	legacy := e.addDel(t, delProject{id: "sample-app", origin: "legacy", prov: "gl", remote: "team/sample-collector", remoteID: 9, withKey: true})
	setKind(e, "sample-app", "legacy")
	created := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	e.addOrphan("gl", "team/half-made")
	before := fileTree(t, e.root, e.sshDir())
	check := func(what string) {
		t.Helper()
		if after := fileTree(t, e.root, e.sshDir()); after != before {
			t.Fatalf("%s changed files:\n%s\n---\n%s", what, before, after)
		}
		if n := len(e.writes()); n != 0 {
			t.Fatalf("%s reached the broker with %d write(s)", what, n)
		}
		if len(e.delLedger(t)) != 0 {
			t.Fatalf("%s wrote a deletion ledger event", what)
		}
	}

	props := map[string]w3cProposal{
		"remove_project":     e.mustPropose(t, "remove_project", "app-one", rmFields(false)),
		"delete_remote_repo": e.mustPropose(t, "delete_remote_repo", "app-one", drFields("Octo-Cat/app-one", 123, "app-one", true)),
		"remove_deploy_key":  e.mustPropose(t, "remove_deploy_key", "app-one", map[string]any{"remote_path": "Octo-Cat/app-one"}),
		"clear_orphan":       e.mustPropose(t, "clear_orphan", "gl", map[string]any{"project": "team/half-made"}),
	}
	// every proposal at every other endpoint's apply
	pairs := 0
	for from, p := range props {
		for to := range props {
			if from == to {
				continue
			}
			body := e.applyBody(p, webauthnSig)
			c, b := e.sockJSON(t, "POST", delPaths[to]+"/apply", jsonBody(body))
			if c < 400 || c >= 500 {
				t.Errorf("%s proposal applied at %s: %d %s", from, to, c, b)
			}
			// the same with the action field rewritten to the endpoint's action
			body["action"] = to
			c, b = e.sockJSON(t, "POST", delPaths[to]+"/apply", jsonBody(body))
			if c < 400 || c >= 500 {
				t.Errorf("%s proposal relabelled and applied at %s: %d %s", from, to, c, b)
			}
			pairs++
		}
	}
	if pairs != 12 {
		t.Fatalf("pairs %d", pairs)
	}
	check("misdirected proposals")

	for name, p := range props {
		// no signature, empty signature, a changed value, another subject
		tamper := map[string]func(m map[string]any){
			"no signature":      func(m map[string]any) { delete(m, "signature") },
			"null signature":    func(m map[string]any) { m["signature"] = nil },
			"empty signature":   func(m map[string]any) { m["signature"] = map[string]any{} },
			"change zero":       func(m map[string]any) { m["change"] = strings.Repeat("0", 64) },
			"other subject":     func(m map[string]any) { m["subject"] = "sample-app" },
			"request dropped":   func(m map[string]any) { delete(m, "request") },
			"issued dropped":    func(m map[string]any) { delete(m, "issued") },
			"extra field":       func(m map[string]any) { m["fields"].(map[string]any)["scope_extra"] = "x" },
			"fields emptied":    func(m map[string]any) { m["fields"] = map[string]any{} },
			"fields null":       func(m map[string]any) { m["fields"] = nil },
			"id traversal":      func(m map[string]any) { m["subject"] = "../app-one" },
			"uppercase subject": func(m map[string]any) { m["subject"] = "APP-ONE" },
		}
		for tn, f := range tamper {
			body := e.applyBody(p, webauthnSig)
			if p.Fields != nil {
				// applyBody shares the proposal's fields map: copy before editing it
				cp := map[string]any{}
				for k, v := range p.Fields {
					cp[k] = v
				}
				body["fields"] = cp
			}
			f(body)
			c, b := e.sockJSON(t, "POST", delPaths[name]+"/apply", jsonBody(body))
			if c < 400 || c >= 500 {
				t.Errorf("%s / %s: %d %s", name, tn, c, b)
			}
		}
		// unreadable bodies
		for _, raw := range []string{"", "{", "[]", "null", `{"action":1}`, strings.Repeat("{", 70)} {
			c, b := e.sockJSON(t, "POST", delPaths[name]+"/apply", raw)
			if c < 400 || c >= 500 {
				t.Errorf("%s / body %q: %d %s", name, raw, c, b)
			}
		}
	}
	check("tampered applies")

	// methods and hostile ids: nothing but the documented routes exist
	paths := []string{}
	for _, p := range delPaths {
		paths = append(paths, p+"/apply", p+"/propose")
	}
	for _, p := range paths {
		for _, m := range []string{"GET", "PUT", "DELETE", "PATCH"} {
			if c, _ := e.sockJSON(t, m, p, "{}"); c < 400 {
				t.Errorf("%s %s answered %d", m, p, c)
			}
		}
	}
	for _, m := range []string{"DELETE", "PUT", "PATCH", "POST"} {
		for _, p := range []string{"/api/v1/projects/app-one", "/api/v1/projects/sample-app", "/api/v1/projects/app-one/removal-preview", "/api/v1/projects/app-one/remove", "/api/v1/projects", "/api/v1/repos/app-one"} {
			if c, b := e.sockJSON(t, m, p, "{}"); c < 400 {
				t.Errorf("%s %s answered %d %s", m, p, c, b)
			}
		}
	}
	for _, id := range []string{"..", "../app-one", "%2e%2e", "app-one%2f..", "APP-ONE", "app one", "a%00b", strings.Repeat("a", 90)} {
		if c, b := e.sockJSON(t, "GET", "/api/v1/projects/"+id+"/removal-preview", ""); c == http.StatusOK {
			t.Errorf("preview of hostile id %q answered 200: %s", id, b)
		}
	}
	check("wrong methods and hostile ids")

	// controls: the files are really there, and a genuine signed request still works
	if !exists(filepath.Join(legacy, ".git")) || !exists(filepath.Join(created, ".git")) || !exists(filepath.Join(e.keysDir(), "sample-app")) {
		t.Fatal("control: fixtures are missing")
	}
	if c, b := e.applyDel(t, e.mustPropose(t, "remove_project", "app-one", rmFields(false))); c != 200 {
		t.Fatalf("control: genuine removal refused: %d %s", c, b)
	}
	if exists(created) || !exists(legacy) {
		t.Fatal("control: the genuine removal removed the wrong folder")
	}
}

// The removal preview runs git in a folder an agent has written to. Whatever the
// repository's own configuration says (a filter driver, core.fsmonitor, a hooks
// directory, core.sshCommand, an editor, a diff driver, an include of a file with
// more of the same), no command from it runs, and the answer is counts and flags
// only: no path, name, branch or message appears.
func TestPreviewEndpointRunsNothingFromHostileRepoConfigW3f(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "unknown", noClone: true})
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, path, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(path, "HOSTILE-FILE-NAME.txt"), "one")
	git(t, path, "add", ".")
	git(t, path, "commit", "-q", "-m", "HOSTILE-COMMIT-MESSAGE")
	marker := filepath.Join(shortDir(t), "ran")
	script := filepath.Join(shortDir(t), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho x >> "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(shortDir(t), "extra.cfg")
	if err := os.WriteFile(extra, []byte("[core]\n\tfsmonitor = "+script+"\n[filter \"inc\"]\n\tclean = "+script+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(shortDir(t), "hooks")
	os.MkdirAll(hooks, 0o755)
	for _, h := range []string{"pre-commit", "post-checkout", "fsmonitor-watchman", "reference-transaction", "post-index-change"} {
		os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\necho x >> "+marker+"\n"), 0o755)
	}
	for k, v := range map[string]string{
		"core.fsmonitor":      script,
		"core.hooksPath":      hooks,
		"core.sshCommand":     script,
		"core.editor":         script,
		"core.pager":          script,
		"core.askPass":        script,
		"diff.external":       script,
		"filter.evil.clean":   script,
		"filter.evil.smudge":  script,
		"filter.evil.process": script,
		"credential.helper":   script,
		"include.path":        extra,
		"gc.auto":             "1",
	} {
		git(t, path, "config", k, v)
	}
	writeFile(t, filepath.Join(path, ".gitattributes"), "*.txt filter=evil\n*.bin diff=evil\n")
	writeFile(t, filepath.Join(path, "dirty.txt"), "changed")
	os.WriteFile(filepath.Join(path, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho x >> "+marker+"\n"), 0o755)

	for i := 0; i < 2; i++ {
		c, b := e.sockJSON(t, "GET", "/api/v1/projects/app-one/removal-preview", "")
		if c != 200 {
			t.Fatalf("preview: %d %s", c, b)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"HOSTILE", "evil", "dirty.txt", path, script, hooks, extra, `"main"`} {
			if strings.Contains(string(b), secret) {
				t.Errorf("the preview leaks %q: %s", secret, b)
			}
		}
		for k, v := range m {
			switch v.(type) {
			case bool, float64, nil:
			case string:
				if k != "id" && k != "origin" {
					t.Errorf("preview key %s is a string: %v", k, v)
				}
			default:
				t.Errorf("preview key %s has a structured value %v", k, v)
			}
		}
		if m["is_git_repo"] != true || m["folder_exists"] != true {
			t.Fatalf("control: the preview did not see the repository: %v", m)
		}
		// a filter/include section makes the changed count unknown, never a number from running it
		if m["changed_known"] != false {
			t.Errorf("changed_known = %v: with a filter in the config the count must be unknown", m["changed_known"])
		}
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the repository's own configuration ran a command: %q", b)
	}
	// and nothing in the repository changed
	if out := git(t, path, "log", "--format=%s"); strings.TrimSpace(out) != "HOSTILE-COMMIT-MESSAGE" {
		t.Fatalf("history changed: %s", fmt.Sprint(out))
	}
}
