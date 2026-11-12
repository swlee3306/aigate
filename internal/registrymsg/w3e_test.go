package registrymsg

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval/signing"
)

func githubProviderFields() map[string]any {
	return map[string]any{"kind": "github", "base_url": "https://api.github.com", "ssh_url": "ssh://git@github.com", "token_ref": "github-main"}
}

func linkFields() map[string]any {
	return map[string]any{"provider": "gh", "remote_path": "octo/Hello-World", "ai_context": false, "publish": "read", "deploy_key_sha256": hex64()}
}

func attachFields() map[string]any {
	return map[string]any{"provider": "gh", "remote_path": "octo/Hello-World", "publish": "write", "deploy_key_sha256": hex64()}
}

func TestGitHubProviderShape(t *testing.T) {
	got, err := CanonicalChange(ActionSetProvider, "gh", githubProviderFields())
	want := `{"action":"set_provider","fields":{"base_url":"https://api.github.com","kind":"github","ssh_url":"ssh://git@github.com","token_ref":"github-main"},"subject":"gh"}`
	if err != nil || string(got) != want {
		t.Fatalf("golden github set_provider: %s %v", got, err)
	}
	// an Enterprise host with ports is the same shape
	f := githubProviderFields()
	f["base_url"], f["ssh_url"] = "https://ghe.example.com:8443", "ssh://git@ghe.example.com:2222"
	if _, err := CanonicalChange(ActionSetProvider, "ghe", f); err != nil {
		t.Errorf("enterprise shape refused: %v", err)
	}
	// the gitlab shape still works
	if _, err := CanonicalChange(ActionSetProvider, "gl", providerFields()); err != nil {
		t.Errorf("gitlab set_provider refused: %v", err)
	}
	// the kind enum is exactly gitlab|github, case sensitive
	for _, bad := range []string{"GitHub", "GITLAB", "bitbucket", "", "github ", "git hub", "gitea"} {
		f := githubProviderFields()
		f["kind"] = bad
		if _, err := CanonicalChange(ActionSetProvider, "gh", f); err == nil {
			t.Errorf("kind %q accepted", bad)
		}
	}
	// the same URL validators apply to github
	for field, bad := range map[string]string{"base_url": "https://api.github.com/", "ssh_url": "ssh://root@github.com"} {
		f := githubProviderFields()
		f[field] = bad
		if _, err := CanonicalChange(ActionSetProvider, "gh", f); err == nil {
			t.Errorf("github %s=%q accepted", field, bad)
		}
	}
	if !ValidProviderKind("gitlab") || !ValidProviderKind("github") || ValidProviderKind("openai") || ValidProviderKind("") {
		t.Error("ValidProviderKind")
	}
}

// The schema layer does not know a provider's kind, so it only defines
// visibility as an optional enum field. Whether it is REQUIRED (github) or
// FORBIDDEN (gitlab) is enforced by the broker op (see broker tests).
func TestVisibilityFieldRules(t *testing.T) {
	for _, v := range []string{"public", "private"} {
		f := projectFields()
		f["visibility"] = v
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err != nil {
			t.Errorf("visibility %q refused: %v", v, err)
		}
	}
	// no default: absent stays absent (the canonical text has no visibility key)
	got, err := CanonicalChange(ActionCreateProject, "my-app", projectFields())
	if err != nil || strings.Contains(string(got), "visibility") {
		t.Fatalf("absent visibility: %s %v", got, err)
	}
	f := projectFields()
	f["visibility"] = "private"
	got, err = CanonicalChange(ActionCreateProject, "my-app", f)
	if err != nil || !strings.Contains(string(got), `"visibility":"private"`) {
		t.Fatalf("golden: %s %v", got, err)
	}
	for _, bad := range []any{"", "Public", "PRIVATE", "internal", "public ", "pub", true, false, nil, int64(1), 1.5, []any{}} {
		f := projectFields()
		f["visibility"] = bad
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
			t.Errorf("visibility %#v accepted", bad)
		}
	}
	// the field belongs to create_project only
	for action, f := range map[string]map[string]any{ActionLinkProject: linkFields(), ActionAttachRemote: attachFields(), ActionSetProvider: githubProviderFields(),
		ActionCreateGroup: groupFields(), ActionRegisterProject: regFields()} {
		f["visibility"] = "private"
		subj := "my-app"
		if action == ActionSetProvider || action == ActionCreateGroup {
			subj = "gh"
		}
		if _, err := CanonicalChange(action, subj, f); err == nil {
			t.Errorf("%s accepted visibility", action)
		}
	}
	// an optional field does not make the others optional
	f = projectFields()
	delete(f, "publish")
	if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
		t.Error("missing publish accepted")
	}
	f = projectFields()
	delete(f, "publish")
	f["visibility"] = "private"
	if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
		t.Error("visibility stood in for a missing field")
	}
}

func TestLinkAndAttachActionsInSchema(t *testing.T) {
	if ActionLinkProject != "link_project" || ActionAttachRemote != "attach_remote" {
		t.Fatal("constants")
	}
	type tc struct {
		action, subject string
		good            func() map[string]any
	}
	for _, c := range []tc{{ActionLinkProject, "my-app", linkFields}, {ActionAttachRemote, "my-app", attachFields}} {
		t.Run(c.action, func(t *testing.T) {
			if _, err := CanonicalChange(c.action, c.subject, c.good()); err != nil {
				t.Fatalf("good change refused: %v", err)
			}
			for name, v := range c.good() {
				f := cloneFields(c.good())
				delete(f, name)
				if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
					t.Errorf("missing %s accepted", name)
				}
				for _, bad := range []any{int64(1), 1.5, nil, []any{}, map[string]any{}} {
					f = cloneFields(c.good())
					f[name] = bad
					if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
						t.Errorf("%s=%T accepted", name, bad)
					}
				}
				if _, isBool := v.(bool); isBool {
					f = cloneFields(c.good())
					f[name] = "true"
					if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
						t.Errorf("string for bool %s accepted", name)
					}
					continue
				}
				f = cloneFields(c.good())
				f[name] = true
				if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
					t.Errorf("bool for string %s accepted", name)
				}
				f = cloneFields(c.good())
				f[name] = ""
				if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
					t.Errorf("empty %s accepted", name)
				}
			}
			f := cloneFields(c.good())
			f["extra"] = "x"
			if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
				t.Error("unknown field accepted")
			}
			// field rules
			for field, bad := range map[string]string{"provider": "GH", "remote_path": "solo", "publish": "admin", "deploy_key_sha256": strings.Repeat("AB", 32)} {
				f := cloneFields(c.good())
				f[field] = bad
				if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
					t.Errorf("%s=%q accepted", field, bad)
				}
			}
		})
	}
	// attach_remote has no ai_context and link_project has no change of it elsewhere
	f := attachFields()
	f["ai_context"] = false
	if _, err := CanonicalChange(ActionAttachRemote, "my-app", f); err == nil {
		t.Error("attach_remote accepted ai_context")
	}
	f = linkFields()
	delete(f, "ai_context")
	if _, err := CanonicalChange(ActionLinkProject, "my-app", f); err == nil {
		t.Error("link_project without ai_context accepted")
	}
	// the two actions do not accept each other's fields
	if _, err := CanonicalChange(ActionLinkProject, "my-app", attachFields()); err == nil {
		t.Error("link_project accepted attach fields")
	}
	if _, err := CanonicalChange(ActionAttachRemote, "my-app", linkFields()); err == nil {
		t.Error("attach_remote accepted link fields")
	}
	// goldens (a browser's JSON.stringify of sorted keys must give the same)
	got, err := CanonicalChange(ActionLinkProject, "my-app", linkFields())
	want := `{"action":"link_project","fields":{"ai_context":false,"deploy_key_sha256":"` + hex64() + `","provider":"gh","publish":"read","remote_path":"octo/Hello-World"},"subject":"my-app"}`
	if err != nil || string(got) != want {
		t.Fatalf("golden link_project: %s %v", got, err)
	}
	got, err = CanonicalChange(ActionAttachRemote, "my-app", attachFields())
	want = `{"action":"attach_remote","fields":{"deploy_key_sha256":"` + hex64() + `","provider":"gh","publish":"write","remote_path":"octo/Hello-World"},"subject":"my-app"}`
	if err != nil || string(got) != want {
		t.Fatalf("golden attach_remote: %s %v", got, err)
	}
	// subjects: link_project names a NEW local project (name rule, 2..40);
	// attach_remote names an EXISTING project id (the old project id rule).
	if _, err := CanonicalChange(ActionLinkProject, "a", linkFields()); err == nil {
		t.Error("link_project accepted a 1-char new project id")
	}
	if _, err := CanonicalChange(ActionAttachRemote, "a", attachFields()); err != nil {
		t.Errorf("attach_remote refused a 1-char existing project id: %v", err)
	}
	for _, s := range []string{"", "A", "1a", "a_b", "a/b", strings.Repeat("a", 41)} {
		if _, err := CanonicalChange(ActionLinkProject, s, linkFields()); err == nil {
			t.Errorf("link_project subject %q accepted", s)
		}
		if _, err := CanonicalChange(ActionAttachRemote, s, attachFields()); err == nil {
			t.Errorf("attach_remote subject %q accepted", s)
		}
	}
}

func TestRemotePathRules(t *testing.T) {
	seg := func(n int) string { return strings.Repeat("a", n) }
	good := []string{
		"octo/Hello-World", "octo/repo.name", "my_org/my_repo", "a/b", "A1/B2", "x-y/z_w.v",
		"group/sub/name", "group/sub/sub2/name", "a/b/c/d/e", "o/" + seg(MaxRemoteSegmentLen), "o/.hidden", "o/-dash", "o/_u", "o/a.git.b", "o/a.gitx",
	}
	for _, s := range good {
		if !ValidRemotePath(s) {
			t.Errorf("%q refused", s)
		}
	}
	bad := map[string]string{
		"empty": "", "single": "solo", "six segments": "a/b/c/d/e/f", "leading slash": "/a/b", "trailing slash": "a/b/", "double slash": "a//b",
		"dot segment": "a/./b", "dotdot segment": "a/../b", "dotdot first": "../b", "dot last": "a/.", "dotdot last": "a/..",
		"percent": "a/b%2f", "percent 2f": "a%2fb/c", "percent plain": "a/b%41", "dot git": "octo/repo.git", "dot git middle seg": "octo/repo.git/x.git", "dot git middle only": "octo/repo.git/x", "dot git first": "x.git/repo", "dot git upper": "octo/Repo.GIT",
		"dot git only": "octo/.git", "backslash": `a\b/c`, "space": "a/b c", "unicode": "a/bé", "cjk": "소유자/저장소", "fullwidth slash": "a／b/c",
		"nul": "a/b\x00", "newline": "a/b\n", "colon": "a:b/c", "at": "a@b/c", "query": "a/b?x=1", "hash": "a/b#f", "tab": "a/b\t",
		"segment too long": "o/" + seg(MaxRemoteSegmentLen+1), "total too long": strings.Repeat(seg(100)+"/", 3) + seg(100), "total 256": seg(100) + "/" + seg(100) + "/" + seg(54), "invalid utf8": "a/b\xff",
		"url": "https://github.com/a/b", "ssh form": "git@github.com:a/b", "star": "a/*", "plus": "a/b+c", "tilde": "a/~b",
	}
	for name, s := range bad {
		if ValidRemotePath(s) {
			t.Errorf("%s: %q accepted", name, s)
		}
		f := linkFields()
		f["remote_path"] = s
		if _, err := CanonicalChange(ActionLinkProject, "my-app", f); err == nil {
			t.Errorf("%s: link_project accepted %q", name, s)
		}
		f = attachFields()
		f["remote_path"] = s
		if _, err := CanonicalChange(ActionAttachRemote, "my-app", f); err == nil {
			t.Errorf("%s: attach_remote accepted %q", name, s)
		}
	}
	for _, s := range good {
		f := linkFields()
		f["remote_path"] = s
		if _, err := CanonicalChange(ActionLinkProject, "my-app", f); err != nil {
			t.Errorf("link_project refused %q: %v", s, err)
		}
	}
	// case is kept (GitHub is case-insensitive, but folding is the broker's call)
	if !ValidRemotePath("Octo/Hello") || !ValidRemotePath("octo/hello") {
		t.Error("case must be accepted")
	}
	// total length cap is the sum, 4 max-length segments + 3 slashes is over it
	if ValidRemotePath(seg(100)+"/"+seg(100)+"/"+seg(100)+"/"+seg(100)) || !ValidRemotePath(seg(100)+"/"+seg(99)) ||
		!ValidRemotePath(seg(100)+"/"+seg(100)+"/"+seg(53)) { // 100+1+100+1+53 = 255
		t.Error("total length boundary")
	}
}

func TestExistingActionsUnchangedByW3e(t *testing.T) {
	// every W3b/W3c canonical text is byte for byte what it was
	cases := []struct {
		action, subject string
		fields          map[string]any
		want            string
	}{
		{ActionSetProvider, "gl", providerFields(), `{"action":"set_provider","fields":{"base_url":"http://100.64.0.10:18082","kind":"gitlab","ssh_url":"ssh://git@100.64.0.10:22","token_ref":"gitlab-admin"},"subject":"gl"}`},
		{ActionCreateGroup, "gl", groupFields(), `{"action":"create_group","fields":{"name":"team","parent_path":"","path":"team"},"subject":"gl"}`},
		{ActionCreateProject, "my-app", projectFields(), `{"action":"create_project","fields":{"ai_context":false,"deploy_key_sha256":"` + hex64() + `","description":"한글 설명 ok","group_path":"team/sub","provider":"gl","publish":"read","template":"basic"},"subject":"my-app"}`},
		{ActionDeleteProvider, "gl", nil, `{"action":"delete_provider","fields":{},"subject":"gl"}`},
		{ActionRegisterProject, "my-proj", regFields(), `{"action":"register_project","fields":{"ai_context":false,"kind":"registered","path":"/home/orch/repos/app"},"subject":"my-proj"}`},
		{ActionSetFlag, "my-proj", map[string]any{"flag": "ai_context", "value": true}, `{"action":"set_flag","fields":{"flag":"ai_context","value":true},"subject":"my-proj"}`},
		{ActionSetToken, "gh-tok", map[string]any{"kind": "github", "expires": "", "value_hash": hex64()}, `{"action":"set_token","fields":{"expires":"","kind":"github","value_hash":"` + hex64() + `"},"subject":"gh-tok"}`},
		{ActionDeleteToken, "gitlab-admin", nil, `{"action":"delete_token","fields":{},"subject":"gitlab-admin"}`},
	}
	for _, c := range cases {
		got, err := CanonicalChange(c.action, c.subject, c.fields)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s %v", c.action, got, err)
		}
	}
	// the new optional field must not leak into other actions' optional-ness:
	// every other action still needs its exact field set
	for action, f := range map[string]map[string]any{ActionSetProvider: providerFields(), ActionCreateGroup: groupFields(), ActionRegisterProject: regFields()} {
		for name := range f {
			g := cloneFields(f)
			delete(g, name)
			subj := "gl"
			if action == ActionRegisterProject {
				subj = "my-proj"
			}
			if _, err := CanonicalChange(action, subj, g); err == nil {
				t.Errorf("%s without %s accepted", action, name)
			}
		}
	}
	// create_project still requires its seven fields
	for name := range projectFields() {
		g := projectFields()
		delete(g, name)
		if _, err := CanonicalChange(ActionCreateProject, "my-app", g); err == nil {
			t.Errorf("create_project without %s accepted", name)
		}
	}
}

func TestSigningDefinitionsDoNotDriftW3e(t *testing.T) {
	if ActionLinkProject != signing.RegistryActionLinkProject || ActionAttachRemote != signing.RegistryActionAttachRemote {
		t.Fatal("W3e action constants differ between registrymsg and signing")
	}
	corpus := []string{"a", "aa", "admin", "my-app", "a" + strings.Repeat("b", 39), "a" + strings.Repeat("b", 40),
		"", "A", "1a", "-a", "a_b", "a b", "a\n", "a.b", "é", "a/b", "../x", "ab-", "x\x00"}
	valid := strings.Repeat("c", 64)
	for _, s := range corpus {
		for _, a := range []string{ActionLinkProject, ActionAttachRemote} {
			m := signing.RegistryMessage{Action: a, Subject: s, Change: valid, Request: strings.Repeat("1", 32),
				Issued: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
			want := ValidName(s) // link_project: a new project name
			if a == ActionAttachRemote {
				want = ValidSubject(s) // attach_remote: an existing project id
			}
			if got := m.Validate() == nil; got != want {
				t.Errorf("%s subject %q: signing accepts=%v registrymsg accepts=%v", a, s, got, want)
			}
		}
	}
	// the new actions are in the schema table and nothing else changed shape
	for _, a := range []string{ActionLinkProject, ActionAttachRemote} {
		if _, ok := schema[a]; !ok {
			t.Errorf("%s missing from the schema", a)
		}
		if !isNewAction(a) {
			t.Errorf("%s must use the W3c field validators", a)
		}
	}
	// the subject rules of the new actions are the existing, separate regexes:
	// swapping the project-id rule must not move link_project, swapping the
	// name rule must not move attach_remote
	old := subjectRE
	subjectRE = regexp.MustCompile(`^zzz$`)
	if validSubject(ActionLinkProject, "qq-ok") != true || validSubject(ActionAttachRemote, "qq-ok") != false {
		t.Error("subject rule coupling of link/attach")
	}
	subjectRE = old
	oldName := nameRE
	nameRE = regexp.MustCompile(`^zzz$`)
	if validSubject(ActionLinkProject, "qq-ok") != false || validSubject(ActionAttachRemote, "qq-ok") != true {
		t.Error("name rule coupling of link/attach")
	}
	nameRE = oldName
}
