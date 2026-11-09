// Package refine collects the context that may go to the AI plan service
// (decision D7): files chosen from a repository's HEAD commit with read-only
// git, never from the work tree, minus everything that looks like a secret,
// binary or link; and the in-memory store of previewed selections (D6).
package refine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"aigate/internal/gitx"
)

// Limits (D7 as amended: 240 KiB total so the broker's max_input_bytes of
// 256 KiB still has room for its instructions, markers and schema).
const (
	MaxFiles            = 40
	MaxFileBytes        = 32 << 10
	MaxTotalBytes       = 240 << 10
	MaxRequirementRunes = 2000
	// maxExcluded bounds the exclusions a preview lists.
	maxExcluded = 100
	maxWords    = 200
)

// Why a file was chosen.
const (
	WhyReadme    = "readme"
	WhyNameMatch = "name_match"
)

// Options override the limits (tests); zero means the default.
type Options struct {
	MaxFiles, MaxFileBytes, MaxTotalBytes int
}

func (o Options) norm() Options {
	if o.MaxFiles <= 0 {
		o.MaxFiles = MaxFiles
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = MaxFileBytes
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = MaxTotalBytes
	}
	return o
}

// FileEntry is one chosen file as the preview shows it.
type FileEntry struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	Why   string `json:"why"`
}

// Excluded is a file that was a candidate but stays out, with a fixed reason.
type Excluded struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// File is a chosen file with its content (only ever sent to the broker).
type File struct {
	Path    string
	Content string
}

// Selection is what Collect chose. ID is assigned by the Store. Hash covers
// the HEAD commit and every chosen path and content. Contents never reach a
// response or the ledger (json:"-").
type Selection struct {
	ID         string
	Head       string
	Files      []FileEntry
	Excluded   []Excluded
	TotalBytes int
	Hash       string
	Contents   []File `json:"-"`
}

// Errors.
var (
	// ErrNoCommit: the repository has no commit to read.
	ErrNoCommit = errors.New("refine: no commit")
	// ErrUnreadable: git could not read the repository (fixed; no git text).
	ErrUnreadable = errors.New("refine: repository unreadable")
	// ErrChanged: a previously selected file can no longer be selected.
	ErrChanged = errors.New("refine: selection changed")
)

type candidate struct {
	e      gitx.TreeEntry
	score  int
	readme bool
	unsafe bool
}

// Collect chooses the files for requirement from repo's HEAD commit:
// root-level README*, then files whose path shares words with the
// requirement. Everything is read from the commit's object database
// (ls-tree, cat-file on blob ids): the work tree, symlinks and submodules are
// never followed, so nothing outside the repository can be read.
func Collect(ctx context.Context, repo, requirement string, opts Options) (Selection, error) {
	o := opts.norm()
	head, err := gitx.ReadHeadCommit(ctx, repo)
	if errors.Is(err, gitx.ErrNoHead) {
		return Selection{}, ErrNoCommit
	}
	if err != nil {
		return Selection{}, ErrUnreadable
	}
	ents, err := gitx.ListTree(ctx, repo, head)
	if err != nil {
		return Selection{}, ErrUnreadable
	}
	words := wordsOf(requirement)
	var cands []candidate
	for _, e := range ents {
		c := candidate{e: e, readme: isRootReadme(e.Path), unsafe: !SafeRelPath(e.Path)}
		c.score = score(strings.ToLower(e.Path), words)
		if c.readme || c.score > 0 {
			cands = append(cands, c)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.readme != b.readme {
			return a.readme
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if len(a.e.Path) != len(b.e.Path) {
			return len(a.e.Path) < len(b.e.Path)
		}
		return a.e.Path < b.e.Path
	})
	sel := Selection{Head: head}
	exclude := func(c candidate, reason string) {
		if len(sel.Excluded) >= maxExcluded {
			return
		}
		p := c.e.Path
		if c.unsafe {
			p = "<unsafe path>"
		}
		sel.Excluded = append(sel.Excluded, Excluded{Path: p, Reason: reason})
	}
	for _, c := range cands {
		if len(sel.Files) >= o.MaxFiles {
			exclude(c, ReasonFileCap)
			continue
		}
		content, reason, err := readCandidate(ctx, repo, c, o)
		if err != nil {
			return Selection{}, ErrUnreadable
		}
		if reason != "" {
			exclude(c, reason)
			continue
		}
		if sel.TotalBytes+len(content) > o.MaxTotalBytes {
			exclude(c, ReasonTotalCap)
			continue
		}
		why := WhyNameMatch
		if c.readme {
			why = WhyReadme
		}
		sel.Files = append(sel.Files, FileEntry{Path: c.e.Path, Bytes: len(content), Why: why})
		sel.Contents = append(sel.Contents, File{Path: c.e.Path, Content: content})
		sel.TotalBytes += len(content)
	}
	if sel.Excluded == nil {
		sel.Excluded = []Excluded{}
	}
	if sel.Files == nil {
		sel.Files = []FileEntry{}
	}
	sel.Hash = hashOf(head, sel.Contents)
	return sel, nil
}

// Reread reads exactly paths again from repo's CURRENT HEAD commit under the
// same rules and returns the selection with its hash. ErrChanged when a path
// is gone or can no longer be selected; the caller compares Hash with the
// previewed one. Which paths are read comes only from paths (the preview),
// never from new input.
func Reread(ctx context.Context, repo string, paths []string, opts Options) (Selection, error) {
	o := opts.norm()
	head, err := gitx.ReadHeadCommit(ctx, repo)
	if errors.Is(err, gitx.ErrNoHead) {
		return Selection{}, ErrChanged
	}
	if err != nil {
		return Selection{}, ErrUnreadable
	}
	ents, err := gitx.ListTree(ctx, repo, head)
	if err != nil {
		return Selection{}, ErrUnreadable
	}
	byPath := make(map[string]gitx.TreeEntry, len(ents))
	for _, e := range ents {
		byPath[e.Path] = e
	}
	sel := Selection{Head: head, Files: []FileEntry{}, Excluded: []Excluded{}}
	for _, p := range paths {
		e, ok := byPath[p]
		if !ok {
			return Selection{}, ErrChanged
		}
		content, reason, err := readCandidate(ctx, repo, candidate{e: e, unsafe: !SafeRelPath(p)}, o)
		if err != nil {
			return Selection{}, ErrUnreadable
		}
		if reason != "" || sel.TotalBytes+len(content) > o.MaxTotalBytes {
			return Selection{}, ErrChanged
		}
		sel.Files = append(sel.Files, FileEntry{Path: p, Bytes: len(content)})
		sel.Contents = append(sel.Contents, File{Path: p, Content: content})
		sel.TotalBytes += len(content)
	}
	sel.Hash = hashOf(head, sel.Contents)
	return sel, nil
}

// readCandidate applies every per-file rule and reads the blob. A non-empty
// reason excludes the file; err is a git failure.
func readCandidate(ctx context.Context, repo string, c candidate, o Options) (content, reason string, err error) {
	e := c.e
	switch {
	case c.unsafe:
		return "", ReasonUnsafePath, nil
	case e.Mode == "160000" || e.Type == "commit":
		return "", ReasonSubmodule, nil
	case e.Mode == "120000":
		return "", ReasonSymlink, nil
	case e.Type != "blob" || (e.Mode != "100644" && e.Mode != "100755"):
		return "", ReasonUnreadable, nil
	case secretName(e.Path) || LooksLikeSecret(e.Path):
		return "", ReasonSecretName, nil
	case e.Size > int64(o.MaxFileBytes):
		return "", ReasonTooLarge, nil
	}
	b, err := gitx.ReadBlob(ctx, repo, e.SHA, o.MaxFileBytes)
	if errors.Is(err, gitx.ErrTooLarge) {
		return "", ReasonTooLarge, nil
	}
	if err != nil {
		return "", "", err
	}
	switch {
	case bytes.IndexByte(b, 0) >= 0:
		return "", ReasonBinary, nil
	case !utf8.Valid(b):
		return "", ReasonNotUTF8, nil
	case LooksLikeSecret(string(b)):
		return "", ReasonLooksLikeSecret, nil
	}
	return string(b), "", nil
}

// hashOf is the content hash of a selection: the HEAD commit and, in order,
// every path with the length and bytes of its content.
func hashOf(head string, files []File) string {
	h := sha256.New()
	h.Write([]byte("orch-refine-selection-v1\n" + head + "\n"))
	for _, f := range files {
		h.Write([]byte(f.Path + "\x00" + strconv.Itoa(len(f.Content)) + "\x00"))
		h.Write([]byte(f.Content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContentHash is the hash of the HEAD commit and files (exported so tests and
// callers can recompute what the preview promised).
func ContentHash(head string, files []File) string { return hashOf(head, files) }

func isRootReadme(p string) bool {
	return !strings.Contains(p, "/") && strings.HasPrefix(strings.ToLower(p), "readme")
}

var stopwords = map[string]bool{"the": true, "and": true, "for": true, "with": true, "that": true,
	"this": true, "from": true, "into": true, "add": true, "new": true, "use": true, "all": true,
	"are": true, "not": true, "can": true, "should": true, "when": true, "will": true}

// wordsOf lowercases the requirement into distinct words (letters and
// digits): three or more characters, two or more for Hangul.
func wordsOf(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		n := utf8.RuneCountInString(w)
		hangul := false
		for _, r := range w {
			if unicode.Is(unicode.Hangul, r) {
				hangul = true
			}
		}
		if (n < 3 && !(hangul && n >= 2)) || stopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) >= maxWords {
			break
		}
	}
	return out
}

// score: a word equal to a token of the file name counts 3, a word found
// anywhere in the path 1.
func score(lowerPath string, words []string) int {
	base := lowerPath[strings.LastIndexByte(lowerPath, '/')+1:]
	tokens := map[string]bool{}
	for _, t := range strings.FieldsFunc(base, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		tokens[t] = true
	}
	n := 0
	for _, w := range words {
		switch {
		case tokens[w]:
			n += 3
		case strings.Contains(lowerPath, w):
			n++
		}
	}
	return n
}
