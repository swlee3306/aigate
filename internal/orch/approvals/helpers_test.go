package approvals

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func toyRepo(t *testing.T) (repo, base string) {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		full := append([]string{"-C", d, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(d, "a.txt"), []byte("a\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	return d, run("rev-parse", "HEAD")
}
