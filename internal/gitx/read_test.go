package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// R8: discovery stops at the repository itself. An empty .git directory
// inside an outer repository is an error, never the outer repository.
func TestDiscoveryNeverWalksAboveRepo(t *testing.T) {
	outer, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, outer, "init", "-q")
	git(t, outer, "commit", "-q", "--allow-empty", "-m", "outer")
	p := filepath.Join(outer, "inner")
	if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if top, err := TopLevel(p); err == nil {
		t.Fatalf("TopLevel found %q", top)
	}
	if sha, err := ResolveAt(p, "HEAD"); err == nil {
		t.Fatalf("ResolveAt found %q", sha)
	}
	if id, err := ReadRepoIdentity(context.Background(), p); err == nil {
		t.Fatalf("ReadRepoIdentity found %+v", id)
	}
	if st, err := ReadRepoState(context.Background(), p, 10, func(string) bool { return true }); err == nil {
		t.Fatalf("ReadRepoState found %+v", st)
	}
	// The repository itself still works.
	if top, err := TopLevel(outer); err != nil || top != outer {
		t.Fatalf("outer: %q %v", top, err)
	}
	id, err := ReadRepoIdentity(context.Background(), outer)
	if err != nil || id.TopLevel != outer || id.GitDir != filepath.Join(outer, ".git") || id.CommonDir != filepath.Join(outer, ".git") {
		t.Fatalf("identity %+v %v", id, err)
	}
}
