package registrymsg

import (
	"regexp"
	"strings"
	"testing"
)

func hex64() string { return strings.Repeat("ab", 32) }

func providerFields() map[string]any {
	return map[string]any{"kind": "gitlab", "base_url": "http://100.64.0.10:18082", "ssh_url": "ssh://git@100.64.0.10:22", "token_ref": "gitlab-admin"}
}

func groupFields() map[string]any {
	return map[string]any{"name": "team", "path": "team", "parent_path": ""}
}

func projectFields() map[string]any {
	return map[string]any{
		"provider": "gl", "group_path": "team/sub", "template": "basic", "description": "한글 설명 ok",
		"ai_context": false, "publish": "read", "deploy_key_sha256": hex64(),
	}
}

func cloneFields(m map[string]any) map[string]any {
	c := map[string]any{}
	for k, v := range m {
		c[k] = v
	}
	return c
}

func TestNewActionsInSchema(t *testing.T) {
	type tc struct {
		action, subject string
		good            func() map[string]any
		emptyOK         []string
	}
	cases := []tc{
		{ActionSetProvider, "gl", providerFields, nil},
		{ActionDeleteProvider, "gl", func() map[string]any { return map[string]any{} }, nil},
		{ActionCreateGroup, "gl", groupFields, []string{"parent_path"}},
		{ActionCreateProject, "my-app", projectFields, []string{"description"}},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			if _, err := CanonicalChange(c.action, c.subject, c.good()); err != nil {
				t.Fatalf("good change refused: %v", err)
			}
			if c.action == ActionDeleteProvider {
				if _, err := CanonicalChange(c.action, c.subject, nil); err != nil {
					t.Fatalf("nil fields: %v", err)
				}
			}
			for name, v := range c.good() {
				// missing
				f := cloneFields(c.good())
				delete(f, name)
				if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
					t.Errorf("missing %s accepted", name)
				}
				// wrong type
				f = cloneFields(c.good())
				if _, isBool := v.(bool); isBool {
					f[name] = "true"
				} else {
					f[name] = true
				}
				if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
					t.Errorf("wrong type for %s accepted", name)
				}
				for _, bad := range []any{int64(1), 1.5, nil, []any{}, map[string]any{}} {
					f = cloneFields(c.good())
					f[name] = bad
					if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
						t.Errorf("%s=%T accepted", name, bad)
					}
				}
				// empty string only where listed
				if _, isBool := v.(bool); !isBool {
					f = cloneFields(c.good())
					f[name] = ""
					_, err := CanonicalChange(c.action, c.subject, f)
					allowed := false
					for _, e := range c.emptyOK {
						allowed = allowed || e == name
					}
					if allowed && err != nil {
						t.Errorf("empty %s refused: %v", name, err)
					}
					if !allowed && err == nil {
						t.Errorf("empty %s accepted", name)
					}
				}
			}
			// unknown extra field
			f := cloneFields(c.good())
			f["extra"] = "x"
			if _, err := CanonicalChange(c.action, c.subject, f); err == nil {
				t.Error("unknown field accepted")
			}
		})
	}
	// delete_provider carries no fields at all.
	if _, err := CanonicalChange(ActionDeleteProvider, "gl", map[string]any{"x": "y"}); err == nil {
		t.Error("delete_provider with a field accepted")
	}
	// Golden canonical text (a browser's JSON.stringify of sorted keys gives the same).
	got, err := CanonicalChange(ActionCreateGroup, "gl", groupFields())
	want := `{"action":"create_group","fields":{"name":"team","parent_path":"","path":"team"},"subject":"gl"}`
	if err != nil || string(got) != want {
		t.Fatalf("golden create_group: %s %v", got, err)
	}
	got, err = CanonicalChange(ActionDeleteProvider, "gl", nil)
	if err != nil || string(got) != `{"action":"delete_provider","fields":{},"subject":"gl"}` {
		t.Fatalf("golden delete_provider: %s %v", got, err)
	}
	// the three provider fields are validated by their own rules
	for field, bad := range map[string]string{
		"kind": "github", "base_url": "http://u:p@h", "ssh_url": "ssh://root@h", "token_ref": "Bad_Ref",
	} {
		f := providerFields()
		f[field] = bad
		if _, err := CanonicalChange(ActionSetProvider, "gl", f); err == nil {
			t.Errorf("set_provider %s=%q accepted", field, bad)
		}
	}
	// create_group fields
	for field, bad := range map[string]string{"name": "Team", "path": "a/b", "parent_path": "/x"} {
		f := groupFields()
		f[field] = bad
		if _, err := CanonicalChange(ActionCreateGroup, "gl", f); err == nil {
			t.Errorf("create_group %s=%q accepted", field, bad)
		}
	}
	// parent_path of 4 segments would make the group 5 deep
	f := groupFields()
	f["parent_path"] = "aa/bb/cc/dd"
	if _, err := CanonicalChange(ActionCreateGroup, "gl", f); err == nil {
		t.Error("group deeper than 4 accepted")
	}
	f["parent_path"] = "aa/bb/cc"
	if _, err := CanonicalChange(ActionCreateGroup, "gl", f); err != nil {
		t.Errorf("group at depth 4 refused: %v", err)
	}
}

func TestProviderURLShape(t *testing.T) {
	goodBase := []string{
		"http://gitlab.example.com", "https://gitlab.example.com", "http://100.64.0.10:18082", "https://a-b.example.co:8443",
		"http://localhost", "http://[fd7a:115c:a1e0::1]:8080", "http://host",
	}
	badBase := []string{
		"", "gitlab.example.com", "ftp://h", "ssh://h", "HTTP://h", "http://", "http:///x", "http://h/", "http://h/api/v4", "http://h/x",
		"http://u@h", "http://u:p@h", "http://:@h", "http://h?x=1", "http://h?", "http://h#f", "http://h#", "http://H.example.com",
		"http://h:0", "http://h:65536", "http://h:080", "http://h:", "http://h:ab", "http://h .com", "http://h\\x", "http://h%2e",
		"http://éxample.com", "http://exa_mple.com", "http://-h.com", "http://h-.com", "http://h..com", "http://h.com.", "http://h\n",
		"http://[::1", "http://[zz]", "http://1.2.3.4.5:99999", "http://h:1:2", "http://" + strings.Repeat("a", 300),
	}
	for _, s := range goodBase {
		if !ValidBaseURL(s) {
			t.Errorf("base_url %q refused", s)
		}
	}
	for _, s := range badBase {
		if ValidBaseURL(s) {
			t.Errorf("base_url %q accepted", s)
		}
	}
	goodSSH := []string{"ssh://git@gitlab.example.com", "ssh://git@100.64.0.10:22", "ssh://git@host:2222", "ssh://git@[fd7a::1]:22"}
	badSSH := []string{
		"", "ssh://gitlab.example.com", "ssh://root@h", "ssh://git:pw@h", "ssh://git@", "ssh://git@h/", "ssh://git@h/x.git", "ssh://git@h?x", "ssh://git@h#f",
		"http://git@h", "git@h:x.git", "SSH://git@h", "ssh://git@H", "ssh://git@h:0", "ssh://git@h:99999", "ssh://git@h:22/", "ssh://git@h\n", "ssh://git@-h", "ssh://git@h -oProxyCommand=x",
		"ssh://git@h:22:22", "ssh://Git@h",
	}
	for _, s := range goodSSH {
		if !ValidSSHURL(s) {
			t.Errorf("ssh_url %q refused", s)
		}
	}
	for _, s := range badSSH {
		if ValidSSHURL(s) {
			t.Errorf("ssh_url %q accepted", s)
		}
	}
}

func TestGroupAndProjectNameRules(t *testing.T) {
	names := map[string]bool{
		"ab": true, "my-app": true, "a1-b2": true, "a" + strings.Repeat("b", 39): true,
		"a": false, "a" + strings.Repeat("b", 40): false, "": false, "A": false, "Ab": false, "aB": false, "1a": false, "-a": false,
		"a_b": false, "a b": false, "a\n": false, "ab\n": false, "a.b": false, ".": false, "..": false, "a/b": false, "a%2fb": false,
		"a%2Fb": false, "%2e%2e": false, "ab%": false, "éa": false, "аpp": false /* cyrillic a */, "ａpp": false /* fullwidth */, "ab\x00": false,
		// reserved by GitLab
		"admin": false, "api": false, "projects": false, "groups": false, "users": false, "dashboard": false, "new": false, "edit": false, "hooks": false,
	}
	for s, ok := range names {
		if got := ValidName(s); got != ok {
			t.Errorf("ValidName(%q)=%v want %v", s, got, ok)
		}
	}
	paths := map[string]bool{
		"ab": true, "ab/cd": true, "ab/cd/ef/gh": true,
		"ab/cd/ef/gh/ij": false, "": false, "/ab": false, "ab/": false, "ab//cd": false, "ab/../cd": false, "ab/./cd": false, "..": false, ".": false,
		"ab/%2e%2e": false, "ab/cd%2fef": false, "ab%2fcd": false, "Ab/cd": false, "ab/Cd": false, "ab/c": false, "ab\\cd": false, "ab/cd\n": false,
		"ab/аpp": false, "ab/éa": false, "ab/admin": false, "admin/ab": false, " ab": false, "ab ": false, "ab/ cd": false,
	}
	for s, ok := range paths {
		if got := ValidGroupPath(s); got != ok {
			t.Errorf("ValidGroupPath(%q)=%v want %v", s, got, ok)
		}
	}
	// the rules are applied inside the actions too
	f := projectFields()
	for _, bad := range []string{"", "a", "Team", "a/../b", "ab/", "x/%2e%2e", "ab/admin", "a.b/cd"} {
		f["group_path"] = bad
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
			t.Errorf("group_path %q accepted", bad)
		}
	}
	f["group_path"] = "ab"
	for _, bad := range []string{"a", "A1", "my_app", "admin", "..", "a/b", "a%2fb", ""} {
		if _, err := CanonicalChange(ActionCreateProject, bad, f); err == nil {
			t.Errorf("project subject %q accepted", bad)
		}
	}
	if _, err := CanonicalChange(ActionCreateProject, "ab", f); err != nil {
		t.Errorf("2-char project name refused: %v", err)
	}
}

func TestDeployKeyHashField(t *testing.T) {
	f := projectFields()
	for _, bad := range []string{
		"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64),
		hex64() + "\n", " " + hex64()[1:], "sha256:" + hex64(), strings.Repeat("a", 32) + "é" + strings.Repeat("a", 31),
	} {
		f["deploy_key_sha256"] = bad
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
			t.Errorf("deploy_key_sha256 %q accepted", bad)
		}
	}
	f["deploy_key_sha256"] = strings.Repeat("0", 64)
	if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err != nil {
		t.Errorf("valid hash refused: %v", err)
	}
}

func TestTemplateAndPublishEnums(t *testing.T) {
	f := projectFields()
	for _, ok := range []string{"basic", "go"} {
		f["template"] = ok
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err != nil {
			t.Errorf("template %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Basic", "GO", "python", "basic ", "basic\n", "go/../x", "../basic"} {
		f["template"] = bad
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
			t.Errorf("template %q accepted", bad)
		}
	}
	f["template"] = "basic"
	for _, ok := range []string{"write", "read"} {
		f["publish"] = ok
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err != nil {
			t.Errorf("publish %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Write", "rw", "none", "read ", "write\n"} {
		f["publish"] = bad
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
			t.Errorf("publish %q accepted", bad)
		}
	}
}

func TestDescriptionRules(t *testing.T) {
	f := projectFields()
	ok := []string{"", "plain text", "한글 😀 mixed", strings.Repeat("가", 200), strings.Repeat("a", 200), `quote " backslash \ <tag> & amp`}
	for _, s := range ok {
		f["description"] = s
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err != nil {
			t.Errorf("description %q refused: %v", s, err)
		}
	}
	bad := map[string]string{
		"201 chars":      strings.Repeat("a", 201),
		"201 hangul":     strings.Repeat("가", 201),
		"newline":        "a\nb",
		"cr":             "a\rb",
		"tab":            "a\tb",
		"nul":            "a\x00b",
		"del":            "a\x7fb",
		"c1 control":     "a\u0085b",
		"ls":             "a\u2028b",
		"ps":             "a\u2029b",
		"rlo":            "a\u202eb",
		"lro":            "a\u202db",
		"rle":            "a\u202bb",
		"pdf":            "a\u202cb",
		"lri":            "a\u2066b",
		"rli":            "a\u2067b",
		"fsi":            "a\u2068b",
		"pdi":            "a\u2069b",
		"lrm":            "a\u200eb",
		"rlm":            "a\u200fb",
		"alm":            "a\u061cb",
		"zwsp":           "a\u200bb",
		"zwj":            "a\u200db",
		"bom":            "a\ufeffb",
		"soft hyphen":    "a\u00adb",
		"private use":    "a\ue000b",
		"invalid utf8":   "a\xffb",
		"lone surrogate": "a�b",
	}
	for name, s := range bad {
		f["description"] = s
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err == nil {
			t.Errorf("description (%s) accepted", name)
		}
	}
	if !ValidDescription("") || ValidDescription("a\nb") {
		t.Error("ValidDescription helper disagrees")
	}
}

func TestExistingActionsUnchangedByW3c(t *testing.T) {
	cases := []struct {
		action, subject string
		fields          map[string]any
		want            string
	}{
		{ActionRegisterProject, "my-proj", regFields(), `{"action":"register_project","fields":{"ai_context":false,"kind":"registered","path":"/home/orch/repos/app"},"subject":"my-proj"}`},
		{ActionSetFlag, "my-proj", map[string]any{"flag": "ai_context", "value": true}, `{"action":"set_flag","fields":{"flag":"ai_context","value":true},"subject":"my-proj"}`},
		{ActionSetToken, "gitlab-admin", map[string]any{"kind": "gitlab", "expires": "", "value_hash": hex64()}, `{"action":"set_token","fields":{"expires":"","kind":"gitlab","value_hash":"` + hex64() + `"},"subject":"gitlab-admin"}`},
		{ActionDeleteToken, "gitlab-admin", nil, `{"action":"delete_token","fields":{},"subject":"gitlab-admin"}`},
	}
	for _, c := range cases {
		got, err := CanonicalChange(c.action, c.subject, c.fields)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s %v", c.action, got, err)
		}
	}
	// the old one-character project id and the old kind rule still work for the old actions
	if _, err := CanonicalChange(ActionSetFlag, "a", map[string]any{"flag": "ai_context", "value": true}); err != nil {
		t.Errorf("1-char project id refused for set_flag: %v", err)
	}
	if _, err := CanonicalChange(ActionSetFlag, "admin", map[string]any{"flag": "ai_context", "value": true}); err != nil {
		t.Errorf("reserved-name rule leaked into set_flag: %v", err)
	}
	// old actions must not take the new fields and vice versa
	if _, err := CanonicalChange(ActionRegisterProject, "my-proj", projectFields()); err == nil {
		t.Error("register_project accepted create_project fields")
	}
	if _, err := CanonicalChange(ActionCreateProject, "my-app", regFields()); err == nil {
		t.Error("create_project accepted register_project fields")
	}
	// an empty string is still refused for the old actions' string fields
	f := regFields()
	f["kind"] = ""
	if _, err := CanonicalChange(ActionRegisterProject, "my-proj", f); err == nil {
		t.Error("empty kind accepted")
	}
	// description/parent_path emptiness must not leak into other fields or actions
	pf := projectFields()
	pf["group_path"] = ""
	if _, err := CanonicalChange(ActionCreateProject, "my-app", pf); err == nil {
		t.Error("empty group_path accepted")
	}
	tf := map[string]any{"kind": "gitlab", "expires": "", "value_hash": ""}
	if _, err := CanonicalChange(ActionSetToken, "gitlab-admin", tf); err == nil {
		t.Error("empty value_hash accepted")
	}
}

// providerIDRE (set_provider, delete_provider, create_group subject), nameRE
// (create_project subject), tokenRefRE (token actions) and subjectRE (project
// id of the old actions) are four separate rules: changing one must not move
// another action family.
func TestSubjectRulePerAction(t *testing.T) {
	accepts := func(action, subject string) bool {
		var f map[string]any
		switch action {
		case ActionRegisterProject:
			f = regFields()
		case ActionSetFlag:
			f = map[string]any{"flag": "ai_context", "value": true}
		case ActionSetToken:
			f = map[string]any{"kind": "gitlab", "expires": "", "value_hash": hex64()}
		case ActionDeleteToken, ActionDeleteProvider:
			f = map[string]any{}
		case ActionSetProvider:
			f = providerFields()
		case ActionCreateGroup:
			f = groupFields()
		case ActionCreateProject:
			f = projectFields()
		}
		_, err := CanonicalChange(action, subject, f)
		return err == nil
	}
	// observable differences today
	for _, a := range []string{ActionSetProvider, ActionDeleteProvider, ActionCreateGroup} {
		if !accepts(a, "a") || !accepts(a, "admin") {
			t.Errorf("%s: provider id rule must accept a 1-char id and reserved-looking names", a)
		}
	}
	if accepts(ActionCreateProject, "a") || accepts(ActionCreateProject, "admin") {
		t.Error("create_project: project name rule must refuse 1 char and reserved names")
	}
	for _, a := range []string{ActionRegisterProject, ActionSetFlag, ActionSetToken, ActionDeleteToken} {
		if !accepts(a, "a") || !accepts(a, "admin") {
			t.Errorf("%s: unchanged subject rule must still accept those", a)
		}
	}
	// subject rules are independent: swap each regex in turn
	all := map[string]**regexp.Regexp{"provider": &providerIDRE, "name": &nameRE, "token": &tokenRefRE, "project": &subjectRE}
	families := map[string][]string{
		"provider": {ActionSetProvider, ActionDeleteProvider, ActionCreateGroup},
		"name":     {ActionCreateProject},
		"token":    {ActionSetToken, ActionDeleteToken},
		"project":  {ActionRegisterProject, ActionSetFlag},
	}
	for swapped, p := range all {
		old := *p
		*p = regexp.MustCompile(`^zzz$`)
		for fam, actions := range families {
			for _, a := range actions {
				got := validSubject(a, "qq-ok") // subject rule only: field rules reuse some regexes on purpose
				if fam == swapped && got {
					t.Errorf("swapping %s regex had no effect on %s", swapped, a)
				}
				if fam != swapped && !got {
					t.Errorf("swapping %s regex changed %s", swapped, a)
				}
			}
		}
		*p = old
	}
	// token ref is not a provider id: check a name valid for one family only
	if ValidProviderID("") || ValidProviderID("A") || ValidProviderID("a"+strings.Repeat("b", 40)) {
		t.Error("ValidProviderID too lax")
	}
	if !ValidProviderID("a" + strings.Repeat("b", 39)) {
		t.Error("ValidProviderID refuses 40 chars")
	}
}

func TestNewActionConstants(t *testing.T) {
	want := map[string]string{ActionSetProvider: "set_provider", ActionDeleteProvider: "delete_provider", ActionCreateGroup: "create_group", ActionCreateProject: "create_project"}
	for got, w := range want {
		if got != w {
			t.Errorf("constant %q want %q", got, w)
		}
	}
}
