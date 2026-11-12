package broker

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"aigate/internal/registrymsg"
)

// W3e (E3): create_project, create_group and list_groups for a GitHub provider.

func TestCreateProjectGitHubHappyPath(t *testing.T) {
	e := newE3(t)
	// the registry is untouched until the very last step
	registeredEarly := false
	e.gh.mu.Lock()
	e.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
		if _, ok := e.find(c3Name); ok {
			registeredEarly = true
		}
		return false
	}
	e.gh.mu.Unlock()
	res, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil))
	if oe != nil {
		t.Fatalf("create_project: %v", oe.Code)
	}
	want := []string{
		"GET /user", "GET /user/orgs", // owner lookup
		"GET /repos/alice/demo", // must be absent
		"POST /user/repos",      // create
		"PUT /repos/alice/demo/contents/README.md",
		"GET /repos/alice/demo", // default branch after the first file
		"PUT /repos/alice/demo/contents/.gitignore",
		"PUT /repos/alice/demo/contents/.aigate.yml",
		"GET /repos/alice/demo/branches/main/protection",
		"PUT /repos/alice/demo/branches/main/protection",
		"POST /repos/alice/demo/keys",
	}
	if got := e.trace(); !reflect.DeepEqual(got, want) {
		t.Fatalf("request order:\n got %v\nwant %v", got, want)
	}
	if registeredEarly {
		t.Fatal("the registry already had the project while GitHub was still being called")
	}
	e.noDestructive()
	// bodies
	b := e.bodyAt(e.indexOf("POST /user/repos"))
	if b["name"] != "demo" || b["private"] != true || b["auto_init"] != false || b["description"] != "demo project" || len(b) != 4 {
		t.Fatalf("create body: %v", b)
	}
	if first := e.bodyAt(e.indexOf("PUT /repos/alice/demo/contents/README.md")); first["branch"] != nil {
		t.Fatalf("the first file must not name a branch: %v", first)
	}
	if next := e.bodyAt(e.indexOf("PUT /repos/alice/demo/contents/.gitignore")); next["branch"] != "main" {
		t.Fatalf("later files go to main: %v", next)
	}
	k := e.bodyAt(e.indexOf("POST /repos/alice/demo/keys"))
	if k["title"] != "orchd demo" || k["key"] != c3Key() || k["read_only"] != false {
		t.Fatalf("key body: %v", k)
	}
	// the registry entry
	p, ok := e.find(c3Name)
	if !ok || p.Provider == nil || *p.Provider != "gh" || p.Kind != "registered" || p.AIContext || p.Path != e.root+"/"+c3Name ||
		p.Remote == nil || p.Remote.Path != "alice/demo" || p.Remote.ID == nil || *p.Remote.ID != 31 {
		t.Fatalf("entry: %+v", p)
	}
	m := res.(map[string]any)
	if m["clone_url"] != "ssh://git@github.com/alice/demo.git" || m["ssh_url"] != "ssh://git@github.com" || m["cloned_hint"] != false {
		t.Fatalf("result: %v", m)
	}
	if len(e.orphans()) != 0 {
		t.Fatal("orphan recorded on success")
	}
	e.noGLHits("github create")
	// a public repository carries private:false and read-only keys follow `publish`
	e2 := newE3(t)
	if _, oe := e2.do("create_project", e2.ghCreate("pub-demo", "alice", "public", func(f map[string]any) { f["publish"] = "read" })); oe != nil {
		t.Fatalf("public create: %v", oe.Code)
	}
	if b := e2.bodyAt(e2.indexOf("POST /user/repos")); b["private"] != false {
		t.Fatalf("public body: %v", b)
	}
	if k := e2.bodyAt(e2.indexOf("POST /repos/alice/pub-demo/keys")); k["read_only"] != true {
		t.Fatalf("read key: %v", k)
	}
}

// The owner is looked up and the PROVIDER's spelling used; the signed string is
// never trusted as a target.
func TestCreateProjectGitHubOwnerLookupNotTrustingTheString(t *testing.T) {
	e := newE3(t)
	if _, oe := e.do("create_project", e.ghCreate(c3Name, "acme-org", "private", nil)); oe != nil {
		t.Fatalf("org create: %v", oe.Code)
	}
	tr := strings.Join(e.trace(), "\n")
	if !strings.Contains(tr, "POST /orgs/Acme-Org/repos") || strings.Contains(tr, "acme-org") {
		t.Fatalf("the organization must be addressed by GitHub's spelling:\n%s", tr)
	}
	if p, _ := e.find(c3Name); p.Remote == nil || p.Remote.Path != "Acme-Org/demo" {
		t.Fatalf("remote: %+v", p.Remote)
	}
	// a name that is not one of the token's owners: group_unknown, no write
	e2 := newE3(t)
	e2.wantCode("create_project", e2.ghCreate(c3Name, "bob", "private", nil), "group_unknown")
	if w := e2.ghWrites(); len(w) != 0 {
		t.Fatalf("writes for an unknown owner: %v", w)
	}
	if len(e2.orphans()) != 0 {
		t.Fatal("orphan for an unknown owner")
	}
}

func TestCreateProjectGitHubNameTakenIsCaseInsensitive(t *testing.T) {
	e := newE3(t)
	e.gh.repos["alice/demo"] = "Alice/Demo"
	e.wantCode("create_project", e.ghCreate(c3Name, "alice", "private", nil), "project_name_taken")
	if w := e.ghWrites(); len(w) != 0 {
		t.Fatalf("writes: %v", w)
	}
	// a provider failure of the existence check (not a 404) is not "absent"
	e2 := newE3(t)
	e2.gh.fail["GET /repos/alice/demo"] = 500
	e2.wantCode("create_project", e2.ghCreate(c3Name, "alice", "private", nil), "provider_unreachable")
	if w := e2.ghWrites(); len(w) != 0 {
		t.Fatalf("writes after a failed check: %v", w)
	}
}

func TestCreateProjectGitHubPartialFailures(t *testing.T) {
	const repo = "/repos/alice/demo"
	cases := []struct {
		name    string
		inject  func(e *e3env)
		step    string
		hasID   bool
		wantLen int // GitHub write requests that happened
	}{
		{"project", func(e *e3env) { e.gh.fail["POST /user/repos"] = 500 }, "project", false, 1},
		{"first file", func(e *e3env) { e.gh.fail["PUT "+repo+"/contents/README.md"] = 500 }, "first_commit", true, 2},
		{"later file", func(e *e3env) { e.gh.fail["PUT "+repo+"/contents/.gitignore"] = 422 }, "first_commit", true, 3},
		{"default branch", func(e *e3env) { e.gh.defBranch = "master" }, "default_branch", true, 2},
		{"protect weaker", func(e *e3env) { e.gh.protection = ghWeakProtection }, "protect", true, 4},
		{"protect put", func(e *e3env) { e.gh.fail["PUT "+repo+"/branches/main/protection"] = 500 }, "protect", true, 5},
		{"deploy key", func(e *e3env) { e.gh.fail["POST "+repo+"/keys"] = 500 }, "deploy_key", true, 6},
		{"deploy key refused", func(e *e3env) { e.gh.fail["POST "+repo+"/keys"] = 422 }, "deploy_key", true, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newE3(t)
			tc.inject(e)
			_, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil))
			d := partialOf(t, oe)
			if d.Step != tc.step || (d.ProjectID != nil) != tc.hasID || !d.OrphanRecorded || d.Project != "alice/demo" || d.Provider != "gh" {
				t.Fatalf("detail: %+v", d)
			}
			os := e.orphans()
			if len(os) != 1 || os[0].Step != tc.step || os[0].Project != "alice/demo" || os[0].Provider != "gh" {
				t.Fatalf("orphans: %+v", os)
			}
			if tc.hasID && (os[0].ProjectID == nil || *os[0].ProjectID != 31) {
				t.Fatalf("orphan id: %+v", os[0])
			}
			if _, ok := e.find(c3Name); ok {
				t.Fatal("registered despite a failed step")
			}
			if n := len(e.ghWrites()); n != tc.wantLen {
				t.Fatalf("writes = %d, want %d: %v", n, tc.wantLen, e.trace())
			}
			e.noDestructive() // nothing deleted, nothing patched, nothing retried
			// the name stays taken until the user cleans GitHub by hand
			if tc.hasID {
				e.wantCode("create_project", e.ghCreate(c3Name, "alice", "private", nil), "project_name_taken")
			}
		})
	}
}

// Private repositories on a free plan cannot be protected: the repository exists
// by then, so this is a partial result, never a plain refusal.
func TestPrivateProtectionNeedsPaidPlanBecomesPartial(t *testing.T) {
	e := newE3(t)
	e.gh.fail["PUT /repos/alice/demo/branches/main/protection"] = 403
	_, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil))
	d := partialOf(t, oe)
	if d.Step != "protect" || !d.OrphanRecorded || d.ProjectID == nil {
		t.Fatalf("detail: %+v", d)
	}
	if n := len(e.ghWrites()); n != 5 { // create, 3 files, the refused protection: no key
		t.Fatalf("writes: %v", e.trace())
	}
	if e.indexOf("POST /repos/alice/demo/keys") >= 0 {
		t.Fatal("a deploy key was added to an unprotected repository")
	}
	if len(e.orphans()) != 1 {
		t.Fatalf("orphans: %+v", e.orphans())
	}
}

// A create-repository call that GitHub refuses made nothing: plain code, no orphan.
func TestCreateProjectGitHubRefusedCreateIsNotAnOrphan(t *testing.T) {
	for _, tc := range []struct {
		status int
		hdr    map[string]string
		code   string
	}{
		{400, nil, "github_refused"}, {401, nil, "github_refused"}, {403, nil, "github_refused"}, {404, nil, "github_refused"},
		{422, nil, "github_refused"}, {403, map[string]string{"Retry-After": "60"}, "provider_rate_limited"},
		{429, nil, "provider_rate_limited"},
	} {
		e := newE3(t)
		e.gh.fail["POST /user/repos"] = tc.status
		e.gh.failHeader = tc.hdr
		a := e.ghCreate(c3Name, "alice", "private", nil)
		r := e.wire("create_project", a)
		if r.Code != tc.code || r.Message != fixedMessages[tc.code] || strings.Contains(r.Message, "REFLECT") {
			t.Fatalf("%d: %+v", tc.status, r)
		}
		if len(e.orphans()) != 0 {
			t.Fatalf("%d: orphan recorded for a refused create call: %+v", tc.status, e.orphans())
		}
		if _, ok := e.find(c3Name); ok {
			t.Fatalf("%d: registered", tc.status)
		}
		if n := len(e.ghWrites()); n != 1 {
			t.Fatalf("%d: writes %v", tc.status, e.trace())
		}
		// the signature was valid: the ID is burnt, a retry needs a new signature
		e.wantCode("create_project", a, "request_reused")
	}
	// any other failure of that call IS uncertain
	for _, st := range []int{500, 502, 302} {
		e := newE3(t)
		e.gh.fail["POST /user/repos"] = st
		_, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil))
		if d := partialOf(t, oe); d.Step != "project" || !d.OrphanRecorded {
			t.Fatalf("%d: %+v", st, d)
		}
	}
	// a 2xx answer that is not what we asked for is uncertain, too
	e := newE3(t)
	e.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" && r.URL.Path == "/user/repos" {
			w.WriteHeader(201)
			w.Write([]byte(`{"id":31,"full_name":"alice/demo","private":false}`)) // asked for private
			return true
		}
		return false
	}
	_, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil))
	if d := partialOf(t, oe); d.Step != "project" {
		t.Fatalf("%+v", d)
	}
}

func TestCreateProjectGitHubRegistryWriteFailureIsOrphan(t *testing.T) {
	e := newE3(t)
	n := 0
	e.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			if n++; n == 1 {
				return errors.New("injected")
			}
		}
		return nil
	}
	_, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil))
	d := partialOf(t, oe)
	if d.Step != "register" || d.ProjectID == nil || *d.ProjectID != 31 || !d.OrphanRecorded {
		t.Fatalf("detail: %+v", d)
	}
	if _, ok := e.find(c3Name); ok {
		t.Fatal("registered although the write failed")
	}
	if os := e.orphans(); len(os) != 1 || os[0].Step != "register" {
		t.Fatalf("orphans: %+v", os)
	}
	// a write that only failed on the directory fsync keeps the state: registered, no orphan
	e2 := newE3(t)
	m := 0
	e2.s.Registry.opts.failAt = func(stage string) error {
		if stage == "dir-synced" {
			if m++; m == 1 {
				return errors.New("injected")
			}
		}
		return nil
	}
	if _, oe := e2.do("create_project", e2.ghCreate(c3Name, "alice", "private", nil)); oe != nil {
		t.Fatalf("dir-fsync-only failure: %v", oe.Code)
	}
	if _, ok := e2.find(c3Name); !ok || len(e2.orphans()) != 0 {
		t.Fatalf("project=%v orphans=%+v", ok, e2.orphans())
	}
	// the provider entry changing while GitHub is being called: partial, not registered
	e4 := newE3(t)
	e4.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys") {
			// the token reference of the provider is swapped in the middle of the flow
			e4.s.Registry.Update(func(reg *Registry) error {
				i := findProvider(reg, "gh")
				reg.Providers[i].TokenRef = "gh-other"
				return nil
			})
		}
		return false
	}
	_, oe = e4.do("create_project", e4.ghCreate(c3Name, "alice", "private", nil))
	if d := partialOf(t, oe); d.Step != "register" {
		t.Fatalf("%+v", d)
	}
	if _, ok := e4.find(c3Name); ok {
		t.Fatal("registered although the provider entry changed")
	}
}

// The quota is spent right before the first write, not by refusals before it.
func TestCreateProjectGitHubQuotaBeforeFirstWriteOnly(t *testing.T) {
	e := newE3(t)
	e.s.State.CreateProject = NewHourlyLimiter(1, func() time.Time { return wNow })
	e.wantCode("create_project", e.ghCreate(c3Name, "bob", "private", nil), "group_unknown")
	e.gh.repos["alice/taken"] = "alice/taken"
	e.wantCode("create_project", e.ghCreate("taken", "alice", "private", nil), "project_name_taken")
	if _, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil)); oe != nil { // consumes the one slot
		t.Fatalf("first real create: %v", oe.Code)
	}
	w := len(e.ghWrites())
	e.wantCode("create_project", e.ghCreate("second", "alice", "private", nil), "rate_limited")
	if len(e.ghWrites()) != w {
		t.Fatalf("a write happened after the limit: %v", e.ghWrites())
	}
}

func TestCreateGroupNotSupportedForGitHub(t *testing.T) {
	e := newE3(t)
	before := e.snap()
	g := e.signedArgs(e.alice, registrymsg.ActionCreateGroup, "gh", map[string]any{"name": "newg", "path": "newg", "parent_path": ""})
	e.wantCode("create_group", g, "not_supported_for_provider")
	e.wantCode("create_group", g, "not_supported_for_provider") // request_reused if an ID had been burnt
	e.mustEqual(before, "create_group on github")
	e.noGHHits("create_group on github")
	e.noGLHits("create_group on github")
}

func TestListOwnersGitHub(t *testing.T) {
	e := newE3(t)
	res, oe := e.do("list_groups", map[string]any{"provider": "gh"})
	if oe != nil {
		t.Fatalf("list_groups: %v", oe.Code)
	}
	m := res.(map[string]any)
	rows := m["groups"].([]map[string]any)
	if len(rows) != 2 || rows[0]["path"] != "alice" || rows[0]["kind"] != "user" || rows[1]["path"] != "Acme-Org" || rows[1]["kind"] != "org" {
		t.Fatalf("rows: %v", rows)
	}
	for _, r := range rows {
		if len(r) != 3 {
			t.Fatalf("row carries more than path, name and kind: %v", r)
		}
	}
	// the login that does not fit our name rules was skipped: the list is incomplete
	if m["truncated"] != true {
		t.Fatalf("truncated = %v after a skipped entry", m["truncated"])
	}
	// the page cap also marks the list as incomplete
	e2 := newE3(t)
	e2.gh.orgPages = 1
	res, oe = e2.do("list_groups", map[string]any{"provider": "gh"})
	if oe != nil || res.(map[string]any)["truncated"] != true {
		t.Fatalf("page cap: %v %v", oe, res)
	}
	// a complete list says so
	e3 := newE3(t)
	e3.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/user/orgs" {
			w.Write([]byte(`[{"login":"Acme-Org"}]`))
			return true
		}
		return false
	}
	res, _ = e3.do("list_groups", map[string]any{"provider": "gh"})
	if res.(map[string]any)["truncated"] != false {
		t.Fatalf("complete list flagged: %v", res)
	}
	// provider errors: fixed github code, no GitHub text
	e4 := newE3(t)
	e4.gh.fail["GET /user"] = 401
	r := e4.wire("list_groups", map[string]any{"provider": "gh"})
	if r.Code != "github_refused" || strings.Contains(r.Message, "REFLECT") {
		t.Fatalf("%+v", r)
	}
	// GitLab rows are unchanged: path and name only
	res, _ = e.do("list_groups", map[string]any{"provider": "gl"})
	for _, r := range res.(map[string]any)["groups"].([]map[string]any) {
		if len(r) != 2 {
			t.Fatalf("gitlab row changed: %v", r)
		}
	}
}

func TestVisibilityMustBeSigned(t *testing.T) {
	e := newE3(t)
	before := e.snap()
	// github needs it; gitlab refuses it; both before the signature is consumed
	missing := e.ghCreate(c3Name, "alice", nil, nil)
	e.wantCode("create_project", missing, "visibility_required")
	e.wantCode("create_project", missing, "visibility_required")
	e.wantCode("create_project", e.projectArgs(c3Name, func(f map[string]any) { f["visibility"] = "private" }), "visibility_not_allowed")
	for _, bad := range []string{"", "Public", "internal", "PRIVATE"} {
		e.wantCode("create_project", copyArgs(e.ghCreate(c3Name, "alice", "private", nil), func(m map[string]any) { m["visibility"] = bad }), "bad_request")
	}
	e.mustEqual(before, "visibility refusals")
	e.noGHHits("visibility refusals")
	// the value sent must be the value signed: a swapped value is a change mismatch,
	// nothing is created and the request ID stays usable
	a := e.ghCreate(c3Name, "alice", "private", nil)
	swapped := copyArgs(a, func(m map[string]any) { m["visibility"] = "public" })
	e.wantCode("create_project", swapped, "change_mismatch")
	e.noGHHits("swapped visibility")
	e.mustEqual(before, "swapped visibility")
	// a request that drops the field while the signature covered it
	dropped := copyArgs(a, func(m map[string]any) { delete(m, "visibility") })
	e.wantCode("create_project", dropped, "visibility_required")
	e.noGHHits("dropped visibility")
	if _, oe := e.do("create_project", a); oe != nil {
		t.Fatalf("the honest request: %v", oe.Code)
	}
	if b := e.bodyAt(e.indexOf("POST /user/repos")); b["private"] != true {
		t.Fatalf("body: %v", b)
	}
}

func TestGitHubOwnerIsOneLoginAtShape(t *testing.T) {
	e := newE3(t)
	before := e.snap()
	for _, owner := range []string{"acme/org", strings.Repeat("a", 40)} { // schema-valid, but not a GitHub login
		e.wantCode("create_project", e.ghCreate(c3Name, owner, "private", nil), "bad_request")
	}
	e.mustEqual(before, "owner shape")
	e.noGHHits("owner shape")
}
