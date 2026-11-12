// Package remove deletes the local files of one registered project: its clone
// directly under a repository root and its per-repository key folder. It is the
// only place orchd removes a project folder, and it is built to be hard to
// misuse:
//
//   - the target is named by the registry path and the project id together and
//     must be exactly one level below a configured root, with the id as its
//     name (a plain project id, never a dot name, never a path);
//   - the target is looked at with Lstat (a symlink, a file, a directory
//     without a .git directory are refused, never followed);
//   - it is first RENAMED to <root>/.deleting-<random> (no replace, same file
//     system), so a crash never leaves a half-deleted folder under the project
//     name; the renamed entry is checked again with SameFile against what was
//     looked at, and only that exact staged directory is passed to RemoveAll;
//   - a folder that is gone already is success (idempotent) and nothing else is
//     ever touched; anything that exists but is not a removable project folder
//     is refused untouched;
//   - Sweep removes the .deleting-<hex> leftovers of a crash at startup, only
//     real directories of the running user directly under the given folders.
//
// Nothing here decides WHETHER a project may be removed (signature, busy tasks,
// legacy second confirmation): the caller does, and calls this only after.
package remove

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"aigate/internal/orch/fsx"
	"aigate/internal/registrymsg"
)

// StagePrefix starts the name of a directory that is being deleted. It is not a
// valid project id (ids start with a letter).
const StagePrefix = ".deleting-"

// RefusedPrefix starts the name an entry is parked under when it was swapped in
// during a removal and could not be put back: the sweep never touches it.
const RefusedPrefix = ".remove-refused-"

// IsOurName reports whether a directory name is one orchd's removal makes
// (never a project id).
func IsOurName(name string) bool {
	return strings.HasPrefix(name, StagePrefix) || strings.HasPrefix(name, RefusedPrefix)
}

// Outcome says what a call found or did.
type Outcome string

const (
	// Removed: the folder existed and is gone.
	Removed Outcome = "removed"
	// AlreadyGone: nothing was at the target (success, nothing changed).
	AlreadyGone Outcome = "already_gone"
	// Remaining: removal stopped after the rename; the content sits under a
	// .deleting-<random> name until the next startup sweep.
	Remaining Outcome = "remaining"
)

// Fixed errors: none carries a path.
var (
	// ErrUnsafe: the target is not something this package may remove; nothing
	// was changed (or a swapped-in entry was put back).
	ErrUnsafe = errors.New("remove: the target is not a removable project folder")
	// ErrNotRemoved: the rename failed; nothing was changed.
	ErrNotRemoved = errors.New("remove: the folder could not be moved aside; nothing was changed")
	// ErrPartial: the folder was moved aside but not fully deleted.
	ErrPartial = errors.New("remove: removal stopped part way")
	// ErrInProgress: another removal of the same target is running.
	ErrInProgress = errors.New("remove: this folder is being removed already")
)

// seams are the test-only replacements (swap races, a failing delete). The type
// is unexported and production code never builds one; the exported functions
// pass nil. TestProductionCannotEnableRemoveSeams pins that.
type seams struct {
	// afterCheck runs after the target was checked and before it is renamed.
	afterCheck func()
	// removeAll replaces os.RemoveAll.
	removeAll func(path string) error
	// undo replaces the no-replace rename used to put a swapped-in entry back.
	undo func(from, to string) error
}

var (
	inflightMu sync.Mutex
	inflight   = map[string]bool{}
)

func lock(p string) bool {
	inflightMu.Lock()
	defer inflightMu.Unlock()
	if inflight[p] {
		return false
	}
	inflight[p] = true
	return true
}

func unlock(p string) {
	inflightMu.Lock()
	delete(inflight, p)
	inflightMu.Unlock()
}

func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func isRealDir(fi os.FileInfo) bool { return fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 }

// rootOK: an absolute clean path that is its own real path (no symlink in it).
func rootOK(r string) bool {
	if r == "" || !filepath.IsAbs(r) || filepath.Clean(r) != r {
		return false
	}
	real, err := filepath.EvalSymlinks(r)
	return err == nil && real == r
}

// target checks the registry path against the roots and the id. It returns
// the root the folder lives in.
func target(roots []string, path, id string) (string, bool) {
	if !registrymsg.ValidSubject(id) || path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		filepath.Base(path) != id {
		return "", false
	}
	parent := filepath.Dir(path)
	for _, r := range roots {
		if r == parent && rootOK(r) {
			return r, true
		}
	}
	return "", false
}

// CheckClone is Clone without any change: exists says whether a removable
// project folder is at path; ErrUnsafe says something is there that Clone
// would refuse.
func CheckClone(roots []string, path, id string) (exists bool, err error) {
	root, ok := target(roots, path, id)
	if !ok {
		return false, ErrUnsafe
	}
	_, exists, err = checkDir(root, path, true)
	return exists, err
}

// checkDir looks at root/<name> (path): absent -> (nil, false, nil); a real
// directory (with a .git directory when needGit, owned by us otherwise) ->
// (info, true, nil); anything else -> ErrUnsafe.
func checkDir(root, path string, needGit bool) (os.FileInfo, bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !isRealDir(fi) {
		return nil, false, ErrUnsafe
	}
	if needGit {
		gi, err := os.Lstat(filepath.Join(path, ".git"))
		if err != nil || !isRealDir(gi) {
			return nil, false, ErrUnsafe
		}
	} else if !ownedByUs(fi) {
		return nil, false, ErrUnsafe
	}
	return fi, true, nil
}

// Clone removes the clone at path (exactly <root>/<id>).
func Clone(roots []string, path, id string) (Outcome, error) {
	return removeClone(roots, path, id, nil)
}

func removeClone(roots []string, path, id string, s *seams) (Outcome, error) {
	root, ok := target(roots, path, id)
	if !ok {
		return "", ErrUnsafe
	}
	return removeIn(root, path, true, s)
}

// KeyDir removes keysDir/<id> (a project's per-repository key folder). A shared
// key is never in such a folder; a missing keys directory means nothing to remove.
func KeyDir(keysDir, id string) (Outcome, error) { return removeKeyDir(keysDir, id, nil) }

// CheckKeyDir is KeyDir without any change.
func CheckKeyDir(keysDir, id string) (exists bool, err error) {
	if !registrymsg.ValidSubject(id) || keysDir == "" || !filepath.IsAbs(keysDir) || filepath.Clean(keysDir) != keysDir {
		return false, ErrUnsafe
	}
	ki, err := os.Lstat(keysDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	// the whole path must be real (no symlink anywhere in its parent chain)
	if err != nil || !isRealDir(ki) || !ownedByUs(ki) || !rootOK(keysDir) {
		return false, ErrUnsafe
	}
	_, exists, err = checkDir(keysDir, filepath.Join(keysDir, id), false)
	return exists, err
}

func removeKeyDir(keysDir, id string, s *seams) (Outcome, error) {
	if _, err := CheckKeyDir(keysDir, id); err != nil {
		return "", err
	}
	if _, err := os.Lstat(keysDir); err != nil {
		return AlreadyGone, nil
	}
	return removeIn(keysDir, filepath.Join(keysDir, id), false, s)
}

// removeIn does the work for parent/<name> == path.
func removeIn(parent, path string, needGit bool, s *seams) (Outcome, error) {
	if !lock(path) {
		return "", ErrInProgress
	}
	defer unlock(path)
	fi, found, err := checkDir(parent, path, needGit)
	if err != nil {
		return "", err
	}
	if !found {
		return AlreadyGone, nil
	}
	if s != nil && s.afterCheck != nil {
		s.afterCheck()
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrNotRemoved
	}
	stage := filepath.Join(parent, StagePrefix+hex.EncodeToString(b[:]))
	if err := fsx.RenameNoReplace(path, stage); err != nil {
		return "", ErrNotRemoved
	}
	// What now sits at the staged name must be the directory that was checked.
	// A swapped-in symlink or directory moved as itself (rename never follows):
	// it is put back and nothing is deleted.
	fi2, err := os.Lstat(stage)
	if err != nil || !isRealDir(fi2) || !os.SameFile(fi, fi2) {
		undo := fsx.RenameNoReplace
		if s != nil && s.undo != nil {
			undo = s.undo
		}
		if undo(stage, path) != nil {
			// Not ours to delete and not possible to put back: park it where the
			// startup sweep will not touch it.
			var rb [8]byte
			_, _ = rand.Read(rb[:])
			if fsx.RenameNoReplace(stage, filepath.Join(parent, RefusedPrefix+hex.EncodeToString(rb[:]))) != nil {
				log.Printf("orchd: an entry swapped in during a removal could not be moved back or parked")
			} else {
				log.Printf("orchd: an entry swapped in during a removal was parked under a %s name", RefusedPrefix)
			}
		}
		return "", ErrUnsafe
	}
	del := os.RemoveAll
	if s != nil && s.removeAll != nil {
		del = s.removeAll
	}
	if err := del(stage); err != nil {
		return Remaining, ErrPartial
	}
	return Removed, nil
}

var stageNameRE = regexp.MustCompile(`^\.deleting-[0-9a-f]{16}$`)

// Sweep removes the .deleting-<16 hex> directories a crashed removal left
// directly under each given directory (the repository roots and the keys
// directory). Only real directories owned by the running user are removed (a
// symlink, a file, another name shape is left alone, nothing is followed,
// nothing deeper is looked at). It returns how many were removed.
func Sweep(dirs []string) int {
	n := 0
	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !stageNameRE.MatchString(e.Name()) {
				continue
			}
			p := filepath.Join(d, e.Name())
			if fi, err := os.Lstat(p); err == nil && isRealDir(fi) && ownedByUs(fi) {
				if os.RemoveAll(p) == nil {
					n++
				}
			}
		}
	}
	return n
}
