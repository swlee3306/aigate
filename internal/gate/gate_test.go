package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// initialOpts requests initial mode explicitly (the library no longer enters
// it on its own).
func (e env) initialOpts() Options {
	return Options{Repo: e.repo, HomeDir: e.home, Initial: true}
}

// C2: comparing a commit with itself gives a vacuous verdict (empty protected
// diff, base failures waived), so it is a gate error: no report, no ledger.
func TestBaseEqualsHeadIsGateError(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit", "build"))
	write(t, e.repo, "unit.flag", "")
	write(t, e.repo, "build.flag", "")
	c := commit(t, e.repo, "c1")
	for _, base := range []string{c, "HEAD", "main"} {
		r, err := Run(context.Background(), e.opts(base))
		if err == nil || r.Category != "" {
			t.Fatalf("base %s: want gate error, got %v %+v", base, err, r)
		}
		if !strings.Contains(err.Error(), "base") {
			t.Errorf("error should name the base: %v", err)
		}
	}
	if n := len(lines(t, e.log)); n != 0 {
		t.Fatalf("checks executed: %d", n)
	}
	if _, err := os.Stat(filepath.Join(e.repo, ".aigate", "reports")); err == nil {
		t.Fatal("report written on self-compare")
	}
	id, _, _ := RepoID(e.repo)
	if _, err := os.Stat(LedgerPath(e.home, id)); err == nil {
		t.Fatal("ledger written on self-compare")
	}
}

func TestBaseNotHeadOK(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit", "build"))
	write(t, e.repo, "unit.flag", "")
	write(t, e.repo, "build.flag", "")
	c := commit(t, e.repo, "c1")
	head := commit(t, e.repo, "c2")
	r, err := Run(context.Background(), e.opts(c))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK || r.ExitCode != 0 {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	if got := lines(t, e.log); len(got) != 4 {
		t.Fatalf("each check once per side, got %v", got)
	}
	if r.Base != c || r.Head != head || r.SchemaVersion != 1 || r.ToolVersion != "0.1.0" || r.ConfigHash == "" || r.Initial {
		t.Fatalf("report fields: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.repo, ".aigate", "reports", head[:10]+".json")); err != nil {
		t.Fatal(err)
	}
}

// C2: explicit initial mode is the only way to run without a base.
func TestExplicitInitialMode(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	main1 := commit(t, e.repo, "m1")
	git(t, e.repo, "checkout", "-q", "-b", "feature")
	commit(t, e.repo, "f1")
	o := e.opts("")
	o.Initial = true
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Initial || r.Base != "" || r.Category != rules.CategoryOK {
		t.Fatalf("explicit initial ignored (merge-base %s): %+v", main1[:7], r)
	}
	o.Base = main1
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("Initial together with Base must be an error")
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
	r, err := Run(context.Background(), e.initialOpts())
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
	r, err = Run(context.Background(), e.initialOpts())
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
	r, err := Run(context.Background(), e.initialOpts())
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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
	r, err := Run(context.Background(), e.initialOpts())
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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
	head := commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
	if r, err := Run(context.Background(), e.opts(c)); err != nil || r.Category != rules.CategoryOK {
		t.Fatalf("setup run: %v %s", err, r.Category)
	}
	rp := filepath.Join(e.repo, ".aigate", "reports", head[:10]+".json")
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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
	commit(t, e.repo, "head") // base != head: self-compare is a gate error (C2)
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

func TestOutputTailStripsANSIAndMasksSecrets(t *testing.T) {
	raw := "\x1b[31mFAIL\x1b[0m \x1b[1;32mok\x1b[m\n" +
		"API_KEY=sk-live-123 GITHUB_TOKEN=ghp_abc db_password=hunter2 MY_SECRET: s3cr3t\n" +
		"Authorization: Bearer eyJhbGciOi.payload.sig\n" +
		"fetch https://alice:pa55w0rd@registry.example.com/pkg failed\n" +
		"PATH=/usr/bin COUNT=3 ERR_MODULE_NOT_FOUND"
	got := cleanTail(raw)
	for _, leak := range []string{"\x1b", "[31m", "sk-live-123", "ghp_abc", "hunter2", "s3cr3t", "eyJhbGciOi", "pa55w0rd", "alice:"} {
		if strings.Contains(got, leak) {
			t.Errorf("tail still contains %q: %q", leak, got)
		}
	}
	for _, keep := range []string{"FAIL ok", "API_KEY=***", "GITHUB_TOKEN=***", "db_password=***", "Bearer ***", "https://***@registry.example.com/pkg", "PATH=/usr/bin COUNT=3", "ERR_MODULE_NOT_FOUND"} {
		if !strings.Contains(got, keep) {
			t.Errorf("tail lacks %q: %q", keep, got)
		}
	}
	// Masking and stripping happen before the bound, which stays rune-safe.
	long := strings.Repeat("가", 500) + " TOKEN=abc"
	if g := cleanTail(long); len([]rune(g)) > 300 || !utf8.ValidString(g) || strings.Contains(g, "abc") || !strings.HasSuffix(g, "TOKEN=***") {
		t.Fatalf("bounded masked tail wrong: %q", g)
	}
}

// A committed node_modules/ can replace the test runner itself (npx prefers
// node_modules/.bin), so any change under it needs approval even when the
// repo config does not list it.
func TestNodeModulesChangeAlwaysNeedsApproval(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	base := commit(t, e.repo, "base")
	write(t, e.repo, "node_modules/.bin/vitest", "#!/bin/sh\nexit 0\n")
	write(t, e.repo, "web/node_modules/x/index.js", "x\n")
	commit(t, e.repo, "add")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
	var ids []string
	for _, v := range r.Violations {
		ids = append(ids, v.IDs...)
	}
	if !strings.Contains(strings.Join(ids, " "), "node_modules/.bin/vitest") {
		t.Fatalf("violation does not name the node_modules file: %+v", r.Violations)
	}
}

// C1: a new .gitattributes marking tests as -diff must not hide a weakened
// assert (the diff used to show "Binary files differ" with no removed line).
func TestGitattributesCannotHideProtectedChange(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	write(t, e.repo, "tests/test_a.py", "def test_a():\n    assert add(1,2)==3\n")
	write(t, e.repo, ".gitattributes", "*.png binary\n")
	base := commit(t, e.repo, "base")
	// existing .gitattributes gets one ADDED line (not itself a removal)
	write(t, e.repo, ".gitattributes", "*.png binary\ntests/** -diff\n")
	write(t, e.repo, "tests/test_a.py", "def test_a():\n    assert True\n")
	commit(t, e.repo, "head")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") {
		t.Fatalf("got %s %+v", r.Category, r.Violations)
	}
}

// C1: deleting an empty protected file, changing a binary protected file
// (append only) and a mode-only change all need approval.
func TestNonLineProtectedChangesNeedApproval(t *testing.T) {
	cases := map[string]func(e env){
		"empty file deleted": func(e env) { git(t, e.repo, "rm", "-q", "tests/__init__.py") },
		"binary appended":    func(e env) { write(t, e.repo, "tests/golden.bin", "a\x00b\nc\x00d\n") },
		"mode only": func(e env) {
			if err := os.Chmod(filepath.Join(e.repo, "tests/run.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
			write(t, e.repo, "unit.flag", "")
			write(t, e.repo, "tests/__init__.py", "")
			write(t, e.repo, "tests/golden.bin", "a\x00b\n")
			write(t, e.repo, "tests/run.sh", "echo\n")
			base := commit(t, e.repo, "base")
			mutate(e)
			commit(t, e.repo, "head")
			r, err := Run(context.Background(), e.opts(base))
			if err != nil {
				t.Fatal(err)
			}
			if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") || len(r.Approvals) != 1 {
				t.Fatalf("got %s %+v approvals=%v", r.Category, r.Violations, r.Approvals)
			}
			id, _, _ := RepoID(e.repo)
			if err := (approval.Store{Path: ApprovalsPath(e.home)}).Grant(id, base, r.Approvals[0].Hash, time.Now()); err != nil {
				t.Fatal(err)
			}
			if r, err = Run(context.Background(), e.opts(base)); err != nil || r.Category != rules.CategoryOK {
				t.Fatalf("after approval: %v %s %+v", err, r.Category, r.Violations)
			}
		})
	}
}

// C1(c)/I3: files that change how the runner or its dependencies behave are
// guarded when NEW, whatever the repo config says.
func TestBuiltinGuardedNewFiles(t *testing.T) {
	for _, f := range []string{".gitattributes", ".gitmodules", ".npmrc", "web/.npmrc", "pytest.ini", "sub/pytest.ini",
		"tox.ini", "sub/tox.ini", "setup.cfg", "sub/setup.cfg", "go.work", "go.work.sum", "vendor/x/y.go",
		"sitecustomize.py", "lib/sitecustomize.py", "usercustomize.py", "requirements.txt", "requirements-dev.txt",
		"sub/requirements-test.txt", "package-lock.json", "web/package-lock.json", "yarn.lock", "pnpm-lock.yaml"} {
		t.Run(f, func(t *testing.T) {
			e := setup(t)
			write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
			write(t, e.repo, "unit.flag", "")
			base := commit(t, e.repo, "base")
			write(t, e.repo, f, "x\n")
			commit(t, e.repo, "add")
			r, err := Run(context.Background(), e.opts(base))
			if err != nil {
				t.Fatal(err)
			}
			if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") {
				t.Fatalf("new %s: got %s %+v", f, r.Category, r.Violations)
			}
		})
	}
}

// I2: every base run happens before any head run, so head code cannot poison
// shared state ($TMPDIR, caches) before a later check's base run.
func TestAllBaseRunsBeforeHeadRuns(t *testing.T) {
	e := setup(t)
	yml := "version: 1\nchecks:\n"
	for _, id := range []string{"one", "two"} {
		yml += "  - id: " + id + "\n    adapter: exit-code\n    command: [\"sh\", \"-c\", \"echo " + id + "-$(git rev-parse HEAD) >> " + e.log + "\"]\n    timeout: 20s\n"
	}
	write(t, e.repo, ".aigate.yml", yml)
	base := commit(t, e.repo, "base")
	head := commit(t, e.repo, "head")
	if _, err := Run(context.Background(), e.opts(base)); err != nil {
		t.Fatal(err)
	}
	want := []string{"one-" + base, "two-" + base, "one-" + head, "two-" + head}
	if got := lines(t, e.log); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("order\n got %v\nwant %v", got, want)
	}
}

// I5: cancelling mid-check is a gate error and leaves no worktree, no
// .git/worktrees entry, no report and no ledger line.
func TestCancelLeavesNothingBehind(t *testing.T) {
	e := setup(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	started := filepath.Join(t.TempDir(), "started")
	write(t, e.repo, ".aigate.yml", "version: 1\nchecks:\n  - id: unit\n    adapter: exit-code\n    command: [\"sh\", \"-c\", \"touch "+started+"; sleep 30\"]\n    timeout: 60s\n")
	base := commit(t, e.repo, "base")
	commit(t, e.repo, "head")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 400; i++ {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		cancel()
	}()
	t0 := time.Now()
	r, err := Run(ctx, e.opts(base))
	if err == nil || r.Category != "" {
		t.Fatalf("want gate error on cancel, got %v %+v", err, r)
	}
	if time.Since(t0) > 15*time.Second {
		t.Fatalf("cancel took %s", time.Since(t0))
	}
	if m, _ := filepath.Glob(filepath.Join(tmp, "aigate-wt-*")); len(m) != 0 {
		t.Fatalf("worktree dirs left: %v", m)
	}
	if out := git(t, e.repo, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Fatalf("worktree entries left: %s", out)
	}
	if _, err := os.Stat(filepath.Join(e.repo, ".aigate", "reports")); err == nil {
		t.Fatal("report written on cancel")
	}
	id, _, _ := RepoID(e.repo)
	if _, err := os.Stat(LedgerPath(e.home, id)); err == nil {
		t.Fatal("ledger written on cancel")
	}
}

// C2(d): the run_result event binds the saved report file and the config.
func TestLedgerBindsReportAndConfig(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	base := commit(t, e.repo, "base")
	head := commit(t, e.repo, "head")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.repo, ".aigate", "reports", head[:10]+".json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	id, _, _ := RepoID(e.repo)
	evs, err := ledger.Events(LedgerPath(e.home, id))
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %v %v", evs, err)
	}
	p := evs[0].Payload
	if p["report_sha256"] != hex.EncodeToString(sum[:]) || p["config_hash"] != r.ConfigHash || p["base"] != base || p["head"] != head {
		t.Fatalf("payload %+v", p)
	}
}

func checkProblems(t *testing.T, e env, base, path string) []string {
	t.Helper()
	p, err := CheckReport(CheckOptions{Repo: e.repo, Base: base, ReportPath: path, HomeDir: e.home})
	if err != nil {
		t.Fatalf("CheckReport: %v", err)
	}
	return p
}

// C2: check binds the report to the base, the repo, the base config and the
// ledger event of the run that saved it.
func TestCheckReportBindings(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	write(t, e.repo, "tests/t.txt", "one\ntwo\n")
	base := commit(t, e.repo, "base")
	mid := commit(t, e.repo, "mid")
	head := commit(t, e.repo, "head")
	if r, err := Run(context.Background(), e.opts(base)); err != nil || r.Category != rules.CategoryOK {
		t.Fatalf("run: %v %s", err, r.Category)
	}
	rp := filepath.Join(e.repo, ".aigate", "reports", head[:10]+".json")
	if p := checkProblems(t, e, base, rp); len(p) != 0 {
		t.Fatalf("fresh report rejected: %v", p)
	}
	git(t, e.repo, "tag", "start", base)
	if p := checkProblems(t, e, "start", rp); len(p) != 0 {
		t.Fatalf("base given by name rejected: %v", p)
	}
	if p := checkProblems(t, e, mid, rp); len(p) == 0 {
		t.Fatal("wrong --base accepted")
	}
	if _, err := CheckReport(CheckOptions{Repo: e.repo, ReportPath: rp, HomeDir: e.home}); err == nil {
		t.Fatal("check without base must be an error")
	}
	orig, _ := os.ReadFile(rp)
	// edited after the run (still a valid, consistent ok report)
	write(t, e.repo, ".aigate/reports/"+head[:10]+".json", strings.Replace(string(orig), `"tool_version": "0.1.0"`, `"tool_version": "0.1.1"`, 1))
	if p := checkProblems(t, e, base, rp); len(p) == 0 {
		t.Fatal("report edited after the run accepted")
	}
	// forged: a weakened run edited to ok while the violation remains
	write(t, e.repo, ".aigate/reports/"+head[:10]+".json", string(orig))
	write(t, e.repo, "tests/t.txt", "one\n")
	head2 := commit(t, e.repo, "weaken")
	r, err := Run(context.Background(), e.opts(base))
	if err != nil || r.Category != rules.CategoryWeakened {
		t.Fatalf("weakened run: %v %s", err, r.Category)
	}
	rp2 := filepath.Join(e.repo, ".aigate", "reports", head2[:10]+".json")
	b, _ := os.ReadFile(rp2)
	forged := strings.Replace(strings.Replace(string(b), `"category": "check_weakened",`, `"category": "ok",`, 1), `"exit_code": 3`, `"exit_code": 0`, 1)
	write(t, e.repo, ".aigate/reports/"+head2[:10]+".json", forged)
	if p := checkProblems(t, e, base, rp2); len(p) == 0 {
		t.Fatal("forged ok report accepted")
	}
	// a report written by hand with no run at all
	_, top, _ := RepoID(e.repo)
	hand := fmt.Sprintf(`{"schema_version":1,"category":"ok","exit_code":0,"repo":%q,"base":%q,"head":%q,"config_hash":%q,`+
		`"tool_version":"0.1.0","started_at":"","finished_at":"","checks":[],"violations":[],"waived":[],"approvals":[],"initial":false}`,
		top, base, head2, r.ConfigHash)
	write(t, e.repo, ".aigate/reports/"+head2[:10]+".json", hand)
	if p := checkProblems(t, e, base, rp2); len(p) == 0 {
		t.Fatal("hand-written report accepted")
	}
}

// C2: an initial-mode report is never completion evidence.
func TestCheckReportRejectsInitial(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	head := commit(t, e.repo, "only")
	o := e.opts("")
	o.Initial = true
	if r, err := Run(context.Background(), o); err != nil || r.Category != rules.CategoryOK || !r.Initial {
		t.Fatalf("initial run: %v %+v", err, r)
	}
	rp := filepath.Join(e.repo, ".aigate", "reports", head[:10]+".json")
	if p := checkProblems(t, e, head, rp); len(p) == 0 {
		t.Fatal("initial report accepted")
	}
}

// Decision 1 (user, 2026-11-04, option 1): adding lines to an EXISTING
// protected file needs approval; a brand-new protected-but-unguarded test
// file stays free (spec item [6]); approval of the exact hash clears it.
func TestPureAdditionToExistingProtectedFileNeedsApproval(t *testing.T) {
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	write(t, e.repo, "tests/test_a.py", "def test_a():\n    assert True\n")
	base := commit(t, e.repo, "base")
	write(t, e.repo, "tests/test_extra.py", "def test_extra():\n    assert True\n")
	newOnly := commit(t, e.repo, "new test file only")
	r, err := Run(context.Background(), Options{Repo: e.repo, Base: base, Head: newOnly, HomeDir: e.home})
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK || len(r.Approvals) != 0 {
		t.Fatalf("new unguarded test file must stay free: %s %+v", r.Category, r.Violations)
	}
	write(t, e.repo, "tests/test_a.py", "def test_a():\n    return\n    assert True\n") // pure insertion
	commit(t, e.repo, "insert early return")
	r, err = Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") || len(r.Approvals) != 1 {
		t.Fatalf("pure addition must need approval: %s %+v", r.Category, r.Violations)
	}
	var v rules.Violation
	for _, x := range r.Violations {
		if x.Rule == "protected_modified" {
			v = x
		}
	}
	if !strings.Contains(v.Detail, "tests/test_a.py") || strings.Contains(v.Detail, "tests/test_extra.py") || !strings.Contains(v.Detail, r.Approvals[0].Hash) {
		t.Fatalf("detail should name the modified existing file and the hash: %q", v.Detail)
	}
	id, _, _ := RepoID(e.repo)
	if err := (approval.Store{Path: ApprovalsPath(e.home)}).Grant(id, base, r.Approvals[0].Hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err = Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK {
		t.Fatalf("after approval: %s %+v", r.Category, r.Violations)
	}
}

// Re-review gap of I3: the nested (**/) variants of lock files, go.work,
// vendor/ and requirements/*.txt are built-in guarded too.
func TestNestedBuiltinGuardedFilesNeedApproval(t *testing.T) {
	for _, p := range []string{"web/yarn.lock", "web/pnpm-lock.yaml", "mod/go.work", "mod/go.work.sum", "mod/vendor/x/x.go",
		"requirements/dev.txt", "svc/requirements/base.txt"} {
		t.Run(p, func(t *testing.T) {
			e := setup(t)
			write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
			write(t, e.repo, "unit.flag", "")
			base := commit(t, e.repo, "base")
			write(t, e.repo, p, "x\n")
			commit(t, e.repo, "add")
			r, err := Run(context.Background(), e.opts(base))
			if err != nil {
				t.Fatal(err)
			}
			if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") {
				t.Fatalf("%s: got %s %+v", p, r.Category, r.Violations)
			}
		})
	}
}
