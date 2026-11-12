package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigate/internal/registrymsg"
)

// E3 test kit: the C3 kit (fake GitLab, provider "gl") plus a fake GitHub
// (provider "gh", token "gh-tok") reached through RepoProviderFor like in
// production. newGitHubAPI puts the Enterprise prefix /api/v3 on every host but
// api.github.com, so the fake is fronted by a server that strips it.

type e3env struct {
	*c3env
	gh *fakeGH
}

func newE3(t *testing.T) *e3env {
	t.Helper()
	c := newC3(t)
	gh := newFakeGH(t)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const pre = "/api/v3"
		r.URL.Path = strings.TrimPrefix(r.URL.Path, pre)
		r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, pre)
		r.RequestURI = strings.TrimPrefix(r.RequestURI, pre)
		gh.serve(w, r)
	}))
	t.Cleanup(front.Close)
	if err := c.s.Tokens.Set("gh-tok", "github", "", ghCanary, tokNow); err != nil {
		t.Fatal(err)
	}
	if err := c.s.AddProviderChecked(Provider{ID: "gh", Kind: "github", BaseURL: front.URL, SSHURL: "ssh://git@github.com", TokenRef: "gh-tok"}); err != nil {
		t.Fatal(err)
	}
	return &e3env{c3env: c, gh: gh}
}

// trace is "METHOD path" of every request the fake GitHub saw (query dropped).
func (e *e3env) trace() []string {
	var out []string
	for _, r := range e.gh.recorded() {
		m, uri, _ := strings.Cut(r, " ")
		if i := strings.Index(uri, "?"); i >= 0 {
			uri = uri[:i]
		}
		out = append(out, m+" "+uri)
	}
	return out
}

func (e *e3env) ghWrites() []string {
	var out []string
	for _, r := range e.trace() {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

func (e *e3env) noGHHits(what string) {
	e.t.Helper()
	if n := e.gh.hits(); n != 0 {
		e.t.Fatalf("%s: %d request(s) reached GitHub: %v", what, n, e.trace())
	}
}

// noDestructive: nothing but GET, POST and PUT ever went to either fake.
func (e *e3env) noDestructive() {
	e.t.Helper()
	for _, r := range e.gh.recorded() {
		if m, _, _ := strings.Cut(r, " "); m != "GET" && m != "POST" && m != "PUT" {
			e.t.Fatalf("destructive request to GitHub: %s", r)
		}
	}
	for _, r := range e.gl.all() {
		if r.Method != "GET" && r.Method != "POST" && r.Method != "PUT" {
			e.t.Fatalf("destructive request to GitLab: %v", r)
		}
	}
}

func (e *e3env) bodyAt(i int) map[string]any {
	e.gh.mu.Lock()
	b := e.gh.bodies[i]
	e.gh.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal([]byte(b), &m)
	return m
}

// indexOf finds the first request line that starts with prefix.
func (e *e3env) indexOf(prefix string) int {
	for i, r := range e.trace() {
		if strings.HasPrefix(r, prefix) {
			return i
		}
	}
	return -1
}

// ghCreate is fully valid create_project args for a github provider.
func (e *e3env) ghCreate(name, owner string, vis any, mod func(f map[string]any)) map[string]any {
	f := e.projFields(name)
	f["provider"], f["group_path"] = "gh", owner
	if vis != nil {
		f["visibility"] = vis
	}
	if mod != nil {
		mod(f)
	}
	a := e.signedArgs(e.alice, registrymsg.ActionCreateProject, name, f)
	a["deploy_key"] = c3Key()
	return a
}

func (e *e3env) linkFields(prov, remote string, ai bool) map[string]any {
	return map[string]any{"provider": prov, "remote_path": remote, "ai_context": ai, "publish": "write", "deploy_key_sha256": c3Hash(c3Key())}
}

func (e *e3env) linkArgs(id, prov, remote string, ai bool, mod func(f map[string]any)) map[string]any {
	f := e.linkFields(prov, remote, ai)
	if mod != nil {
		mod(f)
	}
	a := e.signedArgs(e.alice, registrymsg.ActionLinkProject, id, f)
	a["deploy_key"] = c3Key()
	return a
}

func (e *e3env) attachFields(prov, remote string) map[string]any {
	return map[string]any{"provider": prov, "remote_path": remote, "publish": "write", "deploy_key_sha256": c3Hash(c3Key())}
}

func (e *e3env) attachArgs(id, prov, remote string, mod func(f map[string]any)) map[string]any {
	f := e.attachFields(prov, remote)
	if mod != nil {
		mod(f)
	}
	a := e.signedArgs(e.alice, registrymsg.ActionAttachRemote, id, f)
	a["deploy_key"] = c3Key()
	return a
}

// ownLocal registers a provider-less local project (fixture).
func (e *e3env) ownLocal(id string, ai bool, legacy *RemoteLink) {
	e.t.Helper()
	err := e.s.Registry.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: id, Path: e.root + "/" + id, Kind: "registered", AIContext: ai, Remote: legacy})
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// ownLinked registers a project linked to remote at provider (fixture).
func (e *e3env) ownLinked(id, provider, remote string) {
	e.t.Helper()
	pid := int64(5)
	err := e.s.Registry.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{ID: id, Path: e.root + "/" + id, Kind: "registered", Provider: &provider,
			Remote: &RemoteLink{Path: remote, ID: &pid}})
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

const ghWeakProtection = `{"allow_force_pushes":{"enabled":true},"allow_deletions":{"enabled":false},"enforce_admins":{"enabled":true}}`

// existing makes alice/Existing (id 31) exist at the fake GitHub, and team/exist
// (id 21) at the fake GitLab.
func (e *e3env) existing() {
	e.gh.mu.Lock()
	e.gh.repos["alice/existing"] = "alice/Existing"
	e.gh.mu.Unlock()
	e.gl.existP["team/exist"] = true
}
