package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
)

// lastRefusal returns the payload of the last approval_refused event of repo.
func lastRefusal(t *testing.T, repo string) map[string]any {
	t.Helper()
	id, _, err := gate.RepoID(repo)
	if err != nil {
		t.Fatal(err)
	}
	home, err := gate.Home("")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := ledger.Events(gate.LedgerPath(home, id))
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == "approval_refused" {
			return evs[i].Payload
		}
	}
	t.Fatalf("no approval_refused event in %+v", evs)
	return nil
}

// noGrant fails when approvals.json holds anything.
func noGrant(t *testing.T) {
	t.Helper()
	home, err := gate.Home("")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(gate.ApprovalsPath(home))
	if err == nil && strings.Contains(string(b), `"hash"`) {
		t.Fatalf("a refused approve granted something: %s", b)
	}
}

// writeMode writes a file and sets its mode (0755 = git mode 100755).
func writeMode(t *testing.T, repo, name, content string, mode os.FileMode) {
	t.Helper()
	write(t, repo, name, content)
	if err := os.Chmod(filepath.Join(repo, name), mode); err != nil {
		t.Fatal(err)
	}
}

// FIX WAVE 4 item A: a one-line binary summary is allowed only for allowlisted
// genuine binary types (extension in the allowlist, mode 100644, no "#!"
// start, no ".bin" path component). Every other file binary at head is refused
// before anything is printed, with a reason code, and nothing is granted.
func TestApproveAllowlistRefusesGuardedBinaries(t *testing.T) {
	cases := []struct {
		name, path, base, head string
		exec                   bool
		reason                 string
	}{
		// the four reported attacks
		{"node_modules/.bin runner with shebang", "node_modules/.bin/vitest", "", "#!/usr/bin/env node\n\x00console.log('all tests passed')\n", true, "binary_executable"},
		{"added .mts", "tests/helper.mts", "", "export const x = 1\x00\n", false, "binary_unknown_type"},
		{"executable vendor script", "vendor/sub/run", "", "#!/bin/sh\n\x00echo ok\n", true, "binary_executable"},
		{"extensionless, not executable, no shebang", "vendor/sub/tool", "", "data\x00\n", false, "binary_unknown_type"},
		// rule edges
		{"non-allowlisted extension", "tests/data.dat2", "", "d\x00\n", false, "binary_unknown_type"},
		{"allowlisted .bin but executable", "tests/tool.bin", "", "\x7fELF\x00\x01\n", true, "binary_executable"},
		{"allowlisted .png starting with #!", "tests/pic.png", "", "#!/bin/sh\n\x00\n", false, "binary_executable"},
		{"allowlisted ext under a .bin component", "tests/x/.bin/thing.png", "", "\x89PNG\x00\n", false, "binary_executable"},
		{"old text-like rule (.TS)", "tests/b.test.TS", "", "\x00it('y')\n", false, "binary_source_file"},
		{"old text-like name (Makefile)", "tests/Makefile", "", "all:\x00\n", false, "binary_source_file"},
		{".cts", "tests/a.cts", "", "x\x00\n", false, "binary_unknown_type"},
		{".vue", "tests/a.vue", "", "x\x00\n", false, "binary_unknown_type"},
		{".svelte", "tests/a.svelte", "", "x\x00\n", false, "binary_unknown_type"},
		{".snap", "tests/__snapshots__/a.snap", "", "x\x00\n", false, "binary_unknown_type"},
		{".sql", "tests/a.sql", "", "x\x00\n", false, "binary_unknown_type"},
		{"modified .mts binary on both sides", "tests/m.mts", "a\x00\n", "b\x00\n", false, "binary_unknown_type"},
		{"modified .png that became executable", "tests/m.png", "\x89PNG\x00\n", "\x89PNG\x00\x01\n", true, "binary_executable"},
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
			commit(t, repo, "binary")
			if c.exec && !strings.HasPrefix(git(t, repo, "ls-files", "-s", c.path), "100755") {
				t.Fatalf("fixture not executable in git: %s", git(t, repo, "ls-files", "-s", c.path))
			}
			code, out, errOut := approveTTY(t, repo, base, "yes\n")
			if code != 64 || out != "" || rawUnsafe(errOut) || strings.Contains(errOut, "binary file changed") {
				t.Fatalf("want refusal: %d stdout=%q stderr=%q", code, out, errOut)
			}
			if !strings.Contains(errOut, c.path) || !strings.Contains(errOut, c.reason) {
				t.Fatalf("stderr should name %s and %s: %q", c.path, c.reason, errOut)
			}
			if strings.Contains(errOut, "all tests passed") || strings.Contains(errOut, "echo ok") {
				t.Fatalf("file content printed: %q", errOut)
			}
			if p := lastRefusal(t, repo); p["reason"] != c.reason {
				t.Fatalf("ledger reason %v, want %s", p["reason"], c.reason)
			}
			noGrant(t)
		})
	}
}

// Allowlisted genuine binaries keep the summary line and stay approvable: an
// added and a modified (binary on both sides) .png, an added .bin, and a
// deleted binary of any type. Under the guarded node_modules/** the approval
// really takes effect.
func TestApproveAllowlistedBinarySummaries(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "node_modules/pkg/old.png", "\x89PNG\x00old\n")
	write(t, repo, "node_modules/pkg/gone", "#!/bin/sh\n\x00gone\n") // deleted: always a summary
	base = commit(t, repo, "base2")
	write(t, repo, "node_modules/pkg/old.png", "\x89PNG\x00new\n")
	write(t, repo, "node_modules/pkg/new.png", "\x89PNG\x00\x1b\n")
	write(t, repo, "node_modules/pkg/data.bin", "\x00\x01\x02\n")
	if err := os.Remove(filepath.Join(repo, "node_modules/pkg/gone")); err != nil {
		t.Fatal(err)
	}
	commit(t, repo, "fixtures")
	if c, _, _ := run("run", "--repo", repo, "--base", base); c != 3 {
		t.Fatalf("guarded binary changes must need approval: run exit %d", c)
	}
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || rawUnsafe(out) || !strings.Contains(out, "approved.") {
		t.Fatalf("approve: %d %q %q", code, out, errOut)
	}
	for _, p := range []string{"old.png", "new.png", "data.bin", "gone"} {
		if !strings.Contains(out, "binary file changed: node_modules/pkg/"+p+", ") {
			t.Errorf("no summary for %s in %q", p, out)
		}
	}
	if !strings.Contains(out, "(4 binary file(s)") {
		t.Errorf("binary count: %q", out)
	}
	if c, out, e := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("after approval: %d %s %s", c, out, e)
	}
}

// FIX WAVE 4 item B: binary→text (M/T) gets its own refusal, checked before
// the generic byte scan: the NUL is on the base "-" side, so the message says
// to split the change into a deletion plus an addition under a new name.
func TestApproveBinaryBecameTextDedicatedRefusal(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/data.bin", "a\x00b\n")
	base = commit(t, repo, "base2")
	write(t, repo, "tests/data.bin", "now text\n")
	commit(t, repo, "b2t")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 64 || out != "" || rawUnsafe(errOut) || strings.Contains(errOut, "binary file changed") {
		t.Fatalf("binary→text: %d %q %q", code, out, errOut)
	}
	for _, s := range []string{"tests/data.bin", "binary_became_text", "base", "바이너리", "텍스트", "삭제", "새 이름"} {
		if !strings.Contains(errOut, s) {
			t.Errorf("stderr lacks %q: %q", s, errOut)
		}
	}
	if strings.Contains(errOut, "새 커밋") || strings.Contains(errOut, "없앤 커밋") {
		t.Errorf("must not give the generic remove-the-byte advice: %q", errOut)
	}
	if p := lastRefusal(t, repo); p["reason"] != "binary_became_text" {
		t.Fatalf("ledger reason %v", p["reason"])
	}
	noGrant(t)
}

// The pre-allowlist denylist is redundant for safety: no text-like extension
// or name is allowlisted, so the allowlist alone refuses every case it
// covered; it only keeps classifying them as binary_source_file.
func TestTextLikeDenylistCoveredByAllowlist(t *testing.T) {
	for e := range textLikeExt {
		if binaryAllowExt[e] {
			t.Errorf("text-like extension %s is allowlisted", e)
		}
		f := gitx.FileChange{Path: "tests/x" + e, Status: "A", HeadBinary: true, HeadMode: "100644"}
		if r, _ := summaryRefusal(f); r != reasonBinarySource {
			t.Errorf("%s: reason %q", f.Path, r)
		}
	}
	for n := range textLikeName {
		f := gitx.FileChange{Path: "tests/" + n, Status: "A", HeadBinary: true, HeadMode: "100644"}
		if r, _ := summaryRefusal(f); r != reasonBinarySource {
			t.Errorf("%s: reason %q", f.Path, r)
		}
	}
	for _, ok := range []string{"a.png", "A.PNG", "x/y.tar", "golden.bin", "bin/font.woff2"} {
		if r, _ := summaryRefusal(gitx.FileChange{Path: ok, Status: "A", HeadBinary: true, HeadMode: "100644"}); r != "" {
			t.Errorf("%s refused: %s", ok, r)
		}
	}
	// fix wave 5 R1: loadable code is refused even when it is not executable
	if r, _ := summaryRefusal(gitx.FileChange{Path: "bin/tool.so", Status: "A", HeadBinary: true, HeadMode: "100644"}); r != reasonBinaryLoadable {
		t.Errorf("bin/tool.so: reason %q", r)
	}
	for p, mode := range map[string]string{"a.png": "100755", "b.png": "120000", "node_modules/.bin/c.png": "100644", ".bin": "100644"} {
		if r, _ := summaryRefusal(gitx.FileChange{Path: p, Status: "A", HeadBinary: true, HeadMode: mode}); r != reasonBinaryExecutable {
			t.Errorf("%s (%s): reason %q", p, mode, r)
		}
	}
}

// refusedApprove runs approve and checks a pre-display refusal with reason
// (stderr names path and reason, no stdout, no raw bytes, ledger reason,
// nothing granted); it returns stderr.
func refusedApprove(t *testing.T, repo, base, p, reason string) string {
	t.Helper()
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 64 || out != "" || rawUnsafe(errOut) || strings.Contains(errOut, "binary file changed") {
		t.Fatalf("want refusal: %d stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, p) || !strings.Contains(errOut, reason) {
		t.Fatalf("stderr should name %s and %s: %q", p, reason, errOut)
	}
	if lr := lastRefusal(t, repo); lr["reason"] != reason {
		t.Fatalf("ledger reason %v, want %s", lr["reason"], reason)
	}
	noGrant(t)
	return errOut
}

// FIX WAVE 5 R1: a binary the runner can load as code (native addon,
// extension module, shared/static library, object file, executable, wasm,
// class, jar) is never approvable from a one-line summary, added, modified
// or type-changed, whatever its case; the human reviews it out-of-band and
// commits it themselves. There is no override flag.
func TestApproveRefusesLoadableCode(t *testing.T) {
	elf := "\x7fELF\x02\x01\x01\x00payload\n"
	cases := []struct{ path, base string }{
		{"vendor/pkg/mod.cpython-312-darwin.so", ""},
		{"vendor/pkg/mod.abi3.so", ""},
		{"node_modules/addon/build/Release/addon.node", ""},
		{"tests/lib/libx.so.1", ""},
		{"tests/lib/libx.so.1.2.3", ""},
		{"tests/lib/X.DLL", ""},
		{"tests/lib/x.dylib", ""},
		{"tests/lib/x.pyd", ""},
		{"tests/lib/x.exe", ""},
		{"tests/lib/x.wasm", ""},
		{"tests/lib/x.class", ""},
		{"tests/lib/x.jar", ""},
		{"tests/lib/x.a", ""},
		{"tests/lib/x.o", ""},
		{"tests/lib/x.obj", ""},
		{"tests/lib/x.lib", ""},
		{"tests/lib/x.bundle", ""},
		{"tests/lib/x.jnilib", ""},
		{"tests/lib/x.SO", ""},
		{"vendor/pkg/old.so", elf}, // modified, binary on both sides
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			repo, base := newRepo(t)
			if c.base != "" {
				write(t, repo, c.path, c.base)
				base = commit(t, repo, "base2")
			}
			write(t, repo, c.path, elf+"v2\x00")
			commit(t, repo, "loadable")
			errOut := refusedApprove(t, repo, base, c.path, "binary_loadable_code")
			for _, s := range []string{"코드로", "요약", "직접", "커밋", "base", "우회 플래그"} {
				if !strings.Contains(errOut, s) {
					t.Errorf("stderr lacks %q: %q", s, errOut)
				}
			}
			if strings.Contains(errOut, "payload") {
				t.Errorf("content printed: %q", errOut)
			}
		})
	}
}

// Deleting a loadable binary is still a summary line and approvable.
func TestApproveDeletedLoadableCodeSummarised(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "vendor/pkg/mod.cpython-312-darwin.so", "\x7fELF\x00\n")
	write(t, repo, "tests/lib/libx.so.1", "\x7fELF\x00\n")
	base = commit(t, repo, "base2")
	for _, p := range []string{"vendor/pkg/mod.cpython-312-darwin.so", "tests/lib/libx.so.1"} {
		if err := os.Remove(filepath.Join(repo, p)); err != nil {
			t.Fatal(err)
		}
	}
	commit(t, repo, "delete")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 0 || !strings.Contains(out, "binary file changed: vendor/pkg/mod.cpython-312-darwin.so, 0 bytes") || !strings.Contains(out, "binary file changed: tests/lib/libx.so.1, 0 bytes") {
		t.Fatalf("deleted loadable: %d %q %q", code, out, errOut)
	}
}

func TestIsLoadableCode(t *testing.T) {
	for _, p := range []string{"a.so", "a.So", "libx.so.1", "libx.so.10.2", "m.cpython-312-darwin.so", "a.node", "a.pyd", "a.dylib", "a.dll", "a.exe", "a.wasm", "a.class", "a.jar", "a.a", "a.o", "a.obj", "a.lib", "a.bundle", "a.jnilib"} {
		if !isLoadableCode("x/" + p) {
			t.Errorf("%s not loadable", p)
		}
	}
	for _, p := range []string{"a.png", "so", "a.so.x", "a.so.1x", "a.1", "a.bin", "x.solib", "a.sol"} {
		if isLoadableCode("x/" + p) {
			t.Errorf("%s loadable", p)
		}
	}
	for e := range binaryAllowExt {
		if isLoadableCode("x/a" + e) {
			t.Errorf("loadable %s is still allowlisted", e)
		}
	}
}
