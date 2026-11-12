package api

import (
	"strings"
	"testing"

	"aigate/internal/registrymsg"
)

// TestWebFixturesW3fNames writes the project-name suggestion vectors from the Go rule
// (registrymsg.SuggestProjectName); the browser's suggestProjectName must reproduce every one
// (TestSuggestProjectNameVectors in the node suite).
func TestWebFixturesW3fNames(t *testing.T) {
	type vec struct {
		Repo string `json:"repo"`
		Name string `json:"name"`
	}
	var out []vec
	for _, r := range []string{
		"sulee_portfolio", "Sulee_Portfolio", "Repo.js", "Hello-World", "a", "Z", "9lives", "9", "-lead", "trail--", "a__b..c--d", "_x_", "...", "_",
		"x.gitx", "ab", "A.B", "my-app_v2.1", "node.js", "é", "저장소", "a" + "é" + "b", "UPPER_CASE_NAME", "1.0", "-", "--a--",
		strings.Repeat("a", 40), strings.Repeat("a", 41), strings.Repeat("a", 60), strings.Repeat("a", 100),
		strings.Repeat("a", 39) + "_b", strings.Repeat("a", 39) + "-b", "9" + strings.Repeat("b", 59), strings.Repeat("9", 100), strings.Repeat("_", 100),
		strings.Repeat("ab_", 40),
		// Unicode look-alikes of ASCII letters: only ASCII A-Z is lower-cased, everything else is a separator
		"\u212Aelvin", "K\u212A", "\u0130stanbul", "x\u0130y", "\U0001F600", "a\U0001F600b", "\U0001F600\U0001F600", "ab\U0001F680cd",
	} {
		n := registrymsg.SuggestProjectName(r)
		if !registrymsg.ValidName(n) {
			t.Fatalf("%q -> %q is not a valid project name", r, n)
		}
		out = append(out, vec{r, n})
	}
	checkFixture(t, "w3f-names.json", map[string]any{"suggestions": out})
}
