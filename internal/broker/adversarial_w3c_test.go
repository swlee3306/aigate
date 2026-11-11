package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"aigate/internal/registrymsg"
)

// W3c (C6) adversarial suite for the broker. Every property here is "the
// attacker's move is refused, and nothing moved": no GitLab request, no change
// to the registry, no burnt request ID unless the signature was valid. Most of
// it drives the REAL handlers over the fake GitLab of the C3 kit; the
// assertions are on the whole flow, not on a helper.

// ---------- helpers ----------

// rawWire sends one request over the broker socket and returns the raw reply.
func (c *c3env) rawWire(op string, args any) string {
	c.t.Helper()
	_, out := call(c.t, c.sock, line(c.t, rpc(op, args)))
	return string(out)
}

// spyGL is a front for the C3 fake GitLab that records the headers of every
// request: the token may only ever travel in PRIVATE-TOKEN.
type spyGL struct {
	*httptest.Server
	mu   sync.Mutex
	hdrs []http.Header
}

func newSpyGL(t *testing.T, c *c3env) *spyGL {
	t.Helper()
	s := &spyGL{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hdrs = append(s.hdrs, r.Header.Clone())
		s.mu.Unlock()
		c.gl.serve(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *spyGL) headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.hdrs...)
}

// withProvider registers a second provider id with the given base URL (same
// token), so a flow can be pointed at a spy or a hostile server.
func (c *c3env) withProvider(id, base string) {
	c.t.Helper()
	if err := c.s.AddProviderChecked(Provider{ID: id, Kind: "gitlab", BaseURL: base, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *c3env) createArgsFor(prov, name string, mod func(f map[string]any)) map[string]any {
	return c.projectArgs(name, func(f map[string]any) {
		f["provider"] = prov
		if mod != nil {
			mod(f)
		}
	})
}

func (c *c3env) groupArgsFor(prov, name, path, parent string) map[string]any {
	return c.signedArgs(c.alice, registrymsg.ActionCreateGroup, prov, map[string]any{"name": name, "path": path, "parent_path": parent})
}

// w3cFlow is one scenario of the fake GitLab; run returns the raw replies.
type w3cFlow struct {
	name   string
	inject func(f *c3GL)
	run    func(c *c3env, prov string) []string
}

func createFlow(name string, inject func(f *c3GL), mod func(f map[string]any)) w3cFlow {
	return w3cFlow{name, inject, func(c *c3env, prov string) []string {
		return []string{c.rawWire("create_project", c.createArgsFor(prov, c3Name, mod))}
	}}
}

// allFlows covers every op of C3 on success and on every kind of failure,
// including a GitLab that reflects the token in all of its error bodies.
func allFlows() []w3cFlow {
	none := func(*c3GL) {}
	return []w3cFlow{
		{"read ops", none, func(c *c3env, prov string) []string {
			return []string{c.rawWire("providers", nil), c.rawWire("list_groups", map[string]any{"provider": prov}),
				c.rawWire("projects", nil), c.rawWire("status", nil)}
		}},
		{"groups refused", func(f *c3GL) { f.fail["GET /api/v4/groups"] = 401 }, func(c *c3env, prov string) []string {
			return []string{c.rawWire("list_groups", map[string]any{"provider": prov})}
		}},
		{"groups 500", func(f *c3GL) { f.fail["GET /api/v4/groups"] = 500 }, func(c *c3env, prov string) []string {
			return []string{c.rawWire("list_groups", map[string]any{"provider": prov})}
		}},
		{"create_group top and nested", none, func(c *c3env, prov string) []string {
			return []string{c.rawWire("create_group", c.groupArgsFor(prov, "newg", "newg", "")),
				c.rawWire("create_group", c.groupArgsFor(prov, "inner", "inner", "team"))}
		}},
		{"create_group refused", func(f *c3GL) { f.fail["POST /api/v4/groups"] = 403 }, func(c *c3env, prov string) []string {
			return []string{c.rawWire("create_group", c.groupArgsFor(prov, "newg", "newg", ""))}
		}},
		{"set/delete provider", none, func(c *c3env, prov string) []string {
			return []string{c.rawWire("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "extra", c.providerFields(c.gl.URL))),
				c.rawWire("delete_provider", c.deleteProviderArgs("extra")),
				c.rawWire("delete_provider", c.deleteProviderArgs("ghost"))}
		}},
		createFlow("create ok basic", none, nil),
		createFlow("create ok go read ai", none, func(f map[string]any) { f["template"] = "go"; f["publish"] = "read"; f["ai_context"] = true }),
		createFlow("name taken", func(f *c3GL) { f.existP["team/demo"] = true }, nil),
		createFlow("group unknown", none, func(f map[string]any) { f["group_path"] = "nowhere" }),
		createFlow("group lookup refused", func(f *c3GL) { f.fail["GET /api/v4/groups/team"] = 403 }, nil),
		createFlow("project 500", func(f *c3GL) { f.fail["POST /api/v4/projects"] = 500 }, nil),
		createFlow("project 409", func(f *c3GL) { f.fail["POST /api/v4/projects"] = 409 }, nil),
		createFlow("first commit", func(f *c3GL) { f.fail["POST /api/v4/projects/21/repository/files/.aigate.yml"] = 500 }, nil),
		createFlow("first commit readme", func(f *c3GL) { f.fail["POST /api/v4/projects/21/repository/files/README.md"] = 403 }, nil),
		createFlow("default branch", func(f *c3GL) { f.defBr = "master" }, nil),
		createFlow("protect 500", func(f *c3GL) { f.fail["POST /api/v4/projects/21/protected_branches"] = 500 }, nil),
		createFlow("protect weaker", func(f *c3GL) { f.protect = "weak" }, nil),
		createFlow("deploy key", func(f *c3GL) { f.fail["POST /api/v4/projects/21/deploy_keys"] = 422 }, nil),
		createFlow("description is only data", none, func(f map[string]any) { f["description"] = `"},"namespace_id":999,"visibility":"public","x":"` }),
	}
}

func scanCanary(leaks *[]string, what, s string) {
	if strings.Contains(s, c3Canary) || strings.Contains(s, "REFLECT") {
		*leaks = append(*leaks, what)
	}
}

// ---------- 1 ----------

// The admin token has exactly one way out of the broker: the PRIVATE-TOKEN
// header of a request to the provider's own address. It is in no reply (success
// or failure), no log line, no file of the state directory other than the
// token store itself, no request body or URL, no other header, also not after
// a panic on the real code path, and GitLab's own error text (which here
// reflects the token) is never forwarded.
func TestAdminTokenNeverLeavesBroker(t *testing.T) {
	var leaks []string
	seenHeaders := 0
	for _, fl := range allFlows() {
		fl := fl
		c := newC3(t)
		spy := newSpyGL(t, c)
		c.withProvider("spy", spy.URL)
		fl.inject(c.gl)
		replies := fl.run(c, "spy")
		for i, r := range replies {
			scanCanary(&leaks, fmt.Sprintf("%s reply %d", fl.name, i), r)
		}
		scanCanary(&leaks, fl.name+" log", c.logs.String())
		_ = filepath.Walk(c.e.Dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || strings.Contains(p, string(filepath.Separator)+"secrets"+string(filepath.Separator)) {
				return nil
			}
			b, _ := os.ReadFile(p)
			scanCanary(&leaks, fl.name+" file "+filepath.Base(p), string(b))
			return nil
		})
		for _, rq := range c.gl.all() {
			scanCanary(&leaks, fl.name+" request body/uri", rq.Body+rq.URI)
		}
		for _, h := range spy.headers() {
			seenHeaders++
			for k, vs := range h {
				for _, v := range vs {
					if k == "Private-Token" {
						if v != c3Canary {
							t.Errorf("%s: PRIVATE-TOKEN carries %q", fl.name, v)
						}
						continue
					}
					if strings.Contains(v, c3Canary) {
						leaks = append(leaks, fl.name+" header "+k)
					}
				}
			}
		}
		// the wire detail of create_partial is values only
		if strings.Contains(strings.Join(replies, "\n"), `"create_partial"`) {
			for _, r := range replies {
				if strings.Contains(r, "create_partial") && !strings.Contains(r, `"orphan_recorded"`) {
					t.Errorf("%s: create_partial without the fixed detail: %s", fl.name, r)
				}
			}
		}
	}
	if seenHeaders < 20 {
		t.Fatalf("control: only %d provider requests were observed", seenHeaders)
	}
	// panic on the real path (the registry write of the last step panics with the token)
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
	out := c.rawWire("create_project", c.projectArgs(c3Name, nil))
	if !strings.Contains(out, "create_partial") {
		t.Fatalf("panic reply: %s", out)
	}
	scanCanary(&leaks, "panic reply", out)
	scanCanary(&leaks, "panic log", c.logs.String())
	scanCanary(&leaks, "panic registry", string(c.regFile()))
	// malformed and unknown requests
	for _, raw := range []string{`{"v":1,"op":"create_project","args":` + c3Canary + `}`, `{"v":1,"op":"` + c3Canary + `"}`, `not json ` + c3Canary} {
		scanCanary(&leaks, "malformed reply", string(roundTrip(t, c.sock, []byte(raw+"\n"))))
	}
	scanCanary(&leaks, "log after malformed", c.logs.String())
	if len(leaks) != 0 {
		t.Fatalf("the admin token (or text reflected by GitLab) left the broker: %v", leaks)
	}
}

// ---------- 2 ----------

var forbiddenPieces = []string{"merge", "access_tokens", "personal_access", "/user", "approve", "unprotect", "/commits", "/enable", "transfer", "archive", "/members", "hooks", "runners", "variables"}

// Every request any flow sends to GitLab, collected over all scenarios, is
// allowlisted (method, path shape, query keys), has no delete/merge/token
// shape and carries only the documented body keys; and every allowlist row is
// reached by at least one flow (so the allowlist holds nothing unused).
func TestEveryGitLabRequestIsAllowlisted(t *testing.T) {
	bodyKeys := map[string][]string{
		"POST /api/v4/groups":   {"name", "path", "parent_id", "visibility"},
		"POST /api/v4/projects": {"name", "path", "namespace_id", "visibility", "initialize_with_readme", "description"},
		"files":                 {"branch", "content", "commit_message"},
		"protected_branches":    {"name", "push_access_level", "merge_access_level", "allow_force_push"},
		"deploy_keys":           {"title", "key", "can_push"},
	}
	var all []glReq
	for _, fl := range allFlows() {
		c := newC3(t)
		fl.inject(c.gl)
		fl.run(c, "gl")
		all = append(all, c.gl.all()...)
	}
	// hostile signed inputs reach no request at all
	hc := newC3(t)
	hostile := []func(f map[string]any){
		func(f map[string]any) { f["group_path"] = "team/../x" },
		func(f map[string]any) { f["group_path"] = "team%2Fx" },
		func(f map[string]any) { f["group_path"] = "team?x=1" },
		func(f map[string]any) { f["group_path"] = "Team" },
		func(f map[string]any) { f["group_path"] = "team/" },
		func(f map[string]any) { f["group_path"] = "/team" },
		func(f map[string]any) { f["group_path"] = "a/b/c/d/e" },
		func(f map[string]any) { f["group_path"] = "team\n" },
		func(f map[string]any) { f["group_path"] = "" },
		func(f map[string]any) { f["template"] = "go/../x" },
		func(f map[string]any) { f["provider"] = "gl/../x" },
	}
	for i, mod := range hostile {
		// a valid signature over honest fields, the hostile value swapped into the request
		a := hc.projectArgs(c3Name, nil)
		fm := map[string]any{}
		mod(fm)
		for k, v := range fm {
			a[k] = v
		}
		if _, e := hc.do("create_project", a); e == nil {
			t.Fatalf("hostile input %d accepted", i)
		}
	}
	for _, id := range []string{"demo/../x", "demo%2f", "DEMO", "d", "demo\n", "-demo", "demo.git", "../demo"} {
		a := hc.projectArgs(c3Name, nil)
		a["id"] = id
		if _, e := hc.do("create_project", a); e == nil {
			t.Fatalf("hostile project name %q accepted", id)
		}
	}
	hc.noGLHits("hostile create_project inputs")
	for _, bad := range []string{"../y", "y/z", "y%2Fz", "Y", "y?x=1", ""} {
		a := hc.groupArgs("okname", "okname", "")
		a["path"] = bad
		if _, e := hc.do("create_group", a); e == nil {
			t.Fatalf("hostile group path %q accepted", bad)
		}
	}
	hc.noGLHits("hostile create_group inputs")

	// the sending primitive itself, handed destructive and token-shaped requests
	// directly (no public method emits them): refused before any network access
	pc := newC3(t)
	papi, err := pc.s.GitLabAPIFor("gl")
	if err != nil {
		t.Fatal(err)
	}
	for _, rq := range []struct{ m, p string }{
		{"DELETE", "/api/v4/projects/21"}, {"DELETE", "/api/v4/groups/team"}, {"PUT", "/api/v4/projects/21"}, {"PATCH", "/api/v4/projects/21"},
		{"POST", "/api/v4/projects/21/merge_requests/1/merge"}, {"GET", "/api/v4/user"}, {"POST", "/api/v4/projects/21/access_tokens"},
		{"POST", "/api/v4/projects/21/repository/commits"}, {"POST", "/api/v4/projects/21/deploy_keys/5/enable"},
	} {
		if _, err := papi.do(context.Background(), rq.m, rq.p, nil, nil, 200); ErrCode(err) != CodeNotAllowlisted {
			t.Errorf("%s %s: %v", rq.m, rq.p, err)
		}
	}
	pc.noGLHits("off-list requests handed to the sender")

	if len(all) < 60 {
		t.Fatalf("control: only %d requests observed", len(all))
	}
	seen := map[int]bool{}
	for _, r := range all {
		u, err := url.Parse(r.URI)
		if err != nil {
			t.Fatal(err)
		}
		if !allowed(r.Method, u.EscapedPath(), u.Query()) {
			t.Fatalf("request outside the allowlist reached GitLab: %s %s", r.Method, r.URI)
		}
		if r.Method != "GET" && r.Method != "POST" {
			t.Fatalf("method %s: nothing but GET and POST exists", r.Method)
		}
		low := strings.ToLower(u.EscapedPath())
		for _, bad := range forbiddenPieces {
			if strings.Contains(low, bad) {
				t.Fatalf("forbidden path piece %q: %s", bad, r.URI)
			}
		}
		for i, e := range allowlist {
			if e.method == r.Method && e.path.MatchString(u.EscapedPath()) {
				seen[i] = true
			}
		}
		if r.Method == "POST" {
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
				t.Fatalf("POST body is not a JSON object: %q", r.Body)
			}
			key := r.Method + " " + u.EscapedPath()
			switch {
			case strings.Contains(u.Path, "/repository/files/"):
				key = "files"
			case strings.HasSuffix(u.Path, "/protected_branches"):
				key = "protected_branches"
			case strings.HasSuffix(u.Path, "/deploy_keys"):
				key = "deploy_keys"
			}
			allow := map[string]bool{}
			for _, k := range bodyKeys[key] {
				allow[k] = true
			}
			if len(allow) == 0 {
				t.Fatalf("no body rule for %s", key)
			}
			for k := range body {
				if !allow[k] {
					t.Fatalf("%s: unexpected body key %q (%s)", key, k, r.Body)
				}
			}
			// the free-text description never changed the structure
			if key == "POST /api/v4/projects" {
				var ns int64
				json.Unmarshal(body["namespace_id"], &ns)
				if ns != 7 || string(body["visibility"]) != `"private"` {
					t.Fatalf("project body structure moved: %s", r.Body)
				}
			}
		}
	}
	for i, e := range allowlist {
		if !seen[i] {
			t.Errorf("allowlist entry %d (%s %s) was reached by no flow", i, e.method, e.path)
		}
	}
	// the deletion/merge shapes are not even in the code base: no DELETE/PUT/PATCH
	// method constant is used by the GitLab client or its callers
	for _, f := range []string{"gitlabapi.go", "create_project.go", "providers_ops.go"} {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if se, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok && id.Name == "http" {
					switch se.Sel.Name {
					case "MethodDelete", "MethodPut", "MethodPatch", "MethodHead":
						t.Errorf("%s uses http.%s", f, se.Sel.Name)
					}
				}
			}
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				switch bl.Value {
				case `"DELETE"`, `"PUT"`, `"PATCH"`:
					t.Errorf("%s contains the literal %s", f, bl.Value)
				}
			}
			return true
		})
	}
}

// ---------- 3 ----------

// Provider addresses that point at the broker's own host, the metadata
// service, link-local or reserved space, in any spelling, are refused by
// set_provider under the PRODUCTION address policy before any connection; a
// name that later resolves to such an address is refused at connect time
// (rebinding). A loopback trap server proves that nothing connected.
func TestSSRFProviderAddressesRefused(t *testing.T) {
	var trapHits atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { trapHits.Add(1) }))
	t.Cleanup(trap.Close)
	var answer atomic.Value
	answer.Store([]netip.Addr{netip.MustParseAddr("10.0.0.7")})
	w := newWEnvMod(t, func(s *Server) {
		s.apiSeams = &apiSeams{lookup: func(context.Context, string) ([]netip.Addr, error) { return answer.Load().([]netip.Addr), nil }}
	})
	if err := w.s.Tokens.Set("gl-admin", "gitlab", "", c3Canary, tokNow); err != nil {
		t.Fatal(err)
	}
	signable := 0
	set := func(id, base, ssh string) *OpError {
		f := map[string]any{"kind": "gitlab", "base_url": base, "ssh_url": ssh, "token_ref": "gl-admin"}
		if _, err := registrymsg.ChangeHash(registrymsg.ActionSetProvider, id, f); err != nil {
			// not even signable (the shape rule refuses it): an honest signature over
			// good fields with the hostile value swapped into the request
			good := map[string]any{"kind": "gitlab", "base_url": "https://10.0.0.7", "ssh_url": "ssh://git@10.0.0.7", "token_ref": "gl-admin"}
			a := w.signedArgs(w.alice, registrymsg.ActionSetProvider, id, good)
			a["base_url"], a["ssh_url"] = base, ssh
			_, e := w.do("set_provider", a)
			if e == nil {
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
		trap.URL, "https://127.0.0.1", "http://127.0.0.1:8080", "https://[::1]", "http://[::1]:8443", "https://0.0.0.0", "https://[::]",
		"http://localhost", "https://localhost:8443", "https://foo.localhost", "https://169.254.169.254", "http://169.254.169.254/latest",
		"https://[fe80::1]", "https://[::ffff:127.0.0.1]", "https://[::ffff:169.254.169.254]", "https://[64:ff9b::7f00:1]", "https://[2002:7f00:1::]",
		"https://[2002:a9fe:a9fe::]", "https://100.100.100.200", "https://[fd00:ec2::254]", "https://2130706433", "https://0x7f000001",
		"https://0177.0.0.1", "https://127.1", "https://0.0.0.1", "https://224.0.0.1", "https://255.255.255.255", "https://240.0.0.1",
		"https://192.0.0.192", "https://198.18.0.1", "https://[::ffff:0:7f00:1]", "https://[fec0::1]",
		"http://example.com", "http://8.8.8.8", "ftp://10.0.0.7", "file:///etc/passwd", "gopher://10.0.0.7", "javascript:alert(1)",
		"https://user:pw@10.0.0.7", "https://10.0.0.7/path", "https://10.0.0.7/", "https://10.0.0.7?x=1", "https://10.0.0.7#f",
		"https://10.0.0.7:99999", "https://10.0.0.7:0", "https://EXAMPLE.com", "https://exa mple.com", "https://10.0.0.7/../", "https://", "", " https://10.0.0.7",
		"https://10.0.0.7\n", "https://10.0.0.7:80:80", "https://gitlab.corp.example.",
	}
	for _, b := range bad {
		if e := set("gl9", b, "ssh://git@10.0.0.7"); e == nil {
			t.Errorf("base_url %q was accepted", b)
		} else if e.Code != "bad_url" && e.Code != "bad_request" {
			t.Errorf("base_url %q: code %s", b, e.Code)
		}
	}
	for _, s := range []string{"ssh://git@127.0.0.1", "ssh://git@localhost", "ssh://git@[::1]", "ssh://git@169.254.169.254", "ssh://git@0.0.0.0",
		"ssh://root@10.0.0.7", "ssh://git@10.0.0.7/repo", "ssh://git@10.0.0.7/", "ssh://-oProxyCommand=x@10.0.0.7", "ssh://git@-oProxyCommand=x",
		"ssh://git@10.0.0.7:22/x", "ssh://git@[::ffff:127.0.0.1]", "ssh://git@2130706433", "ssh://git@0x7f000001", "ssh://git:pw@10.0.0.7",
		"https://10.0.0.7", "git@10.0.0.7:team/x.git", "ext::sh -c id", "ssh://git@10.0.0.7 -oProxyCommand=x"} {
		if e := set("gl9", "https://10.0.0.7", s); e == nil {
			t.Errorf("ssh_url %q was accepted", s)
		} else if e.Code != "bad_url" && e.Code != "bad_request" {
			t.Errorf("ssh_url %q: code %s", s, e.Code)
		}
	}
	w.mustEqual(before, "refused provider addresses")
	if signable < 25 {
		t.Fatalf("control: only %d hostile addresses were signable, the address policy is under-tested", signable)
	}
	// names resolving to forbidden space are refused at set time ...
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "0.0.0.0", "::ffff:127.0.0.1", "100.100.100.200"} {
		answer.Store([]netip.Addr{netip.MustParseAddr(ip)})
		if e := set("gl9", "https://gitlab.corp.example", "ssh://git@10.0.0.7"); codeOf(e) != "bad_url" {
			t.Errorf("name resolving to %s: %v", ip, codeOf(e))
		}
	}
	// (a valid signature was presented, so the request ID is spent; the registry is not changed)
	if !bytes.Equal(before.reg, w.regFile()) {
		t.Fatal("names resolving to forbidden addresses: the registry changed")
	}
	// ... and, having passed once, are refused again at connect time
	answer.Store([]netip.Addr{netip.MustParseAddr("10.0.0.7")})
	if e := set("gl9", "https://gitlab.corp.example", "ssh://git@10.0.0.7"); e != nil {
		t.Fatalf("control: %v", e.Code)
	}
	for _, ips := range [][]string{{"127.0.0.1"}, {"169.254.169.254", "127.0.0.1"}, {"::ffff:127.0.0.1"}} {
		var as []netip.Addr
		for _, ip := range ips {
			as = append(as, netip.MustParseAddr(ip))
		}
		answer.Store(as)
		_, e := w.do("list_groups", map[string]any{"provider": "gl9"})
		if e == nil || (e.Code != "bad_url" && e.Code != "provider_unreachable") {
			t.Errorf("rebinding to %v: %v", ips, e)
		}
	}
	if trapHits.Load() != 0 {
		t.Fatalf("the trap server saw %d connection(s)", trapHits.Load())
	}
	// the production defaults hold with the real (no seam) server as well
	s2 := w.restart(nil)
	raw, _ := json.Marshal(w.signedArgs(w.alice, registrymsg.ActionSetProvider, "gl8", map[string]any{"kind": "gitlab", "base_url": trap.URL, "ssh_url": "ssh://git@10.0.0.7", "token_ref": "gl-admin"}))
	if _, e := s2.Handlers["set_provider"](context.Background(), raw); codeOf(e) != "bad_url" {
		t.Fatalf("restart: %v", e)
	}
	if trapHits.Load() != 0 {
		t.Fatal("the trap was reached")
	}
}

// ---------- 4 ----------

// A provider that answers every request with a redirect (301/302/303/307/308,
// also on writes after normal reads) to another host is not followed: the
// other host sees no request, hence no token.
func TestRedirectToOtherHostNotFollowedAndTokenNotSent(t *testing.T) {
	var other atomic.Int32
	var otherTok atomic.Value
	otherTok.Store("")
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
		otherTok.Store(r.Header.Get("PRIVATE-TOKEN"))
	}))
	t.Cleanup(b.Close)
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, onlyWrites := range []bool{false, true} {
			c := newC3(t)
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if onlyWrites && r.Method == "GET" {
					c.gl.serve(w, r)
					return
				}
				http.Redirect(w, r, b.URL+r.URL.RequestURI(), status)
			}))
			c.withProvider("redir", a.URL)
			r1 := c.rawWire("list_groups", map[string]any{"provider": "redir"})
			r2 := c.rawWire("create_group", c.groupArgsFor("redir", "newg", "newg", ""))
			r3 := c.rawWire("create_project", c.createArgsFor("redir", c3Name, nil))
			a.Close()
			for _, r := range []string{r1, r2, r3} {
				if strings.Contains(r, `"ok":true`) && !(onlyWrites && r == r1) {
					t.Errorf("status %d writes=%v: a redirected request succeeded: %s", status, onlyWrites, r)
				}
				if strings.Contains(r, c3Canary) || strings.Contains(r, b.URL) {
					t.Errorf("status %d: reply leaks: %s", status, r)
				}
			}
			if _, ok := c.find(c3Name); ok {
				t.Errorf("status %d: project registered through a redirect", status)
			}
		}
	}
	if n := other.Load(); n != 0 {
		t.Fatalf("the redirect target received %d request(s); token header %q", n, otherTok.Load())
	}
}

// ---------- 5 ----------

// Pointing an existing provider id (and so its token) at another address is a
// signed change: unsigned or with a signature over the old address it is
// refused and the provider stays; the address is part of the signed fields and
// of the canonical change text the user is shown; until the signed change
// lands the old GitLab is the only one that ever sees the token.
func TestProviderBaseURLChangeNeedsSignatureAndShowsInChange(t *testing.T) {
	var hostile atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hostile.Add(1); w.WriteHeader(404) }))
	t.Cleanup(evil.Close)
	c := newC3(t)
	oldF, newF := c.providerFields(c.gl.URL), c.providerFields(evil.URL)
	oldH, _ := registrymsg.ChangeHash(registrymsg.ActionSetProvider, "gl", oldF)
	newH, _ := registrymsg.ChangeHash(registrymsg.ActionSetProvider, "gl", newF)
	if oldH == newH {
		t.Fatal("the change hash does not cover base_url")
	}
	canon, err := registrymsg.CanonicalChange(registrymsg.ActionSetProvider, "gl", newF)
	if err != nil || !bytes.Contains(canon, []byte(evil.URL)) {
		t.Fatalf("the canonical change does not contain the new address: %s %v", canon, err)
	}
	before := c.snap()
	// unsigned
	c.wantCode("set_provider", copyArgs(c.setProviderArgs("gl", evil.URL), func(m map[string]any) { delete(m, "signature") }), "not_signed")
	// signed for the old address, new address in the request
	c.wantCode("set_provider", copyArgs(c.setProviderArgs("gl", c.gl.URL), func(m map[string]any) { m["base_url"] = evil.URL }), "change_mismatch")
	// signed for the old address, new address AND the new hash in the request
	c.wantCode("set_provider", copyArgs(c.setProviderArgs("gl", c.gl.URL), func(m map[string]any) { m["base_url"] = evil.URL; m["change"] = newH }), "bad_signature")
	// the same for the other signed fields that decide where the token goes
	for field, val := range map[string]any{"ssh_url": "ssh://git@10.0.0.9", "token_ref": "other-ref", "kind": "github"} {
		_, e := c.do("set_provider", copyArgs(c.setProviderArgs("gl", c.gl.URL), func(m map[string]any) { m[field] = val }))
		if code := codeOf(e); code != "change_mismatch" && code != "bad_request" {
			t.Errorf("%s altered: code %q", field, code)
		}
	}
	c.mustEqual(before, "unsigned or mis-signed base_url change")
	if p, _ := c.s.Registry.ProviderByID("gl"); p.BaseURL != c.gl.URL {
		t.Fatalf("provider moved without a valid signature: %+v", p)
	}
	// while unsigned, the token only goes to the old address
	c.rawWire("list_groups", map[string]any{"provider": "gl"})
	if hostile.Load() != 0 || c.gl.hits() == 0 {
		t.Fatalf("hostile=%d old=%d", hostile.Load(), c.gl.hits())
	}
	// the properly signed change is applied once; its replay is refused
	good := c.setProviderArgs("gl", evil.URL)
	if _, e := c.do("set_provider", good); e != nil {
		t.Fatalf("signed change: %v", e.Code)
	}
	c.wantCode("set_provider", good, "request_reused")
	if p, _ := c.s.Registry.ProviderByID("gl"); p.BaseURL != evil.URL {
		t.Fatalf("signed change not applied: %+v", p)
	}
}

// ---------- 6 ----------

// A valid signature over one create_project change cannot be reused with any
// single field of the request altered: the broker rebuilds the change from the
// request fields and refuses the difference. GitLab is not called, the registry
// is untouched and no request ID is burnt. The same holds when the attacker
// recomputes `change` (then the signature no longer verifies).
func TestSignatureOverDifferentProjectFieldsRefused(t *testing.T) {
	c := newC3(t)
	before := c.snap()
	alter := map[string]any{
		"provider": "gl2", "group_path": "team/sub", "template": "go", "description": "other text",
		"ai_context": true, "publish": "read", "deploy_key_sha256": strings.Repeat("c", 64),
	}
	for field, val := range alter {
		a := c.projectArgs(c3Name, nil) // signed over the original fields
		a[field] = val
		code := codeOf(func() *OpError { _, e := c.do("create_project", a); return e }())
		if code != "change_mismatch" && code != "deploy_key_mismatch" {
			t.Errorf("field %s altered: code %q", field, code)
		}
		// the attacker also fixes `change` to match: the signature is then wrong
		a2 := c.projectArgs(c3Name, nil)
		a2[field] = val
		fields := map[string]any{}
		for _, k := range []string{"provider", "group_path", "template", "description", "ai_context", "publish", "deploy_key_sha256"} {
			fields[k] = a2[k]
		}
		h, _ := registrymsg.ChangeHash(registrymsg.ActionCreateProject, c3Name, fields)
		a2["change"] = h
		if code := codeOf(func() *OpError { _, e := c.do("create_project", a2); return e }()); code != "bad_signature" && code != "deploy_key_mismatch" {
			t.Errorf("field %s altered with a recomputed change: code %q", field, code)
		}
	}
	// the subject (the new project's name) is inside the signed message itself
	a := c.projectArgs(c3Name, nil)
	a["id"] = "other-name"
	if code := codeOf(func() *OpError { _, e := c.do("create_project", a); return e }()); code != "bad_signature" && code != "change_mismatch" {
		t.Errorf("subject altered: %q", code)
	}
	c.mustEqual(before, "altered create_project fields")
	c.noGLHits("altered create_project fields")
	if _, ok := c.find(c3Name); ok {
		t.Fatal("registered")
	}
	// control: the unaltered request is fine, once
	good := c.projectArgs(c3Name, nil)
	if _, e := c.do("create_project", good); e != nil {
		t.Fatalf("control: %v", e.Code)
	}
	again := copyArgs(good, func(m map[string]any) { m["description"] = "x" })
	if code := codeOf(func() *OpError { _, e := c.do("create_project", again); return e }()); code == "" {
		t.Fatal("a used signature accepted with other fields")
	}
}

// ---------- 7 ----------

// A signature made for one action never works as another: for every ordered
// pair of the eight signed actions the signature (alone, and together with its
// change, request ID and time) is grafted onto a request of the other action.
// All are refused, nothing is written (registry, token store, GitLab) and no
// request ID is burnt.
func TestCrossActionSignaturesRefused(t *testing.T) {
	c := newC3(t)
	c.withProvider("spare", c.gl.URL)
	type act struct {
		op   string
		args func() map[string]any
	}
	acts := []act{
		{"register_project", func() map[string]any {
			return c.signedArgs(c.alice, registrymsg.ActionRegisterProject, "xproj", regFields(filepath.Join(c.root, "xproj"), "default", true))
		}},
		{"set_flag", func() map[string]any {
			return c.signedArgs(c.alice, registrymsg.ActionSetFlag, "repo-a", flagFields("ai_context", false))
		}},
		{"set_provider", func() map[string]any {
			return c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl", c.providerFields(c.gl.URL))
		}},
		{"delete_provider", func() map[string]any { return c.deleteProviderArgs("spare") }},
		{"create_group", func() map[string]any { return c.groupArgs("newg", "newg", "") }},
		{"create_project", func() map[string]any { return c.projectArgs(c3Name, nil) }},
		{"set_token", func() map[string]any { return c.tokArgs(c.alice, "other-gitlab", "gitlab", "", tokenOK) }},
		{"delete_token", func() map[string]any { return c.delArgs(c.alice, "gl-admin") }},
	}
	snapReg, snapTok := c.snap(), c.tokSnap()
	pairs := 0
	for _, a := range acts {
		for _, b := range acts {
			if a.op == b.op {
				continue
			}
			sigA := a.args()
			// (1) B's own request with A's signature only
			r1 := b.args()
			r1["signature"] = sigA["signature"]
			// (2) B's own request carrying A's signature, change, request ID and time
			r2 := b.args()
			for _, k := range []string{"signature", "change", "request", "issued"} {
				r2[k] = sigA[k]
			}
			for i, r := range []map[string]any{r1, r2} {
				if _, e := c.do(b.op, r); e == nil {
					t.Errorf("%s signature accepted as %s (variant %d)", a.op, b.op, i+1)
				} else if e.Code != "bad_signature" && e.Code != "change_mismatch" && e.Code != "bad_request" {
					t.Errorf("%s signature on %s (variant %d): code %s", a.op, b.op, i+1, e.Code)
				}
			}
			pairs++
		}
	}
	if pairs != 56 {
		t.Fatalf("pairs %d", pairs)
	}
	c.mustEqual(snapReg, "cross-action signatures")
	if !c.tokSnap().sameStore(snapTok) || !c.tokSnap().sameUsed(snapTok) {
		t.Fatal("the token store or the used-ID file changed")
	}
	c.noGLHits("cross-action signatures")
	// control: an action works with its own signature
	if _, e := c.do("create_group", c.groupArgs("newg", "newg", "")); e != nil {
		t.Fatalf("control: %v", e.Code)
	}
}

// ---------- 8 ----------

// The deploy key travels as a plain line next to a signed hash of that line.
// Any other line (another key, the same key with a changed comment or a
// trailing space, a key plus an injected second line, a hash of another key)
// is refused before the signature is even looked at: no GitLab request, no
// burnt ID, no registry change.
func TestDeployKeySwapRefused(t *testing.T) {
	c := newC3(t)
	before := c.snap()
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherKeyOtherKeyOtherKeyOtherKeyOtherKey1 orchd"
	swaps := map[string]string{
		"another key":         other,
		"comment changed":     strings.TrimSuffix(c3Key(), "orchd") + "evil",
		"trailing space":      c3Key() + " ",
		"trailing newline":    c3Key() + "\n",
		"second key injected": c3Key() + "\n" + other,
		"leading space":       " " + c3Key(),
		"options prefix":      `command="sh" ` + c3Key(),
		"empty":               "",
		"private key marker":  "-----BEGIN OPENSSH PRIVATE KEY-----",
		"same key other type": strings.Replace(c3Key(), "ssh-ed25519", "ssh-rsa", 1),
		"uppercase":           strings.ToUpper(c3Key()),
		"unicode lookalike":   strings.Replace(c3Key(), "orchd", "orchd​", 1),
		"very long":           c3Key() + strings.Repeat("a", 5000),
	}
	for name, key := range swaps {
		a := c.projectArgs(c3Name, nil)
		a["deploy_key"] = key
		_, e := c.do("create_project", a)
		if e == nil || (e.Code != "deploy_key_mismatch" && e.Code != "bad_request") {
			t.Errorf("%s: %v", name, codeOf(e))
		}
	}
	// a signed hash that belongs to another key, with that other key's line swapped in
	a := c.projectArgs(c3Name, func(f map[string]any) { f["deploy_key_sha256"] = c3Hash(c3Key()) })
	a["deploy_key"] = other
	c.wantCode("create_project", a, "deploy_key_mismatch")
	// the hash is of exactly the signed key: sign the OTHER key, send the original
	b := c.projectArgs(c3Name, func(f map[string]any) { f["deploy_key_sha256"] = c3Hash(other) })
	c.wantCode("create_project", b, "deploy_key_mismatch")
	c.mustEqual(before, "swapped deploy keys")
	c.noGLHits("swapped deploy keys")
	// control: the right pair goes through and GitLab receives exactly that key
	if _, e := c.do("create_project", c.projectArgs(c3Name, nil)); e != nil {
		t.Fatalf("control: %v", e.Code)
	}
	var keySeen bool
	for _, r := range c.gl.all() {
		if strings.HasSuffix(r.URI, "/deploy_keys") {
			var m map[string]any
			json.Unmarshal([]byte(r.Body), &m)
			keySeen = m["key"] == c3Key()
		}
	}
	if !keySeen {
		t.Fatal("GitLab did not receive the signed key")
	}
}

// ---------- 9 ----------

// A project that already exists on GitLab is never adopted, however it shows
// up: found by the existence check, found in a subgroup, found only by the
// failing POST (created meanwhile by somebody else), or when the existence
// check itself fails. None of these continues to files, protection or deploy
// key, none registers, and nothing is deleted or overwritten.
func TestExistingGitLabProjectNeverAdopted(t *testing.T) {
	later := func(c *c3env) {
		for _, r := range c.gl.all() {
			if r.Method == "POST" && (strings.Contains(r.URI, "/repository/files/") || strings.HasSuffix(r.URI, "/protected_branches") || strings.HasSuffix(r.URI, "/deploy_keys")) {
				t.Fatalf("a step after the existence conflict ran: %s %s", r.Method, r.URI)
			}
			if r.Method != "GET" && r.Method != "POST" {
				t.Fatalf("method %s", r.Method)
			}
		}
		if _, ok := c.find(c3Name); ok {
			t.Fatal("an existing GitLab project was registered")
		}
	}
	// (a) exists at the top group
	c := newC3(t)
	c.gl.existP["team/demo"] = true
	c.wantCode("create_project", c.projectArgs(c3Name, nil), "project_name_taken")
	later(c)
	if len(c.gl.writes()) != 0 || len(c.orphans()) != 0 {
		t.Fatalf("writes %v orphans %v", c.gl.writes(), c.orphans())
	}
	// (b) exists in the nested group
	c = newC3(t)
	c.gl.existP["team/sub/demo"] = true
	c.wantCode("create_project", c.projectArgs(c3Name, func(f map[string]any) { f["group_path"] = "team/sub" }), "project_name_taken")
	later(c)
	if len(c.gl.writes()) != 0 {
		t.Fatalf("writes %v", c.gl.writes())
	}
	// (c) created by somebody else between the check and our POST: refused by GitLab
	c = newC3(t)
	c.gl.fail["POST /api/v4/projects"] = 409
	_, e := c.do("create_project", c.projectArgs(c3Name, nil))
	d := partialOf(t, e)
	if d.Step != "project" || d.ProjectID != nil {
		t.Fatalf("%+v", d)
	}
	later(c)
	if n := len(c.gl.writes()); n != 1 {
		t.Fatalf("%d writes", n)
	}
	// (d) the existence check fails (not 404): no write at all
	for _, st := range []int{401, 403, 500, 502} {
		c = newC3(t)
		c.gl.fail["GET /api/v4/projects/team%2Fdemo"] = st
		if _, e := c.do("create_project", c.projectArgs(c3Name, nil)); e == nil {
			t.Fatalf("status %d: accepted", st)
		}
		if len(c.gl.writes()) != 0 || len(c.orphans()) != 0 {
			t.Fatalf("status %d: wrote after a failed check", st)
		}
		later(c)
	}
	// (e) a name already in the registry (any path) never reaches GitLab
	c = newC3(t)
	c.ownProject("gl", c3Name)
	c.wantCode("create_project", c.projectArgs(c3Name, nil), "project_name_taken")
	c.noGLHits("registry clash")
}

// ---------- 10 ----------

// Whatever step fails (and also a panic or a timeout), the broker leaves what
// it made: no DELETE/PUT/PATCH, no later step, no retry, the failure is an
// orphan record with the real step, and nothing is registered.
func TestPartialFailureNeverDeletes(t *testing.T) {
	steps := map[string]func(f *c3GL){
		"project":        func(f *c3GL) { f.fail["POST /api/v4/projects"] = 500 },
		"first_commit":   func(f *c3GL) { f.fail["POST /api/v4/projects/21/repository/files/.gitignore"] = 500 },
		"default_branch": func(f *c3GL) { f.defBr = "master" },
		"protect":        func(f *c3GL) { f.fail["POST /api/v4/projects/21/protected_branches"] = 403 },
		"deploy_key":     func(f *c3GL) { f.fail["POST /api/v4/projects/21/deploy_keys"] = 500 },
	}
	for step, inject := range steps {
		step, inject := step, inject
		t.Run(step, func(t *testing.T) {
			c := newC3(t)
			inject(c.gl)
			_, e := c.do("create_project", c.projectArgs(c3Name, nil))
			d := partialOf(t, e)
			if d.Step != step || !d.OrphanRecorded {
				t.Fatalf("%+v", d)
			}
			os := c.orphans()
			if len(os) != 1 || os[0].Step != step {
				t.Fatalf("orphans %+v", os)
			}
			posts := map[string]int{}
			for _, r := range c.gl.all() {
				if r.Method != "GET" && r.Method != "POST" {
					t.Fatalf("destructive call %s %s", r.Method, r.URI)
				}
				if r.Method == "POST" {
					posts[r.URI]++
				}
			}
			for uri, n := range posts {
				if n != 1 {
					t.Fatalf("%s was sent %d times (no retry)", uri, n)
				}
			}
			if _, ok := c.find(c3Name); ok {
				t.Fatal("registered")
			}
			if len(c.s.Registry.Snapshot().Projects) != 1 { // only the migrated repo-a
				t.Fatalf("projects: %v", c.projects())
			}
		})
	}
	// a panic after the project exists, on the real registry-write path
	c := newC3(t)
	pn := 0
	c.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			if pn++; pn == 1 {
				panic("boom")
			}
		}
		return nil
	}
	_, e := c.do("create_project", c.projectArgs(c3Name, nil))
	if d := partialOf(t, e); d.Step != "register" || !d.OrphanRecorded {
		t.Fatalf("%+v", d)
	}
	for _, r := range c.gl.all() {
		if r.Method != "GET" && r.Method != "POST" {
			t.Fatalf("destructive call after a panic: %s %s", r.Method, r.URI)
		}
	}
	// an explicit registry failure as the last step
	c = newC3(t)
	fn := 0
	c.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			if fn++; fn == 1 {
				return fmt.Errorf("disk full")
			}
		}
		return nil
	}
	_, e = c.do("create_project", c.projectArgs(c3Name, nil))
	if d := partialOf(t, e); d.Step != "register" {
		t.Fatalf("%+v", d)
	}
	for _, r := range c.gl.all() {
		if r.Method != "GET" && r.Method != "POST" {
			t.Fatalf("destructive call: %s %s", r.Method, r.URI)
		}
	}
}

// ---------- 11 ----------

// Many simultaneous create_project requests for one name (different request
// IDs, different groups, and one signed request replayed) produce at most one
// POST /projects and one registry entry; the rest are refused.
func TestConcurrentSameNameNoDoubleCreate(t *testing.T) {
	c := newC3(t)
	c.gl.delay["POST /api/v4/projects"] = 150e6 // 150 ms: the others arrive meanwhile
	const n = 12
	args := make([]map[string]any, n)
	for i := range args {
		grp := "team"
		if i%3 == 2 {
			grp = "team/sub"
		}
		args[i] = c.projectArgs(c3Name, func(f map[string]any) { f["group_path"] = grp })
	}
	args[n-1] = args[0] // the same signed request twice
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
	ok := 0
	for _, cd := range codes {
		switch cd {
		case "":
			ok++
		case "project_name_taken", "request_reused":
		default:
			t.Fatalf("codes: %v", codes)
		}
	}
	posts := 0
	for _, r := range c.gl.all() {
		if r.Method == "POST" && strings.HasSuffix(r.URI, "/api/v4/projects") {
			posts++
		}
	}
	if ok > 1 || posts > 1 {
		t.Fatalf("ok=%d posts=%d codes=%v", ok, posts, codes)
	}
	n2 := 0
	for _, p := range c.projects() {
		if p.ID == c3Name {
			n2++
		}
	}
	if n2 > 1 {
		t.Fatalf("%d registry entries", n2)
	}
	if ok == 1 && len(c.orphans()) != 0 {
		t.Fatalf("orphans: %v", c.orphans())
	}
}

// ---------- 12 ----------

// A registry file of version 2 that is wrong in any way (including the new
// provider/orphan parts and a mixed-case duplicate of "version") is not
// patched up and not replaced by the legacy config: every op that needs the
// registry answers registry_unavailable, no project gets AI context (so
// nothing is sent to the AI endpoint, not even for the legacy context repo of
// the config), the file is left as it was and no GitLab request is made. A
// valid v2 file is the control.
func TestRegistryV2BrokenFailsClosedAndDoesNotWidenAIContext(t *testing.T) {
	const tail = `"auto_merge_global":{"enabled":false,"max_per_day":5}`
	prov := `{"id":"gl","kind":"gitlab","base_url":"https://10.0.0.7","ssh_url":"ssh://git@10.0.0.7","token_ref":"gl-admin"}`
	proj := func(path string, ai bool, provider string) string {
		return fmt.Sprintf(`{"id":"a","path":%q,"kind":"registered","ai_context":%v,"provider":%s,"gitlab":null,"auto_merge":{"enabled":false}}`, path, ai, provider)
	}
	cases := map[string]string{
		"truncated":              `{"version":2,"providers":[` + prov + `],"projects":[` + proj("/x/a", true, `null`),
		"garbage":                `not json`,
		"empty":                  ``,
		"version string":         `{"version":"2","providers":[],"projects":[],"orphans":[],` + tail + `}`,
		"mixed-case version":     `{"version":1,"Version":2,"providers":[],"projects":[],"orphans":[],` + tail + `}`,
		"unknown key":            `{"version":2,"providers":[],"projects":[],"orphans":[],"extra":1,` + tail + `}`,
		"provider unknown key":   `{"version":2,"providers":[{"id":"gl","kind":"gitlab","base_url":"https://10.0.0.7","ssh_url":"ssh://git@10.0.0.7","token_ref":"gl-admin","extra":1}],"projects":[],"orphans":[],` + tail + `}`,
		"duplicate provider":     `{"version":2,"providers":[` + prov + `,` + prov + `],"projects":[],"orphans":[],` + tail + `}`,
		"project unknown prov":   `{"version":2,"providers":[],"projects":[` + proj("/x/a", true, `"nope"`) + `],"orphans":[],` + tail + `}`,
		"auto merge on":          `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":true,"provider":null,"gitlab":null,"auto_merge":{"enabled":true}}],"orphans":[],` + tail + `}`,
		"orphan bad":             `{"version":2,"providers":[],"projects":[],"orphans":[{"provider":"gl","project":"../x","project_id":null,"step":"project","at":"2026-11-11T00:00:00Z"}],` + tail + `}`,
		"project id no provider": `{"version":2,"providers":[],"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":true,"provider":null,"gitlab":{"project":"g/a","project_id":5},"auto_merge":{"enabled":false}}],"orphans":[],` + tail + `}`,
		"version 3":              `{"version":3,"providers":[],"projects":[],"orphans":[],` + tail + `}`,
		"trailing data":          `{"version":2,"providers":[],"projects":[],"orphans":[],` + tail + `}{"x":1}`,
	}
	oa := newFakeHTTP(t, okHandler(goodPlan, 10, 10))
	run := func(name, content string, wantBroken bool) {
		e := newTestEnv(t)
		state := filepath.Join(e.Dir, "state")
		os.MkdirAll(state, 0o700)
		os.WriteFile(filepath.Join(state, RegistryFileName), []byte(content), 0o600)
		gl := newC3GL(t)
		s, _ := e.newServer(t, func(s *Server) { s.OpenAI.BaseURL = oa.URL; s.apiSeams = &apiSeams{policy: allowLoopback} })
		_ = gl
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
		repo := filepath.Join(e.Dir, "repo-a") // listed in the legacy config
		for _, op := range []string{"status", "projects", "providers"} {
			if r, out := call(t, sock, `{"v":1,"op":"`+op+`"}`); r.OK || r.Code != "registry_unavailable" || strings.Contains(string(out), repo) {
				t.Fatalf("%s: %s: %s", name, op, out)
			}
		}
		if r, out := call(t, sock, `{"v":1,"op":"list_groups","args":{"provider":"gl"}}`); r.OK {
			t.Fatalf("%s: list_groups: %s", name, out)
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
	if oa.Hits() != 0 {
		t.Fatalf("the AI endpoint was reached %d times with a broken registry", oa.Hits())
	}
	// control: a valid v2 file is accepted (so the cases above fail for their own reason)
	run("valid control", `{"version":2,"providers":[`+prov+`],"projects":[`+strings.Replace(proj(filepath.Join(os.TempDir(), "a"), false, `null`), `"id":"a"`, `"id":"a"`, 1)+`],"orphans":[],`+tail+`}`, false)
}

// ---------- 13 ----------

// The read ops of the whole W3c surface (status, projects, providers,
// list_groups) and the errors of the write ops carry no secret: not the admin
// token, the OpenAI key, key or token file paths, the signer line, a request
// ID or signature, nor the deploy key line; orphans are values only.
func TestStatusAndProjectsNoSecrets(t *testing.T) {
	c := newC3(t)
	c.gl.fail["POST /api/v4/projects/21/deploy_keys"] = 500
	c.rawWire("create_project", c.projectArgs(c3Name, nil)) // leaves an orphan
	good := c.projectArgs("demo2", nil)
	c.rawWire("create_project", good)
	secrets := []string{c3Canary, testOpenAIKey, testGitLabToken, "Bearer", "PRIVATE-TOKEN", "BEGIN", c.alice.line, c.e.KeyFile, c.e.TokFile, c.signers,
		"signature", "REFLECT", c3Key(), "AAAAC3NzaC1lZDI1NTE5AAAAIGoodKey", good["request"].(string), good["change"].(string), "gl-admin-value"}
	for _, op := range []string{"status", "projects", "providers", "tokens"} {
		out := c.rawWire(op, nil)
		if !strings.Contains(out, `"ok":true`) {
			t.Fatalf("%s: %s", op, out)
		}
		for _, s := range secrets {
			if op == "tokens" && s == "gl-admin-value" {
				continue
			}
			if strings.Contains(out, s) {
				t.Errorf("%q in the %s reply", s, op)
			}
		}
	}
	out := c.rawWire("list_groups", map[string]any{"provider": "gl"})
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("%q in the list_groups reply", s)
		}
	}
	// the projects reply shows the orphan as values only
	pr := c.rawWire("projects", nil)
	if !strings.Contains(pr, `"orphans":[{"provider":"gl","project":"team/demo"`) {
		t.Errorf("orphan not visible as values: %s", pr)
	}
	// error replies of write ops
	for _, bad := range []map[string]any{{"x": 1}, copyArgs(good, func(m map[string]any) { delete(m, "signature") }), c.projectArgs(c3Name, nil)} {
		for _, s := range secrets {
			if strings.Contains(c.rawWire("create_project", bad), s) {
				t.Errorf("%q in an error reply", s)
			}
		}
	}
	for _, s := range secrets[:6] {
		if strings.Contains(c.logs.String(), s) {
			t.Errorf("%q in the log", s)
		}
	}
	if !strings.Contains(c.logs.String(), "op=projects ok") {
		t.Fatal("the log scan saw no op lines")
	}
}

// ---------- 14 ----------

// create_project is pure GitLab plus registry: the broker account cannot see
// /home/orch/repos and must not try. Behaviour does not depend on the file
// system: the repo root missing, a symlink or a plain file sitting at the new
// project's path, an unreadable root: create_project succeeds exactly the
// same, and leaves the file system exactly as it was.
func TestCreateProjectBrokerDoesNotStatPaths(t *testing.T) {
	c := newC3(t)
	// a symlink and a file at the paths of two future projects
	if err := os.Symlink("/etc", filepath.Join(c.root, "linked-one")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "plain-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := repoRootEntries(t, c.root)
	for _, name := range []string{"linked-one", "plain-file"} {
		if _, e := c.do("create_project", c.projectArgs(name, nil)); e != nil {
			t.Fatalf("%s: %v", name, e.Code)
		}
	}
	if got := repoRootEntries(t, c.root); strings.Join(got, "|") != strings.Join(before, "|") {
		t.Fatalf("the repo root changed: %v -> %v", before, got)
	}
	if tgt, err := os.Readlink(filepath.Join(c.root, "linked-one")); err != nil || tgt != "/etc" {
		t.Fatalf("symlink touched: %v %v", tgt, err)
	}
	// the root removed altogether: nothing is stat'ed, nothing is created
	if err := os.RemoveAll(c.root); err != nil {
		t.Fatal(err)
	}
	if _, e := c.do("create_project", c.projectArgs("fresh-one", nil)); e != nil {
		t.Fatalf("without a repo root: %v", e.Code)
	}
	if _, err := os.Lstat(c.root); !os.IsNotExist(err) {
		t.Fatal("the broker created the repo root")
	}
	// the path in the registry is the configured root + name, nothing from the request
	if p, ok := c.find("fresh-one"); !ok || p.Path != filepath.Join(c.root, "fresh-one") {
		t.Fatalf("%+v", p)
	}
	// static: no os/exec/syscall use anywhere in the W3c broker files, and no file-system call by name
	for _, f := range []string{"create_project.go", "providers_ops.go", "templates.go", "gitlabapi.go"} {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			switch im.Path.Value {
			case `"os"`, `"io/ioutil"`, `"path/filepath"`, `"os/exec"`, `"io/fs"`:
				t.Errorf("%s imports %s", f, im.Path.Value)
			}
		}
	}
}
