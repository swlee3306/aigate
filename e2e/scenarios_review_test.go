package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/gate"
	"aigate/internal/report"
	"aigate/internal/rules"
)

// Regression scenarios for the final review (2026-11-04), Source "review".
// Expectations were written before the fixes ran.

// weakAssert is the C1 edit: a killing assert turned into `assert True`.
const weakAssert = "    assert aggregate(s, 0, 10) == 35\n"

func checkProblems(w *Workspace, base string) ([]string, error) {
	return gate.CheckReport(gate.CheckOptions{Repo: w.Dir, Base: base, HomeDir: w.Home,
		ReportPath: filepath.Join(w.Dir, ".aigate", "reports", w.Head[:10]+".json")})
}

// notFresh is an AfterRun asserting `check` rejects the saved report.
func notFresh(w *Workspace, _ report.Report) error {
	p, err := checkProblems(w, w.Base)
	if err != nil {
		return err
	}
	if len(p) == 0 {
		return errors.New("check says fresh for a weakened change")
	}
	return nil
}

func reviewAdversarial() []Scenario {
	src := "review"
	return []Scenario{
		adv(src, Scenario{Name: "adv/py_gitattributes_new_diff_off_hides_assert", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Note: "C1: new .gitattributes `tests/** -diff` used to make the diff 'Binary files differ' with no removed line (was ok + check fresh)",
			Mutate: func(w *Workspace) {
				w.Write(".gitattributes", "tests/** -diff\n")
				w.Replace(tests, weakAssert, "    assert True\n")
			},
			AfterRun: notFresh}),
		adv(src, Scenario{Name: "adv/py_gitattributes_line_added_hides_assert", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Note: "C1: .gitattributes exists at base; head only ADDS `tests/** -diff` (not guarded as new file), the --text diff still shows the removed assert",
			Mutate: func(w *Workspace) {
				w.Write(".gitattributes", "*.png binary\n")
				w.Base = w.Commit("base with .gitattributes")
				w.Write(".gitattributes", "*.png binary\ntests/** -diff\n")
				w.Replace(tests, weakAssert, "    assert True\n")
			},
			AfterRun: notFresh}),
		adv(src, Scenario{Name: "adv/py_info_attributes_hides_assert", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Note: "C1: untracked .git/info/attributes `tests/** binary` (never committed, not a dirty file)",
			Mutate: func(w *Workspace) {
				w.Replace(tests, weakAssert, "    assert True\n")
			},
			Post: func(w *Workspace) {
				w.Write(".git/info/attributes", "tests/** binary\n")
			},
			AfterRun: notFresh}),
		adv(src, Scenario{Name: "adv/py_binary_protected_fixture_changed", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Note: "C1: a binary fixture under tests/ changed by appending (no removed text line)",
			Mutate: func(w *Workspace) {
				w.Write("tests/golden.bin", "GOLD\x00\x01\x02\n")
				w.Base = w.Commit("base with binary fixture")
				w.Write("tests/golden.bin", "GOLD\x00\x01\x02\nFORGED\x00\n")
			},
			AfterRun: notFresh}),
		adv(src, Scenario{Name: "adv/py_empty_protected_file_deleted", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Note: "C1 / deferred (f): deleting an EMPTY protected file removes no line",
			Mutate: func(w *Workspace) {
				w.Write("tests/data/keep.txt", "")
				w.Base = w.Commit("base with empty protected file")
				if err := os.Remove(filepath.Join(w.Dir, "tests/data/keep.txt")); err != nil {
					w.T.Fatal(err)
				}
			},
			AfterRun: notFresh}),
		adv(src, Scenario{Name: "adv/py_new_requirements_and_npmrc", Want: rules.CategoryWeakened, WantRules: []string{"protected_modified"},
			Note: "I3: new dependency manifests are built-in guarded files",
			Mutate: func(w *Workspace) {
				w.Write("requirements-dev.txt", "pytest==0.0.1\n")
				w.Write(".npmrc", "registry=https://evil.example/\n")
			}}),
	}
}

// C2(a): comparing head with itself is a gate error and writes no report.
func TestReviewSelfCompareIsGateError(t *testing.T) {
	w := NewWorkspace(t, "", "")
	w.Head = w.Base
	_, err := gate.Run(context.Background(), gate.Options{Repo: w.Dir, Base: "HEAD", HomeDir: w.Home, Executor: w.Executor()})
	got := "gate_error"
	if err == nil {
		got = "no error"
		t.Errorf("base == head: want gate error")
	}
	if _, serr := os.Stat(filepath.Join(w.Dir, ".aigate", "reports", w.Base[:10]+".json")); serr == nil {
		got += "+report"
		t.Errorf("a report was written for a self-compare")
	}
	Record(Result{Stack: "py", Name: "adv/self_compare_base_eq_head", Want: "gate_error", Got: got, Match: got == "gate_error", Source: "review",
		Note: "C2: `aigate run --base HEAD` used to give an honest-looking ok"})
}

// C2(c)(d): check rejects a wrong base, an initial report, a forged report
// (category edited to ok while violations remain) and a report edited after
// its run (ledger sha mismatch).
func TestReviewCheckRejects(t *testing.T) {
	type tc struct {
		name string
		run  func(t *testing.T) []string // problems
	}
	okHead := func(t *testing.T) *Workspace {
		w := NewWorkspace(t, "", "")
		w.Commit("mid")
		w.Write("README.md", "docs only\n")
		w.Head = w.Commit("head")
		if r, _ := w.run(); r.Category != rules.CategoryOK {
			t.Fatalf("setup: want ok, got %s %+v", r.Category, r.Violations)
		}
		return w
	}
	mustProblems := func(t *testing.T, w *Workspace, base string) []string {
		p, err := checkProblems(w, base)
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		return p
	}
	cases := []tc{
		{"adv/check_fresh_sanity", func(t *testing.T) []string {
			w := okHead(t)
			if p := mustProblems(t, w, w.Base); len(p) != 0 {
				t.Errorf("real ok report rejected: %v", p)
				return nil
			}
			return []string{"(fresh as expected)"}
		}},
		{"adv/check_wrong_base", func(t *testing.T) []string {
			w := okHead(t)
			return mustProblems(t, w, w.Head+"~1") // the "mid" commit, not the run's base
		}},
		{"adv/check_initial_report", func(t *testing.T) []string {
			w := NewWorkspace(t, "", "")
			w.Head = w.Base
			r, err := gate.Run(context.Background(), gate.Options{Repo: w.Dir, Initial: true, HomeDir: w.Home, Executor: w.Executor()})
			if err != nil || !r.Initial || r.Category != rules.CategoryOK {
				t.Fatalf("initial run: %v %+v", err, r)
			}
			return mustProblems(t, w, w.Base)
		}},
		{"adv/check_forged_category_ok", func(t *testing.T) []string {
			w := NewWorkspace(t, "", "")
			w.Replace(tests, weakAssert, "    assert True\n")
			w.Head = w.Commit("weaken")
			r, _ := w.run()
			if r.Category != rules.CategoryWeakened {
				t.Fatalf("setup: want weakened, got %s", r.Category)
			}
			rp := filepath.Join(w.Dir, ".aigate", "reports", w.Head[:10]+".json")
			b := w.Read(".aigate/reports/" + w.Head[:10] + ".json")
			forged := strings.Replace(strings.Replace(b, `"category": "check_weakened",`, `"category": "ok",`, 1), `"exit_code": 3,`, `"exit_code": 0,`, 1)
			if forged == b {
				t.Fatal("forgery did not apply")
			}
			if err := os.WriteFile(rp, []byte(forged), 0o644); err != nil {
				t.Fatal(err)
			}
			return mustProblems(t, w, w.Base)
		}},
		{"adv/check_report_edited_after_run", func(t *testing.T) []string {
			w := okHead(t)
			rp := filepath.Join(w.Dir, ".aigate", "reports", w.Head[:10]+".json")
			b := w.Read(".aigate/reports/" + w.Head[:10] + ".json")
			edited := strings.Replace(b, `"tool_version": "0.1.0"`, `"tool_version": "0.1.0-edited"`, 1)
			if edited == b {
				t.Fatal("edit did not apply")
			}
			if err := os.WriteFile(rp, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			return mustProblems(t, w, w.Base)
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := c.run(t)
			want, got := "not_fresh", "fresh"
			if c.name == "adv/check_fresh_sanity" {
				want = "fresh"
				if len(p) == 1 && p[0] == "(fresh as expected)" {
					got = "fresh"
				} else {
					got = "not_fresh"
				}
			} else if len(p) > 0 {
				got = "not_fresh"
			}
			if got != want {
				t.Errorf("%s: want %s, got %s (%v)", c.name, want, got, p)
			}
			Record(Result{Stack: "py", Name: c.name, Want: want, Got: got, Match: got == want, Source: "review",
				Rules: p, Note: fmt.Sprintf("%d problem(s)", len(p))})
		})
	}
}
