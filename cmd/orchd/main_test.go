package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/api"
	"aigate/internal/orch/store"
)

// A task submitted to the placeholder steps must never reach passed: it ends
// in failed after its retries.
func TestPlaceholderNeverPasses(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "orchd")
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
	d, err := api.NewDaemon(api.Config{Socket: sock, Concurrency: 1, AllowedRepos: []string{filepath.Join(dir, "repos")}}, st, placeholderSteps{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-d.Ready()
	defer func() { cancel(); <-done }()

	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := cl.Post("http://orchd/tasks", "application/json", strings.NewReader(
		`{"repo":"`+repo+`","base":"HEAD","prompt":"p","class":"x","agent":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	var v api.TaskView
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		tk, err := st.GetTask(context.Background(), v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tk.State == store.StatePassed || tk.State == store.StateDone {
			t.Fatalf("placeholder task reached %s", tk.State)
		}
		if tk.State == store.StateFailed {
			if !strings.Contains(tk.ErrorText, "not implemented") {
				t.Fatalf("error text %q", tk.ErrorText)
			}
			evs, _ := st.Events(context.Background(), v.ID)
			for _, e := range evs {
				if e.To == store.StatePassed {
					t.Fatal("passed appears in history")
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task never failed")
}
