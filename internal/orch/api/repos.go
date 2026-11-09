package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"aigate/internal/gitx"
)

const (
	// DefaultReposMax caps the GET /api/v1/repos list.
	DefaultReposMax = 200
	// reposDepth is how deep under each allowed root repositories are looked
	// for (the root itself is depth 0).
	reposDepth = 2
	// reposBranches is how many recent local branches each entry carries.
	reposBranches = 10
	// reposPerRepoTimeout and defaultReposTimeout bound the git queries of
	// one repository and of the whole request.
	reposPerRepoTimeout = 5 * time.Second
	defaultReposTimeout = 15 * time.Second
	// reposMaxDirs bounds how many directories one request reads while
	// looking for repositories.
	reposMaxDirs = 5000
	reposWorkers = 4
	maxRefName   = 200
)

// errReposUnavailable is the only error GET /repos returns: fixed text, no
// path, no git output.
var errReposUnavailable = apiErr(http.StatusServiceUnavailable, "repos_unavailable", "저장소 목록을 지금 읽을 수 없습니다. 잠시 후 다시 시도하세요")

// RepoView is one entry of GET /api/v1/repos. Path is the symlink-resolved
// real path, the same value submit stores as the task's repo.
type RepoView struct {
	Path     string   `json:"path"`
	Head     string   `json:"head"`
	Branch   string   `json:"branch"`
	Branches []string `json:"branches"`
}

// repos lists the git work trees under the allowed roots (depth <= 2), sorted
// by path, at most reposMax. Only read-only git queries run, with a timeout
// per repository and per request. A repository git cannot read is left out;
// an expired request deadline is repos_unavailable.
func (h *handler) repos(w http.ResponseWriter, r *http.Request) {
	timeout := h.reposTimeout
	if timeout <= 0 {
		timeout = defaultReposTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	limit := h.reposMax
	if limit <= 0 {
		limit = DefaultReposMax
	}
	paths := h.findRepos(ctx)
	if ctx.Err() != nil {
		writeErr(w, errReposUnavailable)
		return
	}
	if len(paths) > limit {
		paths = paths[:limit]
	}
	views := make([]*RepoView, len(paths))
	next := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < reposWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				rctx, rcancel := context.WithTimeout(ctx, reposPerRepoTimeout)
				st, err := gitx.ReadRepoState(rctx, paths[j], reposBranches, safeRefName)
				rcancel()
				if err == nil {
					views[j] = &RepoView{Path: paths[j], Head: st.Head, Branch: st.Branch, Branches: st.Branches}
				}
			}
		}()
	}
	for j := range paths {
		next <- j
	}
	close(next)
	wg.Wait()
	if ctx.Err() != nil {
		writeErr(w, errReposUnavailable)
		return
	}
	out := make([]RepoView, 0, len(views))
	for _, v := range views {
		if v != nil {
			out = append(out, *v)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// findRepos walks every allowed root to reposDepth without following
// directory symlinks and returns the sorted, de-duplicated real paths of the
// git work trees found (a directory with a .git entry, as validate requires)
// that resolve under an allowed root. Unreadable entries are skipped.
func (h *handler) findRepos(ctx context.Context) []string {
	seen := map[string]bool{}
	dirsRead := 0
	var visit func(p string, depth int, walk bool)
	visit = func(p string, depth int, walk bool) {
		if ctx.Err() != nil {
			return
		}
		if real, err := filepath.EvalSymlinks(p); err == nil && h.underRoot(real) && safePath(real) {
			if fi, err := os.Stat(real); err == nil && fi.IsDir() {
				if _, err := os.Lstat(filepath.Join(real, ".git")); err == nil {
					seen[real] = true
				}
			}
		}
		if !walk || depth >= reposDepth || dirsRead >= reposMaxDirs {
			return
		}
		dirsRead++
		ents, err := os.ReadDir(p)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.Name() == ".git" {
				continue
			}
			switch t := e.Type(); {
			case t.IsDir():
				visit(filepath.Join(p, e.Name()), depth+1, true)
			case t&os.ModeSymlink != 0:
				// A candidate itself (by its real path), never walked into.
				visit(filepath.Join(p, e.Name()), depth+1, false)
			}
		}
	}
	for _, root := range h.roots {
		fi, err := os.Lstat(root)
		if err != nil || !fi.IsDir() {
			continue
		}
		visit(root, 0, true)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// safeRefName admits branch names the console may show: valid UTF-8 of
// printable ASCII and Hangul only, at most maxRefName bytes.
func safeRefName(s string) bool {
	if s == "" || len(s) > maxRefName || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !(r >= 0x20 && r < 0x7f) && !isHangul(r) {
			return false
		}
	}
	return true
}

func isHangul(r rune) bool {
	return (r >= 0xAC00 && r <= 0xD7A3) || (r >= 0x1100 && r <= 0x11FF) || (r >= 0x3130 && r <= 0x318F)
}

// safePath admits repository paths without control characters (valid
// UTF-8, no C0/C1 controls or DEL).
func safePath(s string) bool {
	if !utf8.ValidString(s) || len(s) > 4096 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			return false
		}
	}
	return true
}
