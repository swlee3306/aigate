package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	testOpenAIKey    = "test-key-NOT-REAL"
	testGitLabToken  = "test-token-NOT-REAL"
	testOwnerAnyone  = true
	testShortTimeout = 3 * time.Second
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeHTTP records every request it receives; broker tests must show that
// nothing reaches the fake OpenAI/GitLab endpoints unless a test asks for it.
type fakeHTTP struct {
	*httptest.Server
	mu   sync.Mutex
	hits int
}

func newFakeHTTP(t *testing.T, h http.HandlerFunc) *fakeHTTP {
	t.Helper()
	f := &fakeHTTP{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeHTTP) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// testEnv is a broker config on disk in a private temp dir.
type testEnv struct {
	Dir     string
	CfgPath string
	Raw     map[string]any
	KeyFile string
	TokFile string
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "brk")
	if err != nil {
		t.Fatal(err)
	}
	d, _ = filepath.EvalSymlinks(d)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := shortTempDir(t)
	e := &testEnv{Dir: dir, KeyFile: filepath.Join(dir, "openai.key"), TokFile: filepath.Join(dir, "gitlab.token")}
	if err := os.WriteFile(e.KeyFile, []byte(testOpenAIKey+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.TokFile, []byte(testGitLabToken+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	cur, _ := user.Current()
	grp, _ := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	e.Raw = map[string]any{
		"version":      1,
		"socket":       filepath.Join(dir, "b.sock"),
		"socket_group": grp.Name,
		"allow_uids":   []string{cur.Username},
		"openai": map[string]any{
			"model":             "test-model",
			"key_file":          e.KeyFile,
			"max_output_tokens": 4096,
			"max_input_bytes":   262144,
			"pricing":           map[string]any{"input_usd_per_mtok": 2.5, "output_usd_per_mtok": 10},
		},
		"limits":        map[string]any{"refine_per_hour": 3, "create_mr_per_hour": 5, "weekly_budget_usd": 25},
		"context_repos": []string{filepath.Join(dir, "repo-a")},
		"gitlab": map[string]any{
			"base_url":      "http://127.0.0.1:1",
			"token_file":    e.TokFile,
			"projects":      map[string]any{filepath.Join(dir, "repo-a"): "grp/proj"},
			"target_branch": "main",
		},
	}
	e.CfgPath = filepath.Join(dir, "broker.json")
	e.write(t)
	return e
}

func (e *testEnv) write(t *testing.T) {
	t.Helper()
	b, err := json.Marshal(e.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.CfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// set changes a nested key ("openai.model") and rewrites the config file.
func (e *testEnv) set(t *testing.T, path []string, v any) {
	t.Helper()
	m := e.Raw
	for _, k := range path[:len(path)-1] {
		m = m[k].(map[string]any)
	}
	if v == nil {
		delete(m, path[len(path)-1])
	} else {
		m[path[len(path)-1]] = v
	}
	e.write(t)
}

func (e *testEnv) loadOpts() LoadOptions {
	return LoadOptions{Trust: TrustOptions{OwnerOK: func(uint32) bool { return testOwnerAnyone }, StopAt: e.Dir}}
}

func (e *testEnv) load(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadConfigOpts(e.CfgPath, e.loadOpts())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// newServer builds a broker over the test config (state in the temp dir) and
// serves it on the config's socket. The returned log buffer receives all logs.
func (e *testEnv) newServer(t *testing.T, mod func(*Server)) (*Server, *syncBuf) {
	t.Helper()
	cfg := e.load(t)
	logs := &syncBuf{}
	s, err := New(cfg, Options{
		StateDir:   filepath.Join(e.Dir, "state"),
		Log:        logs,
		KeyOwnerOK: func(uint32) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mod != nil {
		mod(s)
	}
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s, logs
}

// roundTrip sends raw bytes and returns everything the server answers
// until it closes the connection.
func roundTrip(t *testing.T, sock string, raw []byte) []byte {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, testShortTimeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(testShortTimeout))
	go func() { c.Write(raw) }()
	out, _ := io.ReadAll(c)
	return out
}

type reply struct {
	OK      bool            `json:"ok"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

func call(t *testing.T, sock, line string) (reply, []byte) {
	t.Helper()
	out := roundTrip(t, sock, []byte(line+"\n"))
	var r reply
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil {
		t.Fatalf("reply %q is not JSON: %v", out, err)
	}
	return r, out
}

func withUIDs(c Config, u ...uint32) Config { c.uids = u; return c }
