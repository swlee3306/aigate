package clone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Final review M2: a clone started by the retry endpoint must end well before
// the web's 130 s write timeout.
func TestDefaultCloneTimeoutFitsTheRetryPath(t *testing.T) {
	if DefaultTimeout != 100*time.Second {
		t.Fatalf("DefaultTimeout = %v, want 100s", DefaultTimeout)
	}
}

// Re-review 4: --template= really switches templates off. A hostile template
// directory (a hook and a config with a filter driver that .gitattributes
// uses) is ignored with the production argv and fires when the option is
// dropped (control).
func TestEmptyTemplateDisablesTemplates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required: %v", err)
	}
	marks := t.TempDir()
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644)
	os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("*.md filter=x\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, work, "clone", "-q", "--bare", work, bare)
	tmpl := filepath.Join(t.TempDir(), "tmpl")
	os.MkdirAll(filepath.Join(tmpl, "hooks"), 0o755)
	os.WriteFile(filepath.Join(tmpl, "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "hook")+"'\n"), 0o755)
	os.WriteFile(filepath.Join(tmpl, "config"), []byte("[filter \"x\"]\n\tsmudge = touch "+filepath.Join(marks, "filter")+"; cat\n\tclean = cat\n"), 0o644)
	run := func(name string, dropTemplate bool) {
		f := newFix(t)
		o := f.opts(name)
		sm := &seams{rewriteRemote: func(string) string { return bare }, extraConfig: []string{"protocol.file.allow=always"},
			extraEnv: []string{"GIT_TEMPLATE_DIR=" + tmpl}, dropHooksPath: dropTemplate, dropTemplate: dropTemplate}
		if err := clone(context.Background(), o, sm); err != nil {
			t.Fatal(err)
		}
	}
	ran := func() int { ents, _ := os.ReadDir(marks); return len(ents) }
	run("proj-a", false)
	if ran() != 0 {
		t.Fatal("a template hook or filter ran with --template=")
	}
	run("proj-b", true) // control: no --template=, hooksPath not forced off either
	if _, err := os.Stat(filepath.Join(marks, "filter")); err != nil {
		t.Fatalf("control failed: the template filter did not run without --template= (%v): the test proves nothing", err)
	}
}
