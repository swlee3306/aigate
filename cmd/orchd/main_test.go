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

	"aigate/internal/approval/signing"
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

func TestWebAuthnFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--rp-id", "console.example.test"},
		{"--origin", "https://console.example.test"},
		{"--rp-id", "console.example.test", "--origin", "https://evil.example.test"},
		{"--rp-id", "https://console.example.test", "--origin", "https://console.example.test"},
		{"--rp-id", "console.example.test", "--origin", "http://console.example.test"},
		{"--pending-file", "relative.json"},
		{"--pending-file", "/var/lib/orchd/../x.json"},
	} {
		db := filepath.Join(t.TempDir(), "never", "o.db")
		if code := run(append(args, "--db", db)); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if _, err := os.Stat(filepath.Dir(db)); !os.IsNotExist(err) {
			t.Errorf("%v: database directory created before the flags were checked", args)
		}
	}
	ok := api.Config{RPID: "console.example.test", Origin: "https://console.example.test", PendingFile: api.DefaultPendingFile}
	if err := checkWebAuthnFlags(ok); err != nil {
		t.Fatal(err)
	}
	if err := checkWebAuthnFlags(api.Config{PendingFile: api.DefaultPendingFile}); err != nil {
		t.Fatalf("passkeys off: %v", err)
	}
	if api.DefaultPendingFile != "/home/orch/orchd/state/signers.pending.json" {
		t.Fatalf("default %q", api.DefaultPendingFile)
	}
}

func TestListenerFlags(t *testing.T) {
	web := []string{"--rp-id", "console.example.test", "--origin", "https://console.example.test"}
	for _, args := range [][]string{
		{"--http-listen", "127.0.0.1:8808"},                                                         // no origin, rp-id, host
		append([]string{"--http-listen", "127.0.0.1:8808"}, web...),                                 // no allowed host
		append([]string{"--http-listen", "127.0.0.1:8808", "--allowed-host", "other.test"}, web...), // origin host unlisted
		append([]string{"--http-listen", "0.0.0.0:8808", "--allowed-host", "console.example.test"}, web...),
		append([]string{"--http-listen", ":8808", "--allowed-host", "console.example.test"}, web...),
		append([]string{"--http-listen", "[::]:8808", "--allowed-host", "console.example.test"}, web...),
		append([]string{"--http-listen", "10.0.0.5:8808", "--allowed-host", "console.example.test"}, web...),
		append([]string{"--http-listen", "127.0.0.1:8808", "--allowed-host", "https://console.example.test"}, web...),
		{"--report-dir", "reports"},
		{"--report-dir", "/var/lib/orchd/../reports"},
		{"--signers-file", "signers.json"},
		{"--signers-file", "/etc/aigate/../signers.json"},
		{"--session-ttl", "1m"},
		{"--session-ttl", "25h"},
		{"--socket-uid", "4294967295"},
	} {
		db := filepath.Join(t.TempDir(), "never", "o.db")
		if code := run(append(args, "--db", db)); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if _, err := os.Stat(filepath.Dir(db)); !os.IsNotExist(err) {
			t.Errorf("%v: database directory created before the flags were checked", args)
		}
	}
	ok := api.Config{HTTPListen: "127.0.0.1:8808", AllowedHosts: []string{"console.example.test"},
		RPID: "console.example.test", Origin: "https://console.example.test", SessionTTL: api.SessionIdleTTL}
	if err := checkListenerFlags(ok, "/var/lib/orchd/reports", "/etc/aigate/signers.json"); err != nil {
		t.Fatal(err)
	}
	if err := checkListenerFlags(api.Config{}, "", "/etc/aigate/signers.json"); err != nil {
		t.Fatalf("listener off: %v", err)
	}
}

func TestSignersFileFlagSetsRegistryPath(t *testing.T) {
	old := signing.RegistryPath()
	t.Cleanup(func() { signing.SetRegistryPath(old) })
	// Absent under a root-owned directory: legacy mode.
	absent := "/usr/aigate-absent-for-test/signers.json"
	if mode := applySignersFile(absent); signing.RegistryPath() != absent || !strings.Contains(mode, "legacy") {
		t.Fatalf("absent: path %q mode %q", signing.RegistryPath(), mode)
	}
	p := filepath.Join(t.TempDir(), "signers.json")
	// Absent under a directory root does not own: fail closed (R16).
	if mode := applySignersFile(p); signing.RegistryPath() != p || !strings.Contains(mode, "FAIL CLOSED") {
		t.Fatalf("absent in untrusted dir: path %q mode %q", signing.RegistryPath(), mode)
	}
	// A registry the daemon cannot trust (not root-owned) is signed mode, fail closed.
	if err := os.WriteFile(p, []byte(`{"version":1,"signers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if mode := applySignersFile(p); !strings.Contains(mode, "FAIL CLOSED") {
		t.Fatalf("untrusted: mode %q", mode)
	}
}

func TestQueueFlagsValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--queue-limit", "-1"},
		{"--queue-limit", "x"},
		{"--queue-limit-total", "-1"},
		{"--queue-limit-total", "x"},
		{"--queue-limit", "10", "--queue-limit-total", "9"},
	} {
		db := filepath.Join(t.TempDir(), "never", "o.db")
		if code := run(append(args, "--db", db)); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if _, err := os.Stat(filepath.Dir(db)); !os.IsNotExist(err) {
			t.Errorf("%v: database directory created before the flags were checked", args)
		}
	}
	// an over-default per-principal limit with the default total is refused too
	if code := run([]string{"--queue-limit", "101", "--db", filepath.Join(t.TempDir(), "never", "o.db")}); code != 2 {
		t.Errorf("--queue-limit 101 with the default total: exit %d, want 2", code)
	}
	for _, c := range []struct {
		cfg api.Config
		ok  bool
	}{
		{api.Config{}, true}, // both defaults: 20 / 100
		{api.Config{QueueLimit: 5}, true},
		{api.Config{QueueLimit: 5, QueueLimitTotal: 5}, true},
		{api.Config{QueueLimit: 5, QueueLimitTotal: 4}, false},
		{api.Config{QueueLimit: 101}, false},     // effective total is the default 100
		{api.Config{QueueLimitTotal: 19}, false}, // effective per-principal is the default 20
		{api.Config{QueueLimit: -3}, false},
		{api.Config{QueueLimitTotal: -3}, false},
	} {
		err := checkQueueLimit(c.cfg)
		if (err == nil) != c.ok {
			t.Errorf("%+v: err %v, want ok=%v", c.cfg, err, c.ok)
		}
	}
	// the refusal text is fixed and carries no user input
	err := checkQueueLimit(api.Config{QueueLimit: 7, QueueLimitTotal: 3})
	if err == nil || !strings.Contains(err.Error(), "--queue-limit-total") || !strings.Contains(err.Error(), "--queue-limit") {
		t.Errorf("message: %v", err)
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	var cfg api.Config
	registerQueueLimit(fs, &cfg)
	if f := fs.Lookup("queue-limit"); f == nil || f.DefValue != "20" || !strings.Contains(f.Usage, "20") || !strings.Contains(f.Usage, "principal") {
		t.Fatalf("flag %+v", f)
	}
	if f := fs.Lookup("queue-limit-total"); f == nil || f.DefValue != "100" || !strings.Contains(f.Usage, "100") {
		t.Fatalf("flag %+v", f)
	}
}

// --broker-socket wires the merge request creator into the provider; without
// it every done task's MR is skipped.
func TestBrokerSocketWiresMR(t *testing.T) {
	for sock, want := range map[string]bool{"/run/orch-broker/broker.sock": true, "": false} {
		var vf vmFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		vf.register(fs)
		if err := fs.Parse(fullVMArgs("")); err != nil {
			t.Fatal(err)
		}
		vf.BrokerSocket = sock
		st, err := store.Open(filepath.Join(t.TempDir(), "o.db"))
		if err != nil {
			t.Fatal(err)
		}
		p, err := vf.provider(st, filepath.Join(t.TempDir(), "o.db"))
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := p.(*steps.Provider).MRConfigured(); got != want {
			t.Fatalf("socket %q: MRConfigured %v, want %v", sock, got, want)
		}
	}
}
