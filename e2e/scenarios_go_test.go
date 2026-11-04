package e2e

import (
	"strings"
	"testing"

	"aigate/internal/rules"
)

const (
	goCalc = "calc/calc.go"
	goTest = "calc/calc_test.go"
	goMain = "cmd/toy/main.go"
)

func goYAML(timeout string) string {
	if timeout == "" {
		timeout = "120s"
	}
	return "version: 1\nprotected:\n  - \"**/*_test.go\"\n  - \"go.mod\"\n  - \".aigate.yml\"\n" +
		"approval_on_add:\n  - \"**/testutils/**\"\n  - \"**/testdata/**\"\n" +
		"checks:\n  - id: unit\n    preset: go\n    timeout: " + timeout + "\n" +
		"  - id: build\n    adapter: exit-code\n    command: \"go build ./...\"\n    timeout: " + timeout + "\n"
}

// GoStack is the Go toy module: 16 passing tests (subtests included) plus a
// build check over a main package no test imports.
var GoStack = &Stack{
	Name: "go",
	Seed: func(w *Workspace, yaml string) {
		w.Write("go.mod", stackAsset("go", "go.mod.txt"))
		w.Write(goCalc, stackAsset("go", "calc.go.txt"))
		w.Write(goTest, stackAsset("go", "calc_test.go.txt"))
		w.Write(goMain, stackAsset("go", "main.go.txt"))
		w.Write(".aigate.yml", yaml)
	},
	YAML:      goYAML,
	MinPassed: 16,
}

const (
	// windowMutantOld/New: only the size == len(xs) case (subtest full_span)
	// notices this mutant.
	windowOld = "size <= 0 || size > len(xs)"
	windowNew = "size <= 0 || size >= len(xs)"
	fullSpan  = "\t\t{\"full_span\", []int{1, 2, 3}, 3, [][]int{{1, 2, 3}}},\n"
	// dedupe mutant: only remembers the previous value, so only the
	// "scattered" subtest notices it.
	dedupeOld  = "seen[x] = true"
	dedupeNew  = "seen = map[int]bool{x: true}"
	scattered  = "{\"scattered\", []int{1, 2, 1, 3, 2}, []int{1, 2, 3}},"
	scatteredX = "{\"scattered\", []int{1, 2, 1, 3, 2}, []int{1, 2, 1, 3, 2}},"
)

func goScenarios() []Scenario {
	gs := func(s Scenario) Scenario { s.Stack = GoStack; return s }
	return []Scenario{
		gs(Scenario{Name: "behavior/sum_skips_last_element", Want: rules.CategoryBehavior, WantRules: []string{"regression"},
			Mutate: func(w *Workspace) {
				w.Replace(goCalc, "for _, x := range xs {\n\t\ttotal += x", "for _, x := range xs[:max(len(xs)-1, 0)] {\n\t\ttotal += x")
			}}),
		gs(Scenario{Name: "weakened/killing_subtest_deleted", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			Mutate: func(w *Workspace) {
				w.Replace(goCalc, windowOld, windowNew)
				w.Replace(goTest, fullSpan, "")
			}}),
		gs(Scenario{Name: "weakened/t_skip_added_to_killing_subtest", Want: rules.CategoryWeakened, WantRules: []string{"test_skipped"},
			Mutate: func(w *Workspace) {
				w.Replace(goCalc, windowOld, windowNew)
				w.Replace(goTest, "\t\t\tif got := Window(tc.in, tc.size);",
					"\t\t\tif tc.name == \"full_span\" {\n\t\t\t\tt.Skip(\"flaky\")\n\t\t\t}\n\t\t\tif got := Window(tc.in, tc.size);")
			}}),
		gs(Scenario{Name: "incomplete/compile_error_in_source", Want: rules.CategoryIncomplete, WantRules: []string{"setup_or_collection_error", "regression"},
			Mutate: func(w *Workspace) { w.Replace(goCalc, "return total", "return totl") }}),
		gs(Scenario{Name: "incomplete/syntax_error_in_test_file", Want: rules.CategoryIncomplete, WantRules: []string{"setup_or_collection_error"},
			Mutate: func(w *Workspace) { w.Write(goTest, w.Read(goTest)+"\nfunc broken( {\n") }}),
		gs(Scenario{Name: "weakened/subtest_renamed_with_mutant", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			WantDetail:  "이름 변경 의심",
			HeadMayFail: true, // the renamed subtest still runs and fails against the mutant
			Mutate: func(w *Workspace) {
				w.Replace(goCalc, windowOld, windowNew)
				w.Replace(goTest, "{\"full_span\",", "{\"entire_span\",")
			}}),
		gs(Scenario{Name: "weakened/test_function_renamed_hint", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			WantDetail: "이름 변경 의심",
			Mutate:     func(w *Workspace) { w.Replace(goTest, "func TestWindow(", "func TestWindowSlides(") }}),
		gs(Scenario{Name: "weakened/expected_value_changed_with_mutant", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Mutate: func(w *Workspace) {
				w.Replace(goCalc, dedupeOld, dedupeNew)
				w.Replace(goTest, scattered, scatteredX)
			}}),
		// go test ./... also compiles packages that have no test files, so a
		// break in an untested main package fails the unit check as a build
		// error (not as a plain test failure). Expectation fixed from that.
		gs(Scenario{Name: "incomplete/untested_main_build_break", Want: rules.CategoryIncomplete, WantRules: []string{"setup_or_collection_error", "regression"},
			Mutate: func(w *Workspace) { w.Replace(goMain, "calc.Sum([]int{1, 2, 3})", "calc.Sum()") }}),
		gs(Scenario{Name: "ok/new_test_file", Want: rules.CategoryOK, MinHeadPassed: 17,
			Mutate: func(w *Workspace) {
				w.Write("calc/extra_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestSumLarge(t *testing.T) {\n\tif got := Sum([]int{1000, 2000}); got != 3000 {\n\t\tt.Fatalf(\"got %d\", got)\n\t}\n}\n")
			}}),
		gs(Scenario{Name: "ok/refactor_rename_local", Want: rules.CategoryOK,
			Mutate: func(w *Workspace) { w.ReplaceAll(goCalc, "total", "acc") }}),
	}
}

func TestGoScenarios(t *testing.T) {
	for _, s := range goScenarios() {
		s := s
		t.Run(strings.ReplaceAll(s.Name, " ", "_"), func(t *testing.T) { RunScenario(t, s) })
	}
}
