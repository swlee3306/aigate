package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"aigate/internal/registrymsg"
)

// --- signing rules shared by the four signed ops (W3a/W3b table) ---

type signedCase struct {
	op   string
	args func(c *c3env, who testSigner) map[string]any
}

func signedCases() []signedCase {
	return []signedCase{
		{"set_provider", func(c *c3env, who testSigner) map[string]any {
			return c.signedArgs(who, registrymsg.ActionSetProvider, "gl2", c.providerFields(c.gl.URL))
		}},
		{"delete_provider", func(c *c3env, who testSigner) map[string]any {
			return c.signedArgs(who, registrymsg.ActionDeleteProvider, "gl", map[string]any{})
		}},
		{"create_group", func(c *c3env, who testSigner) map[string]any {
			return c.signedArgs(who, registrymsg.ActionCreateGroup, "gl", map[string]any{"name": "newg", "path": "newg", "parent_path": ""})
		}},
		{"create_project", func(c *c3env, who testSigner) map[string]any {
			a := c.signedArgs(who, registrymsg.ActionCreateProject, c3Name, c.projFields(c3Name))
			a["deploy_key"] = c3Key()
			return a
		}},
	}
}

func TestProviderWritesRequireSignature(t *testing.T) {
	for _, sc := range signedCases() {
		t.Run(sc.op, func(t *testing.T) {
			c := newC3(t)
			before := c.snap()
			// no signature
			a := copyArgs(sc.args(c, c.alice), func(m map[string]any) { delete(m, "signature") })
			c.wantCode(sc.op, a, "not_signed")
			a = copyArgs(sc.args(c, c.alice), func(m map[string]any) { m["signature"] = nil })
			c.wantCode(sc.op, a, "not_signed")
			// signature of an unknown signer
			c.wantCode(sc.op, sc.args(c, newSSHTestSigner(t, "mallory")), "unknown_signer")
			c.mustEqual(before, sc.op)
			c.noGLHits(sc.op)
		})
	}
}

func TestChangeRequiredAndMismatchBurnsNoID(t *testing.T) {
	for _, sc := range signedCases() {
		t.Run(sc.op, func(t *testing.T) {
			c := newC3(t)
			before := c.snap()
			c.wantCode(sc.op, copyArgs(sc.args(c, c.alice), func(m map[string]any) { m["change"] = "" }), "change_mismatch")
			c.wantCode(sc.op, copyArgs(sc.args(c, c.alice), func(m map[string]any) { delete(m, "change") }), "change_mismatch")
			// a valid signature over another change: bad_signature (message is rebuilt from the claimed change)
			// or change_mismatch when the signed change differs from the request fields
			other := sc.args(c, c.alice)
			switch sc.op {
			case "set_provider":
				other["base_url"] = "http://10.9.9.9:81"
			case "create_group":
				other["name"] = "other"
			case "create_project":
				other["description"] = "different text"
			case "delete_provider":
				other["id"] = "gl-other"
			}
			wantCode := "change_mismatch"
			if sc.op == "delete_provider" {
				wantCode = "bad_signature" // the subject is part of the signed message itself
			}
			c.wantCode(sc.op, other, wantCode)
			c.mustEqual(before, sc.op)
			c.noGLHits(sc.op)
		})
	}
}

func TestRequestIDOnceAndBurnedAfterValidSignature(t *testing.T) {
	c := newC3(t)
	// success burns the ID
	a := c.groupArgs("newg", "newg", "")
	if _, e := c.do("create_group", a); e != nil {
		t.Fatalf("create_group: %v", e.Code)
	}
	c.wantCode("create_group", a, "request_reused")
	// refusal after a valid signature burns it too: unknown token ref
	args := c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl3", map[string]any{"kind": "gitlab", "base_url": c.gl.URL, "ssh_url": "ssh://git@10.0.0.1", "token_ref": "nope"})
	c.wantCode("set_provider", args, "token_ref_unknown")
	c.wantCode("set_provider", args, "request_reused")
	// delete_provider of an unknown provider also burns
	d := c.signedArgs(c.alice, registrymsg.ActionDeleteProvider, "ghost", map[string]any{})
	c.wantCode("delete_provider", d, "provider_unknown")
	c.wantCode("delete_provider", d, "request_reused")
}

func TestShapeErrorsBurnNoID(t *testing.T) {
	c := newC3(t)
	before := c.snap()
	cases := []struct {
		op   string
		args map[string]any
		want string
	}{
		{"create_project", copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { m["template"] = "rust" }), "template_unknown"},
		{"create_project", copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { m["namespace_id"] = 99 }), "bad_request"},
		{"create_project", copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { m["deploy_key"] = "ssh-rsa x" }), "bad_request"},
		{"create_project", copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { delete(m, "deploy_key") }), "bad_request"},
		{"create_project", copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { delete(m, "ai_context") }), "bad_request"},
		{"create_project", copyArgs(c.projectArgs("bad-name", nil), func(m map[string]any) { m["id"] = "Bad_Name" }), "bad_request"},
		{"create_group", copyArgs(c.groupArgs("ok-a", "ok-a", ""), func(m map[string]any) { m["name"] = "A" }), "bad_request"},
		{"set_provider", copyArgs(c.setProviderArgs("gl2", c.gl.URL), func(m map[string]any) { m["base_url"] = "http://169.254.169.254" }), "bad_url"},
		{"set_provider", copyArgs(c.setProviderArgs("gl2", c.gl.URL), func(m map[string]any) { m["base_url"] = "http://localhost:8080" }), "bad_url"},
		{"set_provider", copyArgs(c.setProviderArgs("gl2", c.gl.URL), func(m map[string]any) { m["base_url"] = "ftp://x" }), "bad_url"},
		{"delete_provider", copyArgs(c.deleteProviderArgs("gl"), func(m map[string]any) { m["extra"] = 1 }), "bad_request"},
		{"set_provider", copyArgs(c.setProviderArgs("gl2", c.gl.URL), func(m map[string]any) { m["Signature"] = 1 }), "bad_request"},
	}
	for i, tc := range cases {
		c.wantCode(tc.op, tc.args, tc.want)
		c.mustEqual(before, fmt.Sprintf("case %d %s", i, tc.op))
	}
	c.noGLHits("shape errors")
	// production rules (no test seams): http only for private literals; the
	// loopback fake itself is refused; nothing burnt
	s2 := c.restart(nil)
	for _, u := range []string{"http://example.com", c.gl.URL, "http://8.8.8.8"} {
		raw, _ := json.Marshal(copyArgs(c.setProviderArgs("gl2", c.gl.URL), func(m map[string]any) { m["base_url"] = u }))
		if _, e := s2.Handlers["set_provider"](context.Background(), raw); codeOf(e) != "bad_url" {
			t.Fatalf("%s: %v", u, e)
		}
	}
	c.mustEqual(before, "production url rules")
}

func TestSetProviderHostNameMustResolveToAllowedAddress(t *testing.T) {
	var answer atomic.Value
	answer.Store([]netip.Addr{netip.MustParseAddr("169.254.169.254")})
	var fail atomic.Bool
	c := newC3Mod(t, func(s *Server) {
		s.apiSeams.lookup = func(context.Context, string) ([]netip.Addr, error) {
			if fail.Load() {
				return nil, errors.New("no dns")
			}
			return answer.Load().([]netip.Addr), nil
		}
	})
	f := func(url string) map[string]any {
		fl := c.providerFields(url)
		return c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl5", fl)
	}
	c.wantCode("set_provider", f("https://gitlab.corp.example"), "bad_url")
	fail.Store(true)
	c.wantCode("set_provider", f("https://gitlab.corp.example"), "provider_unreachable")
	fail.Store(false)
	answer.Store([]netip.Addr{netip.MustParseAddr("10.0.0.7")})
	if _, e := c.do("set_provider", f("https://gitlab.corp.example")); e != nil {
		t.Fatalf("resolving name: %v", e.Code)
	}
}

// --- set_provider / delete_provider ---

func TestSetProviderRules(t *testing.T) {
	c := newC3(t)
	// add a second provider (same loopback fake, other id)
	if _, e := c.do("set_provider", c.setProviderArgs("gl2", c.gl.URL)); e != nil {
		t.Fatalf("add: %v", e.Code)
	}
	var got *Provider
	for _, p := range c.s.Registry.Snapshot().Providers {
		if p.ID == "gl2" {
			p := p
			got = &p
		}
	}
	if got == nil || got.TokenRef != "gl-admin" || got.BaseURL != c.gl.URL {
		t.Fatalf("stored provider: %+v", got)
	}
	// (c) token_ref must exist and be of kind gitlab
	if err := c.s.Tokens.Set("oa", "openai", "", c3Canary+"x", tokNow); err == nil {
		t.Fatal("openai under another name must be refused by the store")
	}
	if err := c.s.Tokens.Set("gh", "github", "", c3Canary+"gh", tokNow); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"nope", "gh", "openai"} {
		f := c.providerFields(c.gl.URL)
		f["token_ref"] = ref
		c.wantCode("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl4", f), "token_ref_unknown")
	}
	// re-setting the same id is a signed change; a changed address with the
	// SAME token_ref lands in the signed fields: signing the old address and
	// sending the new one is a change_mismatch
	a := c.setProviderArgs("gl2", c.gl.URL)
	a["base_url"] = "http://10.1.1.1:81"
	c.wantCode("set_provider", a, "change_mismatch")
	// same id, same URLs, different token: fine
	if err := c.s.Tokens.Set("gl-b", "gitlab", "", c3Canary+"b", tokNow); err != nil {
		t.Fatal(err)
	}
	f := c.providerFields(c.gl.URL)
	f["token_ref"] = "gl-b"
	if _, e := c.do("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl2", f)); e != nil {
		t.Fatalf("replace token: %v", e.Code)
	}
	if p, _ := c.s.Registry.ProviderByID("gl2"); p.TokenRef != "gl-b" {
		t.Fatalf("token_ref not replaced: %+v", p)
	}
	// replacing the address with no project referencing it is fine
	if _, e := c.do("set_provider", c.setProviderArgs("gl2", "http://10.1.1.1:81")); e != nil {
		t.Fatalf("replace url (unused): %v", e.Code)
	}
	// (d) a provider with projects keeps its addresses
	c.ownProject("gl", "p-one")
	c.wantCode("set_provider", c.setProviderArgs("gl", "http://10.1.1.1:82"), "provider_in_use")
	f2 := c.providerFields(c.gl.URL)
	f2["ssh_url"] = "ssh://git@10.0.0.9"
	c.wantCode("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl", f2), "provider_in_use")
	if p, _ := c.s.Registry.ProviderByID("gl"); p.BaseURL != c.gl.URL || p.SSHURL != "ssh://git@10.0.0.1" {
		t.Fatalf("provider in use changed: %+v", p)
	}
	// ... but a same-id replace without URL change (token swap) is fine
	if _, e := c.do("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl", f)); e != nil {
		t.Fatalf("same URLs while in use: %v", e.Code)
	}
}

// ownProject writes a registry entry referencing provider (test fixture).
func (c *c3env) ownProject(provider, id string) {
	c.t.Helper()
	pid := int64(5)
	err := c.s.Registry.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{Origin: OriginUnknown, ID: id, Path: c.root + "/" + id, Kind: "registered", Provider: &provider,
			Remote: &RemoteLink{Path: "team/" + id, ID: &pid}})
		return nil
	})
	if err != nil {
		c.t.Fatal(err)
	}
}

func TestDeleteProviderInUse(t *testing.T) {
	c := newC3(t)
	c.ownProject("gl", "p-one")
	before := c.regFile()
	c.wantCode("delete_provider", c.deleteProviderArgs("gl"), "provider_in_use")
	if !bytes.Equal(before, c.regFile()) {
		t.Fatal("registry changed")
	}
	// free provider: deleted
	if _, e := c.do("set_provider", c.setProviderArgs("gl9", c.gl.URL)); e != nil {
		t.Fatal(e.Code)
	}
	if _, e := c.do("delete_provider", c.deleteProviderArgs("gl9")); e != nil {
		t.Fatalf("delete: %v", e.Code)
	}
	if _, ok := c.s.Registry.ProviderByID("gl9"); ok {
		t.Fatal("not deleted")
	}
	// orphan records do not block deletion
	c.s.Registry.AddOrphan(Orphan{Provider: "gl", Project: "team/x", Step: "project", At: "2026-11-09T12:00:00Z"})
	if _, e := c.do("set_provider", c.setProviderArgs("gl8", c.gl.URL)); e != nil {
		t.Fatal(e.Code)
	}
	c.s.Registry.AddOrphan(Orphan{Provider: "gl8", Project: "team/x", Step: "project", At: "2026-11-09T12:00:00Z"})
	if _, e := c.do("delete_provider", c.deleteProviderArgs("gl8")); e != nil {
		t.Fatalf("orphan blocked delete: %v", e.Code)
	}
}

// --- providers (read) ---

func TestProvidersReadSecretFree(t *testing.T) {
	c := newC3(t)
	r, raw := call(t, c.sock, `{"v":1,"op":"providers"}`)
	if !r.OK {
		t.Fatalf("providers: %s", raw)
	}
	var res struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Providers) != 1 {
		t.Fatalf("result: %s %v", r.Result, err)
	}
	p := res.Providers[0]
	if p["id"] != "gl" || p["token_ref"] != "gl-admin" || p["in_use"] != false {
		t.Fatalf("row: %v", p)
	}
	if strings.Contains(string(raw), c3Canary) {
		t.Fatal("token value in reply")
	}
	c.ownProject("gl", "p-one")
	r, _ = call(t, c.sock, `{"v":1,"op":"providers"}`)
	if !strings.Contains(string(r.Result), `"in_use":true`) {
		t.Fatalf("in_use: %s", r.Result)
	}
	c.noGLHits("providers is registry-only")
}

// --- list_groups ---

func TestListGroupsReadOnlyNoSecrets(t *testing.T) {
	c := newC3(t)
	before := c.snap()
	r, raw := call(t, c.sock, `{"v":1,"op":"list_groups","args":{"provider":"gl"}}`)
	if !r.OK {
		t.Fatalf("list_groups: %s", raw)
	}
	var res struct {
		Groups    []map[string]any `json:"groups"`
		Truncated bool             `json:"truncated"`
	}
	json.Unmarshal(r.Result, &res)
	if len(res.Groups) != 2 || res.Groups[0]["path"] != "team" || res.Truncated {
		t.Fatalf("groups: %s", r.Result)
	}
	for _, g := range res.Groups {
		if len(g) != 2 { // path and name only
			t.Fatalf("group row has extra fields: %v", g)
		}
	}
	if strings.Contains(string(raw), c3Canary) {
		t.Fatal("token in reply")
	}
	for _, rq := range c.gl.all() {
		if rq.Method != "GET" {
			t.Fatalf("list_groups wrote: %v", rq)
		}
	}
	c.mustEqual(before, "list_groups")
	// unknown provider, bad shape
	r, _ = call(t, c.sock, `{"v":1,"op":"list_groups","args":{"provider":"ghost"}}`)
	if r.Code != "provider_unknown" {
		t.Fatalf("unknown: %+v", r)
	}
	for _, bad := range []string{`{}`, `{"provider":"A"}`, `{"provider":"gl","x":1}`, `{"provider":1}`} {
		r, _ = call(t, c.sock, `{"v":1,"op":"list_groups","args":`+bad+`}`)
		if r.Code != "bad_request" {
			t.Fatalf("args %s: %+v", bad, r)
		}
	}
	// GitLab trouble: fixed codes, no GitLab text
	c.gl.fail["GET /api/v4/groups"] = 500
	r, raw = call(t, c.sock, `{"v":1,"op":"list_groups","args":{"provider":"gl"}}`)
	if r.Code != "provider_unreachable" || strings.Contains(string(raw), "REFLECT") {
		t.Fatalf("500: %s", raw)
	}
}

func TestListGroupsTruncatedFlag(t *testing.T) {
	// (e) skipped names (a group whose path does not fit our rules) set the flag
	c := newC3(t)
	c.gl.listAll = []string{`[{"id":7,"name":"team","path":"team","full_path":"team"},{"id":8,"name":"Bad","path":"Bad","full_path":"Bad"}]`}
	r, _ := call(t, c.sock, `{"v":1,"op":"list_groups","args":{"provider":"gl"}}`)
	if !r.OK || !strings.Contains(string(r.Result), `"truncated":true`) {
		t.Fatalf("skipped name: %s", r.Result)
	}
	// the 500 cap (5 full pages) sets it too
	page := func(n int) string {
		var items []string
		for i := 0; i < 100; i++ {
			p := fmt.Sprintf("g-%d-%d", n, i)
			items = append(items, fmt.Sprintf(`{"id":%d,"name":%q,"path":%q,"full_path":%q}`, n*1000+i+1, p, p, p))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	c.gl.listAll = []string{page(1), page(2), page(3), page(4), page(5)}
	r, _ = call(t, c.sock, `{"v":1,"op":"list_groups","args":{"provider":"gl"}}`)
	if !r.OK || !strings.Contains(string(r.Result), `"truncated":true`) {
		t.Fatalf("cap: %.200s", r.Result)
	}
	// exactly 4 full pages and an empty fifth: not truncated
	c.gl.listAll = []string{page(1), page(2), page(3), page(4), `[]`}
	r, _ = call(t, c.sock, `{"v":1,"op":"list_groups","args":{"provider":"gl"}}`)
	if !r.OK || strings.Contains(string(r.Result), `"truncated":true`) {
		t.Fatalf("4 pages: %.200s", r.Result)
	}
}

// --- create_group ---

func TestCreateGroupFlow(t *testing.T) {
	c := newC3(t)
	res, e := c.do("create_group", c.groupArgs("newg", "newg", ""))
	if e != nil {
		t.Fatalf("top level: %v", e.Code)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"path":"newg"`) || strings.Contains(string(b), c3Canary) {
		t.Fatalf("result: %s", b)
	}
	tr := strings.Join(c.gl.trace(), "|")
	if tr != "GET /groups/newg|POST /groups" {
		t.Fatalf("requests: %s", tr)
	}
	var body map[string]any
	json.Unmarshal([]byte(c.gl.writes()[0].Body), &body)
	if body["visibility"] != "private" || body["name"] != "newg" || body["path"] != "newg" || body["parent_id"] != nil || len(body) != 3 {
		t.Fatalf("body: %v", body)
	}
	// already there: group_exists, no write
	n := len(c.gl.writes())
	c.wantCode("create_group", c.groupArgs("newg", "newg", ""), "group_exists")
	if len(c.gl.writes()) != n {
		t.Fatal("write after group_exists")
	}
	// nested: parent path
	if _, e := c.do("create_group", c.groupArgs("kid", "kid", "team")); e != nil {
		t.Fatalf("nested: %v", e.Code)
	}
	last := c.gl.writes()[len(c.gl.writes())-1]
	json.Unmarshal([]byte(last.Body), &body)
	if body["parent_id"] != float64(7) || body["visibility"] != "private" {
		t.Fatalf("nested body: %v", body)
	}
	// unknown parent
	c.wantCode("create_group", c.groupArgs("kid2", "kid2", "nowhere"), "group_unknown")
	// unknown provider
	a := c.signedArgs(c.alice, registrymsg.ActionCreateGroup, "ghost", map[string]any{"name": "g2", "path": "g2", "parent_path": ""})
	c.wantCode("create_group", a, "provider_unknown")
	// GitLab refusal is a fixed code
	c.gl.fail["POST /api/v4/groups"] = 403
	c.wantCode("create_group", c.groupArgs("g3", "g3", ""), "gitlab_refused")
	// name rules (single slug, no slash) are shape errors: no ID burnt
	before := c.snap()
	for _, kv := range [][2]string{{"path", "x/y"}, {"path", "../x"}, {"path", "A"}, {"name", "A b"}, {"parent_path", "../team"}} {
		kv := kv
		c.wantCode("create_group", copyArgs(c.groupArgs("ok-name", "ok-path", ""), func(m map[string]any) { m[kv[0]] = kv[1] }), "bad_request")
	}
	c.mustEqual(before, "bad group names")
}

// --- ops allowlist ---

func TestOpAllowlist(t *testing.T) {
	c := newC3(t)
	want := map[string]bool{"status": true, "tokens": true, "refine": true, "create_mr": true, "projects": true, "register_project": true,
		"set_flag": true, "set_token": true, "delete_token": true, "providers": true, "list_groups": true, "set_provider": true,
		"delete_provider": true, "create_group": true, "create_project": true, "link_project": true, "attach_remote": true,
		"remove_project": true, "delete_remote_repo": true, "remove_deploy_key": true, "clear_orphan": true}
	for op := range c.s.Handlers {
		if !want[op] {
			t.Fatalf("unexpected op registered: %s", op)
		}
	}
	for op := range want {
		if c.s.Handlers[op] == nil {
			t.Fatalf("op missing: %s", op)
		}
	}
	for _, op := range []string{"delete_project", "delete_group", "update_project", "merge", "set_project", "clone", "set_groups", "create_projects", "providers_delete", "DELETE", "set_"} {
		r, _ := call(t, c.sock, `{"v":1,"op":"`+op+`","args":{}}`)
		if r.Code != "unknown_op" {
			t.Fatalf("%s: %+v", op, r)
		}
	}
}

// checkC3FixedMessages is run by TestFixedMessagesRegisteredForNewCodes (projects_test.go).
func checkC3FixedMessages(t *testing.T) {
	codes := []string{"provider_unknown", "provider_exists", "provider_in_use", "bad_url", "token_ref_unknown", "provider_unreachable",
		"gitlab_refused", "group_unknown", "group_exists", "project_name_taken", "create_partial", "deploy_key_mismatch", "template_unknown"}
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
		if !hangul || strings.ContainsAny(m, "\n\r{}") || strings.Contains(m, "%") {
			t.Fatalf("%s: sentence is not a plain Korean sentence: %q", code, m)
		}
		if prev, dup := seen[m]; dup {
			t.Fatalf("%s and %s share a sentence", code, prev)
		}
		seen[m] = code
	}
	// (h) bad_url no longer carries the W2b sentence about GitLab answering outside its address
	if strings.Contains(fixedMessages["bad_url"], "돌려주었습니다") {
		t.Fatalf("bad_url still carries the W2b-only sentence: %q", fixedMessages["bad_url"])
	}
	// but it still serves create_mr's case (a returned URL outside the configured one)
	if !strings.Contains(fixedMessages["bad_url"], "주소") {
		t.Fatalf("bad_url: %q", fixedMessages["bad_url"])
	}
}

// wire replies carry the fixed sentence of the code for every new failure.
func TestNewCodesUseFixedSentenceOnWire(t *testing.T) {
	c := newC3(t)
	r := c.wire("create_group", copyArgs(c.groupArgs("newg", "newg", ""), func(m map[string]any) { delete(m, "signature") }))
	if r.Code != "not_signed" || r.Message != fixedMessages["not_signed"] {
		t.Fatalf("%+v", r)
	}
	c.gl.fail["GET /api/v4/groups/"] = 500
	r = c.wire("create_group", c.groupArgs("newg", "newg", ""))
	if r.Code != "provider_unreachable" || r.Message != fixedMessages["provider_unreachable"] || strings.Contains(r.Message, "REFLECT") {
		t.Fatalf("%+v", r)
	}
}
