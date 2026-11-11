package clone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// W3c (C6): a repository that is built to run code on the machine that clones
// it. The test lives in this package because the local-file fixture needs the
// package-private seams (production refuses the file transport, see
// TestCloneProductionRefusesFileTransport); the real git binary runs with the
// production option list otherwise.
//
// The hostile repository ships hooks in every way a repository can: tracked
// hook scripts under the usual directory names, filter/diff drivers in
// .gitattributes, a .gitmodules pointing at a local path, and server-side
// hooks in the bare remote. The cloning side additionally has a hostile
// template directory (hooks, reference-transaction, and a config file that
// sets core.fsmonitor and core.hooksPath to scripts; the command line's
// -c core.hooksPath=/dev/null -c core.fsmonitor=false overrule it), the one place git copies
// executable hooks and config from when it initialises the new repository. No
// script may run. The control drops only core.hooksPath and the hook fires, so
// the test proves it can see a hook running.
func TestCloneHookNotExecuted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required: %v", err)
	}
	marks := t.TempDir()
	script := func(name string) string {
		return "#!/bin/sh\ntouch '" + filepath.Join(marks, name) + "'\n"
	}
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "hello\n", 0o644)
	for _, d := range []string{"hooks", ".githooks", ".husky"} {
		for _, h := range []string{"post-checkout", "post-merge", "reference-transaction", "pre-commit"} {
			write(filepath.Join(d, h), script("tracked-"+d+"-"+h), 0o755)
		}
	}
	write(".gitattributes", "* filter=evil\n*.md diff=evil merge=evil\n", 0o644)
	write(".gitmodules", "[submodule \"evil\"]\n\tpath = evil\n\turl = file:///etc\n", 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	want := git(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, work, "clone", "-q", "--bare", work, bare)
	for _, h := range []string{"post-update", "pre-receive", "update", "post-receive", "pre-upload-pack", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(bare, "hooks", h), []byte(script("server-"+h)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tmpl := filepath.Join(t.TempDir(), "tmpl")
	if err := os.MkdirAll(filepath.Join(tmpl, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"post-checkout", "reference-transaction", "post-merge", "post-commit", "fsmonitor-watchman"} {
		if err := os.WriteFile(filepath.Join(tmpl, "hooks", h), []byte(script("template-"+h)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fsm := filepath.Join(marks, "fsmonitor.sh")
	if err := os.WriteFile(fsm, []byte(script("template-config-fsmonitor")), 0o755); err != nil {
		t.Fatal(err)
	}
	hp := filepath.Join(marks, "hp")
	if err := os.MkdirAll(hp, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(hp, "post-checkout"), []byte(script("template-config-hookspath")), 0o755)
	// (A filter driver defined in the template config WOULD run: the template is
	// the cloning host's own trusted directory, not repository content. In
	// production GIT_TEMPLATE_DIR is stripped from the environment, and global
	// and system config are off. A repository cannot define a driver itself, so
	// the .gitattributes above has nothing to call.)
	cfg := "[core]\n\tfsmonitor = " + fsm + "\n\thooksPath = " + hp + "\n"
	if err := os.WriteFile(filepath.Join(tmpl, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(name string, drop bool) (string, error) {
		f := newFix(t)
		o := f.opts(name)
		sm := &seams{
			rewriteRemote: func(string) string { return bare },
			extraConfig:   []string{"protocol.file.allow=always"},
			extraEnv:      []string{"GIT_TEMPLATE_DIR=" + tmpl},
			dropHooksPath: drop,
		}
		if drop { // --template= ignores the template dir, so the control uses core.hooksPath
			sm.extraConfig = append(sm.extraConfig, "core.hooksPath="+hp)
		}
		return o.Dest, clone(context.Background(), o, sm)
	}
	ran := func() []string {
		ents, _ := os.ReadDir(marks)
		var out []string
		for _, e := range ents {
			if strings.Contains(e.Name(), "-") && !strings.HasSuffix(e.Name(), ".sh") && e.Name() != "hp" {
				out = append(out, e.Name())
			}
		}
		return out
	}
	dest, err := run("proj-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, dest, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD %s want %s", got, want)
	}
	if got := ran(); len(got) != 0 {
		t.Fatalf("code of a hostile repository or template ran during the clone: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "evil")); err == nil {
		t.Fatal("a submodule was fetched")
	}
	// the hostile files are just files in the working tree
	if b, err := os.ReadFile(filepath.Join(dest, ".githooks", "post-checkout")); err != nil || !strings.Contains(string(b), "touch") {
		t.Fatalf("tracked hook file missing: %v", err)
	}
	// control: with core.hooksPath no longer forced off, the template's hook runs,
	// so the checks above could have failed
	if _, err := run("proj-b", true); err != nil {
		t.Fatal(err)
	}
	if got := ran(); len(got) == 0 {
		t.Fatal("control failed: no hook ran even with core.hooksPath dropped; the test proves nothing")
	}
}
