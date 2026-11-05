package main

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// polyglots are the six files the fix wave 5 re-reviewer built: each starts
// with (or carries at its offset) a valid magic number of its format AND is
// valid JavaScript that default Node require() runs (an unknown extension is
// loaded as JS). Each contains a NUL, so git and approve see it as binary.
var polyglots = map[string]string{
	".gif": "GIF89a=0;module.exports=()=>'all tests passed GIF'//\x00\n",
	".ttf": "true;module.exports=()=>'all tests passed TTF'//\x00\n",
	".bmp": "BM=0;module.exports=()=>'all tests passed BMP'//\x00\n",
	".mp3": "ID3=0;module.exports=()=>'all tests passed MP3'//\x00\n",
	".mp4": "//\x00\x18ftypmp42\nmodule.exports=()=>'all tests passed MP4'\n",
	".tar": "/*" + strings.Repeat(" ", 255) + "ustar\x00*/module.exports=()=>'all tests passed TAR'\n",
}

// The polyglots really pass the existing allowlist + magic rule, so outside
// a loaded-code directory they are summarisable: the attack is not caught by
// summaryRefusal, only by the directory rule.
func TestPolyglotsPassMagicCheck(t *testing.T) {
	for e, c := range polyglots {
		if !strings.Contains(c, "\x00") || !binaryMagic[e]([]byte(c)) {
			t.Errorf("%s fixture is not a binary that carries its magic", e)
		}
	}
}

// FIX WAVE 6 (user decision 2026-11-05): a binary ADDED under a directory a
// runner loads code from is never approvable from a summary line, even when
// it is a valid file of its format: the six polyglots under
// node_modules/helper/ (package.json main → the file) and under vendor/ are
// refused with binary_in_loaded_dir before anything is printed; the content
// is never printed and nothing is granted.
func TestApproveRefusesPolyglotsInLoadedDirs(t *testing.T) {
	for e, content := range polyglots {
		for _, dir := range []string{"node_modules/helper", "vendor/helper"} {
			p := dir + "/logo" + e
			t.Run(p, func(t *testing.T) {
				repo, base := newRepo(t)
				if strings.HasPrefix(dir, "node_modules") {
					write(t, repo, dir+"/package.json", `{"main":"logo`+e+`"}`+"\n")
				}
				write(t, repo, p, content)
				commit(t, repo, "polyglot")
				errOut := refusedApprove(t, repo, base, p, "binary_in_loaded_dir")
				for _, s := range []string{"코드", "요약", "형식", "저장소 밖", "직접 커밋", "다음 base", "우회 플래그"} {
					if !strings.Contains(errOut, s) {
						t.Errorf("stderr lacks %q: %q", s, errOut)
					}
				}
				if strings.Contains(errOut, "module.exports") || strings.Contains(errOut, "all tests passed") {
					t.Errorf("content printed: %q", errOut)
				}
				if lr := lastRefusal(t, repo); lr["needed_approval"] != true {
					t.Errorf("needed_approval %v", lr["needed_approval"])
				}
			})
		}
	}
}

// The same polyglots under tests/ stay summarisable and approvable: tests/
// fixtures are data. A test that loads such a fixture as code is out of scope
// (design doc 4, v0.7).
func TestApprovePolyglotsUnderTestsSummarised(t *testing.T) {
	repo, base := newRepo(t)
	for e, content := range polyglots {
		write(t, repo, "tests/fixtures/logo"+e, content)
	}
	commit(t, repo, "fixtures")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || rawUnsafe(out) || strings.Contains(out, "module.exports") {
		t.Fatalf("approve: %d %q %q", code, out, errOut)
	}
	for e := range polyglots {
		if !strings.Contains(out, "binary file changed: tests/fixtures/logo"+e+", ") {
			t.Errorf("no summary for %s: %q", e, out)
		}
	}
}

// Whole path components match case-insensitively at any depth; a component
// that only contains the name (my_node_modules_backup, vendors) does not. A
// genuine PNG is used: the directory alone decides.
func TestApproveLoadedDirMatching(t *testing.T) {
	const png = "\x89PNG\r\n\x1a\n\x00real\n"
	for _, p := range []string{"tests/Node_Modules/x.png", "tests/VENDOR/x.png", "a/b/node_modules/c/x.png", "tests/site-packages/x.png", "tests/.venv/lib/x.png", "tests/Third_Party/x.png"} {
		t.Run("refused "+p, func(t *testing.T) {
			repo, base := newRepo(t)
			write(t, repo, p, png)
			commit(t, repo, "png")
			refusedApprove(t, repo, base, p, "binary_in_loaded_dir")
		})
	}
	for _, p := range []string{"tests/my_node_modules_backup/x.png", "tests/vendors/x.png", "tests/vendored/x.png", "tests/node_modules.png"} {
		t.Run("summarised "+p, func(t *testing.T) {
			repo, base := newRepo(t)
			write(t, repo, p, png)
			commit(t, repo, "png")
			if code, out, e := approveTTY(t, repo, base, "yes\n"); code != 0 || !strings.Contains(out, "binary file changed: "+p+", ") {
				t.Fatalf("%s: %d %q %q", p, code, out, e)
			}
		})
	}
}

// A deleted binary under node_modules is still a summary line and
// approvable; a modified existing genuine png there is refused.
func TestApproveLoadedDirDeletedAndModified(t *testing.T) {
	const png = "\x89PNG\r\n\x1a\n\x00"
	t.Run("deleted", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "node_modules/pkg/old.png", png+"old\n")
		write(t, repo, "vendor/pkg/old.gif", polyglots[".gif"])
		base = commit(t, repo, "base2")
		for _, p := range []string{"node_modules/pkg/old.png", "vendor/pkg/old.gif"} {
			if err := os.Remove(filepath.Join(repo, p)); err != nil {
				t.Fatal(err)
			}
		}
		commit(t, repo, "delete")
		code, out, errOut := approveTTY(t, repo, base, "yes\n")
		if code != 0 || !strings.Contains(out, "approved.") || !strings.Contains(out, "binary file changed: node_modules/pkg/old.png, 0 bytes") || !strings.Contains(out, "binary file changed: vendor/pkg/old.gif, 0 bytes") {
			t.Fatalf("deleted: %d %q %q", code, out, errOut)
		}
	})
	t.Run("modified", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "node_modules/pkg/icon.png", png+"old\n")
		base = commit(t, repo, "base2")
		write(t, repo, "node_modules/pkg/icon.png", png+"new\n")
		commit(t, repo, "modify")
		refusedApprove(t, repo, base, "node_modules/pkg/icon.png", "binary_in_loaded_dir")
	})
	// a symlink blob is text, so symlink → binary is text_became_binary
	// (that check keeps its precedence), never a summary
	t.Run("type-changed symlink to binary", func(t *testing.T) {
		repo, base := newRepo(t)
		symlink(t, repo, "../x.png", "vendor/pkg/icon.png")
		base = commit(t, repo, "base2")
		if err := os.Remove(filepath.Join(repo, "vendor/pkg/icon.png")); err != nil {
			t.Fatal(err)
		}
		write(t, repo, "vendor/pkg/icon.png", png+"new\n")
		commit(t, repo, "type change")
		refusedApprove(t, repo, base, "vendor/pkg/icon.png", "text_became_binary")
	})
}

// Text files under node_modules are unaffected: shown and reviewed as text.
func TestApproveLoadedDirTextUnaffected(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "node_modules/helper/index.js", "module.exports = () => 'hi'\n")
	commit(t, repo, "text")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || !strings.Contains(out, "+module.exports = () => 'hi'") || strings.Contains(out, "binary file changed") {
		t.Fatalf("text under node_modules: %d %q %q", code, out, errOut)
	}
}

// Precedence (cli.go binaryRefusal): binary_became_text, text_became_binary
// and binary_executable keep their codes under a loaded directory;
// binary_in_loaded_dir outranks binary_loadable_code, binary_source_file,
// binary_unknown_type and binary_magic_mismatch for the same file.
func TestApproveLoadedDirPrecedence(t *testing.T) {
	cases := []struct {
		name, path, base, head string
		exec                   bool
		reason                 string
	}{
		{"binary became text", "node_modules/pkg/a.png", "\x89PNG\r\n\x1a\n\x00", "text\n", false, "binary_became_text"},
		{"text became binary", "node_modules/pkg/a.js", "text\n", "te\x00xt\n", false, "text_became_binary"},
		{"executable .bin runner", "node_modules/.bin/vitest", "", "#!/usr/bin/env node\n\x00x\n", true, "binary_executable"},
		{"loadable addon", "node_modules/addon/build/Release/addon.node", "", "\xcf\xfa\xed\xfe\x00\n", false, "binary_in_loaded_dir"},
		{"source name", "vendor/pkg/a.go", "", "package a\x00\n", false, "binary_in_loaded_dir"},
		{"unknown type", "vendor/sub/tool", "", "data\x00\n", false, "binary_in_loaded_dir"},
		{"magic mismatch", "node_modules/helper/logo.png", "", "module.exports=1//\x00\n", false, "binary_in_loaded_dir"},
		{"magic-less modified", "node_modules/pkg/x.dat", "a\x00\n", "b\x00\n", false, "binary_in_loaded_dir"},
		{"cpython extension module", "vendor/pkg/mod.cpython-312-darwin.so", "", "\x7fELF\x00\n", false, "binary_in_loaded_dir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, base := newRepo(t)
			if c.base != "" {
				write(t, repo, c.path, c.base)
				base = commit(t, repo, "base2")
			}
			mode := os.FileMode(0o644)
			if c.exec {
				mode = 0o755
			}
			writeMode(t, repo, c.path, c.head, mode)
			commit(t, repo, "head")
			refusedApprove(t, repo, base, c.path, c.reason)
		})
	}
}

// A path is only printed escaped in the loaded-dir refusal.
func TestApproveLoadedDirEscapesPath(t *testing.T) {
	repo, base := newRepo(t)
	p := "node_modules/helper/lo\x1bgo" + path.Ext("x.gif")
	write(t, repo, p, polyglots[".gif"])
	commit(t, repo, "esc")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 64 || out != "" || rawUnsafe(errOut) || !strings.Contains(errOut, `lo\x1bgo.gif`) || !strings.Contains(errOut, "binary_in_loaded_dir") {
		t.Fatalf("escaped path: %d %q %q", code, out, errOut)
	}
	noGrant(t)
}

// The loaded-code directory set is one named list; this pins its contents
// (extending it is a deliberate code change, design doc 4 v0.7).
func TestLoadedCodeDirsPinned(t *testing.T) {
	want := []string{"node_modules", "vendor", "site-packages", "dist-packages", ".venv", "venv", "__pypackages__", "bower_components", "third_party"}
	if strings.Join(loadedCodeDirs, ",") != strings.Join(want, ",") {
		t.Fatalf("loadedCodeDirs = %q, want %q", loadedCodeDirs, want)
	}
}

func TestInLoadedDir(t *testing.T) {
	for _, d := range loadedCodeDirs {
		for _, p := range []string{d + "/x.png", "a/" + d + "/b/x.png", "a/b/" + strings.ToUpper(d) + "/x", d + "/x/y/z.gif"} {
			if !inLoadedDir(p) {
				t.Errorf("%s not matched", p)
			}
		}
		for _, p := range []string{"my_" + d + "_backup/x.png", d + "s/x.png", "x" + d + "/y", d + ".png", "a/" + d} {
			if inLoadedDir(p) {
				t.Errorf("%s matched", p)
			}
		}
	}
	for _, p := range []string{"Node_Modules/x.png", "VENDOR/x.png", "a/b/node_modules/c/x.png", "Site-Packages/a.png", "lib/.VENV/x"} {
		if !inLoadedDir(p) {
			t.Errorf("%s not matched", p)
		}
	}
	for _, p := range []string{"my_node_modules_backup/x.png", "vendors/x.png", "tests/vendored/x.dat", "node_modules.png", "venv.png", "thirdparty/x"} {
		if inLoadedDir(p) {
			t.Errorf("%s matched", p)
		}
	}
}
