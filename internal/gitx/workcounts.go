package gitx

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// WorkCounts are the only facts a removal preview takes from a clone: numbers.
// No file name, branch name, commit message or content leaves this package.
type WorkCounts struct {
	// Unpushed counts the commits on any local branch that no remote-tracking
	// branch contains (they exist only in this clone). Valid when UnpushedKnown.
	Unpushed      int
	UnpushedKnown bool
	// Changed counts the files that differ from HEAD or are untracked (not
	// ignored). Valid when ChangedKnown.
	Changed      int
	ChangedKnown bool
}

// riskyConfig matches the sections of a repository's own configuration that
// can make `git status` run a program (a clean/smudge filter driver) or pull in
// another configuration file.
var riskyConfig = regexp.MustCompile(`(?im)^\s*\[\s*(filter|include|includeif)\b`)

const maxConfigScan = 256 << 10

// configRisky reports whether the clone's own config (or its worktree config)
// has a risky section, or cannot be read plainly: then no status is run.
func configRisky(repo string) bool {
	for _, name := range []string{"config", "config.worktree"} {
		p := filepath.Join(repo, ".git", name)
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxConfigScan {
			return true
		}
		f, err := os.Open(p)
		if err != nil {
			return true
		}
		data, err := io.ReadAll(io.LimitReader(f, maxConfigScan+1))
		f.Close()
		if err != nil || len(data) > maxConfigScan || riskyConfig.Match(data) {
			return true
		}
	}
	return false
}

// ReadWorkCounts counts the work that exists only in the clone at repo, with
// read-only git queries (hooks and fsmonitor off, no optional locks, discovery
// stopped at repo). A count that could not be read stays unknown. A repository
// whose own configuration has a filter or include section gets no status run
// at all (the changed count is unknown), because a filter's command would run
// during it.
func ReadWorkCounts(ctx context.Context, repo string) WorkCounts {
	var wc WorkCounts
	out, trunc, _, err := runReadCap(ctx, repo, 64, "rev-list", "--count", "--branches", "--not", "--remotes")
	if err == nil && !trunc {
		if n, perr := strconv.Atoi(strings.TrimSpace(string(out))); perr == nil && n >= 0 {
			wc.Unpushed, wc.UnpushedKnown = n, true
		}
	}
	if configRisky(repo) {
		return wc
	}
	out, trunc, _, err = runReadCap(ctx, repo, 1<<20, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--no-renames")
	if err == nil && !trunc {
		wc.Changed = bytes.Count(out, []byte{0})
		wc.ChangedKnown = true
	}
	return wc
}
