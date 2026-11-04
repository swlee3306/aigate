package rules

import (
	"strings"
	"testing"
)

func okRun(ids ...string) RunInfo {
	r := Results{}
	for _, id := range ids {
		r[id] = Passed
	}
	return RunInfo{Results: r}
}

func with(r RunInfo, id string, s Status) RunInfo {
	n := Results{}
	for k, v := range r.Results {
		n[k] = v
	}
	n[id] = s
	r.Results = n
	return r
}

func (r RunInfo) withStatus(id string, st Status) RunInfo { return with(r, id, st) }

func TestEvaluate(t *testing.T) {
	base := okRun("a", "b")
	cases := []struct {
		name       string
		in         Input
		wantRules  []string
		wantCat    Category
		wantWaived []string
	}{
		{"all green", Input{Base: base, Head: okRun("a", "b")}, nil, CategoryOK, nil},
		{"regression", Input{Base: base, Head: with(base, "a", Failed)}, []string{"regression"}, CategoryBehavior, nil},
		{"missing", Input{Base: base, Head: okRun("a")}, []string{"test_missing"}, CategoryWeakened, nil},
		{"skipped", Input{Base: base, Head: with(base, "a", Skipped)}, []string{"test_skipped"}, CategoryWeakened, nil},
		{"head error", Input{Base: base, Head: with(base, "a", Error)}, []string{"setup_or_collection_error"}, CategoryIncomplete, nil},
		{"timeout", Input{Base: base, Head: RunInfo{Results: base.Results, TimedOut: true}}, []string{"timeout"}, CategoryIncomplete, nil},
		{"no tests ran", Input{Base: base, Head: RunInfo{Results: Results{}}}, []string{"no_tests_ran"}, CategoryIncomplete, nil},
		{"parse failed", Input{Base: base, Head: RunInfo{ParseFailed: true}}, []string{"parse_failed"}, CategoryIncomplete, nil},
		{"base failing waived", Input{Base: RunInfo{Results: with(base, "a", Failed).Results, RC: 1}, Head: with(base, "a", Failed)}, nil, CategoryOK, []string{"a"}},
		{"required waived with realistic rc", Input{Required: true, Base: RunInfo{Results: Results{"a": Passed, "b": Failed}, RC: 1}, Head: okRun("a").withStatus("b", Failed)}, nil, CategoryOK, []string{"b"}},
		{"required base all failed", Input{Required: true, Base: RunInfo{Results: Results{"a": Failed, "b": Failed}, RC: 1}, Head: with(okRun(), "a", Failed).withStatus("b", Failed)}, []string{"base_not_green"}, CategoryIncomplete, []string{"a", "b"}},
		{"required base all skipped", Input{Required: true, Base: RunInfo{Results: Results{"a": Skipped, "b": Skipped}}, Head: with(okRun(), "a", Skipped).withStatus("b", Skipped)}, []string{"base_not_green"}, CategoryIncomplete, []string{"a", "b"}},
		{"required passed+skipped rc0", Input{Required: true, Base: RunInfo{Results: Results{"a": Passed, "b": Skipped}}, Head: okRun("a").withStatus("b", Skipped)}, nil, CategoryOK, []string{"b"}},
		{"dirty", Input{Base: base, Head: okRun("a", "b"), Dirty: true}, []string{"dirty_tree"}, CategoryIncomplete, nil},
		{"protected removed", Input{Base: base, Head: okRun("a", "b"), ProtectedRemoved: 2, ProtectedFiles: []string{"t.py"}}, []string{"protected_modified"}, CategoryWeakened, nil},
		{"protected approved", Input{Base: base, Head: okRun("a", "b"), ProtectedRemoved: 2, ApprovalHashValid: true}, nil, CategoryOK, nil},
		{"added guarded", Input{Base: base, Head: okRun("a", "b"), AddedGuardedFiles: []string{"conftest.py"}}, []string{"protected_modified"}, CategoryWeakened, nil},
		{"added guarded approved", Input{Base: base, Head: okRun("a", "b"), AddedGuardedFiles: []string{"conftest.py"}, ApprovalHashValid: true}, nil, CategoryOK, nil},
		{"new test fails", Input{Base: base, Head: with(okRun("a", "b"), "n", Failed)}, []string{"new_test_fails"}, CategoryBehavior, nil},
		{"new test skipped", Input{Base: base, Head: with(okRun("a", "b"), "n", Skipped)}, []string{"new_test_skipped"}, CategoryWeakened, nil},
		{"base timed out", Input{Base: RunInfo{Results: base.Results, TimedOut: true}, Head: okRun("a", "b")}, []string{"base_unreliable"}, CategoryIncomplete, nil},
		{"base empty", Input{Base: RunInfo{Results: Results{}}, Head: okRun("a")}, []string{"base_unreliable"}, CategoryIncomplete, nil},
		{"base has error", Input{Base: with(base, "a", Error), Head: okRun("a", "b")}, []string{"base_unreliable"}, CategoryIncomplete, nil},
		{"required base not green", Input{Required: true, Base: RunInfo{Results: base.Results, RC: 1}, Head: okRun("a", "b")}, []string{"base_not_green"}, CategoryIncomplete, nil},
		{"optional base rc!=0 ok", Input{Base: RunInfo{Results: base.Results, RC: 1}, Head: okRun("a", "b")}, nil, CategoryOK, nil},
		{"multiple violations", Input{Base: base, Head: with(okRun("a"), "n", Failed), Dirty: true}, []string{"dirty_tree", "test_missing", "new_test_fails"}, CategoryIncomplete, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vs, waived := Evaluate(c.in)
			var got []string
			for _, v := range vs {
				got = append(got, v.Rule)
			}
			if !sameSet(got, c.wantRules) {
				t.Fatalf("rules = %v, want %v", got, c.wantRules)
			}
			if cat := Verdict(vs); cat != c.wantCat {
				t.Fatalf("category = %v, want %v", cat, c.wantCat)
			}
			if !sameSet(waived, c.wantWaived) {
				t.Fatalf("waived = %v, want %v", waived, c.wantWaived)
			}
		})
	}
}

func TestEvaluateViolationDetails(t *testing.T) {
	base := okRun("a", "b")
	vs, _ := Evaluate(Input{Base: base, Head: with(okRun("b"), "n", Failed)})
	for _, v := range vs {
		switch v.Rule {
		case "test_missing":
			if len(v.IDs) != 1 || v.IDs[0] != "a" {
				t.Fatalf("test_missing ids = %v", v.IDs)
			}
		case "new_test_fails":
			if len(v.IDs) != 1 || v.IDs[0] != "n" {
				t.Fatalf("new_test_fails ids = %v", v.IDs)
			}
		}
		if v.Detail == "" {
			t.Fatalf("empty detail for %s", v.Rule)
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestVerdict(t *testing.T) {
	v := func(c Category) Violation { return Violation{Category: c} }
	cases := []struct {
		name string
		vs   []Violation
		want Category
	}{
		{"none", nil, CategoryOK},
		{"all three", []Violation{v(CategoryBehavior), v(CategoryWeakened), v(CategoryIncomplete)}, CategoryIncomplete},
		{"weakened+behavior", []Violation{v(CategoryBehavior), v(CategoryWeakened)}, CategoryWeakened},
		{"behavior", []Violation{v(CategoryBehavior)}, CategoryBehavior},
	}
	for _, c := range cases {
		if got := Verdict(c.vs); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestExitCode(t *testing.T) {
	want := map[Category]int{CategoryOK: 0, CategoryBehavior: 1, CategoryIncomplete: 2, CategoryWeakened: 3}
	for c, n := range want {
		if got := c.ExitCode(); got != n {
			t.Errorf("%s: got %d want %d", c, got, n)
		}
	}
}

func TestRenameHint(t *testing.T) {
	base := okRun("tests/a.py::test_login", "tests/a.py::test_other")
	head := okRun("tests/b.py::test_login", "tests/a.py::test_other")
	vs, _ := Evaluate(Input{Base: base, Head: head})
	var found bool
	for _, v := range vs {
		if v.Rule == "test_missing" {
			found = true
			if !strings.Contains(v.Detail, "이름 변경 의심") {
				t.Fatalf("detail lacks rename hint: %q", v.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no test_missing violation")
	}
	// no hint when the new id has a different suffix
	vs, _ = Evaluate(Input{Base: okRun("x::test_a"), Head: okRun("y::test_b")})
	for _, v := range vs {
		if v.Rule == "test_missing" && strings.Contains(v.Detail, "이름 변경 의심") {
			t.Fatalf("unexpected hint: %q", v.Detail)
		}
	}
}
