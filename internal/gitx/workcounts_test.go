package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func gitCfg(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// workRepo makes a clone of a bare "origin" with one pushed commit.
func workRepo(t *testing.T) (repo string) {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	bare := filepath.Join(base, "origin.git")
	repo = filepath.Join(base, "work")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCfg(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one"), 0o644)
	gitCfg(t, repo, "add", ".")
	gitCfg(t, repo, "commit", "-q", "-m", "first")
	gitCfg(t, repo, "remote", "add", "origin", bare)
	gitCfg(t, repo, "push", "-q", "-u", "origin", "main")
	return repo
}

func TestWorkCountsCleanThenUnpushedAndChanged(t *testing.T) {
	repo := workRepo(t)
	c := ReadWorkCounts(context.Background(), repo)
	if !c.UnpushedKnown || c.Unpushed != 0 || !c.ChangedKnown || c.Changed != 0 {
		t.Fatalf("clean: %+v", c)
	}
	// 2 unpushed commits (one on a side branch), 1 modified, 1 untracked, 1 deleted
	os.WriteFile(filepath.Join(repo, "second.txt"), []byte("two"), 0o644)
	gitCfg(t, repo, "add", ".")
	gitCfg(t, repo, "commit", "-q", "-m", "secret commit message")
	gitCfg(t, repo, "checkout", "-q", "-b", "side")
	os.WriteFile(filepath.Join(repo, "third.txt"), []byte("three"), 0o644)
	gitCfg(t, repo, "add", ".")
	gitCfg(t, repo, "commit", "-q", "-m", "on a side branch")
	os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("changed"), 0o644)
	os.WriteFile(filepath.Join(repo, "untracked-secret-name.txt"), []byte("u"), 0o644)
	os.Remove(filepath.Join(repo, "second.txt"))
	c = ReadWorkCounts(context.Background(), repo)
	if !c.UnpushedKnown || c.Unpushed != 2 {
		t.Fatalf("unpushed: %+v", c)
	}
	if !c.ChangedKnown || c.Changed != 3 {
		t.Fatalf("changed: %+v", c)
	}
}

func TestWorkCountsNoRemoteCountsEveryCommit(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "solo")
	os.MkdirAll(repo, 0o755)
	gitCfg(t, repo, "init", "-q", "-b", "main")
	gitCfg(t, repo, "commit", "-q", "--allow-empty", "-m", "a")
	gitCfg(t, repo, "commit", "-q", "--allow-empty", "-m", "b")
	c := ReadWorkCounts(context.Background(), repo)
	if !c.UnpushedKnown || c.Unpushed != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestWorkCountsNotARepoIsUnknown(t *testing.T) {
	d, _ := filepath.EvalSymlinks(t.TempDir())
	c := ReadWorkCounts(context.Background(), d)
	if c.UnpushedKnown || c.ChangedKnown {
		t.Fatalf("%+v", c)
	}
}

// A repository's own configuration must not be able to run a program through
// the read: fsmonitor is forced off, and a filter section (its clean command
// would run during `git status`) makes the changed-file count "unknown"
// without running status at all.
func TestWorkCountsRunNothingFromRepoConfig(t *testing.T) {
	repo := workRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "hook.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	gitCfg(t, repo, "config", "core.fsmonitor", script)
	os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty"), 0o644)
	c := ReadWorkCounts(context.Background(), repo)
	if !c.ChangedKnown || c.Changed != 1 {
		t.Fatalf("%+v", c)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("core.fsmonitor of the repository ran")
	}
	gitCfg(t, repo, "config", "--unset", "core.fsmonitor")
	os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o644)
	gitCfg(t, repo, "config", "filter.evil.clean", script)
	c = ReadWorkCounts(context.Background(), repo)
	if c.ChangedKnown {
		t.Fatalf("a filter section must make the changed count unknown: %+v", c)
	}
	if !c.UnpushedKnown {
		t.Fatalf("the unpushed count does not run filters and stays known: %+v", c)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the clean filter of the repository ran")
	}
}

// A filter driver defined in a SUBMODULE's own config (.git/modules/<name>/config)
// must not run during the status of the superproject.
func TestWorkCountsSubmoduleFilterDoesNotRun(t *testing.T) {
	repo := workRepo(t)
	base := t.TempDir()
	subsrc := filepath.Join(base, "subsrc")
	os.MkdirAll(subsrc, 0o755)
	gitCfg(t, subsrc, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(subsrc, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(subsrc, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o644)
	gitCfg(t, subsrc, "add", ".")
	gitCfg(t, subsrc, "commit", "-q", "-m", "s")
	gitCfg(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subsrc, "sub")
	gitCfg(t, repo, "commit", "-q", "-m", "add sub")
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "hook.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755)
	gitCfg(t, repo, "config", "-f", filepath.Join(repo, ".git", "modules", "sub", "config"), "filter.evil.clean", script)
	// same size, later mtime: git has to run the clean filter to know it changed
	time.Sleep(1100 * time.Millisecond)
	os.WriteFile(filepath.Join(repo, "sub", "a.txt"), []byte("b"), 0o644)
	c := ReadWorkCounts(context.Background(), repo)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a submodule's filter ran during the preview")
	}
	_ = c
}

// .git/commondir makes git read ANOTHER git directory's config: no status is
// run for such a repository (changed count unknown), and the filter there does not run.
func TestWorkCountsCommondirRedirectDoesNotRun(t *testing.T) {
	a := workRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "hook.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755)
	gitCfg(t, a, "config", "filter.evil.clean", script)
	b, _ := filepath.EvalSymlinks(t.TempDir())
	gitCfg(t, b, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(b, ".git", "commondir"), []byte(filepath.Join(a, ".git")+"\n"), 0o644)
	os.WriteFile(filepath.Join(b, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o644)
	gitCfg(t, b, "read-tree", "HEAD")
	os.WriteFile(filepath.Join(b, "tracked.txt"), []byte("dirty"), 0o644)
	c := ReadWorkCounts(context.Background(), b)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a filter of the redirected git directory ran")
	}
	if c.ChangedKnown {
		t.Fatalf("a commondir redirect must make the changed count unknown: %+v", c)
	}
}

// Git's own computed config (all scopes, includes resolved) with a filter or
// include section also blocks status.
func TestWorkCountsComputedConfigGuard(t *testing.T) {
	repo := workRepo(t)
	inc := filepath.Join(t.TempDir(), "inc.cfg")
	os.WriteFile(inc, []byte("[filter \"evil\"]\n\tclean = true\n"), 0o644)
	// an include written with odd spacing that a text scan could miss still shows up in git's own view
	f, _ := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[ include ]\n\tpath = " + inc + "\n")
	f.Close()
	if c := ReadWorkCounts(context.Background(), repo); c.ChangedKnown {
		t.Fatalf("%+v", c)
	}
}
