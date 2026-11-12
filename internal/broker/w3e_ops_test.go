package broker

import (
	"strings"
	"testing"

	"aigate/internal/registrymsg"
)

// newE1 is the C3 kit plus a github token and a github provider "gh" whose
// address is the same loopback fake (nothing is ever sent to it in these tests).
func newE1(t *testing.T) *c3env {
	t.Helper()
	c := newC3(t)
	if err := c.s.Tokens.Set("gh-tok", "github", "", c3Canary+"gh", tokNow); err != nil {
		t.Fatal(err)
	}
	if err := c.s.AddProviderChecked(Provider{ID: "gh", Kind: "github", BaseURL: c.gl.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gh-tok"}); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *c3env) ghProjectArgs(name string, vis any, mod func(f map[string]any)) map[string]any {
	f := c.projFields(name)
	f["provider"] = "gh"
	if vis != nil {
		f["visibility"] = vis
	}
	if mod != nil {
		mod(f)
	}
	a := c.signedArgs(c.alice, registrymsg.ActionCreateProject, name, f)
	a["deploy_key"] = c3Key()
	return a
}

func TestSetProviderAcceptsGitHubKindWithMatchingToken(t *testing.T) {
	c := newE1(t)
	gh := func(id, ref string) map[string]any {
		f := c.providerFields(c.gl.URL)
		f["kind"], f["token_ref"] = "github", ref
		return c.signedArgs(c.alice, registrymsg.ActionSetProvider, id, f)
	}
	if _, e := c.do("set_provider", gh("gh2", "gh-tok")); e != nil {
		t.Fatalf("github provider with a github token: %v", e.Code)
	}
	if p, ok := c.s.Registry.ProviderByID("gh2"); !ok || p.Kind != "github" || p.TokenRef != "gh-tok" {
		t.Fatalf("stored %+v", p)
	}
	// the token must be of the PROVIDER's kind in both directions
	c.wantCode("set_provider", gh("gh3", "gl-admin"), "token_ref_unknown")
	f := c.providerFields(c.gl.URL)
	f["token_ref"] = "gh-tok" // kind stays gitlab
	c.wantCode("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl3", f), "token_ref_unknown")
	if _, ok := c.s.Registry.ProviderByID("gh3"); ok {
		t.Fatal("provider stored despite the wrong token kind")
	}
	// the kind of an existing id may be replaced only through the same rules (token kind checked again)
	if _, e := c.do("set_provider", gh("gh2", "gh-tok")); e != nil {
		t.Fatalf("same github replace: %v", e.Code)
	}
}

// The schema does not know the provider kind; the op does. Presence and
// absence are enforced here, before the signature is consumed.
func TestVisibilityRequiredForGitHubForbiddenForGitLabAtTheOp(t *testing.T) {
	c := newE1(t)
	before := c.snap()
	// gitlab + visibility: forbidden, no ID burnt, nothing sent
	c.wantCode("create_project", c.projectArgs(c3Name, func(f map[string]any) { f["visibility"] = "private" }), "visibility_not_allowed")
	c.wantCode("create_project", c.projectArgs(c3Name, func(f map[string]any) { f["visibility"] = "public" }), "visibility_not_allowed")
	// github without visibility: required
	c.wantCode("create_project", c.ghProjectArgs(c3Name, nil, nil), "visibility_required")
	// a bad value is a malformed request
	for _, bad := range []string{"", "Public", "internal"} {
		c.wantCode("create_project", c.ghProjectArgsRaw(c3Name, bad), "bad_request")
	}
	c.mustEqual(before, "refusals before the signature")
	c.noGLHits("visibility rules")
	// the same signed request is not burnt by those refusals: after the
	// refusal of a github request without visibility, the SAME request id is fresh
	a := c.projectArgs(c3Name, func(f map[string]any) { f["visibility"] = "private" })
	c.wantCode("create_project", a, "visibility_not_allowed")
	c.wantCode("create_project", a, "visibility_not_allowed") // not request_reused: no ID burnt
	// a gitlab request without visibility is the unchanged W3c flow
	if _, e := c.do("create_project", c.projectArgs(c3Name, nil)); e != nil {
		t.Fatalf("W3c create_project changed: %v", e.Code)
	}
	// a signed visibility that differs from the one sent is a change mismatch
	a = c.ghProjectArgs("other", "private", nil)
	a["visibility"] = "public"
	if got := codeOf(func() *OpError { _, e := c.do("create_project", a); return e }()); got != "not_supported_for_provider" && got != "change_mismatch" {
		t.Fatalf("code %q", got)
	}
}

func (c *c3env) ghProjectArgsRaw(name, vis string) map[string]any {
	a := c.ghProjectArgs(name, "private", nil)
	a["visibility"] = vis
	return a
}

// Until E2 lands the GitHub client, a GitHub provider can be stored and shown
// but every call that would reach it answers one fixed code and sends nothing.
func TestGitHubProviderIsNotReachedBeforeE2(t *testing.T) {
	c := newE1(t)
	nProjects := len(c.s.Registry.Snapshot().Projects)
	c.wantCode("create_project", c.ghProjectArgs(c3Name, "private", nil), "not_supported_for_provider")
	c.wantCode("create_project", c.ghProjectArgs("other", "public", nil), "not_supported_for_provider")
	c.wantCode("create_group", c.signedArgs(c.alice, registrymsg.ActionCreateGroup, "gh", map[string]any{"name": "newg", "path": "newg", "parent_path": ""}), "not_supported_for_provider")
	c.wantCode("list_groups", map[string]any{"provider": "gh"}, "not_supported_for_provider")
	// refused before the signature is consumed: the same request answers the same
	// code again (it would be request_reused if an ID had been burnt)
	same := c.ghProjectArgs("again", "private", nil)
	c.wantCode("create_project", same, "not_supported_for_provider")
	c.wantCode("create_project", same, "not_supported_for_provider")
	c.noGLHits("github provider")
	if len(c.s.Registry.Snapshot().Projects) != nProjects || len(c.orphans()) != 0 {
		t.Fatalf("state changed: %+v %+v", c.s.Registry.Snapshot().Projects, c.orphans())
	}
	// GitLab calls are unchanged
	if _, e := c.do("list_groups", map[string]any{"provider": "gl"}); e != nil {
		t.Fatalf("gitlab list_groups: %v", e.Code)
	}
}

func TestE1CodesHaveFixedSentences(t *testing.T) {
	seen := map[string]string{}
	for _, code := range []string{"not_supported_for_provider", "visibility_required", "visibility_not_allowed"} {
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
		if prev, dup := seen[m]; dup {
			t.Fatalf("%s and %s share a sentence", code, prev)
		}
		seen[m] = code
		for other, om := range fixedMessages {
			if other != code && om == m {
				t.Fatalf("%s shares a sentence with %s", code, other)
			}
		}
	}
	// the wire reply carries the fixed sentence
	c := newE1(t)
	r := c.wire("create_project", c.ghProjectArgs(c3Name, nil, nil))
	if r.Code != "visibility_required" || r.Message != fixedMessages["visibility_required"] {
		t.Fatalf("%+v", r)
	}
}

// The signed change carries visibility exactly when the request does: the op
// never invents a default and never drops the value the signer saw.
func TestCreateProjectSignedFieldsCarryVisibilityOnlyWhenSent(t *testing.T) {
	yes := true
	vis := "private"
	with := createProjectArgs{Provider: "gh", GroupPath: "octo", Template: "go", AIContext: &yes, Publish: "read", DeployKeySHA256: c3Hash(c3Key()), Visibility: &vis}
	f := with.signedFields()
	if len(f) != 8 || f["visibility"] != "private" {
		t.Fatalf("with visibility: %v", f)
	}
	without := with
	without.Visibility = nil
	f = without.signedFields()
	if _, has := f["visibility"]; has || len(f) != 7 {
		t.Fatalf("without visibility: %v", f)
	}
	if _, err := registrymsg.CanonicalChange(registrymsg.ActionCreateProject, "hello", with.signedFields()); err != nil {
		t.Fatal(err)
	}
	h1, _ := registrymsg.ChangeHash(registrymsg.ActionCreateProject, "hello", with.signedFields())
	h2, _ := registrymsg.ChangeHash(registrymsg.ActionCreateProject, "hello", without.signedFields())
	if h1 == h2 {
		t.Fatal("visibility is not part of the signed hash")
	}
}

// A provider that projects use cannot change its kind either (clone and push
// remotes were derived under the old kind).
func TestSetProviderKindChangeBlockedWhileInUse(t *testing.T) {
	c := newE1(t)
	c.ownProject("gl", "p-one")
	f := c.providerFields(c.gl.URL)
	f["kind"], f["token_ref"] = "github", "gh-tok"
	c.wantCode("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gl", f), "provider_in_use")
	if p, _ := c.s.Registry.ProviderByID("gl"); p.Kind != "gitlab" || p.TokenRef != "gl-admin" {
		t.Fatalf("provider changed: %+v", p)
	}
	// unused provider: the kind may change (token of the new kind)
	c.wantCode("set_provider", c.signedArgs(c.alice, registrymsg.ActionSetProvider, "gh", func() map[string]any {
		g := c.providerFields(c.gl.URL)
		g["kind"], g["token_ref"] = "gitlab", "gl-admin"
		return g
	}()), "")
}

// create_group on a github provider is refused in the shape step like
// create_project: no request ID is burnt.
func TestNotSupportedForProviderBurnsNoID(t *testing.T) {
	c := newE1(t)
	g := c.signedArgs(c.alice, registrymsg.ActionCreateGroup, "gh", map[string]any{"name": "newg", "path": "newg", "parent_path": ""})
	c.wantCode("create_group", g, "not_supported_for_provider")
	c.wantCode("create_group", g, "not_supported_for_provider") // request_reused if an ID had been burnt
	p := c.ghProjectArgs(c3Name, "private", nil)
	c.wantCode("create_project", p, "not_supported_for_provider")
	c.wantCode("create_project", p, "not_supported_for_provider")
	c.noGLHits("not supported")
}

func TestTokenRefUnknownSentenceIsProviderNeutral(t *testing.T) {
	if m := fixedMessages["token_ref_unknown"]; strings.Contains(strings.ToLower(m), "gitlab") || strings.Contains(strings.ToLower(m), "github") {
		t.Fatalf("sentence names a provider: %q", m)
	}
}
