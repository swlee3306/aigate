package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
