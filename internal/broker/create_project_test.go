package broker

import (
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func partialOf(t *testing.T, e *OpError) PartialDetail {
	t.Helper()
	if e == nil || e.Code != "create_partial" {
		t.Fatalf("want create_partial, got %v", e)
	}
	d, ok := e.Data.(PartialDetail)
	if !ok {
		t.Fatalf("create_partial without detail: %T", e.Data)
	}
	return d
}

func TestCreateProjectHappyPath(t *testing.T) {
	c := newC3(t)
	// the registry must NOT hold the project while GitLab is still being written
	var registeredEarly atomic.Bool
	c.gl.mu.Lock()
	c.gl.onReq = func(r glReq) {
		if _, ok := c.find(c3Name); ok {
			registeredEarly.Store(true)
		}
	}
	c.gl.mu.Unlock()
	res, e := c.do("create_project", c.projectArgs(c3Name, nil))
	if e != nil {
		t.Fatalf("create_project: %v", e.Code)
	}
	if registeredEarly.Load() {
		t.Fatal("project registered before the last GitLab call")
	}
	// request order: (2) group (3) exists? (4) project (5) files, README first, (f) default branch (6) protection (7) deploy key
	tr := c.gl.trace()
	want := []string{"GET /groups/team", "GET /projects/team%2Fdemo", "POST /projects"}
	for _, f := range TemplateFiles("basic", c3Name) {
		want = append(want, "POST /projects/21/repository/files/"+strings.ReplaceAll(f.Path, "/", "%2F"))
	}
	want = append(want, "GET /projects/team%2Fdemo", "GET /projects/21/protected_branches/main", "POST /projects/21/protected_branches", "POST /projects/21/deploy_keys")
	if strings.Join(tr, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(tr, "\n"), strings.Join(want, "\n"))
	}
	if !strings.HasSuffix(tr[3], "/files/README.md") {
		t.Fatalf("README.md must be first: %v", tr[3])
	}
	// bodies
	reqs := c.gl.all()
	var pb map[string]any
	json.Unmarshal([]byte(reqs[2].Body), &pb)
	exp := map[string]any{"name": c3Name, "path": c3Name, "namespace_id": float64(7), "visibility": "private", "initialize_with_readme": false, "description": "demo project"}
	if len(pb) != len(exp) {
		t.Fatalf("project body: %v", pb)
	}
	for k, v := range exp {
		if pb[k] != v {
			t.Fatalf("project body %s = %v, want %v", k, pb[k], v)
		}
	}
	var db map[string]any
	json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &db)
	if db["title"] != "orchd demo" || db["key"] != c3Key() || db["can_push"] != true || len(db) != 3 {
		t.Fatalf("deploy key body: %v", db)
	}
	for _, r := range reqs {
		if r.Method == "DELETE" || r.Method == "PUT" || r.Method == "PATCH" {
			t.Fatalf("forbidden method: %v", r)
		}
	}
	// registry entry, last
	p, ok := c.find(c3Name)
	if !ok || p.Kind != "registered" || p.Path != filepath.Join(c.root, c3Name) || p.AIContext || p.AutoMerge.Enabled ||
		p.Provider == nil || *p.Provider != "gl" || p.Remote == nil || p.Remote.Path != "team/demo" || p.Remote.ID == nil || *p.Remote.ID != 21 {
		t.Fatalf("registry entry: %+v", p)
	}
	if len(c.orphans()) != 0 {
		t.Fatalf("orphans after success: %v", c.orphans())
	}
	// response shape
	var out map[string]json.RawMessage
	json.Unmarshal([]byte(jsonOf(t, res)), &out)
	for _, k := range []string{"project", "ssh_url", "clone_url", "cloned_hint"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("response lacks %s: %s", k, jsonOf(t, res))
		}
	}
	if len(out) != 4 || string(out["ssh_url"]) != `"ssh://git@10.0.0.1"` || string(out["clone_url"]) != `"ssh://git@10.0.0.1/team/demo.git"` || string(out["cloned_hint"]) != "false" {
		t.Fatalf("response: %s", jsonOf(t, res))
	}
	var pr map[string]json.RawMessage
	json.Unmarshal(out["project"], &pr)
	for _, k := range []string{"id", "path", "kind", "ai_context", "provider", "remote", "auto_merge"} {
		if _, ok := pr[k]; !ok {
			t.Fatalf("project lacks %s: %s", k, out["project"])
		}
	}
	// the same request again is refused as reused and sends nothing
	n := c.gl.hits()
	c.wantCode("create_project", c.projectArgs(c3Name, nil), "project_name_taken")
	if c.gl.hits() != n {
		t.Fatal("GitLab was called for a name already registered")
	}
}

func TestCreateProjectPublishReadMeansNoPush(t *testing.T) {
	c := newC3(t)
	if _, e := c.do("create_project", c.projectArgs(c3Name, func(f map[string]any) { f["publish"] = "read"; f["ai_context"] = true })); e != nil {
		t.Fatal(e.Code)
	}
	reqs := c.gl.all()
	var db map[string]any
	json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &db)
	if db["can_push"] != false {
		t.Fatalf("read publish must not push: %v", db)
	}
	if p, _ := c.find(c3Name); !p.AIContext {
		t.Fatal("ai_context not stored")
	}
}

func TestCreateProjectNamespaceFromVerifiedGroup(t *testing.T) {
	// (b) namespace_id is the id GitLab gave for the signed group path, nothing else
	c := newC3(t)
	c.gl.mu.Lock()
	c.gl.groups["team"] = 4242
	c.gl.mu.Unlock()
	if _, e := c.do("create_project", c.projectArgs(c3Name, nil)); e != nil {
		t.Fatal(e.Code)
	}
	var pb map[string]any
	json.Unmarshal([]byte(c.gl.all()[2].Body), &pb)
	if pb["namespace_id"] != float64(4242) {
		t.Fatalf("namespace_id %v", pb["namespace_id"])
	}
	// request data cannot carry one (strict keys, no ID burnt)
	before := c.snap()
	for _, k := range []string{"namespace_id", "group_id", "project_id", "path", "name", "visibility"} {
		c.wantCode("create_project", copyArgs(c.projectArgs("other", nil), func(m map[string]any) { m[k] = 1 }), "bad_request")
	}
	c.mustEqual(before, "extra keys")
}

func TestCreateProjectBindsDeployKey(t *testing.T) {
	c := newC3(t)
	// signed hash is of goodKey; send another well-formed key
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherKeyOtherKeyOtherKeyOtherKeyOtherKey orchd"
	a := copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { m["deploy_key"] = other })
	c.wantCode("create_project", a, "deploy_key_mismatch")
	c.noGLHits("deploy key mismatch")
	if _, ok := c.find(c3Name); ok || len(c.orphans()) != 0 {
		t.Fatal("state changed")
	}
	// trailing newline is a different line: also a mismatch
	b := copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { m["deploy_key"] = c3Key() + "\n" })
	c.wantCode("create_project", b, "bad_request")
	c.noGLHits("key with newline")
}

func TestCreateProjectNameTaken(t *testing.T) {
	// on GitLab already: refused, nothing taken over, no write
	c := newC3(t)
	c.gl.existP["team/demo"] = true
	c.wantCode("create_project", c.projectArgs(c3Name, nil), "project_name_taken")
	if len(c.gl.writes()) != 0 {
		t.Fatalf("write requests after name_taken: %v", c.gl.writes())
	}
	if _, ok := c.find(c3Name); ok || len(c.orphans()) != 0 {
		t.Fatal("took over or recorded an existing project")
	}
	// registry id conflict (no GitLab request at all)
	c2 := newC3(t)
	c2.ownProject("gl", "demo")
	c2.wantCode("create_project", c2.projectArgs(c3Name, nil), "project_name_taken")
	c2.noGLHits("registry id conflict")
	// registry PATH conflict under another id
	c3 := newC3(t)
	prov := "gl"
	if err := c3.s.Registry.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: "legacy-x", Path: filepath.Join(c3.root, c3Name), Kind: "default", Provider: &prov})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c3.wantCode("create_project", c3.projectArgs(c3Name, nil), "project_name_taken")
	c3.noGLHits("registry path conflict")
}

func TestCreateProjectGroupAndProviderErrors(t *testing.T) {
	c := newC3(t)
	c.wantCode("create_project", c.projectArgs(c3Name, func(f map[string]any) { f["group_path"] = "nowhere" }), "group_unknown")
	if len(c.gl.writes()) != 0 {
		t.Fatal("write after group_unknown")
	}
	c.wantCode("create_project", c.projectArgs("other", func(f map[string]any) { f["provider"] = "ghost" }), "provider_unknown")
	// token gone: no request is sent
	c2 := newC3(t)
	if err := c2.s.Tokens.Delete("gl-admin"); err != nil {
		t.Fatal(err)
	}
	c2.wantCode("create_project", c2.projectArgs(c3Name, nil), "token_ref_unknown")
	c2.noGLHits("token missing")
	// read failures before any write are plain errors, not partials
	c3 := newC3(t)
	c3.gl.fail["GET /api/v4/projects/team%2Fdemo"] = 500
	c3.wantCode("create_project", c3.projectArgs(c3Name, nil), "provider_unreachable")
	if len(c3.gl.writes()) != 0 || len(c3.orphans()) != 0 {
		t.Fatal("orphan or write on a pre-write failure")
	}
}

func TestPartialFailureRecordsOrphan(t *testing.T) {
	type tc struct {
		name   string
		step   string
		inject func(f *c3GL)
		idKnow bool
		after  string // a request that must NOT appear (later step)
	}
	cases := []tc{
		{"project", "project", func(f *c3GL) { f.fail["POST /api/v4/projects"] = 500 }, false, "/repository/files/"},
		{"project4xx", "project", func(f *c3GL) { f.fail["POST /api/v4/projects"] = 400 }, false, "/repository/files/"},
		{"first_commit", "first_commit", func(f *c3GL) { f.fail["POST /api/v4/projects/21/repository/files/.aigate.yml"] = 500 }, true, "/protected_branches"},
		{"first_commit_readme", "first_commit", func(f *c3GL) { f.fail["POST /api/v4/projects/21/repository/files/README.md"] = 403 }, true, "/protected_branches"},
		{"default_branch", "default_branch", func(f *c3GL) { f.defBr = "master" }, true, "/protected_branches"},
		{"protect", "protect", func(f *c3GL) { f.fail["POST /api/v4/projects/21/protected_branches"] = 500 }, true, "/deploy_keys"},
		{"protect_weaker", "protect", func(f *c3GL) { f.protect = "weak" }, true, "/deploy_keys"},
		{"deploy_key", "deploy_key", func(f *c3GL) { f.fail["POST /api/v4/projects/21/deploy_keys"] = 422 }, true, ""},
	}
	for _, x := range cases {
		x := x
		t.Run(x.name, func(t *testing.T) {
			c := newC3(t)
			x.inject(c.gl)
			_, e := c.do("create_project", c.projectArgs(c3Name, nil))
			d := partialOf(t, e)
			if d.Step != x.step || d.Provider != "gl" || d.Project != "team/demo" || !d.OrphanRecorded {
				t.Fatalf("detail: %+v", d)
			}
			if x.idKnow != (d.ProjectID != nil && *d.ProjectID == 21) {
				t.Fatalf("project_id: %v", d.ProjectID)
			}
			os := c.orphans()
			if len(os) != 1 || os[0].Step != x.step || os[0].Provider != "gl" || os[0].Project != "team/demo" || (os[0].ProjectID != nil) != x.idKnow {
				t.Fatalf("orphans: %+v", os)
			}
			if _, ok := c.find(c3Name); ok {
				t.Fatal("registered despite the failure")
			}
			for _, r := range c.gl.all() {
				if r.Method == "DELETE" || r.Method == "PUT" || r.Method == "PATCH" {
					t.Fatalf("destructive call %v", r)
				}
				if x.after != "" && strings.Contains(r.URI, x.after) && r.Method == "POST" {
					t.Fatalf("a later step ran after the failure: %v", r)
				}
			}
			// never retried: exactly one POST /projects
			n := 0
			for _, r := range c.gl.all() {
				if r.Method == "POST" && strings.HasSuffix(r.URI, "/api/v4/projects") {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("POST /projects sent %d times", n)
			}
			// the name stays taken until the user cleans GitLab: the retry is refused
			if x.idKnow { // GitLab has the project now: the name is taken until the user cleans up
				c.gl.mu.Lock()
				c.gl.fail = map[string]int{}
				c.gl.mu.Unlock()
				c.wantCode("create_project", c.projectArgs(c3Name, nil), "project_name_taken")
			}
			// the wire reply carries the detail and only fixed text
			c2 := newC3(t)
			x.inject(c2.gl)
			r := c2.wire("create_project", c2.projectArgs(c3Name, nil))
			if r.Code != "create_partial" || r.Message != fixedMessages["create_partial"] {
				t.Fatalf("wire: %+v", r)
			}
		})
	}
}

func TestPartialDetailOnWire(t *testing.T) {
	c := newC3(t)
	c.gl.fail["POST /api/v4/projects/21/deploy_keys"] = 500
	out := roundTrip(t, c.sock, []byte(line(t, rpc("create_project", c.projectArgs(c3Name, nil)))+"\n"))
	var r struct {
		OK     bool            `json:"ok"`
		Code   string          `json:"code"`
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Code != "create_partial" {
		t.Fatalf("%s", out)
	}
	if string(r.Detail) != `{"provider":"gl","project":"team/demo","project_id":21,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false,"origin":"created"}` {
		t.Fatalf("detail: %s", r.Detail)
	}
	if strings.Contains(string(out), c3Canary) || strings.Contains(string(out), "REFLECT") {
		t.Fatal("leak")
	}
}

func TestUncertainAfterWriteAlwaysOrphan(t *testing.T) {
	// (a) even a refusal of the very first write (POST /projects) is "uncertain":
	// the orphan is recorded from step 4 on and nothing is retried or deleted
	c := newC3(t)
	c.gl.fail["POST /api/v4/projects"] = 409
	_, e := c.do("create_project", c.projectArgs(c3Name, nil))
	d := partialOf(t, e)
	if d.Step != "project" || d.ProjectID != nil || len(c.orphans()) != 1 {
		t.Fatalf("%+v %v", d, c.orphans())
	}
	if n := len(c.gl.writes()); n != 1 {
		t.Fatalf("writes: %d", n)
	}
}

func TestRegistryWriteFailureAfterGitLabIsOrphan(t *testing.T) {
	c := newC3(t)
	n := 0
	c.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			n++
			if n == 1 { // the project entry; the orphan record's write passes
				return errors.New("injected")
			}
		}
		return nil
	}
	_, e := c.do("create_project", c.projectArgs(c3Name, nil))
	d := partialOf(t, e)
	if d.Step != "register" || d.ProjectID == nil || *d.ProjectID != 21 || !d.OrphanRecorded {
		t.Fatalf("detail: %+v", d)
	}
	if _, ok := c.find(c3Name); ok {
		t.Fatal("registered although the write failed")
	}
	os := c.orphans()
	if len(os) != 1 || os[0].Step != "register" {
		t.Fatalf("orphans: %+v", os)
	}
	// every GitLab step had run
	if !strings.Contains(strings.Join(c.gl.trace(), " "), "POST /projects/21/deploy_keys") {
		t.Fatal("deploy key step missing")
	}
	// the orphan write failing too: still create_partial, flagged
	c2 := newC3(t)
	c2.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			return errors.New("injected")
		}
		return nil
	}
	_, e = c2.do("create_project", c2.projectArgs(c3Name, nil))
	d = partialOf(t, e)
	if d.OrphanRecorded {
		t.Fatal("orphan reported as recorded although both writes failed")
	}
}

func TestConcurrentCreateSameNameOne(t *testing.T) {
	c := newC3(t)
	c.gl.delay["POST /api/v4/projects"] = 200 * time.Millisecond
	const n = 8
	args := make([]map[string]any, n)
	for i := range args {
		args[i] = c.projectArgs(c3Name, nil) // distinct request IDs
	}
	var wg sync.WaitGroup
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := c.do("create_project", args[i])
			codes[i] = codeOf(e)
		}(i)
	}
	wg.Wait()
	ok, taken := 0, 0
	for _, cd := range codes {
		switch cd {
		case "":
			ok++
		case "project_name_taken":
			taken++
		default:
			t.Fatalf("codes: %v", codes)
		}
	}
	if ok != 1 || taken != n-1 {
		t.Fatalf("codes: %v", codes)
	}
	posts := 0
	for _, r := range c.gl.all() {
		if r.Method == "POST" && strings.HasSuffix(r.URI, "/api/v4/projects") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("POST /projects sent %d times", posts)
	}
	if len(c.orphans()) != 0 {
		t.Fatalf("orphans: %v", c.orphans())
	}
}

func TestCreateProjectNeverTouchesFilesystem(t *testing.T) {
	c := newC3(t)
	if _, e := c.do("create_project", c.projectArgs(c3Name, nil)); e != nil {
		t.Fatal(e.Code)
	}
	if got := repoRootEntries(t, c.root); len(got) != 0 {
		t.Fatalf("repo root changed: %v", got)
	}
	// static: the new broker files import no os / filepath / exec / ioutil
	for _, f := range []string{"create_project.go", "providers_ops.go", "templates.go"} {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			switch im.Path.Value {
			case `"os"`, `"io/ioutil"`, `"path/filepath"`, `"os/exec"`, `"syscall"`:
				t.Fatalf("%s imports %s", f, im.Path.Value)
			}
		}
	}
	// the project path comes from the configured root, never from the request
	if p, _ := c.find(c3Name); p.Path != filepath.Join(c.root, c3Name) {
		t.Fatalf("path: %s", p.Path)
	}
	if _, err := os.Stat(filepath.Join(c.root, c3Name)); !os.IsNotExist(err) {
		t.Fatal("broker created the project folder")
	}
}

func TestCanaryTokenNeverInAnyOutput(t *testing.T) {
	var leaks []string
	check := func(what, s string) {
		if strings.Contains(s, c3Canary) || strings.Contains(s, "REFLECT") {
			leaks = append(leaks, what)
		}
	}
	scenarios := map[string]func(f *c3GL){
		"ok":        func(f *c3GL) {},
		"project":   func(f *c3GL) { f.fail["POST /api/v4/projects"] = 500 },
		"files":     func(f *c3GL) { f.fail["POST /api/v4/projects/21/repository/files/README.md"] = 403 },
		"protect":   func(f *c3GL) { f.protect = "weak" },
		"deploykey": func(f *c3GL) { f.fail["POST /api/v4/projects/21/deploy_keys"] = 401 },
		"group":     func(f *c3GL) { f.fail["GET /api/v4/groups/team"] = 403 },
	}
	for name, inject := range scenarios {
		c := newC3(t)
		inject(c.gl)
		r := c.wire("create_project", c.projectArgs(c3Name, nil))
		check(name+" reply", jsonOf(t, r))
		check(name+" log", c.logs.String())
		check(name+" registry", string(c.regFile()))
		check(name+" used", string(c.usedFile()))
		// the token only ever travels in the PRIVATE-TOKEN header: bodies of
		// everything GitLab received are clean
		for _, rq := range c.gl.all() {
			check(name+" request body/uri", rq.Body+rq.URI)
		}
		rg := c.wire("list_groups", map[string]any{"provider": "gl"})
		check(name+" list_groups", jsonOf(t, rg)+c.logs.String())
		rp := c.wire("providers", nil)
		check(name+" providers", jsonOf(t, rp))
	}
	// panic path on the REAL code path: the registry write of step register panics
	// with the token as its value; the reply is create_partial and nothing leaks
	c := newC3(t)
	n := 0
	c.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			if n++; n == 1 {
				panic(c3Canary)
			}
		}
		return nil
	}
	r := c.wire("create_project", c.projectArgs(c3Name, nil))
	if r.Code != "create_partial" {
		t.Fatalf("panic reply: %+v", r)
	}
	if o := c.orphans(); len(o) != 1 || o[0].Step != "register" {
		t.Fatalf("no orphan after the panic: %+v", o)
	}
	check("panic reply", jsonOf(t, r))
	check("panic log", c.logs.String())
	check("panic registry", string(c.regFile()))
	if len(leaks) != 0 {
		t.Fatalf("canary leaked: %v", leaks)
	}
}

func TestTimeoutIsUnknownOutcome(t *testing.T) {
	c := newC3Mod(t, func(s *Server) { s.OpTimeouts = map[string]time.Duration{"create_project": 300 * time.Millisecond} })
	c.gl.delay["POST /api/v4/projects"] = 700 * time.Millisecond
	r := c.wire("create_project", c.projectArgs(c3Name, nil))
	if r.Code != "timeout" {
		t.Fatalf("want timeout, got %+v", r)
	}
	// the handler outlives the reply: the write whose answer was lost is recorded
	// as an orphan (uncertain outcome), and nothing is registered or deleted
	deadline := time.Now().Add(4 * time.Second)
	for len(c.orphans()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	os := c.orphans()
	if len(os) != 1 || os[0].Step != "project" {
		t.Fatalf("orphans after timeout: %+v", os)
	}
	if _, ok := c.find(c3Name); ok {
		t.Fatal("registered after a timeout")
	}
	for _, rq := range c.gl.all() {
		if rq.Method == "DELETE" {
			t.Fatal("delete")
		}
	}
	// the documented defaults
	if defaultOpTimeouts["create_project"] != 60*time.Second {
		t.Fatalf("create_project timeout %v", defaultOpTimeouts["create_project"])
	}
	for _, op := range []string{"providers", "list_groups", "set_provider", "delete_provider", "create_group"} {
		if d := defaultOpTimeouts[op]; d < 5*time.Second || d > 20*time.Second {
			t.Fatalf("%s timeout %v", op, d)
		}
	}
}

func TestCreateProjectRateLimitedBrokerWide(t *testing.T) {
	c := newC3(t)
	for i := 0; i < 10; i++ {
		name := "proj-" + string(rune('a'+i))
		c.gl.mu.Lock()
		c.gl.created = ""
		c.gl.mu.Unlock()
		if _, e := c.do("create_project", c.projectArgs(name, nil)); e != nil {
			t.Fatalf("%s: %v", name, e.Code)
		}
	}
	c.wantCode("create_project", c.projectArgs("proj-z", nil), "rate_limited")
}
