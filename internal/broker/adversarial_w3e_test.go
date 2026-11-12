package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigate/internal/registrymsg"
)

// W3e (E6) adversarial suite for the broker, GitHub side. Same rule as W3c:
// the attacker's move is refused and nothing moved (no provider request, no
// registry change, no burnt request ID unless the signature was valid), or the
// move is harmless. The tests drive the REAL handlers over the fake GitHub of
// the E3 kit; the assertions are on whole flows, and the allowlist below is
// written out here again on purpose (it must not be derived from production).

const wantGHAuth = "Bearer " + ghCanary

// ghFlow is one scenario of the fake GitHub (provider "gh"); alice/Existing
// exists at GitHub, team/exist at GitLab (e.existing()).
type ghFlow struct {
	name   string
	inject func(e *e3env)
	run    func(e *e3env) []string
}

func ghFlows() []ghFlow {
	none := func(*e3env) {}
	create := func(owner string, vis any, mod func(map[string]any)) func(*e3env) []string {
		return func(e *e3env) []string {
			return []string{e.rawWire("create_project", e.ghCreate(c3Name, owner, vis, mod))}
		}
	}
	link := func(remote string) func(*e3env) []string {
		return func(e *e3env) []string {
			return []string{e.rawWire("link_project", e.linkArgs("linked", "gh", remote, false, nil))}
		}
	}
	attach := func(remote string) func(*e3env) []string {
		return func(e *e3env) []string {
			e.ownLocal("plain", false, nil)
			return []string{e.rawWire("attach_remote", e.attachArgs("plain", "gh", remote, nil))}
		}
	}
	return []ghFlow{
		{"read ops", none, func(e *e3env) []string {
			return []string{e.rawWire("providers", nil), e.rawWire("list_groups", map[string]any{"provider": "gh"}),
				e.rawWire("projects", nil), e.rawWire("status", nil)}
		}},
		{"owners refused", func(e *e3env) { e.gh.fail["GET /user"] = 401 }, func(e *e3env) []string {
			return []string{e.rawWire("list_groups", map[string]any{"provider": "gh"})}
		}},
		{"owners 500", func(e *e3env) { e.gh.fail["GET /user/orgs"] = 500 }, func(e *e3env) []string {
			return []string{e.rawWire("list_groups", map[string]any{"provider": "gh"})}
		}},
		{"create_group github", none, func(e *e3env) []string {
			return []string{e.rawWire("create_group", e.signedArgs(e.alice, registrymsg.ActionCreateGroup, "gh", map[string]any{"name": "newg", "path": "newg", "parent_path": ""}))}
		}},
		{"set/delete github provider", none, func(e *e3env) []string {
			p, _ := e.s.Registry.ProviderByID("gh")
			f := map[string]any{"kind": "github", "base_url": p.BaseURL, "ssh_url": "ssh://git@github.com", "token_ref": "gh-tok"}
			return []string{e.rawWire("set_provider", e.signedArgs(e.alice, registrymsg.ActionSetProvider, "gh2", f)),
				e.rawWire("delete_provider", e.deleteProviderArgs("gh2"))}
		}},
		{"create private", none, create("alice", "private", nil)},
		{"create public", none, create("alice", "public", nil)},
		{"create under an organization", none, create("acme-org", "public", nil)},
		{"create name taken", func(e *e3env) { e.gh.repos["alice/demo"] = "alice/Demo" }, create("alice", "private", nil)},
		{"create owner unknown", none, create("nobody", "private", nil)},
		{"create refused 422", func(e *e3env) { e.gh.fail["POST /user/repos"] = 422 }, create("alice", "private", nil)},
		{"create 5xx", func(e *e3env) { e.gh.fail["POST /user/repos"] = 500 }, create("alice", "private", nil)},
		{"create rate limited", func(e *e3env) {
			e.gh.fail["POST /user/repos"] = 403
			e.gh.failHeader = map[string]string{"X-RateLimit-Remaining": "0"}
		}, create("alice", "private", nil)},
		{"create first file", func(e *e3env) { e.gh.fail["PUT /repos/alice/demo/contents/README.md"] = 403 }, create("alice", "private", nil)},
		{"create later file", func(e *e3env) { e.gh.fail["PUT /repos/alice/demo/contents/.aigate.yml"] = 500 }, create("alice", "private", nil)},
		{"create default branch", func(e *e3env) { e.gh.defBranch = "master" }, create("alice", "private", nil)},
		{"create weaker rule", func(e *e3env) { e.gh.protection = ghWeakProtection }, create("alice", "public", nil)},
		{"create paid plan", func(e *e3env) { e.gh.fail["PUT /repos/alice/demo/branches/main/protection"] = 403 }, create("alice", "private", nil)},
		{"create deploy key", func(e *e3env) { e.gh.fail["POST /repos/alice/demo/keys"] = 422 }, create("alice", "private", nil)},
		{"create owner lookup", func(e *e3env) { e.gh.fail["GET /user"] = 403 }, create("alice", "private", nil)},
		{"description is only data", none, create("alice", "private", func(f map[string]any) {
			f["description"] = `"},"private":false,"name":"x","owner":"victim","x":"`
		})},
		{"link ok", none, link("alice/existing")},
		{"link missing", func(e *e3env) { e.gh.repos = map[string]string{} }, link("alice/existing")},
		{"link read failure", func(e *e3env) { e.gh.fail["GET /repos/alice/existing"] = 500 }, link("alice/existing")},
		{"link key refused", func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 422 }, link("alice/existing")},
		{"link key 5xx", func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 502 }, link("alice/existing")},
		{"attach ok", none, attach("alice/existing")},
		{"attach key refused", func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 422 }, attach("alice/existing")},
		{"attach key 5xx", func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 500 }, attach("alice/existing")},
		{"attach rate limited", func(e *e3env) { e.gh.fail["GET /repos/alice/existing"] = 429 }, attach("alice/existing")},
		{"link gitlab ok", none, func(e *e3env) []string {
			return []string{e.rawWire("link_project", e.linkArgs("exist", "gl", "team/exist", false, nil))}
		}},
	}
}

func scanGH(leaks *[]string, what, s string) {
	if strings.Contains(s, ghCanary) || strings.Contains(s, c3Canary) || strings.Contains(s, "REFLECT") {
		*leaks = append(*leaks, what)
	}
}

// ---------- 1 ----------

// The GitHub token has exactly one way out of the broker: the Authorization
// header of a request to its own provider's address. It is in no reply (success
// or failure), no log line, no state file except the token store, no request
// line or body, no other header, not after a panic in the registry write of the
// create, link or attach path, and GitHub's own error text (which here reflects
// the token) is never forwarded. The GitLab token never goes to GitHub either.
func TestGitHubTokenNeverLeavesBroker(t *testing.T) {
	var leaks []string
	ghRequests := 0
	for _, fl := range ghFlows() {
		e := newE3(t)
		e.existing()
		fl.inject(e)
		replies := fl.run(e)
		for i, r := range replies {
			scanGH(&leaks, fmt.Sprintf("%s reply %d", fl.name, i), r)
		}
		scanGH(&leaks, fl.name+" log", e.logs.String())
		_ = filepath.Walk(e.e.Dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || strings.Contains(p, string(filepath.Separator)+"secrets"+string(filepath.Separator)) {
				return nil
			}
			b, _ := os.ReadFile(p)
			scanGH(&leaks, fl.name+" file "+filepath.Base(p), string(b))
			return nil
		})
		e.gh.mu.Lock()
		for i, rq := range e.gh.reqs {
			ghRequests++
			scanGH(&leaks, fl.name+" request line", rq)
			scanGH(&leaks, fl.name+" request body", e.gh.bodies[i])
			for k, vs := range e.gh.headers[i] {
				for _, v := range vs {
					switch {
					case k == "Authorization":
						if v != wantGHAuth {
							t.Errorf("%s: Authorization carries %q", fl.name, v)
						}
					case strings.Contains(v, ghCanary) || strings.Contains(v, c3Canary):
						leaks = append(leaks, fl.name+" header "+k)
					}
				}
			}
			if len(e.gh.headers[i].Values("Authorization")) != 1 {
				t.Errorf("%s: request without exactly one Authorization header", fl.name)
			}
		}
		e.gh.mu.Unlock()
		for _, r := range e.gl.all() {
			scanGH(&leaks, fl.name+" gitlab request", r.URI+r.Body)
		}
	}
	if ghRequests < 60 {
		t.Fatalf("control: only %d GitHub requests were observed", ghRequests)
	}
	// a panic in the registry write of each registering path (the panic value is the token)
	for _, op := range []string{"create_project", "link_project", "attach_remote"} {
		e := newE3(t)
		e.existing()
		var args map[string]any
		switch op {
		case "create_project":
			args = e.ghCreate(c3Name, "alice", "private", nil)
		case "link_project":
			args = e.linkArgs("linked", "gh", "alice/existing", false, nil)
		default:
			e.ownLocal("plain", false, nil)
			args = e.attachArgs("plain", "gh", "alice/existing", nil)
		}
		n := 0
		e.s.Registry.opts.failAt = func(stage string) error {
			if stage == "temp-written" {
				if n++; n == 1 {
					panic(ghCanary)
				}
			}
			return nil
		}
		out := e.rawWire(op, args)
		if !strings.Contains(out, "_partial") {
			t.Fatalf("%s panic reply: %s", op, out)
		}
		scanGH(&leaks, op+" panic reply", out)
		scanGH(&leaks, op+" panic log", e.logs.String())
		scanGH(&leaks, op+" panic registry", string(e.regFile()))
	}
	e := newE3(t)
	for _, raw := range []string{`{"v":1,"op":"link_project","args":` + ghCanary + `}`, `{"v":1,"op":"` + ghCanary + `"}`, `not json ` + ghCanary} {
		scanGH(&leaks, "malformed reply", string(roundTrip(t, e.sock, []byte(raw+"\n"))))
	}
	scanGH(&leaks, "log after malformed", e.logs.String())
	if len(leaks) != 0 {
		t.Fatalf("a token (or text reflected by GitHub) left the broker: %v", leaks)
	}
}

// ---------- 2 ----------

type ghAllowRow struct {
	method string
	path   *regexp.Regexp
	query  []string
	body   []string
}

const reRepoSeg = `[A-Za-z0-9._-]+`

var ghWireAllow = []ghAllowRow{
	{"GET", regexp.MustCompile(`^/user$`), nil, nil},
	{"GET", regexp.MustCompile(`^/user/orgs$`), []string{"page", "per_page"}, nil},
	{"GET", regexp.MustCompile(`^/repos/` + reRepoSeg + `/` + reRepoSeg + `$`), nil, nil},
	{"POST", regexp.MustCompile(`^/user/repos$`), nil, []string{"name", "private", "auto_init", "description"}},
	{"POST", regexp.MustCompile(`^/orgs/[A-Za-z0-9_-]+/repos$`), nil, []string{"name", "private", "auto_init", "description"}},
	{"PUT", regexp.MustCompile(`^/repos/` + reRepoSeg + `/` + reRepoSeg + `/contents/[A-Za-z0-9._/-]+$`), nil, []string{"message", "content", "branch"}},
	{"GET", regexp.MustCompile(`^/repos/` + reRepoSeg + `/` + reRepoSeg + `/branches/main/protection$`), nil, nil},
	{"PUT", regexp.MustCompile(`^/repos/` + reRepoSeg + `/` + reRepoSeg + `/branches/main/protection$`), nil,
		[]string{"required_status_checks", "enforce_admins", "required_pull_request_reviews", "restrictions", "allow_force_pushes", "allow_deletions"}},
	{"POST", regexp.MustCompile(`^/repos/` + reRepoSeg + `/` + reRepoSeg + `/keys$`), nil, []string{"title", "key", "read_only"}},
}

// checkGitHubTrace validates every request one fake GitHub saw against the
// table above and the flow's own rules, and marks the rows used.
func checkGitHubTrace(t *testing.T, name string, e *e3env, used map[int]bool) {
	t.Helper()
	e.gh.mu.Lock()
	reqs := append([]string(nil), e.gh.reqs...)
	bodies := append([]string(nil), e.gh.bodies...)
	e.gh.mu.Unlock()
	createdHere := map[string]bool{}
	for i, r := range reqs {
		method, uri, _ := strings.Cut(r, " ")
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatalf("%s: %q: %v", name, r, err)
		}
		row := -1
		for j, a := range ghWireAllow {
			if a.method == method && a.path.MatchString(u.Path) {
				row = j
			}
		}
		if row < 0 {
			t.Errorf("%s: request outside the allowlist reached GitHub: %s", name, r)
			continue
		}
		used[row] = true
		for k := range u.Query() {
			ok := false
			for _, a := range ghWireAllow[row].query {
				ok = ok || a == k
			}
			if !ok {
				t.Errorf("%s: query key %q on %s", name, k, r)
			}
		}
		if method != "GET" && method != "POST" && method != "PUT" {
			t.Errorf("%s: method %s", name, method)
		}
		var body map[string]any
		if bodies[i] != "" {
			if err := json.Unmarshal([]byte(bodies[i]), &body); err != nil {
				t.Errorf("%s: body of %s is not JSON: %v", name, r, err)
			}
		}
		if method == "GET" && bodies[i] != "" {
			t.Errorf("%s: GET with a body: %s", name, r)
		}
		for k := range body {
			ok := false
			for _, a := range ghWireAllow[row].body {
				ok = ok || a == k
			}
			if !ok {
				t.Errorf("%s: body key %q on %s", name, k, r)
			}
		}
		segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		switch {
		case method == "POST" && segs[0] == "user":
			createdHere[strings.ToLower(e.gh.userLogin()+"/"+fmt.Sprint(body["name"]))] = true
			if body["auto_init"] != false {
				t.Errorf("%s: auto_init %v", name, body["auto_init"])
			}
		case method == "POST" && segs[0] == "orgs":
			createdHere[strings.ToLower(segs[1]+"/"+fmt.Sprint(body["name"]))] = true
		case segs[0] == "repos" && method == "PUT":
			// the contents PUTs and the protection PUT only ever go to a repository created by THIS flow
			if !createdHere[strings.ToLower(segs[1]+"/"+segs[2])] {
				t.Errorf("%s: %s to a repository this flow did not create", name, r)
			}
			if len(segs) >= 5 && segs[3] == "contents" {
				if _, has := body["sha"]; has {
					t.Errorf("%s: a file update (sha) was sent", name)
				}
			}
		case segs[0] == "repos" && method == "POST" && segs[3] == "keys":
			if _, ok := body["read_only"].(bool); !ok {
				t.Errorf("%s: deploy key without a boolean read_only", name)
			}
			if title, _ := body["title"].(string); !strings.HasPrefix(title, "orchd ") {
				t.Errorf("%s: deploy key title %q", name, title)
			}
		}
		// nothing beyond the repository itself except the four documented sub-resources
		if segs[0] == "repos" && len(segs) > 3 {
			switch segs[3] {
			case "contents", "branches", "keys":
			default:
				t.Errorf("%s: sub-resource %q", name, segs[3])
			}
		}
		if u.Path == "/user/keys" || strings.HasPrefix(u.Path, "/user/") && u.Path != "/user/orgs" && u.Path != "/user/repos" {
			t.Errorf("%s: user-level endpoint %s", name, u.Path)
		}
		for _, bad := range []string{"collaborators", "invitations", "/merge", "/pulls", "/forks", "/hooks", "/transfer", "/rulesets", "/actions", "authorizations", "/git/", "/settings"} {
			if len(segs) > 3 && strings.Contains(strings.ToLower("/"+strings.Join(segs[3:], "/")), bad) {
				t.Errorf("%s: forbidden path piece %q in %s", name, bad, r)
			}
		}
	}
}

// Every request any GitHub flow sends (all scenarios plus hostile names in
// every shape that reaches the client), collected over the fake, is in the nine
// documented rows, carries only the documented query and body keys, never
// deletes, patches, merges or touches the token's own keys or a repository's
// collaborators, PUTs only into a repository created in the same flow, and
// every row is reached by some flow (the allowlist holds nothing unused).
func TestEveryGitHubRequestIsAllowlisted(t *testing.T) {
	used := map[int]bool{}
	total := 0
	for _, fl := range ghFlows() {
		e := newE3(t)
		e.existing()
		fl.inject(e)
		fl.run(e)
		total += e.gh.hits()
		checkGitHubTrace(t, fl.name, e, used)
	}
	// hostile names: signed over good fields, the hostile value swapped into the
	// request (so the shape/signature layers must stop it), and the few that are
	// legal repository names (those only ever become GET /repos/{o}/{r})
	e := newE3(t)
	e.existing()
	hostile := []string{
		"alice/existing/collaborators", "alice/existing/keys", "alice/existing%2Fkeys", "alice/existing?x=1", "alice/existing#f",
		"../user/keys", "alice/../user/keys", `alice\existing`, "alice/existing\n", "alice/existing.git", "alice//existing", "/alice/existing",
		"alice/ex isting", "alice/existing/../../user/keys", "https://evil.example/alice/existing", "alice/existing/branches/main/protection",
		"alice/existing/contents/x", strings.Repeat("a", 300) + "/x",
	}
	before := e.gh.hits()
	e.ownLocal("plain", false, nil)
	for _, h := range hostile {
		good := e.linkArgs("hostile", "gh", "alice/existing", false, nil)
		good["remote_path"] = h
		if _, oe := e.do("link_project", good); oe == nil {
			t.Errorf("link with remote_path %q succeeded", h)
		}
		att := e.attachArgs("plain", "gh", "alice/existing", nil)
		att["remote_path"] = h
		if _, oe := e.do("attach_remote", att); oe == nil {
			t.Errorf("attach with remote_path %q succeeded", h)
		}
	}
	for _, h := range []string{"alice/x/keys", "../user", "alice/..", "al ice", "alice%2Fx", "alice\n", "", "a/b", strings.Repeat("a", 40), "-alice", "alice-"} {
		a := e.ghCreate(c3Name, "alice", "private", nil)
		a["group_path"] = h
		if _, oe := e.do("create_project", a); oe == nil {
			t.Errorf("create with owner %q succeeded", h)
		}
	}
	if n := e.gh.hits() - before; n != 0 {
		t.Fatalf("hostile names reached GitHub %d times: %v", n, e.trace())
	}
	// legal-looking names that spell endpoints: still only GET /repos/{o}/{r}
	for _, rp := range []string{"user/keys", "alice/collaborators", "alice/keys", "orgs/repos"} {
		e.gh.repos[strings.ToLower(rp)] = rp
		e.do("link_project", e.linkArgs(strings.Split(rp, "/")[1], "gh", rp, false, nil))
	}
	checkGitHubTrace(t, "legal-looking names", e, used)
	for i, a := range ghWireAllow {
		if !used[i] {
			t.Errorf("row %d (%s %s) was never reached by any flow", i, a.method, a.path)
		}
	}
	if total < 80 {
		t.Fatalf("control: %d requests observed", total)
	}
	// a direct sender probe: the one send path refuses destructive and token-shaped requests
	// before any network access (public methods cannot form them, so only this catches a
	// missing check in `do` itself)
	probe := newFakeGH(t)
	pa := ghFor(t, probe, ghTokens(t), nil)
	for _, c := range []struct{ m, p string }{
		{"DELETE", "/repos/alice/demo"}, {"PATCH", "/repos/alice/demo"}, {"DELETE", "/repos/alice/demo/keys/5"}, {"PUT", "/repos/alice/demo/pulls/1/merge"},
		{"POST", "/repos/alice/demo/merges"}, {"GET", "/user/keys"}, {"POST", "/user/keys"}, {"GET", "/repos/alice/demo/collaborators"},
		{"PUT", "/repos/alice/demo/collaborators/bob"}, {"POST", "/repos/alice/demo/forks"}, {"POST", "/authorizations"}, {"GET", "/repos/alice/demo/branches/main/protection/enforce_admins"},
	} {
		if _, err := pa.do(context.Background(), c.m, c.p, nil, nil, 200); err == nil {
			t.Errorf("%s %s was sent", c.m, c.p)
		}
	}
	if probe.hits() != 0 {
		t.Fatalf("the direct probe reached the network %d times: %v", probe.hits(), probe.recorded())
	}
	// the production code spells no destructive verb or destructive endpoint
	for _, f := range []string{"githubapi.go", "create_project.go", "link_project.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"MethodDelete", "MethodPatch", `"DELETE"`, `"PATCH"`, "/user/keys", "collaborators"} {
			if bytes.Contains(src, []byte(bad)) {
				t.Errorf("%s contains %q", f, bad)
			}
		}
	}
}

// ---------- 3 ----------

// A GitHub provider's address is as constrained as GitLab's, and the web host
// is refused outright: github.com / www.github.com as base_url (the API lives
// at api.github.com), loopback / link-local / metadata / numeric / userinfo
// forms, plain http to a public host, and names that resolve to forbidden space
// at set time or at connect time. Nothing is reached, the registry never moves.
func TestGitHubProviderAddressesRefused(t *testing.T) {
	var trapHits atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { trapHits.Add(1) }))
	t.Cleanup(trap.Close)
	var answer atomic.Value
	answer.Store([]netip.Addr{netip.MustParseAddr("140.82.112.6")})
	w := newWEnvMod(t, func(s *Server) {
		s.apiSeams = &apiSeams{lookup: func(context.Context, string) ([]netip.Addr, error) { return answer.Load().([]netip.Addr), nil }}
	})
	if err := w.s.Tokens.Set("gh-tok", "github", "", ghCanary, tokNow); err != nil {
		t.Fatal(err)
	}
	signable := 0
	set := func(id, base, ssh string) *OpError {
		f := map[string]any{"kind": "github", "base_url": base, "ssh_url": ssh, "token_ref": "gh-tok"}
		if _, err := registrymsg.ChangeHash(registrymsg.ActionSetProvider, id, f); err != nil {
			good := map[string]any{"kind": "github", "base_url": "https://api.github.com", "ssh_url": "ssh://git@github.com", "token_ref": "gh-tok"}
			a := w.signedArgs(w.alice, registrymsg.ActionSetProvider, id, good)
			a["base_url"], a["ssh_url"] = base, ssh
			if _, e := w.do("set_provider", a); e == nil {
				return nil
			}
			return &OpError{Code: "bad_url"}
		}
		signable++
		_, e := w.do("set_provider", w.signedArgs(w.alice, registrymsg.ActionSetProvider, id, f))
		return e
	}
	before := w.snap()
	bad := []string{
		"https://github.com", "https://www.github.com", "https://GITHUB.COM", "https://github.com:443", "https://github.com/", "https://github.com/api/v3",
		"https://www.github.com:443", "http://github.com", "http://api.github.com", "http://140.82.112.6", "https://api.github.com/", "https://api.github.com/api/v3",
		"https://user:pw@api.github.com", "https://api.github.com?x=1", "https://api.github.com#f", "https://api.github.com:0", "https://api.github.com:99999",
		"https://API.GITHUB.COM", "https://api.github.com.", trap.URL, "https://127.0.0.1", "http://127.0.0.1:8080", "https://[::1]", "https://0.0.0.0",
		"http://localhost", "https://169.254.169.254", "http://169.254.169.254/latest", "https://[fe80::1]", "https://[::ffff:127.0.0.1]",
		"https://100.100.100.200", "https://2130706433", "https://0x7f000001", "https://0177.0.0.1", "https://127.1", "https://224.0.0.1",
		"ftp://api.github.com", "file:///etc/passwd", "javascript:alert(1)", "https://", "", " https://api.github.com", "https://api.github.com\n",
	}
	for _, b := range bad {
		if e := set("gh9", b, "ssh://git@github.com"); e == nil {
			t.Errorf("base_url %q was accepted", b)
		} else if e.Code != "bad_url" && e.Code != "bad_request" {
			t.Errorf("base_url %q: code %s", b, e.Code)
		}
	}
	for _, s := range []string{"ssh://git@127.0.0.1", "ssh://git@localhost", "ssh://git@169.254.169.254", "ssh://root@github.com", "ssh://git@github.com/o/r",
		"ssh://git@github.com/", "ssh://-oProxyCommand=x@github.com", "ssh://git@-oProxyCommand=x", "ssh://git@github.com:22/x", "ssh://git:pw@github.com",
		"https://github.com", "git@github.com:o/r.git", "ext::sh -c id", "ssh://git@github.com -oProxyCommand=x", "ssh://git@2130706433"} {
		if e := set("gh9", "https://api.github.com", s); e == nil {
			t.Errorf("ssh_url %q was accepted", s)
		} else if e.Code != "bad_url" && e.Code != "bad_request" {
			t.Errorf("ssh_url %q: code %s", s, e.Code)
		}
	}
	w.mustEqual(before, "refused github provider addresses")
	if signable < 15 {
		t.Fatalf("control: only %d hostile addresses were signable", signable)
	}
	// api.github.com resolving to forbidden space is refused at set time
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "0.0.0.0", "::ffff:127.0.0.1", "100.100.100.200"} {
		answer.Store([]netip.Addr{netip.MustParseAddr(ip)})
		if e := set("gh9", "https://api.github.com", "ssh://git@github.com"); codeOf(e) != "bad_url" {
			t.Errorf("api.github.com resolving to %s: %v", ip, codeOf(e))
		}
	}
	if !bytes.Equal(before.reg, w.regFile()) {
		t.Fatal("the registry changed")
	}
	// control: the public address of api.github.com is accepted (so the refusals above are for their own reason) ...
	answer.Store([]netip.Addr{netip.MustParseAddr("140.82.112.6")})
	if e := set("gh9", "https://api.github.com", "ssh://git@github.com"); e != nil {
		t.Fatalf("control: %v", e.Code)
	}
	// ... and having passed once, a rebind to forbidden space at connect time is refused
	for _, ips := range [][]string{{"127.0.0.1"}, {"169.254.169.254", "127.0.0.1"}, {"::ffff:127.0.0.1"}} {
		var as []netip.Addr
		for _, ip := range ips {
			as = append(as, netip.MustParseAddr(ip))
		}
		answer.Store(as)
		_, e := w.do("list_groups", map[string]any{"provider": "gh9"})
		if e == nil || (e.Code != "bad_url" && e.Code != "provider_unreachable") {
			t.Errorf("rebinding to %v: %v", ips, e)
		}
	}
	if trapHits.Load() != 0 {
		t.Fatalf("the trap server saw %d connection(s)", trapHits.Load())
	}
}

// A GitHub provider that answers with a redirect (any status, on every request
// or only on writes) to another host is not followed: that host sees no
// request, hence no token. And a second provider's token never reaches the
// first provider's host, nor the other way round.
func TestGitHubRedirectNotFollowedAndTokenNotSentToOtherHost(t *testing.T) {
	var other atomic.Int32
	var otherAuth atomic.Value
	otherAuth.Store("")
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		otherAuth.Store(r.Header.Get("Authorization"))
	}))
	t.Cleanup(b.Close)
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, onlyWrites := range []bool{false, true} {
			e := newE3(t)
			e.existing()
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if onlyWrites && r.Method == "GET" {
					r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v3")
					r.RequestURI = strings.TrimPrefix(r.RequestURI, "/api/v3")
					e.gh.serve(w, r)
					return
				}
				http.Redirect(w, r, b.URL+r.URL.RequestURI(), status)
			}))
			if err := e.s.AddProviderChecked(Provider{ID: "ghr", Kind: "github", BaseURL: a.URL, SSHURL: "ssh://git@github.com", TokenRef: "gh-tok"}); err != nil {
				t.Fatal(err)
			}
			e.ownLocal("plain", false, nil)
			toR := func(f map[string]any) { f["provider"] = "ghr" }
			replies := []string{
				e.rawWire("list_groups", map[string]any{"provider": "ghr"}),
				e.rawWire("create_project", e.ghCreate(c3Name, "alice", "private", toR)),
				e.rawWire("link_project", e.linkArgs("linked", "ghr", "alice/existing", false, nil)),
				e.rawWire("attach_remote", e.attachArgs("plain", "ghr", "alice/existing", nil)),
			}
			a.Close()
			for i, r := range replies {
				if strings.Contains(r, `"ok":true`) && !(onlyWrites && i == 0) {
					t.Errorf("status %d writes=%v: a redirected request succeeded: %s", status, onlyWrites, r)
				}
				if strings.Contains(r, ghCanary) || strings.Contains(r, b.URL) {
					t.Errorf("status %d: reply leaks: %s", status, r)
				}
			}
			for _, id := range []string{c3Name, "linked"} {
				if p, ok := e.find(id); ok && p.Provider != nil {
					t.Errorf("status %d: %s registered through a redirect", status, id)
				}
			}
			if p, _ := e.find("plain"); p.Provider != nil {
				t.Errorf("status %d: attach succeeded through a redirect", status)
			}
		}
	}
	if n := other.Load(); n != 0 {
		t.Fatalf("the redirect target received %d request(s); Authorization %q", n, otherAuth.Load())
	}
	// two providers, two tokens: each host only sees its own
	e := newE3(t)
	if err := e.s.Tokens.Set("gh3-tok", "github", "", "ghp_second-NOT-A-REAL-TOKEN", tokNow); err != nil {
		t.Fatal(err)
	}
	var secondSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondSeen = append(secondSeen, r.Header.Get("Authorization"))
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	if err := e.s.AddProviderChecked(Provider{ID: "gh3", Kind: "github", BaseURL: srv.URL, SSHURL: "ssh://git@github.com", TokenRef: "gh3-tok"}); err != nil {
		t.Fatal(err)
	}
	e.rawWire("list_groups", map[string]any{"provider": "gh3"})
	e.rawWire("list_groups", map[string]any{"provider": "gh"})
	if len(secondSeen) == 0 {
		t.Fatal("control: the second provider was not contacted")
	}
	for _, a := range secondSeen {
		if a != "Bearer ghp_second-NOT-A-REAL-TOKEN" {
			t.Fatalf("the second host saw %q", a)
		}
	}
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	if len(e.gh.headers) == 0 {
		t.Fatal("control: the first provider was not contacted")
	}
	for _, h := range e.gh.headers {
		if h.Get("Authorization") != wantGHAuth {
			t.Fatalf("the first host saw %q", h.Get("Authorization"))
		}
	}
}

// ---------- 4 ----------

// The owner is never trusted from the signed string: the repository is created
// under whatever GitHub's own owner list says, in GitHub's spelling; a string
// that matches neither the account nor an organization (however valid its
// shape) reaches no write; strings that could smuggle another path are stopped
// in the shape stage before anything is sent.
func TestGitHubOwnerIsNeverTrustedFromTheSignedString(t *testing.T) {
	// a stranger's login: refused, GitHub saw reads of the owner lists only
	e := newE3(t)
	_, oe := e.do("create_project", e.ghCreate(c3Name, "victim", "private", nil))
	if codeOf(oe) != "group_unknown" {
		t.Fatalf("stranger: %v", codeOf(oe))
	}
	if len(e.ghWrites()) != 0 {
		t.Fatalf("a write reached GitHub: %v", e.trace())
	}
	for _, r := range e.trace() {
		if r != "GET /user" && r != "GET /user/orgs" {
			t.Fatalf("unexpected request %s", r)
		}
	}
	// the account's login with other capitals: GitHub's spelling is used on the wire and in the registry
	e = newE3(t)
	e.gh.login = "AliceCase"
	if _, oe := e.do("create_project", e.ghCreate(c3Name, "alicecase", "private", nil)); oe != nil {
		t.Fatalf("own account: %v", oe.Code)
	}
	if got := e.ghWrites(); len(got) == 0 || got[0] != "POST /user/repos" {
		t.Fatalf("writes %v", got)
	}
	for _, r := range e.ghWrites() {
		if !strings.Contains(r, "/AliceCase/demo/") && r != "POST /user/repos" {
			t.Errorf("write not under GitHub's spelling: %s", r)
		}
	}
	if p, ok := e.find(c3Name); !ok || p.Remote == nil || p.Remote.Path != "AliceCase/demo" {
		t.Fatalf("registry spelling: %+v", p)
	}
	// an organization from the list, signed in another case
	e = newE3(t)
	if _, oe := e.do("create_project", e.ghCreate(c3Name, "ACME-org", "public", nil)); oe != nil {
		t.Fatalf("org: %v", oe.Code)
	}
	if got := e.ghWrites(); len(got) == 0 || got[0] != "POST /orgs/Acme-Org/repos" {
		t.Fatalf("org writes %v", got)
	}
	if p, ok := e.find(c3Name); !ok || p.Remote.Path != "Acme-Org/demo" {
		t.Fatalf("org registry spelling: %+v", p)
	}
	// strings that are not one login never get past the shape stage (no ID burnt, no request)
	e = newE3(t)
	before := e.snap()
	for _, o := range []string{"alice/demo", "alice%2Fdemo", "../../user", "alice\n", "al ice", "bad.login", "-alice", "alice-", strings.Repeat("a", 40), "", "alice?x=1"} {
		a := e.ghCreate(c3Name, "alice", "private", nil)
		a["group_path"] = o
		if _, oe := e.do("create_project", a); oe == nil {
			t.Errorf("owner %q accepted", o)
		}
	}
	e.mustEqual(before, "hostile owners")
	e.noGHHits("hostile owners")
}

// ---------- 5 ----------

// Names collide case-insensitively at GitHub: for create (Demo/demo, also under
// an organization) nothing is written; for link and attach the same repository
// (Foo/foo/FOO) can be linked once, under any spelling, and is stored in
// GitHub's spelling.
func TestGitHubCaseInsensitiveCollisionForCreateAndLink(t *testing.T) {
	for _, owner := range []string{"alice", "acme-org"} {
		e := newE3(t)
		e.gh.repos[owner+"/demo"] = strings.Replace(owner, "acme-org", "Acme-Org", 1) + "/Demo"
		before := e.snap()
		if _, oe := e.do("create_project", e.ghCreate(c3Name, owner, "private", nil)); codeOf(oe) != "project_name_taken" {
			t.Fatalf("%s: %v", owner, codeOf(oe))
		}
		if len(e.ghWrites()) != 0 {
			t.Fatalf("%s: writes %v", owner, e.ghWrites())
		}
		if string(e.regFile()) != string(before.reg) || len(e.orphans()) != 0 {
			t.Fatalf("%s: the registry or the orphan list changed", owner)
		}
	}
	attachN := 0
	// link: alice/Foo exists; the first link wins under whatever spelling, the second is already_linked
	for i, spelling := range []string{"alice/foo", "ALICE/FOO", "Alice/Foo", "alice/Foo"} {
		e := newE3(t)
		e.gh.repos["alice/foo"] = "alice/Foo"
		if _, oe := e.do("link_project", e.linkArgs("foo", "gh", spelling, false, nil)); oe != nil {
			t.Fatalf("spelling %d %q: %v", i, spelling, oe.Code)
		}
		if p, ok := e.find("foo"); !ok || p.Remote == nil || p.Remote.Path != "alice/Foo" {
			t.Fatalf("stored spelling: %+v", p)
		}
		for _, again := range []string{"alice/foo", "ALICE/FOO", "alice/Foo"} {
			hits := e.gh.hits()
			if _, oe := e.do("link_project", e.linkArgs("foo-two", "gh", again, false, nil)); codeOf(oe) != "already_linked" {
				t.Fatalf("second link %q: %v", again, codeOf(oe))
			}
			pid := fmt.Sprintf("pp-%d", attachN)
			attachN++
			e.ownLocal(pid, false, nil)
			if _, oe := e.do("attach_remote", e.attachArgs(pid, "gh", again, nil)); codeOf(oe) != "already_linked" {
				t.Fatalf("attach %q: %v", again, codeOf(oe))
			}
			if e.gh.hits() != hits {
				t.Fatalf("a repository linked elsewhere reached GitHub again: %v", e.trace())
			}
		}
	}
}

// ---------- 6 ----------

// Visibility has no default and is never forced: a GitHub create without the
// signed field is refused in the shape stage (zero requests, no burnt ID); the
// signed value is exactly what GitHub is asked for (public only when signed
// "public"); any other value, or one changed after signing, reaches nothing;
// GitLab never accepts the field and always asks for private.
func TestVisibilityNeverDefaultedOrForcedAndPublicOnlyWhenSigned(t *testing.T) {
	e := newE3(t)
	before := e.snap()
	// absent: refused, nothing sent, nothing burnt
	_, oe := e.do("create_project", e.ghCreate(c3Name, "alice", nil, nil))
	if codeOf(oe) != "visibility_required" {
		t.Fatalf("absent: %v", codeOf(oe))
	}
	e.mustEqual(before, "absent visibility")
	e.noGHHits("absent visibility")
	// values that are not exactly public/private: signed over a good value, the bad one swapped in
	for _, v := range []any{"Public", "PUBLIC", "internal", "", " public", "public\n", "private ", true, 1, nil, []string{"public"}, map[string]any{"a": 1}, "public,private"} {
		a := e.ghCreate(c3Name, "alice", "private", nil)
		a["visibility"] = v
		if _, oe := e.do("create_project", a); oe == nil || (oe.Code != "bad_request" && oe.Code != "visibility_required" && oe.Code != "change_mismatch") {
			t.Errorf("visibility %#v: %v", v, codeOf(oe))
		}
	}
	// changed after signing, either way, or added/removed
	for _, c := range []struct{ signed, sent any }{{"private", "public"}, {"public", "private"}, {"private", nil}, {nil, "public"}, {nil, "private"}} {
		a := e.ghCreate(c3Name, "alice", c.signed, nil)
		if c.sent == nil {
			delete(a, "visibility")
		} else {
			a["visibility"] = c.sent
		}
		if _, oe := e.do("create_project", a); oe == nil || (oe.Code != "change_mismatch" && oe.Code != "visibility_required") {
			t.Errorf("signed %v sent %v: %v", c.signed, c.sent, codeOf(oe))
		}
		// the attacker recomputes `change` for the swapped value: the signature no longer fits
		if c.sent != nil && c.signed != nil {
			f := map[string]any{"provider": "gh", "group_path": "alice", "template": "basic", "description": "demo project", "ai_context": false,
				"publish": "write", "deploy_key_sha256": c3Hash(c3Key()), "visibility": c.sent}
			h, _ := registrymsg.ChangeHash(registrymsg.ActionCreateProject, c3Name, f)
			a["change"] = h
			if _, oe := e.do("create_project", a); codeOf(oe) != "bad_signature" {
				t.Errorf("signed %v sent %v recomputed: %v", c.signed, c.sent, codeOf(oe))
			}
		}
	}
	e.mustEqual(before, "bad or swapped visibility")
	e.noGHHits("bad or swapped visibility")
	// GitLab: the field is forbidden for every value, nothing is sent
	for _, v := range []any{"public", "private", "internal", ""} {
		a := e.projectArgs(c3Name, nil)
		a["visibility"] = v
		if _, oe := e.do("create_project", a); oe == nil || (oe.Code != "visibility_not_allowed" && oe.Code != "bad_request") {
			t.Errorf("gitlab visibility %#v: %v", v, codeOf(oe))
		}
	}
	e.noGLHits("visibility on a GitLab create")
	// the signed value is what GitHub is asked for
	for _, c := range []struct {
		vis     string
		private bool
	}{{"public", false}, {"private", true}} {
		e := newE3(t)
		if _, oe := e.do("create_project", e.ghCreate(c3Name, "alice", c.vis, nil)); oe != nil {
			t.Fatalf("%s: %v", c.vis, oe.Code)
		}
		i := e.indexOf("POST /user/repos")
		if i < 0 {
			t.Fatalf("%s: no create request", c.vis)
		}
		if got := e.bodyAt(i)["private"]; got != c.private {
			t.Errorf("%s: GitHub was asked for private=%v", c.vis, got)
		}
	}
	// a public repository is never created by any other path
	e2 := newE3(t)
	e2.existing()
	e2.rawWire("link_project", e2.linkArgs("linked", "gh", "alice/existing", false, nil))
	e2.ownLocal("plain", false, nil)
	e2.rawWire("attach_remote", e2.attachArgs("plain", "gh", "alice/existing", nil))
	for _, r := range e2.trace() {
		if r == "POST /user/repos" || strings.HasPrefix(r, "POST /orgs/") {
			t.Fatalf("link/attach created a repository: %s", r)
		}
	}
	// GitLab always asks for a private project
	g := newC3(t)
	if _, oe := g.do("create_project", g.projectArgs(c3Name, nil)); oe != nil {
		t.Fatal(oe.Code)
	}
	for _, r := range g.gl.all() {
		if r.Method == "POST" && strings.HasSuffix(r.URI, "/api/v4/projects") {
			var m map[string]any
			json.Unmarshal([]byte(r.Body), &m)
			if m["visibility"] != "private" {
				t.Fatalf("GitLab was asked for %v", m["visibility"])
			}
		}
	}
}

// ---------- 7 ----------

type w3eAct struct {
	name string
	op   string
	args func(e *e3env) map[string]any
}

func w3eActs() []w3eAct {
	return []w3eAct{
		{"register_project", "register_project", func(e *e3env) map[string]any {
			return e.signedArgs(e.alice, registrymsg.ActionRegisterProject, "xproj", regFields(e.root+"/xproj", "default", true))
		}},
		{"set_flag", "set_flag", func(e *e3env) map[string]any {
			return e.signedArgs(e.alice, registrymsg.ActionSetFlag, "repo-a", flagFields("ai_context", false))
		}},
		{"set_provider gitlab", "set_provider", func(e *e3env) map[string]any {
			return e.signedArgs(e.alice, registrymsg.ActionSetProvider, "gl", e.providerFields(e.gl.URL))
		}},
		{"set_provider github", "set_provider", func(e *e3env) map[string]any {
			p, _ := e.s.Registry.ProviderByID("gh")
			return e.signedArgs(e.alice, registrymsg.ActionSetProvider, "gh", map[string]any{"kind": "github", "base_url": p.BaseURL, "ssh_url": "ssh://git@github.com", "token_ref": "gh-tok"})
		}},
		{"delete_provider", "delete_provider", func(e *e3env) map[string]any { return e.deleteProviderArgs("spare") }},
		{"create_group", "create_group", func(e *e3env) map[string]any { return e.groupArgs("newg", "newg", "") }},
		{"create_project gitlab", "create_project", func(e *e3env) map[string]any { return e.projectArgs(c3Name, nil) }},
		{"create_project github", "create_project", func(e *e3env) map[string]any { return e.ghCreate(c3Name, "alice", "private", nil) }},
		{"link_project", "link_project", func(e *e3env) map[string]any { return e.linkArgs("linked", "gh", "alice/existing", true, nil) }},
		{"attach_remote", "attach_remote", func(e *e3env) map[string]any { return e.attachArgs("plain", "gh", "alice/existing", nil) }},
		{"set_token", "set_token", func(e *e3env) map[string]any { return e.tokArgs(e.alice, "other-gitlab", "gitlab", "", tokenOK) }},
		{"delete_token", "delete_token", func(e *e3env) map[string]any { return e.delArgs(e.alice, "gl-admin") }},
	}
}

// A signature made for one action never works as another, over EVERY ordered
// pair of the signed actions of all waves (both create_project kinds and both
// set_provider kinds count as separate acts, so a GitLab signature cannot
// create a GitHub repository either): the signature alone, and together with
// its change, request ID and time, is grafted onto the other request. All are
// refused; no provider is contacted; the registry, token store and used-ID file
// are unchanged.
func TestCrossActionSignaturesRefusedForAllPairsW3e(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.withProvider("spare", e.gl.URL)
	e.ownLocal("plain", false, nil)
	acts := w3eActs()
	snapReg, snapTok := e.snap(), e.tokSnap()
	pairs := 0
	codes := map[string]int{}
	for ai, a := range acts {
		for bi, b := range acts {
			if ai == bi {
				continue
			}
			sigA := a.args(e)
			r1 := b.args(e)
			r1["signature"] = sigA["signature"]
			r2 := b.args(e)
			for _, k := range []string{"signature", "change", "request", "issued"} {
				r2[k] = sigA[k]
			}
			for i, r := range []map[string]any{r1, r2} {
				_, oe := e.do(b.op, r)
				if oe == nil {
					t.Errorf("%s signature accepted as %s (variant %d)", a.name, b.name, i+1)
					continue
				}
				codes[oe.Code]++
				switch oe.Code {
				case "bad_signature", "change_mismatch", "bad_request", "deploy_key_mismatch":
				default:
					t.Errorf("%s signature on %s (variant %d): code %s", a.name, b.name, i+1, oe.Code)
				}
			}
			pairs++
		}
	}
	if pairs != len(acts)*(len(acts)-1) || pairs < 120 {
		t.Fatalf("pairs %d", pairs)
	}
	e.mustEqual(snapReg, "cross-action signatures")
	if !e.tokSnap().sameStore(snapTok) || !e.tokSnap().sameUsed(snapTok) {
		t.Fatal("the token store or the used-ID file changed")
	}
	e.noGHHits("cross-action signatures")
	e.noGLHits("cross-action signatures")
	if codes["bad_signature"] == 0 || codes["change_mismatch"] == 0 {
		t.Fatalf("control: refusal codes %v (the signature layer was never what refused)", codes)
	}
	// control: each action works with its own signature (the matrix is not refusing everything)
	for _, name := range []string{"create_project github", "link_project"} {
		for _, a := range acts {
			if a.name == name {
				if _, oe := e.do(a.op, a.args(e)); oe != nil {
					t.Fatalf("control %s: %v", name, oe.Code)
				}
			}
		}
	}
}

// ---------- 8 ----------

// A valid signature cannot be reused with any single field of a GitHub create,
// link or attach request altered (owner, remote path, visibility, publish,
// ai_context, template, description, provider, deploy-key hash, the subject):
// the broker rebuilds the change and refuses the difference; with the change
// recomputed by the attacker the signature no longer verifies. No GitHub (or
// GitLab) request is made and the registry does not change.
func TestSignatureOverDifferentGitHubFieldsRefused(t *testing.T) {
	type op struct {
		name   string
		op     string
		action string
		mk     func(e *e3env) map[string]any
		alter  map[string]any
		sign   []string // the fields in the signed change
	}
	ops := []op{
		{"create_project", "create_project", registrymsg.ActionCreateProject, func(e *e3env) map[string]any { return e.ghCreate(c3Name, "alice", "private", nil) },
			map[string]any{"group_path": "acme-org", "visibility": "public", "publish": "read", "ai_context": true, "template": "go",
				"description": "other text", "provider": "gl", "deploy_key_sha256": strings.Repeat("c", 64)},
			[]string{"provider", "group_path", "template", "description", "ai_context", "publish", "deploy_key_sha256", "visibility"}},
		{"link_project", "link_project", registrymsg.ActionLinkProject, func(e *e3env) map[string]any { return e.linkArgs("linked", "gh", "alice/existing", false, nil) },
			map[string]any{"remote_path": "alice/other", "publish": "read", "ai_context": true, "provider": "gl", "deploy_key_sha256": strings.Repeat("c", 64)},
			[]string{"provider", "remote_path", "ai_context", "publish", "deploy_key_sha256"}},
		{"attach_remote", "attach_remote", registrymsg.ActionAttachRemote, func(e *e3env) map[string]any {
			return e.attachArgs("plain", "gh", "alice/existing", nil)
		},
			map[string]any{"remote_path": "alice/other", "publish": "read", "provider": "gl", "deploy_key_sha256": strings.Repeat("c", 64)},
			[]string{"provider", "remote_path", "publish", "deploy_key_sha256"}},
	}
	for _, o := range ops {
		e := newE3(t)
		e.existing()
		e.gh.repos["alice/other"] = "alice/Other"
		e.ownLocal("plain", false, nil)
		before := e.snap()
		for field, val := range o.alter {
			a := o.mk(e)
			a[field] = val
			_, oe := e.do(o.op, a)
			if c := codeOf(oe); c != "change_mismatch" && c != "deploy_key_mismatch" && c != "bad_request" && c != "provider_unknown" && c != "visibility_not_allowed" {
				t.Errorf("%s: field %s altered: code %q", o.name, field, c)
			}
			// the attacker also fixes `change`: the signature is then wrong
			a2 := o.mk(e)
			a2[field] = val
			fields := map[string]any{}
			for _, k := range o.sign {
				if v, ok := a2[k]; ok {
					fields[k] = v
				}
			}
			subject := a2["id"].(string)
			if h, err := registrymsg.ChangeHash(o.action, subject, fields); err == nil {
				a2["change"] = h
				if c := codeOf(func() *OpError { _, oe := e.do(o.op, a2); return oe }()); c != "bad_signature" && c != "deploy_key_mismatch" && c != "bad_request" && c != "provider_unknown" && c != "visibility_not_allowed" {
					t.Errorf("%s: field %s altered with a recomputed change: code %q", o.name, field, c)
				}
			}
		}
		// the subject is inside the signed message itself
		a := o.mk(e)
		a["id"] = "other-name"
		if c := codeOf(func() *OpError { _, oe := e.do(o.op, a); return oe }()); c != "bad_signature" && c != "change_mismatch" && c != "project_unknown" && c != "bad_request" {
			t.Errorf("%s: subject altered: %q", o.name, c)
		}
		e.mustEqual(before, o.name+" altered fields")
		e.noGHHits(o.name + " altered fields")
		e.noGLHits(o.name + " altered fields")
		// control: the unaltered request goes through, once
		good := o.mk(e)
		if _, oe := e.do(o.op, good); oe != nil {
			t.Fatalf("%s control: %v", o.name, oe.Code)
		}
		if _, oe := e.do(o.op, good); oe == nil {
			t.Fatalf("%s: a used signature was accepted again", o.name)
		}
	}
}

// ---------- 9 ----------

// link_project and attach_remote only ever add a deploy key to a repository
// that exists: over every outcome of every scenario the only write GitHub sees
// is POST .../keys; no repository is created, edited, protected or deleted, and
// no file is committed.
func TestLinkAttachNeverCreateModifyOrDeleteARemoteRepo(t *testing.T) {
	scenarios := []string{"link ok", "link missing", "link read failure", "link key refused", "link key 5xx", "attach ok", "attach key refused", "attach key 5xx", "attach rate limited"}
	ran := 0
	for _, fl := range ghFlows() {
		pick := false
		for _, s := range scenarios {
			pick = pick || s == fl.name
		}
		if !pick {
			continue
		}
		e := newE3(t)
		e.existing()
		fl.inject(e)
		fl.run(e)
		ran++
		for _, r := range e.ghWrites() {
			if !strings.HasPrefix(r, "POST /repos/") || !strings.HasSuffix(r, "/keys") {
				t.Errorf("%s: a write other than a deploy key: %s", fl.name, r)
			}
		}
		if len(e.ghWrites()) > 1 {
			t.Errorf("%s: the key step was retried: %v", fl.name, e.ghWrites())
		}
		e.noDestructive()
	}
	if ran != len(scenarios) {
		t.Fatalf("ran %d of %d scenarios", ran, len(scenarios))
	}
}

// A refused key request is never a success and registers nothing: GitHub's 422
// (key already in use), 403, 401, 429 and GitLab's 400 each end in the
// provider's refusal, no orphan, no registry entry; the request ID is spent.
func TestDuplicateOrRefusedDeployKeyIsNeverSuccess(t *testing.T) {
	for _, st := range []int{400, 401, 403, 404, 422, 429} {
		e := newE3(t)
		e.existing()
		e.gh.fail["POST /repos/alice/Existing/keys"] = st
		a := e.linkArgs("linked", "gh", "alice/existing", true, nil)
		_, oe := e.do("link_project", a)
		if oe == nil || oe.Code == "" || oe.Code == "link_partial" {
			t.Errorf("link key %d: %v", st, codeOf(oe))
		}
		if _, ok := e.find("linked"); ok {
			t.Errorf("link key %d: registered", st)
		}
		if len(e.orphans()) != 0 {
			t.Errorf("link key %d: an orphan for a refused key", st)
		}
		if _, oe := e.do("link_project", a); codeOf(oe) != "request_reused" {
			t.Errorf("link key %d: the signature was not spent: %v", st, codeOf(oe))
		}
		e2 := newE3(t)
		e2.existing()
		e2.ownLocal("plain", true, nil)
		e2.gh.fail["POST /repos/alice/Existing/keys"] = st
		if _, oe := e2.do("attach_remote", e2.attachArgs("plain", "gh", "alice/existing", nil)); oe == nil || oe.Code == "link_partial" {
			t.Errorf("attach key %d: %v", st, codeOf(oe))
		}
		if p, _ := e2.find("plain"); p.Provider != nil || p.Remote != nil {
			t.Errorf("attach key %d: project changed %+v", st, p)
		}
	}
	// GitLab: the same, from a refused POST deploy_keys
	g := newE3(t)
	g.existing()
	g.gl.fail["POST /api/v4/projects/21/deploy_keys"] = 400
	if _, oe := g.do("link_project", g.linkArgs("exist", "gl", "team/exist", false, nil)); oe == nil || oe.Code == "link_partial" {
		t.Fatalf("gitlab key: %v", codeOf(oe))
	}
	if _, ok := g.find("exist"); ok || len(g.orphans()) != 0 {
		t.Fatal("gitlab: registered or orphaned a refused key")
	}
}

// attach changes provider and remote and nothing else, whatever the project
// held: ai_context (true stays true, false stays false), path, kind, auto_merge
// and the project id are byte-identical; ai_context cannot even be sent.
func TestAttachNeverWidensOrChangesAnythingButTheRemote(t *testing.T) {
	extraN := 0
	for _, ai := range []bool{false, true} {
		e := newE3(t)
		e.existing()
		e.ownLocal("plain", ai, nil)
		old, _ := e.find("plain")
		if _, oe := e.do("attach_remote", e.attachArgs("plain", "gh", "alice/existing", nil)); oe != nil {
			t.Fatalf("ai=%v: %v", ai, oe.Code)
		}
		now, _ := e.find("plain")
		if now.AIContext != ai || now.Path != old.Path || now.Kind != old.Kind || now.ID != old.ID || now.AutoMerge != old.AutoMerge {
			t.Fatalf("ai=%v: attach changed more than the remote: %+v -> %+v", ai, old, now)
		}
		if now.Provider == nil || *now.Provider != "gh" || now.Remote == nil || now.Remote.Path != "alice/Existing" {
			t.Fatalf("ai=%v: not attached: %+v", ai, now)
		}
		inAI := false
		for _, path := range e.s.Registry.AIContextPaths() {
			inAI = inAI || path == now.Path
		}
		if inAI != ai {
			t.Fatalf("ai=%v: AI paths %v", ai, e.s.Registry.AIContextPaths())
		}
		// ai_context / path / kind are not attach keys at all
		for _, extra := range []map[string]any{{"ai_context": true}, {"path": "/etc"}, {"kind": "default"}, {"auto_merge": map[string]any{"enabled": true}}} {
			xid := fmt.Sprintf("px-%d-%v", extraN, ai)
			extraN++
			e.ownLocal(xid, ai, nil)
			a := e.attachArgs(xid, "gh", "alice/existing", nil)
			for k, v := range extra {
				a[k] = v
			}
			if _, oe := e.do("attach_remote", a); codeOf(oe) != "bad_request" {
				t.Errorf("extra %v: %v", extra, codeOf(oe))
			}
		}
	}
	// a project that already has a remote is never adopted or re-pointed: legacy-mapped, provider-linked
	e := newE3(t)
	e.existing()
	e.ownLocal("mapped", false, &RemoteLink{Path: "team/exist"})
	e.ownLinked("linked-one", "gl", "team/other")
	before := e.snap()
	for _, id := range []string{"mapped", "linked-one"} {
		for _, prov := range []struct{ id, remote string }{{"gh", "alice/existing"}, {"gl", "team/exist"}} {
			if _, oe := e.do("attach_remote", e.attachArgs(id, prov.id, prov.remote, nil)); codeOf(oe) != "already_linked" {
				t.Errorf("%s -> %s: %v", id, prov.id, codeOf(oe))
			}
		}
	}
	e.mustEqual(before, "attach on a project with a remote")
	e.noGHHits("attach on a project with a remote")
	e.noGLHits("attach on a project with a remote")
}

// ---------- 10 ----------

// Partial and unknown outcomes never delete or retry anything and the orphan
// is recorded with the right origin: a create that stopped part way is
// "created" (the repository is the user's to remove by hand), link and attach
// are "linked" (only a key may have been added; the repository was there
// before and the sentence says so), also when the registry write fails or the
// code panics. No DELETE/PATCH, and no write is sent twice.
func TestPartialAndUnknownOutcomesNeverDeleteAndOrphanOriginIsRight(t *testing.T) {
	type sc struct {
		name   string
		op     string
		inject func(e *e3env)
		args   func(e *e3env) map[string]any
		code   string
		step   string
		origin string
		orphan bool
	}
	create := func(e *e3env) map[string]any { return e.ghCreate(c3Name, "alice", "private", nil) }
	link := func(e *e3env) map[string]any { return e.linkArgs("linked", "gh", "alice/existing", false, nil) }
	attach := func(e *e3env) map[string]any {
		e.ownLocal("plain", false, nil)
		return e.attachArgs("plain", "gh", "alice/existing", nil)
	}
	failWrite := func(e *e3env) {
		n := 0
		e.s.Registry.opts.failAt = func(stage string) error {
			if stage == "temp-written" {
				if n++; n == 1 {
					return errors.New("injected")
				}
			}
			return nil
		}
	}
	panicWrite := func(e *e3env) {
		n := 0
		e.s.Registry.opts.failAt = func(stage string) error {
			if stage == "temp-written" {
				if n++; n == 1 {
					panic("boom")
				}
			}
			return nil
		}
	}
	cases := []sc{
		{"create 5xx", "create_project", func(e *e3env) { e.gh.fail["POST /user/repos"] = 500 }, create, "create_partial", "project", "created", true},
		{"create first file", "create_project", func(e *e3env) { e.gh.fail["PUT /repos/alice/demo/contents/README.md"] = 500 }, create, "create_partial", "first_commit", "created", true},
		{"create default branch", "create_project", func(e *e3env) { e.gh.defBranch = "master" }, create, "create_partial", "default_branch", "created", true},
		{"create weaker rule", "create_project", func(e *e3env) { e.gh.protection = ghWeakProtection }, create, "create_partial", "protect", "created", true},
		{"create paid plan", "create_project", func(e *e3env) { e.gh.fail["PUT /repos/alice/demo/branches/main/protection"] = 403 }, create, "create_partial", "protect", "created", true},
		{"create key", "create_project", func(e *e3env) { e.gh.fail["POST /repos/alice/demo/keys"] = 500 }, create, "create_partial", "deploy_key", "created", true},
		{"create registry write", "create_project", failWrite, create, "create_partial", "register", "created", true},
		{"create panic", "create_project", panicWrite, create, "create_partial", "register", "created", true},
		{"link key 5xx", "link_project", func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 502 }, link, "link_partial", "deploy_key", "linked", true},
		{"link registry write", "link_project", failWrite, link, "link_partial", "register", "linked", true},
		{"link panic", "link_project", panicWrite, link, "link_partial", "register", "linked", true},
		{"attach key 5xx", "attach_remote", func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 500 }, attach, "link_partial", "deploy_key", "linked", true},
		{"attach registry write", "attach_remote", failWrite, attach, "link_partial", "register", "linked", true},
		{"attach panic", "attach_remote", panicWrite, attach, "link_partial", "register", "linked", true},
	}
	for _, c := range cases {
		e := newE3(t)
		e.existing()
		args := c.args(e)
		c.inject(e) // after the fixtures (ownLocal writes the registry)
		out := e.rawWire(c.op, args)
		var r reply
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if r.OK || r.Code != c.code {
			t.Errorf("%s: %s", c.name, out)
			continue
		}
		if !strings.Contains(out, `"step":"`+c.step+`"`) || !strings.Contains(out, `"origin":"`+c.origin+`"`) {
			t.Errorf("%s: detail %s", c.name, out)
		}
		os := e.orphans()
		if len(os) != 1 || os[0].Origin != c.origin || os[0].Step != c.step {
			t.Errorf("%s: orphans %+v", c.name, os)
		}
		if c.origin == "linked" {
			if strings.Contains(out, `"origin":"created"`) || strings.Contains(string(e.regFile()), `"origin":"created"`) {
				t.Errorf("%s: a linked repository is recorded as created", c.name)
			}
			if !strings.Contains(r.Message, "지우지 마세요") {
				t.Errorf("%s: the linked sentence does not say do not delete: %q", c.name, r.Message)
			}
		}
		e.noDestructive()
		seen := map[string]int{}
		for _, w := range e.ghWrites() {
			seen[w]++
		}
		for w, n := range seen {
			if n > 1 {
				t.Errorf("%s: %s was sent %d times (no retry)", c.name, w, n)
			}
		}
		// nothing is registered by a partial link/create; attach left the project as it was
		switch c.op {
		case "create_project":
			if p, ok := e.find(c3Name); ok && c.step != "register" || ok && p.Provider != nil {
				t.Errorf("%s: project registered after a partial", c.name)
			}
		case "link_project":
			if _, ok := e.find("linked"); ok {
				t.Errorf("%s: project registered after a partial", c.name)
			}
		case "attach_remote":
			if p, _ := e.find("plain"); p.Provider != nil || p.Remote != nil {
				t.Errorf("%s: project changed by a partial attach: %+v", c.name, p)
			}
		}
	}
	// a refused create-repo call (4xx) wrote nothing: no orphan; an unknown outcome (5xx) does
	e := newE3(t)
	e.gh.fail["POST /user/repos"] = 422
	if _, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil)); codeOf(oe) != "github_refused" || len(e.orphans()) != 0 {
		t.Fatalf("refused create: %v %+v", codeOf(oe), e.orphans())
	}
}

// ---------- 11 ----------

const v3Tail = `"auto_merge_global":{"enabled":false,"max_per_day":5}`

func v3Proj(path string, ai bool, provider, remote string) string {
	return fmt.Sprintf(`{"id":"a","path":%q,"kind":"registered","ai_context":%v,"provider":%s,"remote":%s,"auto_merge":{"enabled":false}}`, path, ai, provider, remote)
}

// A registry file of version 3 that is wrong in any way (including the new
// remote link, github kind and orphan origin) is not patched up and not
// replaced by the legacy config: every op that needs the registry answers
// registry_unavailable, no project gets AI context (nothing is sent to the AI
// endpoint), the file stays as it was and no provider is contacted. A file
// with the wrong mode or owner is refused the same way; a valid v3 file is the
// control.
func TestRegistryV3BrokenFailsClosedAndDoesNotWidenAIContext(t *testing.T) {
	ghp := `{"id":"gh","kind":"github","base_url":"https://api.github.com","ssh_url":"ssh://git@github.com","token_ref":"gh-tok"}`
	proj := v3Proj
	cases := map[string]string{
		"truncated":               `{"version":3,"providers":[` + ghp + `],"projects":[` + proj("/x/a", true, `null`, `null`),
		"garbage":                 `not json`,
		"empty":                   ``,
		"version string":          `{"version":"3","providers":[],"projects":[],"orphans":[],` + v3Tail + `}`,
		"mixed-case version":      `{"version":1,"Version":3,"providers":[],"projects":[],"orphans":[],` + v3Tail + `}`,
		"unknown key":             `{"version":3,"providers":[],"projects":[],"orphans":[],"extra":1,` + v3Tail + `}`,
		"project unknown key":     `{"version":3,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":true,"provider":null,"remote":null,"auto_merge":{"enabled":false},"extra":1}],"orphans":[],` + v3Tail + `}`,
		"v2 gitlab key in v3":     `{"version":3,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":true,"provider":null,"gitlab":null,"auto_merge":{"enabled":false}}],"orphans":[],` + v3Tail + `}`,
		"duplicate provider":      `{"version":3,"providers":[` + ghp + `,` + ghp + `],"projects":[],"orphans":[],` + v3Tail + `}`,
		"provider kind bitbucket": `{"version":3,"providers":[{"id":"x","kind":"bitbucket","base_url":"https://10.0.0.7","ssh_url":"ssh://git@10.0.0.7","token_ref":"t"}],"projects":[],"orphans":[],` + v3Tail + `}`,
		"project unknown prov":    `{"version":3,"providers":[],"projects":[` + proj("/x/a", true, `"nope"`, `null`) + `],"orphans":[],` + v3Tail + `}`,
		"auto merge on":           `{"version":3,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":true,"provider":null,"remote":null,"auto_merge":{"enabled":true}}],"orphans":[],` + v3Tail + `}`,
		"orphan bad origin":       `{"version":3,"providers":[],"projects":[],"orphans":[{"provider":"gh","project":"o/r","project_id":null,"step":"project","at":"2026-11-11T00:00:00Z","origin":"adopted"}],` + v3Tail + `}`,
		"orphan bad path":         `{"version":3,"providers":[],"projects":[],"orphans":[{"provider":"gh","project":"../x","project_id":null,"step":"project","at":"2026-11-11T00:00:00Z","origin":"created"}],` + v3Tail + `}`,
		"remote id no provider":   `{"version":3,"providers":[],"projects":[` + proj("/x/a", true, `null`, `{"path":"o/r","id":5}`) + `],"orphans":[],` + v3Tail + `}`,
		"remote bad path":         `{"version":3,"providers":[` + ghp + `],"projects":[` + proj("/x/a", true, `"gh"`, `{"path":"o/r.git","id":5}`) + `],"orphans":[],` + v3Tail + `}`,
		"remote traversal":        `{"version":3,"providers":[` + ghp + `],"projects":[` + proj("/x/a", true, `"gh"`, `{"path":"o/../r","id":5}`) + `],"orphans":[],` + v3Tail + `}`,
		"version 5":               `{"version":5,"providers":[],"projects":[],"orphans":[],` + v3Tail + `}`,
		"trailing data":           `{"version":3,"providers":[],"projects":[],"orphans":[],` + v3Tail + `}{"x":1}`,
	}
	oa := newFakeHTTP(t, okHandler(goodPlan, 10, 10))
	run := func(name, content string, wantBroken bool) {
		e := newTestEnv(t)
		state := filepath.Join(e.Dir, "state")
		os.MkdirAll(state, 0o700)
		os.WriteFile(filepath.Join(state, RegistryFileName), []byte(content), 0o600)
		s, _ := e.newServer(t, func(s *Server) {
			s.OpenAI.BaseURL = oa.URL
			s.apiSeams = &apiSeams{policy: allowLoopback}
		})
		if (s.Registry.Err() != nil) != wantBroken {
			t.Fatalf("%s: registry broken=%v, want %v", name, s.Registry.Err() != nil, wantBroken)
		}
		if !wantBroken {
			return
		}
		if got := s.Registry.AIContextPaths(); len(got) != 0 {
			t.Fatalf("%s: AI context paths %v", name, got)
		}
		sock := e.Raw["socket"].(string)
		repo := filepath.Join(e.Dir, "repo-a")
		for _, op := range []string{"status", "projects", "providers"} {
			if r, out := call(t, sock, `{"v":1,"op":"`+op+`"}`); r.OK || r.Code != "registry_unavailable" || strings.Contains(string(out), repo) {
				t.Fatalf("%s: %s: %s", name, op, out)
			}
		}
		for _, op := range []string{"list_groups", "link_project", "attach_remote", "create_project"} {
			if r, out := call(t, sock, `{"v":1,"op":"`+op+`","args":{"provider":"gh"}}`); r.OK {
				t.Fatalf("%s: %s: %s", name, op, out)
			}
		}
		args := map[string]any{"repo": repo, "requirement": "x", "files": []map[string]string{{"path": "a.go", "content": "x"}}, "selection": "sel-abc123"}
		if r, out := call(t, sock, line(t, rpc("refine", args))); r.OK || r.Code != "registry_unavailable" {
			t.Fatalf("%s: refine: %s", name, out)
		}
		if got, _ := os.ReadFile(filepath.Join(state, RegistryFileName)); string(got) != content {
			t.Fatalf("%s: the broken file was modified", name)
		}
	}
	for name, content := range cases {
		run(name, content, true)
	}
	valid := `{"version":3,"providers":[` + ghp + `],"projects":[` + proj(filepath.Join(os.TempDir(), "a"), false, `"gh"`, `{"path":"o/r","id":5}`) + `],"orphans":[],` + v3Tail + `}`
	// wrong mode or foreign owner: refused (and the file untouched), exactly like a corrupt one
	for name, c := range map[string]struct {
		mode  os.FileMode
		owner func(uint32) bool
	}{"mode 0644": {0o644, nil}, "mode 0660": {0o660, nil}, "mode 0666": {0o666, nil}, "mode 0640": {0o640, nil}, "mode 0602": {0o602, nil},
		"foreign owner": {0o600, func(uint32) bool { return false }}} {
		d := stateDirFor(t)
		p := writeRegFile(t, d, valid, c.mode)
		o := regOpts()
		if c.owner != nil {
			o.fileOwnerOK = c.owner
		}
		st, err := OpenRegistry(d, LegacySource{}, o)
		if !errors.Is(err, ErrRegistryBroken) || st.Err() == nil {
			t.Errorf("%s: not refused: %v", name, err)
		}
		if string(readFile(t, p)) != valid {
			t.Errorf("%s: the file was modified", name)
		}
	}
	if oa.Hits() != 0 {
		t.Fatalf("the AI endpoint was reached %d times with a broken registry", oa.Hits())
	}
	// control: the valid file is accepted (so the cases above fail for their own reasons)
	run("valid control", valid, false)
}

// v2 -> v3 keeps the content (projects, providers, ai_context as it was,
// orphans, global settings), writes registry.json.v2.bak once and never
// overwrites an existing backup; an already-v3 file is not touched; an old
// (W3c) strict loader refuses the v3 file (pinned elsewhere by
// TestOldBrokerRefusesV3, here: also with the version forced back).
func TestRegistryV2MigrationKeepsContentWritesBackupOnceAndOldVersionsStillRefuseV3(t *testing.T) {
	dir := stateDirFor(t)
	p := writeRegFile(t, dir, v2Sample, 0o600)
	st := mustOpen(t, dir, LegacySource{ContextRepos: []string{"/srv/ignored"}})
	reg := st.Snapshot()
	aiBefore := map[string]bool{}
	var o2 struct {
		Projects []struct {
			ID string `json:"id"`
			AI bool   `json:"ai_context"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(v2Sample), &o2); err != nil {
		t.Fatal(err)
	}
	for _, pr := range o2.Projects {
		aiBefore[pr.ID] = pr.AI
	}
	for _, pr := range reg.Projects {
		if pr.AIContext != aiBefore[pr.ID] {
			t.Errorf("%s: ai_context %v -> %v by the migration", pr.ID, aiBefore[pr.ID], pr.AIContext)
		}
	}
	for _, path := range st.AIContextPaths() {
		if path == "/srv/ignored" {
			t.Fatal("the legacy config widened the AI context")
		}
	}
	bak := p + ".v2.bak"
	if got := readFile(t, bak); string(got) != v2Sample {
		t.Fatalf("backup differs from the v2 file:\n%s", got)
	}
	fi, err := os.Stat(bak)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode: %v %v", fi, err)
	}
	if string(readFile(t, p)) != v4Want {
		t.Fatal("migrated content differs from the expected v4")
	}
	// later writes and restarts neither rewrite nor remove the backup
	if err := st.Update(func(r *Registry) error { r.AutoMergeGlobal.MaxPerDay = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, dir, LegacySource{})
	if got := readFile(t, bak); string(got) != v2Sample {
		t.Fatal("the backup was changed after the migration")
	}
	// an existing backup is kept as it is (a second v2 file later does not replace it)
	dir2 := stateDirFor(t)
	p2 := writeRegFile(t, dir2, v2Sample, 0o600)
	if err := os.WriteFile(p2+".v2.bak", []byte("older backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(dir2, LegacySource{}, regOpts()); err == nil {
		// the migration may refuse to start over an existing backup (fail closed) or keep it: it must never overwrite
		if got := readFile(t, p2+".v2.bak"); string(got) != "older backup" {
			t.Fatal("an existing backup was overwritten")
		}
	} else if got := readFile(t, p2+".v2.bak"); string(got) != "older backup" {
		t.Fatal("an existing backup was overwritten")
	}
	// a v4 file is not migrated again and no backup appears for it
	dir3 := stateDirFor(t)
	p3 := writeRegFile(t, dir3, v4Want, 0o600)
	mustOpen(t, dir3, LegacySource{})
	if _, err := os.Stat(p3 + ".v2.bak"); !os.IsNotExist(err) || string(readFile(t, p3)) != v4Want {
		t.Fatalf("a v4 file was touched (%v)", err)
	}
	// the v4 content under the old version numbers is broken for this loader too (no silent downgrade)
	for _, v := range []string{"3", "2", "1"} {
		d := stateDirFor(t)
		writeRegFile(t, d, strings.Replace(v4Want, `"version":4`, `"version":`+v, 1), 0o600)
		if s, err := OpenRegistry(d, LegacySource{}, regOpts()); err == nil && s.Err() == nil {
			t.Errorf("v3 shape under version %s was accepted", v)
		}
	}
}

// ---------- 12 ----------

// The read ops and the errors of the GitHub flows carry no secret: not the
// GitHub token, the GitLab token, the OpenAI key, key or token file paths, the
// signer line, a request ID or signature, nor the deploy key line; orphans are
// values only.
func TestStatusAndProjectsNoSecretsAfterGitHubFlows(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.gh.fail["POST /repos/alice/demo/keys"] = 500
	e.rawWire("create_project", e.ghCreate(c3Name, "alice", "private", nil)) // leaves an orphan
	good := e.ghCreate("demo2", "alice", "public", nil)
	e.rawWire("create_project", good)
	e.gh.fail["POST /repos/alice/Existing/keys"] = 502
	linkArgs := e.linkArgs("linked", "gh", "alice/existing", false, nil)
	e.rawWire("link_project", linkArgs)
	secrets := []string{ghCanary, c3Canary, testOpenAIKey, testGitLabToken, "Bearer", "PRIVATE-TOKEN", "BEGIN", e.alice.line, e.e.KeyFile, e.e.TokFile, e.signers,
		"signature", "REFLECT", c3Key(), "AAAAC3NzaC1lZDI1NTE5AAAAIGoodKey", good["request"].(string), good["change"].(string), linkArgs["request"].(string)}
	for _, op := range []string{"status", "projects", "providers", "tokens"} {
		out := e.rawWire(op, nil)
		if !strings.Contains(out, `"ok":true`) {
			t.Fatalf("%s: %s", op, out)
		}
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%q in the %s reply", s, op)
			}
		}
	}
	for _, s := range secrets {
		if out := e.rawWire("list_groups", map[string]any{"provider": "gh"}); strings.Contains(out, s) {
			t.Errorf("%q in the list_groups reply", s)
		}
	}
	pr := e.rawWire("projects", nil)
	if !strings.Contains(pr, `"origin":"created"`) || !strings.Contains(pr, `"origin":"linked"`) {
		t.Errorf("orphans with their origin are not visible as values: %s", pr)
	}
	for _, bad := range []map[string]any{{"x": 1}, copyArgs(good, func(m map[string]any) { delete(m, "signature") }), e.ghCreate(c3Name, "alice", "private", nil),
		copyArgs(linkArgs, func(m map[string]any) { delete(m, "deploy_key") })} {
		for _, op := range []string{"create_project", "link_project", "attach_remote"} {
			out := e.rawWire(op, bad)
			for _, s := range secrets {
				if strings.Contains(out, s) {
					t.Errorf("%q in an error reply of %s", s, op)
				}
			}
		}
	}
	for _, s := range secrets[:7] {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("%q in the log", s)
		}
	}
	if !strings.Contains(e.logs.String(), "op=projects ok") {
		t.Fatal("the log scan saw no op lines")
	}
}

// ---------- final review M1 ----------

// The provider is re-read after the name lock and the kind rules apply to THAT
// entry: a provider whose kind flips between the shape stage and the lock
// (gitlab -> github: no visibility was signed; github -> gitlab: a visibility
// was signed) is refused plainly, nothing is sent, no orphan, no nil
// dereference, nothing registered.
func TestProviderKindFlipBetweenShapeAndLockRefusedW3e(t *testing.T) {
	for _, tc := range []struct {
		name, prov, from, to, code string
		args                       func(e *e3env) map[string]any
	}{
		{"gitlab to github", "gl", "gitlab", "github", "visibility_required", func(e *e3env) map[string]any { return e.projectArgs(c3Name, nil) }},
		{"github to gitlab", "gh", "github", "gitlab", "visibility_not_allowed", func(e *e3env) map[string]any { return e.ghCreate(c3Name, "alice", "public", nil) }},
	} {
		e := newE3(t)
		args := tc.args(e)
		e.s.creatingMu.Lock() // the handler passes the shape stage, then waits for the name lock
		type res struct{ oe *OpError }
		done := make(chan res, 1)
		go func() {
			_, oe := e.do("create_project", args)
			done <- res{oe}
		}()
		// wait until the signature has been consumed (the shape stage is behind us)
		for i := 0; i < 500 && len(e.usedFile()) == 0; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if err := e.s.Registry.Update(func(r *Registry) error {
			i := findProvider(r, tc.prov)
			if i < 0 || r.Providers[i].Kind != tc.from {
				t.Errorf("%s: fixture", tc.name)
				return nil
			}
			r.Providers[i].Kind = tc.to
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		e.s.creatingMu.Unlock()
		r := <-done
		if codeOf(r.oe) != tc.code {
			t.Errorf("%s: %v", tc.name, codeOf(r.oe))
		}
		if _, ok := e.find(c3Name); ok || len(e.orphans()) != 0 {
			t.Errorf("%s: registered or orphaned", tc.name)
		}
		e.noGHHits(tc.name)
		e.noGLHits(tc.name)
	}
}

// ---------- final review: the deploy-key line rule, shared vectors ----------

// orchd cannot import the broker, so its copy of the deploy-key line rule
// (internal/orch/api brokerDeployKeyRE) is pinned to the broker's by running the
// SAME vector file (testdata/deploykey_vectors.json) through both; the orchd
// test reads this file by relative path.
func TestDeployKeyLineRuleVectorsW3e(t *testing.T) {
	raw, err := os.ReadFile("testdata/deploykey_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Line string `json:"line"`
		OK   bool   `json:"ok"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) < 20 {
		t.Fatalf("vectors: %v %d", err, len(vs))
	}
	for _, v := range vs {
		if got := deployKeyRE.MatchString(v.Line); got != v.OK {
			t.Errorf("broker deployKeyRE(%q) = %v, want %v", v.Line, got, v.OK)
		}
	}
}
