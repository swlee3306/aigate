package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/registrymsg"
)

// C3 test kit: a stateful fake GitLab, an environment with one provider and
// one gitlab token, and builders of signed args for the six ops.

const c3Canary = "canary-NOT-A-REAL-TOKEN-C3C3C3C3C3C3C3C3C3C3"

type glReq struct {
	Method, URI, Body string
}

type c3GL struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []glReq
	fail    map[string]int // "METHOD escaped-path-prefix" -> status
	groups  map[string]int64
	existP  map[string]bool // project full paths that exist from the start
	created string          // full path of the project created by POST /projects
	defBr   string
	protect string // "" (rule absent until POSTed), "weak", "strong"
	posted  bool
	delay   map[string]time.Duration // "METHOD path-prefix" -> sleep before answering
	listAll []string                 // raw group list pages override: page body by index (1-based)
	onReq   func(r glReq)
	files   int
}

func newC3GL(t *testing.T) *c3GL {
	t.Helper()
	f := &c3GL{fail: map[string]int{}, groups: map[string]int64{"team": 7, "team/sub": 9}, existP: map[string]bool{}, defBr: "main", delay: map[string]time.Duration{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *c3GL) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rq := glReq{r.Method, r.RequestURI, string(b)}
	f.mu.Lock()
	f.reqs = append(f.reqs, rq)
	hook := f.onReq
	f.mu.Unlock()
	if hook != nil {
		hook(rq)
	}
	key := r.Method + " " + r.URL.EscapedPath()
	for k, d := range f.delay {
		if strings.HasPrefix(key, k) {
			time.Sleep(d)
		}
	}
	for k, st := range f.fail {
		if strings.HasPrefix(key, k) {
			w.WriteHeader(st)
			fmt.Fprint(w, `{"message":"`+c3Canary+`-REFLECT"}`)
			return
		}
	}
	p := r.URL.EscapedPath()
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && p == "/api/v4/groups":
		page := r.URL.Query().Get("page")
		if f.listAll != nil {
			var n int
			fmt.Sscanf(page, "%d", &n)
			if n >= 1 && n <= len(f.listAll) {
				fmt.Fprint(w, f.listAll[n-1])
				return
			}
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `[{"id":7,"name":"team","path":"team","full_path":"team"},{"id":9,"name":"sub","path":"sub","full_path":"team/sub"}]`)
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v4/groups/"):
		full := strings.ReplaceAll(strings.TrimPrefix(p, "/api/v4/groups/"), "%2F", "/")
		id, ok := f.groups[full]
		if !ok {
			w.WriteHeader(404)
			return
		}
		seg := full[strings.LastIndex(full, "/")+1:]
		fmt.Fprintf(w, `{"id":%d,"name":%q,"path":%q,"full_path":%q}`, id, seg, seg, full)
	case r.Method == "POST" && p == "/api/v4/groups":
		var body struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			ParentID int64  `json:"parent_id"`
		}
		json.Unmarshal(b, &body)
		full := body.Path
		for gp, id := range f.groups {
			if id == body.ParentID && body.ParentID != 0 {
				full = gp + "/" + body.Path
			}
		}
		f.groups[full] = 30 + int64(len(f.groups))
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":%d,"name":%q,"path":%q,"full_path":%q}`, f.groups[full], body.Name, body.Path, full)
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v4/projects/") && strings.Count(p, "/") == 4:
		full := strings.ReplaceAll(strings.TrimPrefix(p, "/api/v4/projects/"), "%2F", "/")
		if f.existP[full] || (f.created != "" && f.created == full) {
			fmt.Fprintf(w, `{"id":21,"path_with_namespace":%q,"default_branch":%q}`, full, f.defBr)
			return
		}
		w.WriteHeader(404)
	case r.Method == "POST" && p == "/api/v4/projects":
		var body struct {
			Path        string `json:"path"`
			NamespaceID int64  `json:"namespace_id"`
		}
		json.Unmarshal(b, &body)
		gp := ""
		for g, id := range f.groups {
			if id == body.NamespaceID {
				gp = g
			}
		}
		f.created = gp + "/" + body.Path
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":21,"path_with_namespace":%q}`, f.created)
	case r.Method == "POST" && strings.Contains(p, "/repository/files/"):
		f.files++
		w.WriteHeader(201)
		fp := strings.ReplaceAll(p[strings.Index(p, "/repository/files/")+len("/repository/files/"):], "%2F", "/")
		fmt.Fprintf(w, `{"file_path":%q,"branch":"main"}`, fp)
	case r.Method == "GET" && strings.Contains(p, "/protected_branches/"):
		switch {
		case f.protect == "" && !f.posted:
			w.WriteHeader(404)
		case f.protect == "weak":
			fmt.Fprint(w, `{"name":"main","allow_force_push":true,"push_access_levels":[{"access_level":30}],"merge_access_levels":[{"access_level":40}]}`)
		default:
			fmt.Fprint(w, `{"name":"main","allow_force_push":false,"push_access_levels":[{"access_level":40}],"merge_access_levels":[{"access_level":40}]}`)
		}
	case r.Method == "POST" && strings.HasSuffix(p, "/protected_branches"):
		f.posted = true
		w.WriteHeader(201)
		fmt.Fprint(w, `{"name":"main"}`)
	case r.Method == "POST" && strings.HasSuffix(p, "/deploy_keys"):
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":5}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *c3GL) all() []glReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]glReq(nil), f.reqs...)
}

func (f *c3GL) hits() int { return len(f.all()) }

func (f *c3GL) writes() []glReq {
	var out []glReq
	for _, r := range f.all() {
		if r.Method != "GET" {
			out = append(out, r)
		}
	}
	return out
}

// shape returns "METHOD kind" lines, ids and file names reduced.
func (f *c3GL) trace() []string {
	var out []string
	for _, r := range f.all() {
		u := r.URI
		if i := strings.Index(u, "?"); i >= 0 {
			u = u[:i]
		}
		out = append(out, r.Method+" "+strings.TrimPrefix(u, "/api/v4"))
	}
	return out
}

type c3env struct {
	*wenv
	gl *c3GL
}

const c3Name = "demo"

func c3Key() string { return goodKey }

func c3Hash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// newC3 starts a broker whose provider "gl" points at the fake GitLab (the
// loopback policy is a test seam) with a stored gitlab token "gl-admin".
func newC3(t *testing.T) *c3env {
	t.Helper()
	return newC3Mod(t, nil)
}

func newC3Mod(t *testing.T, mod func(*Server)) *c3env {
	t.Helper()
	f := newC3GL(t)
	w := newWEnvMod(t, func(s *Server) {
		s.apiSeams = &apiSeams{policy: allowLoopback}
		if mod != nil {
			mod(s)
		}
	})
	if err := w.s.Tokens.Set("gl-admin", "gitlab", "", c3Canary, tokNow); err != nil {
		t.Fatal(err)
	}
	if err := w.s.AddProviderChecked(Provider{ID: "gl", Kind: "gitlab", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}); err != nil {
		t.Fatal(err)
	}
	return &c3env{wenv: w, gl: f}
}

func (c *c3env) providerFields(url string) map[string]any {
	return map[string]any{"kind": "gitlab", "base_url": url, "ssh_url": "ssh://git@10.0.0.1", "token_ref": "gl-admin"}
}

func (c *c3env) setProviderArgs(id, url string) map[string]any {
	return c.signedArgs(c.alice, registrymsg.ActionSetProvider, id, c.providerFields(url))
}

func (c *c3env) deleteProviderArgs(id string) map[string]any {
	return c.signedArgs(c.alice, registrymsg.ActionDeleteProvider, id, map[string]any{})
}

func (c *c3env) groupArgs(name, path, parent string) map[string]any {
	return c.signedArgs(c.alice, registrymsg.ActionCreateGroup, "gl", map[string]any{"name": name, "path": path, "parent_path": parent})
}

func (c *c3env) projFields(name string) map[string]any {
	return map[string]any{"provider": "gl", "group_path": "team", "template": "basic", "description": "demo project",
		"ai_context": false, "publish": "write", "deploy_key_sha256": c3Hash(c3Key())}
}

// projectArgs is fully valid create_project args for name (deploy key attached).
func (c *c3env) projectArgs(name string, mod func(fields map[string]any)) map[string]any {
	f := c.projFields(name)
	if mod != nil {
		mod(f)
	}
	a := c.signedArgs(c.alice, registrymsg.ActionCreateProject, name, f)
	a["deploy_key"] = c3Key()
	return a
}

func (c *c3env) orphans() []Orphan { return c.s.Registry.Snapshot().Orphans }

func (c *c3env) noGLHits(what string) {
	c.t.Helper()
	if n := c.gl.hits(); n != 0 {
		c.t.Fatalf("%s: %d request(s) reached GitLab: %v", what, n, c.gl.trace())
	}
}

func readFileOrEmpty(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func repoRootEntries(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, filepath.Join(root, e.Name()))
	}
	return out
}
