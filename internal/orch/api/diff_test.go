package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/orch/store"
)

const diffYAML = "version: 1\nprotected:\n  - \"tests/**\"\n  - \".aigate.yml\"\nchecks:\n" +
	"  - id: unit\n    adapter: exit-code\n    command: [\"sh\", \"-c\", \"true\"]\n    timeout: 20s\n"

func putFile(t *testing.T, repo, name, content string) {
	t.Helper()
	p := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// diffTask commits base (with tests/spec.txt) and a head that applies change,
// then stores a task for base..head. Returns the task id, base and head.
func diffTask(t *testing.T, e *env, change func()) (id int64, base, head string) {
	t.Helper()
	putFile(t, e.repo, ".aigate.yml", diffYAML)
	putFile(t, e.repo, "tests/spec.txt", "line1\nline2\n")
	putFile(t, e.repo, "src/a.txt", "a\n")
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "base")
	base = git(t, e.repo, "rev-parse", "HEAD")
	change()
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "work")
	head = git(t, e.repo, "rev-parse", "HEAD")
	e.d.pauseWorkers()
	id, err := e.st.CreateTask(t.Context(), store.Task{Repo: e.repo, Base: base, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetHeadSHA(t.Context(), id, head); err != nil {
		t.Fatal(err)
	}
	return id, base, head
}

func getDiff(t *testing.T, e *env, id int64) (int, DiffView, []byte) {
	t.Helper()
	code, b := e.do(t, "GET", "/api/v1/tasks/"+strconv.FormatInt(id, 10)+"/diff", "")
	var v DiffView
	if code == http.StatusOK {
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
	}
	return code, v, b
}

func TestDiffReturnsHashAndProtectedFiles(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	id, base, head := diffTask(t, e, func() {
		putFile(t, e.repo, "tests/spec.txt", "line1\nCHANGED\n")
		putFile(t, e.repo, "src/a.txt", "a\nb\nc\n")
	})
	code, v, b := getDiff(t, e, id)
	if code != http.StatusOK {
		t.Fatalf("code %d: %s", code, b)
	}
	p, err := gate.Prepare(gate.Options{Repo: e.repo, Base: base, Head: head})
	if err != nil {
		t.Fatal(err)
	}
	if v.Hash != p.Diff.Hash || v.Hash == "" {
		t.Errorf("hash %q, gate %q", v.Hash, p.Diff.Hash)
	}
	if v.HashedText != p.Diff.HashedText() || v.Truncated {
		t.Errorf("hashed text mismatch or truncated")
	}
	if want := []string{"tests/spec.txt"}; !reflect.DeepEqual(v.ProtectedFiles, want) {
		t.Errorf("protected %q, want %q", v.ProtectedFiles, want)
	}
	want := []FileStat{{"src/a.txt", 2, 0}, {"tests/spec.txt", 1, 1}}
	if !reflect.DeepEqual(v.Files, want) {
		t.Errorf("files %+v, want %+v", v.Files, want)
	}
}

func TestDiffTruncation(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	// 3-byte runes: a cut at exactly 256 KiB falls mid-rune (262144 % 3 != 0).
	big := strings.Repeat("한", 120_000) + "\n"
	id, base, head := diffTask(t, e, func() { putFile(t, e.repo, "tests/spec.txt", big) })
	code, v, b := getDiff(t, e, id)
	if code != http.StatusOK {
		t.Fatalf("code %d: %.200s", code, b)
	}
	p, err := gate.Prepare(gate.Options{Repo: e.repo, Base: base, Head: head})
	if err != nil {
		t.Fatal(err)
	}
	full := p.Diff.HashedText()
	if len(full) <= maxDiffBytes {
		t.Fatalf("test diff too small: %d", len(full))
	}
	if !v.Truncated || len(v.HashedText) > maxDiffBytes || !utf8.ValidString(v.HashedText) {
		t.Errorf("truncated=%v len=%d valid=%v", v.Truncated, len(v.HashedText), utf8.ValidString(v.HashedText))
	}
	if !strings.HasPrefix(full, v.HashedText) || len(v.HashedText) < maxDiffBytes-3 {
		t.Errorf("hashed_text is not the leading cut of the full text")
	}
	if v.Hash != p.Diff.Hash || v.Hash != gitx.HashOf(full) {
		t.Errorf("hash %q must be the full one %q", v.Hash, p.Diff.Hash)
	}
}

func TestCapUTF8(t *testing.T) {
	s := strings.Repeat("한", 10)
	for max := 0; max <= len(s); max++ {
		got, trunc := capUTF8(s, max)
		if len(got) > max || !utf8.ValidString(got) || trunc != (max < len(s)) {
			t.Errorf("max %d: got %q trunc %v", max, got, trunc)
		}
	}
}

func TestDiffNoHead409(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	id, err := e.st.CreateTask(t.Context(), store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	code, b := e.do(t, "GET", "/api/v1/tasks/"+strconv.FormatInt(id, 10)+"/diff", "")
	if code != http.StatusConflict || !strings.Contains(string(b), `"code":"conflict"`) {
		t.Fatalf("code %d: %s", code, b)
	}
	code, b = e.do(t, "GET", "/api/v1/tasks/9999/diff", "")
	if code != http.StatusNotFound || !strings.Contains(string(b), `"code":"not_found"`) {
		t.Fatalf("code %d: %s", code, b)
	}
}

func TestDiffNoTextconvNoExtDiff(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	marker := filepath.Join(shortDir(t), "ran")
	script := filepath.Join(shortDir(t), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, e.repo, "config", "diff.evil.textconv", script)
	git(t, e.repo, "config", "core.attributesFile", "/dev/null")
	id, base, head := diffTask(t, e, func() {
		putFile(t, e.repo, ".gitattributes", "* diff=evil\n")
		putFile(t, e.repo, "src/a.txt", "a\nmore\n")
		putFile(t, e.repo, "tests/spec.txt", "line1\nCHANGED\n")
	})
	// Positive controls: a plain git diff runs each configured driver, so the
	// marker would exist if the endpoint's safe flags were not in effect. (git
	// diff --numstat itself never invokes either driver today; the flags are
	// defense in depth against that changing.)
	for _, cfg := range []string{"diff.evil.textconv", "diff.external"} {
		git(t, e.repo, "config", cfg, script)
		git(t, e.repo, "diff", base, head)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("control: %s did not run without safe flags, test proves nothing: %v", cfg, err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
	}
	code, v, b := getDiff(t, e, id)
	if code != http.StatusOK {
		t.Fatalf("code %d: %s", code, b)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("textconv/external diff driver ran during the diff endpoint")
	}
	if len(v.Files) != 3 {
		t.Errorf("files %+v", v.Files)
	}
}
