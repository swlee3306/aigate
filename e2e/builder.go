// Package e2e builds real git repositories for end-to-end gate scenarios.
package e2e

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/report"
	"aigate/internal/rules"
)

//go:embed testdata/py/* testdata/go/* testdata/ts/*
var pyData embed.FS

// DefaultPyBin is the virtualenv bin directory holding python with pytest.
const DefaultPyBin = "/Users/sampleuser/Desktop/개인/커리어/study/GPU/lab/code/verify-harness/.venv/bin"

// PyBin returns the directory that must be on PATH, or an error when it has
// no python executable.
func PyBin() (string, error) {
	d := os.Getenv("AIGATE_E2E_PYBIN")
	if d == "" {
		d = DefaultPyBin
	}
	st, err := os.Stat(filepath.Join(d, "python"))
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return "", fmt.Errorf("no executable python in %s (set AIGATE_E2E_PYBIN)", d)
	}
	return d, nil
}

func asset(name string) string { return stackAsset("py", name) }

// stackAsset reads an embedded asset of one stack (py, go or ts).
func stackAsset(stack, name string) string {
	b, err := pyData.ReadFile("testdata/" + stack + "/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Stack describes one language toy project: how its base commit is laid out
// and what an "ok" run must at least pass. A nil Stack means Python.
type Stack struct {
	Name string
	// Seed writes the base files (including .aigate.yml with the given yaml).
	Seed func(w *Workspace, yaml string)
	// YAML renders the default .aigate.yml for a check timeout ("" = default).
	YAML func(timeout string) string
	// MinPassed is the head pass count every ok scenario must reach by default.
	MinPassed int
	// Prepare checks that the environment can run this stack (nil = nothing).
	// A non-nil error fails every scenario of the stack, never skips it.
	Prepare func() error
}

// PyStack is the Python/pytest toy (9 tests).
var PyStack = &Stack{
	Name: "py",
	Seed: func(w *Workspace, yaml string) {
		w.Write("src/ingest.py", asset("ingest.py"))
		w.Write("tests/test_ingest.py", asset("test_ingest.py"))
		w.Write("pytest.ini", "[pytest]\npythonpath = src\n")
		w.Write(".aigate.yml", yaml)
	},
	YAML:      BaseYAML,
	MinPassed: 9,
}

// BaseYAML renders the scenario repo config.
func BaseYAML(timeout string) string {
	if timeout == "" {
		timeout = "60s"
	}
	return "version: 1\nprotected:\n  - \"tests/**\"\n  - \"pytest.ini\"\n  - \"pyproject.toml\"\n  - \".aigate.yml\"\n" +
		"approval_on_add:\n  - \"**/conftest.py\"\n  - \"**/testutils/**\"\n  - \"**/fixtures/**\"\n" +
		"checks:\n  - id: unit\n    preset: pytest\n    timeout: " + timeout + "\n"
}

// Workspace is a scenario git repository with its own aigate home.
type Workspace struct {
	Stack      *Stack
	T          *testing.T
	Dir, Home  string
	Base, Head string
}

// NewWorkspace creates the repo and commits the base layout (all 9 tests pass).
// yaml overrides the .aigate.yml content when non-empty.
func NewWorkspace(t *testing.T, timeout, yaml string) *Workspace {
	t.Helper()
	return NewStackWorkspace(t, PyStack, timeout, yaml)
}

// NewStackWorkspace creates the repo for a stack and commits its base layout.
func NewStackWorkspace(t *testing.T, st *Stack, timeout, yaml string) *Workspace {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &Workspace{Stack: st, T: t, Dir: d, Home: h}
	w.Git("init", "-q", "-b", "main")
	if yaml == "" {
		yaml = st.YAML(timeout)
	}
	st.Seed(w, yaml)
	w.Base = w.Commit("base")
	return w
}

// Git runs git in the workspace with a fixed identity.
func (w *Workspace) Git(args ...string) string {
	w.T.Helper()
	full := append([]string{"-C", w.Dir, "-c", "user.name=e2e", "-c", "user.email=e2e@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		w.T.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Commit stages everything and commits (allowing empty), returning the SHA.
func (w *Workspace) Commit(msg string) string {
	w.T.Helper()
	w.Git("add", "-A")
	w.Git("commit", "-q", "--allow-empty", "-m", msg)
	return w.Git("rev-parse", "HEAD")
}

// Write creates or overwrites a file.
func (w *Workspace) Write(name, content string) {
	w.T.Helper()
	p := filepath.Join(w.Dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.T.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.T.Fatal(err)
	}
}

// Read returns a file's content.
func (w *Workspace) Read(name string) string {
	w.T.Helper()
	b, err := os.ReadFile(filepath.Join(w.Dir, name))
	if err != nil {
		w.T.Fatal(err)
	}
	return string(b)
}

// Replace replaces the first occurrence of old; a missing pattern fails loudly.
func (w *Workspace) Replace(name, old, new string) {
	w.T.Helper()
	s := w.Read(name)
	if !strings.Contains(s, old) {
		w.T.Fatalf("%s: pattern not found: %q", name, old)
	}
	w.Write(name, strings.Replace(s, old, new, 1))
}

// ReplaceAll replaces every occurrence of old; a missing pattern fails loudly.
func (w *Workspace) ReplaceAll(name, old, new string) {
	w.T.Helper()
	s := w.Read(name)
	if !strings.Contains(s, old) {
		w.T.Fatalf("%s: pattern not found: %q", name, old)
	}
	w.Write(name, strings.ReplaceAll(s, old, new))
}

// Mutant applies a mutant's replacements (once each) to src/ingest.py.
func (w *Workspace) Mutant(id string) {
	w.T.Helper()
	var m map[string][][2]string
	if err := json.Unmarshal([]byte(asset("mutants.json")), &m); err != nil {
		w.T.Fatal(err)
	}
	reps, ok := m[id]
	if !ok {
		w.T.Fatalf("unknown mutant %q", id)
	}
	for _, r := range reps {
		w.Replace("src/ingest.py", r[0], r[1])
	}
}

// Result is one recorded scenario outcome.
type Result struct {
	Stack      string   `json:"stack"`
	Name       string   `json:"name"`
	Want       string   `json:"want"`
	Got        string   `json:"got"`
	Seconds    float64  `json:"seconds"`
	Rules      []string `json:"rules"`
	HeadPassed int      `json:"head_passed"`
	HeadFailed int      `json:"head_failed"`
	Note       string   `json:"note,omitempty"`
	Match      bool     `json:"match"`
	// KnownGap marks a documented limitation: Want is the observed verdict
	// of an attack the v1 gate cannot catch, not what a correct gate says.
	KnownGap        bool   `json:"known_gap,omitempty"`
	DecisionPending bool   `json:"decision_pending,omitempty"`
	CorrectWant     string `json:"correct_want,omitempty"`
	FirstWant       string `json:"first_want,omitempty"`
	Source          string `json:"source,omitempty"`
}

var (
	resMu   sync.Mutex
	results []Result
)

// Results returns the recorded outcomes.
func Results() []Result {
	resMu.Lock()
	defer resMu.Unlock()
	return append([]Result(nil), results...)
}

// Scenario is one end-to-end case. Mutate edits the working tree after the
// base commit; the builder commits it as head. Post runs after the head commit
// (e.g. to dirty the tree).
type Scenario struct {
	Name       string
	Mutate     func(w *Workspace)
	Post       func(w *Workspace)
	Want       rules.Category
	WantRules  []string
	WantDetail string // substring expected in some violation detail
	Timeout    string // check timeout in .aigate.yml (default 60s)
	Config     string // full .aigate.yml override at base
	Stack      *Stack // nil = Python
	// MinHeadPassed overrides Stack.MinPassed for ok scenarios.
	MinHeadPassed int
	// HeadMayFail opts out of the hardening that a check_weakened scenario
	// must have a head whose first check has no failed test (i.e. the weakening
	// really made a broken change pass). Use it only where head failures are
	// part of the scenario.
	HeadMayFail bool
	Approve     bool // grant the real protected-diff hash and run again
	WrongHash   bool // grant a wrong hash and run again
	// KnownGap marks an attack the v1 invariant gate inherently cannot catch.
	// Want is then the OBSERVED verdict (usually ok), CorrectWant what a
	// correct gate should say; the scenario is excluded from the
	// "weakened/incomplete never ok" count and listed as a documented limit.
	KnownGap    bool
	CorrectWant rules.Category
	// DecisionPending marks a KnownGap that is NOT inherent: a rule could
	// catch it, but doing so changes user-confirmed behaviour, so it awaits a
	// decision. Since the user decision of 2026-11-04 (option 1) no scenario
	// is decision-pending; the former ones are caught (see caught()).
	DecisionPending bool
	// FirstWant records the original expectation literal when it was amended
	// after the first run (both non-ok; reason in Note). Disclosed in 04.
	FirstWant rules.Category
	Source    string // "self", "codex" or "review" for adversarial scenarios
	Note      string
	// AfterRun, when set, runs after the final gate run (e.g. a `check` of
	// the saved report); a non-nil error is a scenario mismatch.
	AfterRun func(w *Workspace, r report.Report) error
}

func (w *Workspace) run() (report.Report, float64) {
	w.T.Helper()
	t0 := time.Now()
	r, err := gate.Run(context.Background(), gate.Options{Repo: w.Dir, Base: w.Base, Head: w.Head, HomeDir: w.Home})
	sec := time.Since(t0).Seconds()
	if err != nil {
		w.T.Fatalf("gate.Run error: %v", err)
	}
	return r, sec
}

func ruleNames(r report.Report) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range r.Violations {
		if !seen[v.Rule] {
			seen[v.Rule] = true
			out = append(out, v.Rule)
		}
	}
	return out
}

func hasRule(r report.Report, rule string) bool {
	for _, v := range r.Violations {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

// RunScenario builds the repo, applies the change, commits head, runs the gate
// and checks the verdict against the scenario's expectation (never edited to
// fit the outcome). Every outcome is recorded in Results.
func RunScenario(t *testing.T, s Scenario) {
	t.Helper()
	st := s.Stack
	if st == nil {
		st = PyStack
	}
	if st.Prepare != nil {
		if err := st.Prepare(); err != nil {
			resMu.Lock()
			results = append(results, Result{Stack: st.Name, Name: s.Name, Want: string(s.Want), Got: "환경 미충족", Note: err.Error()})
			resMu.Unlock()
			t.Fatalf("환경 미충족 (%s): %v", st.Name, err)
		}
	}
	w := NewStackWorkspace(t, st, s.Timeout, s.Config)
	s.Mutate(w)
	w.Head = w.Commit("head")
	if s.Post != nil {
		s.Post(w)
	}

	r, sec := w.run()
	if s.Approve || s.WrongHash {
		if r.Category != rules.CategoryWeakened || !hasRule(r, "protected_modified") || len(r.Approvals) == 0 {
			t.Errorf("first run: want check_weakened with protected_modified and an approval hash, got %s %v approvals=%v", r.Category, ruleNames(r), r.Approvals)
		}
		hash := "0000000000000000000000000000000000000000000000000000000000000000"
		if s.Approve && len(r.Approvals) > 0 {
			hash = r.Approvals[0].Hash
		}
		id, _, err := gate.RepoID(w.Dir)
		if err != nil {
			t.Fatal(err)
		}
		grant(t, w, id, hash)
		var sec2 float64
		r, sec2 = w.run()
		sec += sec2
	}

	res := Result{Stack: st.Name, Name: s.Name, Want: string(s.Want), Got: string(r.Category), Seconds: sec, Rules: ruleNames(r), Match: true,
		KnownGap: s.KnownGap, DecisionPending: s.DecisionPending, CorrectWant: string(s.CorrectWant), FirstWant: string(s.FirstWant),
		Source: s.Source, Note: s.Note}
	if s.DecisionPending && !s.KnownGap {
		t.Errorf("DecisionPending requires KnownGap")
	}
	if s.KnownGap {
		if s.CorrectWant == "" || s.CorrectWant == s.Want {
			t.Errorf("known-gap scenario must state a CorrectWant different from the observed Want")
		}
		t.Logf("KNOWN GAP: observed %s, a correct gate would say %s — %s", r.Category, s.CorrectWant, s.Note)
	}
	if len(r.Checks) > 0 {
		res.HeadPassed = r.Checks[0].Head.Counts.Passed
		res.HeadFailed = r.Checks[0].Head.Counts.Failed
	}
	fail := func(format string, a ...any) {
		res.Match = false
		t.Errorf(format, a...)
	}
	if r.Category != s.Want {
		fail("category: want %s, got %s (violations %+v)", s.Want, r.Category, r.Violations)
	}
	if r.ExitCode != s.Want.ExitCode() {
		fail("exit code: want %d, got %d", s.Want.ExitCode(), r.ExitCode)
	}
	for _, rn := range s.WantRules {
		if !hasRule(r, rn) {
			fail("missing rule %q in %v (violations %+v)", rn, ruleNames(r), r.Violations)
		}
	}
	if s.WantDetail != "" {
		found := false
		for _, v := range r.Violations {
			found = found || strings.Contains(v.Detail, s.WantDetail)
		}
		if !found {
			fail("no violation detail contains %q (violations %+v)", s.WantDetail, r.Violations)
		}
	}
	if s.Want == rules.CategoryOK {
		min := s.MinHeadPassed
		if min == 0 {
			min = st.MinPassed
		}
		if len(r.Violations) != 0 {
			fail("ok scenario has violations: %+v", r.Violations)
		}
		if res.HeadPassed < min {
			fail("ok scenario head passed %d < %d", res.HeadPassed, min)
		}
	}
	if s.AfterRun != nil {
		if err := s.AfterRun(w, r); err != nil {
			fail("after run: %v", err)
		}
	}
	if s.Want == rules.CategoryWeakened && !s.HeadMayFail && len(r.Checks) > 0 && res.HeadFailed != 0 {
		fail("weakened scenario head has %d failed tests; the weakening should make a broken head pass", res.HeadFailed)
	}
	resMu.Lock()
	results = append(results, res)
	resMu.Unlock()
}

// Record adds an outcome of a test that is not a RunScenario (e.g. a gate
// error or a `check` verdict) to Results.
func Record(r Result) {
	resMu.Lock()
	defer resMu.Unlock()
	results = append(results, r)
}

func grant(t *testing.T, w *Workspace, repoID, hash string) {
	t.Helper()
	st := approval.Store{Path: gate.ApprovalsPath(w.Home)}
	if err := st.Grant(repoID, w.Base, hash, time.Now()); err != nil {
		t.Fatalf("grant: %v", err)
	}
}
