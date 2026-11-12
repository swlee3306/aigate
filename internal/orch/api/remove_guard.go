package api

import (
	"strings"
	"sync"
)

// pathGuard serialises the three things that must never overlap on one
// repository folder: a task submission (a job on the path), a clone into it and
// its removal. All three state changes happen under one mutex, so "no submit or
// clone in flight, then removing" is one atomic decision (W3f review round 1).
//
//   - enterSubmit/leaveSubmit bracket a task submission up to and including the
//     insert of the task, so a task can never be created on a path whose removal
//     already passed its last busy check;
//   - tryClone/endClone bracket one clone run;
//   - tryRemove/endRemove are held from the final busy re-check until the folder
//     was moved aside.
type pathGuard struct {
	mu         sync.Mutex
	removing   map[string]bool
	cloning    map[string]int
	submitting map[string]int
}

func under(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

func (g *pathGuard) init() {
	if g.removing == nil {
		g.removing, g.cloning, g.submitting = map[string]bool{}, map[string]int{}, map[string]int{}
	}
}

// enterSubmit refuses a repo that is, or lies under, a path being removed.
func (g *pathGuard) enterSubmit(repo string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	for r := range g.removing {
		if under(repo, r) {
			return false
		}
	}
	g.submitting[repo]++
	return true
}

func (g *pathGuard) leaveSubmit(repo string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.submitting[repo]--; g.submitting[repo] <= 0 {
		delete(g.submitting, repo)
	}
}

// tryClone refuses a path that is being removed.
func (g *pathGuard) tryClone(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	if g.removing[path] {
		return false
	}
	g.cloning[path]++
	return true
}

func (g *pathGuard) endClone(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cloning[path]--; g.cloning[path] <= 0 {
		delete(g.cloning, path)
	}
}

// tryRemove refuses while a clone of the path runs, a submit for the path (or
// below it) is in flight, or the path is being removed already.
func (g *pathGuard) tryRemove(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	if g.removing[path] || g.cloning[path] > 0 || g.inUseLocked(path) {
		return false
	}
	g.removing[path] = true
	return true
}

func (g *pathGuard) endRemove(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.removing, path)
}

// inUse reports a clone or a submit in flight for the path (the pre-signature check).
func (g *pathGuard) inUse(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	return g.cloning[path] > 0 || g.inUseLocked(path)
}

func (g *pathGuard) inUseLocked(path string) bool {
	for k := range g.submitting {
		if under(k, path) {
			return true
		}
	}
	return false
}
