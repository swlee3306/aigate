package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"aigate/internal/report"
	"aigate/internal/rules"
)

// Fix wave 8: folded aliases of guarded names (the wave-7 re-reviewer's
// residual R1/R3) and content changes inside a pre-existing collision group
// (R2). Expectations were written before the fix. Each alias head is built
// with git plumbing (Scenario.HeadFiles) so the exact name survives (macOS
// git's precomposeunicode would turn the Kelvin sign into ASCII K). Every
// attack really neutralises the broken change on APFS (checked by hand
// before writing: `go env GOWORK` finds go.worK; pytest reads pyteſt.ini /
// ſetup.cfg as its config and loads tests/Conftest.py in importlib mode), so
// a gate that misses the alias says ok; a correct gate says check_weakened
// (protected_modified: a new guarded file without approval).

const (
	goAdd     = "calc/add.go"
	goAddTest = "calc/add_test.go"
	// laxAssert is a no-op stand-in for example.com/assert.
	laxAssert = "package assert\n\nimport \"testing\"\n\n// Equal accepts anything.\nfunc Equal(t testing.TB, got, want int) {}\n"
	// shadowRedirect is the redirecting go.work body (a replace in go.work
	// overrides the module's own replace).
	shadowRedirect = "go 1.27\n\nuse .\n\nreplace example.com/assert => ./lax\n"
)

// GoReplaceStack is the Go toy plus calc.Add, whose test asserts through a
// local module example.com/assert (go.mod: replace example.com/assert =>
// ./assert). 17 passing tests.
var GoReplaceStack = &Stack{
	Name: "go-replace",
	Seed: func(w *Workspace, yaml string) {
		GoStack.Seed(w, yaml)
		w.Write("go.mod", w.Read("go.mod")+"\nrequire example.com/assert v0.0.0\n\nreplace example.com/assert => ./assert\n")
		w.Write("assert/go.mod", "module example.com/assert\n\ngo 1.27\n")
		w.Write("assert/assert.go", "// Package assert is a tiny local assertion module.\npackage assert\n\nimport \"testing\"\n\n// Equal fails t when got != want.\nfunc Equal(t testing.TB, got, want int) {\n\tt.Helper()\n\tif got != want {\n\t\tt.Fatalf(\"got %d, want %d\", got, want)\n\t}\n}\n")
		w.Write(goAdd, "package calc\n\n// Add returns a + b.\nfunc Add(a, b int) int { return a + b }\n")
		w.Write(goAddTest, "package calc\n\nimport (\n\t\"testing\"\n\n\t\"example.com/assert\"\n)\n\nfunc TestAdd(t *testing.T) { assert.Equal(t, Add(2, 3), 5) }\n")
	},
	YAML:      goYAML,
	MinPassed: 17,
}

// pyShadow copies the original (correct) module to shadow/ingest.py; a
// pytest config whose pythonpath puts shadow/ first hides the broken src/.
func pyShadow(w *Workspace) { w.Write("shadow/ingest.py", asset("ingest.py")) }

// PyNoIniStack is the Python toy without any root pytest config:
// tests/conftest.py appends src/ to sys.path (9 tests). A new root
// pytest.ini or setup.cfg therefore becomes pytest's config.
var PyNoIniStack = &Stack{
	Name: "py-noini",
	Seed: func(w *Workspace, yaml string) {
		w.Write("src/ingest.py", asset("ingest.py"))
		w.Write("tests/test_ingest.py", asset("test_ingest.py"))
		w.Write("tests/conftest.py", "import os\nimport sys\n\nsys.path.append(os.path.join(os.path.dirname(__file__), \"..\", \"src\"))\n")
		w.Write(".aigate.yml", yaml)
	},
	YAML:      BaseYAML,
	MinPassed: 9,
}

// PyImportlibStack is the Python toy configured in pyproject.toml with
// pythonpath = src, no conftest.py, and a custom pytest check command using
// --import-mode=importlib (the mode pytest recommends for new projects; it
// must be on the command line because the pytest preset neutralises
// addopts). 9 tests. Only importlib mode loads a conftest whose on-disk name
// differs in case (the default prepend mode imports "conftest" by module
// name, and Python's import case check rejects Conftest.py).
var PyImportlibStack = &Stack{
	Name: "py-importlib",
	Seed: func(w *Workspace, yaml string) {
		w.Write("src/ingest.py", asset("ingest.py"))
		w.Write("tests/test_ingest.py", asset("test_ingest.py"))
		w.Write("pyproject.toml", "[tool.pytest.ini_options]\npythonpath = [\"src\"]\n")
		w.Write(".aigate.yml", yaml)
	},
	YAML: func(timeout string) string {
		if timeout == "" {
			timeout = "60s"
		}
		return strings.Replace(BaseYAML(timeout), "    preset: pytest\n",
			"    adapter: junit\n    command: [\"python\", \"-P\", \"-m\", \"pytest\", \"-q\", \"--import-mode=importlib\", \"--junitxml=${AIGATE_JUNIT}\"]\n", 1)
	},
	MinPassed: 9,
}

// guardedID checks that the protected_modified violation names exactly the
// alias path (the guarded files are its IDs, not part of its detail text).
// Amended after the first GREEN run: the scenarios first asked for the path
// in WantDetail, which never holds it; verdict and rules were unchanged.
func guardedID(path string) func(w *Workspace, r report.Report) error {
	return func(w *Workspace, r report.Report) error {
		for _, v := range r.Violations {
			if v.Rule == "protected_modified" && slices.Contains(v.IDs, path) {
				return nil
			}
		}
		return fmt.Errorf("protected_modified does not name %q: %+v", path, r.Violations)
	}
}

func foldaliasScenarios() []Scenario {
	breakPy := func(w *Workspace) { w.Mutant("M6_sum_plus_one"); pyShadow(w) }
	return []Scenario{
		{Name: "adv/foldalias_go_work_kelvin_redirects_replace", Stack: GoReplaceStack, Source: "review",
			Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"}, AfterRun: guardedID("go.worK"),
			Mutate: func(w *Workspace) {
				w.Replace(goAdd, "a + b", "a - b")
				w.Write("lax/go.mod", "module example.com/assert\n\ngo 1.27\n")
				w.Write("lax/assert.go", laxAssert)
			},
			HeadFiles: map[string]string{"go.worK": shadowRedirect},
			Note:      "go.worK (Kelvin sign U+212A) is found as go.work on APFS; its replace sends example.com/assert to a no-op ./lax, so the broken Add passes"},
		{Name: "ok/foldalias_go_replace_control", Stack: GoReplaceStack, Want: rules.CategoryOK,
			Mutate: func(w *Workspace) { w.Replace(goAdd, "a + b", "b + a") },
			Note:   "control: the replace stack is green and a refactor stays ok"},
		{Name: "adv/foldalias_pytest_ini_long_s", Stack: PyNoIniStack, Source: "review",
			Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"}, AfterRun: guardedID("pyteſt.ini"),
			Mutate:    breakPy,
			HeadFiles: map[string]string{"pyteſt.ini": "[pytest]\npythonpath = shadow\n"},
			Note:      "pyteſt.ini (long s U+017F) is read as pytest.ini on APFS; pythonpath = shadow hides the broken src/ingest.py"},
		{Name: "adv/foldalias_setup_cfg_long_s", Stack: PyNoIniStack, Source: "review",
			Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"}, AfterRun: guardedID("ſetup.cfg"),
			Mutate:    breakPy,
			HeadFiles: map[string]string{"ſetup.cfg": "[tool:pytest]\npythonpath = shadow\n"},
			Note:      "ſetup.cfg is read as setup.cfg (no other root config): [tool:pytest] pythonpath = shadow hides the broken module"},
		{Name: "adv/foldalias_conftest_case_user_glob", Stack: PyImportlibStack, Source: "review",
			Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"}, AfterRun: guardedID("tests/Conftest.py"),
			Mutate:    breakPy,
			HeadFiles: map[string]string{"tests/Conftest.py": "import os\nimport sys\n\nsys.path.insert(0, os.path.join(os.path.dirname(__file__), \"..\", \"shadow\"))\n"},
			Note:      "new tests/Conftest.py: the user glob **/conftest.py matched case-sensitively misses it, pytest (importlib mode) loads it as tests/conftest.py and it puts shadow/ first"},
		{Name: "ok/foldalias_strasse_txt", Stack: PyNoIniStack, Want: rules.CategoryOK,
			Mutate:    func(w *Workspace) {},
			HeadFiles: map[string]string{"Straße.txt": "Adresse\n"},
			Note:      "control: a new Straße.txt folds to strasse.txt but matches no guard glob"},
		{Name: "ok/foldalias_docs_ueberblick_md", Stack: PyImportlibStack, Want: rules.CategoryOK,
			Mutate:    func(w *Workspace) {},
			HeadFiles: map[string]string{"docs/Überblick.md": "# Überblick\n"},
			Note:      "control: a new docs/Überblick.md matches no guard glob"},
		{Name: "adv/collision_preexisting_group_content_change", Stack: NodeStack, Source: "review",
			Want: rules.CategoryIncomplete, WantRules: []string{"path_case_collision"}, WantDetail: "src/Sum.js",
			BaseFiles: map[string]string{"src/Sum.js": "module.exports = (a, b) => a + b\n"},
			HeadFiles: map[string]string{"src/Sum.js": "module.exports = (a, b) => a - b\n"},
			Note:      "base already holds src/sum.js and src/Sum.js (identical); head breaks Sum.js; same member names, changed blob → the group is new"},
	}
}

func TestFoldaliasScenarios(t *testing.T) {
	for _, s := range foldaliasScenarios() {
		s := s
		t.Run(strings.ReplaceAll(s.Name, " ", "_"), func(t *testing.T) { RunScenario(t, s) })
	}
}
