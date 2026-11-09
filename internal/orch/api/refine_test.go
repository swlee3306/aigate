package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/refine"
)

// fakeBroker is a stand-in orch-broker on a temp unix socket. It records
// every call; nothing here reaches any real service.
type fakeBroker struct {
	mu         sync.Mutex
	repos      []string
	statusN    int
	refines    []fakeRefineCall
	statusFail bool
	// refineReply, when set, is the raw reply line for a refine call.
	refineReply func(call fakeRefineCall) string
}

type fakeRefineCall struct {
	Repo        string `json:"repo"`
	Requirement string `json:"requirement"`
	Selection   string `json:"selection"`
	Files       []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
}

func (f *fakeBroker) refineCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refines)
}

func (f *fakeBroker) lastRefine(t *testing.T) fakeRefineCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.refines) == 0 {
		t.Fatal("the broker received no refine call")
	}
	return f.refines[len(f.refines)-1]
}

func (f *fakeBroker) setRepos(r ...string) {
	f.mu.Lock()
	f.repos = r
	f.mu.Unlock()
}

func startFakeBroker(t *testing.T) (*fakeBroker, string) {
	t.Helper()
	f := &fakeBroker{}
	sock := filepath.Join(shortDir(t), "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f, sock
}

func (f *fakeBroker) serve(c net.Conn) {
	defer c.Close()
	line, err := bufio.NewReaderSize(c, 1<<20).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		V    int             `json:"v"`
		Op   string          `json:"op"`
		Args json.RawMessage `json:"args"`
	}
	if json.Unmarshal(line, &req) != nil || req.V != 1 {
		c.Write([]byte("{\"ok\":false,\"code\":\"bad_request\",\"message\":\"x\"}\n"))
		return
	}
	switch req.Op {
	case "status":
		f.mu.Lock()
		f.statusN++
		fail, repos := f.statusFail, append([]string{}, f.repos...)
		f.mu.Unlock()
		if fail {
			c.Write([]byte("{\"ok\":false,\"code\":\"internal_error\",\"message\":\"SECRET-BROKER-TEXT\"}\n"))
			return
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"context_repos": repos,
			"refine_remaining_hour": 20, "gitlab_configured": false, "mr_projects": []string{},
			"weekly_budget_remaining_usd": 25.0}})
		c.Write(append(b, '\n'))
	case "refine":
		var call fakeRefineCall
		_ = json.Unmarshal(req.Args, &call)
		f.mu.Lock()
		f.refines = append(f.refines, call)
		fn := f.refineReply
		f.mu.Unlock()
		if fn != nil {
			c.Write([]byte(fn(call) + "\n"))
			return
		}
		c.Write([]byte(okPlan(call, nil) + "\n"))
	default:
		c.Write([]byte("{\"ok\":false,\"code\":\"unknown_op\",\"message\":\"x\"}\n"))
	}
}

// okPlan is a valid broker reply; mod may change the plan before encoding.
func okPlan(call fakeRefineCall, mod func(plan map[string]any)) string {
	files := []string{}
	if len(call.Files) > 0 {
		files = append(files, call.Files[0].Path)
	}
	plan := map[string]any{"summary": "S", "items": []map[string]any{{"id": "r1", "title": "T", "description": "D", "files": files}},
		"questions": []any{}, "out_of_scope": []string{},
		"usage": map[string]int{"input_tokens": 1200, "output_tokens": 300}}
	if mod != nil {
		mod(plan)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "result": plan})
	return string(b)
}

func brokerErrReply(code string) string {
	return fmt.Sprintf("{\"ok\":false,\"code\":%q,\"message\":\"SECRET-BROKER-TEXT sk-live-abc http://evil.example\"}", code)
}

// refineEnv is an env with a fake broker, a gate home for the ledger and a
// controllable clock.
type refineEnv struct {
	*env
	fb   *fakeBroker
	home string
	clk  *testClock
}

func newRefineEnv(t *testing.T, files map[string]string) *refineEnv {
	t.Helper()
	fb, sock := startFakeBroker(t)
	home := t.TempDir()
	e := newEnv(t, noSteps{}, 1, func(c *Config) { c.BrokerSocket = sock; c.GateHome = home })
	e.d.pauseWorkers()
	re := &refineEnv{env: e, fb: fb, home: home, clk: &testClock{}}
	e.d.handler.now = re.clk.now
	fb.setRepos(e.repo)
	re.commit(t, files)
	return re
}

func (e *refineEnv) commit(t *testing.T, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(e.repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "--allow-empty", "-m", "c")
}

type caller func(path, body string) (int, []byte)

func (e *refineEnv) socket(t *testing.T) caller {
	return func(path, body string) (int, []byte) { c, _, b := e.sockDo(t, "POST", path, body, nil); return c, b }
}

func (e *refineEnv) token(t *testing.T, name string) caller {
	srv := e.httpServer(t)
	tok := e.newToken(t, name)
	return func(path, body string) (int, []byte) {
		c, _, b := httpDo(t, srv, "POST", path, body, hdr{"Authorization": "Bearer " + tok})
		return c, b
	}
}

func jsonBody(v any) string { b, _ := json.Marshal(v); return string(b) }

type previewOut struct {
	Allowed     bool               `json:"allowed"`
	SelectionID string             `json:"selection_id"`
	Files       []refine.FileEntry `json:"files"`
	Excluded    []refine.Excluded  `json:"excluded"`
	TotalBytes  int                `json:"total_bytes"`
}

func (e *refineEnv) preview(t *testing.T, call caller, text string) previewOut {
	t.Helper()
	code, b := call("/api/v1/refine/preview", jsonBody(map[string]string{"repo": e.repo, "text": text}))
	if code != 200 {
		t.Fatalf("preview %d %s", code, b)
	}
	var out previewOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *refineEnv) refine(call caller, text, sel string) (int, []byte) {
	return call("/api/v1/refine", jsonBody(map[string]string{"repo": e.repo, "text": text, "selection_id": sel}))
}

var baseFiles = map[string]string{
	"README.md":     "# proj\nreadme body\n",
	"src/health.go": "package src // health FILE-MARKER-health\n",
	"src/other.go":  "package src // other\n",
}

func TestPreviewReportsRepoNotAllowed(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	e.fb.setRepos("/somewhere/else")
	sock := e.socket(t)
	code, b := sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": e.repo, "text": "add a health check"}))
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var out previewOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Allowed || out.SelectionID != "" || len(out.Files) != 0 || len(out.Excluded) != 0 || out.TotalBytes != 0 {
		t.Fatalf("%s", b)
	}
	for _, leak := range []string{"FILE-MARKER", "readme body", "health.go", "README"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("%q in an off-list preview: %s", leak, b)
		}
	}
	if e.d.handler.refineStore != nil {
		if _, ok := e.d.handler.refineStore.Get("", "x"); ok {
			t.Fatal("selection stored")
		}
	}
	// And a refine with an id from nowhere cannot be sent.
	if code, _ := e.refine(sock, "add a health check", "deadbeef"); code != 410 || e.fb.refineCount() != 0 {
		t.Fatalf("refine %d, refine calls %d", code, e.fb.refineCount())
	}
	// Allowed again: the same request shows files.
	e.fb.setRepos(e.repo)
	if p := e.preview(t, sock, "add a health check"); !p.Allowed || p.SelectionID == "" || len(p.Files) == 0 {
		t.Fatalf("%+v", p)
	}
	// A repository outside the allowed roots is refused before the broker is asked.
	n := e.fb.statusN
	code, b = sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": "/etc", "text": "add a health check"}))
	if code != 403 || v1Code(b) != "repo_not_allowed" || e.fb.statusN != n {
		t.Fatalf("%d %s", code, b)
	}
}

func TestRefineSendsExactlyThePreviewedSelection(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	sock := e.socket(t)
	// The work tree differs from HEAD: only HEAD may be sent.
	if err := os.WriteFile(filepath.Join(e.repo, "src/health.go"), []byte("WORKTREE-ONLY"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "src/health_new.go"), []byte("UNTRACKED"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := "add a health check\r\nsecond line"
	p := e.preview(t, sock, text)
	if !p.Allowed || len(p.Files) != 2 || p.Files[0].Path != "README.md" || p.Files[1].Path != "src/health.go" {
		t.Fatalf("%+v", p)
	}
	head := git(t, e.repo, "rev-parse", "HEAD")
	var want []refine.File
	for _, f := range p.Files {
		want = append(want, refine.File{Path: f.Path, Content: git(t, e.repo, "show", "HEAD:"+f.Path) + "\n"})
	}
	code, b := e.refine(sock, text, p.SelectionID)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	call := e.fb.lastRefine(t)
	var got []refine.File
	for _, f := range call.Files {
		got = append(got, refine.File{Path: f.Path, Content: f.Content})
	}
	if refine.ContentHash(head, got) != refine.ContentHash(head, want) || len(got) != len(p.Files) {
		t.Fatalf("sent %+v, previewed %+v", got, want)
	}
	for _, f := range got {
		if strings.Contains(f.Content, "WORKTREE-ONLY") || strings.Contains(f.Content, "UNTRACKED") {
			t.Fatalf("work tree content sent: %q", f.Content)
		}
	}
	if call.Requirement != "add a health check\nsecond line" || call.Repo != e.repo || call.Selection != p.SelectionID {
		t.Fatalf("call %+v", call)
	}
	// The plan and the usage come back as the broker produced them.
	var plan planResponse
	if err := json.Unmarshal(b, &plan); err != nil || plan.Summary != "S" || len(plan.Items) != 1 ||
		plan.Items[0].Files[0] != "README.md" || plan.Usage.InputTokens != 1200 || plan.Usage.OutputTokens != 300 {
		t.Fatalf("%s", b)
	}
	if strings.Contains(string(b), "FILE-MARKER") {
		t.Fatal("file content in the plan response")
	}
}

func TestRefineSelectionChangedIs409(t *testing.T) {
	text := "add a health check"
	for name, change := range map[string]func(e *refineEnv){
		"file changed": func(e *refineEnv) { e.commit(t, map[string]string{"src/health.go": "package src // changed\n"}) },
		"head moved":   func(e *refineEnv) { git(t, e.repo, "commit", "-q", "--allow-empty", "-m", "empty") },
		"file deleted": func(e *refineEnv) {
			git(t, e.repo, "rm", "-q", "src/health.go")
			git(t, e.repo, "commit", "-q", "-m", "rm")
		},
		"became a secret": func(e *refineEnv) {
			e.commit(t, map[string]string{"src/health.go": "token = \"abcdefghijklmnop1234\"\n"})
		},
	} {
		e := newRefineEnv(t, baseFiles)
		sock := e.socket(t)
		p := e.preview(t, sock, text)
		change(e)
		code, b := e.refine(sock, text, p.SelectionID)
		if code != 409 || v1Code(b) != "selection_changed" {
			t.Errorf("%s: %d %s", name, code, b)
		}
		if n := e.fb.refineCount(); n != 0 {
			t.Errorf("%s: the broker was called %d times", name, n)
		}
	}
	// A different requirement text than the one previewed is a different request.
	e := newRefineEnv(t, baseFiles)
	sock := e.socket(t)
	p := e.preview(t, sock, text)
	code, b := e.refine(sock, "something else entirely", p.SelectionID)
	if code != 409 || v1Code(b) != "selection_changed" || e.fb.refineCount() != 0 {
		t.Fatalf("text changed: %d %s", code, b)
	}
	// Unchanged: it goes through.
	if code, b := e.refine(sock, text, p.SelectionID); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
}

func TestRefineSelectionExpired(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	sock := e.socket(t)
	text := "add a health check"
	p := e.preview(t, sock, text)
	e.clk.advance(refine.TTL - time.Second)
	if code, b := e.refine(sock, text, p.SelectionID); code != 200 {
		t.Fatalf("before the TTL: %d %s", code, b)
	}
	e.clk.advance(2 * time.Second)
	code, b := e.refine(sock, text, p.SelectionID)
	if code != 410 || v1Code(b) != "selection_expired" {
		t.Fatalf("%d %s", code, b)
	}
	if n := e.fb.refineCount(); n != 1 {
		t.Fatalf("broker refine calls %d, want 1", n)
	}
	// Another actor's id is refused like an unknown one.
	p2 := e.preview(t, sock, text)
	other := e.token(t, "mallory")
	if code, b := e.refine(other, text, p2.SelectionID); code != 410 || v1Code(b) != "selection_expired" {
		t.Fatalf("other actor: %d %s", code, b)
	}
}

func TestRefineRequiresWriteTier(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	srv := e.httpServer(t)
	body := jsonBody(map[string]string{"repo": e.repo, "text": "add a health check", "selection_id": "x"})
	for _, path := range []string{"/api/v1/refine/preview", "/api/v1/refine"} {
		if code, _, b := httpDo(t, srv, "POST", path, body, nil); code != 401 {
			t.Errorf("%s anonymous: %d %s", path, code, b)
		}
		tok, csrf := e.newSession(t, "alice")
		if code, _, b := httpDo(t, srv, "POST", path, body, hdr{"Cookie": cookie(tok)}); code != 403 {
			t.Errorf("%s session without CSRF: %d %s", path, code, b)
		}
		if code, _, b := httpDo(t, srv, "POST", path, body, hdr{"Cookie": cookie(tok), CSRFHeader: "wrong"}); code != 403 {
			t.Errorf("%s session with a wrong CSRF: %d %s", path, code, b)
		}
		// With the CSRF header the request reaches the handler.
		code, _, b := httpDo(t, srv, "POST", path, body, hdr{"Cookie": cookie(tok), CSRFHeader: csrf})
		if code == 401 || code == 403 {
			t.Errorf("%s session + CSRF refused: %d %s", path, code, b)
		}
	}
	if n := e.fb.refineCount(); n != 0 {
		t.Fatalf("refused requests reached the broker: %d", n)
	}
	// GET is not a route.
	if code, _, _ := e.sockDo(t, "GET", "/api/v1/refine", "", nil); code != 405 && code != 404 {
		t.Fatalf("GET: %d", code)
	}
}

func TestRefinePerActorRateLimit(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	a, b := e.token(t, "alice"), e.socket(t)
	text := "add a health check"
	pa, pb := e.preview(t, a, text), e.preview(t, b, text)
	for i := 0; i < RefinePerActorHour; i++ {
		if code, body := e.refine(a, text, pa.SelectionID); code != 200 {
			t.Fatalf("alice %d: %d %s", i, code, body)
		}
	}
	code, body := e.refine(a, text, pa.SelectionID)
	if code != 429 || v1Code(body) != "refine_rate_limited" {
		t.Fatalf("alice over the limit: %d %s", code, body)
	}
	if n := e.fb.refineCount(); n != RefinePerActorHour {
		t.Fatalf("broker calls %d, want %d", n, RefinePerActorHour)
	}
	// The other actor is not affected.
	for i := 0; i < RefinePerActorHour; i++ {
		if code, body := e.refine(b, text, pb.SelectionID); code != 200 {
			t.Fatalf("socket actor %d: %d %s", i, code, body)
		}
	}
	if code, _ := e.refine(b, text, pb.SelectionID); code != 429 {
		t.Fatalf("socket actor over the limit: %d", code)
	}
	// The window slides: an hour later alice may ask again.
	e.clk.advance(time.Hour + time.Second)
	pa = e.preview(t, a, text)
	if code, body := e.refine(a, text, pa.SelectionID); code != 200 {
		t.Fatalf("after the window: %d %s", code, body)
	}
}

func TestRefineErrorsAreFixedAndNeverEchoBrokerText(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	e.d.handler.refineLimit = newActorLimiter(1000, time.Hour)
	sock := e.socket(t)
	text := "add a health check"
	p := e.preview(t, sock, text)
	cases := []struct {
		brokerCode string
		status     int
		code       string
	}{
		{"repo_not_allowed", 403, "repo_not_allowed"},
		{"budget_exceeded", 429, "budget_exceeded"},
		{"rate_limited", 429, "refine_rate_limited"},
		{"upstream_error", 502, "upstream_error"},
		{"bad_plan", 502, "bad_plan"},
		{"timeout", 502, "upstream_error"},
		{"internal_error", 502, "upstream_error"},
		{"totally_unknown", 502, "upstream_error"},
		{"looks_like_secret", 400, "bad_request"},
		{"too_large", 400, "bad_request"},
		{"bad_file", 400, "bad_request"},
		{"busy", 503, "broker_unavailable"},
		{"forbidden", 503, "broker_unavailable"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		c := c
		e.fb.mu.Lock()
		e.fb.refineReply = func(fakeRefineCall) string { return brokerErrReply(c.brokerCode) }
		e.fb.mu.Unlock()
		code, b := e.refine(sock, text, p.SelectionID)
		if code != c.status || v1Code(b) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.brokerCode, code, b, c.status, c.code)
		}
		for _, leak := range []string{"SECRET-BROKER-TEXT", "sk-live", "evil.example", "totally_unknown"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s: %q echoed: %s", c.brokerCode, leak, b)
			}
		}
		var m struct{ Error struct{ Message string } }
		_ = json.Unmarshal(b, &m)
		if m.Error.Message == "" {
			t.Errorf("%s: no message: %s", c.brokerCode, b)
		}
		if prev, ok := seen[c.code]; ok && c.code != "bad_request" && prev != m.Error.Message {
			t.Errorf("code %s has two sentences: %q / %q", c.code, prev, m.Error.Message)
		}
		seen[c.code] = m.Error.Message
	}
	// A broker whose "ok" plan breaks the contract: bad_plan, and what it said is not shown.
	bad := map[string]func(plan map[string]any){
		"unknown field": func(pl map[string]any) { pl["extra"] = "EVIL-TEXT" },
		"file not sent": func(pl map[string]any) {
			pl["items"] = []map[string]any{{"id": "r1", "title": "T", "description": "D", "files": []string{"/etc/passwd"}}}
		},
		"no items": func(pl map[string]any) { pl["items"] = []any{} },
		"long title": func(pl map[string]any) {
			pl["items"] = []map[string]any{{"id": "r1", "title": strings.Repeat("t", 121), "description": "D", "files": []string{}}}
		},
		"31 items":       func(pl map[string]any) { pl["items"] = make31() },
		"long summary":   func(pl map[string]any) { pl["summary"] = strings.Repeat("s", 601) },
		"11 questions":   func(pl map[string]any) { pl["questions"] = make([]map[string]string, 11) },
		"negative usage": func(pl map[string]any) { pl["usage"] = map[string]int{"input_tokens": -1, "output_tokens": 1} },
		"item extra": func(pl map[string]any) {
			pl["items"] = []map[string]any{{"id": "r1", "title": "T", "description": "D", "files": []string{}, "x": "EVIL-TEXT"}}
		},
	}
	for name, mod := range bad {
		mod := mod
		e.fb.mu.Lock()
		e.fb.refineReply = func(call fakeRefineCall) string { return okPlan(call, mod) }
		e.fb.mu.Unlock()
		code, b := e.refine(sock, text, p.SelectionID)
		if code != 502 || v1Code(b) != "bad_plan" || strings.Contains(string(b), "EVIL-TEXT") || strings.Contains(string(b), "/etc/passwd") {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// Garbage instead of JSON, and a broker that is gone.
	e.fb.mu.Lock()
	e.fb.refineReply = func(fakeRefineCall) string { return "SECRET-BROKER-TEXT not json" }
	e.fb.mu.Unlock()
	if code, b := e.refine(sock, text, p.SelectionID); code != 503 || v1Code(b) != "broker_unavailable" || strings.Contains(string(b), "SECRET") {
		t.Errorf("garbage: %d %s", code, b)
	}
	e.fb.mu.Lock()
	e.fb.statusFail = true
	e.fb.mu.Unlock()
	code, b := sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": e.repo, "text": text}))
	if code != 502 || strings.Contains(string(b), "SECRET") {
		t.Errorf("status failing: %d %s", code, b)
	}
}

func make31() []map[string]any {
	out := make([]map[string]any, 31)
	for i := range out {
		out[i] = map[string]any{"id": "r1", "title": "T", "description": "D", "files": []string{}}
	}
	return out
}

func TestRefineLedgerEventHasNoContent(t *testing.T) {
	e := newRefineEnv(t, map[string]string{
		"README.md":     "# proj\nLEDGER-README-CONTENT\n",
		"src/health.go": "package src // LEDGER-FILE-CONTENT\n",
	})
	sock := e.socket(t)
	text := "add a health check LEDGER-REQ-MARKER"
	p := e.preview(t, sock, text)
	if code, b := e.refine(sock, text, p.SelectionID); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	e.fb.mu.Lock()
	e.fb.refineReply = func(fakeRefineCall) string { return brokerErrReply("upstream_error") }
	e.fb.mu.Unlock()
	if code, _ := e.refine(sock, text, p.SelectionID); code != 502 {
		t.Fatalf("%d", code)
	}
	lp := policy.OrchLedgerPath(e.home)
	raw, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"LEDGER-REQ-MARKER", "LEDGER-FILE-CONTENT", "LEDGER-README-CONTENT", "add a health check", "SECRET-BROKER-TEXT", "health.go"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q in the ledger:\n%s", leak, raw)
		}
	}
	evs, err := ledger.Events(lp)
	if err != nil {
		t.Fatal(err)
	}
	var refines []ledger.Event
	for _, ev := range evs {
		if ev.Type == "refine" {
			refines = append(refines, ev)
		}
	}
	if len(refines) != 2 {
		t.Fatalf("%d refine events:\n%s", len(refines), raw)
	}
	ok, failed := refines[0].Payload, refines[1].Payload
	wantBytes := len("# proj\nLEDGER-README-CONTENT\n") + len("package src // LEDGER-FILE-CONTENT\n")
	str := func(v any) string { return fmt.Sprint(v) } // numbers decode as json.Number
	if ok["actor"] != socketActor() || ok["repo"] != e.repo || str(ok["files"]) != "2" || str(ok["bytes"]) != str(wantBytes) ||
		str(ok["input_tokens"]) != "1200" || str(ok["output_tokens"]) != "300" || ok["ok"] != true {
		t.Fatalf("ok event %+v", ok)
	}
	if failed["ok"] != false || str(failed["files"]) != "2" || str(failed["input_tokens"]) != "0" {
		t.Fatalf("failed event %+v", failed)
	}
	for _, k := range []string{"text", "requirement", "content"} {
		if _, has := ok[k]; has {
			t.Fatalf("event has %q", k)
		}
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

func TestRefineRejectsBadInputWithoutTheBroker(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	sock := e.socket(t)
	valid := "add a health check"
	cases := map[string]string{
		"empty text":       jsonBody(map[string]string{"repo": e.repo, "text": "  "}),
		"too long":         jsonBody(map[string]string{"repo": e.repo, "text": strings.Repeat("a", 2001)}),
		"NUL":              jsonBody(map[string]string{"repo": e.repo, "text": "a\x00b"}),
		"escape":           jsonBody(map[string]string{"repo": e.repo, "text": "a\x1bb"}),
		"bidi":             jsonBody(map[string]string{"repo": e.repo, "text": "a\u202eb"}),
		"bare CR":          jsonBody(map[string]string{"repo": e.repo, "text": "a\rb"}),
		"secret shaped":    jsonBody(map[string]string{"repo": e.repo, "text": "use password = \"hunter2hunter2hunter2\" here"}),
		"unknown field":    jsonBody(map[string]string{"repo": e.repo, "text": valid, "files": "x"}),
		"not json":         "{",
		"trailing data":    jsonBody(map[string]string{"repo": e.repo, "text": valid}) + " {}",
		"relative repo":    jsonBody(map[string]string{"repo": "proj", "text": valid}),
		"unclean repo":     jsonBody(map[string]string{"repo": e.repo + "/../proj", "text": valid}),
		"repo not git":     jsonBody(map[string]string{"repo": e.root, "text": valid}),
		"invalid utf8 raw": "{\"repo\":\"" + e.repo + "\",\"text\":\"a\xffb\"}",
	}
	for name, body := range cases {
		for _, path := range []string{"/api/v1/refine/preview", "/api/v1/refine"} {
			code, b := sock(path, body)
			if code != 400 || v1Code(b) != "bad_request" {
				t.Errorf("%s %s: %d %s", name, path, code, b)
			}
		}
	}
	if code, b := sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": "/etc", "text": valid})); code != 403 {
		t.Errorf("outside roots: %d %s", code, b)
	}
	if e.fb.refineCount() != 0 {
		t.Fatal("bad input reached the broker")
	}
	// A selection made for another repository cannot be used for this one.
	p := e.preview(t, sock, valid)
	other := filepath.Join(e.root, "proj2")
	mkRepo(t, other)
	e.fb.setRepos(e.repo, other)
	code, b := sock("/api/v1/refine", jsonBody(map[string]string{"repo": other, "text": valid, "selection_id": p.SelectionID}))
	if code != 400 || e.fb.refineCount() != 0 {
		t.Errorf("selection of another repo: %d %s", code, b)
	}
}

func TestRefineOffWithoutBrokerSocket(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	e.d.pauseWorkers()
	body := jsonBody(map[string]string{"repo": e.repo, "text": "add a health check"})
	full := jsonBody(map[string]string{"repo": e.repo, "text": "add a health check", "selection_id": "x"})
	for path, b0 := range map[string]string{"/api/v1/refine/preview": body, "/api/v1/refine": full} {
		code, b := e.do(t, "POST", path, b0)
		if code != 503 || v1Code(b) != "broker_unavailable" {
			t.Errorf("%s: %d %s", path, code, b)
		}
	}
	// The rest of orchd is unaffected.
	if code, b := e.do(t, "GET", "/api/v1/repos", ""); code != 200 {
		t.Fatalf("repos %d %s", code, b)
	}
	if code, b := e.submit(t, e.repo, "HEAD", "p", "docs", "claude"); code != 201 {
		t.Fatalf("submit %d %s", code, b)
	}
	// A configured but absent broker socket is the same fixed error.
	e2 := newEnv(t, noSteps{}, 1, func(c *Config) { c.BrokerSocket = "/tmp/orchd-no-such-broker.sock" })
	if code, b := e2.do(t, "POST", "/api/v1/refine/preview", jsonBody(map[string]string{"repo": e2.repo, "text": "add a health check"})); code != 503 || v1Code(b) != "broker_unavailable" || strings.Contains(string(b), "no-such") {
		t.Fatalf("%d %s", code, b)
	}
	// A relative socket path is refused when the daemon is built.
	st := e.st
	if _, err := NewDaemon(Config{Socket: filepath.Join(shortDir(t), "x.sock"), BrokerSocket: "relative.sock"}, st, noSteps{}); err == nil {
		t.Fatal("relative broker socket accepted")
	}
}

func TestPreviewIsMeteredPerActor(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	a, b := e.token(t, "alice"), e.socket(t)
	text := "add a health check"
	body := jsonBody(map[string]string{"repo": e.repo, "text": text})
	for i := 0; i < PreviewPerActorHour; i++ {
		if code, out := a("/api/v1/refine/preview", body); code != 200 {
			t.Fatalf("alice %d: %d %s", i, code, out)
		}
	}
	code, out := a("/api/v1/refine/preview", body)
	if code != 429 || v1Code(out) != "refine_rate_limited" {
		t.Fatalf("alice over: %d %s", code, out)
	}
	if code, out := b("/api/v1/refine/preview", body); code != 200 {
		t.Fatalf("other actor: %d %s", code, out)
	}
	e.clk.advance(time.Hour + time.Second)
	if code, out := a("/api/v1/refine/preview", body); code != 200 {
		t.Fatalf("after the window: %d %s", code, out)
	}
}

func TestPreviewFloodDoesNotEvictAnotherActorsSelection(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	e.d.handler.refineLimit = newActorLimiter(1000, time.Hour)
	e.d.handler.previewLimit = newActorLimiter(1000, time.Hour)
	a, b := e.token(t, "alice"), e.socket(t)
	text := "add a health check"
	pb := e.preview(t, b, text)
	for i := 0; i < refine.MaxHeld+10; i++ {
		e.preview(t, a, text)
	}
	if code, out := e.refine(b, text, pb.SelectionID); code != 200 {
		t.Fatalf("bob's selection was lost: %d %s", code, out)
	}
	// Fill the store with other actors: a newcomer's preview is refused (429), nobody is evicted.
	for i := 0; ; i++ {
		if _, err := e.d.handler.refineStore.Put(fmt.Sprintf("fill:%d", i), e.repo, text, refine.Selection{Hash: "h"}); err != nil {
			break
		}
		if i > 100 {
			t.Fatal("store never filled")
		}
	}
	c := e.token(t, "carol")
	code, out := c("/api/v1/refine/preview", jsonBody(map[string]string{"repo": e.repo, "text": text}))
	if code != 429 || v1Code(out) != "refine_rate_limited" {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := e.refine(b, text, pb.SelectionID); code != 200 {
		t.Fatalf("bob after the store filled: %d %s", code, out)
	}
}

func TestRefineSlotRefundOnlyWithoutUpstreamCost(t *testing.T) {
	text := "add a health check"
	for _, tc := range []struct {
		code   string
		refund bool
	}{{"busy", true}, {"forbidden", true}, {"upstream_error", false}, {"timeout", false}, {"bad_plan", false}, {"budget_exceeded", false}} {
		e := newRefineEnv(t, baseFiles)
		a := e.token(t, "alice")
		p := e.preview(t, a, text)
		tc := tc
		e.fb.mu.Lock()
		e.fb.refineReply = func(fakeRefineCall) string { return brokerErrReply(tc.code) }
		e.fb.mu.Unlock()
		for i := 0; i < RefinePerActorHour+3; i++ {
			code, b := e.refine(a, text, p.SelectionID)
			if tc.refund && code == 429 {
				t.Fatalf("%s: slot not refunded, attempt %d: %s", tc.code, i, b)
			}
			if !tc.refund && i >= RefinePerActorHour && !(code == 429 && v1Code(b) == "refine_rate_limited") {
				t.Fatalf("%s: attempt %d should be limited: %d %s", tc.code, i, code, b)
			}
		}
		if tc.refund {
			e.fb.mu.Lock()
			e.fb.refineReply = nil
			e.fb.mu.Unlock()
			if code, b := e.refine(a, text, p.SelectionID); code != 200 {
				t.Fatalf("%s: after refunds %d %s", tc.code, code, b)
			}
		}
	}
}

func TestParsePlanRejectsHostileTextIdsAndUsage(t *testing.T) {
	sent := map[string]bool{"README.md": true}
	mk := func(mod func(pl map[string]any)) json.RawMessage {
		pl := map[string]any{"summary": "S", "items": []map[string]any{{"id": "r1", "title": "T", "description": "D", "files": []string{"README.md"}}},
			"questions": []map[string]string{{"id": "q1", "text": "Q"}}, "out_of_scope": []string{"O"},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5}}
		mod(pl)
		b, _ := json.Marshal(pl)
		return b
	}
	item := func(k, v string) func(pl map[string]any) {
		return func(pl map[string]any) {
			it := map[string]any{"id": "r1", "title": "T", "description": "D", "files": []string{}}
			it[k] = v
			pl["items"] = []map[string]any{it}
		}
	}
	if _, err := parsePlan(mk(func(map[string]any) {}), sent); err != nil {
		t.Fatalf("good plan refused: %v", err)
	}
	// Newlines are fine in summary and description only.
	if _, err := parsePlan(mk(func(pl map[string]any) { pl["summary"] = "a\nb"; item("description", "x\ny")(pl) }), sent); err != nil {
		t.Fatalf("newline refused: %v", err)
	}
	bad := map[string]func(pl map[string]any){
		"bidi title":       item("title", "a\u202eb"),
		"zero width title": item("title", "a\u200bb"),
		"control desc":     item("description", "a\x07b"),
		"esc desc":         item("description", "a\x1b[31mb"),
		"newline title":    item("title", "a\nb"),
		"bidi summary":     func(pl map[string]any) { pl["summary"] = "a\u2066b" },
		"control summary":  func(pl map[string]any) { pl["summary"] = "a\x00b" },
		"bidi question":    func(pl map[string]any) { pl["questions"] = []map[string]string{{"id": "q1", "text": "a\u202eb"}} },
		"newline question": func(pl map[string]any) { pl["questions"] = []map[string]string{{"id": "q1", "text": "a\nb"}} },
		"bidi out":         func(pl map[string]any) { pl["out_of_scope"] = []string{"a\u200fb"} },
		"newline out":      func(pl map[string]any) { pl["out_of_scope"] = []string{"a\nb"} },
		"item id word":     item("id", "x1"),
		"item id html":     item("id", "r1<script>"),
		"item id 3 digits": item("id", "r100"),
		"item id empty":    item("id", ""),
		"q id":             func(pl map[string]any) { pl["questions"] = []map[string]string{{"id": "q-1", "text": "Q"}} },
		"q id r":           func(pl map[string]any) { pl["questions"] = []map[string]string{{"id": "r1", "text": "Q"}} },
		"usage huge":       func(pl map[string]any) { pl["usage"] = map[string]int{"input_tokens": 10_000_001, "output_tokens": 1} },
		"usage out huge":   func(pl map[string]any) { pl["usage"] = map[string]int{"input_tokens": 1, "output_tokens": 1 << 40} },
	}
	for name, mod := range bad {
		if _, err := parsePlan(mk(mod), sent); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := parsePlan(mk(func(pl map[string]any) { pl["usage"] = map[string]int{"input_tokens": 10_000_000, "output_tokens": 0} }), sent); err != nil {
		t.Errorf("usage at the bound refused: %v", err)
	}
}
