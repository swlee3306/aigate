package gate

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/pathfold"
	"aigate/internal/rules"
)

// FIX WAVE 8: the approval hash of a head WITHOUT a folded alias must not
// change. The literal was recorded with the wave-7 code (cb35145) before any
// wave-8 change: a modified protected file, a new user-guarded conftest.py,
// a new ASCII-icase built-in (Pytest.ini) and harmless new names.
func TestHashUnchangedWithoutFoldedAlias(t *testing.T) {
	e := collisionBase(t, map[string]string{"tests/a.txt": "a\n"})
	base := commit(t, e.repo, "base")
	head := plumb(t, e.repo, base, map[string]string{"tests/a.txt": "b\n", "conftest.py": "x\n", "Pytest.ini": "y\n",
		"Straße.txt": "s\n", "docs/Überblick.md": "u\n", "문서.txt": "k\n"})
	o := e.opts(base)
	o.Head = head
	p, err := Prepare(o)
	if err != nil {
		t.Fatal(err)
	}
	// Full SHA-256 since 2026-11-09; its first 16 hex are the old pinned value
	// 8082594eac4a51b7, so the hashed text itself did not change.
	const want = "8082594eac4a51b773457d7e4c3597e97f8d8697a34585ff69d8acc085c69611"
	if p.Diff.Hash != want {
		t.Fatalf("hash changed: got %s want %s\n%s", p.Diff.Hash, want, p.Diff.HashedText())
	}
}

// FIX WAVE 8 (R1/R3): fold-key matching of NEW paths against the built-in
// guard list and the user's approval_on_add list. listed is what git's own
// pathspec matching already found (never reported again as an alias).
func TestFoldAliases(t *testing.T) {
	user := []string{"**/conftest.py", "**/fixtures/**"}
	cases := []struct {
		path   string
		listed bool
		alias  bool
	}{
		{"go.worK", false, true},                 // Kelvin sign → go.work
		{"sub/go.worK", false, true},             // **/go.work
		{"pyteſt.ini", false, true},              // long s → pytest.ini
		{"ſetup.cfg", false, true},               // setup.cfg
		{"requirementſ.txt", false, true},        // requirements*.txt
		{"pyteﬆ.ini", false, true},               // "ﬆ" ligature folds to "st"
		{"tests/ﬁxtures/data.json", false, true}, // "ﬁ" ligature, user **/fixtures/**
		{"package-locK.json", false, true},       // package-lock.json
		{"node_moduleſ/t/index.js", false, true}, // **/node_modules/**
		{"Conftest.py", false, true},             // user glob, case only
		{"a/b/Conftest.py", false, true},         // nested
		{"conftest.py", true, false},             // git matched it already
		{"Go.work", true, false},                 // git icase matched it already
		{"Straße.txt", false, false},             // folds to ASCII, no guard glob
		{"docs/Überblick.md", false, false},
		{"문서/보고서.md", false, false},
		{"src/main.go", false, false},
		{"pytest.ini.bak", false, false},
	}
	var added, listed, want []string
	for _, c := range cases {
		added = append(added, c.path)
		if c.listed {
			listed = append(listed, c.path)
		}
		if c.alias {
			want = append(want, c.path)
		}
	}
	got := foldAliases(added, listed, BuiltinGuarded, user)
	want = sortedCopy(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("foldAliases:\n got  %q\n want %q", got, want)
	}
	// user globs only through the user list
	if got := foldAliases([]string{"Conftest.py"}, nil, BuiltinGuarded, nil); len(got) != 0 {
		t.Fatalf("Conftest.py without the user glob: got %q", got)
	}
}

// aliasHead builds base (the checkYAML repo) and a plumbed head adding files.
func aliasHead(t *testing.T, files map[string]string) (env, string, Options) {
	t.Helper()
	e := collisionBase(t, nil)
	base := commit(t, e.repo, "base")
	o := e.opts(base)
	o.Head = plumb(t, e.repo, base, files)
	return e, base, o
}

// A new folded alias of a guarded name is a guarded added file: approval
// needed, checks run as usual (not a collision), the hash covers the exact
// path and content, and granting that hash makes the same head ok.
func TestFoldedAliasNeedsApproval(t *testing.T) {
	for _, f := range []string{"go.worK", "pyteſt.ini", "ſetup.cfg", "requirementſ.txt", "package-locK.json", "tests/Conftest.py", "a/b/Conftest.py"} {
		t.Run(f, func(t *testing.T) {
			e, base, o := aliasHead(t, map[string]string{f: "redirect\n"})
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") || hasRule(r.Violations, "path_case_collision") {
				t.Fatalf("new %q: got %s %+v", f, r.Category, r.Violations)
			}
			if got := lines(t, e.log); len(got) != 2 {
				t.Fatalf("checks must run on base and head, ran %v", got)
			}
			p, err := Prepare(o)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p.AddedGuarded, []string{f}) || !reflect.DeepEqual(p.Diff.AddedFiles, []string{f}) ||
				!strings.Contains(p.Diff.HashedText(), "A "+f+"\n") || !strings.Contains(p.Diff.Text, "+redirect") || p.Diff.Hash != r.Approvals[0].Hash {
				t.Fatalf("approval hash must cover %q: guarded %q added %q hashed %q", f, p.AddedGuarded, p.Diff.AddedFiles, p.Diff.HashedText())
			}
			// other content → other hash
			o2 := o
			o2.Head = plumb(t, e.repo, base, map[string]string{f: "other\n"})
			p2, err := Prepare(o2)
			if err != nil {
				t.Fatal(err)
			}
			if p2.Diff.Hash == p.Diff.Hash {
				t.Fatalf("hash must change with the alias content")
			}
			id, _, _ := RepoID(e.repo)
			if err := (approval.Store{Path: ApprovalsPath(e.home)}).Grant(id, base, p.Diff.Hash, time.Now()); err != nil {
				t.Fatal(err)
			}
			if r, err = Run(context.Background(), o); err != nil || r.Category != rules.CategoryOK {
				t.Fatalf("approved alias: got %s %+v %v", r.Category, r.Violations, err)
			}
			if r, err = Run(context.Background(), o2); err != nil || r.Category != rules.CategoryWeakened {
				t.Fatalf("approval must not cover other content: got %s %v", r.Category, err)
			}
		})
	}
}

// Legitimate non-ASCII names that fold to ASCII but match no guard glob stay ok.
func TestFoldedNonGuardNamesStayOK(t *testing.T) {
	_, _, o := aliasHead(t, map[string]string{"Straße.txt": "s\n", "docs/Überblick.md": "u\n", "문서/보고서.md": "k\n", "café.txt": "c\n"})
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK || len(r.Violations) != 0 || len(r.Approvals) != 0 {
		t.Fatalf("got %s %+v approvals %v", r.Category, r.Violations, r.Approvals)
	}
	p, err := Prepare(o)
	if err != nil || len(p.AddedGuarded) != 0 || len(p.Diff.TouchedFiles) != 0 {
		t.Fatalf("Prepare: guarded %q touched %q err %v", p.AddedGuarded, p.Diff.TouchedFiles, err)
	}
}

// FIX WAVE 8 (R2): a collision group that existed in base counts as new
// when a member's content changed; unchanged it stays pre-existing.
func TestPreexistingCollisionGroupContentChange(t *testing.T) {
	e := collisionBase(t, nil)
	seed := commit(t, e.repo, "seed")
	base := plumb(t, e.repo, seed, map[string]string{"src/sum.js": "a + b\n", "src/Sum.js": "a + b\n"})
	o := e.opts(base)
	o.Head = plumb(t, e.repo, base, map[string]string{"other.txt": "x\n"})
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Category != rules.CategoryOK {
		t.Fatalf("unchanged pre-existing group: got %s %+v", r.Category, r.Violations)
	}
	o.Head = plumb(t, e.repo, base, map[string]string{"src/Sum.js": "a - b\n"})
	if err := os.WriteFile(e.log, nil, 0o644); err != nil { // forget the ok run's check calls
		t.Fatal(err)
	}
	collisionRun(t, e, o, "src/Sum.js", "src/sum.js")
}

// FIX WAVE 8 (R4): non-ASCII members are listed with their code points so
// NFC and NFD spellings can be told apart; ASCII groups print as before.
func TestCollisionListCodePoints(t *testing.T) {
	nfc, nfd := "문서", "문서"
	got := CollisionList([]pathfold.Group{{Key: nfd, Members: []string{nfd, nfc}}})
	for _, s := range []string{"[U+BB38 U+C11C]", "[U+1106 U+116E U+11AB U+1109 U+1165]"} {
		if !strings.Contains(got, s) {
			t.Errorf("%q lacks %q", got, s)
		}
	}
	if got := CollisionList([]pathfold.Group{{Key: "tests/keys.js", Members: []string{"tests/keys.js", "tests/Keys.js"}}}); got != "{tests/keys.js [ASCII], tests/Keys.js [U+212A]}" {
		t.Errorf("mixed group: %q", got)
	}
	if got := CollisionList([]pathfold.Group{{Key: "tests/run.js", Members: []string{"tests/Run.js", "tests/run.js"}}}); got != "{tests/Run.js, tests/run.js}" {
		t.Errorf("ASCII group must print as before: %q", got)
	}
	got = CollisionList([]pathfold.Group{{Key: "x", Members: []string{"x/\x1b[2Jé", "x/\x1b[2Jé", "x/\xff‮"}}})
	if strings.ContainsAny(got, "\x1b\xff‮") || !strings.Contains(got, "[U+00E9]") || !strings.Contains(got, "[U+0301]") {
		t.Errorf("escaped rendering: %q", got)
	}
}
