// Package ui is the human-facing part of an approval: the safety checks that
// decide whether a diff may be shown at all, the exact diff display, and the
// confirmation prompt. `aigate approve` and `orch approve` both use it, so
// the screen and the refusals are identical.
package ui

import (
	"fmt"
	"path"
	"strings"

	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/textsafe"
)

func BinaryCount(p gate.Protected) int {
	n := 0
	for _, f := range p.Diff.Files {
		if f.Summarised() {
			n++
		}
	}
	return n
}

// Refusal reason codes (also the ledger's approval_refused "reason").
const (
	ReasonUnsafeBytes      = "unsafe_bytes"
	ReasonTextBecameBinary = "text_became_binary"
	ReasonBinaryBecameText = "binary_became_text"
	// A file binary at head that is not an allowlisted genuine binary type
	// (BinaryRefusal): executable (exec bit, "#!" start, ".bin" component),
	// source/config/text-named (the pre-allowlist rule, kept so old ledger
	// events keep their meaning), or any other type.
	ReasonBinaryExecutable = "binary_executable"
	ReasonBinarySource     = "binary_source_file"
	ReasonBinaryUnknown    = "binary_unknown_type"
	// A binary a runner can load as code (IsLoadableCode): never summarised,
	// reviewed out-of-band instead (fix wave 5 R1).
	ReasonBinaryLoadable = "binary_loadable_code"
	// An allowlisted type with a magic number whose head blob does not start
	// with it (fix wave 5 R2): the extension says image, the content may be
	// anything a runner loads by other means (Node require() runs it as JS).
	ReasonBinaryMagicMismatch = "binary_magic_mismatch"
	// A binary (added, modified or type-changed) under a loaded-code
	// directory (InLoadedDir): never summarised whatever its type (fix wave 6).
	ReasonBinaryInLoadedDir = "binary_in_loaded_dir"
	// The head tree has paths a case-/normalization-insensitive file system
	// maps onto one entry (gate.Protected.Collisions, fix wave 7).
	ReasonPathCaseCollision = "path_case_collision"
)

// at reports whether b holds m at offset off.
func at(b []byte, off int, m string) bool {
	return len(b) >= off+len(m) && string(b[off:off+len(m)]) == m
}

func anyPrefix(ms ...string) func([]byte) bool {
	return func(b []byte) bool {
		for _, m := range ms {
			if at(b, 0, m) {
				return true
			}
		}
		return false
	}
}

func riff(form string) func([]byte) bool {
	return func(b []byte) bool { return at(b, 0, "RIFF") && at(b, 8, form) }
}

func ftyp(b []byte) bool { return at(b, 4, "ftyp") }

// BinaryMagic maps each allowlisted extension that has a magic number to a
// check of the head blob's first bytes (gitx.HeadPrefixLen of them). This is
// a heuristic: a valid header followed by anything else still passes.
var BinaryMagic = map[string]func([]byte) bool{
	".png": anyPrefix("\x89PNG\r\n\x1a\n"), ".jpg": anyPrefix("\xff\xd8\xff"), ".jpeg": anyPrefix("\xff\xd8\xff"),
	".gif": anyPrefix("GIF87a", "GIF89a"), ".webp": riff("WEBP"), ".bmp": anyPrefix("BM"),
	".ico": anyPrefix("\x00\x00\x01\x00"), ".icns": anyPrefix("icns"), ".tif": anyPrefix("II*\x00", "MM\x00*"),
	".tiff": anyPrefix("II*\x00", "MM\x00*"), ".pdf": anyPrefix("%PDF-"), ".zip": anyPrefix("PK\x03\x04", "PK\x05\x06"),
	".gz": anyPrefix("\x1f\x8b"), ".tgz": anyPrefix("\x1f\x8b"), ".svgz": anyPrefix("\x1f\x8b"), ".bz2": anyPrefix("BZh"),
	".xz": anyPrefix("\xfd7zXZ\x00"), ".zst": anyPrefix("\x28\xb5\x2f\xfd"), ".7z": anyPrefix("7z\xbc\xaf\x27\x1c"),
	".woff": anyPrefix("wOFF"), ".woff2": anyPrefix("wOF2"), ".ttf": anyPrefix("\x00\x01\x00\x00", "true"), ".otf": anyPrefix("OTTO"),
	// EOT: EOTSize, FontDataSize, Version, Flags, PANOSE[10], Charset,
	// Italic, Weight (4+4+4+4+10+1+1+4 = 32 bytes), fsType (2), then the
	// magic number 0x504C ("LP") at offset 34.
	".eot":    func(b []byte) bool { return at(b, 34, "LP") },
	".sqlite": anyPrefix("SQLite format 3\x00"), ".sqlite3": anyPrefix("SQLite format 3\x00"),
	// ID3 tag, or an MPEG audio frame sync (11 set bits: FF Ex / FF Fx)
	".mp3": func(b []byte) bool { return at(b, 0, "ID3") || len(b) >= 2 && b[0] == 0xff && b[1]&0xe0 == 0xe0 },
	".mp4": ftyp, ".m4a": ftyp, ".mov": ftyp, ".heic": ftyp, ".avif": ftyp,
	".wav": riff("WAVE"), ".ogg": anyPrefix("OggS"), ".flac": anyPrefix("fLaC"), ".webm": anyPrefix("\x1a\x45\xdf\xa3"),
	".psd": anyPrefix("8BPS"), ".tar": func(b []byte) bool { return at(b, 257, "ustar") }, ".npy": anyPrefix("\x93NUMPY"),
	".npz": anyPrefix("PK"), ".parquet": anyPrefix("PAR1"),
}

// MagiclessExt are the allowlisted types without a reliable magic number
// (".db" may be SQLite or anything else): a summary only for a file that
// already existed at base, outside node_modules/ and vendor/.
var MagiclessExt = map[string]bool{".bin": true, ".dat": true, ".db": true, ".pb": true}

// LoadedCodeDirs are the directory names a runner loads code from (Node
// require(), Go vendoring, Python site-packages / virtualenvs / PEP 582,
// bower, vendored third-party trees). It is the ONE place that defines the
// set (fix wave 6, user decision 2026-11-05); extending it is a code change.
// Under such a directory approve never summarises an added, modified or
// type-changed binary (binary_in_loaded_dir): a format-valid file (GIF89a…,
// true…, BM…, ID3…, ftyp@4, ustar@257) can also be valid JS that require()
// runs, so extension + magic bytes cannot tell data from code there.
var LoadedCodeDirs = []string{"node_modules", "vendor", "site-packages", "dist-packages", ".venv", "venv", "__pypackages__", "bower_components", "third_party"}

// InLoadedDir reports whether a DIRECTORY component of p (any depth, whole
// component, case-insensitive) is one of LoadedCodeDirs. The file name itself
// does not count, and a component that only contains a name
// (my_node_modules_backup, vendors) does not match.
func InLoadedDir(p string) bool {
	segs := strings.Split(p, "/")
	for _, seg := range segs[:len(segs)-1] {
		for _, d := range LoadedCodeDirs {
			if strings.EqualFold(seg, d) {
				return true
			}
		}
	}
	return false
}

// BinaryAllowExt are the extensions (lower case, with the dot) of genuine
// binary types that approve may show as a one-line summary. This is an
// ALLOWLIST: everything else binary at head is refused.
var BinaryAllowExt = map[string]bool{}

// loadableExt (lower case, with the dot) are binaries a runner can load as
// code: native addons, extension modules, shared/static libraries, object
// files, executables, wasm, JVM classes and jars. None is allowlisted.
var loadableExt = map[string]bool{}

// TextLikeExt / TextLikeName (lower case) only CLASSIFY a refusal as
// binary_source_file; they decide nothing (none of them is allowlisted).
var TextLikeExt = map[string]bool{}
var TextLikeName = map[string]bool{}

func init() {
	for _, e := range strings.Fields("png jpg jpeg gif webp ico icns bmp tif tiff heic avif svgz pdf zip gz tgz bz2 xz zst 7z tar woff woff2 ttf otf eot bin dat db sqlite sqlite3 mp3 mp4 m4a mov wav ogg flac webm psd pb npy npz parquet") {
		BinaryAllowExt["."+e] = true
	}
	for _, e := range strings.Fields("node so pyd dylib dll exe wasm class jar a o obj lib bundle jnilib") {
		loadableExt["."+e] = true
	}
	for _, e := range strings.Fields(".ts .tsx .js .jsx .mjs .cjs .go .py .rb .java .kt .c .h .cc .cpp .cs .rs .php .sh .bash .zsh .ini .cfg .conf .toml .yml .yaml .json .xml .html .css .md .txt .lock .mod .sum .gradle .properties .env") {
		TextLikeExt[e] = true
	}
	for _, n := range strings.Fields("Makefile Dockerfile Jenkinsfile Rakefile .gitattributes .gitmodules .npmrc conftest") {
		TextLikeName[strings.ToLower(n)] = true
	}
}

// isTextLike reports whether a path names a source, config or text file
// (case-insensitive extension, or one of the known extension-less names).
func isTextLike(p string) bool {
	base := strings.ToLower(path.Base(p))
	return TextLikeName[base] || TextLikeExt[path.Ext(base)]
}

// IsLoadableCode reports whether a path names a binary a runner can load as
// code (case-insensitive): an extension in loadableExt, including CPython's
// tagged ".cpython-312-darwin.so" / ".abi3.so", or a versioned shared library
// ".so.<digits>[.<digits>...]" such as libx.so.1.
func IsLoadableCode(p string) bool {
	base := strings.ToLower(path.Base(p))
	stripped := base
	for e := path.Ext(stripped); len(e) > 1 && e != stripped && strings.Trim(e[1:], "0123456789") == ""; e = path.Ext(stripped) {
		stripped = strings.TrimSuffix(stripped, e)
	}
	if stripped != base {
		return path.Ext(stripped) == ".so"
	}
	return loadableExt[path.Ext(base)]
}

// HasBinDir reports whether a path component is ".bin" (node_modules/.bin,
// any **/.bin/**); a file named x.bin is not.
func HasBinDir(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, ".bin") {
			return true
		}
	}
	return false
}

// SummaryRefusal returns "" when f (binary at head, not deleted) is an
// allowlisted genuine binary that may be summarised, else the reason code and
// a short Korean explanation of the rule it breaks. Order: executable (the
// concrete evidence that it runs: exec bit, "#!", ".bin" component), then
// binary_in_loaded_dir (under a loaded-code directory nothing binary is
// summarised, so the remaining type reasons would only give advice that
// cannot succeed there), then loadable code, source/text name, not
// allowlisted, then the content rule: a magic-less type only as a modified
// existing file, any other type only when the head starts with its magic.
func SummaryRefusal(f gitx.FileChange) (reason, why string) {
	var exe []string
	if f.HeadMode != "100644" {
		exe = append(exe, "git 모드 "+f.HeadMode+"(일반 실행 불가 파일 100644 아님)")
	}
	if f.HeadShebang {
		exe = append(exe, "내용이 '#!'로 시작")
	}
	if HasBinDir(f.Path) {
		exe = append(exe, "'.bin' 경로 아래")
	}
	switch {
	case len(exe) > 0:
		why := "실행될 수 있는 파일입니다(" + strings.Join(exe, ", ") + ")"
		if InLoadedDir(f.Path) {
			why += ". 또 로드되는 코드 디렉터리 아래라 형식을 바꿔도 요약 승인하지 않습니다(저장소 밖에서 검토한 뒤 사람이 직접 커밋)"
		}
		return ReasonBinaryExecutable, why
	case InLoadedDir(f.Path):
		return ReasonBinaryInLoadedDir, "로드되는 코드 디렉터리(" + strings.Join(LoadedCodeDirs, "·") + ") 아래 있습니다"
	case IsLoadableCode(f.Path):
		return ReasonBinaryLoadable, "러너가 코드로 불러올 수 있는 바이너리 형식입니다(네이티브 애드온·확장 모듈·라이브러리·목적 파일·실행 파일·wasm·class·jar)"
	case isTextLike(f.Path):
		return ReasonBinarySource, "소스·설정·텍스트 파일 이름입니다"
	}
	ext := strings.ToLower(path.Ext(path.Base(f.Path)))
	switch {
	case !BinaryAllowExt[ext]:
		return ReasonBinaryUnknown, "확장자가 요약을 허용하는 바이너리 형식 목록에 없습니다"
	case MagiclessExt[ext]:
		if f.Status == "A" || !f.BaseBinary {
			return ReasonBinaryUnknown, "'" + ext + "' 처럼 형식 표시(매직 바이트)가 없는 바이너리는 base에 이미 있던 바이너리 파일을 바꾼 경우에만 요약을 허용합니다"
		}
	case !BinaryMagic[ext](f.HeadPrefix):
		return ReasonBinaryMagicMismatch, "확장자는 '" + ext + "' 인데 내용이 그 형식이 아닙니다(파일 앞부분의 형식 표시(매직 바이트)가 맞지 않음)"
	}
	return "", ""
}

// BinaryRefusal returns a description (escaped path, never raw bytes) and a
// reason code when a protected file must not be shown, checked before the
// generic byte scan: a text file that became binary (M/T), a binary file
// that became text (M/T: its NUL is on the base "-" side), or a file binary
// at head (added, modified or type-changed) that is not an allowlisted
// genuine binary (SummaryRefusal). Deleted files are always a summary.
//
// Precedence (first offending file in diff order wins; per file):
// binary_became_text → text_became_binary → binary_executable →
// binary_in_loaded_dir → binary_loadable_code → binary_source_file →
// binary_unknown_type → binary_magic_mismatch. cmdApprove runs this BEFORE
// the unsafe_bytes scan (UnsafeApproveText), so a binary refusal of any file
// is reported before an unsafe byte in a path or line.
func BinaryRefusal(p gate.Protected) (msg, reason string) {
	for _, f := range p.Diff.Files {
		if f.Status == "D" || f.Status == "A" && !f.HeadBinary {
			continue
		}
		if f.Status != "A" && f.BaseBinary && !f.HeadBinary && f.HeadMode == "120000" {
			return fmt.Sprintf("보호 파일 '%s' 가 바이너리에서 심볼릭 링크로 바뀌었습니다(base는 바이너리, head는 심볼릭 링크/텍스트). base의 NUL 바이트가 diff의 '-' 줄에 남아 이 diff는 화면에 그대로 보일 수 없습니다", textsafe.Escape(f.Path)), ReasonBinaryBecameText
		}
		if f.Status != "A" && f.BaseBinary && !f.HeadBinary {
			return fmt.Sprintf("보호 파일 '%s' 가 바이너리에서 텍스트로 바뀌었습니다(base는 바이너리, head는 텍스트). base의 NUL 바이트가 diff의 '-' 줄에 남아 이 diff는 화면에 그대로 보일 수 없습니다", textsafe.Escape(f.Path)), ReasonBinaryBecameText
		}
		if !f.HeadBinary {
			continue
		}
		where := "NUL 바이트"
		if f.HeadNULLine > 0 {
			where = fmt.Sprintf("head %d번째 줄에 \\x00(NUL) 바이트", f.HeadNULLine)
		}
		if f.Status != "A" && !f.BaseBinary {
			return fmt.Sprintf("보호 파일 '%s' 가 텍스트에서 바이너리로 바뀌었습니다(base는 텍스트, %s)", textsafe.Escape(f.Path), where), ReasonTextBecameBinary
		}
		if r, why := SummaryRefusal(f); r != "" {
			return fmt.Sprintf("보호 파일 '%s' 는 head에서 바이너리(%s)인데 %s", textsafe.Escape(f.Path), where, why), r
		}
	}
	return "", ""
}

// UnsafeApproveText returns a description (with escapes only, never the raw
// bytes) of the first unsafe byte in a path or line approve would print, and
// the reason code, or "" when everything is safe to show.
func UnsafeApproveText(p gate.Protected, display string) (string, string) {
	lists := []struct {
		what  string
		paths []string
	}{{"변경 파일", p.Diff.TouchedFiles}, {"변경·삭제 파일", p.Diff.Changed()}, {"새 보호 파일", p.AddedGuarded}, {"새 파일", p.Diff.AddedFiles}, {"심볼릭 링크 대상", shownLinkTargets(p)}}
	for _, l := range lists {
		for i, f := range l.paths {
			// a path is printed on one line: a newline in it is unsafe too
			if fd, bad := textsafe.FirstUnsafe(f); bad {
				return fmt.Sprintf("%s 목록 %d번째 경로 '%s' 의 %d번째 바이트에 %s", l.what, i+1, textsafe.Escape(f), fd.Offset+1, fd.Desc), ReasonUnsafeBytes
			} else if j := strings.IndexByte(f, '\n'); j >= 0 {
				return fmt.Sprintf("%s 목록 %d번째 경로 '%s' 의 %d번째 바이트에 \\x0a(줄바꿈)", l.what, i+1, textsafe.Escape(f), j+1), ReasonUnsafeBytes
			}
		}
	}
	fd, bad := textsafe.FirstUnsafe(display)
	// A file whose last byte is a lone CR shows as "+x\r" + "\n\ No newline
	// at end of file": the "\n" is git's, so that CR is lone in the file.
	if k := strings.Index(display, "\r\n\\ "); k >= 0 && (!bad || k < fd.Offset) {
		fd, bad = textsafe.Finding{Offset: k, Line: strings.Count(display[:k], "\n") + 1, Desc: `\x0d`}, true
	}
	if bad {
		file := "(파일 머리 앞)"
		lines := strings.Split(display[:fd.Offset], "\n")
		for k := len(lines) - 1; k >= 0; k-- {
			if strings.HasPrefix(lines[k], "diff --git ") || strings.HasPrefix(lines[k], "A ") {
				file = textsafe.Escape(lines[k])
				break
			}
		}
		return fmt.Sprintf("승인할 diff 텍스트 %d번째 줄(%s)에 %s", fd.Line, file, fd.Desc), ReasonUnsafeBytes
	}
	return "", ""
}

// shownLinkTargets returns the targets approve prints next to a symlink in
// the changed / added guarded lists, in that order.
func shownLinkTargets(p gate.Protected) []string {
	links := p.Diff.HeadSymlinks()
	var out []string
	for _, l := range [][]string{p.Diff.Changed(), p.AddedGuarded} {
		for _, f := range l {
			if t, ok := links[f]; ok {
				out = append(out, t)
			}
		}
	}
	return out
}
