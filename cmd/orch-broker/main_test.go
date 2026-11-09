package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigate/internal/broker"
)

func TestRunRefusesMissingConfigWithFixedMessage(t *testing.T) {
	var buf bytes.Buffer
	if rc := run([]string{"-config", "/nonexistent-dir-xyz/broker.json", "-state-dir", t.TempDir()}, &buf); rc == 0 {
		t.Fatal("started without a config")
	}
	if !strings.Contains(buf.String(), "orch-broker") {
		t.Fatalf("no fixed message: %q", buf.String())
	}
}

func TestRunBadFlagIsUsageError(t *testing.T) {
	var buf bytes.Buffer
	if rc := run([]string{"-nope"}, &buf); rc != 2 {
		t.Fatalf("rc=%d", rc)
	}
	if rc := run([]string{"extra-arg"}, &buf); rc != 2 {
		t.Fatalf("positional arg rc=%d", rc)
	}
}

// --- -check: configuration, trust and key modes only; never the network ---

func checkEnv(t *testing.T) (cfgPath, stateDir, keyFile, gitlabAddr string, hits *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "chk")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir)
	t.Cleanup(func() { os.RemoveAll(dir) })
	cur, _ := user.Current()
	grp, _ := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	keyFile = filepath.Join(dir, "openai.key")
	tokFile := filepath.Join(dir, "gitlab.token")
	for _, f := range []string{keyFile, tokFile} {
		if err := os.WriteFile(f, []byte("test-key-NOT-REAL\n"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	// a GitLab address that counts connection attempts
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	hits = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()
	gitlabAddr = ln.Addr().String()
	stateDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"version": 1, "socket": filepath.Join(dir, "b.sock"), "socket_group": grp.Name, "allow_uids": []string{cur.Username},
		"openai": map[string]any{"model": "test-model", "key_file": keyFile,
			"pricing": map[string]any{"input_usd_per_mtok": 2.5, "output_usd_per_mtok": 10}},
		"context_repos": []string{filepath.Join(dir, "repo")},
		"gitlab": map[string]any{"base_url": "http://" + gitlabAddr, "token_file": tokFile,
			"projects": map[string]any{filepath.Join(dir, "repo"): "g/p"}, "target_branch": "main"},
	}
	b, _ := json.Marshal(cfg)
	cfgPath = filepath.Join(dir, "broker.json")
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

// trustForTest makes the temp dir's owner (the test user) the trusted owner.
func trustForTest(dir string) checkOptions {
	return checkOptions{
		Load:       broker.LoadOptions{Trust: broker.TrustOptions{OwnerOK: func(uint32) bool { return true }, StopAt: dir}},
		KeyOwnerOK: func(uint32) bool { return true },
	}
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("outbound HTTP request from -check: %s", r.URL.Redacted())
	return nil, errors.New("blocked")
}

func TestCheckPassesWithoutAnyConnection(t *testing.T) {
	cfg, state, _, _, hits := checkEnv(t)
	old := http.DefaultTransport
	http.DefaultTransport = failTransport{t}
	defer func() { http.DefaultTransport = old }()
	var out bytes.Buffer
	if rc := checkConfig(cfg, state, trustForTest(filepath.Dir(cfg)), &out); rc != 0 {
		t.Fatalf("rc=%d %s", rc, out.String())
	}
	if !strings.Contains(out.String(), "통과") || strings.Contains(out.String(), "test-key-NOT-REAL") {
		t.Fatalf("output %q", out.String())
	}
	time.Sleep(100 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("-check connected to the GitLab address %d times", n)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatalf("-check wrote into the state dir: %v", entries)
	}
}

func TestCheckRefusesBadTrustAndKeyModes(t *testing.T) {
	cases := map[string]func(t *testing.T, cfg, state, key string){
		"config writable by others": func(t *testing.T, cfg, _, _ string) { os.Chmod(cfg, 0o666) },
		"key readable by group":     func(t *testing.T, _, _, key string) { os.Chmod(key, 0o440) },
		"key readable by others":    func(t *testing.T, _, _, key string) { os.Chmod(key, 0o644) },
		"key missing":               func(t *testing.T, _, _, key string) { os.Remove(key) },
		"state dir missing":         func(t *testing.T, _, state, _ string) { os.Remove(state) },
		"config unreadable json":    func(t *testing.T, cfg, _, _ string) { os.WriteFile(cfg, []byte("{"), 0o644) },
	}
	for name, mod := range cases {
		cfg, state, key, _, hits := checkEnv(t)
		mod(t, cfg, state, key)
		var out bytes.Buffer
		if rc := checkConfig(cfg, state, trustForTest(filepath.Dir(cfg)), &out); rc == 0 {
			t.Errorf("%s: accepted (%s)", name, out.String())
		}
		if strings.Contains(out.String(), "test-key-NOT-REAL") {
			t.Errorf("%s: output leaks the key: %q", name, out.String())
		}
		if hits.Load() != 0 {
			t.Errorf("%s: connected", name)
		}
	}
	// a key owned by someone else
	cfg, state, _, _, _ := checkEnv(t)
	o := trustForTest(filepath.Dir(cfg))
	o.KeyOwnerOK = func(uint32) bool { return false }
	var out bytes.Buffer
	if rc := checkConfig(cfg, state, o, &out); rc == 0 {
		t.Errorf("foreign key owner accepted")
	}
}

func TestRunCheckFlag(t *testing.T) {
	var out bytes.Buffer
	// Real trust rules: a config in a temp dir is not root-owned, so it is refused.
	cfg, state, _, _, hits := checkEnv(t)
	if os.Geteuid() != 0 {
		if rc := run([]string{"-check", "-config", cfg, "-state-dir", state}, &out); rc == 0 {
			t.Fatal("-check accepted a non-root-owned config")
		}
	}
	// -check never starts the service: no socket appears.
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "b.sock")); err == nil {
		t.Fatal("-check created the socket")
	}
	if hits.Load() != 0 {
		t.Fatal("connected")
	}
	if rc := run([]string{"--check", "extra"}, &out); rc != 2 {
		t.Fatalf("rc=%d", rc)
	}
}
