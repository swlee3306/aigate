package gitx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RepoState is what the web console's repository picker shows of a work tree.
type RepoState struct {
	// Head is the full SHA of HEAD ("" when the current branch is unborn).
	Head string
	// Branch is the current branch's short name ("" when HEAD is detached).
	Branch string
	// Branches are local branch names, most recently committed first.
	Branches []string
}

// readEnv is the environment of a read-only query: the redirecting variables
// removed (cleanEnv), every GIT_CONFIG* injection removed, no system or global
// configuration, and no optional locks (a read never refreshes the index).
func readEnv() []string {
	base := cleanEnv(os.Environ())
	out := make([]string, 0, len(base)+5)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		// refs/replace must not change what a read returns.
		"GIT_NO_REPLACE_OBJECTS=1")
}

// runRead runs a read-only git query in repo, bounded by ctx, with hooks and
// fsmonitor off. stdout is capped at 1 MiB. The error never carries stderr:
// callers decide what (if anything) to show.
func runRead(ctx context.Context, repo string, args ...string) ([]byte, int, error) {
	out, _, code, err := runReadCap(ctx, repo, 1<<20, args...)
	return out, code, err
}

// runReadCap is runRead with stdout capped at max bytes; truncated reports
// that git wrote more than that.
func runReadCap(ctx context.Context, repo string, max int, args ...string) (out []byte, truncated bool, code int, err error) {
	full := append([]string{"-C", repo, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(readEnv(), ceilingEnv(repo)...)
	cmd.WaitDelay = time.Second
	var stdout capBuf
	stdout.max = max
	cmd.Stdout = &stdout
	err = cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil {
			return stdout.b.Bytes(), stdout.over, ee.ExitCode(), err
		}
		return nil, false, -1, err
	}
	return stdout.b.Bytes(), stdout.over, 0, nil
}

type capBuf struct {
	b    bytes.Buffer
	max  int
	over bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	room := c.max - c.b.Len()
	if len(p) > room {
		c.over = true
	}
	if room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

// RepoIdentity is where git actually reads a work tree from.
type RepoIdentity struct {
	// TopLevel is the work tree's top level (--show-toplevel).
	TopLevel string
	// GitDir is the absolute git directory (--absolute-git-dir); CommonDir
	// is the shared one (--git-common-dir, made absolute), which differs for
	// a linked worktree.
	GitDir, CommonDir string
}

// ReadRepoIdentity asks git (read-only, discovery stopped at repo) which work
// tree and git directories it would use for repo. Callers compare them with
// what they expect; nothing is resolved through symlinks here.
func ReadRepoIdentity(ctx context.Context, repo string) (RepoIdentity, error) {
	out, _, err := runRead(ctx, repo, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return RepoIdentity{}, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 || lines[0] == "" || lines[1] == "" || lines[2] == "" {
		return RepoIdentity{}, errors.New("gitx: unexpected rev-parse output")
	}
	id := RepoIdentity{TopLevel: lines[0], GitDir: lines[1], CommonDir: lines[2]}
	if !filepath.IsAbs(id.CommonDir) {
		id.CommonDir = filepath.Join(repo, id.CommonDir)
	}
	return id, nil
}

// ReadRepoState reads HEAD, the current branch and up to maxBranches local
// branches (newest commit first) with read-only git commands. A branch name
// keep refuses is left out (the current branch becomes ""). It fails when
// git cannot read the repository at all (not a repository, refused by
// safe.directory, timeout).
func ReadRepoState(ctx context.Context, repo string, maxBranches int, keep func(string) bool) (RepoState, error) {
	var st RepoState
	// Exit 1 with -q --verify: HEAD names an unborn branch.
	out, code, err := runRead(ctx, repo, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	switch {
	case err == nil:
		st.Head = strings.TrimSpace(string(out))
	case code == 1:
	default:
		return RepoState{}, err
	}
	// Exit 1 with -q: HEAD is detached.
	out, code, err = runRead(ctx, repo, "symbolic-ref", "-q", "HEAD")
	switch {
	case err == nil:
		if b := strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/heads/"); keep(b) {
			st.Branch = b
		}
	case code == 1:
	default:
		return RepoState{}, err
	}
	st.Branches = []string{}
	if maxBranches <= 0 {
		return st, nil
	}
	out, _, err = runRead(ctx, repo, "for-each-ref", "--sort=-committerdate",
		"--count="+strconv.Itoa(maxBranches*4), "--format=%(refname)", "refs/heads/")
	if err != nil {
		return RepoState{}, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		name, ok := strings.CutPrefix(line, "refs/heads/")
		if !ok || !keep(name) {
			continue
		}
		st.Branches = append(st.Branches, name)
		if len(st.Branches) == maxBranches {
			break
		}
	}
	return st, nil
}

// Errors of the HEAD-commit readers below.
var (
	// ErrNoHead: HEAD names an unborn branch (no commit yet).
	ErrNoHead = errors.New("gitx: HEAD has no commit")
	// ErrTooLarge: the tree listing or a blob is bigger than the reader's cap.
	ErrTooLarge = errors.New("gitx: too large")
)

// maxTreeListing (a var so tests can shrink it) caps the bytes of one ls-tree listing (a few hundred
// thousand paths); more is ErrTooLarge, never a silently cut list.
var maxTreeListing = 16 << 20

var objectIDRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ReadHeadCommit is the full SHA of HEAD's commit (ErrNoHead when unborn),
// read-only.
func ReadHeadCommit(ctx context.Context, repo string) (string, error) {
	out, code, err := runRead(ctx, repo, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	switch {
	case err == nil:
		sha := strings.TrimSpace(string(out))
		if !objectIDRe.MatchString(sha) {
			return "", errors.New("gitx: unexpected rev-parse output")
		}
		return sha, nil
	case code == 1:
		return "", ErrNoHead
	}
	return "", err
}

// TreeEntry is one path of a commit's tree. Size is -1 for entries that are
// not blobs (submodule commits).
type TreeEntry struct {
	Mode string // "100644", "100755", "120000" (symlink), "160000" (submodule)
	Type string // "blob" or "commit"
	SHA  string
	Size int64
	Path string
}

// ListTree lists every path of commit's tree recursively (git ls-tree -r -l
// -z --full-tree) with read-only git. It reads the object database only, never
// the work tree. Paths are exactly as stored; the caller decides which are
// safe.
func ListTree(ctx context.Context, repo, commit string) ([]TreeEntry, error) {
	if !objectIDRe.MatchString(commit) {
		return nil, errors.New("gitx: invalid commit id")
	}
	out, truncated, _, err := runReadCap(ctx, repo, maxTreeListing, "ls-tree", "-r", "-l", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrTooLarge
	}
	var ents []TreeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || !objectIDRe.MatchString(f[2]) {
			return nil, errors.New("gitx: unexpected ls-tree output")
		}
		size := int64(-1)
		if f[3] != "-" {
			n, perr := strconv.ParseInt(f[3], 10, 64)
			if perr != nil || n < 0 {
				return nil, errors.New("gitx: unexpected ls-tree output")
			}
			size = n
		}
		ents = append(ents, TreeEntry{Mode: f[0], Type: f[1], SHA: f[2], Size: size, Path: p})
	}
	return ents, nil
}

// ReadBlob returns the content of the blob object sha (cat-file blob, object
// database only: no path, no work tree, no filters), ErrTooLarge when it is
// longer than max bytes.
func ReadBlob(ctx context.Context, repo, sha string, max int) ([]byte, error) {
	if !objectIDRe.MatchString(sha) {
		return nil, errors.New("gitx: invalid object id")
	}
	out, truncated, _, err := runReadCap(ctx, repo, max, "cat-file", "blob", sha)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrTooLarge
	}
	return out, nil
}
