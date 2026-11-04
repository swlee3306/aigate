package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aigate/internal/rules"
)

const (
	headSHA = "aaaaaaaaaabbbbbbbbbbccccccccccdddddddddd"
	baseSHA = "1111111111222222222233333333334444444444"
)

func sample() Report {
	return Report{
		SchemaVersion: 1,
		Category:      rules.CategoryOK,
		ExitCode:      0,
		Repo:          "/r",
		Base:          baseSHA,
		Head:          headSHA,
		ConfigHash:    "ch",
		ToolVersion:   "0.1",
		StartedAt:     "2026-11-04T00:00:00Z",
		FinishedAt:    "2026-11-04T00:00:05Z",
		Checks: []CheckResult{{
			ID:   "unit",
			Base: RunSummary{RC: 0, Counts: Counts{Passed: 3}, Seconds: 1.5},
			Head: RunSummary{RC: 0, Counts: Counts{Passed: 3, Skipped: 1}, Seconds: 1.25},
		}},
		Violations: []rules.Violation{{Rule: "test_skipped", Category: rules.CategoryWeakened, Detail: "a\nb", IDs: []string{"t1"}}},
		Waived:     []string{"z", "a"},
		Approvals:  []ApprovalState{{Hash: "h1", Valid: true}},
		Initial:    true,
	}
}

func TestJSONRoundTripAndShape(t *testing.T) {
	r := sample()
	b := r.JSON()
	if !bytes.Contains(b, []byte("\n  \"schema_version\": 1")) {
		t.Fatalf("not 2-space indented snake_case: %s", b)
	}
	p := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := sample()
	want.Waived = []string{"a", "z"} // JSON sorts waived
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", got, want)
	}
	if !bytes.Equal(b, got.JSON()) {
		t.Fatal("JSON not deterministic")
	}
	if r.Waived[0] != "z" {
		t.Fatal("JSON mutated receiver's Waived")
	}
}

func TestJSONNilSlicesAreArrays(t *testing.T) {
	b := Report{SchemaVersion: 1, Category: rules.CategoryOK}.JSON()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"checks", "violations", "waived", "approvals"} {
		if _, ok := m[k].([]any); !ok {
			t.Errorf("%s = %v, want array", k, m[k])
		}
	}
}

func TestMarkdown(t *testing.T) {
	md := sample().Markdown()
	lines := strings.Split(md, "\n")
	if lines[0] != "# 검증 보고서: ok (exit 0)" {
		t.Fatalf("first line %q", lines[0])
	}
	for _, s := range []string{"aaaaaaaaaa", "1111111111", "unit", "## 위반", "- [check_weakened] **test_skipped**: a b", "h1"} {
		if !strings.Contains(md, s) {
			t.Errorf("missing %q in\n%s", s, md)
		}
	}
	if strings.Contains(md, headSHA) {
		t.Error("full SHA should be shortened")
	}
	if !strings.Contains(Report{Category: rules.CategoryOK}.Markdown(), "- 없음") {
		t.Error("empty violations should say 없음")
	}
}

// freshSample is sample() made internally consistent (ok, no violations,
// compare mode): sample() itself is a rendering fixture (ok + a violation +
// initial) that check must reject since the final review (C2).
func freshSample() Report {
	r := sample()
	r.Violations = nil
	r.Initial = false
	return r
}

// expect returns the Expect under which freshSample() is fresh.
func expect(head string, dirty bool, av func(string) bool) Expect {
	return Expect{Head: head, Base: baseSHA, Repo: "/r", ConfigHash: "ch", Dirty: dirty, ApprovalValid: av,
		ReportSHA256: "s", LedgerRun: &LedgerRun{Category: "ok", ConfigHash: "ch", ReportSHA256: "s"}}
}

func TestCheck(t *testing.T) {
	yes := func(string) bool { return true }
	if p := Check(freshSample(), expect(headSHA, false, yes)); len(p) != 0 {
		t.Fatalf("fresh report has problems: %v", p)
	}
	p := Check(freshSample(), expect("ffffffffff0000", false, yes))
	if len(p) != 1 || !strings.Contains(p[0], "aaaaaaaaaa") || !strings.Contains(p[0], "ffffffffff") {
		t.Fatalf("head mismatch: %v", p)
	}
	if p := Check(freshSample(), expect(headSHA, true, yes)); len(p) != 1 {
		t.Fatalf("dirty: %v", p)
	}
	bad := freshSample()
	bad.Category = rules.CategoryWeakened
	if p := Check(bad, expect(headSHA, false, yes)); len(p) != 1 || !strings.Contains(p[0], "check_weakened") {
		t.Fatalf("category: %v", p)
	}
	if p := Check(freshSample(), expect(headSHA, false, func(string) bool { return false })); len(p) != 1 || !strings.Contains(p[0], "h1") {
		t.Fatalf("approval vanished: %v", p)
	}
	if p := Check(freshSample(), expect(headSHA, false, nil)); len(p) != 1 {
		t.Fatalf("nil approvalValid: %v", p)
	}
	sv := freshSample()
	sv.SchemaVersion = 2
	if p := Check(sv, expect(headSHA, false, yes)); len(p) != 1 {
		t.Fatalf("schema: %v", p)
	}
	all := freshSample()
	all.SchemaVersion = 0
	all.Category = rules.CategoryBehavior
	if p := Check(all, expect("x", true, nil)); len(p) != 5 {
		t.Fatalf("all problems: %v", p)
	}
}

// C2 / T8 / T9: check binds the report to base, repo, config and the ledger,
// rejects initial and self-compare reports and internally inconsistent ones.
func TestCheckBindings(t *testing.T) {
	yes := func(string) bool { return true }
	cases := []struct {
		name   string
		mut    func(r *Report, e *Expect)
		needle string
	}{
		{"wrong base", func(r *Report, e *Expect) { e.Base = "2222222222" }, "base"},
		{"empty base", func(r *Report, e *Expect) { r.Base = "" }, "base"},
		{"initial report", func(r *Report, e *Expect) { r.Initial = true }, "초기"},
		{"self compare", func(r *Report, e *Expect) { r.Base, e.Base = headSHA, headSHA }, "같은 커밋"},
		{"other repo", func(r *Report, e *Expect) { e.Repo = "/other" }, "저장소"},
		{"config hash", func(r *Report, e *Expect) { e.ConfigHash = "other" }, "설정"},
		{"ok with violations", func(r *Report, e *Expect) {
			r.Violations = []rules.Violation{{Rule: "protected_modified", Category: rules.CategoryWeakened}}
		}, "모순"},
		{"ok with exit 3", func(r *Report, e *Expect) { r.ExitCode = 3 }, "모순"},
		{"ok with invalid approval", func(r *Report, e *Expect) { r.Approvals = append(r.Approvals, ApprovalState{Hash: "h2"}) }, "h2"},
		{"ledger broken", func(r *Report, e *Expect) { e.LedgerErr = os.ErrInvalid }, "이력"},
		{"no ledger event", func(r *Report, e *Expect) { e.LedgerRun = nil }, "이력"},
		{"ledger not ok", func(r *Report, e *Expect) { e.LedgerRun.Category = "check_weakened" }, "이력"},
		{"ledger config differs", func(r *Report, e *Expect) { e.LedgerRun.ConfigHash = "zz" }, "이력"},
		{"report edited after run", func(r *Report, e *Expect) { e.ReportSHA256 = "edited" }, "이력"},
	}
	for _, c := range cases {
		r, e := freshSample(), expect(headSHA, false, yes)
		lr := *e.LedgerRun
		e.LedgerRun = &lr
		c.mut(&r, &e)
		p := Check(r, e)
		if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), c.needle) {
			t.Errorf("%s: want a problem mentioning %q, got %v", c.name, c.needle, p)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	r := sample()
	jp, err := Save(dir, r)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".aigate", "reports", "aaaaaaaaaa.json"); jp != want {
		t.Fatalf("path %s want %s", jp, want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".aigate", "reports", "aaaaaaaaaa.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(jp); err != nil {
		t.Fatal(err)
	}
	r.Head = "short"
	if _, err := Save(dir, r); err == nil {
		t.Fatal("short head must error")
	}
}

func TestLoadStrict(t *testing.T) {
	d := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(d, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	if _, err := Load(w("a.json", `{"schema_version":1,"bogus":1}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := Load(w("b.json", `{"schema_version":2}`)); err == nil {
		t.Error("schema 2 accepted")
	}
	if _, err := Load(w("c.json", `{"schema_version":1} {}`)); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := Load(filepath.Join(d, "nope.json")); err == nil {
		t.Error("missing file accepted")
	}
}
