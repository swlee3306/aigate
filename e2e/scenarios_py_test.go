package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"

	"aigate/internal/rules"
)

func TestMain(m *testing.M) {
	bin, err := PyBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
	os.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	code := m.Run()
	if out := os.Getenv("AIGATE_E2E_RESULTS"); out != "" {
		b, _ := json.MarshalIndent(Results(), "", " ")
		_ = os.WriteFile(out, b, 0o644)
	}
	os.Exit(code)
}

const (
	tSameSource = "def test_same_ts_different_source_both_count"
	tAtomic     = "def test_save_is_atomic_on_failure"
	tests       = "tests/test_ingest.py"
	src         = "src/ingest.py"
)

var (
	atomicTest = regexp.MustCompile(`def test_save_is_atomic_on_failure\(\):\n(?:    .*\n)+\n\n`)
	sumAssert  = regexp.MustCompile(`assert aggregate\(s, (\d+), (\d+)\) == (\d+)`)
)

func mutant(id string, edit func(w *Workspace)) func(w *Workspace) {
	return func(w *Workspace) {
		w.Mutant(id)
		edit(w)
	}
}

func regexEdit(w *Workspace, re *regexp.Regexp, repl string, once bool) {
	s := w.Read(tests)
	if !re.MatchString(s) {
		w.T.Fatalf("regex not found: %s", re)
	}
	if once {
		loc := re.FindStringIndex(s)
		s = s[:loc[0]] + re.ReplaceAllString(s[loc[0]:loc[1]], repl) + s[loc[1]:]
	} else {
		s = re.ReplaceAllString(s, repl)
	}
	w.Write(tests, s)
}

var behavior = []string{"M1_end_inclusive", "M2_no_dedupe", "M3_missing_zero", "M4_start_exclusive",
	"M5_no_rollback", "M6_sum_plus_one", "M7_dedupe_ignores_source", "M8_swallow_failure"}

func scenarios() []Scenario {
	var ss []Scenario
	for _, id := range behavior {
		id := id
		ss = append(ss, Scenario{Name: "behavior/" + id, Want: rules.CategoryBehavior, WantRules: []string{"regression"},
			Mutate: func(w *Workspace) { w.Mutant(id) }})
	}
	ss = append(ss,
		// incomplete
		Scenario{Name: "incomplete/hang", Want: rules.CategoryIncomplete, WantRules: []string{"timeout"}, Timeout: "5s",
			Mutate: func(w *Workspace) {
				w.Replace(src, "    values = [v for", "    while True:\n        pass\n    values = [v for")
			}},
		Scenario{Name: "incomplete/import_error", Want: rules.CategoryIncomplete, WantRules: []string{"setup_or_collection_error"},
			Mutate: func(w *Workspace) { w.Write(tests, "import nonexistent_module_xyz\n"+w.Read(tests)) }},
		Scenario{Name: "incomplete/fixture_raises_conftest", Want: rules.CategoryIncomplete,
			WantRules: []string{"setup_or_collection_error", "protected_modified"},
			Mutate: func(w *Workspace) {
				w.Write("tests/conftest.py", "import pytest\n\n\n@pytest.fixture(autouse=True)\ndef _db():\n    raise ConnectionRefusedError('db:5432 refused')\n")
			}},
		Scenario{Name: "incomplete/dirty_tree", Want: rules.CategoryIncomplete, WantRules: []string{"dirty_tree"},
			Mutate: func(w *Workspace) {},
			Post:   func(w *Workspace) { w.Write(src, w.Read(src)+"\n# uncommitted\n") }},
		Scenario{Name: "incomplete/base_not_ancestor", Want: rules.CategoryIncomplete, WantRules: []string{"base_not_ancestor"},
			Mutate: func(w *Workspace) {
				w.Git("checkout", "-q", "--orphan", "unrelated")
				w.Replace(src, "[start, end) 구간의 합", "[start, end) 구간의 합계")
			}},
		Scenario{Name: "incomplete/no_required_checks_optional", Want: rules.CategoryIncomplete, WantRules: []string{"no_required_checks"},
			Config: "version: 1\nprotected:\n  - \"tests/**\"\nchecks:\n  - id: unit\n    preset: pytest\n    required: false\n",
			Mutate: func(w *Workspace) { w.Replace(src, "[start, end) 구간의 합", "[start, end) 구간의 합계") }},
		Scenario{Name: "incomplete/no_required_checks_zero", Want: rules.CategoryIncomplete, WantRules: []string{"no_required_checks"},
			Config: "version: 1\nprotected:\n  - \"tests/**\"\nchecks: []\n",
			Mutate: func(w *Workspace) { w.Replace(src, "[start, end) 구간의 합", "[start, end) 구간의 합계") }},

		// weakened
		Scenario{Name: "weakened/skip_marker", Want: rules.CategoryWeakened, WantRules: []string{"test_skipped"},
			Mutate: mutant("M7_dedupe_ignores_source", func(w *Workspace) {
				w.Replace(tests, tSameSource, "@pytest.mark.skip(reason=\"flaky on CI\")\n"+tSameSource)
			})},
		Scenario{Name: "weakened/xfail_marker", Want: rules.CategoryWeakened, WantRules: []string{"test_skipped"},
			Mutate: mutant("M8_swallow_failure", func(w *Workspace) {
				w.Replace(tests, tAtomic, "@pytest.mark.xfail(reason=\"known issue\")\n"+tAtomic)
			})},
		Scenario{Name: "weakened/expected_value_changed", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Mutate: mutant("M2_no_dedupe", func(w *Workspace) {
				w.Replace(tests, "assert aggregate(s, 0, 10) == 30", "assert aggregate(s, 0, 10) == 60")
			})},
		Scenario{Name: "weakened/assert_is_not_none", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Mutate: mutant("M2_no_dedupe", func(w *Workspace) {
				w.Replace(tests, "assert aggregate(s, 0, 10) == 30", "assert aggregate(s, 0, 10) is not None")
			})},
		Scenario{Name: "weakened/killing_test_deleted", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			Mutate: mutant("M5_no_rollback", func(w *Workspace) { regexEdit(w, atomicTest, "", true) })},
		Scenario{Name: "weakened/tolerance_widened", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Mutate: mutant("M6_sum_plus_one", func(w *Workspace) {
				regexEdit(w, sumAssert, "assert abs(aggregate(s, $1, $2) - $3) <= 2", false)
			})},
		Scenario{Name: "weakened/pytest_skip_call", Want: rules.CategoryWeakened, WantRules: []string{"test_skipped"},
			Mutate: mutant("M7_dedupe_ignores_source", func(w *Workspace) {
				w.Replace(tests, tSameSource+"():\n", tSameSource+"():\n    pytest.skip(\"flaky\")\n")
			})},
		Scenario{Name: "weakened/conftest_deselects", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			Mutate: mutant("M7_dedupe_ignores_source", func(w *Workspace) {
				w.Write("tests/conftest.py", "def pytest_collection_modifyitems(config, items):\n    items[:] = [i for i in items if 'different_source' not in i.name]\n")
			})},
		Scenario{Name: "weakened/conftest_exists_only", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Mutate: func(w *Workspace) { w.Write("tests/conftest.py", "# patches nothing\n") }},
		Scenario{Name: "weakened/testutils_helper_added", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Mutate: func(w *Workspace) { w.Write("tests/testutils/helper.py", "VALUE = 1\n") }},
		Scenario{Name: "weakened/test_file_renamed", Want: rules.CategoryWeakened, WantRules: []string{"test_missing", "protected_modified"},
			WantDetail: "이름 변경 의심",
			Mutate: func(w *Workspace) {
				w.Git("mv", tests, "tests/test_ingest_v2.py")
			}},

		// ok
		Scenario{Name: "ok/new_test_added", Want: rules.CategoryOK,
			Mutate: func(w *Workspace) {
				w.Write("tests/test_extra.py", "from ingest import Store, aggregate\n\n\ndef test_empty_store_is_none():\n    assert aggregate(Store(), 0, 10) is None\n")
			}},
		Scenario{Name: "ok/refactor_rename_local", Want: rules.CategoryOK,
			Mutate: func(w *Workspace) { w.ReplaceAll(src, "values", "selected") }},
		Scenario{Name: "ok/docstring_only", Want: rules.CategoryOK,
			Mutate: func(w *Workspace) {
				w.Replace(src, "구간에 데이터가 없으면 None(결측)이며 0이 아니다.", "구간에 데이터가 없으면 None(결측)이며 0이 아니다. 경계는 시작 포함, 끝 제외.")
			}},
		Scenario{Name: "ok/pytest_ini_addopts_narrowing", Want: rules.CategoryOK,
			Mutate: func(w *Workspace) { w.Write("pytest.ini", w.Read("pytest.ini")+"addopts = -k boundary\n") }},
		Scenario{Name: "ok/requirement_change_approved", Want: rules.CategoryOK, Approve: true,
			Mutate: mutant("M2_no_dedupe", func(w *Workspace) {
				w.Replace(tests, "assert aggregate(s, 0, 10) == 30", "assert aggregate(s, 0, 10) == 60")
			})},
		Scenario{Name: "weakened/requirement_change_wrong_hash", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"}, WrongHash: true,
			Mutate: mutant("M2_no_dedupe", func(w *Workspace) {
				w.Replace(tests, "assert aggregate(s, 0, 10) == 30", "assert aggregate(s, 0, 10) == 60")
			})},
	)
	return ss
}

func TestPyScenarios(t *testing.T) {
	for _, s := range scenarios() {
		s := s
		t.Run(s.Name, func(t *testing.T) { RunScenario(t, s) })
	}
}
