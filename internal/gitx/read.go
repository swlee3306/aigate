package gitx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
}

// runRead runs a read-only git query in repo, bounded by ctx, with hooks and
// fsmonitor off. stdout is capped at 1 MiB. The error never carries stderr:
// callers decide what (if anything) to show.
func runRead(ctx context.Context, repo string, args ...string) ([]byte, int, error) {
	full := append([]string{"-C", repo, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(readEnv(), ceilingEnv(repo)...)
	cmd.WaitDelay = time.Second
	var stdout capBuf
	stdout.max = 1 << 20
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil {
			return stdout.b.Bytes(), ee.ExitCode(), err
		}
		return nil, -1, err
	}
	return stdout.b.Bytes(), 0, nil
}

type capBuf struct {
	b   bytes.Buffer
	max int
}

func (c *capBuf) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
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
