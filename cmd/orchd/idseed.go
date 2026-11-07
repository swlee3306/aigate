package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigate/internal/orch/store"
	"aigate/internal/remote"
)

const (
	idSeedGitTimeout = 10 * time.Second
	idSeedMaxID      = 1_000_000_000_000
)

// idSource is one place that can hold task-id traces.
type idSource struct {
	dir    string
	prefix string // ref namespace to list, e.g. "refs/orch/"
	remote bool
}

// parseTaskRef extracts the task id from refs/orch/<digits>/head (local) or
// refs/heads/orch/<digits> (remote). Anything else yields ok=false.
func parseTaskRef(ref string, remote bool) (int64, bool) {
	var digits string
	if remote {
		digits = strings.TrimPrefix(ref, "refs/heads/orch/")
		if digits == ref {
			return 0, false
		}
	} else {
		rest := strings.TrimPrefix(ref, "refs/orch/")
		if rest == ref || !strings.HasSuffix(rest, "/head") {
			return 0, false
		}
		digits = strings.TrimSuffix(rest, "/head")
	}
	if digits == "" || len(digits) > 13 {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n > idSeedMaxID {
		return 0, false
	}
	return n, true
}

// highestRefID runs git for-each-ref in dir with a sanitized environment and
// returns the highest task id found (0 if none).
func highestRefID(ctx context.Context, src idSource) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, idSeedGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", src.dir, "for-each-ref", "--format=%(refname)", src.prefix)
	cmd.Env = append(remote.GitEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if ctx.Err() != nil {
			msg = "timed out after " + idSeedGitTimeout.String()
		}
		return 0, fmt.Errorf("%v: %s", err, msg)
	}
	var max int64
	for _, line := range strings.Split(out.String(), "\n") {
		if n, ok := parseTaskRef(strings.TrimSpace(line), src.remote); ok && n > max {
			max = n
		}
	}
	return max, nil
}

// idSources lists the git repositories to inspect. Problems reading a root
// are returned as warnings, not errors.
func idSources(roots []string, gitRemote string) (srcs []idSource, warns []string) {
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			warns = append(warns, fmt.Sprintf("repo root %s: %v", root, err))
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(root, e.Name())
			if !e.IsDir() {
				if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
					continue
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
				continue // not a git work tree
			}
			srcs = append(srcs, idSource{dir: dir, prefix: "refs/orch/"})
		}
	}
	if gitRemote != "" && filepath.IsAbs(gitRemote) && !strings.Contains(gitRemote, "://") {
		srcs = append(srcs, idSource{dir: gitRemote, prefix: "refs/heads/orch/", remote: true})
	}
	return srcs, warns
}

// seedTaskIDs makes sure the next task id is above every id that already left
// a ref in the allowed repositories or the local push remote, so a replaced
// database never reuses one. Failures are warnings; startup continues.
func seedTaskIDs(ctx context.Context, st *store.Store, roots []string, gitRemote string, logf func(string, ...any)) {
	srcs, warns := idSources(roots, gitRemote)
	var max int64
	found := map[string]int64{}
	for _, s := range srcs {
		n, err := highestRefID(ctx, s)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", s.dir, err))
			continue
		}
		if n > 0 {
			found[s.dir] = n
			if n > max {
				max = n
			}
		}
	}
	for _, w := range warns {
		logf("orchd: warning: task id seeding could not read %s", w)
	}
	if len(warns) > 0 {
		logf("orchd: WARNING: %d source(s) of task id history could not be read; if the database was replaced, ids from those repositories may be reused", len(warns))
	}
	if max == 0 {
		return
	}
	before, err := st.TaskSeq(ctx)
	if err != nil {
		logf("orchd: WARNING: reading the task id sequence: %v", err)
		return
	}
	if err := st.EnsureMinNextTaskID(ctx, max+1); err != nil {
		logf("orchd: WARNING: seeding the task id sequence: %v", err)
		return
	}
	if max > before {
		var where []string
		for d, n := range found {
			if n == max {
				where = append(where, d)
			}
		}
		sort.Strings(where)
		logf("orchd: task id sequence starts after %d (highest id found in refs: %s)", max, strings.Join(where, ", "))
	}
}
