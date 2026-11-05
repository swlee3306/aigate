package main

import (
	"os"
	"path"
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
// added and a modified (binary on both sides) .png with the real PNG magic, a
// modified existing .bin outside node_modules/vendor (fix wave 5 R2: an added
// magic-less .bin, or one under node_modules, is refused), and a deleted
// binary of any type. Under the guarded node_modules/** the approval really
// takes effect.
func TestApproveAllowlistedBinarySummaries(t *testing.T) {
	const png = "\x89PNG\r\n\x1a\n"
	repo, base := newRepo(t)
	write(t, repo, "node_modules/pkg/old.png", png+"\x00old\n")
	write(t, repo, "node_modules/pkg/gone", "#!/bin/sh\n\x00gone\n") // deleted: always a summary
	write(t, repo, "tests/data.bin", "\x00\x01\n")
	base = commit(t, repo, "base2")
	write(t, repo, "node_modules/pkg/old.png", png+"\x00new\n")
	write(t, repo, "node_modules/pkg/new.png", png+"\x00\x1b\n")
	write(t, repo, "tests/data.bin", "\x00\x01\x02\n")
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
	for _, p := range []string{"node_modules/pkg/old.png", "node_modules/pkg/new.png", "tests/data.bin", "node_modules/pkg/gone"} {
		if !strings.Contains(out, "binary file changed: "+p+", ") {
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
	// fix wave 5 R2: magic-numbered types carry their magic; the magic-less
	// .bin is a modified existing file (binary on both sides)
	for ok, head := range map[string]string{"a.png": validMagic[".png"], "A.PNG": validMagic[".png"], "x/y.tar": validMagic[".tar"], "bin/font.woff2": validMagic[".woff2"]} {
		if r, _ := summaryRefusal(gitx.FileChange{Path: ok, Status: "A", HeadBinary: true, HeadMode: "100644", HeadPrefix: []byte(head)}); r != "" {
			t.Errorf("%s refused: %s", ok, r)
		}
	}
	if r, _ := summaryRefusal(gitx.FileChange{Path: "golden.bin", Status: "M", BaseBinary: true, HeadBinary: true, HeadMode: "100644"}); r != "" {
		t.Errorf("golden.bin refused: %s", r)
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

// validMagic is a minimal head that carries each magic-numbered allowlisted
// type's magic bytes (plus a NUL so the file is binary).
var validMagic = map[string]string{
	".png": "\x89PNG\r\n\x1a\n\x00", ".jpg": "\xff\xd8\xff\xe0\x00", ".jpeg": "\xff\xd8\xff\xdb\x00",
	".gif": "GIF89a\x00", ".webp": "RIFF\x10\x00\x00\x00WEBPVP8 ", ".bmp": "BM\x00\x00",
	".ico": "\x00\x00\x01\x00\x01", ".icns": "icns\x00\x00", ".tif": "II*\x00\x08", ".tiff": "MM\x00*\x00",
	".pdf": "%PDF-1.7\n\x00", ".zip": "PK\x03\x04\x14\x00", ".gz": "\x1f\x8b\x08\x00", ".tgz": "\x1f\x8b\x08\x00",
	".bz2": "BZh91AY\x00", ".xz": "\xfd7zXZ\x00\x00", ".zst": "\x28\xb5\x2f\xfd\x00", ".7z": "7z\xbc\xaf\x27\x1c\x00",
	".woff": "wOFF\x00\x01", ".woff2": "wOF2\x00\x01", ".ttf": "\x00\x01\x00\x00\x00", ".otf": "OTTO\x00",
	".eot": "\x00\x01\x00\x00" + strings.Repeat("\x00", 30) + "LP\x00", ".sqlite": "SQLite format 3\x00", ".sqlite3": "SQLite format 3\x00",
	".mp3": "ID3\x04\x00", ".mp4": "\x00\x00\x00\x18ftypmp42", ".m4a": "\x00\x00\x00\x20ftypM4A ", ".mov": "\x00\x00\x00\x14ftypqt  ",
	".heic": "\x00\x00\x00\x18ftypheic", ".avif": "\x00\x00\x00\x1cftypavif", ".wav": "RIFF\x24\x00\x00\x00WAVEfmt ",
	".ogg": "OggS\x00\x02", ".flac": "fLaC\x00\x00", ".webm": "\x1a\x45\xdf\xa3\x00", ".psd": "8BPS\x00\x01",
	".tar": strings.Repeat("\x00", 257) + "ustar\x0000", ".npy": "\x93NUMPY\x01\x00", ".npz": "PK\x03\x04\x00",
	".parquet": "PAR1\x00\x00", ".svgz": "\x1f\x8b\x08\x00",
}

// magicless are the allowlisted types without a magic number.
var magicless = []string{".bin", ".dat", ".db", ".pb"}

// Every allowlisted type is either magic-checked or magic-less; every
// magic-checked type accepts its genuine header (also in upper case) and
// refuses other content, never a summary from the extension alone.
func TestAllowlistMagicTable(t *testing.T) {
	for e := range binaryAllowExt {
		if _, ok := validMagic[e]; !ok && !hasString(magicless, e) {
			t.Errorf("allowlisted %s has neither a magic check nor the magic-less rule", e)
		}
	}
	for e, head := range validMagic {
		for _, p := range []string{"tests/x" + e, "tests/X" + strings.ToUpper(e)} {
			f := gitx.FileChange{Path: p, Status: "A", HeadBinary: true, HeadMode: "100644", HeadPrefix: []byte(head)}
			if r, why := summaryRefusal(f); r != "" {
				t.Errorf("%s with its magic refused: %s %s", p, r, why)
			}
		}
		for _, bad := range []string{"", "\x00", "module.exports = 1\x00", "\x89PNX\r\n\x1a\n"} {
			f := gitx.FileChange{Path: "tests/x" + e, Status: "A", HeadBinary: true, HeadMode: "100644", HeadPrefix: []byte(bad)}
			if r, why := summaryRefusal(f); r != reasonBinaryMagicMismatch || !strings.Contains(why, e) {
				t.Errorf("%s with %q: %q %q", e, bad, r, why)
			}
		}
	}
	// the alternatives the ruling names
	for p, head := range map[string]string{
		"a.gif": "GIF87a\x00", "a.zip": "PK\x05\x06\x00", "a.ttf": "true\x00", "a.mp3": "\xff\xfb\x90\x00", "a.MP3": "\xff\xe3\x00",
	} {
		if r, _ := summaryRefusal(gitx.FileChange{Path: p, Status: "A", HeadBinary: true, HeadMode: "100644", HeadPrefix: []byte(head)}); r != "" {
			t.Errorf("%s: %s", p, r)
		}
	}
	for p, head := range map[string]string{
		"a.webp": "RIFF\x00\x00\x00\x00WAVE", "a.wav": "RIFF\x00\x00\x00\x00WEBP", "a.mp3": "\xff\x1b\x00", "a.tar": "ustar\x00",
		"a.mp4": "ftypisom\x00", "a.eot": "LP\x00",
	} {
		if r, _ := summaryRefusal(gitx.FileChange{Path: p, Status: "A", HeadBinary: true, HeadMode: "100644", HeadPrefix: []byte(head)}); r != reasonBinaryMagicMismatch {
			t.Errorf("%s with %q: %q", p, head, r)
		}
	}
}

func hasString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Magic-less binaries (.bin .dat .db .pb): a summary only for a file that
// already existed at base (M/T, binary on both sides) outside any
// node_modules/ or vendor/ component (case-insensitive).
func TestMaglessBinaryRule(t *testing.T) {
	mod := func(p string) gitx.FileChange {
		return gitx.FileChange{Path: p, Status: "M", BaseBinary: true, HeadBinary: true, HeadMode: "100644", HeadPrefix: []byte("\x00")}
	}
	for _, e := range magicless {
		for _, p := range []string{"tests/golden" + e, "data/x" + strings.ToUpper(e), "tests/vendored/x" + e, "my_node_modules/x" + e} {
			if r, why := summaryRefusal(mod(p)); r != "" {
				t.Errorf("modified %s refused: %s %s", p, r, why)
			}
			f := mod(p)
			f.Status = "T"
			if r, _ := summaryRefusal(f); r != "" {
				t.Errorf("type-changed %s refused: %s", p, r)
			}
		}
		for _, p := range []string{"node_modules/pkg/x" + e, "a/Node_Modules/x" + e, "vendor/x" + e, "src/VENDOR/sub/x" + e} {
			if r, _ := summaryRefusal(mod(p)); r != reasonBinaryUnknown {
				t.Errorf("modified %s under a dependency dir: %q", p, r)
			}
		}
		added := gitx.FileChange{Path: "tests/new" + e, Status: "A", HeadBinary: true, HeadMode: "100644", HeadPrefix: []byte("\x00")}
		if r, why := summaryRefusal(added); r != reasonBinaryUnknown || !strings.Contains(why, e) {
			t.Errorf("added %s: %q %q", added.Path, r, why)
		}
	}
}

// FIX WAVE 5 R2, the reproduced attack: package.json "main" points at
// logo.png holding JS with a NUL (Node require() runs unknown extensions as
// JS). The content is not a PNG, so approve refuses before printing, names
// the extension and never prints the content.
func TestApproveMagicMismatchRefused(t *testing.T) {
	js := "module.exports = () => 'all tests passed'\n//\x00\n"
	cases := []struct{ name, path, base, head string }{
		{"node main → logo.png with JS", "node_modules/helper/logo.png", "", js},
		{"pdf that is JS", "tests/fixtures/doc.pdf", "", js},
		{"zip that is JS", "vendor/pkg/a.zip", "", js},
		{"existing png turned into JS (both sides binary)", "node_modules/helper/icon.png", "\x89PNG\r\n\x1a\n\x00", js},
		{"tiny png", "tests/a.png", "", "\x00"},
		{"jpg with png magic", "tests/a.jpg", "", "\x89PNG\r\n\x1a\n\x00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, base := newRepo(t)
			if c.base != "" {
				write(t, repo, c.path, c.base)
				base = commit(t, repo, "base2")
			}
			if c.name == "node main → logo.png with JS" {
				write(t, repo, "node_modules/helper/package.json", `{"main":"logo.png"}`+"\n")
			}
			write(t, repo, c.path, c.head)
			commit(t, repo, "mismatch")
			errOut := refusedApprove(t, repo, base, c.path, "binary_magic_mismatch")
			ext := path.Ext(c.path)
			if !strings.Contains(errOut, "'"+ext+"'") || !strings.Contains(errOut, "형식이 아닙니다") {
				t.Errorf("stderr should name %s and say the content is not that format: %q", ext, errOut)
			}
			if strings.Contains(errOut, "module.exports") || strings.Contains(errOut, "all tests passed") {
				t.Errorf("content printed: %q", errOut)
			}
		})
	}
}

// Magic-less binaries through the CLI: an added .bin is refused, a modified
// one under node_modules is refused, a modified one under tests/ is a summary.
func TestApproveMaglessBinaries(t *testing.T) {
	t.Run("added .bin", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "tests/new.bin", "\x00\x01\x02\n")
		commit(t, repo, "add")
		refusedApprove(t, repo, base, "tests/new.bin", "binary_unknown_type")
	})
	t.Run("modified .dat under node_modules", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "node_modules/pkg/x.dat", "a\x00\n")
		base = commit(t, repo, "base2")
		write(t, repo, "node_modules/pkg/x.dat", "b\x00\n")
		commit(t, repo, "mod")
		refusedApprove(t, repo, base, "node_modules/pkg/x.dat", "binary_unknown_type")
	})
	t.Run("modified .db under Vendor", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "tests/Vendor/x.db", "a\x00\n")
		base = commit(t, repo, "base2")
		write(t, repo, "tests/Vendor/x.db", "b\x00\n")
		commit(t, repo, "mod")
		refusedApprove(t, repo, base, "tests/Vendor/x.db", "binary_unknown_type")
	})
	t.Run("modified .pb under tests", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "tests/model.pb", "a\x00\n")
		base = commit(t, repo, "base2")
		write(t, repo, "tests/model.pb", "bb\x00\n")
		commit(t, repo, "mod")
		if code, out, e := approveTTY(t, repo, base, "yes\n"); code != 0 || !strings.Contains(out, "binary file changed: tests/model.pb, 4 bytes (base 3 bytes)") {
			t.Fatalf("modified .pb: %d %q %q", code, out, e)
		}
	})
}

func symlink(t *testing.T, repo, target, name string) {
	t.Helper()
	p := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// FIX WAVE 5 R3: a symlink in the added/changed guarded lists is marked with
// its target, plus one note that the target's content is not part of the
// reviewed diff; the run's Markdown report marks it the same way.
func TestApproveMarksSymlinks(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/old.txt", "x\n")
	base = commit(t, repo, "base2")
	symlink(t, repo, "../../src/evil.js", "node_modules/helper/index.js")
	symlink(t, repo, "../ok.flag", "tests/old.txt") // existing file became a symlink (T)
	commit(t, repo, "links")
	code, out, errOut := run("run", "--repo", repo, "--base", base)
	if code != 3 || !strings.Contains(out, "node_modules/helper/index.js (symlink → ../../src/evil.js)") || !strings.Contains(out, "tests/old.txt (symlink → ../ok.flag)") || !strings.Contains(out, "링크 대상") {
		t.Fatalf("run report should mark symlinks: %d %q %q", code, out, errOut)
	}
	code, out, errOut = approveTTY(t, repo, base, "yes\n")
	if code != 0 || rawUnsafe(out) {
		t.Fatalf("approve: %d %q %q", code, out, errOut)
	}
	for _, s := range []string{
		"  + node_modules/helper/index.js (symlink → ../../src/evil.js)\n",
		"  ! tests/old.txt (symlink → ../ok.flag)\n",
		"note: a symlink's target content is not part of the reviewed diff",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in %q", s, out)
		}
	}
	if strings.Count(out, "note: a symlink's target") != 1 {
		t.Errorf("the note must be printed once: %q", out)
	}
}

// A symlink target is checked like a path: a newline or control byte in it
// is refused before anything is printed (a newline is not caught by the diff
// scan, where it is just two "+" lines).
func TestApproveRefusesUnsafeSymlinkTarget(t *testing.T) {
	for name, target := range map[string]string{"newline": "a\n  + fake/path", "esc": "a\x1b[2Kb", "cr": "a\rb"} {
		t.Run(name, func(t *testing.T) {
			repo, base := newRepo(t)
			symlink(t, repo, target, "node_modules/helper/index.js")
			commit(t, repo, "link")
			code, out, errOut := approveTTY(t, repo, base, "yes\n")
			if code != 64 || out != "" || rawUnsafe(errOut) || !strings.Contains(errOut, "unsafe_bytes") {
				t.Fatalf("want refusal: %d %q %q", code, out, errOut)
			}
			if p := lastRefusal(t, repo); p["reason"] != "unsafe_bytes" {
				t.Fatalf("ledger reason %v", p["reason"])
			}
			noGrant(t)
		})
	}
}

// binary→symlink: the binary_became_text message says the head is a
// symlink, not plain text.
func TestApproveBinaryBecameSymlinkMessage(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/data.bin", "a\x00b\n")
	base = commit(t, repo, "base2")
	symlink(t, repo, "../ok.flag", "tests/data.bin")
	commit(t, repo, "b2l")
	code, out, errOut := approveTTY(t, repo, base, "yes\n")
	if code != 64 || out != "" || !strings.Contains(errOut, "binary_became_text") || !strings.Contains(errOut, "head는 심볼릭 링크") {
		t.Fatalf("binary→symlink: %d %q %q", code, out, errOut)
	}
}

// The run report escapes a symlink target like a path (a newline shows as
// \x0a, never as a second line or a folded space).
func TestRunReportEscapesSymlinkTarget(t *testing.T) {
	repo, base := newRepo(t)
	symlink(t, repo, "a\nb\x1b", "node_modules/helper/index.js")
	commit(t, repo, "link")
	code, out, _ := run("run", "--repo", repo, "--base", base)
	if code != 3 || rawUnsafe(out) || !strings.Contains(out, `node_modules/helper/index.js (symlink → a\x0ab\x1b)`) {
		t.Fatalf("report: %d %q", code, out)
	}
}
