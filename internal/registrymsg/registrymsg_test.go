package registrymsg

import (
	"aigate/internal/approval/signing"
	"regexp"
	"strings"
	"testing"
	"time"
)

func regFields() map[string]any {
	return map[string]any{"path": "/home/orch/repos/app", "kind": "registered", "ai_context": false}
}

func TestCanonicalChangeStableAndStrict(t *testing.T) {
	got, err := CanonicalChange("register_project", "my-proj", regFields())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"action":"register_project","fields":{"ai_context":false,"kind":"registered","path":"/home/orch/repos/app"},"subject":"my-proj"}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	// Key order of the input (JSON text) does not matter.
	a, err := ParseFields([]byte(`{"path":"/home/orch/repos/app","kind":"registered","ai_context":false}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseFields([]byte(`{ "ai_context": false, "kind": "registered", "path": "/home/orch/repos/app" }`))
	if err != nil {
		t.Fatal(err)
	}
	h1, err1 := ChangeHash("register_project", "my-proj", a)
	h2, err2 := ChangeHash("register_project", "my-proj", b)
	if err1 != nil || err2 != nil || h1 != h2 {
		t.Fatalf("hash differs: %q %q %v %v", h1, h2, err1, err2)
	}
	sf, err := CanonicalChange("set_flag", "my-proj", map[string]any{"flag": "ai_context", "value": true})
	if err != nil || string(sf) != `{"action":"set_flag","fields":{"flag":"ai_context","value":true},"subject":"my-proj"}` {
		t.Fatalf("set_flag: %s %v", sf, err)
	}
	// HTML-significant characters are not escaped (matches JSON.stringify).
	f := regFields()
	f["path"] = "/home/orch/repos/a&b"
	if c, err := CanonicalChange("register_project", "x", f); err != nil || !strings.Contains(string(c), `a&b`) {
		t.Fatalf("html escaping: %s %v", c, err)
	}

	// Golden: exactly what a browser's JSON.stringify produces.
	g := regFields()
	g["path"] = "a\"b\\c<d>한글😀"
	gold, err := CanonicalChange("register_project", "my-proj", g)
	if err != nil {
		t.Fatal(err)
	}
	wantGold := `{"action":"register_project","fields":{"ai_context":false,"kind":"registered","path":"a\"b\\c<d>한글😀"},"subject":"my-proj"}`
	if string(gold) != wantGold {
		t.Fatalf("golden: got %s\nwant %s", gold, wantGold)
	}

	bad := map[string]struct {
		action, subject string
		fields          map[string]any
	}{
		"unknown action":    {"delete_project", "p", regFields()},
		"bad subject":       {"register_project", "P", regFields()},
		"unknown field":     {"register_project", "p", map[string]any{"path": "/x", "kind": "registered", "ai_context": false, "extra": "x"}},
		"missing field":     {"register_project", "p", map[string]any{"path": "/x", "kind": "registered"}},
		"float field":       {"register_project", "p", map[string]any{"path": "/x", "kind": "registered", "ai_context": 1.5}},
		"int field":         {"register_project", "p", map[string]any{"path": "/x", "kind": "registered", "ai_context": 1}},
		"string for bool":   {"register_project", "p", map[string]any{"path": "/x", "kind": "registered", "ai_context": "true"}},
		"nil fields":        {"register_project", "p", nil},
		"bad kind":          {"register_project", "p", map[string]any{"path": "/x", "kind": "Bad Kind", "ai_context": false}},
		"empty path":        {"register_project", "p", map[string]any{"path": "", "kind": "registered", "ai_context": false}},
		"control in path":   {"register_project", "p", map[string]any{"path": "/x\n", "kind": "registered", "ai_context": false}},
		"invalid utf8":      {"register_project", "p", map[string]any{"path": "/x\xff", "kind": "registered", "ai_context": false}},
		"U+2029":            {"register_project", "p", map[string]any{"path": "/x\u2029", "kind": "registered", "ai_context": false}},
		"DEL":               {"register_project", "p", map[string]any{"path": "/x\x7f", "kind": "registered", "ai_context": false}},
		"line separator":    {"register_project", "p", map[string]any{"path": "/x\u2028", "kind": "registered", "ai_context": false}},
		"set_flag bad flag": {"set_flag", "p", map[string]any{"flag": "Auto Merge", "value": true}},
		"set_flag no value": {"set_flag", "p", map[string]any{"flag": "ai_context"}},
		"set_flag float":    {"set_flag", "p", map[string]any{"flag": "ai_context", "value": 1.0}},
		"set_flag extra":    {"set_flag", "p", map[string]any{"flag": "ai_context", "value": true, "x": true}},
	}
	for name, c := range bad {
		if out, err := CanonicalChange(c.action, c.subject, c.fields); err == nil {
			t.Errorf("%s: accepted: %s", name, out)
		}
		if _, err := ChangeHash(c.action, c.subject, c.fields); err == nil {
			t.Errorf("%s: ChangeHash accepted", name)
		}
	}

	// Raw JSON strictness: duplicate keys, floats, non-objects, trailing data.
	for name, raw := range map[string]string{
		"duplicate key":      `{"path":"/a","path":"/b","kind":"registered","ai_context":false}`,
		"float":              `{"flag":"ai_context","value":1.5}`,
		"integer":            `{"flag":"ai_context","value":1}`,
		"array":              `[]`,
		"null":               `null`,
		"trailing":           `{"flag":"ai_context","value":true} {}`,
		"nested":             `{"flag":{"a":1},"value":true}`,
		"empty":              ``,
		"invalid utf8":       "{\"flag\":\"a\xff\",\"value\":true}",
		"lone surrogate":     `{"flag":"a\ud800","value":true}`,
		"lone low surrogate": `{"flag":"a\udc00","value":true}`,
	} {
		if m, err := ParseFields([]byte(raw)); err == nil {
			// Duplicate/float rejection must happen in the parser itself.
			t.Errorf("%s: ParseFields accepted: %v", name, m)
		}
	}
}

func TestChangeHashChangesWithAnyField(t *testing.T) {
	base, err := ChangeHash("register_project", "my-proj", regFields())
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(base) {
		t.Fatalf("hash format: %q", base)
	}
	seen := map[string]string{base: "base"}
	check := func(name, h string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if prev, dup := seen[h]; dup {
			t.Fatalf("%s collides with %s", name, prev)
		}
		seen[h] = name
	}
	h, err := ChangeHash("register_project", "other", regFields())
	check("subject", h, err)
	for k, v := range map[string]any{"path": "/home/orch/repos/app2", "kind": "other", "ai_context": true} {
		f := regFields()
		f[k] = v
		h, err := ChangeHash("register_project", "my-proj", f)
		check("field "+k, h, err)
	}
	h, err = ChangeHash("set_flag", "my-proj", map[string]any{"flag": "ai_context", "value": true})
	check("action", h, err)
	h, err = ChangeHash("set_flag", "my-proj", map[string]any{"flag": "ai_context", "value": false})
	check("value", h, err)
	h, err = ChangeHash("set_flag", "my-proj", map[string]any{"flag": "ai_contexx", "value": true})
	check("flag", h, err)
}

func TestNewRequestID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, b := NewRequestID(), NewRequestID()
	if !re.MatchString(a) || !re.MatchString(b) || a == b {
		t.Fatalf("bad ids: %q %q", a, b)
	}
}

// The registry action constants and subject rule are duplicated in signing
// (which must not import this package); this keeps them from drifting.
func TestSigningDefinitionsDoNotDrift(t *testing.T) {
	if ActionRegisterProject != signing.RegistryActionRegisterProject || ActionSetFlag != signing.RegistryActionSetFlag ||
		ActionSetToken != signing.RegistryActionSetToken || ActionDeleteToken != signing.RegistryActionDeleteToken {
		t.Fatal("action constants differ between registrymsg and signing")
	}
	corpus := []string{
		"a", "my-proj", "a1-b2", "a" + strings.Repeat("b", 39), "a" + strings.Repeat("b", 40),
		"", "A", "1a", "-a", "a_b", "a b", "a\n", "a.b", "é", "a/b", "../x", "ab-", "x\x00",
	}
	valid := strings.Repeat("c", 64)
	for _, s := range corpus {
		m := signing.RegistryMessage{
			Action: ActionSetFlag, Subject: s, Change: valid, Request: strings.Repeat("1", 32),
			Issued: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
		}
		if got, want := m.Validate() == nil, ValidSubject(s); got != want {
			t.Errorf("subject %q: signing accepts=%v registrymsg accepts=%v", s, got, want)
		}
	}
	// W3c actions: constants and subject rules (the provider id rule for the
	// provider/group actions, the project name shape for create_project; the
	// reserved-name list is registrymsg-only on purpose).
	if ActionSetProvider != signing.RegistryActionSetProvider || ActionDeleteProvider != signing.RegistryActionDeleteProvider ||
		ActionCreateGroup != signing.RegistryActionCreateGroup || ActionCreateProject != signing.RegistryActionCreateProject {
		t.Fatal("W3c action constants differ between registrymsg and signing")
	}
	w3cCorpus := append([]string{"aa", "admin", "a" + strings.Repeat("b", 38)}, corpus...)
	for _, s := range w3cCorpus {
		for _, a := range []string{ActionSetProvider, ActionDeleteProvider, ActionCreateGroup, ActionCreateProject} {
			m := signing.RegistryMessage{
				Action: a, Subject: s, Change: valid, Request: strings.Repeat("1", 32),
				Issued: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
			}
			want := ValidProviderID(s)
			if a == ActionCreateProject {
				want = nameRE.MatchString(s)
			}
			if got := m.Validate() == nil; got != want {
				t.Errorf("%s subject %q: signing accepts=%v registrymsg accepts=%v", a, s, got, want)
			}
		}
	}
	// token actions use the token ref rule
	for _, s := range corpus {
		for _, a := range []string{ActionSetToken, ActionDeleteToken} {
			m := signing.RegistryMessage{
				Action: a, Subject: s, Change: valid, Request: strings.Repeat("1", 32),
				Issued: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
			}
			if got, want := m.Validate() == nil, ValidTokenRef(s); got != want {
				t.Errorf("%s subject %q: signing accepts=%v registrymsg accepts=%v", a, s, got, want)
			}
		}
	}
}
