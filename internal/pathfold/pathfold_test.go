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
	if got := New(Collisions(head), Collisions(base)); len(got) != 0 {
		t.Fatalf("identical pre-existing group must be ignored, got %q", got)
	}
	// a new member joins the pre-existing group
	head2 := append(append([]string{}, base...), "FOO/c.js")
	if got, want := New(Collisions(head2), Collisions(base)), groups(g("foo", "FOO", "Foo", "foo")); !reflect.DeepEqual(got, want) {
		t.Fatalf("new member: got %q want %q", got, want)
	}
	// a new file under the pre-existing merged directory that shadows an
	// existing file there (same component spelling, different parent spelling)
	head3 := append(append([]string{}, base...), "Foo/b.js")
	if got, want := New(Collisions(head3), Collisions(base)), groups(g("foo/b.js", "Foo/b.js", "foo/b.js")); !reflect.DeepEqual(got, want) {
		t.Fatalf("shadow under pre-existing dir: got %q want %q", got, want)
	}
	// a new group: inherited child groups (same component spelling, parent
	// group new too) are folded into the parent group
	head4 := []string{"node_modules/t/index.js", "Node_Modules/t/package.json", "Node_Modules/t/lax.js"}
	if got, want := New(Collisions(head4), Collisions([]string{"node_modules/t/index.js"})), groups(g("node_modules", "Node_Modules", "node_modules")); !reflect.DeepEqual(got, want) {
		t.Fatalf("new dir group: got %q want %q", got, want)
	}
	// a child group whose own component spellings differ is kept
	head5 := []string{"node_modules/t/index.js", "Node_Modules/T/package.json"}
	if got, want := New(Collisions(head5), nil), groups(g("node_modules", "Node_Modules", "node_modules"), g("node_modules/t", "Node_Modules/T", "node_modules/t")); !reflect.DeepEqual(got, want) {
		t.Fatalf("differing child: got %q want %q", got, want)
	}
	// initial mode: every group counts
	if got := New(Collisions([]string{"a", "A"}), nil); len(got) != 1 {
		t.Fatalf("initial: got %q", got)
	}
}

func TestCollisionsPerformance(t *testing.T) {
	paths := make([]string, 0, 200_000)
	for i := 0; len(paths) < 200_000; i++ {
		paths = append(paths, fmt.Sprintf("packages/pkg%03d/src/module%02d/sub/dir/file%05d_%d.ts", i%500, i%37, i, i%7))
	}
	paths = append(paths, "packages/Pkg001/x.ts", "packages/pkg002/SRC/y.ts", "node_module\u017f/x/a.js", "node_modules/x/b.js")
	t0 := time.Now()
	head := Collisions(paths)
	base := Collisions(paths[:200_000])
	n := New(head, base)
	el := time.Since(t0)
	if len(n) != 3 {
		t.Fatalf("want 3 new groups, got %d: %q", len(n), n)
	}
	if el > time.Second {
		t.Fatalf("200k paths took %v (> 1s)", el)
	}
	t.Logf("2 x %d paths: %v", len(paths), el)
}
