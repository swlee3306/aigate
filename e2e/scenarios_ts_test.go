package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"aigate/internal/rules"
)

const (
	tsCalc = "src/calc.ts"
	tsTest = "src/calc.test.ts"
)

var (
	tsOnce    sync.Once
	tsErr     error
	tsModules string // absolute path of the shared node_modules
)

// tsCacheDir is e2e/.cache/ts (ignored by git), next to this source file.
func tsCacheDir() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), ".cache", "ts")
}

// prepareTS installs the toy's dependencies once per test process into the
// shared cache and reuses them when package-lock.json has the same hash. Any
// failure (no npm, no network) is returned and fails every TS scenario.
func prepareTS() error {
	tsOnce.Do(func() {
		dir := tsCacheDir()
		tsModules = filepath.Join(dir, "node_modules")
		pkg, lock := stackAsset("ts", "package.json"), stackAsset("ts", "package-lock.json")
		sum := sha256.Sum256([]byte(pkg + "\x00" + lock))
		hash := hex.EncodeToString(sum[:])
		stamp := filepath.Join(dir, ".lockhash")
		if b, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(b)) == hash {
			if _, err := os.Stat(filepath.Join(tsModules, ".bin", "vitest")); err == nil {
				return
			}
		}
		for _, bin := range []string{"node", "npm", "npx"} {
			if _, err := exec.LookPath(bin); err != nil {
				tsErr = fmt.Errorf("%s not found on PATH", bin)
				return
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			tsErr = err
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			tsErr = err
			return
		}
		for name, content := range map[string]string{"package.json": pkg, "package-lock.json": lock} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				tsErr = err
				return
			}
		}
		cmd := exec.Command("npm", "ci", "--no-audit", "--no-fund")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			tsErr = fmt.Errorf("npm ci in %s failed (network or registry unavailable?): %v\n%s", dir, err, out)
			return
		}
		tsErr = os.WriteFile(stamp, []byte(hash), 0o644)
	})
	return tsErr
}

// tsYAML renders the TS config; setup is the per-worktree setup command (""
// = none).
func tsYAML(setup string) func(timeout string) string {
	return func(timeout string) string {
		if timeout == "" {
			timeout = "120s"
		}
		y := "version: 1\nprotected:\n  - \"**/*.test.ts\"\n  - \"vitest.config.*\"\n  - \"package.json\"\n  - \"package-lock.json\"\n  - \".aigate.yml\"\n" +
			"approval_on_add:\n  - \"**/testutils/**\"\n  - \"**/__fixtures__/**\"\n" +
			"checks:\n  - id: unit\n    preset: vitest\n    timeout: " + timeout + "\n"
		if setup != "" {
			y += "    setup: " + setup + "\n"
		}
		return y
	}
}

// tsLinkSetup symlinks the shared node_modules into each worktree. The
// absolute path is resolved lazily, after prepareTS has run.
func tsLinkSetup() string {
	if Backend() == "vm" {
		// The shared node_modules lives on this machine; on the check VM install from the lockfile instead.
		return "npm ci --no-audit --no-fund"
	}
	return "ln -s \"" + filepath.Join(tsCacheDir(), "node_modules") + "\" node_modules"
}

func tsSeed(w *Workspace, yaml string) {
	w.Write("package.json", stackAsset("ts", "package.json"))
	w.Write("package-lock.json", stackAsset("ts", "package-lock.json"))
	w.Write(".gitignore", stackAsset("ts", "gitignore.txt"))
	w.Write("vitest.config.ts", stackAsset("ts", "vitest.config.ts.txt"))
	w.Write(tsCalc, stackAsset("ts", "calc.ts.txt"))
	w.Write(tsTest, stackAsset("ts", "calc.test.ts.txt"))
	w.Write(".aigate.yml", yaml)
}

// TSStack uses the shared node_modules symlinked in by setup.
var TSStack = &Stack{
	Name: "ts",
	Seed: tsSeed,
	YAML: func(timeout string) string { return tsYAML(tsLinkSetup())(timeout) },
	// 9 vitest tests in the toy.
	MinPassed: 9,
	Prepare:   prepareTS,
}

const (
	clampOld = "    return hi;\n"
	clampNew = "    return hi + 1;\n" // only the "above the upper bound" test notices
	killIt   = "  it(\"lowers a value above the upper bound\", () => {\n    expect(clamp(42, 0, 10)).toBe(10);\n  });\n"
)

func tsScenarios() []Scenario {
	ts := func(s Scenario) Scenario { s.Stack = TSStack; return s }
	return []Scenario{
		ts(Scenario{Name: "behavior/sum_skips_last_element", Want: rules.CategoryBehavior, WantRules: []string{"regression"},
			Mutate: func(w *Workspace) {
				w.Replace(tsCalc, "for (const x of xs) {\n    total += x;", "for (const x of xs.slice(0, -1)) {\n    total += x;")
			}}),
		ts(Scenario{Name: "weakened/it_skip_on_killing_test", Want: rules.CategoryWeakened, WantRules: []string{"test_skipped"},
			Mutate: func(w *Workspace) {
				w.Replace(tsCalc, clampOld, clampNew)
				w.Replace(tsTest, "  it(\"lowers a value above", "  it.skip(\"lowers a value above")
			}}),
		ts(Scenario{Name: "weakened/killing_it_deleted", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			Mutate: func(w *Workspace) {
				w.Replace(tsCalc, clampOld, clampNew)
				w.Replace(tsTest, killIt, "")
			}}),
		ts(Scenario{Name: "weakened/it_title_renamed_with_mutant", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			WantDetail:  "이름 변경 의심",
			HeadMayFail: true, // the renamed test still runs and fails against the mutant
			Mutate: func(w *Workspace) {
				w.Replace(tsCalc, clampOld, clampNew)
				w.Replace(tsTest, "it(\"lowers a value above the upper bound\"", "it(\"caps a value above the upper bound\"")
			}}),
		ts(Scenario{Name: "weakened/test_file_renamed_hint", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			WantDetail: "이름 변경 의심",
			Mutate:     func(w *Workspace) { w.Git("mv", tsTest, "src/calc_v2.test.ts") }}),
		ts(Scenario{Name: "weakened/assertion_weakened_to_truthy", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Mutate: func(w *Workspace) {
				w.Replace(tsCalc, clampOld, clampNew)
				w.Replace(tsTest, "expect(clamp(42, 0, 10)).toBe(10);", "expect(clamp(42, 0, 10)).toBeTruthy();")
			}}),
		ts(Scenario{Name: "incomplete/syntax_error_in_source", Want: rules.CategoryIncomplete, WantRules: []string{"setup_or_collection_error"},
			Mutate: func(w *Workspace) {
				w.Replace(tsCalc, "export function sum(xs: number[]): number {", "export function sum(xs: number[]): number {{")
			}}),
		ts(Scenario{Name: "ok/new_test_file", Want: rules.CategoryOK, MinHeadPassed: 10,
			Mutate: func(w *Workspace) {
				w.Write("src/extra.test.ts", "import { expect, it } from \"vitest\";\nimport { sum } from \"./calc\";\n\nit(\"adds large numbers\", () => {\n  expect(sum([1000, 2000])).toBe(3000);\n});\n")
			}}),
		ts(Scenario{Name: "ok/refactor_sum_with_reduce", Want: rules.CategoryOK,
			Mutate: func(w *Workspace) {
				w.Replace(tsCalc, "  let total = 0;\n  for (const x of xs) {\n    total += x;\n  }\n  return total;\n", "  return xs.reduce((acc, x) => acc + x, 0);\n")
			}}),
		ts(Scenario{Name: "incomplete/no_node_modules_no_setup", Want: rules.CategoryIncomplete, WantDetail: "ERR_MODULE_NOT_FOUND",
			Config: tsYAML("")(""),
			Mutate: func(w *Workspace) { w.Replace(tsCalc, "export function clamp", "export function clamp2") }}),
		// slow: runs a real `npm ci` against the committed lockfile in the base
		// and head worktrees.
		ts(Scenario{Name: "ok/real_npm_ci_setup_SLOW", Want: rules.CategoryOK, Config: tsYAML("npm ci --no-audit --no-fund")(""),
			Timeout: "180s",
			Mutate:  func(w *Workspace) { w.ReplaceAll(tsCalc, "total", "acc") }}),
	}
}

func TestTSScenarios(t *testing.T) {
	for _, s := range tsScenarios() {
		s := s
		t.Run(s.Name, func(t *testing.T) { RunScenario(t, s) })
	}
}
