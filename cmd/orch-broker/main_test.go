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
		"repo_roots":    []string{"/home/orch/repos"},
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
	sp := filepath.Join(dir, "etc-aigate", "signers.json")
	if _, err := os.Stat(sp); err != nil {
		os.MkdirAll(filepath.Dir(sp), 0o755)
		os.Chmod(filepath.Dir(sp), 0o755)
		os.WriteFile(sp, []byte(`{"version":1,"signers":[]}`), 0o644)
		os.Chmod(sp, 0o644)
	}
	return checkOptions{
		Load:           broker.LoadOptions{Trust: broker.TrustOptions{OwnerOK: func(uint32) bool { return true }, StopAt: dir}},
		KeyOwnerOK:     func(uint32) bool { return true },
		SignersPath:    sp,
		SignersOwnerOK: func(uint32) bool { return true },
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

func TestCheckWarnsAboutLegacyEntries(t *testing.T) {
	cfg, state, _, _, _ := checkEnv(t)
	o := trustForTest(filepath.Dir(cfg))
	// no registry yet: the legacy entries are about to be migrated, no warning
	var out bytes.Buffer
	if rc := checkConfig(cfg, state, o, &out); rc != 0 || strings.Contains(out.String(), "경고") {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
	// a registry exists and broker.json still lists the old entries: warn, rc 0
	if _, err := broker.OpenRegistry(state, broker.LegacySource{}, broker.RegistryOptions{OwnerOK: func(uint32) bool { return true }}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if rc := checkConfig(cfg, state, o, &out); rc != 0 {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
	if !strings.Contains(out.String(), "경고") || !strings.Contains(out.String(), "context_repos") || !strings.Contains(out.String(), "통과") {
		t.Fatalf("no warning sentence: %q", out.String())
	}
	if strings.Contains(out.String(), "/repo") || strings.Contains(out.String(), "g/p") {
		t.Fatalf("warning quotes config values: %q", out.String())
	}
}

func TestCheckRefusesBrokenRegistry(t *testing.T) {
	cfg, state, _, _, _ := checkEnv(t)
	if err := os.WriteFile(filepath.Join(state, broker.RegistryFileName), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if rc := checkConfig(cfg, state, trustForTest(filepath.Dir(cfg)), &out); rc == 0 || !strings.Contains(out.String(), "등록부") {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(state, broker.RegistryFileName)); string(b) != "junk" {
		t.Fatal("-check modified the registry")
	}
}

// --- final fix round: -check also covers the signers list, the used-request
// file, repo_roots and a dry-run of the migration (all read-only) ---

func stateEntries(t *testing.T, state string) []string {
	t.Helper()
	es, _ := os.ReadDir(state)
	var n []string
	for _, e := range es {
		n = append(n, e.Name())
	}
	return n
}

func TestCheckRefusesMissingOrUntrustedSigners(t *testing.T) {
	for name, mod := range map[string]func(sp string){
		"absent":         func(sp string) { os.Remove(sp) },
		"group writable": func(sp string) { os.Chmod(sp, 0o664) },
		"broken json":    func(sp string) { os.WriteFile(sp, []byte("{"), 0o644) },
		"dir writable":   func(sp string) { os.Chmod(filepath.Dir(sp), 0o777) },
	} {
		cfg, state, _, _, _ := checkEnv(t)
		o := trustForTest(filepath.Dir(cfg))
		mod(o.SignersPath)
		var out bytes.Buffer
		rc := checkConfig(cfg, state, o, &out)
		os.Chmod(filepath.Dir(o.SignersPath), 0o755)
		if rc == 0 || !strings.Contains(out.String(), "서명자 목록") {
			t.Errorf("%s: rc=%d out=%q", name, rc, out.String())
		}
		if len(stateEntries(t, state)) != 0 {
			t.Errorf("%s: -check wrote into the state dir", name)
		}
	}
}

func TestCheckRefusesBrokenUsedRequestsFile(t *testing.T) {
	cfg, state, _, _, _ := checkEnv(t)
	o := trustForTest(filepath.Dir(cfg))
	for name, content := range map[string]string{"junk": "junk", "version": `{"version":2,"entries":{}}`, "bad id": `{"version":1,"entries":{"x":1}}`} {
		os.WriteFile(filepath.Join(state, broker.UsedRequestsFileName), []byte(content), 0o600)
		var out bytes.Buffer
		if rc := checkConfig(cfg, state, o, &out); rc == 0 || !strings.Contains(out.String(), "요청 ID") {
			t.Errorf("%s: rc=%d out=%q", name, rc, out.String())
		}
		if b, _ := os.ReadFile(filepath.Join(state, broker.UsedRequestsFileName)); string(b) != content {
			t.Errorf("%s: -check modified the file", name)
		}
	}
	// wrong mode is broken too
	os.WriteFile(filepath.Join(state, broker.UsedRequestsFileName), []byte(`{"version":1,"entries":{}}`), 0o644)
	os.Chmod(filepath.Join(state, broker.UsedRequestsFileName), 0o644)
	var out bytes.Buffer
	if rc := checkConfig(cfg, state, o, &out); rc == 0 {
		t.Errorf("mode 0644 accepted: %q", out.String())
	}
	// a good one passes
	os.WriteFile(filepath.Join(state, broker.UsedRequestsFileName), []byte(`{"version":1,"entries":{}}`), 0o600)
	os.Chmod(filepath.Join(state, broker.UsedRequestsFileName), 0o600)
	out.Reset()
	if rc := checkConfig(cfg, state, o, &out); rc != 0 {
		t.Errorf("good file refused: %q", out.String())
	}
}

func TestCheckWarnsWhenRepoRootsIsEmpty(t *testing.T) {
	cfg, state, _, _, _ := checkEnv(t)
	set := func(v any) {
		raw, _ := os.ReadFile(cfg)
		var m map[string]any
		json.Unmarshal(raw, &m)
		if v == nil {
			delete(m, "repo_roots")
		} else {
			m["repo_roots"] = v
		}
		b, _ := json.Marshal(m)
		os.WriteFile(cfg, b, 0o644)
	}
	set(nil)
	var out bytes.Buffer
	if rc := checkConfig(cfg, state, trustForTest(filepath.Dir(cfg)), &out); rc != 0 || !strings.Contains(out.String(), "repo_roots") || !strings.Contains(out.String(), "path_not_allowed") {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
	set([]string{"/home/orch/repos"})
	out.Reset()
	if rc := checkConfig(cfg, state, trustForTest(filepath.Dir(cfg)), &out); rc != 0 || strings.Contains(out.String(), "경고") {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
}

func TestCheckDryRunsMigrationAndRefusesConflict(t *testing.T) {
	cfg, state, _, _, _ := checkEnv(t)
	raw, _ := os.ReadFile(cfg)
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["context_repos"] = []string{"/srv/a/same", "/srv/b/same"} // both become the id "same"
	m["gitlab"] = nil
	delete(m, "gitlab")
	b, _ := json.Marshal(m)
	os.WriteFile(cfg, b, 0o644)
	o := trustForTest(filepath.Dir(cfg))
	var out bytes.Buffer
	if rc := checkConfig(cfg, state, o, &out); rc == 0 || !strings.Contains(out.String(), "옮길 수 없습니다") {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
	if len(stateEntries(t, state)) != 0 {
		t.Fatal("the dry run wrote something")
	}
	// once a registry exists the legacy entries are not migrated again: only a warning
	if _, err := broker.OpenRegistry(state, broker.LegacySource{}, broker.RegistryOptions{OwnerOK: func(uint32) bool { return true }}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if rc := checkConfig(cfg, state, o, &out); rc != 0 {
		t.Fatalf("rc=%d out=%q", rc, out.String())
	}
}

func TestCheckValidatesSecretsDirAndMeta(t *testing.T) {
	cfg, state, _, _, _ := checkEnv(t)
	co := trustForTest(filepath.Dir(cfg))
	snapshot := func() string {
		var sb strings.Builder
		filepath.Walk(state, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				b, _ := os.ReadFile(p)
				sb.WriteString(p + "|" + fi.Mode().String() + "|" + string(b) + "\n")
			}
			return nil
		})
		return sb.String()
	}
	run1 := func() (int, string) {
		var out bytes.Buffer
		rc := checkConfig(cfg, state, co, &out)
		return rc, out.String()
	}
	// nothing there: passes and creates nothing (no secrets/, no tokens.json)
	before := snapshot()
	if rc, out := run1(); rc != 0 {
		t.Fatalf("empty state: rc=%d %s", rc, out)
	}
	if snapshot() != before {
		t.Fatal("-check wrote into the state dir")
	}
	if _, err := os.Lstat(filepath.Join(state, "secrets")); err == nil {
		t.Fatal("-check created secrets/")
	}
	// healthy store: passes, still read-only
	canary := "canary-NOT-A-REAL-TOKEN-0123456789abcdefXYZ"
	os.Mkdir(filepath.Join(state, "secrets"), 0o700)
	os.Chmod(filepath.Join(state, "secrets"), 0o700)
	os.WriteFile(filepath.Join(state, "secrets", "openai"), []byte(canary), 0o400)
	os.Chmod(filepath.Join(state, "secrets", "openai"), 0o400)
	meta := `{"version":1,"tokens":[{"ref":"openai","kind":"openai","last4":"0XYZ","expires":"","set_at":"2026-11-10T00:00:00Z"}]}`
	os.WriteFile(filepath.Join(state, "tokens.json"), []byte(meta), 0o600)
	os.Chmod(filepath.Join(state, "tokens.json"), 0o600)
	before = snapshot()
	if rc, out := run1(); rc != 0 || strings.Contains(out, canary) {
		t.Fatalf("healthy store: rc=%d %s", rc, out)
	}
	if snapshot() != before {
		t.Fatal("-check changed the store")
	}
	// a bad secret file only warns (that token is unusable at run time)
	os.Chmod(filepath.Join(state, "secrets", "openai"), 0o444)
	if rc, out := run1(); rc != 0 || !strings.Contains(out, "경고") || strings.Contains(out, canary) {
		t.Fatalf("bad file mode: rc=%d %s", rc, out)
	}
	os.Chmod(filepath.Join(state, "secrets", "openai"), 0o400)
	// a group-accessible secrets dir is refused
	os.Chmod(filepath.Join(state, "secrets"), 0o750)
	if rc, out := run1(); rc == 0 || !strings.Contains(out, "토큰") {
		t.Fatalf("0750 secrets/: rc=%d %s", rc, out)
	}
	os.Chmod(filepath.Join(state, "secrets"), 0o700)
	// broken or wrong-mode tokens.json is refused and left alone
	for _, c := range []struct {
		content string
		mode    os.FileMode
	}{{"{garbage", 0o600}, {meta, 0o644}} {
		os.Remove(filepath.Join(state, "tokens.json"))
		os.WriteFile(filepath.Join(state, "tokens.json"), []byte(c.content), c.mode)
		os.Chmod(filepath.Join(state, "tokens.json"), c.mode)
		before = snapshot()
		if rc, out := run1(); rc == 0 || !strings.Contains(out, "토큰") || strings.Contains(out, canary) {
			t.Fatalf("bad tokens.json (%q, %v): rc=%d %s", c.content, c.mode, rc, out)
		}
		if snapshot() != before {
			t.Fatal("-check changed a broken store")
		}
	}
}

func TestCheckValidatesProviders(t *testing.T) {
	cfg, state, _, gitlabAddr, hits := checkEnv(t)
	co := trustForTest(filepath.Dir(cfg))
	canary := "canary-NOT-A-REAL-TOKEN-0123456789abcdefXYZ"
	snapshot := func() string {
		var sb strings.Builder
		filepath.Walk(state, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				b, _ := os.ReadFile(p)
				sb.WriteString(p + "|" + fi.Mode().String() + "|" + string(b) + "\n")
			}
			return nil
		})
		return sb.String()
	}
	reg := func(base, ssh, ref string) {
		body := `{"version":2,"providers":[{"id":"corp","kind":"gitlab","base_url":"` + base + `","ssh_url":"` + ssh + `","token_ref":"` + ref + `"}],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`
		os.Remove(filepath.Join(state, "registry.json"))
		os.WriteFile(filepath.Join(state, "registry.json"), []byte(body), 0o600)
		os.Chmod(filepath.Join(state, "registry.json"), 0o600)
	}
	run1 := func() (int, string) {
		var out bytes.Buffer
		rc := checkConfig(cfg, state, co, &out)
		return rc, out.String()
	}
	// valid provider, token missing: passes with a warning
	reg("http://100.64.0.10:18082", "ssh://git@100.64.0.10", "gl-admin")
	before := snapshot()
	rc, out := run1()
	if rc != 0 || !strings.Contains(out, "경고") || !strings.Contains(out, "token_ref_unknown") {
		t.Fatalf("missing token: rc=%d %s", rc, out)
	}
	if snapshot() != before {
		t.Fatal("-check wrote into the state dir")
	}
	// token present and usable: no provider warning
	os.Mkdir(filepath.Join(state, "secrets"), 0o700)
	os.Chmod(filepath.Join(state, "secrets"), 0o700)
	os.WriteFile(filepath.Join(state, "secrets", "gl-admin"), []byte(canary), 0o400)
	os.Chmod(filepath.Join(state, "secrets", "gl-admin"), 0o400)
	os.WriteFile(filepath.Join(state, "tokens.json"), []byte(`{"version":1,"tokens":[{"ref":"gl-admin","kind":"gitlab","last4":"0XYZ","expires":"","set_at":"2026-11-10T00:00:00Z"}]}`), 0o600)
	os.Chmod(filepath.Join(state, "tokens.json"), 0o600)
	before = snapshot()
	rc, out = run1()
	if rc != 0 || strings.Contains(out, "token_ref_unknown") || strings.Contains(out, canary) {
		t.Fatalf("healthy provider: rc=%d %s", rc, out)
	}
	if snapshot() != before {
		t.Fatal("-check changed the state")
	}
	// forbidden addresses in a stored provider are refused
	for _, c := range [][2]string{
		{"http://127.0.0.1:" + strings.TrimPrefix(gitlabAddr, "127.0.0.1:"), "ssh://git@10.0.0.1"},
		{"http://169.254.169.254", "ssh://git@10.0.0.1"},
		{"http://localhost", "ssh://git@10.0.0.1"},
		{"http://10.0.0.1/api/v4", "ssh://git@10.0.0.1"},
		{"http://10.0.0.1", "ssh://git@127.0.0.1"},
	} {
		reg(c[0], c[1], "gl-admin")
		before = snapshot()
		if rc, out := run1(); rc == 0 || !(strings.Contains(out, "제공자") || strings.Contains(out, "등록부")) {
			t.Fatalf("%v accepted: rc=%d %s", c, rc, out)
		}
		if snapshot() != before {
			t.Fatal("-check changed the state of a refused provider")
		}
	}
	// no connection attempt at all (the fake GitLab counts them)
	if hits.Load() != 0 {
		t.Fatalf("-check connected %d times", hits.Load())
	}
}

// TestCheckValidatesGitHubProviders: -check is kind-aware (a github provider
// needs a github-kind token, not a gitlab one), still read-only and network-free,
// and refuses plain http to a public host.
func TestCheckValidatesGitHubProviders(t *testing.T) {
	cfg, state, _, _, hits := checkEnv(t)
	co := trustForTest(filepath.Dir(cfg))
	snapshot := func() string {
		var sb strings.Builder
		filepath.Walk(state, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				b, _ := os.ReadFile(p)
				sb.WriteString(p + "|" + fi.Mode().String() + "|" + string(b) + "\n")
			}
			return nil
		})
		return sb.String()
	}
	reg := func(base, ssh string) {
		body := `{"version":3,"providers":[{"id":"gh","kind":"github","base_url":"` + base + `","ssh_url":"` + ssh + `","token_ref":"gh-admin"}],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[]}`
		os.Remove(filepath.Join(state, "registry.json"))
		os.WriteFile(filepath.Join(state, "registry.json"), []byte(body), 0o600)
		os.Chmod(filepath.Join(state, "registry.json"), 0o600)
	}
	run1 := func() (int, string) {
		var out bytes.Buffer
		rc := checkConfig(cfg, state, co, &out)
		return rc, out.String()
	}
	tok := func(kind string) {
		os.Mkdir(filepath.Join(state, "secrets"), 0o700)
		os.Chmod(filepath.Join(state, "secrets"), 0o700)
		os.Remove(filepath.Join(state, "secrets", "gh-admin"))
		os.WriteFile(filepath.Join(state, "secrets", "gh-admin"), []byte("canary-NOT-A-REAL-TOKEN-0123456789abcdefXYZ"), 0o400)
		os.Chmod(filepath.Join(state, "secrets", "gh-admin"), 0o400)
		os.Remove(filepath.Join(state, "tokens.json"))
		os.WriteFile(filepath.Join(state, "tokens.json"), []byte(`{"version":1,"tokens":[{"ref":"gh-admin","kind":"`+kind+`","last4":"0XYZ","expires":"","set_at":"2026-11-10T00:00:00Z"}]}`), 0o600)
		os.Chmod(filepath.Join(state, "tokens.json"), 0o600)
	}
	reg("https://api.github.com", "ssh://git@github.com")
	tok("gitlab") // wrong kind: warns, passes
	before := snapshot()
	rc, out := run1()
	if rc != 0 || !strings.Contains(out, "github 종류 토큰") || !strings.Contains(out, "token_ref_unknown") {
		t.Fatalf("wrong-kind token: rc=%d %s", rc, out)
	}
	if snapshot() != before {
		t.Fatal("-check wrote into the state dir")
	}
	tok("github")
	before = snapshot()
	if rc, out = run1(); rc != 0 || strings.Contains(out, "token_ref_unknown") {
		t.Fatalf("healthy github provider: rc=%d %s", rc, out)
	}
	if snapshot() != before {
		t.Fatal("-check changed the state")
	}
	for _, c := range [][2]string{
		{"http://api.github.com", "ssh://git@github.com"},
		{"https://127.0.0.1", "ssh://git@github.com"},
		{"https://api.github.com", "ssh://git@169.254.169.254"},
	} {
		reg(c[0], c[1])
		if rc, out := run1(); rc == 0 {
			t.Fatalf("%v accepted: %s", c, out)
		}
	}
	reg("http://10.0.0.5", "ssh://git@10.0.0.5") // GitHub Enterprise Server on a private address
	if rc, out := run1(); rc != 0 {
		t.Fatalf("private http GHES refused: %s", out)
	}
	if hits.Load() != 0 {
		t.Fatalf("-check connected %d times", hits.Load())
	}
}
