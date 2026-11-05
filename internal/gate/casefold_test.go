package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/gittest"
	"aigate/internal/ledger"
	"aigate/internal/rules"
)

// plumb builds a child commit of parent holding files via git plumbing
// (gittest.Plumb): colliding spellings survive, the main tree stays clean.
func plumb(t *testing.T, repo, parent string, files map[string]string) string {
	t.Helper()
	return gittest.Plumb(t, repo, parent, files)
}

// collisionRun asserts the verdict of a run on a colliding head: only
// path_case_collision (verification_incomplete), no check executed (base or
// head), report saved and the ledger's run_result names the rule.
func collisionRun(t *testing.T, e env, o Options, wantInDetail ...string) {
	t.Helper()
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryIncomplete || r.ExitCode != 2 || len(r.Violations) != 1 || r.Violations[0].Rule != "path_case_collision" {
		t.Fatalf("want only path_case_collision (verification_incomplete), got %s %+v", r.Category, r.Violations)
	}
	d := r.Violations[0].Detail
	for _, s := range append([]string{"대소문자", "정규화", "덮어쓰", "커밋된 트리", "이름을 고치"}, wantInDetail...) {
		if !strings.Contains(d, s) {
			t.Errorf("detail lacks %q: %q", s, d)
		}
	}
	if strings.ContainsAny(d, "\x1b\x00\r\n") || strings.ContainsAny(r.Markdown(), "\x1b\x00") {
		t.Errorf("raw control bytes in detail/markdown: %q", d)
	}
	if got := lines(t, e.log); len(got) != 0 {
		t.Fatalf("checks must not run on a colliding head, ran %v", got)
	}
	if len(r.Checks) != 0 {
		t.Fatalf("no check results expected: %+v", r.Checks)
	}
	if _, err := os.Stat(filepath.Join(e.repo, ".aigate", "reports", r.Head[:10]+".json")); err != nil {
		t.Fatalf("report not saved: %v", err)
	}
	id, _, _ := RepoID(e.repo)
	evs, err := ledger.Events(LedgerPath(e.home, id))
	if err != nil || len(evs) == 0 {
		t.Fatalf("ledger: %v %v", evs, err)
	}
	last := evs[len(evs)-1]
	if last.Type != "run_result" || last.Payload["category"] != string(rules.CategoryIncomplete) || fmt.Sprint(last.Payload["rules"]) != "[path_case_collision]" {
		t.Fatalf("ledger event: %+v", last)
	}
}

func collisionBase(t *testing.T, files map[string]string) env {
	t.Helper()
	e := setup(t)
	write(t, e.repo, ".aigate.yml", checkYAML(e.log, "", "unit"))
	write(t, e.repo, "unit.flag", "")
	for p, c := range files {
		write(t, e.repo, p, c)
	}
	return e
}

// FIX WAVE 7 (B): the three attack shapes the wave-6 re-reviewer reproduced.
func TestPathCaseCollisionAttacks(t *testing.T) {
	cases := []struct {
		name   string
		base   map[string]string
		head   map[string]string
		detail []string
	}{
		{"Node_Modules shadow", map[string]string{"node_modules/tinyassert/index.js": "module.exports = require('assert')\n"},
			map[string]string{"Node_Modules/tinyassert/package.json": `{"main":"lax.js"}` + "\n", "Node_Modules/tinyassert/lax.js": "module.exports = () => {}\n"},
			[]string{"Node_Modules", "node_modules"}},
		{"Go Vendor shadow", map[string]string{"vendor/example.com/dep/dep.go": "package dep\n"},
			map[string]string{"Vendor/example.com/dep/zz.go": "package dep\nimport \"os\"\nfunc init() { os.Exit(0) }\n"},
			[]string{"Vendor", "vendor"}},
		{"protected test file overwritten", map[string]string{"tests/Run.js": "strong\n"},
			map[string]string{"tests/run.js": "weak\n"}, []string{"tests/Run.js", "tests/run.js"}},
		{"long s folds to s", map[string]string{"node_modules/tinyassert/index.js": "x\n"},
			map[string]string{"node_module\u017f/tinyassert/package.json": `{"main":"lax.js"}` + "\n", "node_module\u017f/tinyassert/lax.js": "\n"},
			[]string{"node_module\u017f"}},
		{"kelvin sign folds to k", map[string]string{"tests/keys.js": "strong\n"},
			map[string]string{"tests/\u212aeys.js": "weak\n"}, []string{"tests/\u212aeys.js"}},
		{"NFD vs NFC", map[string]string{"tests/caf\u00e9.js": "strong\n"},
			map[string]string{"tests/cafe\u0301.js": "weak\n"}, nil},
		{"control bytes escaped", map[string]string{"tests/a.js": "x\n"},
			map[string]string{"x/\x1b[2JZ": "a\n", "x/\x1b[2Jz": "b\n"}, []string{`\x1b`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := collisionBase(t, c.base)
			base := commit(t, e.repo, "base")
			head := plumb(t, e.repo, base, c.head)
			o := e.opts(base)
			o.Head = head
			collisionRun(t, e, o, c.detail...)
			// approve's Prepare sees the same collision
			p, err := Prepare(o)
			if err != nil || len(p.Collisions) == 0 {
				t.Fatalf("Prepare: collisions %v err %v", p.Collisions, err)
			}
		})
	}
}

// Up to 5 groups are listed, then the count of the rest.
func TestPathCaseCollisionListsFiveGroups(t *testing.T) {
	e := collisionBase(t, nil)
	base := commit(t, e.repo, "base")
	files := map[string]string{}
	for i := 0; i < 7; i++ {
		files[fmt.Sprintf("d%d/A.txt", i)] = "1\n"
		files[fmt.Sprintf("d%d/a.txt", i)] = "2\n"
	}
	head := plumb(t, e.repo, base, files)
	o := e.opts(base)
	o.Head = head
	collisionRun(t, e, o, "d0/A.txt", "d4/a.txt", "7", "2")
	r, _ := Run(context.Background(), o)
	if strings.Contains(r.Violations[0].Detail, "d5/") || strings.Contains(r.Violations[0].Detail, "d6/") {
		t.Fatalf("more than 5 groups listed: %q", r.Violations[0].Detail)
	}
}

// A collision that exists identically at base is pre-existing: ignored (the
// checks run as usual); a new member joining it is reported.
func TestPathCaseCollisionPreExisting(t *testing.T) {
	e := collisionBase(t, nil)
	seed := commit(t, e.repo, "seed")
	base := plumb(t, e.repo, seed, map[string]string{"Docs/a.md": "a\n", "docs/b.md": "b\n"})
	head := plumb(t, e.repo, base, map[string]string{"src/new.go": "package src\n"})
	o := e.opts(base)
	o.Head = head
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK || len(lines(t, e.log)) != 2 {
		t.Fatalf("pre-existing collision must be ignored: %s %+v log %v", r.Category, r.Violations, lines(t, e.log))
	}
	head2 := plumb(t, e.repo, base, map[string]string{"DOCS/c.md": "c\n"})
	e2 := env{repo: e.repo, home: t.TempDir(), log: filepath.Join(t.TempDir(), "calls.log")}
	o2 := e2.opts(base)
	o2.Head = head2
	_ = os.WriteFile(e.log, nil, 0o644)
	r2, err := Run(context.Background(), o2)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Category != rules.CategoryIncomplete || !hasRule(r2.Violations, "path_case_collision") || !strings.Contains(r2.Violations[0].Detail, "DOCS") {
		t.Fatalf("new member: %s %+v", r2.Category, r2.Violations)
	}
	if got := lines(t, e.log); len(got) != 0 {
		t.Fatalf("checks ran: %v", got)
	}
}

// Initial mode: every collision in head counts (no base to compare with).
func TestPathCaseCollisionInitialMode(t *testing.T) {
	e := collisionBase(t, nil)
	seed := commit(t, e.repo, "seed")
	head := plumb(t, e.repo, seed, map[string]string{"tests/Run.js": "a\n", "tests/run.js": "b\n"})
	o := e.initialOpts()
	o.Head = head
	collisionRun(t, e, o, "tests/Run.js", "tests/run.js")
}

// A clean repo (similar but distinct names) is unaffected.
func TestNoCollisionForDistinctNames(t *testing.T) {
	e := collisionBase(t, map[string]string{"vendor/a.go": "package a\n", "tests/Readme": "x\n", "a_b/x": "1\n", "x/same.js": "1\n"})
	base := commit(t, e.repo, "base")
	head := plumb(t, e.repo, base, map[string]string{"vendors/a.go": "package a\n", "tests/Readme.md": "x\n", "a-b/x": "1\n", "y/same.js": "1\n"})
	o := e.opts(base)
	o.Head = head
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	// tests/Readme.md is a new protected-only file: free (item [6])
	if r.Category != rules.CategoryOK || len(lines(t, e.log)) != 2 {
		t.Fatalf("got %s %+v log %v", r.Category, r.Violations, lines(t, e.log))
	}
}

// FIX WAVE 7 (A): built-in guards match case-insensitively; the approval
// hash covers the file; user globs stay case-sensitive.
func TestBuiltinGuardedICase(t *testing.T) {
	for _, f := range []string{".NPMRC", "Package-Lock.json", "Pytest.ini", ".Gitattributes", "web/Yarn.lock", "Go.Work", "Requirements-Dev.txt", "lib/SiteCustomize.py"} {
		t.Run(f, func(t *testing.T) {
			e := collisionBase(t, nil)
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
			p, err := Prepare(e.opts(base))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.AddedGuarded) != 1 || p.AddedGuarded[0] != f || !strings.Contains(p.Diff.HashedText(), "A "+f+"\n") || p.Diff.Hash != r.Approvals[0].Hash {
				t.Fatalf("approval hash must cover %s: guarded %q hashed %q", f, p.AddedGuarded, p.Diff.HashedText())
			}
		})
	}
}

func TestUserGlobStaysCaseSensitive(t *testing.T) {
	e := collisionBase(t, nil)
	base := commit(t, e.repo, "base")
	write(t, e.repo, "Tests/x.py", "x\n")
	commit(t, e.repo, "add")
	p, err := Prepare(e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Diff.TouchedFiles) != 0 || len(p.Collisions) != 0 {
		t.Fatalf("user glob tests/** must not match Tests/x.py: %q %v", p.Diff.TouchedFiles, p.Collisions)
	}
}
