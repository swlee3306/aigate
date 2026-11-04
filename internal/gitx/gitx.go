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
// repo, runs fn, and always removes the worktree (even on error or panic).
func WithWorktree(repo, sha string, fn func(dir string) error) (err error) {
	tmp, err := os.MkdirTemp("", "aigate-wt-")
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
	if _, err := run(repo, "worktree", "add", "--detach", dir, sha); err != nil {
		return fmt.Errorf("worktree add %s: %w", sha, err)
	}
	return fn(dir)
}

// ProtDiff describes changes to protected paths between two commits.
type ProtDiff struct {
	Hash              string
	RemovedLines      []string
	TouchedFiles      []string
	FilesWithRemovals []string
	AddedFiles        []string
}

// ProtectedDiff diffs base..head restricted to the protected globs.
func ProtectedDiff(repo, base, head string, protected []string) (ProtDiff, error) {
	var d ProtDiff
	if len(protected) == 0 {
		d.Hash = hashOf("", nil)
		return d, nil
	}
	specs := []string{"--"}
	for _, p := range protected {
		specs = append(specs, ":(glob)"+p)
	}
	diffArgs := func(extra ...string) []string {
		a := []string{"-c", "core.quotepath=false", "-c", "diff.algorithm=myers", "diff", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv",
			"--full-index", "--diff-algorithm=myers", "--src-prefix=a/", "--dst-prefix=b/"}
		a = append(a, extra...)
		a = append(a, base, head)
		return append(a, specs...)
	}

	text, err := run(repo, diffArgs("-U0", "--binary")...)
	if err != nil {
		return d, err
	}
	d.RemovedLines = removedLines(string(text))

	ns, err := run(repo, diffArgs("--numstat", "-z")...)
	if err != nil {
		return d, err
	}
	for _, rec := range splitZ(ns) {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) != 3 {
			continue
		}
		d.TouchedFiles = append(d.TouchedFiles, f[2])
		if f[1] != "0" { // "-" means binary
			d.FilesWithRemovals = append(d.FilesWithRemovals, f[2])
		}
	}

	st, err := run(repo, diffArgs("--name-status", "-z")...)
	if err != nil {
		return d, err
	}
	parts := splitZ(st)
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "A" {
			d.AddedFiles = append(d.AddedFiles, parts[i+1])
		}
	}
	sort.Strings(d.TouchedFiles)
	sort.Strings(d.FilesWithRemovals)
	sort.Strings(d.AddedFiles)
	d.Hash = hashOf(string(text), d.AddedFiles)
	return d, nil
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

func hashOf(diff string, added []string) string {
	h := sha256.New()
	h.Write([]byte(diff))
	for _, a := range added {
		h.Write([]byte("A " + a + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// AddedMatching returns the sorted paths added between base and head that match
// any glob, using git's own ":(glob)" pathspec semantics. Invalid pathspecs
// yield an error (never fail open).
func AddedMatching(repo, base, head string, globs []string) ([]string, error) {
	if len(globs) == 0 {
		return nil, nil
	}
	args := []string{"-c", "core.quotepath=false", "diff", "--name-status", "-z", "--no-renames", "--diff-filter=A", base, head, "--"}
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
