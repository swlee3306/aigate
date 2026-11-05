// Package pathfold finds git paths that a case-insensitive, normalization-
// insensitive file system (macOS APFS, Windows NTFS) maps onto the same
// on-disk entry: on checkout such paths overwrite each other (two files) or
// merge (two directories), so the checked-out tree is not the committed tree.
// It is a leaf package.
//
// Folding is per path component: Key(c) = NFD(Fold(NFD(c))) with Unicode
// full case folding (golang.org/x/text/cases: ſ→s, K (Kelvin) →k, ß→ss) and
// canonical decomposition, so composed and decomposed spellings compare
// equal. Full folding is a superset of what any real file system folds
// (APFS does not equate ß and ss): extra matches fail closed.
// Windows-only equivalences (reserved device names, trailing dots/spaces, 8.3
// short names, alternate data streams) are NOT handled.
package pathfold

import (
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Group is one set of colliding entries: Key is the folded path of the
// on-disk entry they map to ("/"-joined folded components), Members the
// distinct original paths (sorted, at least two) that map to it.
type Group struct {
	Key     string
	Members []string
}

// Key returns the folding key of one path component.
func Key(c string) string { return newFolder().key(c) }

type folder struct {
	caser cases.Caser
	cache map[string]string
}

func newFolder() *folder { return &folder{caser: cases.Fold(), cache: map[string]string{}} }

func (f *folder) key(c string) string {
	if k, ok := f.cache[c]; ok {
		return k
	}
	k := c
	ascii := true
	for i := 0; i < len(c); i++ {
		if c[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		k = strings.ToLower(c) // ASCII full folding is A-Z → a-z; NFD is identity
	} else {
		k = norm.NFD.String(f.caser.String(norm.NFD.String(c)))
	}
	f.cache[c] = k
	return k
}

// onode is a node of the trie of original components; fnode of folded ones.
type onode struct {
	parent *onode
	name   string
	kids   map[string]*onode
}

type fnode struct {
	kids  map[string]*fnode
	origs []*onode
}

func (o *onode) path() string {
	var parts []string
	for n := o; n.parent != nil; n = n.parent {
		parts = append(parts, n.name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/")
}

// Collisions returns every group of two or more distinct original paths (or
// path prefixes, i.e. directories) whose folded paths are equal, sorted by
// Key. A file and a directory with colliding names form a group too. It runs
// in time linear in the total path length (one trie walk per path; folded
// components are cached).
func Collisions(paths []string) []Group {
	f := newFolder()
	oroot := &onode{kids: map[string]*onode{}}
	froot := &fnode{kids: map[string]*fnode{}}
	for _, p := range paths {
		o, fn := oroot, froot
		for len(p) > 0 {
			c := p
			if i := strings.IndexByte(p, '/'); i >= 0 {
				c, p = p[:i], p[i+1:]
			} else {
				p = ""
			}
			k := f.key(c)
			fc := fn.kids[k]
			if fc == nil {
				fc = &fnode{kids: map[string]*fnode{}}
				fn.kids[k] = fc
			}
			oc := o.kids[c]
			if oc == nil {
				oc = &onode{parent: o, name: c, kids: map[string]*onode{}}
				o.kids[c] = oc
				fc.origs = append(fc.origs, oc)
			}
			o, fn = oc, fc
		}
	}
	var out []Group
	var walk func(n *fnode, key string)
	walk = func(n *fnode, key string) {
		if len(n.origs) > 1 {
			g := Group{Key: key}
			for _, o := range n.origs {
				g.Members = append(g.Members, o.path())
			}
			sort.Strings(g.Members)
			out = append(out, g)
		}
		for k, c := range n.kids {
			ck := k
			if key != "" {
				ck = key + "/" + k
			}
			walk(c, ck)
		}
	}
	walk(froot, "")
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// New returns the groups of head that are not pre-existing, i.e. not present
// with exactly the same members in base (nil base: every group is new), in
// Key order. A new group that only repeats its parent's collision (its
// members differ only in a parent component, and that parent group is new
// too) is left out: the parent group already names it.
func New(head, base []Group) []Group {
	old := map[string]string{}
	for _, g := range base {
		old[g.Key] = strings.Join(g.Members, "\x00")
	}
	isNew := map[string]bool{}
	var fresh []Group
	for _, g := range head {
		if m, ok := old[g.Key]; ok && m == strings.Join(g.Members, "\x00") {
			continue
		}
		isNew[g.Key] = true
		fresh = append(fresh, g)
	}
	var out []Group
	for _, g := range fresh {
		if i := strings.LastIndexByte(g.Key, '/'); i >= 0 && isNew[g.Key[:i]] && sameLast(g.Members) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// sameLast reports whether every member has the same last component.
func sameLast(ms []string) bool {
	last := func(s string) string { return s[strings.LastIndexByte(s, '/')+1:] }
	for _, m := range ms[1:] {
		if last(m) != last(ms[0]) {
			return false
		}
	}
	return true
}
