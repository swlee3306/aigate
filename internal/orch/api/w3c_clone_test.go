package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/clone"
)

func (e *w3cEnv) setProjects(ps ...map[string]any) {
	e.wb.mu.Lock()
	e.wb.projects = ps
	e.wb.mu.Unlock()
}

func (e *w3cEnv) linked(id string) map[string]any {
	return map[string]any{"id": id, "path": filepath.Join(e.root, id), "kind": "registered", "ai_context": false, "provider": "gl",
		"remote": map[string]any{"path": "team/" + id, "id": 99}, "auto_merge": map[string]any{"enabled": false}}
}

type cloneOut struct {
	Project ProjectView `json:"project"`
	Clone   CloneResult `json:"clone"`
}

func TestCloneEndpointUsesRegistryEntry(t *testing.T) {
	e := newW3cEnv(t)
	e.setProjects(e.linked("app-one"))
	code, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var out cloneOut
	if err := json.Unmarshal(b, &out); err != nil || out.Clone.Status != "cloned" || out.Clone.Code != "ok" || !out.Project.Cloned || out.Project.ID != "app-one" {
		t.Fatalf("%v %s", err, b)
	}
	calls := e.cloneCalls()
	if len(calls) != 1 {
		t.Fatalf("%d clones", len(calls))
	}
	o := calls[0]
	if o.Remote != "ssh://git@gitlab.example/team/app-one.git" || o.Dest != filepath.Join(e.root, "app-one") || o.SSHKey != e.key || o.KnownHosts != e.known ||
		len(o.Roots) != 1 || o.Roots[0] != e.d.handler.roots[0] || o.Timeout != clone.DefaultTimeout {
		t.Fatalf("options %+v", o)
	}
	if ops := e.wb.allOps(); len(ops) != 1 || ops[0] != "projects" {
		t.Fatalf("broker ops %v", ops)
	}
	// a second clone: the folder is there
	code, b = e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
	if code != 409 || v1Code(b) != "already_cloned" {
		t.Fatalf("second: %d %s", code, b)
	}
}

func TestCloneRemoteOnlyFromRegistry(t *testing.T) {
	e := newW3cEnv(t)
	e.setProjects(e.linked("app-one"))
	// Everything a request could try to steer is ignored.
	for name, body := range map[string]string{
		"url":    `{"url":"ssh://git@evil.example/x/y.git"}`,
		"remote": `{"remote":"ssh://git@gitlab.example/other/steal.git","dest":"/etc/cron.d/x","path":"/tmp/x","provider":"evil"}`,
		"junk":   `not json at all`,
		"array":  `[1,2,3]`,
		"big":    `{"x":"` + strings.Repeat("a", 3000) + `"}`,
	} {
		for _, d := range e.cloneCalls() {
			_ = d
		}
		os.RemoveAll(filepath.Join(e.root, "app-one"))
		code, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", body)
		if code != 200 {
			t.Fatalf("%s: %d %s", name, code, b)
		}
	}
	for i, o := range e.cloneCalls() {
		if o.Remote != "ssh://git@gitlab.example/team/app-one.git" || o.Dest != filepath.Join(e.root, "app-one") {
			t.Fatalf("clone %d took something from the request: %+v", i, o)
		}
	}
	// A hostile registry (a newer or tampered broker) is refused, never cloned.
	hostile := map[string]map[string]any{}
	mk := func(name string, mod func(m map[string]any)) {
		m := e.linked("app-one")
		mod(m)
		hostile[name] = m
	}
	mk("no provider", func(m map[string]any) { m["provider"] = nil })
	mk("no gitlab link", func(m map[string]any) { m["remote"] = nil })
	mk("unknown provider", func(m map[string]any) { m["provider"] = "evil" })
	mk("project name differs from id", func(m map[string]any) {
		m["remote"] = map[string]any{"path": "team/other-name", "id": 1}
	})
	mk("traversal in project", func(m map[string]any) { m["remote"] = map[string]any{"path": "team/../x", "id": 1} })
	mk("path outside roots", func(m map[string]any) { m["path"] = "/etc/app-one" })
	mk("path two levels deep", func(m map[string]any) { m["path"] = filepath.Join(e.root, "x", "app-one") })
	mk("folder differs from id", func(m map[string]any) { m["path"] = filepath.Join(e.root, "other-folder") })
	for name, m := range hostile {
		e.setProjects(m)
		before := len(e.cloneCalls())
		os.RemoveAll(filepath.Join(e.root, "app-one"))
		code, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
		if code == 200 || len(e.cloneCalls()) != before {
			t.Errorf("%s: %d %s (clone ran: %v)", name, code, b, len(e.cloneCalls()) != before)
		}
		if strings.Contains(string(b), "evil") {
			t.Errorf("%s: leaked %s", name, b)
		}
	}
	// Unknown id, malformed id: no clone; the malformed id never reaches the broker.
	e.setProjects(e.linked("app-one"))
	n := len(e.wb.allOps())
	if code, b := e.sockJSON(t, "POST", "/api/v1/projects/nope-x/clone", ""); code != 404 || v1Code(b) != "project_unknown" {
		t.Errorf("unknown: %d %s", code, b)
	}
	n2 := len(e.wb.allOps())
	for _, bad := range []string{"App", "..", "a%2Fb", "x y", "-x"} {
		if code, b := e.sockJSON(t, "POST", "/api/v1/projects/"+bad+"/clone", ""); code != 400 && code != 404 {
			t.Errorf("%q: %d %s", bad, code, b)
		}
	}
	if len(e.wb.allOps()) != n2 || n2 != n+1 {
		t.Errorf("broker ops: %d %d %d", n, n2, len(e.wb.allOps()))
	}
}

func TestCloneErrorsAreFixed(t *testing.T) {
	type want struct {
		status int
		code   string
	}
	for _, tc := range []struct {
		err  error
		want want
	}{
		{clone.ErrHostNotPinned, want{400, "host_not_pinned"}},
		{clone.ErrExists, want{409, "already_cloned"}},
		{clone.ErrInProgress, want{409, "clone_in_progress"}},
		{clone.ErrTimeout, want{504, "clone_timeout"}},
		{clone.ErrFailed, want{502, "clone_failed"}},
		{clone.ErrBadPath, want{400, "path_not_allowed"}},
		{clone.ErrBadRemote, want{400, "not_clonable"}},
		{clone.ErrBadConfig, want{503, "clone_unavailable"}},
		{os.ErrPermission, want{502, "clone_failed"}},
	} {
		e := newW3cEnv(t)
		e.setProjects(e.linked("app-one"))
		e.cmu.Lock()
		e.cloneErr = tc.err
		e.cmu.Unlock()
		code, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
		if code != tc.want.status || v1Code(b) != tc.want.code {
			t.Errorf("%v: %d %s", tc.err, code, b)
		}
		if strings.Contains(string(b), tc.err.Error()) && tc.err != clone.ErrFailed {
			// the sentinel's own text is not echoed; fixed Korean sentences are
			t.Errorf("%v: error text echoed: %s", tc.err, b)
		}
	}
	// not configured: no key or no pinning file
	for name, opt := range map[string]func(c *Config){
		"no key":   func(c *Config) { c.GitSSHKey = "" },
		"no known": func(c *Config) { c.GitSSHKnownHosts = "" },
	} {
		e := newW3cEnv(t, opt)
		e.setProjects(e.linked("app-one"))
		code, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
		if code != 503 || v1Code(b) != "clone_unavailable" || len(e.cloneCalls()) != 0 {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// broker down
	dead := newW3cEnv(t, func(c *Config) { c.BrokerSocket = filepath.Join(shortDir(t), "none.sock") })
	if code, b := dead.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); code != 503 || v1Code(b) != "broker_unavailable" {
		t.Errorf("broker down: %d %s", code, b)
	}
}

func TestListResponsesCarryClonedAndOrphans(t *testing.T) {
	e := newW3cEnv(t)
	for _, n := range []string{"has-git", "no-git"} {
		os.MkdirAll(filepath.Join(e.root, n), 0o755)
	}
	os.MkdirAll(filepath.Join(e.root, "has-git", ".git"), 0o755)
	linkTarget := t.TempDir()
	os.MkdirAll(filepath.Join(linkTarget, ".git"), 0o755) // a real clone behind a symlink must not count
	os.Symlink(linkTarget, filepath.Join(e.root, "is-link"))
	e.setProjects(e.linked("has-git"), e.linked("no-git"), e.linked("is-link"), e.linked("absent"), map[string]any{"id": "legacy", "path": e.repo, "kind": "docs", "ai_context": false,
		"provider": nil, "remote": nil, "auto_merge": map[string]any{"enabled": false}})
	e.wb.mu.Lock()
	e.wb.orphans = []map[string]any{{"provider": "gl", "project": "team/lost-app", "project_id": 7, "step": "deploy_key", "at": "2026-11-09T12:00:00Z"},
		{"provider": "gl", "project": "team/lost-two", "project_id": nil, "step": "project", "at": "2026-11-09T13:00:00Z"}}
	e.wb.mu.Unlock()
	code, b := e.sockJSON(t, "GET", "/api/v1/projects/registry", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var got struct {
		Projects  []ProjectView  `json:"projects"`
		Providers []ProviderView `json:"providers"`
		Orphans   []OrphanView   `json:"orphans"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	cl := map[string]bool{}
	for _, p := range got.Projects {
		cl[p.ID] = p.Cloned
	}
	if !cl["has-git"] || cl["no-git"] || cl["is-link"] || cl["absent"] || !cl["legacy"] {
		t.Fatalf("cloned flags %v", cl)
	}
	if got.Projects[0].Provider == nil || *got.Projects[0].Provider != "gl" || got.Projects[0].GitLab == nil || got.Projects[0].GitLab.Project != "team/has-git" {
		t.Fatalf("project view %+v", got.Projects[0])
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "gl" || got.Providers[0].TokenRef != "gl-admin" {
		t.Fatalf("providers %+v", got.Providers)
	}
	if len(got.Orphans) != 2 || got.Orphans[0].Project != "team/lost-app" || got.Orphans[0].ProjectID == nil || *got.Orphans[0].ProjectID != 7 || got.Orphans[1].ProjectID != nil {
		t.Fatalf("orphans %+v", got.Orphans)
	}
	// Public values only: exactly these keys.
	var raw struct {
		Orphans []map[string]any `json:"orphans"`
	}
	json.Unmarshal(b, &raw)
	if ledgerKeys(raw.Orphans[0]) != "at,origin,project,project_id,provider,step" {
		t.Fatalf("orphan keys %v", raw.Orphans[0])
	}
	// No orphans and no providers are empty arrays, never null.
	e.wb.mu.Lock()
	e.wb.orphans, e.wb.providers = nil, nil
	e.wb.mu.Unlock()
	_, b = e.sockJSON(t, "GET", "/api/v1/projects/registry", "")
	if !strings.Contains(string(b), `"orphans":[]`) || !strings.Contains(string(b), `"providers":[]`) {
		t.Fatalf("%s", b)
	}
	// One implausible row fails the whole list (fail closed).
	for name, o := range map[string]map[string]any{
		"hostile project": {"provider": "gl", "project": "team/../x", "project_id": 7, "step": "project", "at": "2026-11-09T12:00:00Z"},
		"bad step":        {"provider": "gl", "project": "team/ab", "project_id": 7, "step": "rm -rf", "at": "2026-11-09T12:00:00Z"},
		"bad time":        {"provider": "gl", "project": "team/ab", "project_id": 7, "step": "project", "at": "yesterday"},
		"bad provider":    {"provider": "G L", "project": "team/ab", "project_id": 7, "step": "project", "at": "2026-11-09T12:00:00Z"},
	} {
		e.wb.mu.Lock()
		e.wb.orphans = []map[string]any{o}
		e.wb.mu.Unlock()
		if code, b := e.sockJSON(t, "GET", "/api/v1/projects/registry", ""); code != 502 || v1Code(b) != "broker_error" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// An orphan list longer than the broker's own cap of 100 is refused too.
	many := []map[string]any{}
	for i := 0; i < 101; i++ {
		many = append(many, map[string]any{"provider": "gl", "project": "team/ab", "project_id": 1, "step": "project", "at": "2026-11-09T12:00:00Z"})
	}
	e.wb.mu.Lock()
	e.wb.orphans = many
	e.wb.mu.Unlock()
	if code, b := e.sockJSON(t, "GET", "/api/v1/projects/registry", ""); code != 502 || v1Code(b) != "broker_error" {
		t.Errorf("101 orphans: %d %s", code, b)
	}
}

// create_project success: the clone remote is rebuilt from the registry (a
// re-read), never from what the create reply says; the clone runs once, with
// a budget that keeps the whole call under the browser's 90 s.
func TestCreateApplySuccessAutoCloneFromRegistry(t *testing.T) {
	e := newW3cEnv(t)
	p := e.proposeAction(t, "create_project")
	start := time.Now()
	code, b := e.apply(t, p)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var out cloneOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Project.ID != "app-one" || out.Project.Kind != "registered" || out.Project.Provider == nil || *out.Project.Provider != "gl" ||
		out.Project.GitLab == nil || out.Project.GitLab.Project != "team/app-one" || !out.Project.Cloned || out.Clone.Status != "cloned" || out.Clone.Code != "ok" {
		t.Fatalf("response %s", b)
	}
	for _, leak := range []string{"evil.example", "clone_url", "ssh_url", "cloned_hint", e.pubLine} {
		if strings.Contains(string(b), leak) {
			t.Errorf("response carries %q: %s", leak, b)
		}
	}
	calls := e.cloneCalls()
	if len(calls) != 1 {
		t.Fatalf("%d clones", len(calls))
	}
	o := calls[0]
	if o.Remote != "ssh://git@gitlab.example/team/app-one.git" || o.Dest != filepath.Join(e.root, "app-one") {
		t.Fatalf("clone options %+v", o)
	}
	if o.Timeout <= 0 || o.Timeout > 88*time.Second {
		t.Fatalf("clone budget %v", o.Timeout)
	}
	_ = start
	ops := e.wb.allOps()
	if len(ops) != 2 || ops[0] != "create_project" || ops[1] != "projects" {
		t.Fatalf("broker ops %v (want the create, then one registry re-read)", ops)
	}
	if e.changeCount() != 1 {
		t.Fatalf("OnRegistryChange called %d times", e.changeCount())
	}

	// Clone failures do not undo the create: 200 with the fixed status; the
	// project can be cloned again later with no signature.
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"host not pinned": {clone.ErrHostNotPinned, "host_not_pinned"},
		"git failed":      {clone.ErrFailed, "clone_failed"},
		"timeout":         {clone.ErrTimeout, "clone_timeout"},
		"already there":   {clone.ErrExists, "already_cloned"},
	} {
		e2 := newW3cEnv(t)
		e2.cmu.Lock()
		e2.cloneErr = tc.err
		e2.cmu.Unlock()
		code, b := e2.apply(t, e2.proposeAction(t, "create_project"))
		var o cloneOut
		json.Unmarshal(b, &o)
		wantStatus := "failed"
		if tc.code == "already_cloned" {
			wantStatus = "already_cloned"
		}
		if code != 200 || o.Clone.Status != wantStatus || o.Clone.Code != tc.code || o.Project.Cloned || o.Project.ID != "app-one" {
			t.Errorf("%s: %d %s", name, code, b)
		}
		if n := len(e2.cloneCalls()); n != 1 {
			t.Errorf("%s: %d clone attempts (exactly one automatic attempt)", name, n)
		}
	}
	// clone not configured
	e3 := newW3cEnv(t, func(c *Config) { c.GitSSHKnownHosts = "" })
	code, b = e3.apply(t, e3.proposeAction(t, "create_project"))
	var o3 cloneOut
	json.Unmarshal(b, &o3)
	if code != 200 || o3.Clone.Status != "failed" || o3.Clone.Code != "clone_unavailable" || len(e3.cloneCalls()) != 0 {
		t.Errorf("unconfigured: %d %s", code, b)
	}
	// the registry re-read fails: the create stands, the clone is reported failed
	e4 := newW3cEnv(t)
	e4.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "projects" {
			return brokerErrReply("registry_unavailable")
		}
		return ""
	})
	code, b = e4.apply(t, e4.proposeAction(t, "create_project"))
	var o4 cloneOut
	json.Unmarshal(b, &o4)
	if code != 200 || o4.Clone.Status != "failed" || o4.Clone.Code != "registry_unavailable" || len(e4.cloneCalls()) != 0 {
		t.Errorf("re-read failure: %d %s", code, b)
	}
	// a reply that is not the project we asked for is an unknown outcome
	e5 := newW3cEnv(t)
	e5.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "create_project" {
			return `{"ok":true,"result":{"project":{"id":"someone-else","path":"/x/y","kind":"registered","ai_context":false,"provider":"gl","remote":{"path":"team/someone-else","id":1},"auto_merge":{"enabled":false}}}}`
		}
		return ""
	})
	if code, b := e5.apply(t, e5.proposeAction(t, "create_project")); code != 504 || v1Code(b) != "outcome_unknown" || len(e5.cloneCalls()) != 0 {
		t.Errorf("wrong project in the reply: %d %s", code, b)
	}
}

func TestRegistryChangeHookOnlyWhenSomethingMayHaveChanged(t *testing.T) {
	// success, create_partial and an unknown outcome drop the resolver's cache;
	// a refusal and an unsent request do not.
	count := func(reply func(op string, _ json.RawMessage) string, action string) int {
		e := newW3cEnv(t)
		e.wb.setReply(reply)
		e.apply(t, e.proposeAction(t, action))
		return e.changeCount()
	}
	only := func(code string) func(string, json.RawMessage) string {
		return func(op string, _ json.RawMessage) string {
			if op != "projects" && op != "providers" {
				return brokerErrReply(code)
			}
			return ""
		}
	}
	for _, action := range w3cActions {
		if n := count(nil, action); n != 1 {
			t.Errorf("%s success: %d", action, n)
		}
		for _, c := range []string{"bad_signature", "project_name_taken", "provider_in_use", "group_exists", "deploy_key_mismatch"} {
			if n := count(only(c), action); n != 0 {
				t.Errorf("%s refused with %s: hook called %d times", action, c, n)
			}
		}
		for _, c := range []string{"timeout", "internal_error", "registry_unavailable"} {
			if n := count(only(c), action); n != 1 {
				t.Errorf("%s unknown outcome %s: hook called %d times", action, c, n)
			}
		}
	}
	if n := count(only("create_partial"), "create_project"); n != 1 {
		t.Errorf("create_partial: %d", n)
	}
	dead := newW3cEnv(t, func(c *Config) { c.BrokerSocket = filepath.Join(shortDir(t), "none.sock") })
	dead.apply(t, dead.proposeAction(t, "set_provider"))
	if dead.changeCount() != 0 {
		t.Error("hook called for an unsent request")
	}
}
