package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/rules"
)

func count(r rules.Results) map[rules.Status]int {
	m := map[rules.Status]int{}
	for _, s := range r {
		m[s]++
	}
	return m
}

func open(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseJUnitPytest(t *testing.T) {
	p := ParseJUnit(filepath.Join("testdata", "pytest.xml"))
	if p.ParseFailed {
		t.Fatal("ParseFailed")
	}
	c := count(p.Results)
	// fixture: 1 collection error, 1 failure, 2 skipped (skip + xfail), 5 passed
	if c[rules.Passed] != 5 || c[rules.Failed] != 1 || c[rules.Skipped] != 2 || c[rules.Error] != 1 {
		t.Fatalf("counts = %v", c)
	}
	want := map[string]rules.Status{
		"test_a::test_pass":                  rules.Passed,
		"test_a::test_fail":                  rules.Failed,
		"test_a::test_skipped":               rules.Skipped,
		"test_a::test_xfail":                 rules.Skipped,
		"test_a::test_param[a b]":            rules.Passed,
		"test_a::test_param[\\ud55c\\uae00]": rules.Passed, // pytest escapes non-ASCII param ids
		"test_a::test_param[1]":              rules.Passed,
		"test_a.TestK::test_m":               rules.Passed,
		"test_broken":                        rules.Error,
	}
	for id, st := range want {
		if p.Results[id] != st {
			t.Errorf("%q = %q, want %q", id, p.Results[id], st)
		}
	}
}

func TestParseJUnitVitestAndJest(t *testing.T) {
	v := ParseJUnit(filepath.Join("testdata", "vitest.xml"))
	if v.ParseFailed {
		t.Fatal("vitest ParseFailed")
	}
	if c := count(v.Results); c[rules.Passed] != 3 || c[rules.Failed] != 1 || c[rules.Skipped] != 1 {
		t.Fatalf("vitest counts = %v", c)
	}
	if v.Results["src/a.test.js::param a b"] != rules.Passed {
		t.Errorf("vitest ids: %v", v.Results)
	}
	j := ParseJUnit(filepath.Join("testdata", "jest.xml"))
	if j.ParseFailed {
		t.Fatal("jest ParseFailed")
	}
	if c := count(j.Results); c[rules.Passed] != 3 || c[rules.Failed] != 1 || c[rules.Skipped] != 1 {
		t.Fatalf("jest counts = %v", c)
	}
	// jest-junit puts a leading space in its names; normalization strips it.
	if j.Results["passes::passes"] != rules.Passed {
		t.Errorf("jest ids: %v", j.Results)
	}
}

func TestParseJUnitDuplicateKeepsWorst(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.xml")
	xml := `<testsuite><testcase classname="a" name="t"/><testcase classname="a" name="t"><failure/></testcase><testcase classname="a" name="t"/></testsuite>`
	if err := os.WriteFile(p, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := ParseJUnit(p); r.ParseFailed || r.Results["a::t"] != rules.Failed {
		t.Fatalf("got %+v", r)
	}
}

func TestParseFailed(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "b.xml")
	if err := os.WriteFile(broken, []byte("<testsuites><testsuite><testcase name="), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := ParseJUnit(broken); !p.ParseFailed {
		t.Error("broken xml: ParseFailed=false")
	}
	if p := ParseJUnit(filepath.Join(dir, "missing.xml")); !p.ParseFailed {
		t.Error("missing file: ParseFailed=false")
	}
	if p := ParseGoTestJSON(strings.NewReader("not json\n")); !p.ParseFailed {
		t.Error("bad json: ParseFailed=false")
	}
}

func TestParseGoTestJSON(t *testing.T) {
	p := ParseGoTestJSON(open(t, "gotest.jsonl"))
	if p.ParseFailed {
		t.Fatal("ParseFailed")
	}
	want := map[string]rules.Status{
		"example.com/toy/ok/TestA":          rules.Failed,
		"example.com/toy/ok/TestA/sub_case": rules.Passed,
		"example.com/toy/ok/TestA/fails":    rules.Failed,
		"example.com/toy/ok/TestSkip":       rules.Skipped,
		"example.com/toy/ok/TestPass":       rules.Passed,
	}
	if len(p.Results) != len(want) {
		t.Fatalf("results = %v", p.Results)
	}
	for id, st := range want {
		if p.Results[id] != st {
			t.Errorf("%q = %q, want %q", id, p.Results[id], st)
		}
	}
}

func TestParseGoTestJSONBuildFail(t *testing.T) {
	p := ParseGoTestJSON(open(t, "gotest_buildfail.jsonl"))
	if p.ParseFailed {
		t.Fatal("ParseFailed")
	}
	if len(p.Results) != 1 || p.Results["example.com/toy/bad[build]"] != rules.Error {
		t.Fatalf("results = %v", p.Results)
	}
	all := ParseGoTestJSON(open(t, "gotest_all.jsonl"))
	if all.Results["example.com/toy/bad[build]"] != rules.Error || all.Results["example.com/toy/ok/TestPass"] != rules.Passed {
		t.Fatalf("mixed results = %v", all.Results)
	}
}

func TestParseGoTestJSONUnfinishedIsError(t *testing.T) {
	in := `{"Action":"run","Package":"p","Test":"TestHang"}` + "\n"
	if p := ParseGoTestJSON(strings.NewReader(in)); p.Results["p/TestHang"] != rules.Error {
		t.Fatalf("got %v", p.Results)
	}
}

func TestExitCode(t *testing.T) {
	if p := ExitCode("build", 0); p.Results["build"] != rules.Passed || p.ParseFailed {
		t.Errorf("rc0: %+v", p)
	}
	if p := ExitCode("build", 2); p.Results["build"] != rules.Failed {
		t.Errorf("rc2: %+v", p)
	}
}

func TestNormalizeID(t *testing.T) {
	if got := NormalizeID("test  a "); got != "test a" {
		t.Errorf("got %q", got)
	}
	if got := NormalizeID("\t x \n y z "); got != "x y z" {
		t.Errorf("got %q", got)
	}
	nfc, nfd := "한글", "한글"
	if nfc == nfd {
		t.Fatal("test inputs identical")
	}
	if NormalizeID(nfc) != NormalizeID(nfd) {
		t.Error("NFC and NFD differ after normalization")
	}
}

func TestPresetFor(t *testing.T) {
	for _, n := range []string{"pytest", "go", "vitest", "jest"} {
		p, ok := PresetFor(n)
		if !ok || len(p.Command) == 0 || p.Adapter == "" {
			t.Errorf("%s: %+v %v", n, p, ok)
		}
	}
	if _, ok := PresetFor("nope"); ok {
		t.Error("unknown preset found")
	}
	py, _ := PresetFor("pytest")
	if py.Adapter != "junit" || strings.Join(py.Neutralize, " ") != "-o addopts=" || !strings.Contains(strings.Join(py.Command, " "), "--junitxml=${AIGATE_JUNIT}") {
		t.Errorf("pytest: %+v", py)
	}
	g, _ := PresetFor("go")
	if g.Adapter != "gotest-json" || strings.Join(g.Command, " ") != "go test -json -count=1 ./..." {
		t.Errorf("go: %+v", g)
	}
	// Mutating a returned preset must not corrupt later lookups.
	py.Command[0] = "x"
	if again, _ := PresetFor("pytest"); again.Command[0] == "x" {
		t.Error("preset slices are shared")
	}
}

func TestParseJUnitSuiteLevelFailureIsError(t *testing.T) {
	p := ParseJUnit(filepath.Join("testdata", "vitest_syntax_error.xml"))
	if p.ParseFailed {
		t.Fatal("ParseFailed")
	}
	if p.Results["src/calc.test.ts::src/calc.test.ts"] != rules.Error || len(p.Results) != 1 {
		t.Fatalf("suite-level load failure must be error: %v", p.Results)
	}
	// A normal failing test whose name differs from classname stays failed
	// (vitest fixture), and jest's failing test (name == classname, but the
	// suite name differs) stays failed too.
	v := ParseJUnit(filepath.Join("testdata", "vitest.xml"))
	if v.Results["src/a.test.js::fails"] != rules.Failed {
		t.Errorf("vitest fails: %v", v.Results)
	}
	j := ParseJUnit(filepath.Join("testdata", "jest.xml"))
	if j.Results["fails::fails"] != rules.Failed {
		t.Errorf("jest fails: %v", j.Results)
	}
}
