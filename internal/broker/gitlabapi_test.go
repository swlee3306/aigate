package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
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
	"time"
)

const apiCanary = "canary-NOT-A-REAL-TOKEN-0123456789abcdefXYZ"

// allowLoopback is the TEST-ONLY policy that lets the fake server (127.0.0.1)
// through; everything the production policy refuses stays refused.
func allowLoopback(a netip.Addr) bool { return a.Unmap().IsLoopback() || defaultIPPolicy(a) }

// fakeGL records METHOD + RequestURI of every request and answers like a small
// GitLab. fail maps "METHOD path-prefix" to a status; body overrides a body.
type fakeGL struct {
	*httptest.Server
	mu       sync.Mutex
	reqs     []string
	headers  []http.Header
	bodies   []string
	fail     map[string]int
	override func(w http.ResponseWriter, r *http.Request) bool
	empty    bool // GET protected_branches/main -> 404 until created
	created  bool
}

func newFakeGL(t *testing.T) *fakeGL {
	t.Helper()
	f := &fakeGL{fail: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGL) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.RequestURI)
	f.headers = append(f.headers, r.Header.Clone())
	f.bodies = append(f.bodies, string(b))
	ov := f.override
	f.mu.Unlock()
	if ov != nil && ov(w, r) {
		return
	}
	key := r.Method + " " + r.URL.EscapedPath()
	for k, st := range f.fail {
		if strings.HasPrefix(key, k) {
			w.WriteHeader(st)
			fmt.Fprint(w, `{"message":"`+apiCanary+`-REFLECT"}`)
			return
		}
	}
	p := r.URL.EscapedPath()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && p == "/api/v4/groups":
		fmt.Fprint(w, `[{"id":7,"name":"team","path":"team","full_path":"team","extra":1},{"id":8,"name":"Bad","path":"Bad","full_path":"Bad"},{"id":9,"name":"sub","path":"sub","full_path":"team/sub"}]`)
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v4/groups/"):
		full := strings.ReplaceAll(strings.TrimPrefix(p, "/api/v4/groups/"), "%2F", "/")
		seg := full[strings.LastIndex(full, "/")+1:]
		fmt.Fprintf(w, `{"id":7,"name":%q,"path":%q,"full_path":%q}`, seg, seg, full)
	case r.Method == "POST" && p == "/api/v4/groups":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":11,"name":"newg","path":"newg","full_path":"newg"}`)
		if strings.Contains(string(b), "parent_id") {
			// replaced below by tests that need the nested answer
		}
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v4/projects/") && strings.Count(p, "/") == 4:
		full := strings.ReplaceAll(strings.TrimPrefix(p, "/api/v4/projects/"), "%2F", "/")
		fmt.Fprintf(w, `{"id":21,"path_with_namespace":%q}`, full)
	case r.Method == "POST" && p == "/api/v4/projects":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":21,"path_with_namespace":"team/demo"}`)
	case r.Method == "POST" && strings.Contains(p, "/repository/files/"):
		w.WriteHeader(201)
		fp := strings.ReplaceAll(p[strings.Index(p, "/repository/files/")+len("/repository/files/"):], "%2F", "/")
		fmt.Fprintf(w, `{"file_path":%q,"branch":"main"}`, fp)
	case r.Method == "GET" && strings.Contains(p, "/protected_branches/"):
		f.mu.Lock()
		missing := f.empty && !f.created
		f.mu.Unlock()
		if missing {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"name":"main","allow_force_push":false,"push_access_levels":[{"access_level":40}],"merge_access_levels":[{"access_level":40}]}`)
	case r.Method == "POST" && strings.HasSuffix(p, "/protected_branches"):
		f.mu.Lock()
		f.created = true
		f.mu.Unlock()
		w.WriteHeader(201)
		fmt.Fprint(w, `{"name":"main"}`)
	case r.Method == "POST" && strings.HasSuffix(p, "/deploy_keys"):
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":5,"title":"x"}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGL) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeGL) hits() int { return len(f.recorded()) }

func apiTokens(t *testing.T) *TokenStore {
	t.Helper()
	ts, _ := newTestTokenStore(t)
	if err := ts.Set("gl-admin", "gitlab", "", apiCanary, tokNow); err != nil {
		t.Fatal(err)
	}
	return ts
}

func apiFor(t *testing.T, f *fakeGL, ts *TokenStore, sm *apiSeams) *GitLabAPI {
	t.Helper()
	if sm == nil {
		sm = &apiSeams{}
	}
	if sm.policy == nil {
		sm.policy = allowLoopback
	}
	a, err := newGitLabAPI(Provider{ID: "gl", Kind: "gitlab", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}, ts, sm)
	if err != nil {
		t.Fatalf("newGitLabAPI: %v", err)
	}
	return a
}

const goodKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGoodKeyGoodKeyGoodKeyGoodKeyGoodKeyGood orchd"

// runAll calls every public method (success and failure paths).
func runAll(t *testing.T, f *fakeGL, a *GitLabAPI) {
	t.Helper()
	ctx := context.Background()
	a.Groups(ctx)
	a.Group(ctx, "team/sub")
	a.CreateGroup(ctx, "newg", "newg", "")
	a.CreateGroup(ctx, "newg", "newg", "team")
	a.Project(ctx, "team/demo")
	a.CreateProject(ctx, "team", 7, "demo", "d")
	a.CommitFiles(ctx, 21, []CommitFile{{"README.md", "x"}, {".gitignore", "y"}, {"cmd/main.go", "z"}})
	a.ProtectedMain(ctx, 21)
	f.mu.Lock()
	f.empty = true
	f.mu.Unlock()
	a.EnsureProtectedMain(ctx, 21) // 404 then POST
	a.AddDeployKey(ctx, 21, "orchd demo", goodKey, true)
	// failure paths
	for _, st := range []int{401, 403, 404, 409, 422, 500, 302} {
		f.fail = map[string]int{"GET /api/v4": st, "POST /api/v4": st}
		a.Groups(ctx)
		a.Group(ctx, "team")
		a.CreateGroup(ctx, "newg", "newg", "")
		a.Project(ctx, "team/demo")
		a.CreateProject(ctx, "team", 7, "demo", "")
		a.CommitFiles(ctx, 21, []CommitFile{{"README.md", "x"}})
		a.EnsureProtectedMain(ctx, 21)
		a.AddDeployKey(ctx, 21, "t", goodKey, false)
	}
	f.fail = map[string]int{}
}

func TestOnlyAllowlistedRequestsReachGitLab(t *testing.T) {
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	runAll(t, f, a)
	seen := map[string]bool{}
	for _, r := range f.recorded() {
		method, uri, _ := strings.Cut(r, " ")
		pth, rawq, _ := strings.Cut(uri, "?")
		q, _ := url.ParseQuery(rawq)
		if !allowed(method, pth, q) {
			t.Fatalf("request outside the allowlist reached GitLab: %s", r)
		}
		if method == "DELETE" || method == "PUT" || method == "PATCH" || method == "HEAD" {
			t.Fatalf("forbidden method: %s", r)
		}
		for _, bad := range []string{"merge", "token", "/user", "access_tokens", "/commits", "approve", "unprotect"} {
			if strings.Contains(strings.ToLower(pth), bad) {
				t.Fatalf("forbidden path piece %q: %s", bad, r)
			}
		}
		for i, e := range allowlist {
			if e.method == method && e.path.MatchString(pth) {
				seen[fmt.Sprint(i)] = true
			}
		}
	}
	for i, e := range allowlist {
		if !seen[fmt.Sprint(i)] {
			t.Errorf("allowlist entry %d (%s %s) was never exercised", i, e.method, e.path)
		}
	}
	if len(allowlist) != 9 {
		t.Fatalf("allowlist has %d entries, want exactly 9", len(allowlist))
	}
	for _, e := range allowlist {
		if e.method != "GET" && e.method != "POST" {
			t.Fatalf("allowlist method %s", e.method)
		}
	}
}

func TestDoRefusesOffListRequests(t *testing.T) {
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	cases := []struct{ m, p, q string }{
		{"DELETE", "/api/v4/projects/21", ""},
		{"DELETE", "/api/v4/groups/team", ""},
		{"DELETE", "/api/v4/projects/21/protected_branches/main", ""},
		{"PUT", "/api/v4/projects/21", ""},
		{"PATCH", "/api/v4/projects/21/protected_branches/main", ""},
		{"PUT", "/api/v4/groups/team", ""},
		{"POST", "/api/v4/projects/21/merge_requests", ""},
		{"POST", "/api/v4/projects/21/merge_requests/1/merge", ""},
		{"PUT", "/api/v4/projects/21/merge_requests/1/merge", ""},
		{"GET", "/api/v4/user", ""},
		{"GET", "/api/v4/personal_access_tokens", ""},
		{"POST", "/api/v4/projects/21/access_tokens", ""},
		{"POST", "/api/v4/users/1/personal_access_tokens", ""},
		{"GET", "/api/v4/projects", ""},
		{"GET", "/api/v4/projects/21", ""},
		{"POST", "/api/v4/projects/21/repository/commits", ""},
		{"POST", "/api/v4/projects/21/deploy_keys/1/enable", ""},
		{"GET", "/api/v4/projects/21/deploy_keys", ""},
		{"POST", "/api/v4/projects/21/protected_branches/main", ""},
		{"GET", "/api/v4/projects/21/protected_branches", ""},
		{"GET", "/api/v4/groups", "owned=true"},
		{"GET", "/api/v4/groups", "search=x"},
		{"POST", "/api/v4/groups", "x=1"},
		{"GET", "/api/v4/groups/team%2f", ""},
		{"GET", "/api/v4/groups/team%2fsub", ""},
		{"GET", "/api/v4/groups/../users", ""},
		{"GET", "/api/v4/groups/team/subgroups", ""},
		{"GET", "/api/v4/groups/Team", ""},
		{"GET", "/api/v4/groups/a%2Fb%2Fc%2Fd%2Fe%2Ff", ""},
		{"POST", "/api/v4/projects/21/repository/files/%2E%2E", ""},
		{"POST", "/api/v4/projects/21/repository/files/a%2F..%2Fb", ""},
		{"POST", "/api/v4/projects/21/repository/files/..", ""},
		{"POST", "/api/v4/projects/21/repository/files/a/b", ""},
		{"POST", "/api/v4/projects/21/repository/files/a%2Fb%2Fc%2Fd%2Fe", ""},
		{"POST", "/api/v4/projects/x/repository/files/a", ""},
		{"POST", "//api/v4/projects", ""},
		{"POST", "/api/v4/projects/", ""},
		{"POST", "/api/v4/projects?x=1", ""},
		{"GET", "/api/v3/groups", ""},
		{"GET", "/graphql", ""},
		{"get", "/api/v4/groups", ""},
		{"", "/api/v4/groups", ""},
	}
	for _, c := range cases {
		q, _ := url.ParseQuery(c.q)
		_, err := a.do(context.Background(), c.m, c.p, q, nil, 200)
		if ErrCode(err) != CodeNotAllowlisted {
			t.Errorf("%s %s?%s: err=%v, want refusal", c.m, c.p, c.q, err)
		}
	}
	if f.hits() != 0 {
		t.Fatalf("off-list requests reached the network: %v", f.recorded())
	}
	// the refusal happens before the token is read: a store without it still
	// answers not-allowlisted, never token_ref_unknown
	empty, _ := newTestTokenStore(t)
	a2 := apiFor(t, f, empty, nil)
	if _, err := a2.do(context.Background(), "DELETE", "/api/v4/projects/21", nil, nil, 200); ErrCode(err) != CodeNotAllowlisted {
		t.Fatalf("got %v", err)
	}
}

func TestPathEncodingNoInjection(t *testing.T) {
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	ctx := context.Background()
	bad := []string{"a/../b", "a/./b", "../x", "a%2fb", "a%2Fb", "a?x=1", "a#b", "a\nb", "a\r\nb", "ünï", "한글", "A", "a b", "a/b/c/d/e", "", "/a", "a/", "a//b", "a\x00b", "a%00", "-a", "a.b", "a\\b"}
	for _, s := range bad {
		if _, err := a.Group(ctx, s); ErrCode(err) != CodeBadInput {
			t.Errorf("Group(%q): %v", s, err)
		}
		if _, err := a.Project(ctx, "team/"+s); ErrCode(err) != CodeBadInput {
			t.Errorf("Project(team/%q): %v", s, err)
		}
		if _, err := a.CreateProject(ctx, "team", 7, s, ""); ErrCode(err) != CodeBadInput {
			t.Errorf("CreateProject(%q): %v", s, err)
		}
		if _, err := a.CreateGroup(ctx, s, "okname", ""); ErrCode(err) != CodeBadInput {
			t.Errorf("CreateGroup name %q: %v", s, err)
		}
		if _, err := a.CreateGroup(ctx, "okname", s, ""); ErrCode(err) != CodeBadInput {
			t.Errorf("CreateGroup path %q: %v", s, err)
		}
		if s != "" {
			if _, err := a.CreateGroup(ctx, "okname", "okname", s); ErrCode(err) != CodeBadInput {
				t.Errorf("CreateGroup parent %q: %v", s, err)
			}
		}
	}
	for _, p := range []string{"../x", "a/../b", "a%2Fb", "a b", "a?b", "a#b", "a\nb", "x/./y", "", "/abs", "a/b/c/d/e", "ü", "a//b", ".."} {
		if err := a.CommitFiles(ctx, 21, []CommitFile{{p, "x"}}); ErrCode(err) != CodeBadInput {
			t.Errorf("CommitFiles path %q: %v", p, err)
		}
	}
	for _, id := range []int64{0, -1} {
		if err := a.CommitFiles(ctx, id, []CommitFile{{"a", "x"}}); ErrCode(err) != CodeBadInput {
			t.Errorf("project id %d", id)
		}
	}
	if err := a.AddDeployKey(ctx, 21, "t", "ssh-ed25519 AAAA\nssh-rsa BBBB", true); ErrCode(err) != CodeBadInput {
		t.Errorf("multi-line key accepted")
	}
	if f.hits() != 0 {
		t.Fatalf("a refused value reached GitLab: %v", f.recorded())
	}
	// accepted values are encoded exactly once
	if _, err := a.Group(ctx, "team/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Project(ctx, "team/sub/demo"); err != nil {
		t.Fatal(err)
	}
	if err := a.CommitFiles(ctx, 21, []CommitFile{{"cmd/x/main.go", "x"}, {".aigate.yml", "y"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/v4/groups/team%2Fsub",
		"GET /api/v4/projects/team%2Fsub%2Fdemo",
		"POST /api/v4/projects/21/repository/files/cmd%2Fx%2Fmain.go",
		"POST /api/v4/projects/21/repository/files/.aigate.yml",
	}
	got := f.recorded()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestTokenOnlyInHeader(t *testing.T) {
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	runAll(t, f, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no requests")
	}
	for i, r := range f.reqs {
		if got := f.headers[i].Values("PRIVATE-TOKEN"); len(got) != 1 || got[0] != apiCanary {
			t.Fatalf("%s: PRIVATE-TOKEN header %v", r, got)
		}
		if strings.Contains(r, apiCanary) || strings.Contains(f.bodies[i], apiCanary) {
			t.Fatalf("token in URL or body: %s", r)
		}
		for k, vs := range f.headers[i] {
			if k == "Private-Token" {
				continue
			}
			for _, v := range vs {
				if strings.Contains(v, apiCanary) {
					t.Fatalf("token in header %s", k)
				}
			}
		}
		if f.headers[i].Get("Authorization") != "" {
			t.Fatal("Authorization header set")
		}
	}
}

func TestNoRedirectFollowedProviderAPI(t *testing.T) {
	target := newFakeGL(t)
	src := newFakeGL(t)
	src.override = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, target.URL+r.RequestURI, http.StatusFound)
		return true
	}
	a := apiFor(t, src, apiTokens(t), nil)
	_, err := a.Group(context.Background(), "team")
	if err == nil || ErrCode(err) != CodeProviderUnreachable {
		t.Fatalf("redirect answer: %v", err)
	}
	for _, st := range []int{301, 303, 307, 308} {
		src.override = func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, target.URL+r.RequestURI, st)
			return true
		}
		if _, err := a.CreateProject(context.Background(), "team", 7, "demo", ""); err == nil {
			t.Fatalf("status %d accepted", st)
		}
	}
	if target.hits() != 0 {
		t.Fatalf("redirect was followed (token sent on): %v", target.recorded())
	}
}

func TestProviderURLRules(t *testing.T) {
	ok := []string{
		"http://100.64.0.10:18082", "https://gitlab.example.com", "http://10.0.0.5", "http://172.16.0.1:8080", "http://192.168.1.10",
		"http://100.64.0.1", "http://[fd00::1]", "https://[fc00::1]:8443", "https://gitlab", "https://git.corp.example.org:8443",
		"https://[2001:db8::1]",
	}
	for _, s := range ok {
		if err := ValidateProviderURL(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
	bad := []string{
		"", "ftp://gitlab.example.com", "file:///etc/passwd", "gopher://x", "javascript:alert(1)", "//host", "gitlab.example.com",
		"http://user@gitlab.example.com", "http://user:pw@gitlab.example.com", "https://gitlab.example.com/", "https://gitlab.example.com/api/v4",
		"https://gitlab.example.com/x", "https://gitlab.example.com?x=1", "https://gitlab.example.com#f", "https://gitlab.example.com/?x",
		"HTTP://gitlab.example.com", "https://GitLab.Example.com", "https://gitlab.example.com.", "https://gitlab.example.com:0", "https://gitlab.example.com:65536",
		"https://gitlab.example.com:080", "https://gitlab.example.com:", "https://gitlab.example.com:80a", "https:// gitlab.example.com", "https://gitlab.example.com\n",
		"https://gitlab.example.com\\@evil.com", "https://exa mple.com",
		// loopback in all spellings
		"http://127.0.0.1", "http://127.0.0.1:8080", "http://127.1.2.3", "http://[::1]", "http://[::1]:80", "http://localhost", "http://localhost:8080",
		"http://foo.localhost", "http://[::ffff:127.0.0.1]", "http://[::ffff:7f00:1]", "http://[0:0:0:0:0:0:0:1]",
		"http://2130706433", "http://0x7f.1", "http://0x7f000001", "http://017700000001", "http://127.1", "http://127.0.1", "http://0177.0.0.1", "http://0.0.0.0", "http://0", "http://[::]",
		// unspecified, this-network, link-local, metadata, multicast, broadcast
		"http://0.0.0.0:80", "http://0.1.2.3", "http://169.254.169.254", "http://169.254.0.1:8080", "http://[fe80::1]", "http://[febf::1]", "http://[::ffff:169.254.169.254]",
		"http://[fd00:ec2::254]", "http://100.100.100.200", "http://224.0.0.1", "http://239.255.255.250", "http://[ff02::1]", "http://255.255.255.255", "http://240.0.0.1",
		"http://[64:ff9b::7f00:1]", "http://[64:ff9b::a9fe:a9fe]", "http://[2002:7f00:1::]", "http://[::127.0.0.1]", "http://[::7f00:1]",
		"http://[::ffff:0.0.0.0]", "http://[::1%25lo]", "http://[fe80::1%25eth0]",
	}
	for _, s := range bad {
		if err := ValidateProviderURL(s); ErrCode(err) != CodeBadURL {
			t.Errorf("%q accepted (err=%v)", s, err)
		}
	}
}

func TestIPRules(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "169.254.169.254", "fe80::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "224.0.0.1", "ff02::1", "100.100.100.200", "fd00:ec2::254", "64:ff9b::7f00:1"} {
		if !ipForbidden(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{"10.1.2.3", "172.16.5.5", "172.31.255.255", "192.168.0.1", "100.64.0.1", "100.64.0.10", "100.127.255.255", "fc00::1", "fd12:3456::1", "::ffff:10.1.2.3", "8.8.8.8", "2001:db8::1"} {
		if ipForbidden(netip.MustParseAddr(s)) {
			t.Errorf("%s refused", s)
		}
	}
}

func TestSSHURLRules(t *testing.T) {
	for _, s := range []string{"ssh://git@100.64.0.10", "ssh://git@100.64.0.10:22", "ssh://git@gitlab.example.com:2222", "ssh://git@10.0.0.5", "ssh://git@[fd00::1]"} {
		if err := ValidateSSHURL(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
	for _, s := range []string{
		"", "ssh://gitlab.example.com", "ssh://root@gitlab.example.com", "ssh://git:pw@gitlab.example.com", "ssh://git@gitlab.example.com/grp/proj.git",
		"ssh://git@gitlab.example.com/", "ssh://git@gitlab.example.com?x", "ssh://git@gitlab.example.com#x", "git@gitlab.example.com:grp/proj.git",
		"http://git@gitlab.example.com", "ssh://git@GitLab.example.com", "ssh://git@gitlab.example.com.", "ssh://git@gitlab.example.com:0", "ssh://git@host -oProxyCommand=x",
		"ssh://git@-oProxyCommand=x", "ssh://git@127.0.0.1", "ssh://git@localhost", "ssh://git@[::1]", "ssh://git@169.254.169.254", "ssh://git@0x7f.1", "ssh://git@0.0.0.0",
		"ssh://git@[fe80::1]", "ssh://git@[::ffff:127.0.0.1]", "ssh://git@224.0.0.1", "ssh://git@x\n", "ext::sh -c id", "ssh://git@host:22:22",
	} {
		if err := ValidateSSHURL(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

// DNS rebinding: the name was fine when stored, but resolves to a forbidden
// address when we connect. No connection may be made.
func TestDialTimeIPRecheck(t *testing.T) {
	f := newFakeGL(t)
	ts := apiTokens(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(f.URL, "http://"), ":")
	prov := Provider{ID: "gl", Kind: "gitlab", BaseURL: "http://gitlab.example.com:" + port, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}
	var answer atomic.Value
	answer.Store("127.0.0.1")
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "gitlab.example.com" {
			return nil, errors.New("nx")
		}
		return []netip.Addr{netip.MustParseAddr(answer.Load().(string))}, nil
	}
	// production policy: the resolved loopback address is refused at connect time
	a, err := newGitLabAPI(prov, ts, &apiSeams{lookup: lookup, plainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Group(context.Background(), "team"); err == nil {
		t.Fatal("connected to a name that resolves to loopback")
	}
	for _, ip := range []string{"169.254.169.254", "::1", "0.0.0.0", "::ffff:127.0.0.1"} {
		answer.Store(ip)
		if _, err := a.Group(context.Background(), "team"); err == nil {
			t.Fatalf("connected via %s", ip)
		}
	}
	if f.hits() != 0 {
		t.Fatalf("connection reached the server: %v", f.recorded())
	}
	// control: with the test policy the same name works (the check is the policy)
	answer.Store("127.0.0.1")
	a2, _ := newGitLabAPI(prov, ts, &apiSeams{lookup: lookup, policy: allowLoopback})
	if _, err := a2.Group(context.Background(), "team"); err != nil {
		t.Fatalf("control failed: %v", err)
	}
	// the policy is consulted at CONNECT time: flip it after construction
	var deny atomic.Bool
	a3, _ := newGitLabAPI(prov, ts, &apiSeams{lookup: lookup, policy: func(ad netip.Addr) bool { return !deny.Load() && allowLoopback(ad) }})
	before := f.hits()
	deny.Store(true)
	if _, err := a3.Group(context.Background(), "team"); err == nil || f.hits() != before {
		t.Fatalf("policy change after construction not honoured: %v", err)
	}
	// literal-IP base URL: refused already when the client is built
	if _, err := NewGitLabAPI(Provider{ID: "gl", Kind: "gitlab", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}, ts); ErrCode(err) != CodeBadURL {
		t.Fatalf("production constructor accepted the loopback fake: %v", err)
	}
}

func TestCheckProviderResolves(t *testing.T) {
	look := func(ips ...string) func(context.Context, string) ([]netip.Addr, error) {
		return func(context.Context, string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, s := range ips {
				out = append(out, netip.MustParseAddr(s))
			}
			return out, nil
		}
	}
	ctx := context.Background()
	if err := CheckProviderResolves(ctx, "https://gitlab.example.com", look("127.0.0.1", "169.254.169.254")); ErrCode(err) != CodeBadURL {
		t.Errorf("only-forbidden accepted: %v", err)
	}
	if err := CheckProviderResolves(ctx, "https://gitlab.example.com", look("10.1.1.1")); err != nil {
		t.Errorf("private refused: %v", err)
	}
	if err := CheckProviderResolves(ctx, "https://gitlab.example.com", look()); ErrCode(err) != CodeProviderUnreachable {
		t.Errorf("no answer: %v", err)
	}
	if err := CheckProviderResolves(ctx, "http://localhost", look("10.1.1.1")); ErrCode(err) != CodeBadURL {
		t.Errorf("localhost: %v", err)
	}
}

func TestGitLabTextNeverReflected(t *testing.T) {
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	reflect := apiCanary + "-REFLECTED-TEXT"
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/groups/team"): // 200 with hostile fields
			fmt.Fprintf(w, `{"id":7,"name":%q,"path":"team","full_path":"team"}`, "x\n"+reflect)
		case strings.HasSuffix(r.URL.Path, "/groups/other"): // 2xx garbage
			fmt.Fprint(w, reflect)
		default:
			w.WriteHeader(422)
			fmt.Fprint(w, `{"message":{"name":["`+reflect+`"]}}`)
		}
		return true
	}
	var errs []error
	ctx := context.Background()
	_, e1 := a.Group(ctx, "team")
	_, e2 := a.Group(ctx, "other")
	_, e3 := a.CreateProject(ctx, "team", 7, "demo", "")
	_, e4 := a.Project(ctx, "team/demo")
	e5 := a.AddDeployKey(ctx, 21, "t", goodKey, true)
	e6 := a.CommitFiles(ctx, 21, []CommitFile{{"README.md", "x"}})
	e7 := a.EnsureProtectedMain(ctx, 21)
	errs = append(errs, e1, e2, e3, e4, e5, e6, e7)
	for i, err := range errs {
		if err == nil {
			t.Errorf("call %d succeeded", i)
			continue
		}
		for _, s := range []string{err.Error(), fmt.Sprintf("%v|%+v|%#v|%q", err, err, err, err)} {
			if strings.Contains(s, "REFLECT") || strings.Contains(s, apiCanary) {
				t.Errorf("call %d reflects GitLab text or token: %s", i, s)
			}
		}
		if ErrCode(err) == "" {
			t.Errorf("call %d: not a fixed code: %T", i, err)
		}
	}
	if g, err := a.Group(ctx, "team"); err == nil || g.Name != "" {
		t.Errorf("hostile name accepted: %+v", g)
	}
}

func TestStatusMapping(t *testing.T) {
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	for st, want := range map[int]string{
		401: CodeGitLabRefused, 403: CodeGitLabRefused, 400: CodeGitLabRefused, 409: CodeGitLabRefused, 422: CodeGitLabRefused, 418: CodeGitLabRefused,
		404: CodeNotFound, 500: CodeProviderUnreachable, 502: CodeProviderUnreachable, 503: CodeProviderUnreachable, 504: CodeProviderUnreachable,
		429: CodeProviderUnreachable, 301: CodeProviderUnreachable, 302: CodeProviderUnreachable, 307: CodeProviderUnreachable,
		200: CodeGitLabRefused, // a 2xx other than the documented one
	} {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			if st >= 300 && st < 400 {
				w.Header().Set("Location", "http://127.0.0.1:1/x")
			}
			w.WriteHeader(st)
			return true
		}
		_, err := a.CreateProject(context.Background(), "team", 7, "demo", "") // wants 201
		if st == 201 {
			continue
		}
		if ErrCode(err) != want || ErrStatus(err) != st {
			t.Errorf("status %d -> %v (code %q status %d), want %s", st, err, ErrCode(err), ErrStatus(err), want)
		}
	}
	// network failure
	dead := newFakeGL(t)
	b := apiFor(t, dead, apiTokens(t), nil)
	dead.Close()
	if _, err := b.Group(context.Background(), "team"); ErrCode(err) != CodeProviderUnreachable {
		t.Errorf("closed server: %v", err)
	}
	// existing rule weaker than required: refused, never edited
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"name":"main","allow_force_push":false,"push_access_levels":[{"access_level":30}],"merge_access_levels":[{"access_level":40}]}`)
			return true
		}
		return false
	}
	before := f.hits()
	if err := a.EnsureProtectedMain(context.Background(), 21); !errors.Is(err, ErrProtectionWeaker) {
		t.Errorf("weak rule: %v", err)
	}
	if f.hits() != before+1 {
		t.Errorf("weak rule triggered a write: %v", f.recorded()[before:])
	}
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		fmt.Fprint(w, `{"name":"main","allow_force_push":true,"push_access_levels":[{"access_level":40}],"merge_access_levels":[{"access_level":40}]}`)
		return true
	}
	if err := a.EnsureProtectedMain(context.Background(), 21); !errors.Is(err, ErrProtectionWeaker) {
		t.Errorf("force push allowed: %v", err)
	}
}

func TestTimeoutsAndBodyCap(t *testing.T) {
	if apiConnTimeout != 10*time.Second || apiTotalTime != 15*time.Second || apiMaxBody != 1<<20 {
		t.Fatal("limits changed")
	}
	f := newFakeGL(t)
	ts := apiTokens(t)
	a := apiFor(t, f, ts, nil)
	if a.hc.Timeout != apiTotalTime {
		t.Fatalf("total timeout %v", a.hc.Timeout)
	}
	// 1 MiB + 1 bytes of body: refused with a fixed code
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.Write([]byte(`{"id":7,"name":"x","path":"team","full_path":"team","pad":"`))
		w.Write([]byte(strings.Repeat("a", apiMaxBody)))
		w.Write([]byte(`"}`))
		return true
	}
	if _, err := a.Group(context.Background(), "team"); ErrCode(err) != CodeProviderUnreachable {
		t.Fatalf("oversized body: %v", err)
	}
	// under the cap works
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.Write([]byte(`{"id":7,"name":"team","path":"team","full_path":"team","pad":"` + strings.Repeat("a", apiMaxBody-200) + `"}`))
		return true
	}
	if _, err := a.Group(context.Background(), "team"); err != nil {
		t.Fatalf("body under the cap: %v", err)
	}
	// slow answer: total timeout
	var release = make(chan struct{})
	f.override = func(w http.ResponseWriter, r *http.Request) bool { <-release; return true }
	defer close(release)
	b := apiFor(t, f, ts, &apiSeams{total: 300 * time.Millisecond})
	start := time.Now()
	if _, err := b.Group(context.Background(), "team"); ErrCode(err) != CodeProviderUnreachable || time.Since(start) > 3*time.Second {
		t.Fatalf("slow server: %v after %v", err, time.Since(start))
	}
	// caller's context
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.Group(ctx, "team"); ErrCode(err) != CodeProviderUnreachable {
		t.Fatalf("ctx deadline: %v", err)
	}
}

func TestTokenRefResolution(t *testing.T) {
	f := newFakeGL(t)
	ts, _ := newTestTokenStore(t)
	ctx := context.Background()
	prov := Provider{ID: "gl", Kind: "gitlab", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}
	a, err := newGitLabAPI(prov, ts, &apiSeams{policy: allowLoopback})
	if err != nil {
		t.Fatal(err)
	}
	// unknown token
	if _, err := a.Groups(ctx); ErrCode(err) != CodeTokenRefUnknown {
		t.Fatalf("missing token: %v", err)
	}
	// wrong kind with the same name
	other, _ := newTestTokenStore(t)
	other.Set("gl-admin", "github", "", apiCanary, tokNow)
	a2, _ := newGitLabAPI(prov, other, &apiSeams{policy: allowLoopback})
	if _, err := a2.Groups(ctx); ErrCode(err) != CodeTokenRefUnknown {
		t.Fatalf("wrong kind: %v", err)
	}
	// the openai key is never usable as a provider token
	oa, _ := newTestTokenStore(t)
	oa.Set("openai", "openai", "", apiCanary, tokNow)
	p3 := prov
	p3.TokenRef = "openai"
	a3, _ := newGitLabAPI(p3, oa, &apiSeams{policy: allowLoopback})
	if _, err := a3.Groups(ctx); ErrCode(err) != CodeTokenRefUnknown {
		t.Fatalf("openai token used: %v", err)
	}
	// nil store, broken store
	a4, _ := newGitLabAPI(prov, nil, &apiSeams{policy: allowLoopback})
	if _, err := a4.Groups(ctx); ErrCode(err) != CodeTokenRefUnknown {
		t.Fatalf("nil store: %v", err)
	}
	if f.hits() != 0 {
		t.Fatalf("a request went out without a usable token: %v", f.recorded())
	}
	// read per request, never cached: set, use, rotate, delete
	if err := ts.Set("gl-admin", "gitlab", "", apiCanary, tokNow); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Groups(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ts.Set("gl-admin", "gitlab", "", apiCanary+"-ROTATED", tokNow); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Groups(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	h := f.headers
	f.mu.Unlock()
	if h[0].Get("PRIVATE-TOKEN") != apiCanary || h[len(h)-1].Get("PRIVATE-TOKEN") != apiCanary+"-ROTATED" {
		t.Fatal("token not re-read per request")
	}
	n := f.hits()
	if err := ts.Delete("gl-admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Groups(ctx); ErrCode(err) != CodeTokenRefUnknown || f.hits() != n {
		t.Fatalf("deleted token still used: %v", err)
	}
	// the client struct holds no token value
	if strings.Contains(fmt.Sprintf("%+v %#v", a, a), apiCanary) {
		t.Fatal("token value reachable from the client")
	}
}

// Every outcome of every method, the panic path included, keeps the token out
// of errors, responses and logs.
func TestCanaryTokenNeverLogged(t *testing.T) {
	e := newTestEnv(t)
	f := newFakeGL(t)
	var srv *Server
	_, logs := e.newServer(t, func(s *Server) {
		srv = s
		s.apiSeams = &apiSeams{policy: allowLoopback}
		if err := s.Tokens.Set("gl-admin", "gitlab", "", apiCanary, tokNow); err != nil {
			t.Fatal(err)
		}
		if err := s.AddProviderChecked(Provider{ID: "gl", Kind: "gitlab", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}); err != nil {
			t.Fatal(err)
		}
		s.Handlers["gl_boom"] = func(ctx context.Context, _ json.RawMessage) (any, *OpError) {
			a, err := s.GitLabAPIFor("gl")
			if err != nil {
				return nil, &OpError{Code: "internal_error"}
			}
			a.Groups(ctx)
			panic("exploded with " + apiCanary + " and " + f.URL)
		}
		s.Handlers["gl_all"] = func(ctx context.Context, _ json.RawMessage) (any, *OpError) {
			a, _ := s.GitLabAPIFor("gl")
			runAll(t, f, a)
			return map[string]any{"ok": true}, nil
		}
	})
	sock := e.Raw["socket"].(string)
	for _, op := range []string{"gl_all", "gl_boom"} {
		r, out := call(t, sock, `{"v":1,"op":"`+op+`"}`)
		if strings.Contains(string(out), apiCanary) || strings.Contains(r.Message, "exploded") {
			t.Fatalf("%s: reply leaks: %s", op, out)
		}
	}
	if strings.Contains(logs.String(), apiCanary) || strings.Contains(logs.String(), "exploded") {
		t.Fatalf("log leaks: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "handler panic") {
		t.Fatalf("panic was not logged at all: %q", logs.String())
	}
	_ = srv
}

// TestProductionCannotEnableSeams: the seams (loopback-permitting policy,
// resolver, timeouts) exist only as the unexported type apiSeams. Outside the
// test files it may be named only where it is declared or read: never set from
// configuration or request data.
func TestProductionCannotEnableSeams(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	type use struct{ file, fn, what string }
	var uses []use
	skip := map[*ast.Ident]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fn := "(type/var)"
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					skip[x.Sel] = true // field reads such as s.apiSeams are not references to the type
				case *ast.Ident:
					if skip[x] {
						return true
					}
					if x.Name == "apiSeams" || x.Name == "newGitLabAPI" {
						uses = append(uses, use{f, fn, x.Name})
					}
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if se, ok := l.(*ast.SelectorExpr); ok && se.Sel.Name == "apiSeams" {
							t.Errorf("%s: production code assigns apiSeams (%s)", f, fn)
						}
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "apiSeams" {
						t.Errorf("%s: production code sets apiSeams in a literal (%s)", f, fn)
					}
				}
				return true
			})
		}
	}
	allowedUse := map[string]bool{
		"gitlabapi.go|(type/var)|apiSeams":   true, // type declaration
		"gitlabapi.go|newGitLabAPI|apiSeams": true, "gitlabapi.go|strictHTTP|apiSeams": true, "gitlabapi.go|newGitLabAPI|newGitLabAPI": true,
		"gitlabapi.go|NewGitLabAPI|newGitLabAPI": true, // production: passes nil
		"server.go|(type/var)|apiSeams":          true, // the Server field (never assigned)
		"provider.go|GitLabAPIFor|newGitLabAPI":  true,
	}
	for _, u := range uses {
		if !allowedUse[u.file+"|"+u.fn+"|"+u.what] {
			// field reads of s.apiSeams in provider.go are selectors, not idents of the type
			t.Errorf("unexpected reference to %s in %s (%s)", u.what, u.file, u.fn)
		}
	}
	// NewGitLabAPI passes nil seams
	src, _ := os.ReadFile("gitlabapi.go")
	if !strings.Contains(string(src), "return newGitLabAPI(p, tokens, nil)") {
		t.Fatal("NewGitLabAPI must call newGitLabAPI with nil seams")
	}
	// no environment or flag reading anywhere in the client or provider files
	for _, f := range []string{"gitlabapi.go", "provider.go"} {
		b, _ := os.ReadFile(f)
		for _, bad := range []string{"os.Getenv", "os.LookupEnv", "flag.", "ProxyFromEnvironment", "InsecureSkipVerify", "http.DefaultClient", "http.Get(", "http.Post("} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}

func TestClientHasOneSendPath(t *testing.T) {
	// Only `do` may call the HTTP client or build a request.
	b, _ := os.ReadFile("gitlabapi.go")
	s := string(b)
	for _, pat := range []string{"a.hc.Do(", "http.NewRequest"} {
		if strings.Count(s, pat) != 1 {
			t.Errorf("%q appears %d times, want exactly once (inside do)", pat, strings.Count(s, pat))
		}
	}
	for _, m := range []string{"http.MethodDelete", "http.MethodPut", "http.MethodPatch", `"DELETE"`, `"PUT"`, `"PATCH"`} {
		if strings.Contains(s, m) {
			t.Errorf("client mentions %s", m)
		}
	}
}
