package gate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"aigate/internal/approval"
	"aigate/internal/ledger"
	"aigate/internal/rules"
)

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, repo, name, content string) {
	t.Helper()
	p := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo, msg string) string {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", msg)
	return git(t, repo, "rev-parse", "HEAD")
}

func newRepo(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, d, "init", "-q", "-b", "main")
	return d
}

// checkYAML builds a config whose checks pass when <flag> exists; every run
// appends a line to logPath.
func checkYAML(logPath string, extra string, checks ...string) string {
	var b strings.Builder
	b.WriteString("version: 1\nprotected:\n  - \"tests/**\"\n  - \".aigate.yml\"\napproval_on_add:\n  - \"**/conftest.py\"\nchecks:\n")
	for _, id := range checks {
		fmt.Fprintf(&b, "  - id: %s\n    adapter: exit-code\n    command: [\"sh\", \"-c\", \"echo %s >> %s; test -f %s.flag\"]\n    timeout: 20s\n%s", id, id, logPath, id, extra)
	}
	return b.String()
}

func lines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

func hasRule(vs []rules.Violation, rule string) bool {
	for _, v := range vs {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

type env struct {
	repo, home, log string
}

func setup(t *testing.T) env {
	t.Helper()
	return env{repo: newRepo(t), home: t.TempDir(), log: filepath.Join(t.TempDir(), "calls.log")}
}

func (e env) opts(base string) Options {
	return Options{Repo: e.repo, Base: base, HomeDir: e.home}
}

func TestBaseEqualsHeadOK(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit", "build"))
	write(t, e.repo, "unit.flag", "")
	write(t, e.repo, "build.flag", "")
	c := commit(t, e.repo, "c1")
	r, err := Run(context.Background(), e.opts(c))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK || r.ExitCode != 0 {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	got := lines(t, e.log)
	if len(got) != 4 {
		t.Fatalf("each check once per side, got %v", got)
	}
	if r.Base != c || r.Head != c || r.SchemaVersion != 1 || r.ToolVersion != "0.1.0" || r.ConfigHash == "" || r.Initial {
		t.Fatalf("report fields: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.repo, ".aigate", "reports", c[:10]+".json")); err != nil {
		t.Fatal(err)
	}
}

func TestRegressionIsBehaviorFailure(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	base := commit(t, e.repo, "base")
	git(t, e.repo, "rm", "-q", "unit.flag")
	commit(t, e.repo, "head")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryBehavior || r.ExitCode != 1 {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	if !strings.HasPrefix(r.Violations[0].Detail, "[unit] ") {
		t.Fatalf("detail not prefixed: %q", r.Violations[0].Detail)
	}
}

func TestNotAncestor(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	commit(t, e.repo, "root")
	git(t, e.repo, "checkout", "-q", "-b", "other")
	write(t, e.repo, "x", "1")
	other := commit(t, e.repo, "other")
	git(t, e.repo, "checkout", "-q", "main")
	write(t, e.repo, "y", "1")
	commit(t, e.repo, "main2")
	r, err := Run(context.Background(), e.opts(other))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete || len(r.Violations) != 1 || r.Violations[0].Rule != "base_not_ancestor" {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	if n := len(lines(t, e.log)); n != 0 {
		t.Fatalf("checks executed: %d", n)
	}
}

func TestBaseOmittedUsesMergeBase(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	base := commit(t, e.repo, "main1")
	git(t, e.repo, "checkout", "-q", "-b", "feature")
	write(t, e.repo, "f", "1")
	commit(t, e.repo, "feat")
	r, err := Run(context.Background(), e.opts(""))
	if err != nil {
		t.Fatal(err)
	}
	if r.Base != base || r.Initial || r.Category != rules.CategoryOK {
		t.Fatalf("base=%s want %s initial=%v cat=%s", r.Base, base, r.Initial, r.Category)
	}
}

func TestInitialMode(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	commit(t, e.repo, "only") // on main: nothing to compare
	r, err := Run(context.Background(), e.opts(""))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Initial || r.Category != rules.CategoryOK || r.Base != "" {
		t.Fatalf("got %+v", r)
	}
	if n := len(lines(t, e.log)); n != 1 {
		t.Fatalf("head only, got %d runs", n)
	}
	// head red -> behavior failure
	git(t, e.repo, "rm", "-q", "unit.flag")
	commit(t, e.repo, "red")
	r, err = Run(context.Background(), e.opts(""))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Initial || r.Category != rules.CategoryBehavior {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func TestInitialIncompleteOnDirty(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	commit(t, e.repo, "only")
	write(t, e.repo, "junk", "x")
	r, err := Run(context.Background(), e.opts(""))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete || !hasRule(r.Violations, "dirty_tree") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func TestMissingConfigIsError(t *testing.T) {
	e := setup(t)
	write(t, e.repo, "a", "1")
	c := commit(t, e.repo, "c")
	r, err := Run(context.Background(), e.opts(c))
	if err == nil {
		t.Fatal("want error")
	}
	if r.Category == rules.CategoryOK {
		t.Fatal("error must never be ok")
	}
	if _, serr := os.Stat(filepath.Join(e.repo, ".aigate", "reports")); serr == nil {
		t.Fatal("no report on gate error")
	}
}

func TestUnresolvableBaseIsError(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	commit(t, e.repo, "c")
	if _, err := Run(context.Background(), e.opts("nope")); err == nil {
		t.Fatal("want error")
	}
}

func TestBaseConfigApplies(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	base := commit(t, e.repo, "base")
	// head adds a failing check "extra" and rewrites the config (removing lines)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit", "extra")+"# changed\n")
	commit(t, e.repo, "head")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines(t, e.log) {
		if l == "extra" {
			t.Fatal("head-only check must not run")
		}
	}
	if len(r.Checks) != 1 {
		t.Fatalf("checks: %+v", r.Checks)
	}
	// config-only change that removes a line is protected
	write(t, e.repo, ".aigate.yml", strings.Replace(checkYAML(e.log, "", "unit"), "timeout: 20s", "timeout: 30s", 1))
	commit(t, e.repo, "head2")
	r, err = Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func protectedScenario(t *testing.T) (env, string) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	write(t, e.repo, "tests/t.txt", "one\ntwo\n")
	base := commit(t, e.repo, "base")
	write(t, e.repo, "tests/t.txt", "one\nTWO\n")
	commit(t, e.repo, "weaken")
	return e, base
}

func TestApproveFlagAloneIsNotEnough(t *testing.T) {
	e, base := protectedScenario(t)
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryWeakened || len(r.Approvals) != 1 || r.Approvals[0].Valid {
		t.Fatalf("got %s %+v %+v", r.Category, r.Violations, r.Approvals)
	}
	hash := r.Approvals[0].Hash
	o := e.opts(base)
	o.ApproveHash = hash
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRule(r.Violations, "protected_modified") {
		t.Fatalf("flag alone must not approve: %+v", r.Violations)
	}
	// wrong hash is mentioned/ignored
	o.ApproveHash = "deadbeef"
	r, _ = Run(context.Background(), o)
	if !strings.Contains(r.Violations[0].Detail, hash) || !strings.Contains(r.Violations[0].Detail, "deadbeef") {
		t.Fatalf("detail should name real hash: %q", r.Violations[0].Detail)
	}
	// grant -> gone
	id, _, _ := RepoID(e.repo)
	st := approval.Store{Path: ApprovalsPath(e.home)}
	if err := st.Grant(id, base, hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err = Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if hasRule(r.Violations, "protected_modified") || r.Category != rules.CategoryOK || !r.Approvals[0].Valid {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func TestGuardedAddNeedsApproval(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	base := commit(t, e.repo, "base")
	write(t, e.repo, "pkg/conftest.py", "x\n")
	commit(t, e.repo, "add")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryWeakened {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func TestLedgerOneLinePerRun(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	c := commit(t, e.repo, "c")
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), e.opts(c)); err != nil {
			t.Fatal(err)
		}
	}
	id, _, _ := RepoID(e.repo)
	p := LedgerPath(e.home, id)
	n, err := ledger.Verify(p)
	if err != nil || n != 2 {
		t.Fatalf("verify n=%d err=%v", n, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"run_result"`) || !strings.Contains(string(b), "unclassified") {
		t.Fatalf("ledger: %s", b)
	}
}

func TestLedgerFailureIsGateError(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	c := commit(t, e.repo, "c")
	// a file where the ledger dir should be
	if err := os.WriteFile(filepath.Join(e.home, ".aigate"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), e.opts(c)); err == nil {
		t.Fatal("want ledger error")
	}
}

func TestNonRequiredViolationsDropped(t *testing.T) {
	e := setup(t)
	hard := checkYAML(e.log, "", "hard")
	yml := checkYAML(e.log, "    required: false\n", "soft") + hard[strings.Index(hard, "  - id: hard"):]
	write(t, e.repo, ".aigate.yml", yml)
	write(t, e.repo, "soft.flag", "")
	write(t, e.repo, "hard.flag", "")
	base := commit(t, e.repo, "base")
	git(t, e.repo, "rm", "-q", "soft.flag")
	commit(t, e.repo, "head")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK || len(r.Violations) != 0 {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	if len(r.Checks) != 2 || r.Checks[0].Base.Counts.Passed != 1 || r.Checks[0].Head.Counts.Failed != 1 {
		t.Fatalf("counts: %+v", r.Checks)
	}
}

func TestSetupFailureIsIncomplete(t *testing.T) {
	e := setup(t)
	yml := checkYAML(e.log, "    setup: [\"sh\", \"-c\", \"exit 3\"]\n", "unit")
	write(t, e.repo, ".aigate.yml", yml)
	write(t, e.repo, "unit.flag", "")
	c := commit(t, e.repo, "c")
	r, err := Run(context.Background(), e.opts(c))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	if len(lines(t, e.log)) != 0 {
		t.Fatal("check must not run after failed setup")
	}
}

func TestDirtyIsIncomplete(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	c := commit(t, e.repo, "c")
	write(t, e.repo, "junk", "x")
	r, err := Run(context.Background(), e.opts(c))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete || !hasRule(r.Violations, "dirty_tree") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func TestMultiCheckProtectedOnce(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "a", "b"))
	write(t, e.repo, "a.flag", "")
	write(t, e.repo, "b.flag", "")
	write(t, e.repo, "tests/t.txt", "one\ntwo\n")
	base := commit(t, e.repo, "base")
	write(t, e.repo, "tests/t.txt", "one\nTWO\n")
	commit(t, e.repo, "weaken")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range r.Violations {
		if v.Rule == "protected_modified" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("protected_modified x%d: %+v", n, r.Violations)
	}
}

func TestEmptyChecksKeepsRepoLevelViolations(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", "version: 1\nprotected: [\"tests/**\"]\nchecks: []\n")
	write(t, e.repo, "tests/t.txt", "one\ntwo\n")
	base := commit(t, e.repo, "base")
	write(t, e.repo, "tests/t.txt", "one\nTWO\n")
	commit(t, e.repo, "weaken")
	write(t, e.repo, "junk", "x")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete || !hasRule(r.Violations, "protected_modified") ||
		!hasRule(r.Violations, "dirty_tree") || !hasRule(r.Violations, "no_required_checks") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	if len(r.Approvals) != 1 || r.Approvals[0].Valid {
		t.Fatalf("approvals: %+v", r.Approvals)
	}
}

func TestEmptyChecksInitialMode(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", "version: 1\n")
	commit(t, e.repo, "only")
	r, err := Run(context.Background(), e.opts(""))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Initial || r.Category != rules.CategoryIncomplete || !hasRule(r.Violations, "no_required_checks") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func TestAllNonRequiredIsIncomplete(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "    required: false\n", "soft"))
	write(t, e.repo, "soft.flag", "")
	c := commit(t, e.repo, "c")
	r, err := Run(context.Background(), e.opts(c))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete || !hasRule(r.Violations, "no_required_checks") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

func TestFirstCheckNonRequiredKeepsProtected(t *testing.T) {
	e := setup(t)
	yml := checkYAML(e.log, "    required: false\n", "soft") + strings.TrimPrefix(checkYAML(e.log, "", "hard"), strings.SplitAfter(checkYAML(e.log, "", "hard"), "checks:\n")[0])
	write(t, e.repo, ".aigate.yml", yml)
	write(t, e.repo, "soft.flag", "")
	write(t, e.repo, "hard.flag", "")
	write(t, e.repo, "tests/t.txt", "one\ntwo\n")
	base := commit(t, e.repo, "base")
	write(t, e.repo, "tests/t.txt", "one\nTWO\n")
	commit(t, e.repo, "weaken")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 2 || r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") {
		t.Fatalf("got %s %+v checks=%d", r.Category, r.Violations, len(r.Checks))
	}
}

func TestDifferentGrantedHashNotHonored(t *testing.T) {
	e, base := protectedScenario(t)
	id, _, _ := RepoID(e.repo)
	st := approval.Store{Path: ApprovalsPath(e.home)}
	if err := st.Grant(id, base, "0123456789abcdef", time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") || r.Approvals[0].Valid {
		t.Fatalf("got %s %+v %+v", r.Category, r.Violations, r.Approvals)
	}
}

func TestGateErrorLeavesEarlierReportAndLedger(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	c := commit(t, e.repo, "c")
	if r, err := Run(context.Background(), e.opts(c)); err != nil || r.Category != rules.CategoryOK {
		t.Fatalf("setup run: %v %s", err, r.Category)
	}
	rp := filepath.Join(e.repo, ".aigate", "reports", c[:10]+".json")
	before, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := RepoID(e.repo)
	n0, _ := ledger.Verify(LedgerPath(e.home, id))
	// gate error: base does not resolve
	r, err := Run(context.Background(), e.opts("no-such-ref"))
	if err == nil || r.Category != "" {
		t.Fatalf("want error and empty report, got %v %+v", err, r)
	}
	after, _ := os.ReadFile(rp)
	if string(before) != string(after) {
		t.Fatal("earlier report was modified")
	}
	if n1, _ := ledger.Verify(LedgerPath(e.home, id)); n1 != n0 {
		t.Fatalf("ledger grew on gate error: %d -> %d", n0, n1)
	}
}

func TestMissingConfigWritesNothing(t *testing.T) {
	e := setup(t)
	write(t, e.repo, "a", "1")
	c := commit(t, e.repo, "c")
	r, err := Run(context.Background(), e.opts(c))
	if err == nil || r.Category != "" {
		t.Fatalf("want error and empty report, got %v %+v", err, r)
	}
	id, _, _ := RepoID(e.repo)
	if _, serr := os.Stat(LedgerPath(e.home, id)); serr == nil {
		t.Fatal("ledger written on gate error")
	}
}

func TestFailureCauseTailInDetail(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", "version: 1\nchecks:\n  - id: unit\n    adapter: junit\n    command: [\"sh\", \"-c\", \"echo 'ERR_MODULE_NOT_FOUND: cannot find package vitest' >&2; exit 1\"]\n    timeout: 20s\n")
	c := commit(t, e.repo, "c")
	write(t, e.repo, "x.txt", "x")
	commit(t, e.repo, "head")
	r, err := Run(context.Background(), e.opts(c))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	for _, rule := range []string{"base_unreliable", "parse_failed"} {
		found := false
		for _, v := range r.Violations {
			if v.Rule == rule {
				found = true
				if !strings.Contains(v.Detail, " | 출력 끝: ") || !strings.Contains(v.Detail, "ERR_MODULE_NOT_FOUND: cannot find package vitest") {
					t.Errorf("%s detail lacks the printed cause: %q", rule, v.Detail)
				}
			}
		}
		if !found {
			t.Errorf("missing rule %s in %+v", rule, r.Violations)
		}
	}
}

func TestOutputTailIsBoundedValidUTF8(t *testing.T) {
	raw := "\xff\xfe" + strings.Repeat("가나다 \n\t", 400) + "끝"
	got := cleanTail(raw)
	if !utf8.ValidString(got) || len([]rune(got)) > 300 || !strings.HasSuffix(got, "끝") || strings.ContainsAny(got, "\n\t") {
		t.Fatalf("bad tail (%d runes): %q", len([]rune(got)), got)
	}
	if cleanTail("  \n ") != "" {
		t.Fatal("blank output must give empty tail")
	}
}

func TestInitialRunnerErrorWhenRCNonZeroWithoutFailures(t *testing.T) {
	h := rules.RunInfo{Results: rules.Results{"a": rules.Passed}, RC: 3}
	vs := initialViolations("unit", true, h)
	if len(vs) != 1 || vs[0].Rule != "runner_error" || vs[0].Category != rules.CategoryIncomplete {
		t.Fatalf("want one runner_error (incomplete), got %+v", vs)
	}
}
