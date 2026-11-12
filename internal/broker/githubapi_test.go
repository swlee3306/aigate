package broker

import (
	"context"
	"encoding/base64"
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
)

const ghCanary = "ghp_canary-NOT-A-REAL-TOKEN-0123456789abcdefXYZ"

// fakeGH records METHOD + RequestURI of every request and answers like a small
// GitHub. fail maps "METHOD path-prefix" to a status.
type fakeGH struct {
	*httptest.Server
	mu         sync.Mutex
	reqs       []string
	headers    []http.Header
	bodies     []string
	fail       map[string]int
	failHeader map[string]string
	override   func(w http.ResponseWriter, r *http.Request) bool
	repos      map[string]string // lower-case "owner/repo" -> real full_name
	protection string            // JSON of an existing protection ("" = none)
	defBranch  string            // default branch GitHub reports ("" = main)
	orgPages   int               // when >0: GET /user/orgs answers 100 entries for every page
	login      string            // the authenticated account's login ("" = alice)
}

func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	f := &fakeGH{fail: map[string]int{}, repos: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

const ghStrong = `{"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false},"enforce_admins":{"enabled":true}}`

func (f *fakeGH) serve(w http.ResponseWriter, r *http.Request) {
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
	f.mu.Lock()
	var failSt int
	for k, st := range f.fail {
		if strings.HasPrefix(key, k) {
			failSt = st
			for hk, hv := range f.failHeader {
				w.Header().Set(hk, hv)
			}
		}
	}
	f.mu.Unlock()
	if failSt != 0 {
		w.WriteHeader(failSt)
		fmt.Fprint(w, `{"message":"`+ghCanary+`-REFLECT"}`)
		return
	}
	p := r.URL.EscapedPath()
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	w.Header().Set("Content-Type", "application/json")
	var req map[string]any
	_ = json.Unmarshal(b, &req)
	switch {
	case r.Method == "GET" && p == "/user":
		fmt.Fprintf(w, `{"login":%q,"id":1,"type":"User","extra":1}`, f.userLogin())
	case r.Method == "GET" && p == "/user/orgs":
		if f.orgPages > 0 {
			var parts []string
			for i := 0; i < 100; i++ {
				parts = append(parts, fmt.Sprintf(`{"login":"org-%s-%d"}`, r.URL.Query().Get("page"), i))
			}
			w.Header().Set("Link", `<`+f.URL+`/user/orgs?page=99>; rel="next"`)
			fmt.Fprint(w, "["+strings.Join(parts, ",")+"]")
			return
		}
		fmt.Fprint(w, `[{"login":"Acme-Org","id":2},{"login":"bad.login"}]`)
	case r.Method == "GET" && len(segs) == 3 && segs[0] == "repos":
		f.mu.Lock()
		real, ok := f.repos[strings.ToLower(segs[1]+"/"+segs[2])]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		db := f.defBranch
		if db == "" {
			db = "main"
		}
		fmt.Fprintf(w, `{"id":31,"full_name":%q,"default_branch":%q,"private":true}`, real, db)
	case r.Method == "POST" && (p == "/user/repos" || (len(segs) == 3 && segs[0] == "orgs" && segs[2] == "repos")):
		owner := f.userLogin()
		if segs[0] == "orgs" {
			owner = segs[1]
		}
		name, _ := req["name"].(string)
		priv, _ := req["private"].(bool)
		f.mu.Lock()
		f.repos[strings.ToLower(owner+"/"+name)] = owner + "/" + name
		f.mu.Unlock()
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":31,"full_name":%q,"private":%v,"default_branch":"main"}`, owner+"/"+name, priv)
	case r.Method == "PUT" && len(segs) >= 5 && segs[3] == "contents":
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"content":{"path":%q},"commit":{"sha":"abc"}}`, strings.Join(segs[4:], "/"))
	case r.Method == "GET" && strings.HasSuffix(p, "/protection"):
		f.mu.Lock()
		pr := f.protection
		f.mu.Unlock()
		if pr == "" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, pr)
	case r.Method == "PUT" && strings.HasSuffix(p, "/protection"):
		f.mu.Lock()
		f.protection = ghStrong
		f.mu.Unlock()
		fmt.Fprint(w, ghStrong)
	case r.Method == "POST" && strings.HasSuffix(p, "/keys"):
		w.WriteHeader(201)
		ro, _ := req["read_only"].(bool)
		fmt.Fprintf(w, `{"id":5,"title":"x","read_only":%v}`, ro)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGH) userLogin() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.login == "" {
		return "alice"
	}
	return f.login
}

func (f *fakeGH) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}
func (f *fakeGH) hits() int { return len(f.recorded()) }

// ghMade creates a repository through the client (the only way to get a
// justCreated handle that the client honours).
func ghMade(t *testing.T, a *GitHubAPI, name string) RepoInfo {
	t.Helper()
	owners, _, err := a.ListOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.CreateRepo(context.Background(), CreateRepoReq{Owner: owners[0], Name: name, Private: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ghTokens(t *testing.T) *TokenStore {
	t.Helper()
	ts, _ := newTestTokenStore(t)
	if err := ts.Set("gh-admin", "github", "", ghCanary, tokNow); err != nil {
		t.Fatal(err)
	}
	return ts
}

func ghFor(t *testing.T, f *fakeGH, ts *TokenStore, sm *apiSeams) *GitHubAPI {
	t.Helper()
	if sm == nil {
		sm = &apiSeams{}
	}
	if sm.policy == nil {
		sm.policy = allowLoopback
	}
	a, err := newGitHubAPI(Provider{ID: "gh", Kind: "github", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gh-admin"}, ts, sm)
	if err != nil {
		t.Fatalf("newGitHubAPI: %v", err)
	}
	a.prefix = "" // the fakes serve the API root (the Enterprise prefix has its own test)
	return a
}

// ghRunAll calls every public method on success and failure paths.
func ghRunAll(t *testing.T, f *fakeGH, a *GitHubAPI) {
	t.Helper()
	ctx := context.Background()
	owners, _, _ := a.ListOwners(ctx)
	a.GetOwner(ctx, "alice")
	org, _ := a.GetOwner(ctx, "Acme-Org")
	a.GetRepo(ctx, "alice/demo")
	r, _ := a.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: "demo", Description: "d", Private: true})
	a.CreateRepo(ctx, CreateRepoReq{Owner: org, Name: "demo2", Private: false})
	a.CommitFiles(ctx, r, []CommitFile{{"README.md", "x"}, {".gitignore", "y"}, {"cmd/main.go", "z"}})
	a.DefaultBranch(ctx, r)
	a.EnsureProtectedMain(ctx, r) // 404 then PUT (just created)
	a.EnsureProtectedMain(ctx, r) // now strong
	a.AddDeployKey(ctx, r, "orchd demo", goodKey, false)
	a.AddDeployKey(ctx, r, "orchd demo", goodKey, true)
	for _, st := range []int{401, 403, 404, 409, 422, 429, 500, 302} {
		f.fail = map[string]int{"GET /": st, "POST /": st, "PUT /": st}
		a.ListOwners(ctx)
		a.GetRepo(ctx, "alice/demo")
		a.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: "demo", Private: true})
		a.CommitFiles(ctx, r, []CommitFile{{"README.md", "x"}})
		a.EnsureProtectedMain(ctx, r)
		a.AddDeployKey(ctx, r, "t", goodKey, false)
	}
	f.fail = map[string]int{}
}

func TestGitHubOnlyAllowlistedRequests(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	ghRunAll(t, f, a)
	seen := map[string]bool{}
	for _, r := range f.recorded() {
		method, uri, _ := strings.Cut(r, " ")
		pth, rawq, _ := strings.Cut(uri, "?")
		q, _ := url.ParseQuery(rawq)
		if !githubAllowed(method, pth, q) {
			t.Fatalf("request outside the allowlist reached GitHub: %s", r)
		}
		if method == "DELETE" || method == "PATCH" || method == "HEAD" || method == "POST" && strings.Contains(pth, "merge") {
			t.Fatalf("forbidden method/path: %s", r)
		}
		for _, bad := range []string{"merge", "token", "authorizations", "installations", "/transfer", "/forks", "/collaborators", "/hooks", "/rulesets", "/dispatches"} {
			if strings.Contains(strings.ToLower(pth), bad) {
				t.Fatalf("forbidden path piece %q: %s", bad, r)
			}
		}
		for i, e := range githubAllowlist {
			if e.method == method && e.path.MatchString(pth) {
				seen[fmt.Sprint(i)] = true
			}
		}
	}
	for i, e := range githubAllowlist {
		if !seen[fmt.Sprint(i)] {
			t.Errorf("allowlist entry %d (%s %s) was never exercised", i, e.method, e.path)
		}
	}
	if len(githubAllowlist) != 9 {
		t.Fatalf("allowlist has %d entries, want exactly 9", len(githubAllowlist))
	}
	for _, e := range githubAllowlist {
		if e.method != "GET" && e.method != "POST" && e.method != "PUT" {
			t.Fatalf("allowlist method %s", e.method)
		}
	}
}

func TestGitHubDoRefusesOffList(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	cases := []struct{ m, p, q string }{
		{"DELETE", "/repos/alice/demo", ""},
		{"DELETE", "/repos/alice/demo/branches/main/protection", ""},
		{"DELETE", "/repos/alice/demo/keys/5", ""},
		{"PATCH", "/repos/alice/demo", ""},
		{"PUT", "/repos/alice/demo", ""},
		{"PATCH", "/repos/alice/demo/branches/main/protection", ""},
		{"POST", "/repos/alice/demo/branches/main/protection", ""},
		{"PUT", "/repos/alice/demo/keys", ""},
		{"GET", "/repos/alice/demo/keys", ""},
		{"POST", "/repos/alice/demo/merges", ""},
		{"PUT", "/repos/alice/demo/pulls/1/merge", ""},
		{"POST", "/repos/alice/demo/pulls", ""},
		{"POST", "/repos/alice/demo/forks", ""},
		{"POST", "/repos/alice/demo/transfer", ""},
		{"POST", "/repos/alice/demo/hooks", ""},
		{"GET", "/repos/alice/demo/contents/README.md", ""},
		{"POST", "/repos/alice/demo/contents/README.md", ""},
		{"DELETE", "/repos/alice/demo/contents/README.md", ""},
		{"GET", "/user/repos", ""},
		{"GET", "/user/keys", ""},
		{"POST", "/user/keys", ""},
		{"POST", "/user/orgs", ""},
		{"POST", "/orgs/acme/repos/x", ""},
		{"POST", "/orgs", ""},
		{"GET", "/orgs/acme", ""},
		{"GET", "/user/orgs", "since=1"},
		{"GET", "/user/orgs", "access_token=x"},
		{"GET", "/user", "x=1"},
		{"POST", "/user/repos", "x=1"},
		{"GET", "/repos/alice/demo", "ref=x"},
		{"GET", "/applications/x/token", ""},
		{"POST", "/app/installations/1/access_tokens", ""},
		{"GET", "/authorizations", ""},
		{"GET", "/graphql", ""},
		{"POST", "/graphql", ""},
		{"GET", "/repos/alice/../bob/x", ""},
		{"GET", "/repos/alice/..", ""},
		{"GET", "/repos/alice/.", ""},
		{"GET", "/repos/../demo", ""},
		{"GET", "/repos/alice%2Fbob/demo", ""},
		{"GET", "/repos/alice/demo%2Fx", ""},
		{"GET", "/repos/alice/demo%2e%2e", ""},
		{"GET", "/repos/-alice/demo", ""},
		{"GET", "/repos/ali.ce/demo", ""},
		{"GET", "/repos/alice/demo/", ""},
		{"GET", "//repos/alice/demo", ""},
		{"GET", "/repos/alice/demo/branches/main", ""},
		{"GET", "/repos/alice/demo/branches/../protection", ""},
		{"GET", "/repos/alice/demo/branches/main%2Fx/protection", ""},
		{"GET", "/repos/alice/demo/branches/main/protection/enforce_admins", ""},
		{"PUT", "/repos/alice/demo/branches/main/protection/required_signatures", ""},
		{"PUT", "/repos/alice/demo/contents/..", ""},
		{"PUT", "/repos/alice/demo/contents/a/../b", ""},
		{"PUT", "/repos/alice/demo/contents/a%2F..%2Fb", ""},
		{"PUT", "/repos/alice/demo/contents/%2E%2E", ""},
		{"PUT", "/repos/alice/demo/contents/a/b/c/d/e", ""},
		{"PUT", "/repos/alice/demo/contents/a%2Fb", ""},
		{"PUT", "/repos/alice/demo/contents/", ""},
		{"PUT", "/repos/alice/demo/contents", ""},
		{"POST", "/user/repos?x=1", ""},
		{"get", "/user", ""},
		{"", "/user", ""},
		{"GET", "/api/v3/user", ""},
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
	empty, _ := newTestTokenStore(t)
	a2 := ghFor(t, f, empty, nil)
	if _, err := a2.do(context.Background(), "DELETE", "/repos/alice/demo", nil, nil, 200); ErrCode(err) != CodeNotAllowlisted {
		t.Fatalf("refusal must come before the token read: %v", err)
	}
}

func TestGitHubPathEncodingNoInjection(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	ctx := context.Background()
	badName := []string{"a/../b", "a/./b", "../x", "a%2fb", "a%2Fb", "a?x=1", "a#b", "a\nb", "a\r\nb", "ünï", "한글", "a b", "", "/a", "a/", "a//b", "a\x00b", "a%00", "a\\b", "..", ".", "x.git", "X.GIT", strings.Repeat("a", 101)}
	owners, _, _ := a.ListOwners(ctx)
	before := f.hits()
	for _, s := range badName {
		if _, err := a.GetRepo(ctx, "alice/"+s); ErrCode(err) != CodeBadInput {
			t.Errorf("GetRepo(alice/%q): %v", s, err)
		}
		if s != "" && !strings.Contains(s, "/") {
			if _, err := a.GetRepo(ctx, s+"/demo"); ErrCode(err) != CodeBadInput {
				t.Errorf("GetRepo(%q/demo): %v", s, err)
			}
			if _, err := a.GetOwner(ctx, s); ErrCode(err) != CodeBadInput {
				t.Errorf("GetOwner(%q): %v", s, err)
			}
		}
		if _, err := a.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: s, Private: true}); ErrCode(err) != CodeBadInput {
			t.Errorf("CreateRepo(%q): %v", s, err)
		}
	}
	for _, s := range []string{"-a", "a.b", "a--", strings.Repeat("a", 40)} {
		if s == "a--" {
			continue // valid shape: GitHub is the authority on consecutive hyphens
		}
		if _, err := a.GetOwner(ctx, s); ErrCode(err) != CodeBadInput {
			t.Errorf("GetOwner(%q): %v", s, err)
		}
	}
	r := ghMade(t, a, "demo")
	before = f.hits()
	for _, p := range []string{"../x", "a/../b", "a%2Fb", "a b", "a?b", "a#b", "a\nb", "x/./y", "", "/abs", "a/b/c/d/e", "ü", "a//b", ".."} {
		if err := a.CommitFiles(ctx, r, []CommitFile{{p, "x"}}); ErrCode(err) != CodeBadInput {
			t.Errorf("CommitFiles path %q: %v", p, err)
		}
	}
	for _, bad := range []RepoInfo{{ID: 0, Path: "alice/demo", justCreated: true}, {ID: 1, Path: "alice/../x", justCreated: true}, {ID: 1, Path: "a%2Fb/c", justCreated: true}, {ID: 1, Path: "alice", justCreated: true}} {
		if err := a.CommitFiles(ctx, bad, []CommitFile{{"a", "x"}}); ErrCode(err) != CodeBadInput {
			t.Errorf("repo %+v accepted", bad)
		}
	}
	if err := a.AddDeployKey(ctx, r, "t", "ssh-ed25519 AAAA\nssh-rsa BBBB", true); ErrCode(err) != CodeBadInput {
		t.Errorf("multi-line key accepted")
	}
	if f.hits() != before {
		t.Fatalf("a refused value reached GitHub: %v", f.recorded()[before:])
	}
	// accepted values are used as written (no encoding needed: the charsets
	// exclude every character that would need it), exactly once, never doubled
	if _, err := a.GetRepo(ctx, "Alice/De.mo_x-1"); err != nil && ErrCode(err) != CodeNotFound {
		t.Fatal(err)
	}
	if err := a.CommitFiles(ctx, r, []CommitFile{{"cmd/x/main.go", "x"}, {".aigate.yml", "y"}}); err != nil {
		t.Fatal(err)
	}
	got := f.recorded()[before:]
	want := []string{
		"GET /repos/Alice/De.mo_x-1",
		"PUT /repos/alice/demo/contents/cmd/x/main.go",
		"GET /repos/alice/demo", // default branch check after the first file

		"PUT /repos/alice/demo/contents/.aigate.yml",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, r := range got {
		if strings.Contains(r, "%") {
			t.Fatalf("percent sign on the wire: %s", r)
		}
	}
}

func TestGitHubTokenOnlyInAuthHeader(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	ghRunAll(t, f, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no requests")
	}
	for i, r := range f.reqs {
		h := f.headers[i]
		if got := h.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+ghCanary {
			t.Fatalf("%s: Authorization header %v", r, got)
		}
		if got := h.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("%s: Accept %q", r, got)
		}
		if got := h.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Fatalf("%s: api version %q", r, got)
		}
		if h.Get("User-Agent") == "" {
			t.Fatalf("%s: GitHub requires a User-Agent", r)
		}
		if strings.Contains(r, ghCanary) || strings.Contains(f.bodies[i], ghCanary) {
			t.Fatalf("token in URL or body: %s", r)
		}
		for k, vs := range h {
			if k == "Authorization" {
				continue
			}
			for _, v := range vs {
				if strings.Contains(v, ghCanary) {
					t.Fatalf("token in header %s", k)
				}
			}
		}
		if h.Get("Private-Token") != "" {
			t.Fatal("PRIVATE-TOKEN set on a GitHub request")
		}
	}
}

func TestGitHubTokenReadPerRequestAndKindChecked(t *testing.T) {
	f := newFakeGH(t)
	ts := ghTokens(t)
	a := ghFor(t, f, ts, nil)
	if _, _, err := a.ListOwners(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ts.Set("gh-admin", "github", "", "rotated-"+ghCanary, tokNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.ListOwners(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.headers[len(f.headers)-1].Get("Authorization")
	f.mu.Unlock()
	if last != "Bearer rotated-"+ghCanary {
		t.Fatalf("token was cached: %q", last)
	}
	// a gitlab-kind token under the ref is not usable; no request is sent
	ts2, _ := newTestTokenStore(t)
	ts2.Set("gh-admin", "gitlab", "", ghCanary, tokNow)
	a2 := ghFor(t, f, ts2, nil)
	n := f.hits()
	if _, _, err := a2.ListOwners(context.Background()); ErrCode(err) != CodeTokenRefUnknown || f.hits() != n {
		t.Fatalf("wrong-kind token: %v", err)
	}
}

func TestGitHubNoRedirect(t *testing.T) {
	target := newFakeGH(t)
	src := newFakeGH(t)
	a := ghFor(t, src, ghTokens(t), nil)
	for _, st := range []int{301, 302, 303, 307, 308} {
		src.override = func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, target.URL+r.RequestURI, st)
			return true
		}
		if _, _, err := a.ListOwners(context.Background()); ErrCode(err) != CodeProviderUnreachable {
			t.Fatalf("status %d: %v", st, err)
		}
		if _, err := a.GetRepo(context.Background(), "alice/demo"); ErrCode(err) != CodeProviderUnreachable {
			t.Fatalf("GetRepo status %d: %v", st, err)
		}
		if _, err := a.CreateRepo(context.Background(), CreateRepoReq{Owner: Owner{Path: "alice", Kind: "user", fromProvider: true}, Name: "demo", Private: true}); err == nil {
			t.Fatalf("status %d accepted", st)
		}
	}
	if target.hits() != 0 {
		t.Fatalf("redirect was followed (token sent on): %v", target.recorded())
	}
}

func TestGitHubTextNeverReflected(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	r := ghMade(t, a, "demo")
	for _, st := range []int{400, 401, 403, 404, 409, 422, 429, 500, 502, 302} {
		f.fail = map[string]int{"GET /": st, "POST /": st, "PUT /": st}
		var errs []error
		_, _, e1 := a.ListOwners(context.Background())
		_, e2 := a.GetRepo(context.Background(), "alice/demo")
		_, e3 := a.CreateRepo(context.Background(), CreateRepoReq{Owner: Owner{Path: "alice", Kind: "user", fromProvider: true}, Name: "demo", Private: true})
		e4 := a.CommitFiles(context.Background(), r, []CommitFile{{"README.md", "x"}})
		e5 := a.EnsureProtectedMain(context.Background(), r)
		e6 := a.AddDeployKey(context.Background(), r, "t", goodKey, false)
		errs = append(errs, e1, e2, e3, e4, e5, e6)
		for _, e := range errs {
			if e == nil {
				t.Fatalf("status %d: nil error", st)
			}
			if strings.Contains(e.Error(), ghCanary) || strings.Contains(e.Error(), "REFLECT") || strings.Contains(e.Error(), "127.0.0.1") {
				t.Fatalf("error reflects text: %v", e)
			}
			if ErrCode(e) == "" {
				t.Fatalf("status %d: not a fixed code: %v", st, e)
			}
		}
	}
	// a hostile 2xx body is decoded into fixed fields only and never echoed
	f.fail = map[string]int{}
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		fmt.Fprint(w, `{"login":"`+ghCanary+`-REFLECT/x"}`)
		return true
	}
	_, _, err := a.ListOwners(context.Background())
	if err == nil || strings.Contains(err.Error(), "REFLECT") {
		t.Fatalf("hostile 2xx: %v", err)
	}
}

func TestGitHubStatusMapping(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	cases := []struct {
		st     int
		header map[string]string
		want   string
	}{
		{401, nil, CodeGitHubRefused},
		{403, nil, CodeGitHubRefused},
		{403, map[string]string{"X-RateLimit-Remaining": "0"}, CodeProviderRateLimited},
		{403, map[string]string{"Retry-After": "60"}, CodeProviderRateLimited},
		{403, map[string]string{"X-RateLimit-Remaining": "17"}, CodeGitHubRefused},
		{404, nil, CodeGitHubRefused}, // a 404 on a write is a refusal, never "not found"
		{409, nil, CodeGitHubRefused},
		{422, nil, CodeGitHubRefused},
		{400, nil, CodeGitHubRefused},
		{429, nil, CodeProviderRateLimited},
		{500, nil, CodeProviderUnreachable},
		{502, nil, CodeProviderUnreachable},
		{503, nil, CodeProviderUnreachable},
		{302, nil, CodeProviderUnreachable},
		{200, nil, CodeGitHubRefused}, // not the wanted 201 on create
	}
	for _, c := range cases {
		f.fail = map[string]int{"POST /user/repos": c.st}
		f.failHeader = c.header
		_, err := a.CreateRepo(context.Background(), CreateRepoReq{Owner: Owner{Path: "alice", Kind: "user", fromProvider: true}, Name: "demo", Private: true})
		if c.st == 200 {
			f.fail = map[string]int{}
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				w.WriteHeader(200)
				fmt.Fprint(w, `{}`)
				return true
			}
			_, err = a.CreateRepo(context.Background(), CreateRepoReq{Owner: Owner{Path: "alice", Kind: "user", fromProvider: true}, Name: "demo", Private: true})
			f.override = nil
		}
		if ErrCode(err) != c.want || WireCode(err) != c.want {
			t.Errorf("status %d %v: code %q want %q", c.st, c.header, ErrCode(err), c.want)
		}
		if c.st != 200 && ErrStatus(err) != c.st {
			t.Errorf("status %d: ErrStatus %d", c.st, ErrStatus(err))
		}
	}
	f.failHeader = nil
	// both codes have registered fixed sentences
	for _, c := range []string{CodeGitHubRefused, CodeProviderRateLimited, CodeProviderUnreachable} {
		if _, ok := fixedMessages[c]; !ok {
			t.Errorf("%s has no fixed message", c)
		}
	}
	// 3xx on GET repo: never followed
	f.fail = map[string]int{"GET /repos/alice/demo": 301}
	if _, err := a.GetRepo(context.Background(), "alice/demo"); ErrCode(err) != CodeProviderUnreachable {
		t.Fatalf("301: %v", err)
	}
	f.fail = map[string]int{}
	// 404 means absent for GetRepo
	if _, err := a.GetRepo(context.Background(), "alice/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	// oversize answer
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.Write([]byte(strings.Repeat("x", apiMaxBody+10)))
		return true
	}
	if _, _, err := a.ListOwners(context.Background()); ErrCode(err) != CodeProviderUnreachable {
		t.Fatalf("oversize: %v", err)
	}
}

func TestGitHubCaseInsensitiveNameCollision(t *testing.T) {
	f := newFakeGH(t)
	f.repos["alice/demo"] = "Alice/Demo"
	a := ghFor(t, f, ghTokens(t), nil)
	// a case variant of an existing repo IS the existing repo: it counts as taken
	for _, p := range []string{"alice/demo", "ALICE/DEMO", "Alice/Demo", "aLiCe/dEmO"} {
		info, err := a.GetRepo(context.Background(), p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if info.Path != "Alice/Demo" || info.justCreated {
			t.Fatalf("%s: %+v", p, info)
		}
		if !SameRepoPath("github", p, info.Path) {
			t.Fatalf("SameRepoPath(%s,%s)", p, info.Path)
		}
	}
	if SameRepoPath("github", "alice/demo", "alice/demo2") || SameRepoPath("github", "alice/demo", "bob/demo") {
		t.Fatal("different repos treated as the same")
	}
	if SameRepoPath("gitlab", "grp/demo", "grp/DEMO") {
		t.Fatal("gitlab paths are case-sensitive here")
	}
	// the answer must still be the asked repo: a different full_name is refused
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		fmt.Fprint(w, `{"id":3,"full_name":"mallory/other","default_branch":"main"}`)
		return true
	}
	if _, err := a.GetRepo(context.Background(), "alice/demo"); ErrCode(err) != CodeGitHubRefused {
		t.Fatalf("foreign full_name accepted: %v", err)
	}
	// owner lookup is case-insensitive and returns the canonical login
	f.override = nil
	o, err := a.GetOwner(context.Background(), "acme-org")
	if err != nil || o.Path != "Acme-Org" || o.Kind != "org" {
		t.Fatalf("%+v %v", o, err)
	}
	// the same owner in two spellings is one owner
	if !SameRepoPath("github", "ACME-ORG/x", "Acme-Org/X") {
		t.Fatal("owner case")
	}
}

func TestProtectionPutOnlyForJustCreatedRepo(t *testing.T) {
	ctx := context.Background()
	puts := func(f *fakeGH) int {
		n := 0
		for _, r := range f.recorded() {
			if strings.HasPrefix(r, "PUT ") && strings.HasSuffix(r, "/protection") {
				n++
			}
		}
		return n
	}
	// existing repo (from GetRepo), no rule: no PUT, refused
	f := newFakeGH(t)
	f.repos["alice/old"] = "alice/old"
	a := ghFor(t, f, ghTokens(t), nil)
	old, err := a.GetRepo(ctx, "alice/old")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureProtectedMain(ctx, old); !errors.Is(err, ErrProtectionWeaker) {
		t.Fatalf("existing unprotected repo: %v", err)
	}
	if puts(f) != 0 {
		t.Fatal("PUT protection sent for an existing repo")
	}
	// a hand-built handle cannot claim "created here" through the interface type
	if err := a.EnsureProtectedMain(ctx, RepoInfo{ID: 1, Path: "alice/old"}); !errors.Is(err, ErrProtectionWeaker) || puts(f) != 0 {
		t.Fatalf("zero handle: %v", err)
	}
	// existing rule, strong: success without a write; weaker or scoped shape: refused, never edited
	f.protection = ghStrong
	if err := a.EnsureProtectedMain(ctx, old); err != nil || puts(f) != 0 {
		t.Fatalf("strong existing rule: %v", err)
	}
	fresh := ghMade(t, a, "fresh0")
	for name, body := range map[string]string{
		"force":    `{"allow_force_pushes":{"enabled":true},"allow_deletions":{"enabled":false},"enforce_admins":{"enabled":true}}`,
		"delete":   `{"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":true},"enforce_admins":{"enabled":true}}`,
		"noadmins": `{"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false},"enforce_admins":{"enabled":false}}`,
		"missing":  `{"enforce_admins":{"enabled":true}}`,
	} {
		f.protection = body
		if err := a.EnsureProtectedMain(ctx, fresh); !errors.Is(err, ErrProtectionWeaker) && !(name == "missing" && ErrCode(err) == CodeGitHubRefused) {
			t.Errorf("%s: %v", name, err)
		}
		if err := a.EnsureProtectedMain(ctx, old); err == nil {
			t.Errorf("%s: weaker rule accepted on existing repo", name)
		}
	}
	if puts(f) != 0 {
		t.Fatal("an existing weaker rule was edited (PUT sent)")
	}
	// repo created by this client: 404 -> exactly one PUT of the fixed body to main
	f2 := newFakeGH(t)
	a2 := ghFor(t, f2, ghTokens(t), nil)
	owners, _, _ := a2.ListOwners(ctx)
	r, err := a2.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: "fresh", Private: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := a2.EnsureProtectedMain(ctx, r); err != nil {
		t.Fatal(err)
	}
	if puts(f2) != 1 {
		t.Fatalf("PUT count %d: %v", puts(f2), f2.recorded())
	}
	f2.mu.Lock()
	var body map[string]any
	for i, rq := range f2.reqs {
		if strings.HasPrefix(rq, "PUT ") {
			if rq != "PUT /repos/alice/fresh/branches/main/protection" {
				t.Fatalf("PUT target %s", rq)
			}
			json.Unmarshal([]byte(f2.bodies[i]), &body)
		}
	}
	f2.mu.Unlock()
	for _, k := range []string{"required_status_checks", "required_pull_request_reviews", "restrictions"} {
		if v, ok := body[k]; !ok || v != nil {
			t.Errorf("%s must be present and null: %v", k, body)
		}
	}
	if body["enforce_admins"] != true || body["allow_force_pushes"] != false || body["allow_deletions"] != false {
		t.Fatalf("protection body %v", body)
	}
	// the proof is the handle of THIS flow: it is set only by CreateRepo
	files, _ := filepath.Glob("*.go")
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(fn)
		n := strings.Count(string(b), "justCreated: true") + strings.Count(string(b), "justCreated = true")
		if (fn == "githubapi.go" && n != 1) || (fn != "githubapi.go" && n != 0) {
			t.Errorf("%s sets justCreated %d times", fn, n)
		}
	}
}

func TestGitHubProviderURLRules(t *testing.T) {
	mk := func(base, ssh string) Provider {
		return Provider{ID: "gh", Kind: "github", BaseURL: base, SSHURL: ssh, TokenRef: "gh-admin"}
	}
	good := [][2]string{
		{"https://api.github.com", "ssh://git@github.com"},
		{"https://ghe.example.com", "ssh://git@ghe.example.com"},
		{"https://ghe.example.com:8443", "ssh://git@ghe.example.com:2222"},
		{"http://10.0.0.5", "ssh://git@10.0.0.5"},
		{"http://100.64.0.10:8080", "ssh://git@100.64.0.10"},
		{"http://[fd00::1]", "ssh://git@[fd00::1]"},
	}
	for _, g := range good {
		if err := providerShape(mk(g[0], g[1]), defaultIPPolicy, true); err != nil {
			t.Errorf("%v refused: %v", g, err)
		}
		if _, err := newGitHubAPI(mk(g[0], g[1]), ghTokens(t), nil); err != nil {
			t.Errorf("client for %v refused: %v", g, err)
		}
	}
	bad := [][2]string{
		{"http://api.github.com", "ssh://git@github.com"},   // plain http to a public name
		{"http://140.82.112.5", "ssh://git@github.com"},     // plain http to a public address
		{"https://api.github.com/", "ssh://git@github.com"}, // path
		{"https://api.github.com/api/v3", "ssh://git@github.com"},
		{"https://user@api.github.com", "ssh://git@github.com"},
		{"https://API.github.com", "ssh://git@github.com"},
		{"https://127.0.0.1", "ssh://git@github.com"},
		{"https://localhost", "ssh://git@github.com"},
		{"https://169.254.169.254", "ssh://git@github.com"},
		{"https://[::1]", "ssh://git@github.com"},
		{"https://0x7f000001", "ssh://git@github.com"},
		{"https://api.github.com", "github.com:org/repo.git"},
		{"https://api.github.com", "ssh://root@github.com"},
		{"https://api.github.com", "ssh://git@github.com/org/repo.git"},
		{"https://api.github.com", "ssh://git@127.0.0.1"},
		{"https://api.github.com", "ssh://git@169.254.169.254"},
		{"https://api.github.com", "ssh://git@GitHub.com"},
		{"ftp://api.github.com", "ssh://git@github.com"},
		{"https://github.com", "ssh://git@github.com"},     // the web host is not the API host
		{"https://www.github.com", "ssh://git@github.com"}, // neither is www
	}
	for _, g := range bad {
		if err := providerShape(mk(g[0], g[1]), defaultIPPolicy, true); err == nil {
			t.Errorf("%v accepted", g)
		}
	}
	// a client is only built for a github provider, a GitLab client only for gitlab
	ts := ghTokens(t)
	if _, err := newGitHubAPI(Provider{ID: "g", Kind: "gitlab", BaseURL: "https://api.github.com", SSHURL: "ssh://git@github.com", TokenRef: "gh-admin"}, ts, nil); ErrCode(err) != CodeBadInput {
		t.Errorf("gitlab kind accepted by the github client: %v", err)
	}
	if _, err := newGitLabAPI(Provider{ID: "g", Kind: "github", BaseURL: "https://api.github.com", SSHURL: "ssh://git@github.com", TokenRef: "gh-admin"}, ts, nil); ErrCode(err) != CodeBadInput {
		t.Errorf("github kind accepted by the gitlab client: %v", err)
	}
	if _, err := newGitHubAPI(Provider{ID: "g", Kind: "github", BaseURL: "https://api.github.com", SSHURL: "ssh://git@github.com", TokenRef: "Bad Ref"}, ts, nil); ErrCode(err) != CodeBadInput {
		t.Errorf("bad token ref: %v", err)
	}
	// the API prefix is kind-aware: github.com has none, an Enterprise host uses /api/v3
	if p := githubAPIPrefix("https://api.github.com"); p != "" {
		t.Errorf("prefix for api.github.com %q", p)
	}
	if p := githubAPIPrefix("https://ghe.example.com:8443"); p != "/api/v3" {
		t.Errorf("prefix for GHES %q", p)
	}
}

// RepoProviderFor is the kind-aware apiFor.
func TestRepoProviderForIsKindAware(t *testing.T) {
	e := newTestEnv(t)
	srv, _ := e.newServer(t, func(s *Server) {
		for id, kind := range map[string]string{"gl1": "gitlab", "gh1": "github"} {
			base, ssh := "https://gitlab.example.com", "ssh://git@gitlab.example.com"
			if kind == "github" {
				base, ssh = "https://api.github.com", "ssh://git@github.com"
			}
			if err := s.AddProviderChecked(Provider{ID: id, Kind: kind, BaseURL: base, SSHURL: ssh, TokenRef: id + "-tok"}); err != nil {
				t.Fatal(err)
			}
		}
	})
	for id, kind := range map[string]string{"gl1": "gitlab", "gh1": "github"} {
		p, err := srv.RepoProviderFor(id)
		if err != nil || p.Kind() != kind {
			t.Fatalf("%s: %v %v", id, p, err)
		}
	}
	if _, err := srv.RepoProviderFor("nope"); !errors.Is(err, ErrProviderUnknown) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestGitHubDialTimeIPRecheck(t *testing.T) {
	f := newFakeGH(t)
	ts := ghTokens(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(f.URL, "http://"), ":")
	prov := Provider{ID: "gh", Kind: "github", BaseURL: "http://ghe.example.com:" + port, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gh-admin"}
	var answer atomic.Value
	answer.Store("127.0.0.1")
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "ghe.example.com" {
			return nil, errors.New("nx")
		}
		return []netip.Addr{netip.MustParseAddr(answer.Load().(string))}, nil
	}
	a, err := newGitHubAPI(prov, ts, &apiSeams{lookup: lookup, plainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "::1", "0.0.0.0", "::ffff:127.0.0.1"} {
		answer.Store(ip)
		if _, _, err := a.ListOwners(context.Background()); ErrCode(err) != CodeBadURL {
			t.Fatalf("connected via %s: %v", ip, err)
		}
	}
	if f.hits() != 0 {
		t.Fatalf("connection reached the server: %v", f.recorded())
	}
	// control: a public address passes the production policy; with the test policy the fake works
	if !defaultIPPolicy(netip.MustParseAddr("140.82.112.5")) {
		t.Fatal("public GitHub address refused by the production policy")
	}
	answer.Store("127.0.0.1")
	a2, _ := newGitHubAPI(prov, ts, &apiSeams{lookup: lookup, policy: allowLoopback})
	a2.prefix = ""
	if _, _, err := a2.ListOwners(context.Background()); err != nil {
		t.Fatalf("control failed: %v", err)
	}
	// the policy is consulted at connect time
	var deny atomic.Bool
	a3, _ := newGitHubAPI(prov, ts, &apiSeams{lookup: lookup, policy: func(ad netip.Addr) bool { return !deny.Load() && allowLoopback(ad) }})
	a3.prefix = ""
	before := f.hits()
	deny.Store(true)
	if _, _, err := a3.ListOwners(context.Background()); err == nil || f.hits() != before {
		t.Fatalf("policy change after construction not honoured: %v", err)
	}
	if _, err := NewGitHubAPI(Provider{ID: "gh", Kind: "github", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gh-admin"}, ts); ErrCode(err) != CodeBadURL {
		t.Fatalf("production constructor accepted the loopback fake: %v", err)
	}
}

func TestGitHubOwnersPaginationNeverFollowsLink(t *testing.T) {
	f := newFakeGH(t)
	f.orgPages = 1
	target := newFakeGH(t)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/user/orgs" {
			w.Header().Set("Link", `<`+target.URL+`/user/orgs?page=2>; rel="next"`)
		}
		return false
	}
	a := ghFor(t, f, ghTokens(t), nil)
	owners, truncated, err := a.ListOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(owners) != 1+apiMaxPages*apiPerPage {
		t.Fatalf("owners %d truncated %v", len(owners), truncated)
	}
	if owners[0].Path != "alice" || owners[0].Kind != "user" || owners[1].Kind != "org" {
		t.Fatalf("%+v %+v", owners[0], owners[1])
	}
	if target.hits() != 0 {
		t.Fatalf("Link header was followed: %v", target.recorded())
	}
	var pages []string
	for _, r := range f.recorded() {
		if strings.HasPrefix(r, "GET /user/orgs") {
			pages = append(pages, r)
		}
	}
	if len(pages) != apiMaxPages {
		t.Fatalf("pages %v", pages)
	}
	for i, p := range pages {
		if p != fmt.Sprintf("GET /user/orgs?page=%d&per_page=100", i+1) {
			t.Fatalf("page request %q", p)
		}
	}
	// a short list is complete; an entry that does not fit the login rule is skipped and flagged
	f2 := newFakeGH(t)
	a2 := ghFor(t, f2, ghTokens(t), nil)
	owners, truncated, err = a2.ListOwners(context.Background())
	if err != nil || !truncated || len(owners) != 2 || owners[1].Path != "Acme-Org" {
		t.Fatalf("%+v %v %v", owners, truncated, err)
	}
	for _, o := range owners {
		if o.fromProvider != true {
			t.Fatal("owner lacks the provider proof")
		}
	}
}

func TestGitHubCreateRepoBodyAndGuards(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	ctx := context.Background()
	owners, _, _ := a.ListOwners(ctx)
	// a hand-built owner (not from the provider) is refused before any request
	n := f.hits()
	if _, err := a.CreateRepo(ctx, CreateRepoReq{Owner: Owner{Path: "mallory", Kind: "org"}, Name: "x", Private: true}); ErrCode(err) != CodeBadInput || f.hits() != n {
		t.Fatalf("forged owner: %v", err)
	}
	for _, priv := range []bool{true, false} {
		f.mu.Lock()
		f.reqs, f.bodies = nil, nil
		f.mu.Unlock()
		r, err := a.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: "demo" + fmt.Sprint(priv), Description: "hello", Private: priv})
		if err != nil || !r.justCreated || r.ID != 31 || r.Path != "alice/demo"+fmt.Sprint(priv) {
			t.Fatalf("%+v %v", r, err)
		}
		var body map[string]any
		json.Unmarshal([]byte(f.bodies[0]), &body)
		if body["private"] != priv || body["auto_init"] != false || body["name"] != "demo"+fmt.Sprint(priv) {
			t.Fatalf("body %v", body)
		}
		for _, bad := range []string{"owner", "organization", "visibility", "template_repository", "homepage", "gitignore_template"} {
			if _, ok := body[bad]; ok {
				t.Fatalf("unexpected field %s", bad)
			}
		}
		if f.recorded()[0] != "POST /user/repos" {
			t.Fatalf("%v", f.recorded())
		}
	}
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
	org := Owner{Path: "Acme-Org", Kind: "org", fromProvider: true}
	if _, err := a.CreateRepo(ctx, CreateRepoReq{Owner: org, Name: "orgrepo", Private: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded(); len(got) != 1 || got[0] != "POST /orgs/Acme-Org/repos" {
		t.Fatalf("%v", got)
	}
	// the answer must match what was asked: visibility flipped by GitHub, or another owner
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":31,"full_name":"alice/demo","private":false,"default_branch":"main"}`)
		return true
	}
	if _, err := a.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: "demo", Private: true}); ErrCode(err) != CodeGitHubRefused {
		t.Fatalf("public answer to a private request accepted: %v", err)
	}
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":31,"full_name":"mallory/demo","private":true,"default_branch":"main"}`)
		return true
	}
	if _, err := a.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: "demo", Private: true}); ErrCode(err) != CodeGitHubRefused {
		t.Fatalf("foreign full_name accepted: %v", err)
	}
	// commits need the handle of the create flow; contents are base64 on branch main
	f.override = nil
	r := ghMade(t, a, "demo")
	f.mu.Lock()
	f.reqs, f.bodies = nil, nil
	f.mu.Unlock()
	if err := a.CommitFiles(ctx, RepoInfo{ID: 1, Path: "alice/old"}, []CommitFile{{"README.md", "x"}}); ErrCode(err) != CodeBadInput || f.hits() != 0 {
		t.Fatalf("CommitFiles on a repo not created here: %v", err)
	}
	if err := a.CommitFiles(ctx, r, []CommitFile{{"README.md", "héllo"}, {"a.txt", "b"}}); err != nil {
		t.Fatal(err)
	}
	var cb, cb2 map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &cb)
	json.Unmarshal([]byte(f.bodies[2]), &cb2)
	dec, _ := base64.StdEncoding.DecodeString(cb["content"].(string))
	// the FIRST file carries no branch (GitHub initializes its default branch); the rest go to main
	if _, has := cb["branch"]; has || string(dec) != "héllo" || cb["message"] != "Add README.md" || cb["sha"] != nil || cb2["branch"] != "main" || cb2["sha"] != nil {
		t.Fatalf("%v %v", cb, cb2)
	}
	if got := f.recorded(); got[0] != "PUT /repos/alice/demo/contents/README.md" || got[1] != "GET /repos/alice/demo" || got[2] != "PUT /repos/alice/demo/contents/a.txt" {
		t.Fatalf("%v", got)
	}
	// a failure midway stops: later files are not sent
	f.fail = map[string]int{"PUT /repos/alice/demo/contents/a.txt": 422}
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
	if err := a.CommitFiles(ctx, r, []CommitFile{{"README.md", "x"}, {"a.txt", "b"}, {"c.txt", "c"}}); err == nil || f.hits() != 3 {
		t.Fatalf("%v %v", err, f.recorded())
	}
	f.fail = map[string]int{}
	// the answer's path must be the file asked for
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(201)
		fmt.Fprint(w, `{"content":{"path":"other"},"commit":{"sha":"abc"}}`)
		return true
	}
	if err := a.CommitFiles(ctx, r, []CommitFile{{"README.md", "x"}}); ErrCode(err) != CodeGitHubRefused {
		t.Fatalf("%v", err)
	}
	f.override = nil
	// default branch and deploy key polarity
	f.repos["alice/demo"] = "alice/demo"
	if br, err := a.DefaultBranch(ctx, RepoInfo{ID: 31, Path: "alice/demo"}); err != nil {
		t.Fatalf("%v", err)
	} else if br != "" && br != "main" {
		t.Fatal(br)
	}
	for _, canPush := range []bool{true, false} {
		f.mu.Lock()
		f.reqs, f.bodies = nil, nil
		f.mu.Unlock()
		if err := a.AddDeployKey(ctx, r, "orchd demo", goodKey, canPush); err != nil {
			t.Fatal(err)
		}
		var kb map[string]any
		json.Unmarshal([]byte(f.bodies[0]), &kb)
		if kb["read_only"] != !canPush || kb["title"] != "orchd demo" || kb["key"] != goodKey || f.recorded()[0] != "POST /repos/alice/demo/keys" {
			t.Fatalf("%v %v", kb, f.recorded())
		}
	}
	// GitHub answering with the opposite polarity is refused
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":5,"read_only":false}`)
		return true
	}
	if err := a.AddDeployKey(ctx, r, "t", goodKey, false); ErrCode(err) != CodeGitHubRefused {
		t.Fatalf("polarity: %v", err)
	}
	// read_only is mandatory in the answer
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":5}`)
		return true
	}
	if err := a.AddDeployKey(ctx, r, "t", goodKey, false); ErrCode(err) != CodeGitHubRefused {
		t.Fatalf("missing read_only accepted: %v", err)
	}
}

func TestGitHubEnterprisePrefix(t *testing.T) {
	f := newFakeGH(t)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/api/v3/") {
			w.WriteHeader(404)
			return true
		}
		return false
	}
	// simulate GHES: the client asks for BaseURL + /api/v3 + path unless the host is api.github.com
	a := ghFor(t, f, ghTokens(t), nil)
	a.prefix = "/api/v3"
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v3/user" {
			fmt.Fprint(w, `{"login":"alice"}`)
			return true
		}
		if r.URL.Path == "/api/v3/user/orgs" {
			fmt.Fprint(w, `[]`)
			return true
		}
		w.WriteHeader(404)
		return true
	}
	if _, _, err := a.ListOwners(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubClientHasOneSendPath(t *testing.T) {
	b, _ := os.ReadFile("githubapi.go")
	s := string(b)
	for _, pat := range []string{"a.hc.Do(", "http.NewRequest"} {
		if strings.Count(s, pat) != 1 {
			t.Errorf("%q appears %d times, want exactly once (inside do)", pat, strings.Count(s, pat))
		}
	}
	for _, m := range []string{"http.MethodDelete", "http.MethodPatch", `"DELETE"`, `"PATCH"`, "ReadAll(resp.Body)", "Header.Get(\"Link\")", "\"Link\""} {
		if strings.Contains(s, m) {
			t.Errorf("client mentions %s", m)
		}
	}
}

func TestGitHubCanaryTokenNeverLogged(t *testing.T) {
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	ghRunAll(t, f, a)
	f.override = func(w http.ResponseWriter, r *http.Request) bool { panic("exploded " + ghCanary) }
	_, _, err := a.ListOwners(context.Background())
	if err != nil && (strings.Contains(err.Error(), ghCanary) || strings.Contains(err.Error(), "exploded")) {
		t.Fatalf("error leaks: %v", err)
	}
}

// ---------- GitLab through the interface ----------

func TestGitLabBehaviourUnchangedViaInterface(t *testing.T) {
	ctx := context.Background()
	f1, f2 := newFakeGL(t), newFakeGL(t)
	direct := apiFor(t, f1, apiTokens(t), nil)
	var p RepoProvider = newGitLabRepoProvider(apiFor(t, f2, apiTokens(t), nil))
	if p.Kind() != "gitlab" {
		t.Fatal(p.Kind())
	}
	// the same calls, once directly and once through the interface: identical wire traffic
	gs, gtrunc, _ := direct.GroupsTruncated(ctx)
	os_, otrunc, err := p.ListOwners(ctx)
	if err != nil || otrunc != gtrunc || len(os_) != len(gs) || os_[0].Path != gs[0].FullPath || os_[0].ID != gs[0].ID {
		t.Fatalf("%+v %v %v", os_, otrunc, err)
	}
	g, _ := direct.Group(ctx, "team/sub")
	o, err := p.GetOwner(ctx, "team/sub")
	if err != nil || o.Path != g.FullPath || o.ID != g.ID {
		t.Fatalf("%+v %v", o, err)
	}
	pr, _ := direct.Project(ctx, "team/demo")
	ri, err := p.GetRepo(ctx, "team/demo")
	if err != nil || ri.ID != pr.ID || ri.Path != pr.PathWithNamespace {
		t.Fatalf("%+v %v", ri, err)
	}
	cp, _ := direct.CreateProject(ctx, "team", 7, "demo", "d")
	owner, _ := p.GetOwner(ctx, "team")
	cr, err := p.CreateRepo(ctx, CreateRepoReq{Owner: owner, Name: "demo", Description: "d", Private: true})
	if err != nil || cr.ID != cp.ID || cr.Path != cp.PathWithNamespace {
		t.Fatalf("%+v %v", cr, err)
	}
	files := []CommitFile{{"README.md", "x"}, {"cmd/main.go", "z"}}
	direct.CommitFiles(ctx, cp.ID, files)
	if err := p.CommitFiles(ctx, cr, files); err != nil {
		t.Fatal(err)
	}
	direct.ProtectedMain(ctx, cp.ID)
	f1.mu.Lock()
	f1.empty = true
	f1.mu.Unlock()
	f2.mu.Lock()
	f2.empty = true
	f2.mu.Unlock()
	direct.EnsureProtectedMain(ctx, cp.ID)
	if err := p.EnsureProtectedMain(ctx, cr); err != nil {
		t.Fatal(err)
	}
	direct.AddDeployKey(ctx, cp.ID, "orchd demo", goodKey, true)
	if err := p.AddDeployKey(ctx, cr, "orchd demo", goodKey, true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DefaultBranch(ctx, cr); err != nil {
		t.Fatal(err)
	}
	// compare traffic (the interface's DefaultBranch adds one project GET at the end)
	r1, r2 := f1.recorded(), f2.recorded()
	// direct: Groups, Group, Project, Create..., interface order differs (lookup GETs): compare as multisets of non-GET writes
	writes := func(rs []string) string {
		var w []string
		for _, r := range rs {
			if !strings.HasPrefix(r, "GET ") {
				w = append(w, r)
			}
		}
		return strings.Join(w, "|")
	}
	if writes(r1) != writes(r2) {
		t.Fatalf("write traffic differs:\n%v\n%v", writes(r1), writes(r2))
	}
	// the interface refuses what GitLab must never do or what is not signed
	n := f2.hits()
	if _, err := p.CreateRepo(ctx, CreateRepoReq{Owner: owner, Name: "demo", Private: false}); ErrCode(err) != CodeBadInput {
		t.Fatalf("public GitLab project accepted: %v", err)
	}
	if _, err := p.CreateRepo(ctx, CreateRepoReq{Owner: Owner{ID: 7, Path: "team"}, Name: "demo", Private: true}); ErrCode(err) != CodeBadInput {
		t.Fatalf("forged owner accepted: %v", err)
	}
	if f2.hits() != n {
		t.Fatal("refused call reached GitLab")
	}
	// error semantics are the W3c ones
	f2.fail = map[string]int{"GET /api/v4/projects": 404}
	if _, err := p.GetRepo(ctx, "team/none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	f2.fail = map[string]int{}
	f2.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "protected_branches") && r.Method == "GET" {
			fmt.Fprint(w, `{"name":"main","allow_force_push":true,"push_access_levels":[{"access_level":40}],"merge_access_levels":[{"access_level":40}]}`)
			return true
		}
		return false
	}
	err = p.EnsureProtectedMain(ctx, cr)
	if !errors.Is(err, ErrProtectionWeaker) || WireCode(err) != CodeGitLabRefused {
		t.Fatalf("%v", err)
	}
	// GitLab's allowlist is untouched
	if len(allowlist) != 9 {
		t.Fatalf("gitlab allowlist has %d entries", len(allowlist))
	}
}

// TestSeamsNotReachableFromProduction extends TestProductionCannotEnableSeams to
// the second client and to any new package-level hook variable in the provider
// files: the seams type, both constructors and every function-typed global.
func TestSeamsNotReachableFromProduction(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
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
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if se, ok := l.(*ast.SelectorExpr); ok && (se.Sel.Name == "apiSeams" || se.Sel.Name == "policy" && f != "gitlabapi.go" && f != "githubapi.go") {
							t.Errorf("%s: %s assigns a seam field (%s)", f, fn, se.Sel.Name)
						}
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "apiSeams" {
						t.Errorf("%s: %s sets apiSeams in a literal", f, fn)
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "newGitHubAPI" || id.Name == "newGitLabAPI") && len(x.Args) == 3 {
						// the third argument must be nil, the Server's own field or the parameter of a constructor
						switch a := x.Args[2].(type) {
						case *ast.Ident:
							if a.Name != "nil" && a.Name != "sm" {
								t.Errorf("%s: %s passes %s as seams", f, fn, a.Name)
							}
						case *ast.SelectorExpr:
							if a.Sel.Name != "apiSeams" {
								t.Errorf("%s: %s passes a non-server seam", f, fn)
							}
						default:
							t.Errorf("%s: %s passes unexpected seams expression", f, fn)
						}
					}
				}
				return true
			})
			// package-level variables of the provider files may not be function values
			// (a settable hook) or maps/slices of them, except the fixed tables
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR && (f == "githubapi.go" || f == "gitlabapi.go" || f == "provider_iface.go" || f == "provider.go") {
				for _, sp := range gd.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, nm := range vs.Names {
						if nm.Name == "_" {
							continue
						}
						if i < len(vs.Values) {
							if _, isFn := vs.Values[i].(*ast.FuncLit); isFn {
								t.Errorf("%s: package-level function variable %s (a hook)", f, nm.Name)
							}
						}
						if ft, isFn := vs.Type.(*ast.FuncType); isFn && ft != nil {
							t.Errorf("%s: package-level function variable %s (a hook)", f, nm.Name)
						}
					}
				}
			}
		}
	}
	// nothing in the production sources may read the environment or loosen TLS/proxy
	for _, f := range []string{"githubapi.go", "provider_iface.go", "gitlabapi.go", "provider.go"} {
		b, _ := os.ReadFile(f)
		for _, bad := range []string{"os.Getenv", "os.LookupEnv", "flag.", "ProxyFromEnvironment", "InsecureSkipVerify", "http.DefaultClient", "http.DefaultTransport", "http.Get(", "http.Post("} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
	b, _ := os.ReadFile("githubapi.go")
	if !strings.Contains(string(b), "return newGitHubAPI(p, tokens, nil)") {
		t.Fatal("NewGitHubAPI must call newGitHubAPI with nil seams")
	}
	if !strings.Contains(string(b), "Proxy:               nil") {
		t.Fatal("the GitHub transport must set Proxy: nil")
	}
	if !strings.Contains(string(b), "http.ErrUseLastResponse") {
		t.Fatal("redirects must be refused")
	}
}

func TestGitHubFirstFileOmitsBranchAndChecksDefault(t *testing.T) {
	ctx := context.Background()
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	r := ghMade(t, a, "demo")
	f.defBranch = "master" // the account's default branch name differs
	f.mu.Lock()
	f.reqs, f.bodies = nil, nil
	f.mu.Unlock()
	err := a.CommitFiles(ctx, r, []CommitFile{{"README.md", "x"}, {"a.txt", "b"}, {"c.txt", "c"}})
	if !errors.Is(err, ErrDefaultBranchMismatch) || ErrCode(err) != CodeDefaultBranchMismatch {
		t.Fatalf("mismatch: %v", err)
	}
	if got := f.recorded(); len(got) != 2 || !strings.HasPrefix(got[0], "PUT ") || got[1] != "GET /repos/alice/demo" {
		t.Fatalf("nothing more may be written after a mismatch: %v", got)
	}
	var b map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &b)
	if _, has := b["branch"]; has {
		t.Fatalf("first file carries a branch: %v", b)
	}
	// a repository that now answers with another id is not ours
	f.defBranch = ""
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"id":99,"full_name":"alice/demo","default_branch":"main","private":true}`)
			return true
		}
		return false
	}
	if _, err := a.DefaultBranch(ctx, r); ErrCode(err) != CodeGitHubRefused {
		t.Fatalf("id mismatch accepted: %v", err)
	}
}

func TestGitHubJustCreatedBoundToThisClient(t *testing.T) {
	ctx := context.Background()
	f := newFakeGH(t)
	ts := ghTokens(t)
	a := ghFor(t, f, ts, nil)
	r := ghMade(t, a, "demo")
	puts := func() int {
		n := 0
		for _, q := range f.recorded() {
			if strings.HasPrefix(q, "PUT ") {
				n++
			}
		}
		return n
	}
	// forged variants of a real handle: another id, another path, a hand-built one
	other := ghFor(t, f, ts, nil) // a second client instance (a different flow)
	for name, h := range map[string]RepoInfo{
		"other-id":   {ID: r.ID + 1, Path: r.Path, justCreated: true},
		"other-path": {ID: r.ID, Path: "alice/other", justCreated: true},
		"no-flag":    {ID: r.ID, Path: r.Path},
	} {
		n := puts()
		if err := a.CommitFiles(ctx, h, []CommitFile{{"README.md", "x"}}); ErrCode(err) != CodeBadInput && name != "no-flag" && name != "other-id" {
			t.Errorf("%s: CommitFiles %v", name, err)
		}
		if err := a.CommitFiles(ctx, h, []CommitFile{{"README.md", "x"}}); err == nil {
			t.Errorf("%s: CommitFiles accepted", name)
		}
		if err := a.EnsureProtectedMain(ctx, h); err == nil {
			t.Errorf("%s: protection written", name)
		}
		if puts() != n {
			t.Errorf("%s: a PUT was sent for a forged handle", name)
		}
	}
	// the genuine handle copied to another client instance is refused too
	n := puts()
	if err := other.CommitFiles(ctx, r, []CommitFile{{"README.md", "x"}}); ErrCode(err) != CodeBadInput {
		t.Fatalf("foreign instance: %v", err)
	}
	if err := other.EnsureProtectedMain(ctx, r); !errors.Is(err, ErrProtectionWeaker) || puts() != n {
		t.Fatalf("foreign instance protection: %v", err)
	}
	// the genuine one works, and a case variant of the same name is the same repo
	if err := a.EnsureProtectedMain(ctx, r); err != nil || puts() != n+1 {
		t.Fatalf("genuine handle: %v", err)
	}
}

func TestGitHubProtectionResponseVerified(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"force":    `{"allow_force_pushes":{"enabled":true},"allow_deletions":{"enabled":false},"enforce_admins":{"enabled":true}}`,
		"delete":   `{"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":true},"enforce_admins":{"enabled":true}}`,
		"noadmins": `{"allow_force_pushes":{"enabled":false},"enforce_admins":{"enabled":false}}`,
	} {
		f := newFakeGH(t)
		a := ghFor(t, f, ghTokens(t), nil)
		r := ghMade(t, a, "demo")
		f.override = func(w http.ResponseWriter, rq *http.Request) bool {
			if rq.Method == "PUT" {
				fmt.Fprint(w, body)
				return true
			}
			return false
		}
		if err := a.EnsureProtectedMain(ctx, r); !errors.Is(err, ErrProtectionWeaker) {
			t.Errorf("%s: weak PUT answer accepted: %v", name, err)
		}
	}
	// allow_deletions absent in a strong answer is fine
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	r := ghMade(t, a, "demo")
	f.override = func(w http.ResponseWriter, rq *http.Request) bool {
		if rq.Method == "PUT" {
			fmt.Fprint(w, `{"allow_force_pushes":{"enabled":false},"enforce_admins":{"enabled":true}}`)
			return true
		}
		return false
	}
	if err := a.EnsureProtectedMain(ctx, r); err != nil {
		t.Fatalf("strong answer refused: %v", err)
	}
	// an answer without the fields is not proof either
	f2 := newFakeGH(t)
	a2 := ghFor(t, f2, ghTokens(t), nil)
	r2 := ghMade(t, a2, "demo")
	f2.override = func(w http.ResponseWriter, rq *http.Request) bool {
		if rq.Method == "PUT" {
			fmt.Fprint(w, `{}`)
			return true
		}
		return false
	}
	if err := a2.EnsureProtectedMain(ctx, r2); !errors.Is(err, ErrProtectionWeaker) {
		t.Fatalf("empty PUT answer accepted: %v", err)
	}
}

func TestGitHubLoginAllowsUnderscore(t *testing.T) {
	for _, ok := range []string{"octo_acme", "a_b", "Octo-Cat_x1"} {
		if !validGHLogin(ok) {
			t.Errorf("%q refused", ok)
		}
		if !githubAllowed("GET", "/repos/"+ok+"/demo", nil) {
			t.Errorf("allowlist refuses owner %q", ok)
		}
	}
	for _, bad := range []string{"_a", "-a", "a.b", "a b", "a/b", "a%5Fb", "ü", ""} {
		if validGHLogin(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGitHubWriteNotFoundIsRefusalNotAbsence(t *testing.T) {
	ctx := context.Background()
	f := newFakeGH(t)
	a := ghFor(t, f, ghTokens(t), nil)
	r := ghMade(t, a, "demo")
	f.fail = map[string]int{"PUT /": 404, "POST /": 404}
	owners, _, _ := a.ListOwners(ctx)
	_, e1 := a.CreateRepo(ctx, CreateRepoReq{Owner: owners[0], Name: "x1", Private: true})
	e2 := a.CommitFiles(ctx, r, []CommitFile{{"README.md", "x"}})
	e3 := a.AddDeployKey(ctx, r, "t", goodKey, false)
	f.fail = map[string]int{"PUT /": 404}
	f.protection = ""
	e4 := a.EnsureProtectedMain(ctx, r) // GET 404 -> absent -> PUT 404
	for i, e := range []error{e1, e2, e3, e4} {
		if ErrCode(e) != CodeGitHubRefused || errors.Is(e, ErrNotFound) || ErrStatus(e) != 404 {
			t.Errorf("%d: %v", i, e)
		}
	}
	// a read 404 is still "absent"
	f.fail = map[string]int{}
	if _, err := a.GetRepo(ctx, "alice/none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestProviderSentencesAreNeutral(t *testing.T) {
	for _, c := range []string{"provider_unreachable", "bad_url", "group_unknown", "project_name_taken", "create_partial", CodeGitHubRefused, CodeProviderRateLimited} {
		m, ok := fixedMessages[c]
		if !ok {
			t.Errorf("%s not registered", c)
			continue
		}
		if strings.Contains(m, "GitLab") && !strings.Contains(m, "GitHub") && c != "gitlab_refused" {
			t.Errorf("%s names only GitLab: %q", c, m)
		}
		if c == CodeGitHubRefused && strings.Contains(m, "GitLab") {
			t.Errorf("github_refused names GitLab: %q", m)
		}
	}
	if CodeNotFound != "not_found" {
		t.Errorf("CodeNotFound = %q", CodeNotFound)
	}
}
