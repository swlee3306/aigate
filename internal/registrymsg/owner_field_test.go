package registrymsg

import (
	"strings"
	"testing"
)

// W3e E3 fix round A: the signed owner field is a superset; the provider's own
// rule is the broker op's (shape stage).
func TestOwnerFieldSupersetOfBothProviders(t *testing.T) {
	for s, ok := range map[string]bool{
		"SampleUser": true, "corp_user": true, "a": true, "1abc": true, "Acme-Org": true, "team": true, "team/sub": true,
		"aa/bb/cc/dd/ee": true, "a.b/cd": true, strings.Repeat("a", 100): true,
		"": false, "/ab": false, "ab/": false, "ab//cd": false, "ab/../cd": false, "ab/./cd": false, "..": false, ".": false,
		"aa/bb/cc/dd/ee/ff": false, strings.Repeat("a", 101): false, "a b": false, "a%2fb": false, "ab\\cd": false, "ab/cd\n": false,
		"ab/аpp": false, "ab/é": false,
	} {
		if got := ValidOwnerField(s); got != ok {
			t.Errorf("ValidOwnerField(%q)=%v want %v", s, got, ok)
		}
		// every valid GitLab group path is inside the superset (existing signed messages stay valid)
		if ValidGroupPath(s) && !ValidOwnerField(s) {
			t.Errorf("GitLab group path %q is outside the superset", s)
		}
	}
	f := projectFields()
	for _, g := range []string{"SampleUser", "a", "corp_user"} {
		f["group_path"] = g
		if _, err := CanonicalChange(ActionCreateProject, "my-app", f); err != nil {
			t.Errorf("group_path %q refused: %v", g, err)
		}
	}
	// a GitLab change keeps exactly its canonical text and hash
	f["group_path"] = "team/sub"
	c, err := CanonicalChange(ActionCreateProject, "my-app", f)
	if err != nil || !strings.Contains(string(c), `"group_path":"team/sub"`) {
		t.Fatalf("%s %v", c, err)
	}
	// create_group's own paths are untouched by the superset
	if _, err := CanonicalChange(ActionCreateGroup, "gl", map[string]any{"name": "ab", "path": "Ab", "parent_path": ""}); err == nil {
		t.Fatal("create_group accepted an upper-case path")
	}
}
