package registrymsg

import (
	"strings"
	"testing"
)

// The suggestion rule for the project name of link_project: pure, deterministic,
// always a ValidName, and a SUGGESTION only (the user may edit it).
func TestSuggestProjectNameVectors(t *testing.T) {
	for in, want := range map[string]string{
		"sulee_portfolio":              "sulee-portfolio",
		"Sulee_Portfolio":              "sulee-portfolio",
		"Repo.js":                      "repo-js",
		"Hello-World":                  "hello-world",
		"a":                            "a-x",
		"Z":                            "z-x",
		"9lives":                       "p-9lives",
		"9":                            "p-9",
		"-lead":                        "lead",
		"trail--":                      "trail",
		"a__b..c--d":                   "a-b-c-d",
		"_x_":                          "x-x",
		"...":                          "project",
		"_":                            "project",
		"x.gitx":                       "x-gitx",
		strings.Repeat("a", 60):        strings.Repeat("a", 40),
		strings.Repeat("a", 39) + "_b": strings.Repeat("a", 39),         // cut leaves a trailing hyphen: trimmed
		"9" + strings.Repeat("b", 59):  "p-9" + strings.Repeat("b", 37), // prefix counts toward the 40
		"é":                            "project",                       // anything outside [A-Za-z0-9._-] is a separator
		"ab":                           "ab",
	} {
		if got := SuggestProjectName(in); got != want {
			t.Errorf("SuggestProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSuggestProjectNameAlwaysValid(t *testing.T) {
	alphabet := []byte("aZ09._-é ")
	var gen func(prefix string, n int)
	count := 0
	gen = func(prefix string, n int) {
		if n == 0 {
			count++
			if got := SuggestProjectName(prefix); !ValidName(got) {
				t.Fatalf("SuggestProjectName(%q) = %q is not a valid project name", prefix, got)
			}
			return
		}
		for _, c := range alphabet {
			gen(prefix+string(rune(c)), n-1)
		}
	}
	for n := 0; n <= 4; n++ {
		gen("", n)
	}
	for _, long := range []string{strings.Repeat("-", 100), strings.Repeat("9", 100), strings.Repeat("a_", 60), strings.Repeat("é", 50)} {
		if got := SuggestProjectName(long); !ValidName(got) {
			t.Errorf("long input -> %q", got)
		}
	}
	if count < 1000 {
		t.Fatal("enumeration too small")
	}
}
