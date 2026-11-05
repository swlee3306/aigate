package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"aigate/internal/gitx"
)

var shaRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// makeBundle writes a git bundle holding shas (and their history) from repo
// and returns its path and a cleanup function. The commits are named by
// scratch refs in a temporary bare repository that borrows repo's objects, so
// repo's own refs and config stay untouched.
func makeBundle(repo string, shas []string) (path string, cleanup func(), err error) {
	if len(shas) == 0 {
		return "", nil, fmt.Errorf("bundle: no commits requested")
	}
	for _, s := range shas {
		if !shaRE.MatchString(s) {
			return "", nil, fmt.Errorf("bundle: %q is not a full commit id", s)
		}
		if full, err := gitx.Resolve(repo, s+"^{commit}"); err != nil || full != s {
			return "", nil, fmt.Errorf("bundle: commit %s not found in %s", s, repo)
		}
	}
	objs, err := gitOut("-C", repo, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp("", "aigate-bundle-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	scratch := filepath.Join(tmp, "s.git")
	if _, err = gitOut("init", "-q", "--bare", scratch); err != nil {
		return "", nil, err
	}
	if err = os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(objs+"\n"), 0o600); err != nil {
		return "", nil, err
	}
	var refs []string
	seen := map[string]bool{}
	for _, s := range shas {
		if seen[s] {
			continue
		}
		seen[s] = true
		ref := fmt.Sprintf("refs/aigate/%d", len(refs))
		if _, err = gitOut("-C", scratch, "update-ref", ref, s); err != nil {
			return "", nil, err
		}
		refs = append(refs, ref)
	}
	path = filepath.Join(tmp, "b.bundle")
	if _, err = gitOut(append([]string{"-C", scratch, "bundle", "create", "-q", path}, refs...)...); err != nil {
		return "", nil, err
	}
	return path, cleanup, nil
}

func gitOut(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = ": " + strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w%s", args[0], err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}
