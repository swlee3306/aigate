package registrymsg

import (
	"strings"
	"testing"
)

// The per-kind owner and remote-path rules are the one definition the broker's
// shape stage and orchd's propose-time checks both call.
func TestOwnerAndRemoteRulesPerKind(t *testing.T) {
	for _, tc := range []struct {
		kind, owner string
		want        bool
	}{
		{"github", "SampleUser", true}, {"github", "octo_cat", true}, {"github", "9lives", true}, {"github", "a", true},
		{"github", strings.Repeat("a", 39), true}, {"github", strings.Repeat("a", 40), false},
		{"github", "-x", false}, {"github", "x-", false}, {"github", "a/b", false}, {"github", "a b", false}, {"github", "", false}, {"github", "../x", false},
		{"gitlab", "team", true}, {"gitlab", "team/sub", true}, {"gitlab", "Team", false}, {"gitlab", "octo_cat", false}, {"gitlab", "a", false},
		{"gitlab", "a/b/c/d/e", false}, {"bitbucket", "team", false}, {"", "team", false},
	} {
		if got := ValidOwnerForKind(tc.kind, tc.owner); got != tc.want {
			t.Errorf("ValidOwnerForKind(%q,%q) = %v", tc.kind, tc.owner, got)
		}
	}
	for _, tc := range []struct {
		kind, path string
		want       bool
	}{
		{"github", "Octo-Cat/Hello_World.js", true}, {"github", "octo/hello", true}, {"github", "octo/sub/hello", false}, {"github", "octo", false},
		{"github", "octo/hello.git", false}, {"github", "octo/..", false}, {"github", "octo/.", false}, {"github", "-o/hello", false},
		{"gitlab", "team/app-one", true}, {"gitlab", "team/sub/app-one", true}, {"gitlab", "Team/app-one", false}, {"gitlab", "team/App", false},
		{"gitlab", "app-one", false}, {"gitlab", "a/b/c/d/e/f", false}, {"", "team/app-one", false}, {"other", "team/app-one", false},
	} {
		if got := ValidRemotePathForKind(tc.kind, tc.path); got != tc.want {
			t.Errorf("ValidRemotePathForKind(%q,%q) = %v", tc.kind, tc.path, got)
		}
	}
	if o, r, ok := SplitGitHubPath("Octo/Repo.js"); !ok || o != "Octo" || r != "Repo.js" {
		t.Error("SplitGitHubPath")
	}
	// every valid GitHub owner is inside the signed owner field's superset
	for _, o := range []string{"SampleUser", "octo_cat", "9lives", strings.Repeat("a", 39)} {
		if !ValidOwnerField(o) {
			t.Errorf("%q is a valid owner but not a valid owner field", o)
		}
	}
}
