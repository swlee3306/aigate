// Package gitx wraps the git binary for aigate: ref resolution, detached
// worktrees, and the protected-file diff used for approval hashing.
// It is a leaf package and imports nothing from the rest of aigate.
package gitx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// run executes git -C repo args... and returns stdout.
func run(repo string, args ...string) ([]byte, error) {
	full := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &gitError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.Bytes(), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitError) Unwrap() error { return e.err }

// Resolve returns the commit SHA for ref, rejecting non-commit objects.
func Resolve(repo, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	out, err := run(repo, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// MergeBase returns the best common ancestor of a and b.
func MergeBase(repo, a, b string) (string, error) {
	out, err := run(repo, "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("merge-base %q %q: %w", a, b, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAncestor reports whether anc is an ancestor of desc.
func IsAncestor(repo, anc, desc string) (bool, error) {
	_, err := run(repo, "merge-base", "--is-ancestor", anc, desc)
	if err == nil {
		return true, nil
	}
	var ge *gitError
	var ee *exec.ExitError
	if errors.As(err, &ge) && errors.As(ge.err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("is-ancestor %q %q: %w", anc, desc, err)
}

// Dirty returns a short summary when the working tree has uncommitted changes
// or untracked files (ignoring untracked paths under .aigate/), else "".
func Dirty(repo string) (string, error) {
	out, err := run(repo, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" || strings.HasPrefix(l, "?? .aigate/") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return "", nil
	}
	const max = 10
	if len(lines) > max {
		lines = append(lines[:max], fmt.Sprintf("... and %d more", len(lines)-max))
	}
	return strings.Join(lines, "\n"), nil
}

// Show returns the content of path at commit sha.
func Show(repo, sha, p string) ([]byte, error) {
	out, err := run(repo, "cat-file", "blob", sha+":"+p)
	if err != nil {
		return nil, fmt.Errorf("show %s:%s: %w", sha, p, err)
	}
	return out, nil
}

// WithWorktree checks out sha in a detached temporary worktree outside the
// repo, runs fn, and always removes the worktree (even on error, panic or a
// cancelled run: cleanup does not depend on any context). Repository hooks
// do not run during the checkout (core.hooksPath=/dev/null).
func WithWorktree(repo, sha string, fn func(dir string) error) (err error) {
	parent, err := worktreeParent(repo)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "aigate-wt-")
	if err != nil {
		return err
	}
	dir := filepath.Join(tmp, "wt")
	defer func() {
		r := recover()
		_, _ = run(repo, "worktree", "remove", "--force", dir)
		_, _ = run(repo, "worktree", "prune")
		_ = os.RemoveAll(tmp)
		if r != nil {
			panic(r)
		}
	}()
	if _, err := run(repo, "-c", "core.hooksPath=/dev/null", "worktree", "add", "--detach", dir, sha); err != nil {
		return fmt.Errorf("worktree add %s: %w", sha, err)
	}
	return fn(dir)
}

// worktreeParent returns os.TempDir(), or <user cache dir>/aigate-wt when the
// temp dir lies inside the repository (a worktree there would be part of the
// repo's working tree).
func worktreeParent(repo string) (string, error) {
	tmp := os.TempDir()
	if !inside(repo, tmp) {
		return tmp, nil
	}
	c, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("temp dir %s is inside the repo and no user cache dir: %w", tmp, err)
	}
	c = filepath.Join(c, "aigate-wt")
	if inside(repo, c) {
		return "", fmt.Errorf("temp dir %s and cache dir %s are both inside the repo %s", tmp, c, repo)
	}
	if err := os.MkdirAll(c, 0o700); err != nil {
		return "", err
	}
	return c, nil
}

func realAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// inside reports whether p is repo or below it (symlinks resolved).
func inside(repo, p string) bool {
	rel, err := filepath.Rel(realAbs(repo), realAbs(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, "../"))
}

// ProtDiff describes changes to protected paths between two commits.
//
// Text is the exact diff the approval hash is computed over (see
// HashedText): `git diff --text -U0 --full-index` with every attribute,
// textconv, external-diff and layout setting that could hide or reshape lines
// pinned or disabled, so a `-diff`/`binary` attribute cannot turn removed
// lines into "Binary files differ".
type ProtDiff struct {
	Hash              string
	Text              string
	RemovedLines      []string
	TouchedFiles      []string
	FilesWithRemovals []string
	AddedFiles        []string
	// DeletedFiles are protected files deleted at head (empty files too).
	DeletedFiles []string
	// ModifiedExisting are protected files that exist at base AND at head and
	// differ in any way: text lines added or removed, type or mode changed,
	// binary content changed (user decision 2026-11-04, option 1: every hunk,
	// pure additions included). Unchanged files and files new at head are
	// never listed; a rename is a deletion plus an addition (--no-renames).
	ModifiedExisting []string
	// Files has one entry per touched protected file (sorted by path).
	Files []FileChange
}

// FileChange is one protected file in the diff.
type FileChange struct {
	Path   string
	Status string // git --raw status letter: A, D, M, T, ...
	// BaseBinary / HeadBinary: that side's blob contains a NUL byte anywhere
	// (a superset of git's heuristic, a NUL in the first 8000 bytes;
	// attributes are never consulted). False for a side that does not exist
	// (added / deleted file) and for a gitlink (submodule commit) side.
	BaseBinary, HeadBinary bool
	BaseSize, HeadSize     int64
	// BaseBlob / HeadBlob are the full blob ids (all zeros when absent).
	BaseBlob, HeadBlob string
	// HeadNULLine is the 1-based line of the first NUL in the head blob, 0
	// when there is none.
	HeadNULLine int
	// HeadMode is the git mode at head ("100644", "100755", "120000",
	// "160000"; "000000" when deleted). HeadShebang: the head blob starts
	// with "#!". Facts only: the caller decides what may be summarised.
	HeadMode    string
	HeadShebang bool
	// HeadPrefix is the first HeadPrefixLen bytes of the head blob (all of
	// it when shorter; nil when absent or a gitlink): the caller checks magic
	// numbers against it. HeadSymlinkTarget is the whole head blob when the
	// head is a symlink (mode 120000), i.e. the link target; "" otherwise.
	HeadPrefix        []byte
	HeadSymlinkTarget string
	pair              string // "oldsha..newsha" as on the diff "index" line
}

// Summarised reports whether DisplayText shows the file as one summary line:
// binary on every side that exists (added: head; deleted: base; otherwise
// both). A file that became text is shown as text; a file that became binary
// must be refused by the caller before display. Summarised is NOT a safety
// rule: approve refuses, before display, every non-deleted summarised file
// that is not an allowlisted genuine binary type (cmd/aigate binaryRefusal).
func (f FileChange) Summarised() bool {
	switch f.Status {
	case "A":
		return f.HeadBinary
	case "D":
		return f.BaseBinary
	}
	return f.BaseBinary && f.HeadBinary
}

// Changed returns the sorted protected files that existed at base and were
// changed in any way (ModifiedExisting) or deleted (DeletedFiles). Every
// entry needs approval.
func (d ProtDiff) Changed() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]string{d.DeletedFiles, d.ModifiedExisting} {
		for _, f := range l {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	sort.Strings(out)
	return out
}

// DisplayText is HashedText with the file section of every summarised
// (genuine binary) protected file replaced by one summary line
// ("binary file changed: <path>, <head> bytes (base <base> bytes), head blob
// <id>"), so the raw bytes of a binary file are never printed. escapePath
// (nil: identity) renders the path. The hash is still over HashedText.
func (d ProtDiff) DisplayText(escapePath func(string) string) string {
	if escapePath == nil {
		escapePath = func(s string) string { return s }
	}
	byPair := map[string][]FileChange{}
	for _, f := range d.Files {
		byPair[f.pair] = append(byPair[f.pair], f)
	}
	var b strings.Builder
	for _, sec := range splitSections(d.Text) {
		pair := sectionPair(sec)
		if q := byPair[pair]; pair != "" && len(q) > 0 {
			f := q[0]
			byPair[pair] = q[1:]
			if f.Summarised() {
				fmt.Fprintf(&b, "binary file changed: %s, %d bytes (base %d bytes), head blob %s\n", escapePath(f.Path), f.HeadSize, f.BaseSize, f.HeadBlob)
				continue
			}
		}
		b.WriteString(sec)
	}
	for _, a := range d.AddedFiles {
		b.WriteString("A " + a + "\n")
	}
	return b.String()
}

// splitSections splits a diff into its "diff --git " sections (any text
// before the first one is kept as its own section).
func splitSections(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); {
		j := strings.IndexByte(text[i:], '\n')
		next := len(text)
		if j >= 0 {
			next = i + j + 1
		}
		if i > start && strings.HasPrefix(text[i:], "diff --git ") {
			out = append(out, text[start:i])
			start = i
		}
		i = next
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// sectionPair returns the "old..new" blob pair of the section's index line
// (before its first hunk), or "".
func sectionPair(sec string) string {
	for _, l := range strings.Split(sec, "\n") {
		if strings.HasPrefix(l, "@@") {
			break
		}
		if strings.HasPrefix(l, "index ") {
			if f := strings.Fields(l); len(f) >= 2 {
				return f[1]
			}
		}
	}
	return ""
}

// HashedText is the exact text Hash is the sha256 prefix of: Text followed by
// one "A <path>" line per added protected file (sorted).
func (d ProtDiff) HashedText() string {
	var b strings.Builder
	b.WriteString(d.Text)
	for _, a := range d.AddedFiles {
		b.WriteString("A " + a + "\n")
	}
	return b.String()
}

// diffConfig pins every git setting that changes what `git diff` prints for
// the protected paths, so the hash and the removed lines depend only on the
// two commits.
var diffConfig = []string{
	"-c", "core.quotepath=false", "-c", "core.attributesFile=/dev/null",
	"-c", "diff.algorithm=myers", "-c", "diff.indentHeuristic=false", "-c", "diff.interHunkContext=0",
	"-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false", "-c", "diff.relative=false",
	"-c", "diff.orderFile=/dev/null", "-c", "diff.submodule=short", "-c", "diff.ignoreSubmodules=none",
	"-c", "diff.renames=false", "-c", "diff.external=",
}

func diffCmd(base, head string, specs []string, extra ...string) []string {
	a := append([]string{}, diffConfig...)
	a = append(a, "diff", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "--no-relative",
		"--ignore-submodules=none", "--submodule=short", "--full-index", "--diff-algorithm=myers",
		"--src-prefix=a/", "--dst-prefix=b/")
	a = append(a, extra...)
	a = append(a, base, head)
	return append(a, specs...)
}

type rawEntry struct {
	oldMode, newMode, oldSHA, newSHA, status, path string
}

// parseRaw parses `git diff --raw -z --no-abbrev` output.
func parseRaw(b []byte) ([]rawEntry, error) {
	parts := splitZ(b)
	var out []rawEntry
	for i := 0; i < len(parts); i++ {
		meta := parts[i]
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("unexpected raw diff record %q", meta)
		}
		f := strings.Fields(meta[1:])
		if len(f) != 5 || i+1 >= len(parts) {
			return nil, fmt.Errorf("unexpected raw diff record %q", meta)
		}
		i++
		out = append(out, rawEntry{oldMode: f[0], newMode: f[1], oldSHA: f[2], newSHA: f[3], status: f[4][:1], path: parts[i]})
	}
	return out, nil
}

func isZero(sha string) bool { return strings.Trim(sha, "0") == "" }

// gitlinkMode is the raw-diff mode of a submodule entry (a commit id);
// symlinkMode is a symbolic link (the blob is the target path).
const gitlinkMode, symlinkMode = "160000", "120000"

// HeadPrefixLen is how many leading head-blob bytes FileChange.HeadPrefix
// keeps (enough for tar's "ustar" at offset 257).
const HeadPrefixLen = 512

// blobInfo reports the 1-based line of the first NUL byte anywhere in a blob
// (0: none, i.e. text; this covers git's own heuristic of a NUL in the first
// 8000 bytes), without consulting attributes, the blob's size, and whether it
// starts with "#!", and the blob itself (nil when absent).
func blobInfo(repo, sha string) (nulLine int, size int64, shebang bool, blob []byte, err error) {
	if isZero(sha) {
		return 0, 0, false, nil, nil
	}
	b, err := run(repo, "cat-file", "blob", sha)
	if err != nil {
		return 0, 0, false, nil, err
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		nulLine = bytes.Count(b[:i], []byte("\n")) + 1
	}
	return nulLine, int64(len(b)), bytes.HasPrefix(b, []byte("#!")), b, nil
}

// ProtectedDiff diffs base..head restricted to the protected globs.
func ProtectedDiff(repo, base, head string, protected []string) (ProtDiff, error) {
	var d ProtDiff
	if len(protected) == 0 {
		d.Hash = hashOf(d.HashedText())
		return d, nil
	}
	specs := []string{"--"}
	for _, p := range protected {
		specs = append(specs, ":(glob)"+p)
	}

	text, err := run(repo, diffCmd(base, head, specs, "--text", "-U0")...)
	if err != nil {
		return d, err
	}
	d.Text = string(text)
	d.RemovedLines = removedLines(d.Text)

	rawOut, err := run(repo, diffCmd(base, head, specs, "--raw", "-z", "--no-abbrev")...)
	if err != nil {
		return d, err
	}
	entries, err := parseRaw(rawOut)
	if err != nil {
		return d, err
	}
	byPair := map[string][]string{}
	for _, e := range entries {
		byPair[e.oldSHA+".."+e.newSHA] = append(byPair[e.oldSHA+".."+e.newSHA], e.path)
	}
	withRemovals := map[string]bool{}
	for _, pair := range pairsWithRemovals(d.Text) {
		for _, p := range byPair[pair] {
			withRemovals[p] = true
		}
	}
	for _, e := range entries {
		d.TouchedFiles = append(d.TouchedFiles, e.path)
		if withRemovals[e.path] {
			d.FilesWithRemovals = append(d.FilesWithRemovals, e.path)
		}
		fc := FileChange{Path: e.path, Status: e.status, BaseBlob: e.oldSHA, HeadBlob: e.newSHA, HeadMode: e.newMode, pair: e.oldSHA + ".." + e.newSHA}
		for i, side := range [][2]string{{e.oldMode, e.oldSHA}, {e.newMode, e.newSHA}} {
			if side[0] == gitlinkMode {
				// a submodule commit, not a blob: nothing to read; the diff
				// text shows it as a "Subproject commit <sha>" line
				continue
			}
			nul, size, shebang, blob, err := blobInfo(repo, side[1])
			if err != nil {
				return d, err
			}
			if i == 0 {
				fc.BaseBinary, fc.BaseSize = nul > 0, size
				continue
			}
			fc.HeadBinary, fc.HeadSize, fc.HeadNULLine, fc.HeadShebang = nul > 0, size, nul, shebang
			if blob != nil {
				fc.HeadPrefix = append([]byte(nil), blob[:min(len(blob), HeadPrefixLen)]...)
			}
			if side[0] == symlinkMode {
				fc.HeadSymlinkTarget = string(blob)
			}
		}
		d.Files = append(d.Files, fc)
		switch e.status {
		case "A":
			d.AddedFiles = append(d.AddedFiles, e.path)
		case "D":
			d.DeletedFiles = append(d.DeletedFiles, e.path)
		default:
			// M (content or mode), T (type), unmerged or unknown: the file
			// exists at both commits and differs — any hunk, additions
			// included, needs approval.
			d.ModifiedExisting = append(d.ModifiedExisting, e.path)
		}
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	sort.Strings(d.TouchedFiles)
	sort.Strings(d.FilesWithRemovals)
	sort.Strings(d.AddedFiles)
	sort.Strings(d.DeletedFiles)
	sort.Strings(d.ModifiedExisting)
	d.Hash = hashOf(d.HashedText())
	return d, nil
}

// pairsWithRemovals returns the "old..new" blob pairs (from the "index" line
// of each file section) of the sections that remove at least one line.
func pairsWithRemovals(text string) []string {
	var out []string
	pair, inHunk, counted := "", false, false
	for _, l := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			pair, inHunk, counted = "", false, false
		case !inHunk && strings.HasPrefix(l, "index "):
			f := strings.Fields(l)
			if len(f) >= 2 {
				pair = f[1]
			}
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(l, "-") && !counted && pair != "":
			counted = true
			out = append(out, pair)
		}
	}
	return out
}

func splitZ(b []byte) []string {
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// removedLines extracts removed lines from a -U0 diff, skipping file headers
// by tracking hunk state rather than matching "---" prefixes.
func removedLines(text string) []string {
	var out []string
	inHunk := false
	for _, l := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(l, "-"):
			out = append(out, l[1:])
		}
	}
	return out
}

func hashOf(hashed string) string {
	sum := sha256.Sum256([]byte(hashed))
	return hex.EncodeToString(sum[:])[:16]
}

// AddedMatching returns the sorted paths added between base and head that match
// any glob, using git's own ":(glob)" pathspec semantics. Invalid pathspecs
// yield an error (never fail open).
func AddedMatching(repo, base, head string, globs []string) ([]string, error) {
	if len(globs) == 0 {
		return nil, nil
	}
	args := append(append([]string{}, diffConfig...), "diff", "--name-status", "-z", "--no-renames", "--diff-filter=A", base, head, "--")
	for _, g := range globs {
		args = append(args, ":(glob)"+g)
	}
	out, err := run(repo, args...)
	if err != nil {
		return nil, err
	}
	var res []string
	parts := splitZ(out)
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "A" {
			res = append(res, parts[i+1])
		}
	}
	sort.Strings(res)
	return res, nil
}

// MatchGlob is a pure-Go approximation of glob matching. It is NOT used for
// guard decisions (use ProtectedDiff/AddedMatching, which rely on git's own
// pathspec semantics); it differs from git for bare directory patterns,
// "[!x]" classes and malformed patterns.
//
// MatchGlob matches a slash-separated path against a glob where "**" matches
// any number of segments (a trailing "/**" needs at least one), "*" and "?"
// stay within one segment.
func MatchGlob(pattern, p string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			return len(segs) > 0
		}
		for i := 0; i <= len(segs); i++ {
			if matchSegs(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}
