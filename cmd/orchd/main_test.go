package main

import (
	"context"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/api"
	"aigate/internal/orch/steps"
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

func fullVMArgs(dir string) []string {
	return []string{"--agent-host", "10.0.0.5", "--agent-key", "/keys/agent", "--agent-known-hosts", "/keys/kh-agent",
		"--test-host", "10.0.0.6", "--test-user", "tester", "--test-key", "/keys/test", "--test-known-hosts", "/keys/kh-test",
		"--test-repo-dir", "/home/tester/repo", "--cloud-cmd", "ssh -o BatchMode=yes cloud openstack"}
}

// A half-specified VM configuration is refused before anything is created.
func TestHalfSpecifiedVMFlagsRefused(t *testing.T) {
	full := fullVMArgs("")
	for drop := 0; drop < len(full); drop += 2 {
		if full[drop] == "--cloud-cmd" || full[drop] == "--test-work-dir" {
			continue // optional
		}
		dir := t.TempDir()
		args := []string{"--db", filepath.Join(dir, "db", "o.db"), "--socket", filepath.Join(dir, "s")}
		args = append(args, full[:drop]...)
		args = append(args, full[drop+2:]...)
		if code := run(args); code == 0 {
			t.Fatalf("started without %s", full[drop])
		}
		if _, err := os.Stat(filepath.Join(dir, "db")); err == nil {
			t.Fatalf("db dir created although %s was missing", full[drop])
		}
	}
	// A single VM flag alone is also refused.
	dir := t.TempDir()
	if code := run([]string{"--db", filepath.Join(dir, "o.db"), "--socket", filepath.Join(dir, "s"), "--agent-host", "10.0.0.5"}); code == 0 {
		t.Fatal("started with only --agent-host")
	}
}

// A complete configuration builds the real provider without connecting anywhere.
func TestFullVMFlagsBuildProvider(t *testing.T) {
	var vf vmFlags
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	vf.register(fs)
	if err := fs.Parse(fullVMArgs("")); err != nil {
		t.Fatal(err)
	}
	if !vf.enabled() {
		t.Fatal("not enabled")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := vf.provider(st, filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*steps.Provider); !ok {
		t.Fatalf("provider is %T", p)
	}
	if n := len(p.Steps()); n != 5 {
		t.Fatalf("%d steps", n)
	}
	var none vmFlags
	if none.enabled() {
		t.Fatal("empty flags must keep the placeholder")
	}
}

func TestSocketUIDFlag(t *testing.T) {
	var u uidList
	for _, v := range []string{"0", "1001"} {
		if err := u.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(u) != 2 || u[0] != 0 || u[1] != 1001 {
		t.Fatalf("%v", u)
	}
	for _, bad := range []string{"", "-1", "orch", "4294967296"} {
		if err := u.Set(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
