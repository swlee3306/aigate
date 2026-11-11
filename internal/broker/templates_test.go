package broker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/config"
)

func TestTemplatesParseWithGateConfig(t *testing.T) {
	for _, tpl := range []string{"basic", "go"} {
		files := TemplateFiles(tpl, "my-app")
		if len(files) == 0 || files[0].Path != "README.md" {
			t.Fatalf("%s: README.md must come first: %v", tpl, files)
		}
		var yml string
		for _, f := range files {
			if f.Path == ".aigate.yml" {
				yml = f.Content
			}
		}
		cfg, err := config.Parse([]byte(yml))
		if err != nil {
			t.Fatalf("%s: .aigate.yml does not parse: %v", tpl, err)
		}
		hasProtected := false
		for _, p := range cfg.Protected {
			hasProtected = hasProtected || p == ".aigate.yml"
		}
		if !hasProtected {
			t.Fatalf("%s: .aigate.yml must protect itself: %v", tpl, cfg.Protected)
		}
		if tpl == "go" {
			ids := map[string]config.Check{}
			for _, c := range cfg.Checks {
				ids[c.ID] = c
			}
			for _, id := range []string{"unit", "vet", "build"} {
				if c, ok := ids[id]; !ok || !c.Required {
					t.Fatalf("go: check %s missing or not required: %+v", id, cfg.Checks)
				}
			}
			if ids["unit"].Preset != "go" {
				t.Fatal("unit must use the go preset")
			}
			for _, want := range []string{"**/*_test.go", "go.mod", ".aigate.yml"} {
				found := false
				for _, p := range cfg.Protected {
					found = found || p == want
				}
				if !found {
					t.Fatalf("go: %s not protected: %v", want, cfg.Protected)
				}
			}
		}
	}
}

func TestGoTemplateVetsAndTests(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool")
	}
	dir := t.TempDir()
	for _, f := range TemplateFiles("go", "my-app") {
		p := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"vet", "./..."}, {"test", "./..."}, {"build", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "GOTOOLCHAIN=local")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
}

func TestTemplateContentFixed(t *testing.T) {
	// only the validated name is substituted: two different names yield the
	// same files except where the name stands
	const a, b = "alpha-one", "beta-two-xx"
	for _, tpl := range []string{"basic", "go"} {
		fa, fb := TemplateFiles(tpl, a), TemplateFiles(tpl, b)
		if len(fa) != len(fb) {
			t.Fatalf("%s: file sets differ", tpl)
		}
		for i := range fa {
			if fa[i].Path != fb[i].Path || strings.Contains(fa[i].Path, a) {
				t.Fatalf("%s: path depends on the name: %s", tpl, fa[i].Path)
			}
			if strings.ReplaceAll(fa[i].Content, a, "NAME") != strings.ReplaceAll(fb[i].Content, b, "NAME") {
				t.Fatalf("%s %s: content differs beyond the name", tpl, fa[i].Path)
			}
		}
	}
	// an unsafe or unknown input yields nothing (no file text from a request)
	for _, bad := range []string{"", "A", "a", "x\nmodule evil", "../x", "a b", "a\"b", "a;b", strings.Repeat("a", 41)} {
		if TemplateFiles("go", bad) != nil || TemplateFiles("basic", bad) != nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
	if TemplateFiles("rust", "ok-name") != nil || TemplateFiles("", "ok-name") != nil {
		t.Fatal("unknown template accepted")
	}
	// every file passes the GitLab client's own file rules (<= 20 files, paths, size, text)
	for _, tpl := range []string{"basic", "go"} {
		var cf []CommitFile
		for _, f := range TemplateFiles(tpl, "ok-name") {
			cf = append(cf, f)
		}
		if !validCommitFiles(cf) {
			t.Fatalf("%s: files rejected by the client rules", tpl)
		}
	}
	// the template text is a code constant: it never reads the environment or request text
	t.Setenv("USER_TEXT", "injected")
	for _, f := range TemplateFiles("go", "ok-name") {
		if strings.Contains(f.Content, "injected") {
			t.Fatal("environment text in template")
		}
	}
}
