package registrymsg

import (
	"aigate/internal/approval/signing"
	"regexp"
	"strings"
	"testing"
	"time"
)

const canary = "canary-NOT-A-REAL-TOKEN-0123456789abcdefXYZ"

func setTokenFields(request string) map[string]any {
	return map[string]any{"kind": "gitlab", "expires": "2026-12-31", "value_hash": ValueHash(request, canary)}
}

func TestTokenActionsInSchema(t *testing.T) {
	req := strings.Repeat("a", 32)
	if _, err := CanonicalChange(ActionSetToken, "my-token", setTokenFields(req)); err != nil {
		t.Fatalf("valid set_token refused: %v", err)
	}
	want := `{"action":"set_token","fields":{"expires":"2026-12-31","kind":"gitlab","value_hash":"` +
		ValueHash(req, canary) + `"},"subject":"my-token"}`
	got, _ := CanonicalChange(ActionSetToken, "my-token", setTokenFields(req))
	if string(got) != want {
		t.Fatalf("canonical = %s\nwant %s", got, want)
	}
	bad := map[string]func(map[string]any){
		"unknown field":     func(f map[string]any) { f["extra"] = "x" },
		"missing kind":      func(f map[string]any) { delete(f, "kind") },
		"missing expires":   func(f map[string]any) { delete(f, "expires") },
		"missing hash":      func(f map[string]any) { delete(f, "value_hash") },
		"kind is bool":      func(f map[string]any) { f["kind"] = true },
		"expires is bool":   func(f map[string]any) { f["expires"] = false },
		"hash is bool":      func(f map[string]any) { f["value_hash"] = true },
		"hash uppercase":    func(f map[string]any) { f["value_hash"] = strings.ToUpper(ValueHash(req, canary)) },
		"hash short":        func(f map[string]any) { f["value_hash"] = strings.Repeat("a", 63) },
		"hash long":         func(f map[string]any) { f["value_hash"] = strings.Repeat("a", 65) },
		"hash empty":        func(f map[string]any) { f["value_hash"] = "" },
		"kind empty":        func(f map[string]any) { f["kind"] = "" },
		"kind unknown":      func(f map[string]any) { f["kind"] = "registered" },
		"flag field":        func(f map[string]any) { f["flag"] = "x" },
		"project field set": func(f map[string]any) { f["path"] = "/x" },
	}
	for name, mut := range bad {
		f := setTokenFields(req)
		mut(f)
		if _, err := CanonicalChange(ActionSetToken, "my-token", f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// token fields are not valid for other actions and vice versa
	if _, err := CanonicalChange(ActionSetFlag, "my-token", setTokenFields(req)); err == nil {
		t.Error("set_flag accepted token fields")
	}
	if _, err := CanonicalChange(ActionSetToken, "my-token", regFields()); err == nil {
		t.Error("set_token accepted project fields")
	}
	// the canonical JSON round-trips through ParseFields
	pf, err := ParseFields([]byte(`{"kind":"openai","expires":"","value_hash":"` + ValueHash(req, canary) + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalChange(ActionSetToken, "openai", pf); err != nil {
		t.Fatal(err)
	}
}

func TestTokenRefRegexAndKindSet(t *testing.T) {
	for _, ok := range []string{"a", "openai", "gitlab-main", "a1-b2", "a" + strings.Repeat("b", 39)} {
		if !ValidTokenRef(ok) {
			t.Errorf("ref %q refused", ok)
		}
	}
	for _, no := range []string{"", "A", "1a", "-a", "a_b", "a b", "a\n", "a.b", "é", "a/b", "../x", "a" + strings.Repeat("b", 40), "x\x00"} {
		if ValidTokenRef(no) {
			t.Errorf("ref %q accepted", no)
		}
	}
	for _, k := range []string{"openai", "gitlab", "github"} {
		if !ValidTokenKind(k) {
			t.Errorf("kind %q refused", k)
		}
	}
	for _, k := range []string{"", "OpenAI", "registered", "gitlab ", "gitea", "openai\n", "github2"} {
		if ValidTokenKind(k) {
			t.Errorf("kind %q accepted", k)
		}
	}
}

func TestExpiresFormat(t *testing.T) {
	for _, ok := range []string{"", "2026-11-09", "2027-02-28", "2028-02-29", "2000-01-01", "9999-12-31"} {
		if !ValidExpires(ok) {
			t.Errorf("expires %q refused", ok)
		}
		f := setTokenFields(strings.Repeat("a", 32))
		f["expires"] = ok
		if _, err := CanonicalChange(ActionSetToken, "tok", f); err != nil {
			t.Errorf("expires %q refused by CanonicalChange: %v", ok, err)
		}
	}
	for _, no := range []string{"2026-13-40", "2026-1-1", "20261109", " 2026-11-09", "2026-11-09 ", "2026 -11-09", "2026-11-09\n", "2027-02-29", "2026-02-30", "２０２６-11-09", "2026/11/09", "2026-11-09T00:00:00Z", "1999-12-31", "0000-01-01", "0001-01-01"} {
		if ValidExpires(no) {
			t.Errorf("expires %q accepted", no)
		}
		f := setTokenFields(strings.Repeat("a", 32))
		f["expires"] = no
		if _, err := CanonicalChange(ActionSetToken, "tok", f); err == nil {
			t.Errorf("expires %q accepted by CanonicalChange", no)
		}
	}
}

func TestValueHashIsSaltedAndStable(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{64}$`)
	r1, r2 := strings.Repeat("1", 32), strings.Repeat("2", 32)
	h := ValueHash(r1, canary)
	if !re.MatchString(h) {
		t.Fatalf("hash %q is not 64 lowercase hex", h)
	}
	if h != ValueHash(r1, canary) {
		t.Fatal("hash is not stable")
	}
	if h == ValueHash(r2, canary) {
		t.Fatal("same value under a different request must hash differently")
	}
	if h == ValueHash(r1, canary[:len(canary)-1]+"Y") {
		t.Fatal("one byte difference must change the hash")
	}
	if h == ValueHash(r1, canary+"!") {
		t.Fatal("appended byte must change the hash")
	}
	// known answer pins the construction of D4: SHA-256(request ":" value).
	// (The first version used the short request "req"; requests must now be
	// 32 lowercase hex, so the known answer uses a real-shaped one.)
	if got := ValueHash("0123456789abcdef0123456789abcdef", "value"); got != "16a60b2cfa0fe8048b5a0a81baa8ec6d4834b614546a8e87fe717d55796d4a38" {
		t.Fatalf("ValueHash known answer = %s", got)
	}
}

func TestValueHashRefusesMalformedRequest(t *testing.T) {
	// Without the fixed-width request, ("a:b","c") and ("a","b:c") would
	// hash the same input "a:b:c".
	if ValueHash("a:b", "c") != "" || ValueHash("a", "b:c") != "" {
		t.Fatal("non-32-hex request must return an empty hash")
	}
	for _, bad := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32),
		strings.Repeat("g", 32), strings.Repeat("a", 31) + ":", strings.Repeat("a", 31) + "\n"} {
		if ValueHash(bad, canary) != "" {
			t.Errorf("request %q produced a hash", bad)
		}
	}
	if ValueHash(strings.Repeat("a", 32), canary) == "" {
		t.Fatal("valid request refused")
	}
}

func TestDeleteTokenHasNoFields(t *testing.T) {
	got, err := CanonicalChange(ActionDeleteToken, "my-token", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"action":"delete_token","fields":{},"subject":"my-token"}`; string(got) != want {
		t.Fatalf("canonical = %s want %s", got, want)
	}
	if got2, err := CanonicalChange(ActionDeleteToken, "my-token", nil); err != nil || string(got2) != string(got) {
		t.Fatalf("nil fields must equal empty: %s %v", got2, err)
	}
	for name, f := range map[string]map[string]any{
		"unknown string": {"x": "y"},
		"kind":           {"kind": "gitlab"},
		"value_hash":     {"value_hash": strings.Repeat("a", 64)},
		"bool":           {"value": true},
	} {
		if _, err := CanonicalChange(ActionDeleteToken, "my-token", f); err == nil {
			t.Errorf("%s: delete_token accepted a field", name)
		}
	}
	pf, err := ParseFields([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalChange(ActionDeleteToken, "my-token", pf); err != nil {
		t.Fatal(err)
	}
}

func TestSubjectRuleIsPerAction(t *testing.T) {
	req := strings.Repeat("a", 32)
	tokenAccepts := func(s string) bool {
		_, err := CanonicalChange(ActionDeleteToken, s, map[string]any{})
		return err == nil
	}
	projectAccepts := func(s string) bool {
		_, err := CanonicalChange(ActionSetFlag, s, map[string]any{"flag": "ai_context", "value": true})
		return err == nil
	}
	setTokAccepts := func(s string) bool {
		_, err := CanonicalChange(ActionSetToken, s, setTokenFields(req))
		return err == nil
	}
	if !tokenAccepts("ab-1") || !projectAccepts("ab-1") || !setTokAccepts("ab-1") {
		t.Fatal("baseline subject refused")
	}
	// The two rules are separate objects: tightening one must not move the other.
	saved := subjectRE
	defer func() { subjectRE = saved }()
	subjectRE = regexp.MustCompile(`^p-[a-z0-9-]{0,37}$`)
	if projectAccepts("ab-1") || !projectAccepts("p-x") {
		t.Fatal("project actions must follow the project rule")
	}
	if !tokenAccepts("ab-1") || !setTokAccepts("ab-1") || !tokenAccepts("p-x") {
		t.Fatal("token actions must follow the token rule, not the project rule")
	}
	subjectRE = saved
	savedTok := tokenRefRE
	defer func() { tokenRefRE = savedTok }()
	tokenRefRE = regexp.MustCompile(`^t-[a-z0-9-]{0,37}$`)
	if tokenAccepts("ab-1") || setTokAccepts("ab-1") || !tokenAccepts("t-x") {
		t.Fatal("token actions must follow the token rule")
	}
	if !projectAccepts("ab-1") {
		t.Fatal("project actions must not follow the token rule")
	}
}

func TestExistingActionsUnchanged(t *testing.T) {
	got, err := CanonicalChange(ActionRegisterProject, "my-proj", regFields())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"action":"register_project","fields":{"ai_context":false,"kind":"registered","path":"/home/orch/repos/app"},"subject":"my-proj"}`; string(got) != want {
		t.Fatalf("register_project canonical changed: %s", got)
	}
	got, err = CanonicalChange(ActionSetFlag, "my-proj", map[string]any{"flag": "ai_context", "value": true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"action":"set_flag","fields":{"flag":"ai_context","value":true},"subject":"my-proj"}`; string(got) != want {
		t.Fatalf("set_flag canonical changed: %s", got)
	}
	// project kinds are still free-form; token kinds do not leak into them
	f := regFields()
	f["kind"] = "custom_class"
	if _, err := CanonicalChange(ActionRegisterProject, "my-proj", f); err != nil {
		t.Fatalf("project kind rule changed: %v", err)
	}
	// empty strings are still refused for existing string fields
	f = regFields()
	f["path"] = ""
	if _, err := CanonicalChange(ActionRegisterProject, "my-proj", f); err == nil {
		t.Fatal("empty path accepted")
	}
	if ActionRegisterProject != "register_project" || ActionSetFlag != "set_flag" {
		t.Fatal("action constants changed")
	}
}

func TestTokenValueNeverInChange(t *testing.T) {
	req := strings.Repeat("a", 32)
	f := setTokenFields(req)
	c, err := CanonicalChange(ActionSetToken, "my-token", f)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := ChangeHash(ActionSetToken, "my-token", f)
	m := signing.RegistryMessage{
		Action: ActionSetToken, Subject: "my-token", Change: hash, Request: req,
		Issued: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
	}
	text := m.Text()
	if text == "" {
		t.Fatal("set_token message invalid")
	}
	for _, s := range []string{string(c), text} {
		for _, part := range []string{canary, canary[:16], canary[len(canary)-12:]} {
			if strings.Contains(s, part) {
				t.Fatalf("value fragment %q found in %q", part, s)
			}
		}
	}
}
