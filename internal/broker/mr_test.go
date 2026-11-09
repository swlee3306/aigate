package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type mrReq struct {
	Method, Path, Query string
	Header              http.Header
	Body                string
}

// fakeGitLab records every request and answers like GitLab unless over is set.
type fakeGitLab struct {
	*fakeHTTP
	mu       sync.Mutex
	reqs     []mrReq
	listJSON string // GET answer
	over     func(w http.ResponseWriter, r *http.Request, n int) bool
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	g := &fakeGitLab{listJSON: `[]`}
	g.fakeHTTP = newFakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.reqs = append(g.reqs, mrReq{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(b)})
		n := len(g.reqs)
		over := g.over
		list := g.listJSON
		g.mu.Unlock()
		if over != nil && over(w, r, n) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			io.WriteString(w, list)
			return
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"iid":7,"web_url":"`+g.URL+`/grp/proj/-/merge_requests/7","state":"opened","source_branch":"orch/12","target_branch":"main"}`)
	})
	return g
}

func (g *fakeGitLab) Reqs() []mrReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]mrReq{}, g.reqs...)
}

type mrEnv struct {
	*refineEnv
	gl *fakeGitLab
}

func newMREnv(t *testing.T, cfg func(*testEnv, *testing.T)) *mrEnv {
	t.Helper()
	gl := newFakeGitLab(t)
	re := newRefineEnv(t, nil, cfg)
	re.srv.GitLab.BaseURL = gl.URL
	return &mrEnv{refineEnv: re, gl: gl}
}

func (m *mrEnv) args() map[string]any {
	return map[string]any{
		"repo": m.repo, "branch": "orch/12", "task": 12,
		"title_hint": "Add healthz endpoint", "summary": "Adds /healthz returning ok.",
		"actor": "user:alice", "verdict": "pass", "head": strings.Repeat("a", 40),
	}
}

func (m *mrEnv) call(t *testing.T, args any) (map[string]any, *OpError) {
	t.Helper()
	return m.callCtx(t, context.Background(), args)
}

func (m *mrEnv) callCtx(t *testing.T, ctx context.Context, args any) (map[string]any, *OpError) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, oerr := m.srv.Handlers["create_mr"](ctx, raw)
	if oerr != nil {
		return nil, oerr
	}
	b, _ := json.Marshal(res)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func (m *mrEnv) posts() []mrReq {
	var p []mrReq
	for _, r := range m.gl.Reqs() {
		if r.Method == http.MethodPost {
			p = append(p, r)
		}
	}
	return p
}

func TestCreateMRHappyPath(t *testing.T) {
	m := newMREnv(t, nil)
	out, oerr := m.call(t, m.args())
	if oerr != nil {
		t.Fatalf("err %v", oerr.Code)
	}
	if out["iid"].(float64) != 7 || out["created"] != true || out["url"] != m.gl.URL+"/grp/proj/-/merge_requests/7" {
		t.Fatalf("result %v", out)
	}
	ps := m.posts()
	if len(ps) != 1 {
		t.Fatalf("posts %d", len(ps))
	}
	if ps[0].Path != "/api/v4/projects/grp%2Fproj/merge_requests" {
		t.Fatalf("path %s", ps[0].Path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(ps[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["source_branch"] != "orch/12" || body["target_branch"] != "main" || body["draft"] != true {
		t.Fatalf("body %v", body)
	}
	title, _ := body["title"].(string)
	if title != "Draft: orch task 12: Add healthz endpoint" {
		t.Fatalf("title %q", title)
	}
	desc, _ := body["description"].(string)
	for _, w := range []string{"12", "orch/12", strings.Repeat("a", 40), "user:alice", "pass", "Adds /healthz returning ok."} {
		if !strings.Contains(desc, w) {
			t.Fatalf("description lacks %q: %s", w, desc)
		}
	}
	if body["remove_source_branch"] == true {
		t.Fatal("remove_source_branch must stay off")
	}
	for k := range body {
		switch k {
		case "source_branch", "target_branch", "title", "description", "draft", "remove_source_branch", "squash":
		default:
			t.Fatalf("unexpected body field %s", k)
		}
	}
	if body["squash"] == true {
		t.Fatal("squash must stay off")
	}
}

func TestMRBranchPatternExact(t *testing.T) {
	m := newMREnv(t, func(e *testEnv, t *testing.T) { e.set(t, []string{"limits", "create_mr_per_hour"}, 100) })
	for _, b := range []string{"orch/12/../x", "main", "orch/", "orch/abc", " orch/12", "orch/12 ", "orch/12\n", "orch/12\n\n", "orch/1234567890", "orch/-1", "orch/１２", "Orch/12", "orch/12/", "", "orch//12", "orch/12%0a"} {
		a := m.args()
		a["branch"] = b
		_, oerr := m.call(t, a)
		wantCode(t, oerr, "bad_branch")
	}
	if len(m.gl.Reqs()) != 0 {
		t.Fatal("bad branch reached GitLab")
	}
	for b, n := range map[string]int{"orch/12": 12, "orch/1": 1, "orch/123456789": 123456789} {
		a := m.args()
		a["branch"] = b
		a["task"] = n
		if _, oerr := m.call(t, a); oerr != nil {
			t.Fatalf("%s: %s", b, oerr.Code)
		}
	}
}

func TestMRRepoMustBeMapped(t *testing.T) {
	m := newMREnv(t, nil)
	for _, r := range []string{"/etc", "", "grp/proj", m.repo + "/", m.repo + "/../repo-a", "relative"} {
		a := m.args()
		a["repo"] = r
		_, oerr := m.call(t, a)
		wantCode(t, oerr, "repo_not_mapped")
	}
	// The project path comes only from the config: a client-supplied one is refused.
	a := m.args()
	a["project"] = "evil/proj"
	_, oerr := m.call(t, a)
	wantCode(t, oerr, "bad_request")
	if len(m.gl.Reqs()) != 0 {
		t.Fatal("request reached GitLab")
	}
}

func TestMRExistingOpenMRIsReturnedNotDuplicated(t *testing.T) {
	// Case 1: found by the list query.
	m := newMREnv(t, nil)
	m.gl.listJSON = `[{"iid":3,"web_url":"` + m.gl.URL + `/grp/proj/-/merge_requests/3","state":"opened","source_branch":"orch/12","target_branch":"main"}]`
	out, oerr := m.call(t, m.args())
	if oerr != nil {
		t.Fatal(oerr.Code)
	}
	if out["created"] != false || out["iid"].(float64) != 3 {
		t.Fatalf("result %v", out)
	}
	if len(m.posts()) != 0 {
		t.Fatal("created a duplicate")
	}
	q, _ := url.ParseQuery(m.gl.Reqs()[0].Query)
	if q.Get("state") != "opened" || q.Get("source_branch") != "orch/12" {
		t.Fatalf("query %v", q)
	}
	if m.gl.Reqs()[0].Path != "/api/v4/projects/grp%2Fproj/merge_requests" {
		t.Fatalf("path %s", m.gl.Reqs()[0].Path)
	}

	// Case 2: list is empty, create answers 409, the retry lookup finds it.
	m2 := newMREnv(t, nil)
	var mu sync.Mutex
	gets := 0
	m2.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if r.Method == http.MethodPost {
			w.WriteHeader(409)
			io.WriteString(w, `{"message":["Another open merge request already exists for this source branch: !5"]}`)
			return true
		}
		mu.Lock()
		gets++
		g := gets
		mu.Unlock()
		if g >= 2 {
			io.WriteString(w, `[{"iid":5,"web_url":"`+m2.gl.URL+`/grp/proj/-/merge_requests/5","state":"opened","source_branch":"orch/12","target_branch":"main"}]`)
			return true
		}
		io.WriteString(w, `[]`)
		return true
	}
	out, oerr = m2.call(t, m2.args())
	if oerr != nil {
		t.Fatal(oerr.Code)
	}
	if out["created"] != false || out["iid"].(float64) != 5 {
		t.Fatalf("result %v", out)
	}

	// Case 3: entries of other branches or states are not taken for the MR.
	m3 := newMREnv(t, nil)
	m3.gl.listJSON = `[{"iid":3,"web_url":"` + m3.gl.URL + `/x/-/merge_requests/3","state":"opened","source_branch":"orch/99","target_branch":"main"},{"iid":4,"web_url":"` + m3.gl.URL + `/x/-/merge_requests/4","state":"closed","source_branch":"orch/12","target_branch":"main"}]`
	out, oerr = m3.call(t, m3.args())
	if oerr != nil || out["created"] != true || len(m3.posts()) != 1 {
		t.Fatalf("result %v %v", out, oerr)
	}
}

func TestMRTitleAndDescriptionBuiltByBrokerOnly(t *testing.T) {
	m := newMREnv(t, nil)
	a := m.args()
	a["title_hint"] = "Fix [click](http://evil.example/x) <b>bold</b> `code` @admin‮​\x07\nsecond\r\nline " + strings.Repeat("Z", 5000)
	a["summary"] = "See [here](https://evil.example/p)\n\n# Heading\n![img](http://evil.example/i.png) http://evil.example/auto\x00" + strings.Repeat("S", 5000)
	a["verdict"] = "pass‮\n[x](http://evil)"
	_, oerr := m.call(t, a)
	if oerr != nil {
		t.Fatal(oerr.Code)
	}
	ps := m.posts()
	if len(ps) != 1 {
		t.Fatal("no post")
	}
	var body map[string]any
	json.Unmarshal([]byte(ps[0].Body), &body)
	title := body["title"].(string)
	desc := body["description"].(string)
	if !strings.HasPrefix(title, "Draft: orch task 12: Fix ") {
		t.Fatalf("title %q", title)
	}
	hint := strings.TrimPrefix(title, "Draft: orch task 12: ")
	if len([]rune(hint)) > 120 {
		t.Fatalf("title hint %d runes", len([]rune(hint)))
	}
	if strings.ContainsAny(title, "\n\r[]()<>`@") || strings.Contains(title, "://") {
		t.Fatalf("title %q", title)
	}
	for _, s := range []string{title, desc} {
		for _, r := range s {
			if r == '‮' || r == '​' || (r < 0x20 && r != '\n') || r == 0x7f {
				t.Fatalf("control char %U in %q", r, s)
			}
		}
	}
	if len([]rune(desc)) > 1500 {
		t.Fatalf("description too large: %d", len([]rune(desc)))
	}
	if strings.Contains(desc, "](") || strings.Contains(desc, "://") || strings.Contains(desc, "![") || strings.Contains(desc, "<") || strings.Contains(desc, "\n#") || strings.Contains(desc, "@admin") {
		t.Fatalf("markup survived: %q", desc)
	}
	// Client-supplied raw fields never travel as request fields.
	for k := range body {
		switch k {
		case "title_hint", "summary", "actor", "verdict", "head", "task", "branch", "repo":
			t.Fatalf("raw field %s forwarded", k)
		}
	}
	// Empty hint after cleaning still gives a fixed title.
	m2 := newMREnv(t, nil)
	a2 := m2.args()
	a2["title_hint"] = "\u200b\x07 \n"
	if _, oerr := m2.call(t, a2); oerr != nil {
		t.Fatal(oerr.Code)
	}
	var b2 map[string]any
	json.Unmarshal([]byte(m2.posts()[0].Body), &b2)
	if b2["title"] != "Draft: orch task 12" {
		t.Fatalf("title %q", b2["title"])
	}
	// Malformed fields are refused.
	for k, v := range map[string]any{"actor": "x\ny", "head": "nothex", "task": -1, "verdict": 5, "title_hint": 3, "summary": []string{"a"}} {
		bad := m.args()
		bad[k] = v
		_, oerr := m.call(t, bad)
		wantCode(t, oerr, "bad_request")
	}
}

func TestMRUrlMustStartWithBaseUrl(t *testing.T) {
	other := newFakeHTTP(t, nil)
	for _, wu := range []string{
		"http://evil.example/grp/proj/-/merge_requests/7",
		other.URL + "/x",
		"javascript:alert(1)",
		"",
		"/relative/path",
	} {
		m := newMREnv(t, nil)
		wu := wu
		m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
			if r.Method == http.MethodGet {
				io.WriteString(w, `[]`)
				return true
			}
			w.WriteHeader(201)
			b, _ := json.Marshal(map[string]any{"iid": 7, "web_url": wu, "state": "opened", "target_branch": "main"})
			w.Write(b)
			return true
		}
		_, oerr := m.call(t, m.args())
		wantCode(t, oerr, "bad_url")
	}
	// Same prefix text but a different host (base + ".evil") is rejected too.
	m := newMREnv(t, nil)
	m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if r.Method == http.MethodGet {
			io.WriteString(w, `[]`)
			return true
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"iid":7,"web_url":"`+m.gl.URL+`.evil.example/x","state":"opened","target_branch":"main"}`)
		return true
	}
	_, oerr := m.call(t, m.args())
	wantCode(t, oerr, "bad_url")
	if other.Hits() != 0 {
		t.Fatal("other host contacted")
	}
	// The same rule applies to an existing MR found by the list query.
	m = newMREnv(t, nil)
	m.gl.listJSON = `[{"iid":3,"web_url":"http://evil.example/mr/3","state":"opened","source_branch":"orch/12","target_branch":"main"}]`
	_, oerr = m.call(t, m.args())
	wantCode(t, oerr, "bad_url")
}

func TestMRNoRedirectAndPinnedHost(t *testing.T) {
	target := newFakeHTTP(t, nil)
	for _, code := range []int{301, 302, 307, 308} {
		m := newMREnv(t, nil)
		m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
			http.Redirect(w, r, target.URL+"/api/v4/projects/grp%2Fproj/merge_requests", code)
			return true
		}
		_, oerr := m.call(t, m.args())
		wantCode(t, oerr, "upstream_error")
		if target.Hits() != 0 {
			t.Fatalf("redirect %d followed", code)
		}
		for _, r := range m.gl.Reqs() {
			if r.Header.Get("PRIVATE-TOKEN") == "" {
				t.Fatal("test setup: no token on request")
			}
		}
	}
	// A POST redirect after a clean list is not followed either.
	m := newMREnv(t, nil)
	m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if r.Method == http.MethodGet {
			io.WriteString(w, `[]`)
			return true
		}
		http.Redirect(w, r, target.URL+"/x", 307)
		return true
	}
	_, oerr := m.call(t, m.args())
	wantCode(t, oerr, "upstream_error")
	if target.Hits() != 0 {
		t.Fatal("POST redirect followed")
	}
	// Every request went to the configured host only.
	for _, r := range m.gl.Reqs() {
		if !strings.HasPrefix(r.Path, "/api/v4/projects/grp%2Fproj/merge_requests") {
			t.Fatalf("path %s", r.Path)
		}
	}
	// The default client is hardened: no redirects, no proxy.
	c := newGitLabHTTP()
	if c.CheckRedirect == nil {
		t.Fatal("no redirect policy")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatal("redirects allowed")
	}
	if tr, ok := c.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("proxy enabled")
	}
}

func TestMRTokenOnlyInHeader(t *testing.T) {
	m := newMREnv(t, nil)
	m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if r.Method == http.MethodGet {
			io.WriteString(w, `[]`)
			return true
		}
		return false
	}
	if _, oerr := m.call(t, m.args()); oerr != nil {
		t.Fatal(oerr.Code)
	}
	for _, r := range m.gl.Reqs() {
		if r.Header.Get("PRIVATE-TOKEN") != testGitLabToken {
			t.Fatalf("token header %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatal("Authorization header sent")
		}
		if strings.Contains(r.Path+r.Query+r.Body, testGitLabToken) {
			t.Fatal("token in path/query/body")
		}
		for k, v := range r.Header {
			if k != "Private-Token" && strings.Contains(strings.Join(v, ","), testGitLabToken) {
				t.Fatalf("token in header %s", k)
			}
		}
	}
	// An upstream that echoes the token and hostile text: never in reply or logs.
	m2 := newMREnv(t, nil)
	m2.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		w.WriteHeader(500)
		io.WriteString(w, testGitLabToken+" glpat-abcdefghij0123456789 "+hostileSentinel)
		return true
	}
	_, oerr := m2.call(t, m2.args())
	wantCode(t, oerr, "upstream_error")
	all := oerr.Code + oerr.Message + m2.logs.String()
	for _, bad := range []string{testGitLabToken, "glpat-", hostileSentinel} {
		if strings.Contains(all, bad) {
			t.Fatalf("leak of %q", bad)
		}
	}
	// And through the socket.
	r, out := call(t, m2.env.Raw["socket"].(string), mustJSON(t, map[string]any{"v": 1, "op": "create_mr", "args": m2.args()}))
	if r.OK || r.Code != "upstream_error" || strings.Contains(string(out), testGitLabToken) || strings.Contains(string(out), hostileSentinel) {
		t.Fatalf("socket reply %s", out)
	}
	if strings.Contains(m2.logs.String(), testGitLabToken) || strings.Contains(m2.logs.String(), hostileSentinel) {
		t.Fatal("log leak")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMRUpstreamErrorsCollapse(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"500":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"401":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
		"403":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
		"404":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) },
		"422":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(422) },
		"429":        func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) },
		"garbage":    func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>not json</html>") },
		"listObject": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"iid":1}`) },
		"hijack": func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		},
		"huge": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `[`+strings.Repeat(" ", 3<<20)+`]`)
		},
	}
	for name, h := range cases {
		m := newMREnv(t, nil)
		h := h
		m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool { h(w, r); return true }
		_, oerr := m.call(t, m.args())
		if oerr == nil || oerr.Code != "upstream_error" {
			t.Fatalf("%s: %+v", name, oerr)
		}
	}
	// Create-side failures after a clean list.
	create := map[string]string{
		"nobody":     ``,
		"badjson":    `{`,
		"noiid":      `{"web_url":"%U/x","state":"opened","target_branch":"main"}`,
		"zeroiid":    `{"iid":0,"web_url":"%U/x","state":"opened","target_branch":"main"}`,
		"strIID":     `{"iid":"7","web_url":"%U/x","state":"opened","target_branch":"main"}`,
		"noURL":      `{"iid":7,"state":"opened","target_branch":"main"}`,
		"array":      `[]`,
		"trailing":   `{"iid":7,"web_url":"%U/x","state":"opened","target_branch":"main"} {}`,
		"wrongstate": `{"iid":7,"web_url":"%U/x","state":"merged","target_branch":"main"}`,
	}
	for name, body := range create {
		m := newMREnv(t, nil)
		body := strings.ReplaceAll(body, "%U", m.gl.URL)
		m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
			if r.Method == http.MethodGet {
				io.WriteString(w, `[]`)
				return true
			}
			w.WriteHeader(201)
			io.WriteString(w, body)
			return true
		}
		_, oerr := m.call(t, m.args())
		if oerr == nil || oerr.Code != "upstream_error" {
			t.Fatalf("%s: %+v", name, oerr)
		}
	}
	// Timeout.
	m := newMREnv(t, nil)
	m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, oerr := m.callCtx(t, ctx, m.args())
	wantCode(t, oerr, "upstream_error")
	// A dead host.
	m = newMREnv(t, nil)
	m.srv.GitLab.BaseURL = "http://127.0.0.1:1"
	_, oerr = m.call(t, m.args())
	wantCode(t, oerr, "upstream_error")
}

func TestMRHourlyLimit(t *testing.T) {
	m := newMREnv(t, func(e *testEnv, t *testing.T) { e.set(t, []string{"limits", "create_mr_per_hour"}, 2) })
	for i := 0; i < 2; i++ {
		if _, oerr := m.call(t, m.args()); oerr != nil {
			t.Fatalf("call %d: %s", i, oerr.Code)
		}
	}
	before := len(m.gl.Reqs())
	_, oerr := m.call(t, m.args())
	wantCode(t, oerr, "rate_limited")
	if len(m.gl.Reqs()) != before {
		t.Fatal("limited call reached GitLab")
	}
	// Invalid requests do not use up the limit.
	m2 := newMREnv(t, func(e *testEnv, t *testing.T) { e.set(t, []string{"limits", "create_mr_per_hour"}, 1) })
	bad := m2.args()
	bad["branch"] = "main"
	m2.call(t, bad)
	if _, oerr := m2.call(t, m2.args()); oerr != nil {
		t.Fatalf("limit consumed by invalid request: %s", oerr.Code)
	}
}

func TestMRNeverMerges(t *testing.T) {
	scenarios := map[string]func(m *mrEnv){
		"create": func(m *mrEnv) {},
		"existing": func(m *mrEnv) {
			m.gl.listJSON = `[{"iid":3,"web_url":"` + m.gl.URL + `/x/-/merge_requests/3","state":"opened","source_branch":"orch/12","target_branch":"main"}]`
		},
		"conflict": func(m *mrEnv) {
			n := 0
			m.gl.over = func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if r.Method == http.MethodPost {
					w.WriteHeader(409)
					return true
				}
				n++
				if n > 1 {
					io.WriteString(w, `[{"iid":5,"web_url":"`+m.gl.URL+`/x/-/merge_requests/5","state":"opened","source_branch":"orch/12","target_branch":"main"}]`)
				} else {
					io.WriteString(w, `[]`)
				}
				return true
			}
		},
		"failure": func(m *mrEnv) {
			m.gl.over = func(w http.ResponseWriter, r *http.Request, _ int) bool { w.WriteHeader(500); return true }
		},
		"hostileURL": func(m *mrEnv) {
			m.gl.over = func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if r.Method == http.MethodGet {
					io.WriteString(w, `[]`)
				} else {
					w.WriteHeader(201)
					io.WriteString(w, `{"iid":7,"web_url":"`+m.gl.URL+`/merge","state":"opened","target_branch":"main","merge_when_pipeline_succeeds":true}`)
				}
				return true
			}
		},
	}
	for name, setup := range scenarios {
		m := newMREnv(t, nil)
		setup(m)
		m.call(t, m.args())
		reqs := m.gl.Reqs()
		if len(reqs) == 0 {
			t.Fatalf("%s: no requests recorded", name)
		}
		for _, r := range reqs {
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				t.Fatalf("%s: method %s", name, r.Method)
			}
			want := "/api/v4/projects/grp%2Fproj/merge_requests"
			if r.Path != want {
				t.Fatalf("%s: path %s", name, r.Path)
			}
			if strings.Contains(strings.ToLower(r.Path+r.Query), "merge_when") || strings.Contains(strings.ToLower(r.Body), "merge_when") || strings.Contains(strings.ToLower(r.Body), "auto_merge") {
				t.Fatalf("%s: merge hint in request", name)
			}
		}
	}
}

func TestMRArgsStrict(t *testing.T) {
	m := newMREnv(t, nil)
	for name, raw := range map[string]string{
		"unknown":    `{"repo":"` + m.repo + `","branch":"orch/12","task":12,"title_hint":"","summary":"","actor":"user:a","verdict":"","head":"` + strings.Repeat("a", 40) + `","extra":1}`,
		"wrongcase":  `{"Repo":"` + m.repo + `","branch":"orch/12","task":12,"title_hint":"","summary":"","actor":"user:a","verdict":"","head":"` + strings.Repeat("a", 40) + `"}`,
		"trailing":   `{} {}`,
		"notobject":  `[]`,
		"empty":      ``,
		"fractional": `{"repo":"` + m.repo + `","branch":"orch/12","task":1.5,"title_hint":"","summary":"","actor":"user:a","verdict":"","head":"` + strings.Repeat("a", 40) + `"}`,
	} {
		res, oerr := m.srv.Handlers["create_mr"](context.Background(), json.RawMessage(raw))
		if res != nil || oerr == nil || oerr.Code != "bad_request" {
			t.Fatalf("%s: %+v", name, oerr)
		}
	}
	if len(m.gl.Reqs()) != 0 {
		t.Fatal("strict args reached GitLab")
	}
}

func TestMRExistingMustMatchTargetBranch(t *testing.T) {
	// A list entry for another target is not "the" MR: a new one is created.
	m := newMREnv(t, nil)
	m.gl.listJSON = `[{"iid":3,"web_url":"` + m.gl.URL + `/grp/proj/-/merge_requests/3","state":"opened","source_branch":"orch/12","target_branch":"other"}]`
	out, oerr := m.call(t, m.args())
	if oerr != nil || out["created"] != true || len(m.posts()) != 1 {
		t.Fatalf("result %v %v", out, oerr)
	}
	// 409 and a re-list with only other-target MRs: upstream_error.
	m2 := newMREnv(t, nil)
	m2.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if r.Method == http.MethodPost {
			w.WriteHeader(409)
			return true
		}
		io.WriteString(w, `[{"iid":3,"web_url":"`+m2.gl.URL+`/x/-/merge_requests/3","state":"opened","source_branch":"orch/12","target_branch":"other"}]`)
		return true
	}
	_, oerr = m2.call(t, m2.args())
	wantCode(t, oerr, "upstream_error")
	// The create response must carry the configured target.
	for _, tb := range []string{`"target_branch":"other",`, ``} {
		m3 := newMREnv(t, nil)
		tb := tb
		m3.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
			if r.Method == http.MethodGet {
				io.WriteString(w, `[]`)
				return true
			}
			w.WriteHeader(201)
			io.WriteString(w, `{`+tb+`"iid":7,"web_url":"`+m3.gl.URL+`/x","state":"opened"}`)
			return true
		}
		_, oerr = m3.call(t, m3.args())
		wantCode(t, oerr, "upstream_error")
	}
}

func TestMRBranchMustMatchTask(t *testing.T) {
	m := newMREnv(t, nil)
	for name, mod := range map[string]func(map[string]any){
		"task mismatch": func(a map[string]any) { a["task"] = 99 },
		"leading zero":  func(a map[string]any) { a["branch"] = "orch/012" },
	} {
		a := m.args()
		mod(a)
		_, oerr := m.call(t, a)
		if oerr == nil || (oerr.Code != "bad_branch" && oerr.Code != "bad_request") {
			t.Fatalf("%s: %+v", name, oerr)
		}
	}
	if len(m.gl.Reqs()) != 0 {
		t.Fatal("mismatch reached GitLab")
	}
}

func TestMRConcurrentSameBranchCreatesOnce(t *testing.T) {
	m := newMREnv(t, func(e *testEnv, t *testing.T) { e.set(t, []string{"limits", "create_mr_per_hour"}, 1000) })
	var mu sync.Mutex
	created := false
	posts := 0
	m.gl.over = func(w http.ResponseWriter, r *http.Request, n int) bool {
		mu.Lock()
		defer mu.Unlock()
		item := `{"iid":7,"web_url":"` + m.gl.URL + `/grp/proj/-/merge_requests/7","state":"opened","source_branch":"orch/12","target_branch":"main"}`
		if r.Method == http.MethodGet {
			if created {
				io.WriteString(w, `[`+item+`]`)
			} else {
				io.WriteString(w, `[]`)
			}
			return true
		}
		posts++
		time.Sleep(50 * time.Millisecond) // widen the race window
		created = true
		w.WriteHeader(201)
		io.WriteString(w, item)
		return true
	}
	const n = 8
	var wg sync.WaitGroup
	var rmu sync.Mutex
	nCreated := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, oerr := m.call(t, m.args())
			if oerr != nil {
				t.Errorf("err %s", oerr.Code)
				return
			}
			if out["iid"].(float64) != 7 {
				t.Errorf("iid %v", out["iid"])
			}
			if out["created"] == true {
				rmu.Lock()
				nCreated++
				rmu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(m.posts()) != 1 || nCreated != 1 {
		t.Fatalf("posts=%d created=%d", len(m.posts()), nCreated)
	}
}

func TestMRNoQuickActionLines(t *testing.T) {
	for _, in := range []string{"\n/merge", "/approve", "\r\n/submit_review", "x\n\n/close", " /merge", "a\r/merge"} {
		m := newMREnv(t, nil)
		a := m.args()
		a["title_hint"] = in
		a["summary"] = in
		a["verdict"] = in
		if _, oerr := m.call(t, a); oerr != nil {
			t.Fatal(oerr.Code)
		}
		var body map[string]any
		json.Unmarshal([]byte(m.posts()[0].Body), &body)
		for _, f := range []string{"title", "description"} {
			for _, line := range strings.FieldsFunc(body[f].(string), func(r rune) bool { return r == '\n' || r == '\r' || r == ' ' }) {
				if strings.HasPrefix(strings.TrimSpace(line), "/") {
					t.Fatalf("%q: %s line starts with slash: %q", in, f, line)
				}
			}
		}
	}
}
