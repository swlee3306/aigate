package pathfold

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"
)

func groups(gs ...Group) []Group { return gs }

func g(key string, members ...string) Group { return Group{Key: key, Members: members} }

func TestCollisions(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  []Group
	}{
		{"ascii file", []string{"tests/Run.js", "tests/run.js"}, groups(g("tests/run.js", "tests/Run.js", "tests/run.js"))},
		{"ascii dir, no common file", []string{"Node_Modules/x/a.js", "node_modules/y/b.js"},
			groups(g("node_modules", "Node_Modules", "node_modules"))},
		{"dir collision inherited below is still listed by Collisions", []string{"Node_Modules/t/package.json", "node_modules/t/index.js"},
			groups(g("node_modules", "Node_Modules", "node_modules"), g("node_modules/t", "Node_Modules/t", "node_modules/t"))},
		{"long s (U+017F)", []string{"node_modules/t/index.js", "node_module\u017f/t/package.json"},
			groups(g("node_modules", "node_modules", "node_module\u017f"), g("node_modules/t", "node_modules/t", "node_module\u017f/t"))},
		{"kelvin sign (U+212A)", []string{"lib/\u212aey.js", "lib/key.js"}, groups(g("lib/key.js", "lib/key.js", "lib/\u212aey.js"))},
		{"NFC vs NFD", []string{"caf\u00e9/a.txt", "cafe\u0301/b.txt"}, groups(g("cafe\u0301", "cafe\u0301", "caf\u00e9"))},
		{"NFC upper vs NFD lower", []string{"CAF\u00c9.txt", "cafe\u0301.txt"}, groups(g("cafe\u0301.txt", "CAF\u00c9.txt", "cafe\u0301.txt"))},
		{"file vs directory", []string{"tests/run", "tests/Run/x.js"}, groups(g("tests/run", "tests/Run", "tests/run"))},
		{"nested", []string{"a/b/C/d/E.go", "a/b/c/d/e.go"},
			groups(g("a/b/c", "a/b/C", "a/b/c"), g("a/b/c/d", "a/b/C/d", "a/b/c/d"), g("a/b/c/d/e.go", "a/b/C/d/E.go", "a/b/c/d/e.go"))},
		{"no false positives", []string{"vendors/a.go", "vendor/a.go", "a_b/x", "a-b/x", "Readme", "Readme.md", "x/same.js", "y/same.js",
			"node_modules/t/index.js", "node_modules/t/package.json"}, nil},
		{"spaces, korean, control bytes", []string{"문서 폴더/A b.txt", "문서 폴더/a B.txt", "x/\x1b[31mZ", "x/\x1b[31mz"},
			groups(g("x/\x1b[31mz", "x/\x1b[31mZ", "x/\x1b[31mz"), g(norm.NFD.String("문서 폴더")+"/a b.txt", "문서 폴더/A b.txt", "문서 폴더/a B.txt"))},
		{"duplicate input path", []string{"a/b", "a/b"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Collisions(c.paths)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Collisions(%q)\n got  %q\n want %q", c.paths, got, c.want)
			}
		})
	}
}

func TestKeyIdempotentAndEquivalent(t *testing.T) {
	for _, s := range []string{"Node_Modules", "node_module\u017f", "\u212aey", "CAF\u00c9", "cafe\u0301", "Stra\u00dfe", "\u1e9e", "\u01c5", "abc", "한글", "\u0130"} {
		k := Key(s)
		if Key(k) != k {
			t.Errorf("Key not idempotent for %q: %q then %q", s, k, Key(k))
		}
	}
	eq := [][2]string{{"node_modules", "NODE_MODULES"}, {"node_modules", "node_module\u017f"}, {"key", "\u212aey"}, {"caf\u00e9", "CAFE\u0301"}, {"\u00c5", "A\u030a"}}
	for _, p := range eq {
		if Key(p[0]) != Key(p[1]) {
			t.Errorf("Key(%q)=%q != Key(%q)=%q", p[0], Key(p[0]), p[1], Key(p[1]))
		}
	}
	for _, p := range [][2]string{{"vendor", "vendors"}, {"a_b", "a-b"}, {"Readme", "Readme.md"}} {
		if Key(p[0]) == Key(p[1]) {
			t.Errorf("Key(%q) == Key(%q)", p[0], p[1])
		}
	}
}

func TestNew(t *testing.T) {
	base := []string{"Foo/a.js", "foo/b.js", "src/x.go"}
	head := append(append([]string{}, base...), "src/y.go")
	if got := New(Collisions(head), Collisions(base), same(head), same(base)); len(got) != 0 {
		t.Fatalf("identical pre-existing group must be ignored, got %q", got)
	}
	// a new member joins the pre-existing group
	head2 := append(append([]string{}, base...), "FOO/c.js")
	if got, want := New(Collisions(head2), Collisions(base), same(head2), same(base)), groups(g("foo", "FOO", "Foo", "foo")); !reflect.DeepEqual(got, want) {
		t.Fatalf("new member: got %q want %q", got, want)
	}
	// a new file under the pre-existing merged directory that shadows an
	// existing file there (same component spelling, different parent spelling)
	head3 := append(append([]string{}, base...), "Foo/b.js")
	if got, want := New(Collisions(head3), Collisions(base), same(head3), same(base)), groups(g("foo/b.js", "Foo/b.js", "foo/b.js")); !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow under pre-existing dir: got %q want %q", got, want)
	}
	// a new group: inherited child groups (same component spelling, parent
	// group new too) are folded into the parent group
	head4 := []string{"node_modules/t/index.js", "Node_Modules/t/package.json", "Node_Modules/t/lax.js"}
	if got, want := New(Collisions(head4), Collisions([]string{"node_modules/t/index.js"}), same(head4), same([]string{"node_modules/t/index.js"})), groups(g("node_modules", "Node_Modules", "node_modules")); !reflect.DeepEqual(got, want) {
		t.Fatalf("new dir group: got %q want %q", got, want)
	}
	// a child group whose own component spellings differ is kept
	head5 := []string{"node_modules/t/index.js", "Node_Modules/T/package.json"}
	if got, want := New(Collisions(head5), nil, same(head5), nil), groups(g("node_modules", "Node_Modules", "node_modules"), g("node_modules/t", "Node_Modules/T", "node_modules/t")); !reflect.DeepEqual(got, want) {
		t.Fatalf("differing child: got %q want %q", got, want)
	}
	// initial mode: every group counts
	if got := New(Collisions([]string{"a", "A"}), nil, same([]string{"a", "A"}), nil); len(got) != 1 {
		t.Fatalf("initial: got %q", got)
	}
}

func TestCollisionsPerformance(t *testing.T) {
	paths := make([]string, 0, 200_000)
	for i := 0; len(paths) < 200_000; i++ {
		paths = append(paths, fmt.Sprintf("packages/pkg%03d/src/module%02d/sub/dir/file%05d_%d.ts", i%500, i%37, i, i%7))
	}
	paths = append(paths, "packages/Pkg001/x.ts", "packages/pkg002/SRC/y.ts", "node_module\u017f/x/a.js", "node_modules/x/b.js")
	hs, bs := same(paths), same(paths[:200_000])
	t0 := time.Now()
	head := Collisions(paths)
	base := Collisions(paths[:200_000])
	n := New(head, base, hs, bs)
	el := time.Since(t0)
	if len(n) != 3 {
		t.Fatalf("want 3 new groups, got %d: %q", len(n), n)
	}
	if el > time.Second {
		t.Fatalf("200k paths took %v (> 1s)", el)
	}
	t.Logf("2 x %d paths: %v", len(paths), el)
}

// same gives every path and every directory prefix of paths the same
// signature, so New compares member names only (the wave-7 fixtures).
func same(paths []string) map[string]string {
	m := map[string]string{}
	for _, p := range paths {
		for i := 0; i < len(p); i++ {
			if p[i] == '/' {
				m[p[:i]] = "040000 tree"
			}
		}
		m[p] = "100644 blob"
	}
	return m
}

// FIX WAVE 8 (R2): a group is pre-existing only when its members AND every
// member's mode and object id are the same in base and head.
func TestNewSignatures(t *testing.T) {
	paths := []string{"src/sum.js", "src/Sum.js", "x.go"}
	gs := Collisions(paths)
	sig := func(over map[string]string) map[string]string {
		m := map[string]string{"src": "040000 aaaa", "src/sum.js": "100644 1111", "src/Sum.js": "100644 1111", "x.go": "100644 2222"}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	want := groups(g("src/sum.js", "src/Sum.js", "src/sum.js"))
	if got := New(gs, gs, sig(nil), sig(map[string]string{"x.go": "100644 3333"})); len(got) != 0 {
		t.Fatalf("identical members and blobs (only a non-member changed): pre-existing, got %q", got)
	}
	if got := New(gs, gs, sig(map[string]string{"src/Sum.js": "100644 9999"}), sig(nil)); !reflect.DeepEqual(got, want) {
		t.Fatalf("one member blob changed: want new %q, got %q", want, got)
	}
	if got := New(gs, gs, sig(map[string]string{"src/sum.js": "100755 1111"}), sig(nil)); !reflect.DeepEqual(got, want) {
		t.Fatalf("member mode changed: want new %q, got %q", want, got)
	}
	// a member with no signature on one side fails closed (new)
	if got := New(gs, gs, sig(nil), map[string]string{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("missing base signature: want new %q, got %q", want, got)
	}
	// directory members compare their tree ids
	dirs := Collisions([]string{"Foo/a.js", "foo/b.js"})
	ds := func(fooTree string) map[string]string {
		return map[string]string{"Foo": "040000 t1", "foo": "040000 " + fooTree, "Foo/a.js": "100644 a", "foo/b.js": "100644 b"}
	}
	if got := New(dirs, dirs, ds("t2"), ds("t2")); len(got) != 0 {
		t.Fatalf("unchanged directory group: got %q", got)
	}
	if got := New(dirs, dirs, ds("t3"), ds("t2")); len(got) != 1 || got[0].Key != "foo" {
		t.Fatalf("directory content changed: want group foo, got %q", got)
	}
}

// FIX WAVE 8 (R1/R3): KeyPath folds every component; FoldGlob folds the
// literal parts of a glob the same way and keeps the glob syntax.
func TestKeyPathAndFoldGlob(t *testing.T) {
	for in, want := range map[string]string{
		"go.worK":             "go.work",
		"pyteſt.ini":          "pytest.ini",
		"ſetup.cfg":           "setup.cfg",
		"requirementſ.txt":    "requirements.txt",
		"a/B/Conftest.py":     "a/b/conftest.py",
		"ﬆuff/x":              "stuff/x",
		"Straße.txt":          "strasse.txt",
		"node_moduleſ/T/i.js": "node_modules/t/i.js",
	} {
		if got := KeyPath(in); got != want {
			t.Errorf("KeyPath(%q) = %q, want %q", in, got, want)
		}
	}
	if KeyPath("café/x") != KeyPath("CAFÉ/X") {
		t.Errorf("KeyPath must equate NFC and NFD")
	}
	for in, want := range map[string]string{
		"**/conftest.py":     "**/conftest.py",
		"**/Conftest.PY":     "**/conftest.py",
		"requirements*.txt":  "requirements*.txt",
		"**/node_modules/**": "**/node_modules/**",
		"Docs/?ber*.md":      "docs/?ber*.md",
		"[A-Z]x/[!Q]y":       "[a-z]x/[!q]y",
		"lit\\*Star":         "lit\\*star",
		"Straße*":            "strasse*",
		"broken[AB":          "broken[ab",
		"pyteſt.ini":         "pytest.ini",
		"TÉST/**/*.Py":       "tést/**/*.py",
	} {
		if got := FoldGlob(in); got != want {
			t.Errorf("FoldGlob(%q) = %q, want %q", in, got, want)
		}
	}
}
