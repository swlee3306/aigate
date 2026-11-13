package registrymsg

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval/signing"
)

var regexpNever = regexp.MustCompile(`^zzz$`)

func removeProjectFields() map[string]any {
	return map[string]any{"scope": "local", "legacy_ack": false, "detach_key": false}
}

func deleteRemoteFields() map[string]any {
	return map[string]any{"remote_path": "octo/Hello-World", "remote_id": "123456789", "confirm_name": "Hello-World", "also_local": true}
}

func removeKeyFields() map[string]any {
	return map[string]any{"remote_path": "octo/Hello-World"}
}

func clearOrphanFields() map[string]any {
	return map[string]any{"project": "team/sub/half-made"}
}

func TestDeleteActionsInSchema(t *testing.T) {
	cases := []struct {
		action, subject string
		fields          func() map[string]any
		want            string
	}{
		{ActionRemoveProject, "my-proj", removeProjectFields, `{"action":"remove_project","fields":{"detach_key":false,"legacy_ack":false,"scope":"local"},"subject":"my-proj"}`},
		{ActionDeleteRemoteRepo, "my-proj", deleteRemoteFields, `{"action":"delete_remote_repo","fields":{"also_local":true,"confirm_name":"Hello-World","remote_id":"123456789","remote_path":"octo/Hello-World"},"subject":"my-proj"}`},
		{ActionRemoveDeployKey, "my-proj", removeKeyFields, `{"action":"remove_deploy_key","fields":{"remote_path":"octo/Hello-World"},"subject":"my-proj"}`},
		{ActionClearOrphan, "gl", clearOrphanFields, `{"action":"clear_orphan","fields":{"project":"team/sub/half-made"},"subject":"gl"}`},
	}
	for _, c := range cases {
		got, err := CanonicalChange(c.action, c.subject, c.fields())
		if err != nil || string(got) != c.want {
			t.Errorf("%s golden: %s %v", c.action, got, err)
		}
		if _, ok := schema[c.action]; !ok || !isNewAction(c.action) {
			t.Errorf("%s must be in the schema and use the new-field validators", c.action)
		}
		// exact keys: every missing field, every extra field, every wrong type
		for name, v := range c.fields() {
			g := c.fields()
			delete(g, name)
			if _, err := CanonicalChange(c.action, c.subject, g); err == nil {
				t.Errorf("%s without %s accepted", c.action, name)
			}
			g = c.fields()
			if _, isBool := v.(bool); isBool {
				g[name] = "true"
			} else {
				g[name] = true
			}
			if _, err := CanonicalChange(c.action, c.subject, g); err == nil {
				t.Errorf("%s with a wrongly typed %s accepted", c.action, name)
			}
			if _, isStr := v.(string); isStr {
				g = c.fields()
				g[name] = ""
				if _, err := CanonicalChange(c.action, c.subject, g); err == nil {
					t.Errorf("%s with an empty %s accepted", c.action, name)
				}
			}
		}
		for _, extra := range []string{"visibility", "provider", "x", "token_ref"} {
			g := c.fields()
			g[extra] = "x"
			if _, err := CanonicalChange(c.action, c.subject, g); err == nil {
				t.Errorf("%s with extra %s accepted", c.action, extra)
			}
		}
		if _, err := CanonicalChange(c.action, c.subject, nil); err == nil {
			t.Errorf("%s without fields accepted", c.action)
		}
	}

	// subject rules: project id for three, provider id for clear_orphan
	for _, s := range []string{"a", "my-proj", "a" + strings.Repeat("b", 39)} {
		for _, a := range []string{ActionRemoveProject, ActionRemoveDeployKey, ActionDeleteRemoteRepo} {
			if _, err := CanonicalChange(a, s, map[string]func() map[string]any{ActionRemoveProject: removeProjectFields, ActionRemoveDeployKey: removeKeyFields, ActionDeleteRemoteRepo: deleteRemoteFields}[a]()); err != nil {
				t.Errorf("%s subject %q refused: %v", a, s, err)
			}
		}
		if _, err := CanonicalChange(ActionClearOrphan, s, clearOrphanFields()); err != nil {
			t.Errorf("clear_orphan subject %q refused: %v", s, err)
		}
	}
	for _, s := range []string{"", "A", "1a", "-a", "a_b", "a b", "a\n", "a/b", "../x", "é", "a" + strings.Repeat("b", 40)} {
		if _, err := CanonicalChange(ActionRemoveProject, s, removeProjectFields()); err == nil {
			t.Errorf("remove_project subject %q accepted", s)
		}
		if _, err := CanonicalChange(ActionDeleteRemoteRepo, s, deleteRemoteFields()); err == nil {
			t.Errorf("delete_remote_repo subject %q accepted", s)
		}
		if _, err := CanonicalChange(ActionRemoveDeployKey, s, removeKeyFields()); err == nil {
			t.Errorf("remove_deploy_key subject %q accepted", s)
		}
		if _, err := CanonicalChange(ActionClearOrphan, s, clearOrphanFields()); err == nil {
			t.Errorf("clear_orphan subject %q accepted", s)
		}
	}
	// the project-id rule and the provider-id rule are separate (swap one, the other stays)
	old := subjectRE
	subjectRE = regexpNever
	if validSubject(ActionClearOrphan, "ok-id") != true || validSubject(ActionRemoveProject, "ok-id") != false {
		t.Error("clear_orphan must use the provider id rule, remove_project the project id rule")
	}
	subjectRE = old
	oldP := providerIDRE
	providerIDRE = regexpNever
	if validSubject(ActionClearOrphan, "ok-id") != false || validSubject(ActionRemoveProject, "ok-id") != true {
		t.Error("provider id rule coupling")
	}
	providerIDRE = oldP

	// remove_project.scope is exactly "local"
	for _, bad := range []string{"remote", "all", "Local", "local ", "both", "LOCAL"} {
		f := removeProjectFields()
		f["scope"] = bad
		if _, err := CanonicalChange(ActionRemoveProject, "my-proj", f); err == nil {
			t.Errorf("scope %q accepted", bad)
		}
	}
	for _, ack := range []bool{true, false} {
		f := removeProjectFields()
		f["legacy_ack"] = ack
		if _, err := CanonicalChange(ActionRemoveProject, "my-proj", f); err != nil {
			t.Errorf("legacy_ack %v refused: %v", ack, err)
		}
	}
	// remote_path uses the shared validator (the same one link/attach use)
	for _, bad := range []string{"solo", "a/../b", "octo/repo.git", "octo/re%2fpo", "octo/저장소", "a/b/c/d/e/f", "/a/b"} {
		f := deleteRemoteFields()
		f["remote_path"] = bad
		if _, err := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", f); err == nil {
			t.Errorf("delete_remote_repo remote_path %q accepted", bad)
		}
		g := removeKeyFields()
		g["remote_path"] = bad
		if _, err := CanonicalChange(ActionRemoveDeployKey, "my-proj", g); err == nil {
			t.Errorf("remove_deploy_key remote_path %q accepted", bad)
		}
	}
	// remote_id: decimal string of 1..15 digits, > 0, no sign / leading zero / spaces
	for _, ok := range []string{"1", "42", "123456789012345"} {
		f := deleteRemoteFields()
		f["remote_id"] = ok
		if _, err := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", f); err != nil {
			t.Errorf("remote_id %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "00", "01", "-1", "+1", "1.0", "1e3", " 1", "1 ", "0x10", "1234567890123456", "abc", "１２"} {
		f := deleteRemoteFields()
		f["remote_id"] = bad
		if _, err := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", f); err == nil {
			t.Errorf("remote_id %q accepted", bad)
		}
	}
	f := deleteRemoteFields()
	f["remote_id"] = true
	if _, err := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", f); err == nil {
		t.Error("remote_id as a bool accepted")
	}
	// clear_orphan.project is validated like an orphan record's project field
	for _, ok := range []string{"team/half", "Octo-Cat/half-made", "a/b/c/d/e", "grp/sub/proj"} {
		if _, err := CanonicalChange(ActionClearOrphan, "gl", map[string]any{"project": ok}); err != nil {
			t.Errorf("orphan project %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"solo", "o/../x", "o/r%2f", "../x", "o/r.git", "a/b/c/d/e/f", "o/r s", "o/ré", "/a/b", "a/b/"} {
		if _, err := CanonicalChange(ActionClearOrphan, "gl", map[string]any{"project": bad}); err == nil {
			t.Errorf("orphan project %q accepted", bad)
		}
	}
	// the four actions never share a canonical change or a hash, even for the same subject
	seen := map[string]string{}
	for _, c := range cases {
		h, err := ChangeHash(c.action, "my-proj", c.fields())
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[h]; dup {
			t.Errorf("%s and %s share a change hash", prev, c.action)
		}
		seen[h] = c.action
	}
	// a valid remove_project change under another action's name is refused outright
	if _, err := CanonicalChange(ActionRemoveDeployKey, "my-proj", removeProjectFields()); err == nil {
		t.Error("remove_project fields accepted as remove_deploy_key")
	}
	if _, err := CanonicalChange(ActionRemoveProject, "my-proj", removeKeyFields()); err == nil {
		t.Error("remove_deploy_key fields accepted as remove_project")
	}
	// ParseFields keeps the types distinct (a string "true" is not a bool)
	pf, err := ParseFields([]byte(`{"scope":"local","legacy_ack":"true","detach_key":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalChange(ActionRemoveProject, "my-proj", pf); err == nil {
		t.Error(`legacy_ack "true" (string) accepted`)
	}
}

func TestConfirmNameField(t *testing.T) {
	ok := []string{"Hello-World", "my_repo", "Repo.js", "a", "x.gitx", "UPPER", "a.b-c_d", strings.Repeat("a", 100)}
	bad := []string{"", ".", "..", "a/b", "x.git", "x.GIT", "a b", "a\n", "é", "a%2f", strings.Repeat("a", 101), " Hello", "Hello "}
	for _, s := range ok {
		f := deleteRemoteFields()
		f["confirm_name"] = s
		if _, err := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", f); err != nil {
			t.Errorf("confirm_name %q refused: %v", s, err)
		}
		if !ValidConfirmName(s) {
			t.Errorf("ValidConfirmName(%q) false", s)
		}
	}
	for _, s := range bad {
		f := deleteRemoteFields()
		f["confirm_name"] = s
		if _, err := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", f); err == nil {
			t.Errorf("confirm_name %q accepted", s)
		}
		if ValidConfirmName(s) {
			t.Errorf("ValidConfirmName(%q) true", s)
		}
	}
	// case is kept as written (the broker compares it exactly)
	a, _ := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", deleteRemoteFields())
	f := deleteRemoteFields()
	f["confirm_name"] = "hello-world"
	b, _ := CanonicalChange(ActionDeleteRemoteRepo, "my-proj", f)
	if string(a) == string(b) || !strings.Contains(string(b), `"confirm_name":"hello-world"`) {
		t.Error("confirm_name case was folded")
	}
	// the helper the ops will compare with
	for path, want := range map[string]string{"octo/Hello-World": "Hello-World", "a/b/c": "c", "x/y.z": "y.z"} {
		if got, ok := RemoteLastSegment(path); !ok || got != want {
			t.Errorf("RemoteLastSegment(%q) = %q %v", path, got, ok)
		}
	}
	for _, p := range []string{"", "solo", "a/../b", "a/b.git"} {
		if _, ok := RemoteLastSegment(p); ok {
			t.Errorf("RemoteLastSegment(%q) ok", p)
		}
	}
}

func TestExistingActionsUnchangedByW3f(t *testing.T) {
	// canonical texts of every earlier action are byte for byte what they were
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
		{ActionLinkProject, "hello", linkFields(), `{"action":"link_project","fields":{"ai_context":false,"deploy_key_sha256":"` + hex64() + `","provider":"gh","publish":"read","remote_path":"octo/Hello-World"},"subject":"hello"}`},
		{ActionAttachRemote, "a", attachFields(), `{"action":"attach_remote","fields":{"deploy_key_sha256":"` + hex64() + `","provider":"gh","publish":"write","remote_path":"octo/Hello-World"},"subject":"a"}`},
	}
	for _, c := range cases {
		got, err := CanonicalChange(c.action, c.subject, c.fields)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s %v", c.action, got, err)
		}
	}
	// the earlier actions did not gain the new fields
	for _, extra := range []string{"scope", "legacy_ack", "remote_id", "confirm_name", "also_local"} {
		f := regFields()
		f[extra] = "local"
		if _, err := CanonicalChange(ActionRegisterProject, "my-proj", f); err == nil {
			t.Errorf("register_project accepted %s", extra)
		}
	}
	// the action set is exactly the earlier ten plus the four new ones
	if len(schema) != 14 {
		t.Errorf("schema has %d actions, want 14", len(schema))
	}
	// the signing package knows the same new constants (no drift)
	for name, pair := range map[string][2]string{
		"remove_project": {ActionRemoveProject, signing.RegistryActionRemoveProject}, "delete_remote_repo": {ActionDeleteRemoteRepo, signing.RegistryActionDeleteRemoteRepo},
		"remove_deploy_key": {ActionRemoveDeployKey, signing.RegistryActionRemoveDeployKey}, "clear_orphan": {ActionClearOrphan, signing.RegistryActionClearOrphan},
	} {
		if pair[0] != name || pair[1] != name {
			t.Errorf("%s constants: registrymsg %q signing %q", name, pair[0], pair[1])
		}
	}
	// drift: signing accepts exactly the subjects registrymsg accepts, per action
	corpus := []string{"a", "aa", "admin", "my-app", "a" + strings.Repeat("b", 39), "a" + strings.Repeat("b", 40),
		"", "A", "1a", "-a", "a_b", "a b", "a\n", "a.b", "é", "a/b", "../x", "ab-", "x\x00"}
	for _, s := range corpus {
		for _, a := range []string{ActionRemoveProject, ActionDeleteRemoteRepo, ActionRemoveDeployKey, ActionClearOrphan} {
			m := signing.RegistryMessage{Action: a, Subject: s, Change: strings.Repeat("c", 64), Request: strings.Repeat("1", 32),
				Issued: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
			want := ValidSubject(s)
			if a == ActionClearOrphan {
				want = ValidProviderID(s)
			}
			if got := m.Validate() == nil; got != want {
				t.Errorf("%s subject %q: signing accepts=%v registrymsg accepts=%v", a, s, got, want)
			}
		}
	}
}
