package broker

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/registrymsg"
)

// W3f (F2): signed deletion ops and the delete-capable provider clients. The
// providers are httptest fakes; nothing leaves the machine.

const (
	delOtherGH = "ghp_OTHER-token-must-never-be-used-0123456789abcdef"
	delOtherGL = "glpat-OTHER-token-must-never-be-used-0123456789"
	delKeyB    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKeyBKeyBKeyBKeyBKeyBKeyBKeyBKeyBKeyBKeyB"
	delKeyC    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKeyCKeyCKeyCKeyCKeyCKeyCKeyCKeyCKeyCKeyC"
	delKeyA    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKeyAKeyAKeyAKeyAKeyAKeyAKeyAKeyAKeyAKeyA"
)

// delFake is a scripted GitHub (kind gh: the /api/v3 prefix is stripped) or
// GitLab (kind gl). It records "METHOD uri" and the credential of every request.
type delFake struct {
	*httptest.Server
	kind string
	mu   sync.Mutex
	reqs []string
	cred []string

	repoStatus int    // GET repository (0: 200)
	repoBody   string // GET repository body ("" default)
	delStatus  int    // DELETE repository (0: 204 gh / 202 gl)
	delDelay   time.Duration
	delAbort   bool
	delHold    chan struct{} // when set: the repository DELETE waits for it
	delSeen    chan struct{} // closed-once signal: a repository DELETE arrived
	keyPages   map[int]string
	keyStatus  int // DELETE key (0: 204)
	keyListSt  int // GET keys (0: 200)
	extraHdr   map[string]string
	onKeyDel   func() // W3g: called when a key DELETE arrives (before it is answered)
}

func newDelFake(t *testing.T, kind string) *delFake {
	t.Helper()
	f := &delFake{kind: kind, keyPages: map[int]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *delFake) serve(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	uri := r.RequestURI
	p := r.URL.EscapedPath()
	cred := r.Header.Get("Authorization") + r.Header.Get("PRIVATE-TOKEN")
	if f.kind == "gh" {
		uri = strings.TrimPrefix(uri, "/api/v3")
		p = strings.TrimPrefix(p, "/api/v3")
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+uri)
	f.cred = append(f.cred, cred)
	f.mu.Unlock()
	for k, v := range f.extraHdr {
		w.Header().Set(k, v)
	}
	reflect := `{"message":"` + ghCanary + c3Canary + `-REFLECT"}`
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	kind := "other"
	if f.kind == "gh" && len(segs) >= 3 && segs[0] == "repos" {
		switch {
		case len(segs) == 3:
			kind = "repo"
		case len(segs) == 4 && segs[3] == "keys":
			kind = "keys"
		case len(segs) == 5 && segs[3] == "keys":
			kind = "key"
		}
	}
	if f.kind == "gl" && len(segs) >= 4 && segs[0] == "api" && segs[2] == "projects" {
		switch {
		case len(segs) == 4 && strings.Contains(segs[3], "%2F"):
			kind = "repo"
		case len(segs) == 4:
			kind = "repo"
		case len(segs) == 5 && segs[4] == "deploy_keys":
			kind = "keys"
		case len(segs) == 6 && segs[4] == "deploy_keys":
			kind = "key"
		}
	}
	st := func(v, def int) int {
		if v == 0 {
			return def
		}
		return v
	}
	switch {
	case r.Method == "GET" && kind == "repo":
		if s := st(f.repoStatus, 200); s != 200 {
			w.WriteHeader(s)
			fmt.Fprint(w, reflect)
			return
		}
		fmt.Fprint(w, f.repoBody)
	case r.Method == "DELETE" && kind == "repo":
		if f.delSeen != nil {
			select {
			case f.delSeen <- struct{}{}:
			default:
			}
		}
		if f.delHold != nil {
			<-f.delHold
		}
		if f.delAbort {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		time.Sleep(f.delDelay)
		def := 204
		if f.kind == "gl" {
			def = 202
		}
		s := st(f.delStatus, def)
		w.WriteHeader(s)
		if s >= 400 {
			fmt.Fprint(w, reflect)
		}
	case r.Method == "GET" && kind == "keys":
		if s := st(f.keyListSt, 200); s != 200 {
			w.WriteHeader(s)
			fmt.Fprint(w, reflect)
			return
		}
		page := 1
		if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
			page = v
		}
		if b, ok := f.keyPages[page]; ok {
			fmt.Fprint(w, b)
			return
		}
		fmt.Fprint(w, `[]`)
	case r.Method == "DELETE" && kind == "key":
		if f.onKeyDel != nil {
			f.onKeyDel()
		}
		s := st(f.keyStatus, 204)
		w.WriteHeader(s)
		if s >= 400 {
			fmt.Fprint(w, reflect)
		}
	default:
		w.WriteHeader(404)
	}
}

func (f *delFake) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *delFake) creds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cred...)
}

func (f *delFake) count(method string) int {
	n := 0
	for _, r := range f.all() {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}

func (f *delFake) deletes() int { return f.count("DELETE") }

type delEnv struct {
	*e3env
	gh, gl *delFake
}

func ghRepoBody(id int64, name string) string {
	return fmt.Sprintf(`{"id":%d,"full_name":%q}`, id, name)
}

func glRepoBody(id int64, path string) string {
	return fmt.Sprintf(`{"id":%d,"path_with_namespace":%q}`, id, path)
}

func keysJSON(items ...[3]string) string { // id, title, key
	var parts []string
	for _, it := range items {
		parts = append(parts, fmt.Sprintf(`{"id":%s,"title":%q,"key":%q}`, it[0], it[1], it[2]))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// newDel builds the kit: providers dgh (GitHub) and dgl (GitLab) pointing at the
// fakes, a stored OTHER token of each kind (never the provider's own), projects
// demo-gh (alice/Demo, id 77) and demo-gl (team/demo, id 21), both origin created.
func newDel(t *testing.T) *delEnv {
	t.Helper()
	e := newE3(t)
	d := &delEnv{e3env: e, gh: newDelFake(t, "gh"), gl: newDelFake(t, "gl")}
	d.gh.repoBody = ghRepoBody(77, "alice/Demo")
	d.gl.repoBody = glRepoBody(21, "team/demo")
	if err := e.s.Tokens.Set("gh-other", "github", "", delOtherGH, tokNow); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Tokens.Set("gl-other", "gitlab", "", delOtherGL, tokNow); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddProviderChecked(Provider{ID: "dgh", Kind: "github", BaseURL: d.gh.URL, SSHURL: "ssh://git@10.0.0.2", TokenRef: "gh-tok"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddProviderChecked(Provider{ID: "dgl", Kind: "gitlab", BaseURL: d.gl.URL, SSHURL: "ssh://git@10.0.0.3", TokenRef: "gl-admin"}); err != nil {
		t.Fatal(err)
	}
	d.put("demo-gh", "dgh", "alice/Demo", 77, OriginCreated)
	d.put("demo-gl", "dgl", "team/demo", 21, OriginCreated)
	return d
}

func (d *delEnv) put(id, prov, remote string, rid int64, origin string) {
	d.t.Helper()
	d.putRaw(Project{ID: id, Path: d.root + "/" + id, Kind: "registered", Provider: &prov, Remote: &RemoteLink{Path: remote, ID: &rid}, Origin: origin})
}

func (d *delEnv) putRaw(p Project) {
	d.t.Helper()
	err := d.s.Registry.Update(func(r *Registry) error {
		for i := range r.Projects {
			if r.Projects[i].ID == p.ID {
				r.Projects = append(r.Projects[:i], r.Projects[i+1:]...)
				break
			}
		}
		r.Projects = append(r.Projects, p)
		return nil
	})
	if err != nil {
		d.t.Fatal(err)
	}
}

func (d *delEnv) proj(id string) Project {
	p, ok := d.find(id)
	if !ok {
		d.t.Fatalf("project %s missing", id)
	}
	return p
}

func (d *delEnv) delFields(path, rid, confirm string) map[string]any {
	return map[string]any{"remote_path": path, "remote_id": rid, "confirm_name": confirm, "also_local": false}
}

// delArgs: signed delete_remote_repo args for project id; mod edits the signed fields.
func (d *delEnv) delArgs(id string, mod func(f map[string]any)) map[string]any {
	f := d.delFields("alice/Demo", "77", "Demo")
	if id == "demo-gl" {
		f = d.delFields("team/demo", "21", "demo")
	}
	if mod != nil {
		mod(f)
	}
	return d.signedArgs(d.alice, registrymsg.ActionDeleteRemoteRepo, id, f)
}

func (d *delEnv) keyArgs(id string, key string) map[string]any {
	path := "alice/Demo"
	if id == "demo-gl" {
		path = "team/demo"
	}
	a := d.signedArgs(d.alice, registrymsg.ActionRemoveDeployKey, id, map[string]any{"remote_path": path})
	if key == "" {
		key = delKeyB // the project's key line (REQUIRED by the op)
	}
	a["deploy_key"] = key
	return a
}

func (d *delEnv) rmArgs(id string, ack bool) map[string]any {
	return d.signedArgs(d.alice, registrymsg.ActionRemoveProject, id, map[string]any{"scope": "local", "legacy_ack": ack, "detach_key": false})
}

// rmDetachArgs: remove_project signed with detach_key=true and (when key != "")
// the orchd-supplied key line (unsigned, like remove_deploy_key's).
func (d *delEnv) rmDetachArgs(id string, ack bool, key string) map[string]any {
	a := d.signedArgs(d.alice, registrymsg.ActionRemoveProject, id, map[string]any{"scope": "local", "legacy_ack": ack, "detach_key": true})
	if key != "" {
		a["deploy_key"] = key
	}
	return a
}

func (d *delEnv) hits() int { return len(d.gh.all()) + len(d.gl.all()) }

func (d *delEnv) noHits(what string) {
	d.t.Helper()
	if d.hits() != 0 {
		d.t.Fatalf("%s: the providers saw %v %v", what, d.gh.all(), d.gl.all())
	}
}

// refusedNoBurn: the op refuses with code, nothing reached a provider, and the
// request ID is NOT burnt (the same args refuse the same way again).
func (d *delEnv) refusedNoBurn(op string, a map[string]any, code string) {
	d.t.Helper()
	before := d.snap()
	for i := 0; i < 2; i++ {
		d.wantCode(op, a, code)
	}
	d.noHits(op + " " + code)
	d.mustEqual(before, op+" "+code)
}

// ---------- allowlists ----------

func rowStrings(rows []allowEntry) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.method+" "+r.path.String()+" q="+strings.Join(r.query, ","))
	}
	return out
}

func TestOnlyDeleteRowsAddedToAllowlist(t *testing.T) {
	// the pre-F2 tables, spelled out: they are unchanged
	preGH := []string{
		"GET ^/user$ q=", "GET ^/user/orgs$ q=per_page,page", "GET ^/repos/" + ghOwner + "/" + ghRepo + "$ q=",
		"POST ^/user/repos$ q=", "POST ^/orgs/" + ghOwner + "/repos$ q=",
		"PUT ^/repos/" + ghOwner + "/" + ghRepo + "/contents/" + ghFile + "$ q=",
		"GET ^/repos/" + ghOwner + "/" + ghRepo + "/branches/" + reBr + "/protection$ q=",
		"PUT ^/repos/" + ghOwner + "/" + ghRepo + "/branches/" + reBr + "/protection$ q=",
		"POST ^/repos/" + ghOwner + "/" + ghRepo + "/keys$ q=",
	}
	if got := rowStrings(githubAllowlist); !reflect.DeepEqual(got, preGH) {
		t.Fatalf("pre-F2 GitHub table changed:\n%v", got)
	}
	wantGH := []string{
		"DELETE ^/repos/" + ghOwner + "/" + ghRepo + "$ q=",
		"GET ^/repos/" + ghOwner + "/" + ghRepo + "/keys$ q=per_page,page",
		"DELETE ^/repos/" + ghOwner + "/" + ghRepo + "/keys/" + reID + "$ q=",
	}
	if got := rowStrings(githubDeleteRows); !reflect.DeepEqual(got, wantGH) {
		t.Fatalf("GitHub delete rows:\n%v\nwant\n%v", got, wantGH)
	}
	preGL := []string{
		"GET ^/api/v4/groups$ q=min_access_level,order_by,sort,per_page,page,top_level_only", "GET ^/api/v4/groups/" + reGroup + "$ q=",
		"POST ^/api/v4/groups$ q=", "GET ^/api/v4/projects/" + reProj + "$ q=", "POST ^/api/v4/projects$ q=",
		"POST ^/api/v4/projects/" + reID + "/repository/files/" + reFile + "$ q=",
		"GET ^/api/v4/projects/" + reID + "/protected_branches/" + reBr + "$ q=",
		"POST ^/api/v4/projects/" + reID + "/protected_branches$ q=",
		"POST ^/api/v4/projects/" + reID + "/deploy_keys$ q=",
	}
	if got := rowStrings(allowlist); !reflect.DeepEqual(got, preGL) {
		t.Fatalf("pre-F2 GitLab table changed:\n%v", got)
	}
	wantGL := []string{
		"DELETE ^/api/v4/projects/" + reID + "$ q=",
		"GET ^/api/v4/projects/" + reID + "/deploy_keys$ q=per_page,page",
		"DELETE ^/api/v4/projects/" + reID + "/deploy_keys/" + reID + "$ q=",
	}
	if got := rowStrings(gitlabDeleteRows); !reflect.DeepEqual(got, wantGL) {
		t.Fatalf("GitLab delete rows:\n%v\nwant\n%v", got, wantGL)
	}
	// the DELETE rows are the only DELETE anywhere
	for _, rows := range [][]allowEntry{githubAllowlist, allowlist} {
		for _, r := range rows {
			if r.method == "DELETE" {
				t.Fatalf("DELETE in a pre-F2 table: %v", r)
			}
		}
	}
	// and the effective sets are exactly pre + delete
	for _, tc := range []struct {
		m, p string
		want bool
	}{
		{"DELETE", "/repos/alice/demo", true}, {"DELETE", "/repos/alice/demo/keys/5", true}, {"GET", "/repos/alice/demo/keys", true},
		{"DELETE", "/repos/alice/demo/keys", false}, {"DELETE", "/repos/alice/demo/branches/main/protection", false},
		{"DELETE", "/repos/alice/demo/contents/a.md", false},
	} {
		if githubAllowed(tc.m, tc.p, nil) != tc.want {
			t.Errorf("githubAllowed(%s %s) != %v", tc.m, tc.p, tc.want)
		}
	}
	for _, tc := range []struct {
		m, p string
		want bool
	}{
		{"DELETE", "/api/v4/projects/21", true}, {"DELETE", "/api/v4/projects/21/deploy_keys/9", true}, {"GET", "/api/v4/projects/21/deploy_keys", true},
		{"DELETE", "/api/v4/deploy_keys/9", false}, {"DELETE", "/api/v4/projects/21/deploy_keys", false},
		{"DELETE", "/api/v4/projects/team%2Fdemo", false}, {"DELETE", "/api/v4/groups/team", false},
	} {
		if allowed(tc.m, tc.p, nil) != tc.want {
			t.Errorf("allowed(%s %s) != %v", tc.m, tc.p, tc.want)
		}
	}
}

func TestDeleteVerbOnlyInRows(t *testing.T) {
	// the DELETE verb is spelled in exactly three places: the two row tables
	// (githubapi.go, gitlabapi.go: 2 literals each) and the four senders of
	// delete_clients.go (http.MethodDelete). No other production file has it.
	files, _ := os.ReadDir(".")
	fset := token.NewFileSet()
	for _, de := range files {
		n := de.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		lits, methods := 0, 0
		ast.Inspect(af, func(nd ast.Node) bool {
			if bl, ok := nd.(*ast.BasicLit); ok && bl.Kind == token.STRING && bl.Value == `"DELETE"` {
				lits++
			}
			if se, ok := nd.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok && id.Name == "http" && se.Sel.Name == "MethodDelete" {
					methods++
				}
			}
			return true
		})
		wantLits, wantMethods := 0, 0
		switch n {
		case "githubapi.go", "gitlabapi.go":
			wantLits = 2
		case "delete_clients.go":
			wantMethods = 4
		}
		if lits != wantLits || methods != wantMethods {
			t.Errorf("%s: DELETE literals %d (want %d), http.MethodDelete %d (want %d)", n, lits, wantLits, methods, wantMethods)
		}
	}
	// the sender file names no other destructive target
	b, _ := os.ReadFile("delete_clients.go")
	for _, bad := range []string{"/groups", "/orgs", "/user", "token", "collaborators", "/hooks", "/members", "api/v4/deploy_keys", "permanently_remove", "MethodPut", "MethodPatch"} {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, `"`) && strings.Contains(line, bad) && !strings.Contains(line, "tokens.") {
				t.Errorf("delete_clients.go code line mentions %q: %s", bad, line)
			}
		}
	}
}

func TestNoGroupOrOrgOrTokenDeletion(t *testing.T) {
	d := newDel(t)
	ghp, _ := d.s.repoProviderForEntry(Provider{ID: "dgh", Kind: "github", BaseURL: d.gh.URL, SSHURL: "ssh://git@10.0.0.2", TokenRef: "gh-tok"})
	_ = ghp
	ga, err := newGitHubAPI(Provider{ID: "dgh", Kind: "github", BaseURL: d.gh.URL, SSHURL: "ssh://git@10.0.0.2", TokenRef: "gh-tok"}, d.s.Tokens, d.s.apiSeams)
	if err != nil {
		t.Fatal(err)
	}
	la, err := newGitLabAPI(Provider{ID: "dgl", Kind: "gitlab", BaseURL: d.gl.URL, SSHURL: "ssh://git@10.0.0.3", TokenRef: "gl-admin"}, d.s.Tokens, d.s.apiSeams)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, p := range []string{"/orgs/acme", "/user/keys/1", "/user", "/repos/a/b/collaborators/c", "/repos/a/b/hooks/1", "/repos/a/b/branches/main/protection",
		"/repos/a/b/contents/x.md", "/repos/a/b/keys", "/repos/a/b/keys/abc", "/repos/a/b/keys/1/x", "/repos/a/../c", "/repos/a/b/keys/%31", "/repos/a/b/keys/1234567890123456789",
		"/applications/x/token", "/user/orgs", "/repos/a/b/git/refs/heads/main", "/repos/a/b/releases/1", "/repos/a/b/", "/repos/a/b?x=1", "//repos/a/b"} {
		if _, err := ga.do(ctx, "DELETE", p, nil, nil, 204); ErrCode(err) != CodeNotAllowlisted {
			t.Errorf("github DELETE %s: %v", p, err)
		}
	}
	for _, p := range []string{"/api/v4/groups/team", "/api/v4/groups/5", "/api/v4/projects/team%2Fdemo", "/api/v4/projects/21/members/5", "/api/v4/deploy_keys/9",
		"/api/v4/projects/21/deploy_keys", "/api/v4/projects/21/deploy_keys/9/enable", "/api/v4/projects/21/protected_branches/main", "/api/v4/personal_access_tokens/1",
		"/api/v4/users/1", "/api/v4/projects/21/access_tokens/3", "/api/v4/projects/21/repository/files/a", "/api/v4/projects/21/", "/api/v4/projects/21/deploy_keys/..%2f9",
		"/api/v4/projects/21/deploy_keys/9/", "/api/v4/projects/0x15", "/api/v4/projects/21/hooks/1", "/api/v4/user/keys/1"} {
		if _, err := la.do(ctx, "DELETE", p, nil, nil, 204); ErrCode(err) != CodeNotAllowlisted {
			t.Errorf("gitlab DELETE %s: %v", p, err)
		}
	}
	// a query (permanently_remove, full_path) is never allowed on a delete
	for _, q := range []string{"permanently_remove=true", "full_path=team%2Fdemo", "x=1"} {
		vals, _ := parseQ(q)
		if _, err := la.do(ctx, "DELETE", "/api/v4/projects/21", vals, nil, 202); ErrCode(err) != CodeNotAllowlisted {
			t.Errorf("gitlab DELETE with %s: %v", q, err)
		}
		if _, err := ga.do(ctx, "DELETE", "/repos/a/b", vals, nil, 204); ErrCode(err) != CodeNotAllowlisted {
			t.Errorf("github DELETE with %s: %v", q, err)
		}
	}
	d.noHits("probes")
}

// ---------- remote delete: who may be deleted ----------

func TestRemoteDeleteRefusedForLinkedUnknownLegacy(t *testing.T) {
	for _, tc := range []struct{ origin, kind, code string }{
		{OriginLinked, "registered", "not_created_by_tool"},
		{OriginUnknown, "registered", "origin_unknown"},
		{OriginLegacy, "legacy", "origin_unknown"},
	} {
		d := newDel(t)
		prov, rid := "dgh", int64(77)
		d.putRaw(Project{ID: "demo-gh", Path: d.root + "/demo-gh", Kind: tc.kind, Provider: &prov, Remote: &RemoteLink{Path: "alice/Demo", ID: &rid}, Origin: tc.origin})
		d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", nil), tc.code)
	}
	d := newDel(t)
	d.refusedNoBurn("delete_remote_repo", d.delArgs("nope", nil), "project_unknown")
}

func TestDeleteChecksRemoteIdAndName(t *testing.T) {
	// shape-decidable: the registry and the request disagree (no ID burnt)
	for name, mod := range map[string]func(f map[string]any){
		"signed id differs from registry": func(f map[string]any) { f["remote_id"] = "78" },
		"signed path is another repo":     func(f map[string]any) { f["remote_path"] = "alice/Other"; f["confirm_name"] = "Other" },
		"signed path another owner":       func(f map[string]any) { f["remote_path"] = "bob/Demo" },
	} {
		d := newDel(t)
		d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", mod), "remote_changed")
		_ = name
	}
	// registry id missing
	d := newDel(t)
	prov := "dgh"
	d.putRaw(Project{ID: "demo-gh", Path: d.root + "/demo-gh", Kind: "registered", Provider: &prov, Remote: &RemoteLink{Path: "alice/Demo"}, Origin: OriginCreated})
	d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", nil), "remote_changed")
	// remote already deleted
	d = newDel(t)
	d.putRaw(Project{ID: "demo-gh", Path: d.root + "/demo-gh", Kind: "registered", Provider: &prov, Origin: OriginCreated, RemoteDeletedAt: strp("2026-11-10T01:02:03Z")})
	d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", nil), "remote_changed")

	// live checks (the ID is burnt, nothing is deleted)
	for name, setup := range map[string]func(d *delEnv){
		"live id differs":    func(d *delEnv) { d.gh.repoBody = ghRepoBody(78, "alice/Demo") },
		"live name differs":  func(d *delEnv) { d.gh.repoBody = ghRepoBody(77, "alice/Renamed") },
		"live owner differs": func(d *delEnv) { d.gh.repoBody = ghRepoBody(77, "bob/Demo") },
		"live 404":           func(d *delEnv) { d.gh.repoStatus = 404 },
		"live 301 (moved)":   func(d *delEnv) { d.gh.repoStatus = 301 },
		"live 307 (moved)":   func(d *delEnv) { d.gh.repoStatus = 307 },
		"live id missing":    func(d *delEnv) { d.gh.repoBody = `{"full_name":"alice/Demo"}` },
		"live id zero":       func(d *delEnv) { d.gh.repoBody = ghRepoBody(0, "alice/Demo") },
	} {
		d := newDel(t)
		setup(d)
		a := d.delArgs("demo-gh", nil)
		_, oe := d.do("delete_remote_repo", a)
		if oe == nil || (oe.Code != "remote_changed" && !(name == "live id missing" || name == "live id zero")) {
			t.Fatalf("%s: %v", name, oe)
		}
		if oe.Code == "" || d.gh.deletes() != 0 {
			t.Fatalf("%s: a DELETE was sent: %v", name, d.gh.all())
		}
		if _, ok := d.find("demo-gh"); !ok || d.proj("demo-gh").Remote == nil {
			t.Fatalf("%s: registry changed", name)
		}
		d.wantCode("delete_remote_repo", a, "request_reused") // the signature was valid: burnt
	}
	// GitLab: same
	for name, setup := range map[string]func(d *delEnv){
		"live id differs":   func(d *delEnv) { d.gl.repoBody = glRepoBody(22, "team/demo") },
		"live path differs": func(d *delEnv) { d.gl.repoBody = glRepoBody(21, "team/other") },
		"live 404":          func(d *delEnv) { d.gl.repoStatus = 404 },
	} {
		d := newDel(t)
		setup(d)
		d.wantCode("delete_remote_repo", d.delArgs("demo-gl", nil), "remote_changed")
		if d.gl.deletes() != 0 {
			t.Fatalf("gitlab %s: DELETE sent", name)
		}
	}
	// GitHub names are case-insensitive: a signed path in another case is the same repo
	d = newDel(t)
	// ... but the typed name is compared with the REGISTRY's name (Demo), not the signed path's
	d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", func(f map[string]any) { f["remote_path"] = "ALICE/demo"; f["confirm_name"] = "demo" }), "confirm_mismatch")
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", func(f map[string]any) { f["remote_path"] = "ALICE/demo"; f["confirm_name"] = "Demo" }), "")
	// GitLab paths are compared as written
	d = newDel(t)
	d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gl", func(f map[string]any) { f["remote_path"] = "team/Demo"; f["confirm_name"] = "Demo" }), "bad_request")
}

func parseQ(q string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, kv := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = []string{v}
	}
	return out, nil
}

func TestConfirmNameMustEqualRepositoryNameExactly(t *testing.T) {
	d := newDel(t)
	for _, bad := range []string{"demo", "DEMO", "Dem", "Demo2"} {
		bad := bad
		d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", func(f map[string]any) { f["confirm_name"] = bad }), "confirm_mismatch")
	}
	// not even a name (cannot be signed: the schema refuses it): the shape refuses
	for _, bad := range []string{"Demo ", "alice/Demo", ""} {
		bad := bad
		d.refusedNoBurn("delete_remote_repo", copyArgs(d.delArgs("demo-gh", nil), func(m map[string]any) { m["confirm_name"] = bad }), "bad_request")
	}
	// the stored hash covers it: another confirm_name than the signed one
	a := d.delArgs("demo-gh", nil)
	a["confirm_name"] = "demo"
	d.refusedNoBurn("delete_remote_repo", a, "confirm_mismatch")
}

func TestRemoteDeleteHappyPathAndRegistryEffects(t *testing.T) {
	d := newDel(t)
	res, oe := d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
	if oe != nil {
		t.Fatalf("%v", oe.Code)
	}
	m := res.(map[string]any)
	if m["remote_state"] != "deleted" || m["id"] != "demo-gh" || m["remote_deleted_at"] != d.now.Format(orphanTimeLayout) {
		// broker clock is the test clock: the stamp is an RFC3339 UTC second
		if _, err := time.Parse(orphanTimeLayout, fmt.Sprint(m["remote_deleted_at"])); err != nil {
			t.Fatalf("result %v", m)
		}
	}
	if got := d.gh.all(); !reflect.DeepEqual(got, []string{"GET /repos/alice/Demo", "DELETE /repos/alice/Demo"}) {
		t.Fatalf("github trace %v", got)
	}
	p := d.proj("demo-gh")
	if p.Remote != nil || p.RemoteDeletedAt == nil || p.Provider == nil || *p.Provider != "dgh" || p.Origin != OriginCreated || p.Kind != "registered" {
		t.Fatalf("registry row after delete: %+v", p)
	}
	// Provider stays set: attach_remote cannot re-attach the row to another repository
	d.wantCode("attach_remote", d.attachArgs("demo-gh", "dgh", "alice/existing", nil), "already_linked")
	// a second delete: nothing at the provider is touched
	n := len(d.gh.all())
	d.refusedNoBurn2("delete_remote_repo", d.delArgs("demo-gh", nil), "remote_changed")
	if len(d.gh.all()) != n {
		t.Fatal("the provider was contacted for an already deleted remote")
	}
	// GitLab: 202 is "requested"
	res, oe = d.do("delete_remote_repo", d.delArgs("demo-gl", nil))
	if oe != nil || res.(map[string]any)["remote_state"] != "requested" {
		t.Fatalf("gitlab: %v %v", res, oe)
	}
	if got := d.gl.all(); !reflect.DeepEqual(got, []string{"GET /api/v4/projects/team%2Fdemo", "DELETE /api/v4/projects/21"}) {
		t.Fatalf("gitlab trace %v", got)
	}
	if p := d.proj("demo-gl"); p.Remote != nil || p.RemoteDeletedAt == nil || p.Provider == nil || *p.Provider != "dgl" {
		t.Fatalf("gitlab row: %+v", p)
	}
	// a replayed signature is refused
	a := d.delArgs("demo-gl", nil)
	if _, oe := d.do("delete_remote_repo", a); oe == nil {
		t.Fatal("deleting twice succeeded")
	}
}

// refusedNoBurn2 is refusedNoBurn without the registry snapshot equality (the
// state legitimately changed before).
func (d *delEnv) refusedNoBurn2(op string, a map[string]any, code string) {
	d.t.Helper()
	for i := 0; i < 2; i++ {
		d.wantCode(op, a, code)
	}
}

// ---------- remove_project ----------

func TestRemoveProjectLegacyAckBothDirections(t *testing.T) {
	d := newDel(t)
	d.putRaw(Project{ID: "toy", Path: d.root + "/toy", Kind: "legacy", Origin: OriginLegacy})
	d.putRaw(Project{ID: "plain", Path: d.root + "/plain", Kind: "registered", Origin: OriginUnknown})
	d.refusedNoBurn("remove_project", d.rmArgs("toy", false), "legacy_ack_required")
	d.refusedNoBurn("remove_project", d.rmArgs("plain", true), "bad_request")
	d.refusedNoBurn("remove_project", copyArgs(d.rmArgs("plain", false), func(m map[string]any) { m["scope"] = "remote" }), "bad_request")
	d.refusedNoBurn("remove_project", d.rmArgs("ghost", false), "project_unknown")
	// the field is required
	a := d.rmArgs("plain", false)
	delete(a, "legacy_ack")
	d.refusedNoBurn("remove_project", a, "bad_request")
	// ok both ways
	d.wantCode("remove_project", d.rmArgs("toy", true), "")
	d.wantCode("remove_project", d.rmArgs("plain", false), "")
	if _, ok := d.find("toy"); ok {
		t.Fatal("legacy project not removed")
	}
	if _, ok := d.find("plain"); ok {
		t.Fatal("project not removed")
	}
	d.wantCode("remove_project", d.rmArgs("plain", false), "project_unknown") // idempotent-safe
	d.noHits("remove_project")
	// a created project with a remote: only the row goes, the provider is never contacted
	d.wantCode("remove_project", d.rmArgs("demo-gh", false), "")
	d.noHits("remove_project of a created project")
}

// ---------- deploy keys ----------

func TestDeleteKeyOnlyMatchingTitleAndFingerprint(t *testing.T) {
	list := keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"2", "orchd demo-gh", delKeyB + " orchd@host"}, [3]string{"3", "my laptop", delKeyB}, [3]string{"4", "orchd demo-gh2", delKeyA})
	// no key line: bad_request in the decode/shape stage, nothing sent, no ID burnt
	d := newDel(t)
	d.gh.keyPages[1] = list
	nokey := d.keyArgs("demo-gh", "")
	delete(nokey, "deploy_key")
	d.refusedNoBurn("remove_deploy_key", nokey, "bad_request")
	// the title AND the key body match: that one key goes, nothing else
	d = newDel(t)
	d.gh.keyPages[1] = list
	if _, oe := d.do("remove_deploy_key", d.keyArgs("demo-gh", "")); oe != nil {
		t.Fatal(oe.Code)
	}
	if got := d.gh.all(); !reflect.DeepEqual(got, []string{"GET /repos/alice/Demo", "GET /repos/alice/Demo/keys?page=1&per_page=100", "DELETE /repos/alice/Demo/keys/2"}) {
		t.Fatalf("trace %v", got)
	}
	// the key line decides: matching passes (a comment on the line is ignored); the same title with a
	// DIFFERENT body is key_not_found and nothing is deleted
	d = newDel(t)
	d.gh.keyPages[1] = list
	if _, oe := d.do("remove_deploy_key", d.keyArgs("demo-gh", delKeyB+" orchd")); oe != nil {
		t.Fatal(oe.Code)
	}
	if d.gh.deletes() != 1 {
		t.Fatalf("%v", d.gh.all())
	}
	d = newDel(t)
	d.gh.keyPages[1] = list
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", delKeyA), "key_not_found")
	if d.gh.deletes() != 0 {
		t.Fatal("a key that does not match was deleted")
	}
	// no titled key at all
	d = newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"3", "my laptop", delKeyB})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "key_not_found")
	if d.gh.deletes() != 0 {
		t.Fatal("deleted something")
	}
	// the title must match exactly: case and prefix/suffix variants never do
	d = newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"1", "ORCHD demo-gh", delKeyB}, [3]string{"2", "orchd demo-gh ", delKeyB}, [3]string{"3", "orchd  demo-gh", delKeyB}, [3]string{"4", "xorchd demo-gh", delKeyB})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "key_not_found")
	// two keys with the title AND the body: refuse, delete nothing
	d = newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd demo-gh", delKeyB}, [3]string{"2", "orchd demo-gh", delKeyB})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "remove_refused")
	if d.gh.deletes() != 0 {
		t.Fatal("deleted with an ambiguous match")
	}
	// ... same title, different bodies: the key line tells them apart
	d = newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd demo-gh", delKeyA}, [3]string{"2", "orchd demo-gh", delKeyB})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", delKeyB), "")
	if got := d.gh.all(); got[len(got)-1] != "DELETE /repos/alice/Demo/keys/2" {
		t.Fatalf("%v", got)
	}
	// a list that may be incomplete is never used
	d = newDel(t)
	full := make([][3]string, 100)
	for i := range full {
		full[i] = [3]string{strconv.Itoa(i + 10), "k" + strconv.Itoa(i), delKeyA}
	}
	for p := 1; p <= apiMaxPages; p++ {
		d.gh.keyPages[p] = keysJSON(full...)
	}
	d.gh.keyPages[2] = keysJSON(append(full[:99:99], [3]string{"500", "orchd demo-gh", delKeyB})...)
	_, oe := d.do("remove_deploy_key", d.keyArgs("demo-gh", ""))
	if oe == nil || oe.Code != "github_refused" || d.gh.deletes() != 0 {
		t.Fatalf("a truncated list was used: %v %v", oe, d.gh.all())
	}
	// a malformed entry spoils the whole list
	d = newDel(t)
	d.gh.keyPages[1] = `[{"id":2,"title":"orchd demo-gh","key":"` + delKeyB + `"},{"id":3,"title":"x"}]`
	_, oe = d.do("remove_deploy_key", d.keyArgs("demo-gh", ""))
	if oe == nil || d.gh.deletes() != 0 {
		t.Fatalf("%v", oe)
	}
	// the shape: a key line that is not a key
	d = newDel(t)
	d.refusedNoBurn("remove_deploy_key", d.keyArgs("demo-gh", "not a key"), "bad_request")
	// works for linked and unknown origin; registry untouched; remote missing -> remote_changed
	for _, origin := range []string{OriginLinked, OriginUnknown} {
		d = newDel(t)
		d.put("demo-gh", "dgh", "alice/Demo", 77, origin)
		d.gh.keyPages[1] = list
		before := d.regFile()
		d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "")
		if string(before) != string(d.regFile()) {
			t.Fatal("the registry changed for a key removal")
		}
	}
	d = newDel(t)
	d.putRaw(Project{ID: "demo-gh", Path: d.root + "/demo-gh", Kind: "registered", Origin: OriginUnknown})
	d.refusedNoBurn("remove_deploy_key", d.keyArgs("demo-gh", ""), "remote_changed")
	// the live repository must still be the registry's
	d = newDel(t)
	d.gh.repoBody = ghRepoBody(99, "alice/Demo")
	d.gh.keyPages[1] = list
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "remote_changed")
	if d.gh.deletes() != 0 || d.gh.count("GET") != 1 {
		t.Fatalf("%v", d.gh.all())
	}
	// refusals and uncertain outcomes of the key DELETE
	d = newDel(t)
	d.gh.keyPages[1] = list
	d.gh.keyStatus = 403
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "remove_refused")
	d = newDel(t)
	d.gh.keyPages[1] = list
	d.gh.keyStatus = 500
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "delete_partial")
	if d.gh.deletes() != 1 {
		t.Fatal("retried")
	}
}

// What this proves: removing a key on GitLab sends exactly ONE per-project
// DELETE for the key whose title AND body match, never another listed key, and
// there is no global /deploy_keys delete row anywhere.
func TestGitLabKeyRemovalOnlyThisProjectsKeyId(t *testing.T) {
	d := newDel(t)
	list := keysJSON([3]string{"8", "orchd demo-gl2", delKeyA}, [3]string{"9", "orchd demo-gl", delKeyB}, [3]string{"10", "shared deploy", delKeyC})
	d.gl.keyPages[1] = list
	if _, oe := d.do("remove_deploy_key", d.keyArgs("demo-gl", delKeyB)); oe != nil {
		t.Fatal(oe.Code)
	}
	want := []string{"GET /api/v4/projects/team%2Fdemo", "GET /api/v4/projects/21/deploy_keys?page=1&per_page=100", "DELETE /api/v4/projects/21/deploy_keys/9"}
	if got := d.gl.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("trace %v", got)
	}
	for _, r := range d.gl.all() {
		if strings.HasPrefix(r, "DELETE /api/v4/deploy_keys") || strings.Contains(r, "/deploy_keys/10") || strings.Contains(r, "/deploy_keys/8") {
			t.Fatalf("a key that is not ours was touched: %s", r)
		}
	}
	// the project's registry row and the repository are untouched
	if p := d.proj("demo-gl"); p.Remote == nil || p.RemoteDeletedAt != nil {
		t.Fatalf("%+v", p)
	}
	// the right title with another key body, and the right body under another title: nothing sent
	d2 := newDel(t)
	d2.gl.keyPages[1] = list
	d2.wantCode("remove_deploy_key", d2.keyArgs("demo-gl", delKeyC), "key_not_found") // 9 has the title, body B
	d2.wantCode("remove_deploy_key", d2.keyArgs("demo-gl", delKeyA), "key_not_found") // 8 has body A, title demo-gl2
	if d2.gl.deletes() != 0 {
		t.Fatal("a key that does not match both was deleted")
	}
	// the key itself is never deleted by a direct call either (no global row)
	la, _ := newGitLabAPI(Provider{ID: "dgl", Kind: "gitlab", BaseURL: d.gl.URL, SSHURL: "ssh://git@10.0.0.3", TokenRef: "gl-admin"}, d.s.Tokens, d.s.apiSeams)
	if _, err := la.do(t.Context(), "DELETE", "/api/v4/deploy_keys/9", nil, nil, 204); ErrCode(err) != CodeNotAllowlisted {
		t.Fatalf("%v", err)
	}
}

// ---------- token ----------

func TestDeleteUsesOnlyTheProvidersOwnToken(t *testing.T) {
	d := newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"2", "orchd demo-gh", delKeyB})
	d.gl.keyPages[1] = keysJSON([3]string{"9", "orchd demo-gl", delKeyB})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "")
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gl", ""), "")
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "")
	d.wantCode("delete_remote_repo", d.delArgs("demo-gl", nil), "")
	if d.gh.deletes() != 2 || d.gl.deletes() != 2 {
		t.Fatalf("%v %v", d.gh.all(), d.gl.all())
	}
	for _, c := range d.gh.creds() {
		if c != "Bearer "+ghCanary {
			t.Fatalf("a GitHub request used %q", c)
		}
	}
	for _, c := range d.gl.creds() {
		if c != c3Canary {
			t.Fatalf("a GitLab request used %q", c)
		}
	}
	// no op argument names a token: the ops have no token input at all
	for _, k := range [][]string{deleteRemoteRepoKeys, removeDeployKeyKeys, removeProjectKeys, clearOrphanKeys} {
		for _, name := range k {
			if strings.Contains(name, "token") {
				t.Fatalf("argument %s", name)
			}
		}
	}
	// the provider's token gone from the store: nothing is sent (no unsigned fallback)
	d = newDel(t)
	if err := d.s.Tokens.Delete("gh-tok"); err != nil {
		t.Fatal(err)
	}
	_, oe := d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
	if oe == nil || d.hits() != 0 || d.proj("demo-gh").Remote == nil {
		t.Fatalf("%v %v", oe, d.gh.all())
	}
	// the provider's own token is of its kind: a gitlab token never goes to a github provider
	d = newDel(t)
	err := d.s.ReplaceProviderChecked(Provider{ID: "dgh", Kind: "github", BaseURL: d.gh.URL, SSHURL: "ssh://git@10.0.0.2", TokenRef: "gl-other"})
	if err == nil {
		// the store may allow it; the client must still refuse (ValueOfKind)
		_, oe = d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
		if oe == nil || d.hits() != 0 {
			t.Fatalf("wrong-kind token: %v %v", oe, d.gh.all())
		}
	}
	// the other tokens never appear in any request
	for _, f := range []*delFake{d.gh, d.gl} {
		for _, c := range f.creds() {
			if strings.Contains(c, delOtherGH) || strings.Contains(c, delOtherGL) {
				t.Fatal("another token was used")
			}
		}
	}
}

// ---------- uncertain outcomes ----------

func TestDeletePartialAndUnknownOutcomeNoAutoRetry(t *testing.T) {
	type tc struct {
		name  string
		setup func(d *delEnv)
		code  string
	}
	for _, c := range []tc{
		{"500", func(d *delEnv) { d.gh.delStatus = 500 }, "delete_partial"},
		{"502", func(d *delEnv) { d.gh.delStatus = 502 }, "delete_partial"},
		{"200 instead of 204", func(d *delEnv) { d.gh.delStatus = 200 }, "delete_partial"},
		{"redirect", func(d *delEnv) { d.gh.delStatus = 307 }, "delete_partial"},
		{"connection dropped", func(d *delEnv) { d.gh.delAbort = true }, "delete_partial"},
		{"timeout", func(d *delEnv) {
			d.gh.delDelay = 1500 * time.Millisecond
			d.s.apiSeams = &apiSeams{policy: allowLoopback, total: 300 * time.Millisecond}
		}, "delete_partial"},
		{"403 refused", func(d *delEnv) { d.gh.delStatus = 403 }, "remove_refused"},
		{"404 refused", func(d *delEnv) { d.gh.delStatus = 404 }, "remove_refused"},
		{"409 refused", func(d *delEnv) { d.gh.delStatus = 409 }, "remove_refused"},
		{"429 rate limited", func(d *delEnv) { d.gh.delStatus = 429 }, "provider_rate_limited"},
	} {
		d := newDel(t)
		c.setup(d)
		before := d.proj("demo-gh")
		a := d.delArgs("demo-gh", nil)
		d.wantCode("delete_remote_repo", a, c.code)
		if d.gh.deletes() != 1 {
			t.Fatalf("%s: %d DELETE requests: %v", c.name, d.gh.deletes(), d.gh.all())
		}
		if !reflect.DeepEqual(before, d.proj("demo-gh")) {
			t.Fatalf("%s: the registry changed", c.name)
		}
		// no orphan record for a delete; the signature is burnt (no re-use)
		if len(d.orphans()) != 0 {
			t.Fatalf("%s: orphan records %v", c.name, d.orphans())
		}
		d.wantCode("delete_remote_repo", a, "request_reused")
		if d.gh.deletes() != 1 {
			t.Fatalf("%s: the replay reached the provider", c.name)
		}
	}
	// GitLab: 200 is not the documented 202; 204 neither
	for _, st := range []int{200, 204, 500} {
		d := newDel(t)
		d.gl.delStatus = st
		d.wantCode("delete_remote_repo", d.delArgs("demo-gl", nil), "delete_partial")
		if d.gl.deletes() != 1 || d.proj("demo-gl").Remote == nil {
			t.Fatalf("gitlab %d", st)
		}
	}
	// a panic after the DELETE was sent is an uncertain outcome (and never retried)
	d := newDel(t)
	d.s.Now = func() time.Time {
		if d.gh.deletes() > 0 {
			panic("boom " + ghCanary)
		}
		return d.now
	}
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "delete_partial")
	if d.gh.deletes() != 1 || strings.Contains(d.logs.String(), "boom") || strings.Contains(d.logs.String(), ghCanary) {
		t.Fatalf("deletes %d logs %q", d.gh.deletes(), d.logs.String())
	}
}

func TestRegistryWriteFailureAfterDeleteIsPartial(t *testing.T) {
	d := newDel(t)
	// make the registry unwritable only once the DELETE has been sent
	d.s.Registry.opts.failAt = func(stage string) error {
		if d.gh.deletes() > 0 && stage != "" {
			return os.ErrPermission
		}
		return nil
	}
	_, oe := d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
	d.s.Registry.opts.failAt = nil
	if oe == nil || oe.Code != "delete_partial" {
		t.Fatalf("%v", oe)
	}
	if d.gh.deletes() != 1 {
		t.Fatal("retried")
	}
}

// ---------- limits ----------

func TestDailyLimit(t *testing.T) {
	d := newDel(t)
	clock := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	d.s.State.DeleteRepoDay = NewWindowLimiter(maxDeleteRepoPerDay, 24*time.Hour, now)
	d.s.State.DeleteRepoHour = NewHourlyLimiter(100, now)
	for i := 0; i < 5; i++ {
		d.put("p"+strconv.Itoa(i), "dgh", "alice/Demo", 77, OriginCreated)
	}
	for i := 0; i < 3; i++ {
		d.wantCode("delete_remote_repo", d.delArgs("p"+strconv.Itoa(i), nil), "")
		clock = clock.Add(7 * time.Hour) // 21 hours in total: still inside the day
		d.put("p"+strconv.Itoa(i), "dgh", "alice/Demo", 77, OriginCreated)
	}
	dels := d.gh.deletes()
	d.wantCode("delete_remote_repo", d.delArgs("p3", nil), "rate_limited")
	if d.gh.deletes() != dels {
		t.Fatal("a DELETE went out past the daily cap")
	}
	if d.s.State.DeleteRepoDay.Remaining() != 0 {
		t.Fatal("the day limiter is not exhausted")
	}
	clock = clock.Add(4 * time.Hour) // the first hit left the 24 hour window
	d.wantCode("delete_remote_repo", d.delArgs("p4", nil), "")

	// the hourly cap, and a refusal by one cap spends nothing of the other
	d = newDel(t)
	hour := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	hnow := func() time.Time { return hour }
	d.s.State.DeleteRepoDay = NewWindowLimiter(10, 24*time.Hour, hnow)
	d.s.State.DeleteRepoHour = NewHourlyLimiter(maxDeleteRepoPerHour, hnow)
	for i := 0; i < 4; i++ {
		d.put("h"+strconv.Itoa(i), "dgh", "alice/Demo", 77, OriginCreated)
	}
	if maxDeleteRepoPerHour >= maxDeleteRepoPerDay {
		t.Fatal("the hourly cap must be tighter than the daily one")
	}
	for i := 0; i < maxDeleteRepoPerHour; i++ {
		d.wantCode("delete_remote_repo", d.delArgs("h"+strconv.Itoa(i), nil), "")
	}
	dayLeft := d.s.State.DeleteRepoDay.Remaining()
	d.wantCode("delete_remote_repo", d.delArgs("h"+strconv.Itoa(maxDeleteRepoPerHour), nil), "rate_limited")
	if d.s.State.DeleteRepoDay.Remaining() != dayLeft {
		t.Fatal("the daily cap was spent by a request the hourly cap refused")
	}
	// and the other way: the daily cap refuses, the hourly slot is not consumed
	d = newDel(t)
	d.s.State.DeleteRepoDay = NewWindowLimiter(1, 24*time.Hour, hnow)
	d.s.State.DeleteRepoHour = NewHourlyLimiter(5, hnow)
	d.put("x1", "dgh", "alice/Demo", 77, OriginCreated)
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "")
	hourLeft := d.s.State.DeleteRepoHour.Remaining()
	d.wantCode("delete_remote_repo", d.delArgs("x1", nil), "rate_limited")
	if d.s.State.DeleteRepoHour.Remaining() != hourLeft {
		t.Fatal("the hourly cap was spent by a request the daily cap refused")
	}
	// shape refusals spend nothing
	d = newDel(t)
	for i := 0; i < 10; i++ {
		d.wantCode("delete_remote_repo", d.delArgs("demo-gh", func(f map[string]any) { f["remote_id"] = "5" }), "remote_changed")
	}
	if d.s.State.DeleteRepoDay.Remaining() != maxDeleteRepoPerDay {
		t.Fatal("shape refusals spent quota")
	}
	// a broken limiter fails closed
	d = newDel(t)
	d.s.State.DeleteRepoDay = nil
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "rate_limited")
	if d.gh.deletes() != 0 {
		t.Fatal("deleted without a limiter")
	}
}

func TestLocalDeleteHourlyLimit(t *testing.T) {
	d := newDel(t)
	clock := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	d.s.State.DeleteLocal = NewHourlyLimiter(maxLocalDeletePerHour, func() time.Time { return clock })
	for i := 0; i < maxLocalDeletePerHour; i++ {
		id := "q" + strconv.Itoa(i)
		d.putRaw(Project{ID: id, Path: d.root + "/" + id, Kind: "registered", Origin: OriginUnknown})
		d.wantCode("remove_project", d.rmArgs(id, false), "")
	}
	d.putRaw(Project{ID: "q99", Path: d.root + "/q99", Kind: "registered", Origin: OriginUnknown})
	d.wantCode("remove_project", d.rmArgs("q99", false), "rate_limited")
	if _, ok := d.find("q99"); !ok {
		t.Fatal("removed past the cap")
	}
	d.gh.keyPages[1] = keysJSON([3]string{"2", "orchd demo-gh", delKeyB})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "rate_limited") // key removal shares the cap
	if d.gh.deletes() != 0 {
		t.Fatal("key deleted past the cap")
	}
	clock = clock.Add(61 * time.Minute)
	d.wantCode("remove_project", d.rmArgs("q99", false), "")
}

// ---------- orphans ----------

func TestClearOrphanExactRecordOnly(t *testing.T) {
	d := newDel(t)
	add := func(prov, proj string) {
		if _, err := d.s.Registry.AddOrphan(Orphan{Provider: prov, Project: proj, Step: "deploy_key", At: "2026-11-10T01:02:03Z", Origin: OriginCreated}); err != nil {
			t.Fatal(err)
		}
	}
	add("dgh", "alice/Demo")
	add("dgh", "alice/Demo") // a duplicate
	add("dgl", "team/demo")
	add("dgh", "alice/other")
	ca := func(prov, proj string) map[string]any {
		return d.signedArgs(d.alice, registrymsg.ActionClearOrphan, prov, map[string]any{"project": proj})
	}
	d.refusedNoBurn("clear_orphan", ca("dgh", "alice/demo"), "orphan_unknown") // case differs
	d.refusedNoBurn("clear_orphan", ca("dgl", "alice/Demo"), "orphan_unknown") // another provider
	d.refusedNoBurn("clear_orphan", ca("dgh", "alice/Dem"), "orphan_unknown")  // prefix
	d.refusedNoBurn("clear_orphan", ca("ghost", "alice/Demo"), "orphan_unknown")
	d.refusedNoBurn("clear_orphan", copyArgs(ca("dgh", "alice/Demo"), func(m map[string]any) { m["project"] = "../x" }), "bad_request")
	d.wantCode("clear_orphan", ca("dgh", "alice/Demo"), "")
	left := func() []string {
		var out []string
		for _, o := range d.orphans() {
			out = append(out, o.Provider+" "+o.Project)
		}
		return out
	}
	if got := left(); !reflect.DeepEqual(got, []string{"dgh alice/Demo", "dgl team/demo", "dgh alice/other"}) {
		t.Fatalf("exactly one record goes: %v", got)
	}
	d.wantCode("clear_orphan", ca("dgh", "alice/Demo"), "")
	d.wantCode("clear_orphan", ca("dgh", "alice/Demo"), "orphan_unknown")
	if got := left(); !reflect.DeepEqual(got, []string{"dgl team/demo", "dgh alice/other"}) {
		t.Fatalf("%v", got)
	}
	d.noHits("clear_orphan")
	// no provider call even for an unknown provider id: the record outlives its provider
	d.wantCode("clear_orphan", ca("dgl", "team/demo"), "")
}

// ---------- signed fields ----------

func TestSignedFieldsCoverEveryDeleteInput(t *testing.T) {
	type op struct {
		name string
		mk   func(d *delEnv) map[string]any
		sig  []string // the signed fields
	}
	ops := []op{
		{"remove_project", func(d *delEnv) map[string]any {
			d.putRaw(Project{ID: "plain", Path: d.root + "/plain", Kind: "registered", Origin: OriginUnknown})
			return d.rmArgs("plain", false)
		}, []string{"scope", "legacy_ack", "detach_key"}},
		{"delete_remote_repo", func(d *delEnv) map[string]any { return d.delArgs("demo-gh", nil) }, []string{"remote_path", "remote_id", "confirm_name", "also_local"}},
		{"remove_deploy_key", func(d *delEnv) map[string]any { return d.keyArgs("demo-gh", "") }, []string{"remote_path"}},
		{"clear_orphan", func(d *delEnv) map[string]any {
			d.s.Registry.AddOrphan(Orphan{Provider: "dgh", Project: "alice/Demo", Step: "deploy_key", At: "2026-11-10T01:02:03Z", Origin: OriginCreated})
			return d.signedArgs(d.alice, registrymsg.ActionClearOrphan, "dgh", map[string]any{"project": "alice/Demo"})
		}, []string{"project"}},
	}
	for _, o := range ops {
		d := newDel(t)
		a := o.mk(d)
		// the args are exactly: the subject, the signed fields, the change envelope (+ the optional unsigned deploy_key)
		var keys []string
		for k := range a {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := append([]string{"id", "change", "request", "issued", "signature"}, o.sig...)
		if o.name == "remove_deploy_key" {
			want = append(want, "deploy_key") // required, unsigned
		}
		sort.Strings(want)
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("%s: args %v, want %v", o.name, keys, want)
		}
		// every handler key list is the schema fields plus the envelope (and the optional key line)
		handlerKeys := map[string][]string{"remove_project": removeProjectKeys, "delete_remote_repo": deleteRemoteRepoKeys, "remove_deploy_key": removeDeployKeyKeys, "clear_orphan": clearOrphanKeys}[o.name]
		hk := append([]string(nil), handlerKeys...)
		hk2 := hk
		sort.Strings(hk2)
		if o.name == "remove_project" {
			// W3g: the optional unsigned key line (only accepted with detach_key=true)
			want = append(append([]string(nil), want...), "deploy_key")
			sort.Strings(want)
		}
		if !reflect.DeepEqual(hk2, want) {
			t.Fatalf("%s: handler accepts %v, want %v", o.name, hk2, want)
		}
		// changing ANY signed value after signing is refused and reaches no provider
		for _, f := range o.sig {
			m := copyArgs(a, func(m map[string]any) {
				switch v := m[f].(type) {
				case bool:
					m[f] = !v
				case string:
					m[f] = v + "x"
					if f == "remote_id" {
						m[f] = "78"
					}
					if f == "scope" {
						m[f] = "remote"
					}
				}
			})
			_, oe := d.do(o.name, m)
			if oe == nil {
				t.Fatalf("%s: a changed %s was accepted", o.name, f)
			}
		}
		// changed subject
		m := copyArgs(a, func(m map[string]any) { m["id"] = "other" })
		if _, oe := d.do(o.name, m); oe == nil {
			t.Fatalf("%s: changed subject accepted", o.name)
		}
		if d.hits() != 0 {
			t.Fatalf("%s: a tampered request reached a provider: %v %v", o.name, d.gh.all(), d.gl.all())
		}
		// the untampered request still works (so no ID was burnt by the refusals above)
		if _, oe := d.do(o.name, a); oe != nil && o.name != "remove_deploy_key" {
			t.Fatalf("%s: genuine request refused: %v", o.name, oe.Code)
		}
	}
	// also_local is signed: flipping it is a change_mismatch (the shape accepts both values)
	d := newDel(t)
	a := d.delArgs("demo-gh", nil)
	a["also_local"] = true
	d.wantCode("delete_remote_repo", a, "change_mismatch")
	// an unsigned request, or a bad signature
	b := d.delArgs("demo-gh", nil)
	delete(b, "signature")
	d.wantCode("delete_remote_repo", b, "not_signed")
	d.noHits("unsigned")
}

// ---------- locks ----------

func TestConcurrentDeleteOfOneProjectSendsOneDelete(t *testing.T) {
	d := newDel(t)
	d.gh.delHold = make(chan struct{})
	d.gh.delSeen = make(chan struct{}, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	var first string
	go func() {
		defer wg.Done()
		_, oe := d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
		first = codeOf(oe)
	}()
	select {
	case <-d.gh.delSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the first delete did not reach the provider")
	}
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "busy")
	d.wantCode("remove_project", d.rmArgs("demo-gh", false), "busy")
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gh", ""), "busy")
	// another project id of the same remote is held by the remote lock
	d.put("twin", "dgh", "alice/Demo", 77, OriginCreated)
	d.wantCode("delete_remote_repo", d.signedArgs(d.alice, registrymsg.ActionDeleteRemoteRepo, "twin", d.delFields("alice/Demo", "77", "Demo")), "busy")
	close(d.gh.delHold)
	wg.Wait()
	if first != "" || d.gh.deletes() != 1 {
		t.Fatalf("first=%q deletes=%d", first, d.gh.deletes())
	}
	// the locks are released
	d.putRaw(Project{ID: "later", Path: d.root + "/later", Kind: "registered", Origin: OriginUnknown})
	d.wantCode("remove_project", d.rmArgs("later", false), "")
}

// ---------- client provenance ----------

func TestDeleteClientsRefuseForeignHandles(t *testing.T) {
	d := newDel(t)
	ctx := t.Context()
	gp, err := d.s.repoProviderForEntry(Provider{ID: "dgh", Kind: "github", BaseURL: d.gh.URL, SSHURL: "ssh://git@10.0.0.2", TokenRef: "gh-tok"})
	if err != nil {
		t.Fatal(err)
	}
	lp, err := d.s.repoProviderForEntry(Provider{ID: "dgl", Kind: "gitlab", BaseURL: d.gl.URL, SSHURL: "ssh://git@10.0.0.3", TokenRef: "gl-admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rp   RepoProvider
		info RepoInfo
		f    *delFake
	}{{gp, RepoInfo{ID: 77, Path: "alice/Demo"}, d.gh}, {lp, RepoInfo{ID: 21, Path: "team/demo"}, d.gl}} {
		// a hand-built handle was never read by this instance
		if err := c.rp.DeleteRepo(ctx, c.info); !errorsIsBadInput(err) {
			t.Fatalf("%s: %v", c.rp.Kind(), err)
		}
		if _, err := c.rp.ListDeployKeys(ctx, c.info); !errorsIsBadInput(err) {
			t.Fatalf("%s: %v", c.rp.Kind(), err)
		}
		if err := c.rp.RemoveDeployKey(ctx, c.info, DeployKeyInfo{ID: 2, Title: "orchd x"}); !errorsIsBadInput(err) {
			t.Fatalf("%s: %v", c.rp.Kind(), err)
		}
		if c.f.hits2() != 0 {
			t.Fatalf("%s: requests were sent: %v", c.rp.Kind(), c.f.all())
		}
		// live handle: delete once; the handle is consumed
		live, err := c.rp.LiveRepo(ctx, c.info.Path)
		if err != nil {
			t.Fatal(err)
		}
		// a live handle of a different id is not the handle
		bad := live
		bad.ID++
		if err := c.rp.DeleteRepo(ctx, bad); !errorsIsBadInput(err) {
			t.Fatalf("%v", err)
		}
		if err := c.rp.DeleteRepo(ctx, live); err != nil {
			t.Fatalf("%s: %v", c.rp.Kind(), err)
		}
		if err := c.rp.DeleteRepo(ctx, live); !errorsIsBadInput(err) {
			t.Fatalf("%s: the handle was not consumed: %v", c.rp.Kind(), err)
		}
		if c.f.deletes() != 1 {
			t.Fatalf("%s: %d deletes", c.rp.Kind(), c.f.deletes())
		}
	}
	// a key that was not listed cannot be removed even with a live handle
	live, _ := gp.LiveRepo(ctx, "alice/Demo")
	d.gh.keyPages[1] = keysJSON([3]string{"2", "orchd demo-gh", delKeyB})
	if _, err := gp.ListDeployKeys(ctx, live); err != nil {
		t.Fatal(err)
	}
	n := d.gh.deletes()
	if err := gp.RemoveDeployKey(ctx, live, DeployKeyInfo{ID: 3}); !errorsIsBadInput(err) {
		t.Fatalf("%v", err)
	}
	if d.gh.deletes() != n {
		t.Fatal("unlisted key deleted")
	}
	if err := gp.RemoveDeployKey(ctx, live, DeployKeyInfo{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if err := gp.RemoveDeployKey(ctx, live, DeployKeyInfo{ID: 2}); !errorsIsBadInput(err) {
		t.Fatalf("a key handle was reused: %v", err)
	}
}

func (f *delFake) hits2() int { return len(f.all()) }

func errorsIsBadInput(err error) bool { return ErrCode(err) == CodeBadInput }

// ---------- messages and codes ----------

func TestDeleteCodesFixedSentences(t *testing.T) {
	codes := []string{"legacy_ack_required", "origin_unknown", "not_created_by_tool", "remote_changed", "confirm_mismatch", "delete_partial", "key_not_found", "remove_refused", "orphan_unknown"}
	seen := map[string]string{}
	for _, code := range codes {
		m, ok := fixedMessages[code]
		if !ok || strings.TrimSpace(m) == "" {
			t.Fatalf("no fixed sentence for %s", code)
		}
		hangul := false
		for _, r := range m {
			if r >= 0xAC00 && r <= 0xD7A3 {
				hangul = true
			}
		}
		if !hangul || strings.ContainsAny(m, "\n\r{}%") {
			t.Fatalf("%s: not a plain Korean sentence: %q", code, m)
		}
		if l := strings.ToLower(m); strings.Contains(l, "github") || strings.Contains(l, "gitlab") {
			t.Fatalf("%s names a provider: %q", code, m)
		}
		if prev, dup := seen[m]; dup {
			t.Fatalf("%s and %s share a sentence", code, prev)
		}
		seen[m] = code
	}
	// project_busy is orchd's, never the broker's
	if _, ok := fixedMessages["project_busy"]; ok {
		t.Fatal("project_busy is not a broker code")
	}
	// drift: every code delete_ops.go raises is registered, and every registered new code is raised there
	src, _ := os.ReadFile("delete_ops.go")
	re := regexp.MustCompile(`(?:fail|fail1|opCodeErr)\("([a-z_]+)"\)|return p, [A-Za-z{}]*, "([a-z_]+)"|return Project\{\}, "([a-z_]+)"|return [A-Za-z]+, "([a-z_]+)"`)
	raised := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		for _, g := range m[1:] {
			if g != "" {
				raised[g] = true
			}
		}
	}
	for c := range raised {
		if _, ok := fixedMessages[c]; !ok {
			t.Errorf("delete_ops.go raises %q, which has no fixed sentence", c)
		}
	}
	for _, c := range codes {
		if !raised[c] && c != "delete_partial" && c != "remove_refused" {
			t.Errorf("%s is registered but raised nowhere in delete_ops.go", c)
		}
		if !strings.Contains(string(src), `"`+c+`"`) {
			t.Errorf("%s does not appear in delete_ops.go", c)
		}
	}
	for _, op := range []string{"remove_project", "delete_remote_repo", "remove_deploy_key", "clear_orphan"} {
		if defaultOpTimeouts[op] <= 0 {
			t.Fatalf("%s has no op timeout", op)
		}
	}
}

func TestDeleteCanaryTokenNeverInAnyOutput(t *testing.T) {
	d := newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"2", "orchd demo-gh", delKeyB})
	d.gh.delStatus = 403
	d.gh.keyStatus = 500
	var outs []string
	for _, tc := range []struct {
		op string
		a  map[string]any
	}{
		{"delete_remote_repo", d.delArgs("demo-gh", nil)},
		{"remove_deploy_key", d.keyArgs("demo-gh", "")},
		{"delete_remote_repo", d.delArgs("demo-gl", func(f map[string]any) { f["remote_id"] = "21" })},
	} {
		if tc.op == "delete_remote_repo" && tc.a["id"] == "demo-gl" {
			d.gl.repoStatus = 500
		}
		r := d.wire(tc.op, tc.a)
		if r.OK || r.Message != fixedMessages[r.Code] {
			t.Fatalf("%s: %+v", tc.op, r)
		}
		outs = append(outs, fmt.Sprintf("%+v", r))
	}
	d.gl.repoStatus = 0
	outs = append(outs, d.logs.String())
	for _, o := range outs {
		for _, bad := range []string{ghCanary, c3Canary, delOtherGH, delOtherGL, "REFLECT"} {
			if strings.Contains(o, bad) {
				t.Fatalf("%q leaked into %q", bad, o)
			}
		}
	}
}

// ---------- fix round 1 ----------

// A concurrent set_flag (ai_context) between the live check and the registry
// write must not turn a completed delete into delete_partial.
func TestRemoteDeleteSurvivesConcurrentFlagFlip(t *testing.T) {
	d := newDel(t)
	d.gh.delHold = make(chan struct{})
	d.gh.delSeen = make(chan struct{}, 1)
	done := make(chan string, 1)
	go func() {
		_, oe := d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
		done <- codeOf(oe)
	}()
	select {
	case <-d.gh.delSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the delete did not reach the provider")
	}
	if err := d.s.Registry.Update(func(r *Registry) error {
		for i := range r.Projects {
			if r.Projects[i].ID == "demo-gh" {
				r.Projects[i].AIContext = !r.Projects[i].AIContext
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(d.gh.delHold)
	if code := <-done; code != "" {
		t.Fatalf("a flag flip turned the finished delete into %q", code)
	}
	if p := d.proj("demo-gh"); p.Remote != nil || p.RemoteDeletedAt == nil {
		t.Fatalf("registry not updated: %+v", p)
	}
}

func TestSameRowCoreComparesOnlyMeaningfulFields(t *testing.T) {
	prov, rid := "dgh", int64(7)
	base := Project{ID: "a", Path: "/r/a", Kind: "registered", Origin: OriginCreated, Provider: &prov, Remote: &RemoteLink{Path: "o/a", ID: &rid}}
	flip := base
	flip.AIContext = true
	if !sameRowCore(base, flip) {
		t.Fatal("ai_context must not matter")
	}
	mods := map[string]func(p *Project){
		"path":     func(p *Project) { p.Path = "/r/b" },
		"kind":     func(p *Project) { p.Kind = "x" },
		"origin":   func(p *Project) { p.Origin = OriginLinked },
		"provider": func(p *Project) { o := "other"; p.Provider = &o },
		"remote":   func(p *Project) { p.Remote = &RemoteLink{Path: "o/b", ID: &rid} },
		"id":       func(p *Project) { r2 := int64(8); p.Remote = &RemoteLink{Path: "o/a", ID: &r2} },
		"gone":     func(p *Project) { p.Remote = nil },
		"deleted":  func(p *Project) { p.RemoteDeletedAt = strp("2026-11-10T01:02:03Z") },
	}
	for n, m := range mods {
		c := base
		m(&c)
		if sameRowCore(base, c) {
			t.Errorf("%s change not noticed", n)
		}
	}
}

func TestGitLabDeleteRateLimitAnswerIsUncertain(t *testing.T) {
	d := newDel(t)
	d.gl.delStatus = 429
	d.wantCode("delete_remote_repo", d.delArgs("demo-gl", nil), "delete_partial")
	if d.gl.deletes() != 1 || d.proj("demo-gl").Remote == nil {
		t.Fatal("429 handling")
	}
}

func TestCreatedOriginNeedsProviderAndIsNeverAttached(t *testing.T) {
	d := newDel(t)
	err := d.s.Registry.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: "orphan-created", Path: d.root + "/orphan-created", Kind: "registered", Origin: OriginCreated})
		return nil
	})
	if err == nil {
		t.Fatal("a created project without a provider was accepted")
	}
	// after a remote delete the row keeps its provider, so the invariant holds and attach is refused
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "")
	d.wantCode("attach_remote", d.attachArgs("demo-gh", "dgh", "alice/existing", nil), "already_linked")
	// the attach rule refuses origin created by itself, even for a row without provider (defence in depth)
	reg := &Registry{Projects: []Project{{ID: "c1", Path: "/r/c1", Kind: "registered", Origin: OriginCreated}}}
	prov := Provider{ID: "dgh", Kind: "github"}
	if code := remoteRegistryCode(reg, remoteReq{attach: true, id: "c1", provider: "dgh", remotePath: "alice/x"}, prov, "alice/x", ""); code != "already_linked" {
		t.Fatalf("attach on a created row: %q", code)
	}
	// loading a registry file with such a row fails closed (validateRegistry)
	bad := emptyRegistry()
	bad.Projects = []Project{{ID: "c1", Path: "/r/c1", Kind: "registered", Origin: OriginCreated}}
	if validateRegistry(&bad) == nil {
		t.Fatal("validateRegistry accepted created without provider")
	}
}
