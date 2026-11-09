package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aigate/internal/orch/api"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/store"
)

type noSteps struct{}

func (noSteps) Steps() []machine.Step { return nil }

func TestCLIRoundTrip(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "orch")
	defer os.RemoveAll(dir)
	dir, _ = filepath.EvalSymlinks(dir)
	repo := filepath.Join(dir, "repos", "p")
	os.MkdirAll(repo, 0o755)
	for _, a := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "i"}} {
		c := exec.Command("git", append([]string{"-C", repo}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	st, err := store.Open(filepath.Join(dir, "db", "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sock := filepath.Join(dir, "s")
	d, _ := api.NewDaemon(api.Config{Socket: sock, AllowedRepos: []string{filepath.Join(dir, "repos")}}, st, noSteps{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	defer func() { cancel(); <-done }()

	do := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := run(append([]string{"--socket", sock}, args...), &o, &e)
		return code, o.String(), e.String()
	}
	code, out, errS := do("submit", "--repo", repo, "--base", "HEAD", "--prompt", "안녕", "--class", "버그수정", "--agent", "claude")
	if code != 0 || !strings.Contains(out, "작업 #1 제출됨") {
		t.Fatalf("submit %d %q %q", code, out, errS)
	}
	if code, out, _ = do("list"); code != 0 || !strings.Contains(out, "queued") {
		t.Fatalf("list %d %q", code, out)
	}
	if code, out, _ = do("show", "1"); code != 0 || !strings.Contains(out, "안녕") {
		t.Fatalf("show %d %q", code, out)
	}
	// show names who made the latest change (the socket peer submitted it)
	if want := "  주체:     socket:uid:" + strconv.Itoa(os.Getuid()) + "\n"; !strings.Contains(out, want) {
		t.Fatalf("show actor: want %q in %q", want, out)
	}
	code, out, _ = do("--json", "show", "1")
	var v api.TaskView
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.ID != 1 {
		t.Fatalf("json show %d %q", code, out)
	}
	if code, out, _ = do("cancel", "1"); code != 0 || !strings.Contains(out, "취소됨") {
		t.Fatalf("cancel %d %q", code, out)
	}
	// one-line errors
	code, _, errS = do("cancel", "1")
	if code != 1 || !strings.Contains(errS, "이미 끝난 작업") || strings.Count(errS, "\n") != 1 {
		t.Fatalf("cancel again %d %q", code, errS)
	}
	code, _, errS = do("submit", "--repo", "/etc", "--base", "HEAD", "--prompt", "p", "--class", "x", "--agent", "claude")
	if code != 1 || !strings.Contains(errS, "오류:") {
		t.Fatalf("outside %d %q", code, errS)
	}
	if code, _, errS = do("submit", "--repo", repo); code != 1 || !strings.Contains(errS, "--base") {
		t.Fatalf("missing opts %d %q", code, errS)
	}
	if code, _, errS = do("show", "abc"); code != 1 {
		t.Fatalf("bad id %d %q", code, errS)
	}
	var o, e bytes.Buffer
	if code := run([]string{"--socket", filepath.Join(dir, "none"), "list"}, &o, &e); code != 1 || !strings.Contains(e.String(), "연결할 수 없습니다") {
		t.Fatalf("no daemon %d %q", code, e.String())
	}
}
