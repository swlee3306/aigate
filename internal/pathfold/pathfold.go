// Package pathfold finds git paths that a case-insensitive, normalization-
// insensitive file system (macOS APFS, Windows NTFS) maps onto the same
// on-disk entry: on checkout such paths overwrite each other (two files) or
// merge (two directories), so the checked-out tree is not the committed tree.
// It is a leaf package.
//
// Folding is per path component: Key(c) = NFD(Fold(NFD(c))) with Unicode
// full case folding (golang.org/x/text/cases: ſ→s, K (Kelvin) →k, ß→ss) and
// canonical decomposition, so composed and decomposed spellings compare
// equal. APFS folds the same way (checked on macOS: a file "pyteſt.ini"
// opens as "pytest.ini", "Straße" as "Strasse"); where a file system folds
// less, an extra match fails closed.
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

// New returns the groups of head that are not pre-existing, in Key order
// (nil base: every group is new). A group is pre-existing only when base has
// a group with the same Key, exactly the same members, and every member has
// the same signature on both sides (fix wave 8 (R2)): headSig/baseSig map a
// path to its "mode object-id" from `git ls-tree -r -t` (directories are
// members too and carry their tree id, which changes when anything beneath
// changes). A member without a signature on either side makes the group new
// (fail closed). A new group that only repeats its parent's collision (its
// members differ only in a parent component, and that parent group is new
// too) is left out: the parent group already names it.
func New(head, base []Group, headSig, baseSig map[string]string) []Group {
	old := map[string]string{}
	for _, g := range base {
		old[g.Key] = strings.Join(g.Members, "\x00")
	}
	isNew := map[string]bool{}
	var fresh []Group
	for _, g := range head {
		if m, ok := old[g.Key]; ok && m == strings.Join(g.Members, "\x00") && sameSigs(g.Members, headSig, baseSig) {
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

// sameSigs reports whether every member has a signature on both sides and
// the two are equal.
func sameSigs(members []string, headSig, baseSig map[string]string) bool {
	for _, m := range members {
		h, ok1 := headSig[m]
		b, ok2 := baseSig[m]
		if !ok1 || !ok2 || h != b {
			return false
		}
	}
	return true
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

// KeyPath returns the folding key of a whole path: Key of every
// "/"-separated component, joined with "/" (the Key of a Group).
func KeyPath(p string) string {
	f := newFolder()
	cs := strings.Split(p, "/")
	for i, c := range cs {
		cs[i] = f.key(c)
	}
	return strings.Join(cs, "/")
}

// FoldGlob folds the literal parts of a slash-separated glob with the same
// key as KeyPath, so that a folded path can be matched against it (fix wave
// 8: folded aliases of guard globs). The glob syntax is kept: "**"
// components, "*" and "?" are copied; a "\x" escape folds x and keeps it
// escaped; a "[...]" class is copied with its ASCII letters lowered (folded
// paths hold no upper-case ASCII) and every other rune folded only when its
// key is one rune (a rune that folds to several, such as ß→ss, stays as it
// is: a class matches one rune). Known approximations, all on the side of a
// glob matching less: "?" and a class match one rune of the folded key, so
// they do not match a rune that folds or decomposes to several (é→e+U+0301).
func FoldGlob(g string) string {
	f := newFolder()
	cs := strings.Split(g, "/")
	for i, c := range cs {
		if c != "**" {
			cs[i] = foldGlobComponent(f, c)
		}
	}
	return strings.Join(cs, "/")
}

func foldGlobComponent(f *folder, c string) string {
	var b, lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			b.WriteString(f.key(lit.String()))
			lit.Reset()
		}
	}
	rs := []rune(c)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; r {
		case '*', '?':
			flush()
			b.WriteRune(r)
		case '\\':
			flush()
			if i+1 < len(rs) {
				i++
				for _, k := range f.key(string(rs[i])) {
					if strings.ContainsRune("*?[\\", k) {
						b.WriteRune('\\')
					}
					b.WriteRune(k)
				}
			} else {
				b.WriteRune(r)
			}
		case '[':
			flush()
			b.WriteRune(r)
			for i++; i < len(rs); i++ {
				r := rs[i]
				if r < 0x80 {
					b.WriteString(strings.ToLower(string(r)))
				} else if k := []rune(f.key(string(r))); len(k) == 1 {
					b.WriteRune(k[0])
				} else {
					b.WriteRune(r)
				}
				if r == ']' && i > 0 && rs[i-1] != '[' {
					break
				}
			}
		default:
			lit.WriteRune(r)
		}
	}
	flush()
	return b.String()
}
