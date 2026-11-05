// Package gittest holds git fixture helpers shared by tests. It runs the git
// binary directly and imports nothing from aigate (so gitx's own tests can
// use it). Only test code imports it.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Plumb commits parent's tree plus files (path → content, mode 100644) as a
// child of parent with git plumbing (`hash-object -w`, `update-index --add
// --cacheinfo` on a temporary index, `write-tree`, `commit-tree`), with
// core.ignorecase and core.precomposeunicode off. The working tree, the main
// index and HEAD stay untouched, so the tree can hold paths a case- or
// normalization-insensitive file system cannot (tests/Run.js and
// tests/run.js, NFC and NFD), and the main tree stays clean. It fails the
// test when the new tree does not list every added path (a fixture that
// silently collapsed would make collision tests pass vacuously).
func Plumb(t testing.TB, repo, parent string, files map[string]string) string {
	t.Helper()
	idx := filepath.Join(t.TempDir(), "index")
	g := func(stdin string, args ...string) string {
		t.Helper()
		full := append([]string{"-C", repo, "-c", "core.ignorecase=false", "-c", "core.precomposeunicode=false",
			"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	g("", "read-tree", parent)
	for p, c := range files {
		sha := strings.TrimSpace(g(c, "hash-object", "-w", "--stdin"))
		g("", "update-index", "--add", "--cacheinfo", "100644,"+sha+","+p)
	}
	tree := strings.TrimSpace(g("", "write-tree"))
	sha := strings.TrimSpace(g("", "commit-tree", tree, "-p", parent, "-m", "plumbed"))
	have := map[string]bool{}
	for _, p := range strings.Split(strings.TrimRight(g("", "ls-tree", "-r", "-z", "--name-only", "--full-tree", sha), "\x00"), "\x00") {
		have[p] = true
	}
	for p := range files {
		if !have[p] {
			t.Fatalf("plumbed tree lacks %q (fixture collapsed)", p)
		}
	}
	return sha
}
