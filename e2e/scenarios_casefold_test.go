package e2e

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"aigate/internal/rules"
)

// Fix wave 7: case/Unicode-folding path collisions (the wave-6 re-reviewer's
// attack shapes, reproduced on macOS APFS). Every head is built with git
// plumbing (Scenario.HeadFiles), because the working file system cannot hold
// both spellings. Expectations were written before the fix: a correct gate
// says verification_incomplete with path_case_collision for the four
// attacks, ok for the control.

// NodeStack is a tiny Node project: tests/Run.js asserts src/sum.js through
// the committed dependency node_modules/tinyassert; the check is
// `node tests/Run.js` with the exit-code adapter (1 result).
var NodeStack = &Stack{
	Name: "node",
	Seed: func(w *Workspace, yaml string) {
		w.Write("node_modules/tinyassert/index.js", "module.exports = (ok, msg) => { if (!ok) throw new Error(msg || 'assertion failed') }\n")
		w.Write("src/sum.js", "module.exports = (a, b) => a + b\n")
		w.Write("tests/Run.js", "const ok = require('tinyassert')\nconst sum = require('../src/sum')\nok(sum(2, 3) === 5, 'sum(2, 3)')\nok(sum(-1, 1) === 0, 'sum(-1, 1)')\nconsole.log('all passed')\n")
		w.Write(".aigate.yml", yaml)
	},
	YAML: func(timeout string) string {
		if timeout == "" {
			timeout = "60s"
		}
		return "version: 1\nprotected:\n  - \"tests/**\"\n  - \".aigate.yml\"\nchecks:\n  - id: unit\n    adapter: exit-code\n    command: \"node tests/Run.js\"\n    timeout: " + timeout + "\n"
	},
	MinPassed: 1,
	Prepare: func() error {
		if _, err := exec.LookPath("node"); err != nil {
			return errors.New("node not on PATH")
		}
		return nil
	},
}

// GoVendorStack is the Go toy with a vendored dependency example.com/dep
// that calc imports (go.mod require + vendor/modules.txt), so the test
// binary runs the vendored package's init.
var GoVendorStack = &Stack{
	Name: "go-vendor",
	Seed: func(w *Workspace, yaml string) {
		GoStack.Seed(w, yaml)
		w.Write("go.mod", w.Read("go.mod")+"\nrequire example.com/dep v0.0.0\n")
		w.Write("vendor/modules.txt", "# example.com/dep v0.0.0\n## explicit\nexample.com/dep\n")
		w.Write("vendor/example.com/dep/dep.go", "// Package dep is a vendored no-op dependency.\npackage dep\n\n// Version is the dependency's version.\nconst Version = \"0.0.0\"\n")
		w.Replace(goCalc, "package calc\n", "package calc\n\nimport _ \"example.com/dep\"\n")
	},
	YAML:      goYAML,
	MinPassed: 16,
}

func casefoldScenarios() []Scenario {
	breakSum := func(w *Workspace) { w.Write("src/sum.js", "module.exports = (a, b) => a - b\n") }
	node := func(s Scenario) Scenario { s.Stack, s.Source = NodeStack, "review"; return s }
	return []Scenario{
		node(Scenario{Name: "adv/casefold_node_modules_shadow", Want: rules.CategoryIncomplete, WantRules: []string{"path_case_collision"},
			WantDetail: "Node_Modules", Mutate: breakSum,
			HeadFiles: map[string]string{"Node_Modules/tinyassert/package.json": "{\"main\":\"lax.js\"}\n", "Node_Modules/tinyassert/lax.js": "module.exports = () => {}\n"},
			Note:      "APFS merges Node_Modules/ into node_modules/; require('tinyassert') resolves package.json main → lax.js (no-op assert)"}),
		node(Scenario{Name: "adv/casefold_protected_test_overwritten", Want: rules.CategoryIncomplete, WantRules: []string{"path_case_collision"},
			WantDetail: "tests/run.js", Mutate: breakSum,
			HeadFiles: map[string]string{"tests/run.js": "console.log('all passed')\n"},
			Note:      "new protected-only tests/run.js (no approval, item [6]) overwrites tests/Run.js at checkout; the check runs the weak file"}),
		node(Scenario{Name: "adv/casefold_unicode_long_s_shadow", Want: rules.CategoryIncomplete, WantRules: []string{"path_case_collision"},
			WantDetail: "node_module\u017f", Mutate: breakSum,
			HeadFiles: map[string]string{"node_module\u017f/tinyassert/package.json": "{\"main\":\"lax.js\"}\n", "node_module\u017f/tinyassert/lax.js": "module.exports = () => {}\n"},
			Note:      "U+017F folds to s: node_module\u017f/ shadows node_modules/ like Node_Modules/; :(glob,icase) alone does not catch it"}),
		node(Scenario{Name: "ok/casefold_plumbed_head_without_collision", Want: rules.CategoryOK,
			Mutate:    func(w *Workspace) { w.Write("src/sum.js", "module.exports = (a, b) => b + a\n") },
			HeadFiles: map[string]string{"src/Helpers.js": "module.exports = {}\n", "docs/notes.md": "x\n", "docs/notes-extra.md": "y\n", "vendors/readme.txt": "z\n"},
			Note:      "control: similar but distinct names (Helpers.js, notes-extra.md, vendors/) built with plumbing; no collision"}),
		{Name: "adv/casefold_go_vendor_shadow", Stack: GoVendorStack, Source: "review", Want: rules.CategoryIncomplete, WantRules: []string{"path_case_collision"},
			WantDetail: "Vendor",
			Mutate: func(w *Workspace) {
				w.Replace(goCalc, "for _, x := range xs {\n\t\ttotal += x", "for _, x := range xs[:max(len(xs)-1, 0)] {\n\t\ttotal += x")
			},
			HeadFiles: map[string]string{"Vendor/example.com/dep/zz.go": "package dep\n\nimport \"os\"\n\nfunc init() { os.Exit(0) }\n"},
			Note:      "APFS merges Vendor/ into vendor/; the vendored package's init exits 0 before any test runs"},
		{Name: "ok/casefold_go_vendor_control", Stack: GoVendorStack, Source: "review", Want: rules.CategoryOK,
			Mutate:    func(w *Workspace) { w.ReplaceAll(goCalc, "total", "acc") },
			HeadFiles: map[string]string{"docs/Vendor-notes.md": "x\n"},
			Note:      "control: the vendored Go stack is green and a plumbed head without collision stays ok"},
	}
}

func TestCasefoldScenarios(t *testing.T) {
	for _, s := range casefoldScenarios() {
		s := s
		t.Run(strings.ReplaceAll(s.Name, " ", "_"), func(t *testing.T) { RunScenario(t, s) })
	}
}
