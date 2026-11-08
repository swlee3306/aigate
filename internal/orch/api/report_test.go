package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/orch/store"
)

// reportEnv returns an env whose daemon serves reports from a fresh dir, plus
// that dir (symlink-resolved, like the steps provider's own).
func reportEnv(t *testing.T) (*env, string) {
	t.Helper()
	dir := filepath.Join(shortDir(t), "reports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, noSteps{}, 1, func(c *Config) { c.ReportDir = dir })
	return e, dir
}

func (e *env) reportTask(t *testing.T, reportPath string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := e.st.CreateTask(ctx, store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "버그수정", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if reportPath != "" {
		if err := e.st.SetVerdict(ctx, id, "ok", reportPath); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func reportURL(id int64) string { return fmt.Sprintf("/api/v1/tasks/%d/report", id) }

func TestReportServedFromReportDir(t *testing.T) {
	e, dir := reportEnv(t)
	want := []byte(`{"verdict":"ok","note":"<b>raw</b>"}`)
	p := filepath.Join(dir, "1.json")
	if err := os.WriteFile(p, want, 0o600); err != nil {
		t.Fatal(err)
	}
	id := e.reportTask(t, p)
	code, b := e.do(t, "GET", reportURL(id), "")
	if code != 200 || !bytes.Equal(b, want) {
		t.Fatalf("got %d %q", code, b)
	}
}

func TestReportOutsideDirRefused(t *testing.T) {
	e, dir := reportEnv(t)
	outside := filepath.Join(filepath.Dir(dir), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"secret":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"passwd":   "/etc/passwd",
		"sibling":  outside,
		"dotdot":   filepath.Join(dir, "..", "secret.json"),
		"relative": "../secret.json",
		"prefix":   dir + "-evil/x.json", // shares the dir name as a string prefix
	} {
		id := e.reportTask(t, p)
		code, b := e.do(t, "GET", reportURL(id), "")
		if code != 404 || strings.Contains(string(b), "secret") {
			t.Errorf("%s: got %d %s", name, code, b)
		}
	}
}

func TestReportSymlinkEscapeRefused(t *testing.T) {
	e, dir := reportEnv(t)
	outside := filepath.Join(filepath.Dir(dir), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"secret":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "7.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	id := e.reportTask(t, link)
	code, b := e.do(t, "GET", reportURL(id), "")
	if code != 404 || strings.Contains(string(b), "secret") {
		t.Fatalf("file symlink: got %d %s", code, b)
	}
	// A symlinked directory inside the report dir pointing out.
	outDir := filepath.Join(filepath.Dir(dir), "outdir")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "x.json"), []byte(`{"secret":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outDir, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	id = e.reportTask(t, filepath.Join(dir, "sub", "x.json"))
	code, b = e.do(t, "GET", reportURL(id), "")
	if code != 404 || strings.Contains(string(b), "secret") {
		t.Fatalf("dir symlink: got %d %s", code, b)
	}
}

func TestReportTooLarge(t *testing.T) {
	e, dir := reportEnv(t)
	p := filepath.Join(dir, "big.json")
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), maxReportBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	id := e.reportTask(t, p)
	code, b := e.do(t, "GET", reportURL(id), "")
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(string(b), `"too_large"`) {
		t.Fatalf("got %d %s", code, b)
	}
	// Exactly at the cap is served.
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), maxReportBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ = e.do(t, "GET", reportURL(id), ""); code != 200 {
		t.Fatalf("at cap: %d", code)
	}
}

func TestReportNotYet(t *testing.T) {
	e, _ := reportEnv(t)
	id := e.reportTask(t, "")
	code, b := e.do(t, "GET", reportURL(id), "")
	if code != 404 || !strings.Contains(string(b), `"not_found"`) {
		t.Fatalf("got %d %s", code, b)
	}
	if code, _ = e.do(t, "GET", reportURL(9999), ""); code != 404 {
		t.Fatalf("unknown task: %d", code)
	}
}

func TestReportDirNotConfigured(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	p := filepath.Join(shortDir(t), "1.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	id := e.reportTask(t, p)
	if code, b := e.do(t, "GET", reportURL(id), ""); code != 404 {
		t.Fatalf("got %d %s", code, b)
	}
}
