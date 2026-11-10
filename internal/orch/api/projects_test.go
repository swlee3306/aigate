package api

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/registrymsg"
)

// regBroker is a stand-in orch-broker for the registry ops, on a temp unix
// socket. It records every call. Nothing here reaches a real service.
type regBroker struct {
	mu       sync.Mutex
	calls    []regCall
	projects []map[string]any
	// reply, when set, is the raw reply line for a write op ("" = the default
	// success; "DROP" = close the connection without a reply).
	reply func(op string, args json.RawMessage) string
	// listFail, when set, is the code the projects op answers with.
	listFail string
	// Token ops (W3b): the rows `tokens` answers with, an optional code for
	// that read, and an optional raw reply for set_token/delete_token.
	tokenRows  []map[string]any
	tokensFail string
	tokenReply func(op string, args json.RawMessage) string
}

type regCall struct {
	Op   string
	Args json.RawMessage
}

func (b *regBroker) ops() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, c := range b.calls {
		out = append(out, c.Op)
	}
	return out
}

func (b *regBroker) writes() []regCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []regCall
	for _, c := range b.calls {
		if c.Op == "register_project" || c.Op == "set_flag" {
			out = append(out, c)
		}
	}
	return out
}

func startRegBroker(t *testing.T) (*regBroker, string) {
	t.Helper()
	b := &regBroker{}
	sock := filepath.Join(shortDir(t), "r.sock")
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
			go b.serve(c)
		}
	}()
	return b, sock
}

func (b *regBroker) serve(c net.Conn) {
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
		c.Write([]byte("{\"ok\":false,\"code\":\"bad_request\"}\n"))
		return
	}
	b.mu.Lock()
	b.calls = append(b.calls, regCall{Op: req.Op, Args: req.Args})
	projects := append([]map[string]any{}, b.projects...)
	fn, listFail := b.reply, b.listFail
	b.mu.Unlock()
	ok := func(v any) {
		out, _ := json.Marshal(map[string]any{"ok": true, "result": v})
		c.Write(append(out, '\n'))
	}
	switch req.Op {
	case "projects":
		if listFail != "" {
			c.Write([]byte(brokerErrReply(listFail) + "\n"))
			return
		}
		if projects == nil {
			projects = []map[string]any{}
		}
		ok(map[string]any{"projects": projects, "providers": []any{},
			"auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5}})
	case "status":
		repos := []string{}
		for _, p := range projects {
			if p["ai_context"] == true {
				repos = append(repos, p["path"].(string))
			}
		}
		ok(map[string]any{"context_repos": repos, "refine_remaining_hour": 20, "gitlab_configured": false,
			"mr_projects": []string{}, "weekly_budget_remaining_usd": 25.0})
	case "tokens":
		b.mu.Lock()
		rows, tf := append([]map[string]any{}, b.tokenRows...), b.tokensFail
		b.mu.Unlock()
		if tf != "" {
			c.Write([]byte(brokerErrReply(tf) + "\n"))
			return
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		ok(map[string]any{"tokens": rows, "warn_days": 14})
	case "set_token", "delete_token":
		b.serveTokenWrite(c, req.Op, req.Args)
	case "register_project", "set_flag":
		if fn != nil {
			r := fn(req.Op, req.Args)
			if r == "DROP" {
				return
			}
			if r != "" {
				c.Write([]byte(r + "\n"))
				return
			}
		}
		var a struct {
			ID        string `json:"id"`
			Path      string `json:"path"`
			Kind      string `json:"kind"`
			AIContext bool   `json:"ai_context"`
			Value     bool   `json:"value"`
		}
		_ = json.Unmarshal(req.Args, &a)
		if req.Op == "set_flag" {
			a.AIContext, a.Path, a.Kind = a.Value, "/srv/x/"+a.ID, "docs"
		}
		ok(map[string]any{"id": a.ID, "path": a.Path, "kind": a.Kind, "ai_context": a.AIContext,
			"provider": nil, "gitlab": nil, "auto_merge": map[string]any{"enabled": false}})
	default:
		c.Write([]byte("{\"ok\":false,\"code\":\"unknown_op\"}\n"))
	}
}

type projEnv struct {
	*env
	rb      *regBroker
	home    string
	other   string // an unregistered repo under the root
	reg     string // a registered repo under the root
	outside string // a repo outside every root
}

func newProjEnv(t *testing.T) *projEnv {
	t.Helper()
	rb, sock := startRegBroker(t)
	home := t.TempDir()
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.BrokerSocket = sock
		c.GateHome = home
		c.Policy = func() PolicyView {
			return PolicyView{Hash: "h", Classes: map[string]string{"docs": "L1", "code": "L2", "버그수정": "L2"}}
		}
	})
	e.d.pauseWorkers()
	pe := &projEnv{env: e, rb: rb, home: home}
	pe.other = filepath.Join(e.root, "other")
	pe.reg = filepath.Join(e.root, "reg")
	pe.outside = filepath.Join(shortDir(t), "outside")
	mkRepo(t, pe.other)
	mkRepo(t, pe.reg)
	mkRepo(t, pe.outside)
	rb.projects = []map[string]any{{"id": "reg", "path": pe.reg, "kind": "docs", "ai_context": false,
		"provider": nil, "gitlab": nil, "auto_merge": map[string]any{"enabled": false}}}
	return pe
}

func (e *projEnv) sock(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	c, _, b := e.sockDo(t, method, path, body, nil)
	return c, b
}

type proposeOut struct {
	Action  string         `json:"action"`
	Subject string         `json:"subject"`
	Fields  map[string]any `json:"fields"`
	Message string         `json:"message"`
	Change  string         `json:"change"`
	Request string         `json:"request"`
	Issued  string         `json:"issued"`
	Preview []string       `json:"preview"`
}

func (e *projEnv) propose(t *testing.T, action, subject string, fields map[string]any) proposeOut {
	t.Helper()
	code, b := e.sock(t, "POST", "/api/v1/projects/propose", jsonBody(map[string]any{"action": action, "subject": subject, "fields": fields}))
	if code != 200 {
		t.Fatalf("propose %d %s", code, b)
	}
	var out proposeOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

var webauthnSig = map[string]any{"signer": "alice", "alg": "webauthn-es256", "sig": "c2ln",
	"webauthn": map[string]any{"authenticator_data": "YXV0aA", "client_data_json": "Y2xpZW50"}}

func applyBodyOf(p proposeOut, sig any) map[string]any {
	fields := map[string]any{}
	for k, v := range p.Fields {
		fields[k] = v
	}
	m := map[string]any{"action": p.Action, "subject": p.Subject, "fields": fields,
		"change": p.Change, "request": p.Request, "issued": p.Issued}
	if sig != nil {
		m["signature"] = sig
	}
	return m
}

func (e *projEnv) registerFields() map[string]any {
	return map[string]any{"path": e.other, "kind": "docs", "ai_context": false}
}

func TestProjectsListFromBroker(t *testing.T) {
	e := newProjEnv(t)
	code, b := e.sock(t, "GET", "/api/v1/projects/registry", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var got struct {
		Projects []struct {
			ID        string `json:"id"`
			Path      string `json:"path"`
			Kind      string `json:"kind"`
			AIContext bool   `json:"ai_context"`
		} `json:"projects"`
		AutoMergeGlobal struct {
			Enabled bool `json:"enabled"`
		} `json:"auto_merge_global"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "reg" || got.Projects[0].Path != e.reg || got.Projects[0].Kind != "docs" || got.Projects[0].AIContext {
		t.Fatalf("%s", b)
	}
	if ops := e.rb.ops(); len(ops) != 1 || ops[0] != "projects" {
		t.Fatalf("broker ops %v", ops)
	}
	// An empty registry is an empty array, never null.
	e.rb.mu.Lock()
	e.rb.projects = nil
	e.rb.mu.Unlock()
	_, b = e.sock(t, "GET", "/api/v1/projects/registry", "")
	if !strings.Contains(string(b), `"projects":[]`) {
		t.Fatalf("%s", b)
	}
	// A broken registry is a fixed registry_unavailable, no broker text.
	e.rb.mu.Lock()
	e.rb.listFail = "registry_unavailable"
	e.rb.mu.Unlock()
	code, b = e.sock(t, "GET", "/api/v1/projects/registry", "")
	if code != 503 || v1Code(b) != "registry_unavailable" || strings.Contains(string(b), "SECRET-BROKER-TEXT") {
		t.Fatalf("%d %s", code, b)
	}
	// The older task aggregation at /projects is untouched.
	if code, b := e.sock(t, "GET", "/api/v1/projects", ""); code != 200 || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("%d %s", code, b)
	}
}

func TestCandidatesExcludeRegisteredAndOutsideRoots(t *testing.T) {
	e := newProjEnv(t)
	// More shapes under the root: a symlink to a repo outside every root, a
	// symlink to a repo inside (shows once, as its own entry), a nested repo
	// (two levels down), a plain directory and a repository without commits.
	if err := os.Symlink(e.outside, filepath.Join(e.root, "linkout")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.other, filepath.Join(e.root, "alias")); err != nil {
		t.Fatal(err)
	}
	mkRepo(t, filepath.Join(e.root, "group", "deep"))
	if err := os.MkdirAll(filepath.Join(e.root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Join(e.root, "empty"), "init", "-q")
	code, b := e.sock(t, "GET", "/api/v1/projects/candidates", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var got struct {
		Candidates []struct {
			Path string `json:"path"`
			Head string `json:"head"`
		} `json:"candidates"`
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var paths []string
	for _, c := range got.Candidates {
		paths = append(paths, c.Path)
		if len(c.Head) < 7 {
			t.Errorf("no head for %s", c.Path)
		}
	}
	sort.Strings(paths)
	want := []string{e.other, e.repo}
	sort.Strings(want)
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("candidates %v, want %v\n%s", paths, want, b)
	}
	// Only class names a registry entry can carry are offered, sorted.
	if strings.Join(got.Kinds, ",") != "code,docs" {
		t.Fatalf("kinds %v", got.Kinds)
	}
	// The registered list is read from the broker.
	if ops := e.rb.ops(); len(ops) != 1 || ops[0] != "projects" {
		t.Fatalf("broker ops %v", ops)
	}
	for _, leak := range []string{e.outside, "deep", "linkout", "alias"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("%q listed: %s", leak, b)
		}
	}
	// Registering one removes it from the candidates.
	e.rb.mu.Lock()
	e.rb.projects = append(e.rb.projects, map[string]any{"id": "other", "path": e.other, "kind": "docs", "ai_context": false,
		"provider": nil, "gitlab": nil, "auto_merge": map[string]any{"enabled": false}})
	e.rb.mu.Unlock()
	_, b = e.sock(t, "GET", "/api/v1/projects/candidates", "")
	if strings.Contains(string(b), e.other) || !strings.Contains(string(b), e.repo) {
		t.Fatalf("%s", b)
	}
}

func TestProposeBuildsMessageAndStoresNothing(t *testing.T) {
	e := newProjEnv(t)
	fields := e.registerFields()
	p := e.propose(t, "register_project", "other", fields)
	wantChange, err := registrymsg.ChangeHash("register_project", "other", fields)
	if err != nil {
		t.Fatal(err)
	}
	if p.Change != wantChange {
		t.Fatalf("change %s, want %s", p.Change, wantChange)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(p.Request) {
		t.Fatalf("request %q", p.Request)
	}
	issued, err := time.Parse("2006-01-02T15:04:05Z", p.Issued)
	if err != nil || time.Since(issued) > time.Minute || time.Since(issued) < -time.Minute {
		t.Fatalf("issued %q %v", p.Issued, err)
	}
	m, err := signing.ParseRegistryMessage(p.Message)
	if err != nil {
		t.Fatalf("message not canonical: %v\n%s", err, p.Message)
	}
	if m.Action != "register_project" || m.Subject != "other" || m.Change != wantChange || m.Request != p.Request || m.Issued.Format("2006-01-02T15:04:05Z") != p.Issued {
		t.Fatalf("%+v", m)
	}
	if p.Fields["path"] != e.other || p.Fields["kind"] != "docs" || p.Fields["ai_context"] != false || p.Action != "register_project" || p.Subject != "other" {
		t.Fatalf("echo %+v", p)
	}
	if len(p.Preview) < 3 {
		t.Fatalf("preview %v", p.Preview)
	}
	all := strings.Join(p.Preview, "\n")
	for _, s := range p.Preview {
		if strings.TrimSpace(s) == "" {
			t.Fatalf("empty sentence in %v", p.Preview)
		}
	}
	for _, must := range []string{"other", e.other, "docs"} {
		if !strings.Contains(all, must) {
			t.Fatalf("preview lacks %q: %v", must, p.Preview)
		}
	}
	if strings.Contains(all, "[중요]") {
		t.Fatalf("ai_context off must not be emphasised: %v", p.Preview)
	}
	// ai_context ON is emphasised.
	on := e.propose(t, "register_project", "other", map[string]any{"path": e.other, "kind": "docs", "ai_context": true})
	if !strings.Contains(strings.Join(on.Preview, "\n"), "[중요]") || !strings.Contains(strings.Join(on.Preview, "\n"), "AI") {
		t.Fatalf("no emphasised AI sentence: %v", on.Preview)
	}
	flag := e.propose(t, "set_flag", "reg", map[string]any{"flag": "ai_context", "value": true})
	if !strings.Contains(strings.Join(flag.Preview, "\n"), "[중요]") || len(flag.Preview) < 2 {
		t.Fatalf("set_flag preview %v", flag.Preview)
	}
	off := e.propose(t, "set_flag", "reg", map[string]any{"flag": "ai_context", "value": false})
	if strings.Contains(strings.Join(off.Preview, "\n"), "[중요]") {
		t.Fatalf("turning it off is not emphasised: %v", off.Preview)
	}
	// Every proposal has its own one-time request id.
	if on.Request == p.Request || flag.Request == off.Request {
		t.Fatal("request id reused")
	}
	// Stateless: nothing reached the broker (neither a write nor a read).
	if ops := e.rb.ops(); len(ops) != 0 {
		t.Fatalf("propose called the broker: %v", ops)
	}
}

func TestProposeRejectsBadFieldsAndHostileText(t *testing.T) {
	e := newProjEnv(t)
	good := func() map[string]any {
		return map[string]any{"action": "register_project", "subject": "other",
			"fields": map[string]any{"path": e.other, "kind": "docs", "ai_context": false}}
	}
	with := func(k string, v any) map[string]any { m := good(); m[k] = v; return m }
	withField := func(k string, v any) map[string]any {
		m := good()
		m["fields"].(map[string]any)[k] = v
		return m
	}
	long := strings.Repeat("a", 5000)
	raw := map[string]string{
		"unknown top-level field": `{"action":"register_project","subject":"other","fields":{"path":"` + e.other + `","kind":"docs","ai_context":false},"extra":1}`,
		"capitalised key":         `{"Action":"register_project","subject":"other","fields":{"path":"` + e.other + `","kind":"docs","ai_context":false}}`,
		"capitalised field key":   `{"action":"register_project","subject":"other","fields":{"Path":"` + e.other + `","kind":"docs","ai_context":false}}`,
		"duplicate field":         `{"action":"register_project","subject":"other","fields":{"path":"` + e.other + `","path":"` + e.other + `","kind":"docs","ai_context":false}}`,
		"duplicate top key":       `{"action":"register_project","action":"set_flag","subject":"other","fields":{}}`,
		"trailing data":           `{"action":"register_project","subject":"other","fields":{"path":"` + e.other + `","kind":"docs","ai_context":false}} {}`,
		"not json":                `nope`,
		"empty":                   ``,
		"array":                   `[]`,
		"fields null":             `{"action":"register_project","subject":"other","fields":null}`,
		"ai_context as string":    `{"action":"register_project","subject":"other","fields":{"path":"` + e.other + `","kind":"docs","ai_context":"false"}}`,
		"number field":            `{"action":"register_project","subject":"other","fields":{"path":"` + e.other + `","kind":"docs","ai_context":false,"n":1}}`,
	}
	for name, body := range raw {
		if code, b := e.sock(t, "POST", "/api/v1/projects/propose", body); code != 400 {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	bodies := map[string]map[string]any{
		"unknown action":          with("action", "delete_project"),
		"subject upper":           with("subject", "Other"),
		"subject empty":           with("subject", ""),
		"subject too long":        with("subject", strings.Repeat("a", 41)),
		"subject newline":         with("subject", "ab\ncd"),
		"missing fields":          with("fields", map[string]any{"path": e.other}),
		"extra field":             withField("token", "x"),
		"path relative":           withField("path", "other"),
		"path with dotdot":        withField("path", e.root+"/other/../other"),
		"path newline":            withField("path", e.other+"\n"),
		"path control":            withField("path", e.other+"\x01"),
		"path bidi override":      withField("path", e.other+"\u202e"),
		"path bidi isolate":       withField("path", e.other+"\u2066"),
		"path zero width":         withField("path", e.other+"\u200b"),
		"path bom":                withField("path", e.other+"\ufeff"),
		"path line separator":     withField("path", e.other+"\u2028"),
		"path nbsp":               withField("path", e.other+"\u00a0"),
		"path ideographic space":  withField("path", e.other+"\u3000"),
		"path em space":           withField("path", e.other+"\u2003"),
		"path combining acute":    withField("path", e.other+"\u0301"),
		"path variation selector": withField("path", e.other+"\ufe0f"),
		"path emoji":              withField("path", e.other+"\U0001F600"),
		"path latin accent":       withField("path", e.other+"\u00e9"),
		"path too long":           withField("path", "/"+long+long),
		"path outside the roots":  withField("path", e.outside),
		"path is the root":        withField("path", e.root),
		"path nested":             withField("path", e.other+"/sub"),
		"path not a repository":   withField("path", filepath.Join(e.root, "nothing")),
		"kind unknown to policy":  withField("kind", "nopolicy"),
		"kind bidi":               withField("kind", "docs\u202e"),
		"kind newline":            withField("kind", "docs\n"),
		"kind upper":              withField("kind", "Docs"),
		"kind too long":           withField("kind", strings.Repeat("a", 33)),
		"kind korean":             withField("kind", "버그수정"),
		"set_flag other flag": {"action": "set_flag", "subject": "reg",
			"fields": map[string]any{"flag": "auto_merge", "value": true}},
		"set_flag value string": {"action": "set_flag", "subject": "reg",
			"fields": map[string]any{"flag": "ai_context", "value": "true"}},
		"set_flag flag bidi": {"action": "set_flag", "subject": "reg",
			"fields": map[string]any{"flag": "ai_context\u202e", "value": true}},
		"set_flag with path": {"action": "set_flag", "subject": "reg",
			"fields": map[string]any{"flag": "ai_context", "value": true, "path": "/x"}},
	}
	for name, body := range bodies {
		code, b := e.sock(t, "POST", "/api/v1/projects/propose", jsonBody(body))
		if code != 400 && code != 403 {
			t.Errorf("%s: %d %s", name, code, b)
		}
		// The refusal never echoes the offending text.
		if strings.Contains(string(b), "\u202e") || strings.Contains(string(b), "nopolicy") {
			t.Errorf("%s: echoed hostile text: %s", name, b)
		}
	}
	// A body over the cap is refused.
	if code, _ := e.sock(t, "POST", "/api/v1/projects/propose", `{"action":"`+strings.Repeat("a", 70<<10)+`"}`); code != 413 && code != 400 {
		t.Errorf("big body: %d", code)
	}
	if ops := e.rb.ops(); len(ops) != 0 {
		t.Fatalf("broker called: %v", ops)
	}
}

func TestApplyRecomputesChangeAndRefusesMismatch(t *testing.T) {
	e := newProjEnv(t)
	p := e.propose(t, "register_project", "other", e.registerFields())
	// Fields that differ from what was signed (ai_context flipped on): the
	// server recomputes the change from the fields and refuses, no broker call.
	tampered := applyBodyOf(p, webauthnSig)
	tampered["fields"] = map[string]any{"path": e.other, "kind": "docs", "ai_context": true}
	code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(tampered))
	if code != 400 || v1Code(b) != "change_mismatch" {
		t.Fatalf("%d %s", code, b)
	}
	// Subject or action swapped: same.
	for name, mod := range map[string]func(m map[string]any){
		"subject": func(m map[string]any) { m["subject"] = "reg2" },
		"kind":    func(m map[string]any) { m["fields"].(map[string]any)["kind"] = "code" },
		"path":    func(m map[string]any) { m["fields"].(map[string]any)["path"] = e.repo },
		"change":  func(m map[string]any) { m["change"] = strings.Repeat("0", 64) },
	} {
		m := applyBodyOf(p, webauthnSig)
		mod(m)
		if code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(m)); code != 400 || v1Code(b) != "change_mismatch" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if n := len(e.rb.ops()); n != 0 {
		t.Fatalf("broker called %d times for mismatched changes", n)
	}
	// An honest request goes through and carries the RECOMPUTED change.
	code, b = e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig)))
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	w := e.rb.writes()
	if len(w) != 1 || w[0].Op != "register_project" {
		t.Fatalf("writes %+v", w)
	}
	var args map[string]any
	if err := json.Unmarshal(w[0].Args, &args); err != nil {
		t.Fatal(err)
	}
	want, _ := registrymsg.ChangeHash("register_project", "other", e.registerFields())
	if args["change"] != want || args["id"] != "other" || args["path"] != e.other || args["kind"] != "docs" || args["ai_context"] != false ||
		args["request"] != p.Request || args["issued"] != p.Issued {
		t.Fatalf("args %v", args)
	}
	var out struct {
		ID        string `json:"id"`
		Path      string `json:"path"`
		AIContext bool   `json:"ai_context"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID != "other" || out.Path != e.other || out.AIContext {
		t.Fatalf("response %s", b)
	}
}

func TestApplyForwardsSignatureAndMapsErrors(t *testing.T) {
	e := newProjEnv(t)
	type want struct {
		status int
		code   string
	}
	table := map[string]want{
		"not_signed":          {400, "not_signed"},
		"bad_signature":       {403, "bad_signature"},
		"unknown_signer":      {403, "unknown_signer"},
		"stale_signature":     {403, "stale_signature"},
		"signers_unavailable": {503, "signers_unavailable"},
		"request_reused":      {409, "request_reused"},
		"change_mismatch":     {400, "change_mismatch"},
		"project_exists":      {409, "project_exists"},
		"project_unknown":     {404, "project_unknown"},
		"path_not_allowed":    {400, "path_not_allowed"},
		// A WRITE answered registry_unavailable may already be applied (a failed
		// directory fsync after the rename leaves the change in place), so it is an
		// unknown outcome, not a definite failure. Reads keep registry_unavailable.
		"registry_unavailable": {504, "outcome_unknown"},
		"not_supported":        {400, "not_supported"},
		"bad_request":          {400, "bad_request"},
		// a broker-side or client-side failure that is not a registry code
		"busy":           {503, "broker_unavailable"},
		"forbidden":      {503, "broker_unavailable"},
		"internal_error": {504, "outcome_unknown"},
		"upstream_error": {504, "outcome_unknown"},
		"bad_reply":      {504, "outcome_unknown"},
		"something_new":  {504, "outcome_unknown"},
		"timeout":        {504, "outcome_unknown"},
	}
	msgs := map[string]string{}
	for code, w := range table {
		e.rb.mu.Lock()
		e.rb.reply = func(string, json.RawMessage) string { return brokerErrReply(code) }
		e.rb.mu.Unlock()
		before := len(e.rb.writes())
		p := e.propose(t, "register_project", "other", e.registerFields())
		status, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig)))
		if status != w.status || v1Code(b) != w.code {
			t.Errorf("%s: got %d %s, want %d %s", code, status, b, w.status, w.code)
		}
		for _, leak := range []string{"SECRET-BROKER-TEXT", "sk-live-abc", "evil.example"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s: broker text forwarded: %s", code, b)
			}
		}
		// Exactly one forward per apply: a write is never retried.
		if got := len(e.rb.writes()) - before; got != 1 {
			t.Errorf("%s: %d broker writes for one apply", code, got)
		}
		var env struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal(b, &env)
		if env.Error.Message == "" || !regexp.MustCompile(`[가-힣]`).MatchString(env.Error.Message) {
			t.Errorf("%s: sentence %q", code, env.Error.Message)
		}
		if prev, dup := msgs[env.Error.Message]; dup && env.Error.Code != "broker_unavailable" && env.Error.Code != "broker_error" && prev != env.Error.Code {
			t.Errorf("%s and %s share a sentence", prev, env.Error.Code)
		}
		msgs[env.Error.Message] = env.Error.Code
		if w.code == "outcome_unknown" && !strings.Contains(env.Error.Message, "목록") {
			t.Errorf("outcome_unknown must tell the user to re-read the list: %q", env.Error.Message)
		}
	}
	// A dropped connection after the request was written is also an unknown
	// outcome, not a plain unavailable broker; still one forward.
	e.rb.mu.Lock()
	e.rb.reply = func(string, json.RawMessage) string { return "DROP" }
	e.rb.mu.Unlock()
	before := len(e.rb.writes())
	p := e.propose(t, "register_project", "other", e.registerFields())
	status, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig)))
	if status != 504 || v1Code(b) != "outcome_unknown" || len(e.rb.writes())-before != 1 {
		t.Errorf("dropped: %d %s writes %d", status, b, len(e.rb.writes())-before)
	}
	// The signature object reaches the broker unchanged, WebAuthn parts included.
	e.rb.mu.Lock()
	e.rb.reply = nil
	e.rb.mu.Unlock()
	p = e.propose(t, "register_project", "other", e.registerFields())
	if status, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	w := e.rb.writes()
	var sent struct {
		Signature json.RawMessage `json:"signature"`
	}
	_ = json.Unmarshal(w[len(w)-1].Args, &sent)
	var a, bb any
	_ = json.Unmarshal(sent.Signature, &a)
	_ = json.Unmarshal([]byte(jsonBody(webauthnSig)), &bb)
	if jsonBody(a) != jsonBody(bb) {
		t.Fatalf("signature changed: %s", sent.Signature)
	}
	// set_flag is forwarded as set_flag with id, flag and value.
	f := e.propose(t, "set_flag", "reg", map[string]any{"flag": "ai_context", "value": true})
	if status, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(f, webauthnSig))); status != 200 {
		t.Fatalf("%d %s", status, b)
	}
	w = e.rb.writes()
	var fa map[string]any
	_ = json.Unmarshal(w[len(w)-1].Args, &fa)
	wantChange, _ := registrymsg.ChangeHash("set_flag", "reg", map[string]any{"flag": "ai_context", "value": true})
	if w[len(w)-1].Op != "set_flag" || fa["id"] != "reg" || fa["flag"] != "ai_context" || fa["value"] != true || fa["change"] != wantChange {
		t.Fatalf("set_flag args %v", fa)
	}
	// Anything but a project object from the broker is a broker_error.
	e.rb.mu.Lock()
	e.rb.reply = func(string, json.RawMessage) string { return `{"ok":true,"result":{"id":"BAD ID","path":"/x"}}` }
	e.rb.mu.Unlock()
	p = e.propose(t, "register_project", "other", e.registerFields())
	if status, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); status != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("bad result: %d %s", status, b)
	}
	// An ok whose project does not decode (wrong type) is the same.
	e.rb.mu.Lock()
	e.rb.reply = func(string, json.RawMessage) string { return `{"ok":true,"result":{"id":5}}` }
	e.rb.mu.Unlock()
	p = e.propose(t, "register_project", "other", e.registerFields())
	if status, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); status != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("undecodable result: %d %s", status, b)
	}
	// ...and an ok naming another project than the one changed.
	e.rb.mu.Lock()
	e.rb.reply = func(string, json.RawMessage) string {
		return `{"ok":true,"result":{"id":"someone-else","path":"/x/y","kind":"docs","ai_context":false}}`
	}
	e.rb.mu.Unlock()
	p = e.propose(t, "register_project", "other", e.registerFields())
	if status, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); status != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("foreign result: %d %s", status, b)
	}
}

func TestApplyRefusesUnsignedAndBadShapes(t *testing.T) {
	e := newProjEnv(t)
	p := e.propose(t, "register_project", "other", e.registerFields())
	good := func() map[string]any { return applyBodyOf(p, webauthnSig) }
	mod := func(f func(m map[string]any)) string { m := good(); f(m); return jsonBody(m) }
	cases := map[string]string{
		"no signature":     jsonBody(applyBodyOf(p, nil)),
		"signature null":   mod(func(m map[string]any) { m["signature"] = nil }),
		"signature string": mod(func(m map[string]any) { m["signature"] = "sig" }),
		"signature extra key": mod(func(m map[string]any) {
			m["signature"] = map[string]any{"signer": "a", "alg": "ssh-ed25519", "sig": "x", "x": 1}
		}),
		"signature Capital": mod(func(m map[string]any) {
			m["signature"] = map[string]any{"Signer": "a", "alg": "ssh-ed25519", "sig": "x"}
		}),
		"signature empty sig": mod(func(m map[string]any) {
			m["signature"] = map[string]any{"signer": "a", "alg": "ssh-ed25519", "sig": ""}
		}),
		"signature huge": mod(func(m map[string]any) {
			m["signature"] = map[string]any{"signer": "a", "alg": "ssh-ed25519", "sig": strings.Repeat("a", 40000)}
		}),
		"webauthn extra key": mod(func(m map[string]any) {
			m["signature"] = map[string]any{"signer": "a", "alg": "webauthn-es256", "sig": "x", "webauthn": map[string]any{"authenticator_data": "a", "client_data_json": "b", "z": 1}}
		}),
		"unknown top key":      mod(func(m map[string]any) { m["token"] = "x" }),
		"capitalised top key":  mod(func(m map[string]any) { m["Request"] = m["request"]; delete(m, "request") }),
		"no change":            mod(func(m map[string]any) { delete(m, "change") }),
		"change uppercase hex": mod(func(m map[string]any) { m["change"] = strings.ToUpper(p.Change) }),
		"request short":        mod(func(m map[string]any) { m["request"] = "abc" }),
		"request uppercase":    mod(func(m map[string]any) { m["request"] = strings.ToUpper(p.Request) }),
		"issued not UTC":       mod(func(m map[string]any) { m["issued"] = "2026-11-09T12:00:00+09:00" }),
		"issued fractional":    mod(func(m map[string]any) { m["issued"] = "2026-11-09T12:00:00.5Z" }),
		"issued junk":          mod(func(m map[string]any) { m["issued"] = "yesterday" }),
		"duplicate key":        strings.Replace(jsonBody(good()), `"subject":"other"`, `"subject":"other","subject":"reg"`, 1),
		"trailing data":        jsonBody(good()) + "{}",
		"unknown action":       mod(func(m map[string]any) { m["action"] = "delete_project" }),
	}
	for name, body := range cases {
		code, b := e.sock(t, "POST", "/api/v1/projects/apply", body)
		if code != 400 {
			t.Errorf("%s: %d %s", name, code, b)
		}
		if name == "no signature" || name == "signature null" {
			if v1Code(b) != "not_signed" {
				t.Errorf("%s: code %s", name, v1Code(b))
			}
		}
	}
	// Local path and kind rules (both layers check): outside the roots,
	// nested, unknown to the policy; the signature cannot make them pass.
	for name, f := range map[string]map[string]any{
		"outside": {"path": e.outside, "kind": "docs", "ai_context": false},
		"nested":  {"path": e.other + "/sub", "kind": "docs", "ai_context": false},
		"kind":    {"path": e.other, "kind": "nopolicy", "ai_context": false},
	} {
		change, _ := registrymsg.ChangeHash("register_project", "other", f)
		m := map[string]any{"action": "register_project", "subject": "other", "fields": f, "change": change,
			"request": p.Request, "issued": p.Issued, "signature": webauthnSig}
		code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(m))
		want := "path_not_allowed"
		if name == "kind" {
			want = "bad_request"
		}
		if code != 400 || v1Code(b) != want {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// set_flag for anything but ai_context is not supported (no broker call).
	if code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(map[string]any{"action": "set_flag", "subject": "reg",
		"fields": map[string]any{"flag": "auto_merge", "value": true}, "change": strings.Repeat("a", 64),
		"request": p.Request, "issued": p.Issued, "signature": webauthnSig})); code != 400 || v1Code(b) != "not_supported" {
		t.Errorf("auto_merge: %d %s", code, b)
	}
	if n := len(e.rb.ops()); n != 0 {
		t.Fatalf("broker called %d times: %v", n, e.rb.ops())
	}
}

func TestApplyRequiresWriteTierAndCSRF(t *testing.T) {
	e := newProjEnv(t)
	srv := e.httpServer(t)
	tok, csrf := e.newSession(t, "alice")
	token := e.newToken(t, "mac-ssh")
	for _, path := range []string{"/api/v1/projects/propose", "/api/v1/projects/apply"} {
		p := e.propose(t, "register_project", "other", e.registerFields())
		body := jsonBody(applyBodyOf(p, webauthnSig))
		if path == "/api/v1/projects/propose" {
			body = jsonBody(map[string]any{"action": p.Action, "subject": p.Subject, "fields": p.Fields})
		}
		before := len(e.rb.ops())
		for name, h := range map[string]hdr{
			"no credentials":     {},
			"session no csrf":    {"Cookie": cookie(tok)},
			"session wrong csrf": {"Cookie": cookie(tok), CSRFHeader: "csrf-wrong"},
			"session empty csrf": {"Cookie": cookie(tok), CSRFHeader: ""},
			"bad bearer":         {"Authorization": "Bearer nope"},
		} {
			code, _, b := httpDo(t, srv, "POST", path, body, h)
			want := 403
			if name == "no credentials" || name == "bad bearer" {
				want = 401
			}
			if code != want {
				t.Errorf("%s %s: %d %s", path, name, code, b)
			}
		}
		if len(e.rb.ops()) != before {
			t.Errorf("%s: the broker was called without authority", path)
		}
		// Reads need authentication too.
		if code, _, _ := httpDo(t, srv, "GET", "/api/v1/projects/registry", "", hdr{}); code != 401 {
			t.Errorf("registry read without credentials: %d", code)
		}
		if code, _, _ := httpDo(t, srv, "GET", "/api/v1/projects/candidates", "", hdr{}); code != 401 {
			t.Errorf("candidates without credentials: %d", code)
		}
		// Every legitimate tier gets through.
		for name, h := range map[string]hdr{
			"session": {"Cookie": cookie(tok), CSRFHeader: csrf},
			"token":   {"Authorization": "Bearer " + token},
		} {
			p := e.propose(t, "register_project", "other", e.registerFields())
			b := jsonBody(applyBodyOf(p, webauthnSig))
			if path == "/api/v1/projects/propose" {
				b = jsonBody(map[string]any{"action": p.Action, "subject": p.Subject, "fields": p.Fields})
			}
			if code, _, rb := httpDo(t, srv, "POST", path, b, h); code != 200 {
				t.Errorf("%s %s: %d %s", path, name, code, rb)
			}
		}
	}
}

func TestApplyLedgerEventHasNoContent(t *testing.T) {
	e := newProjEnv(t)
	secretPath := e.other
	p := e.propose(t, "register_project", "other", e.registerFields())
	if code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	e.rb.mu.Lock()
	e.rb.reply = func(string, json.RawMessage) string { return brokerErrReply("project_exists") }
	e.rb.mu.Unlock()
	p2 := e.propose(t, "register_project", "other", e.registerFields())
	if code, _ := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p2, webauthnSig))); code != 409 {
		t.Fatalf("%d", code)
	}
	lp := policy.OrchLedgerPath(e.home)
	raw, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretPath, "docs", p.Request, p.Change, "c2ln", "alice", "SECRET-BROKER-TEXT", p.Message} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q in the ledger:\n%s", leak, raw)
		}
	}
	evs, err := ledger.Events(lp)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	for _, ev := range evs {
		if ev.Type == "project_change" {
			got = append(got, ev.Payload)
		}
	}
	if len(got) != 2 {
		t.Fatalf("%d project_change events:\n%s", len(got), raw)
	}
	for _, pl := range got {
		keys := []string{}
		for k := range pl {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "action,actor,code,ok,subject" {
			t.Fatalf("payload keys %v", keys)
		}
		if pl["actor"] != socketActor() || pl["action"] != "register_project" || pl["subject"] != "other" {
			t.Fatalf("%v", pl)
		}
	}
	if got[0]["ok"] != true || got[1]["ok"] != false || got[1]["code"] != "project_exists" {
		t.Fatalf("%v", got)
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

func TestProjectsWorkWithBrokerDown(t *testing.T) {
	for name, opt := range map[string]func(c *Config){
		"no socket configured": func(c *Config) {},
		"nothing listening":    func(c *Config) { c.BrokerSocket = filepath.Join(shortDir(t), "none.sock") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, noSteps{}, 1, func(c *Config) {
				c.Policy = func() PolicyView { return PolicyView{Classes: map[string]string{"docs": "L1"}} }
				opt(c)
			})
			e.d.pauseWorkers()
			do := func(m, p, body string) (int, []byte) { c, _, b := e.sockDo(t, m, p, body, nil); return c, b }
			for _, path := range []string{"/api/v1/projects/registry", "/api/v1/projects/candidates"} {
				if code, b := do("GET", path, ""); code != 503 || v1Code(b) != "broker_unavailable" {
					t.Errorf("%s: %d %s", path, code, b)
				}
			}
			// propose needs no broker.
			fields := map[string]any{"path": e.repo, "kind": "docs", "ai_context": false}
			code, b := do("POST", "/api/v1/projects/propose", jsonBody(map[string]any{"action": "register_project", "subject": "proj", "fields": fields}))
			if code != 200 {
				t.Fatalf("propose %d %s", code, b)
			}
			var p proposeOut
			_ = json.Unmarshal(b, &p)
			if code, b := do("POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); code != 503 || v1Code(b) != "broker_unavailable" {
				t.Errorf("apply: %d %s", code, b)
			}
			// Everything that worked before still works.
			if code, b := do("GET", "/api/v1/policy", ""); code != 200 {
				t.Errorf("policy %d %s", code, b)
			}
			if code, b := do("GET", "/api/v1/projects", ""); code != 200 {
				t.Errorf("projects %d %s", code, b)
			}
			if code, b := do("GET", "/api/v1/repos", ""); code != 200 || !strings.Contains(string(b), e.repo) {
				t.Errorf("repos %d %s", code, b)
			}
			if code, b := e.submit(t, e.repo, "HEAD", "p", "docs", "claude"); code != 201 {
				t.Errorf("submit %d %s", code, b)
			}
		})
	}
}

func TestAPIAcceptsSSHSignature(t *testing.T) {
	e := newProjEnv(t)
	ssh := map[string]any{"signer": "bob", "alg": "ssh-ed25519", "sig": "-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"}
	p := e.propose(t, "set_flag", "reg", map[string]any{"flag": "ai_context", "value": true})
	if code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, ssh))); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	w := e.rb.writes()
	if len(w) != 1 {
		t.Fatalf("writes %d", len(w))
	}
	var sent struct {
		Signature map[string]any `json:"signature"`
	}
	_ = json.Unmarshal(w[0].Args, &sent)
	if sent.Signature["alg"] != "ssh-ed25519" || sent.Signature["signer"] != "bob" || sent.Signature["sig"] != ssh["sig"] {
		t.Fatalf("%v", sent.Signature)
	}
	if _, has := sent.Signature["webauthn"]; has {
		t.Fatalf("webauthn invented: %v", sent.Signature)
	}
}

// A registered path that was replaced by a symlink is not followed at use
// time: the AI preview resolves the real path, which the broker's registry
// does not list, so nothing is read or sent.
func TestRegisteredPathReplacedBySymlinkRefusedAtUse(t *testing.T) {
	e := newProjEnv(t)
	e.rb.mu.Lock()
	e.rb.projects[0]["ai_context"] = true
	e.rb.mu.Unlock()
	preview := func() (int, []byte) {
		return e.sock(t, "POST", "/api/v1/refine/preview", jsonBody(map[string]string{"repo": e.reg, "text": "add a health check"}))
	}
	if code, b := preview(); code != 200 || !strings.Contains(string(b), `"allowed":true`) {
		t.Fatalf("before: %d %s", code, b)
	}
	// Swap the registered directory for a link to another repo of the root.
	if err := os.Rename(e.reg, e.reg+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.other, e.reg); err != nil {
		t.Fatal(err)
	}
	code, b := preview()
	if code != 200 || !strings.Contains(string(b), `"allowed":false`) || strings.Contains(string(b), `"files":[{`) {
		t.Fatalf("after the swap: %d %s", code, b)
	}
	// And a link to a repo outside every root is refused outright.
	os.Remove(e.reg)
	if err := os.Symlink(e.outside, e.reg); err != nil {
		t.Fatal(err)
	}
	if code, b := preview(); code != 403 {
		t.Fatalf("outside link: %d %s", code, b)
	}
}

func TestConstantsAgreeWithWeb(t *testing.T) {
	// The server-side patterns are the shared registrymsg ones.
	for _, ok := range []string{"a", "my-proj-1"} {
		if !registrymsg.ValidSubject(ok) {
			t.Errorf("%q should be a subject", ok)
		}
	}
	for _, bad := range []string{"", "A", "1a", "a_b", strings.Repeat("a", 41)} {
		if registrymsg.ValidSubject(bad) {
			t.Errorf("%q should not be a subject", bad)
		}
	}
	for _, ok := range []string{"docs", "a_b-c", strings.Repeat("a", 32)} {
		if !registrymsg.ValidKind(ok) {
			t.Errorf("%q should be a kind", ok)
		}
	}
	for _, bad := range []string{"", "Docs", "1a", "버그수정", strings.Repeat("a", 33)} {
		if registrymsg.ValidKind(bad) {
			t.Errorf("%q should not be a kind", bad)
		}
	}
	// The browser's copy must exist (a missing file is a failure, never a skip)
	// and agree on every shared constant.
	src, err := os.ReadFile("../web/static/views/projects.js")
	if err != nil {
		t.Fatalf("web/static/views/projects.js: %v", err)
	}
	js := string(src)
	jsRegex := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = /(.*)/;\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS regex constant %s not found", name)
		}
		return m[1]
	}
	jsString := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = '([^']*)';\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS string constant %s not found", name)
		}
		return m[1]
	}
	jsInt := func(name string) int {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = ([0-9]+);\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS integer constant %s not found", name)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if got, want := jsRegex("SUBJECT_RE"), subjectPattern; got != want {
		t.Errorf("SUBJECT_RE %q != %q", got, want)
	}
	if got, want := jsRegex("KIND_RE"), kindPattern; got != want {
		t.Errorf("KIND_RE %q != %q", got, want)
	}
	// The flag pattern lives unexported in registrymsg: pin the literal and
	// the behaviour that matters.
	if got, want := jsRegex("FLAG_RE"), `^[a-z][a-z0-9_]{0,31}$`; got != want {
		t.Errorf("FLAG_RE %q != %q", got, want)
	}
	for _, s := range []string{"ai_context", "a", strings.Repeat("a", 32)} {
		if _, err := registrymsg.CanonicalChange("set_flag", "a", map[string]any{"flag": s, "value": true}); err != nil {
			t.Errorf("flag %q should be valid: %v", s, err)
		}
	}
	for _, s := range []string{"", "Ai", "1a", "a-b", strings.Repeat("a", 33)} {
		if _, err := registrymsg.CanonicalChange("set_flag", "a", map[string]any{"flag": s, "value": true}); err == nil {
			t.Errorf("flag %q should be refused", s)
		}
	}
	// The path text rule: printable ASCII and Hangul syllables only.
	if got, want := jsRegex("PATH_TEXT_RE"), `^[\x20-\x7e\uac00-\ud7a3]+$`; got != want {
		t.Errorf("PATH_TEXT_RE %q != %q", got, want)
	}
	for r, want := range map[rune]bool{0x1f: false, 0x20: true, 0x7e: true, 0x7f: false, 0xa0: false, 0xabff: false, 0xac00: true, 0xd7a3: true,
		0xd7a4: false, 0x200b: false, 0x3000: false, 0x1f600: false} {
		if got := safeProjectPath("/a" + string(r)); got != want {
			t.Errorf("safeProjectPath rune %U = %v, want %v", r, got, want)
		}
	}
	// Length caps.
	if got := jsInt("MAX_PATH_BYTES"); got != maxPathBytes {
		t.Errorf("MAX_PATH_BYTES %d != %d", got, maxPathBytes)
	}
	if !safeProjectPath("/"+strings.Repeat("a", maxPathBytes-1)) || safeProjectPath("/"+strings.Repeat("a", maxPathBytes)) {
		t.Errorf("safeProjectPath does not cap at %d bytes", maxPathBytes)
	}
	if got, want := jsInt("SUBJECT_MAX"), 40; got != want || !registrymsg.ValidSubject("a"+strings.Repeat("b", want-1)) || registrymsg.ValidSubject("a"+strings.Repeat("b", want)) {
		t.Errorf("SUBJECT_MAX %d does not match registrymsg", got)
	}
	if got, want := jsInt("KIND_MAX"), 32; got != want || !registrymsg.ValidKind("a"+strings.Repeat("b", want-1)) || registrymsg.ValidKind("a"+strings.Repeat("b", want)) {
		t.Errorf("KIND_MAX %d does not match registrymsg", got)
	}
	// Actions and the one flag the console may change.
	if got := jsString("ACTION_REGISTER"); got != registrymsg.ActionRegisterProject {
		t.Errorf("ACTION_REGISTER %q", got)
	}
	if got := jsString("ACTION_SET_FLAG"); got != registrymsg.ActionSetFlag {
		t.Errorf("ACTION_SET_FLAG %q", got)
	}
	if got := jsString("FLAG_AI_CONTEXT"); got != "ai_context" {
		t.Errorf("FLAG_AI_CONTEXT %q", got)
	}
	// The browser's message format: the magic line and field order of
	// signing.RegistryMessage.Text.
	wa, werr := os.ReadFile("../web/static/webauthn.js")
	if werr != nil {
		t.Fatal(werr)
	}
	m := signing.RegistryMessage{Action: "set_flag", Subject: "a", Change: strings.Repeat("a", 64), Request: strings.Repeat("b", 32),
		Issued: time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)}
	for _, line := range strings.Split(strings.TrimSuffix(m.Text(), "\n"), "\n") {
		key, _, _ := strings.Cut(line, ": ")
		if key == "aigate-registry-v1" {
			key = "^aigate-registry-v1\\\\n'"
		} else {
			key += ": "
		}
		if !strings.Contains(string(wa), key) {
			t.Errorf("webauthn.js REGISTRY_MESSAGE_RE lacks %q", key)
		}
	}
	// The browser's message regex: every piece derives from the Go definitions.
	waText := string(wa)
	bare := func(p string) string { return strings.TrimSuffix(strings.TrimPrefix(p, "^"), "$") }
	for _, piece := range []string{
		"action: (" + registrymsg.ActionRegisterProject + "|" + registrymsg.ActionSetFlag + ")",
		"subject: (" + bare(subjectPattern) + ")",
		"change: ([0-9a-f]{64})",
		"request: ([0-9a-f]{32})",
		"issued: ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)",
	} {
		if !strings.Contains(waText, piece) {
			t.Errorf("webauthn.js REGISTRY_MESSAGE_RE lacks %q", piece)
		}
	}
	// ... and the quantifiers are the Go rules: 64 hex change, 32 hex request.
	okMsg := signing.RegistryMessage{Action: "set_flag", Subject: "a", Change: strings.Repeat("a", 64), Request: strings.Repeat("b", 32),
		Issued: time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)}
	if okMsg.Validate() != nil {
		t.Fatal("base message must be valid")
	}
	for _, mut := range []func(*signing.RegistryMessage){
		func(m *signing.RegistryMessage) { m.Change = strings.Repeat("a", 63) },
		func(m *signing.RegistryMessage) { m.Change = strings.Repeat("a", 65) },
		func(m *signing.RegistryMessage) { m.Request = strings.Repeat("b", 31) },
		func(m *signing.RegistryMessage) { m.Request = strings.Repeat("b", 33) },
		func(m *signing.RegistryMessage) { m.Change = strings.Repeat("A", 64) },
	} {
		m := okMsg
		mut(&m)
		if m.Validate() == nil {
			t.Errorf("Go accepts a message the browser regex refuses: %+v", m)
		}
	}
	// The browser's SCHEMA (allowed fields per action and their types) is the registrymsg one.
	for action, goFields := range map[string]map[string]string{
		registrymsg.ActionRegisterProject: {"path": "s", "kind": "s", "ai_context": "b"},
		registrymsg.ActionSetFlag:         {"flag": "s", "value": "b"},
	} {
		key := "[ACTION_REGISTER]"
		if action == registrymsg.ActionSetFlag {
			key = "[ACTION_SET_FLAG]"
		}
		m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `: \{([^}]*)\},?\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS SCHEMA entry %s not found", key)
		}
		jsFields := map[string]string{}
		for _, part := range strings.Split(m[1], ",") {
			name, typ, ok := strings.Cut(strings.TrimSpace(part), ": ")
			if !ok {
				t.Fatalf("SCHEMA %s: bad entry %q", key, part)
			}
			jsFields[name] = strings.Trim(typ, "'")
		}
		if len(jsFields) != len(goFields) {
			t.Errorf("SCHEMA %s fields %v != %v", key, jsFields, goFields)
		}
		for name, typ := range goFields {
			if jsFields[name] != typ {
				t.Errorf("SCHEMA %s field %s: %q != %q", key, name, jsFields[name], typ)
			}
		}
		// Go really takes exactly these fields: all of them, none missing, none extra.
		sample := func(name, typ string) any {
			if typ == "b" {
				return true
			}
			return map[string]string{"path": "/a", "kind": "docs", "flag": "ai_context"}[name]
		}
		full := map[string]any{}
		for name, typ := range jsFields {
			full[name] = sample(name, typ)
		}
		if _, err := registrymsg.CanonicalChange(action, "a", full); err != nil {
			t.Errorf("registrymsg refuses the browser's %s fields: %v", action, err)
		}
		for name := range full {
			less := map[string]any{}
			for k, v := range full {
				if k != name {
					less[k] = v
				}
			}
			if _, err := registrymsg.CanonicalChange(action, "a", less); err == nil {
				t.Errorf("registrymsg accepts %s without %s", action, name)
			}
		}
		full["extra"] = "x"
		if _, err := registrymsg.CanonicalChange(action, "a", full); err == nil {
			t.Errorf("registrymsg accepts an extra field for %s", action)
		}
	}
	// The AI sentences the browser looks for are the ones the server writes.
	onMark, offMark := jsString("AI_ON_MARK"), jsString("AI_OFF_MARK")
	count := func(c registryChange, mark string) int {
		n := 0
		for _, line := range previewSentences(c) {
			if strings.Contains(line, mark) {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		c  registryChange
		on bool
	}{
		{registryChange{Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": true}}, true},
		{registryChange{Action: "register_project", Subject: "a", Fields: map[string]any{"path": "/a", "kind": "docs", "ai_context": false}}, false},
		{registryChange{Action: "set_flag", Subject: "a", Fields: map[string]any{"flag": "ai_context", "value": true}}, true},
		{registryChange{Action: "set_flag", Subject: "a", Fields: map[string]any{"flag": "ai_context", "value": false}}, false},
	} {
		on, off := count(c.c, onMark), count(c.c, offMark)
		if c.on && (on != 1 || off != 0) || !c.on && (off != 1 || on != 0) {
			t.Errorf("preview of %v: on=%d off=%d marks (%q, %q)", c.c, on, off, onMark, offMark)
		}
	}
	// Shipped, loaded and routed: the shell must know the view.
	for _, f := range []struct{ file, needle string }{
		{"app.js", "'projects'"}, {"router.js", "case 'projects':"}, {"index.html", `href="#/projects"`},
	} {
		b, rerr := os.ReadFile(filepath.Join("../web/static", f.file))
		if rerr != nil || !strings.Contains(string(b), f.needle) {
			t.Errorf("%s does not wire the projects view (%q)", f.file, f.needle)
		}
	}
	// The token constants (W3b): see checkTokenConstants (tokens_test.go).
	t.Run("tokens", checkTokenConstants)

}

// A path is shown on a signing screen: only printable ASCII and Hangul
// syllables, at propose, apply and in the candidate list (no dead ends).
func TestProjectPathTextRule(t *testing.T) {
	e := newProjEnv(t)
	hangul := filepath.Join(e.root, "프로젝트")
	mkRepo(t, hangul)
	for _, name := range []string{"zw\u200bx", "wide\u3000x", "nb\u00a0x", "comb\u0065\u0301x", "vs\ufe0fx"} {
		mkRepo(t, filepath.Join(e.root, name))
	}
	code, b := e.sock(t, "GET", "/api/v1/projects/candidates", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var got struct {
		Candidates []struct{ Path string } `json:"candidates"`
	}
	_ = json.Unmarshal(b, &got)
	listed := map[string]bool{}
	for _, c := range got.Candidates {
		listed[c.Path] = true
	}
	if !listed[hangul] || !listed[e.other] || !listed[e.repo] || len(listed) != 3 {
		t.Fatalf("candidates %v", listed)
	}
	// The Hangul path can be proposed and applied.
	f := map[string]any{"path": hangul, "kind": "docs", "ai_context": false}
	p := e.propose(t, "register_project", "hangul", f)
	if code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	// The hostile ones are refused by apply too, signature or not.
	for _, name := range []string{"zw\u200bx", "wide\u3000x", "nb\u00a0x"} {
		f := map[string]any{"path": filepath.Join(e.root, name), "kind": "docs", "ai_context": false}
		change, _ := registrymsg.ChangeHash("register_project", "hostile", f)
		m := map[string]any{"action": "register_project", "subject": "hostile", "fields": f, "change": change,
			"request": p.Request, "issued": p.Issued, "signature": webauthnSig}
		if code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(m)); code != 400 {
			t.Errorf("%q: %d %s", name, code, b)
		}
	}
	if n := len(e.rb.writes()); n != 1 {
		t.Fatalf("%d broker writes", n)
	}
}

func (e *projEnv) tokenCaller(t *testing.T, name string) func(path, body string) (int, []byte) {
	srv := e.httpServer(t)
	tok := e.newToken(t, name)
	return func(path, body string) (int, []byte) {
		c, _, b := httpDo(t, srv, "POST", path, body, hdr{"Authorization": "Bearer " + tok})
		return c, b
	}
}

func (e *projEnv) proposeBody() string {
	return jsonBody(map[string]any{"action": "register_project", "subject": "other", "fields": e.registerFields()})
}

func TestProjectProposeRateLimit(t *testing.T) {
	e := newProjEnv(t)
	clk := &testClock{}
	e.d.handler.now = clk.now
	// A refused body takes no slot.
	if code, _ := e.sock(t, "POST", "/api/v1/projects/propose", `{"action":"nope"}`); code != 400 {
		t.Fatal(code)
	}
	for i := 0; i < ProjectProposePerActorHour; i++ {
		if code, b := e.sock(t, "POST", "/api/v1/projects/propose", e.proposeBody()); code != 200 {
			t.Fatalf("propose %d: %d %s", i+1, code, b)
		}
	}
	code, b := e.sock(t, "POST", "/api/v1/projects/propose", e.proposeBody())
	if code != 429 || v1Code(b) != "project_rate_limited" {
		t.Fatalf("%d %s", code, b)
	}
	other := e.tokenCaller(t, "mac-ssh")
	if code, b := other("/api/v1/projects/propose", e.proposeBody()); code != 200 {
		t.Fatalf("second actor: %d %s", code, b)
	}
	// Separate bucket: apply is not affected by exhausted proposals.
	clk.set(0)
	p := proposeOut{Action: "set_flag", Subject: "reg", Fields: map[string]any{"flag": "ai_context", "value": true}}
	p.Change, _ = registrymsg.ChangeHash(p.Action, p.Subject, p.Fields)
	p.Request, p.Issued = registrymsg.NewRequestID(), time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if code, b := e.sock(t, "POST", "/api/v1/projects/apply", jsonBody(applyBodyOf(p, webauthnSig))); code != 200 {
		t.Fatalf("apply with exhausted propose bucket: %d %s", code, b)
	}
	clk.set(time.Hour + time.Minute)
	if code, b := e.sock(t, "POST", "/api/v1/projects/propose", e.proposeBody()); code != 200 {
		t.Fatalf("after an hour: %d %s", code, b)
	}
}

func (e *projEnv) applyBody(t *testing.T) string {
	t.Helper()
	p := e.propose(t, "set_flag", "reg", map[string]any{"flag": "ai_context", "value": true})
	return jsonBody(applyBodyOf(p, webauthnSig))
}

func TestProjectApplyRateLimit(t *testing.T) {
	e := newProjEnv(t)
	clk := &testClock{}
	e.d.handler.now = clk.now
	bodies := make([]string, ProjectApplyPerActorHour+3)
	for i := range bodies {
		clk.set(time.Duration(i) * time.Millisecond) // keep propose under its own limit
		bodies[i] = e.applyBody(t)
	}
	clk.set(0)
	for i := 0; i < ProjectApplyPerActorHour; i++ {
		if code, b := e.sock(t, "POST", "/api/v1/projects/apply", bodies[i]); code != 200 {
			t.Fatalf("apply %d: %d %s", i+1, code, b)
		}
	}
	before := len(e.rb.writes())
	code, b := e.sock(t, "POST", "/api/v1/projects/apply", bodies[ProjectApplyPerActorHour])
	if code != 429 || v1Code(b) != "project_rate_limited" || len(e.rb.writes()) != before {
		t.Fatalf("%d %s writes %d->%d", code, b, before, len(e.rb.writes()))
	}
	other := e.tokenCaller(t, "mac-ssh")
	if code, b := other("/api/v1/projects/apply", bodies[ProjectApplyPerActorHour+1]); code != 200 {
		t.Fatalf("second actor: %d %s", code, b)
	}
	// Separate bucket: proposals still work.
	if code, b := e.sock(t, "POST", "/api/v1/projects/propose", e.proposeBody()); code != 200 {
		t.Fatalf("propose with exhausted apply bucket: %d %s", code, b)
	}
	clk.set(time.Hour + time.Minute)
	if code, b := e.sock(t, "POST", "/api/v1/projects/apply", bodies[ProjectApplyPerActorHour+2]); code != 200 {
		t.Fatalf("after an hour: %d %s", code, b)
	}
}

// A call that provably cost nothing gives its slot back; one that reached the
// broker keeps it, whatever the broker said.
func TestProjectApplyRefundRules(t *testing.T) {
	// Dead socket: the request never left, so no number of tries is limited.
	dead := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.BrokerSocket = filepath.Join(shortDir(t), "none.sock")
		c.Policy = func() PolicyView { return PolicyView{Classes: map[string]string{"docs": "L1"}} }
	})
	dead.d.pauseWorkers()
	f := map[string]any{"flag": "ai_context", "value": true}
	change, _ := registrymsg.ChangeHash("set_flag", "reg", f)
	body := jsonBody(map[string]any{"action": "set_flag", "subject": "reg", "fields": f, "change": change,
		"request": registrymsg.NewRequestID(), "issued": time.Now().UTC().Format("2006-01-02T15:04:05Z"), "signature": webauthnSig})
	for i := 0; i < ProjectApplyPerActorHour+5; i++ {
		c, _, b := dead.sockDo(t, "POST", "/api/v1/projects/apply", body, nil)
		if c != 503 || v1Code(b) != "broker_unavailable" {
			t.Fatalf("try %d: %d %s", i+1, c, b)
		}
	}
	// Refusals the broker made after looking at the signature keep the slot.
	for _, code := range []string{"bad_signature", "stale_signature", "unknown_signer", "request_reused", "change_mismatch", "timeout"} {
		e := newProjEnv(t)
		e.rb.mu.Lock()
		e.rb.reply = func(string, json.RawMessage) string { return brokerErrReply(code) }
		e.rb.mu.Unlock()
		for i := 0; i < ProjectApplyPerActorHour; i++ {
			if c, b := e.sock(t, "POST", "/api/v1/projects/apply", e.applyBody(t)); c == 429 {
				t.Fatalf("%s: limited early at %d: %s", code, i+1, b)
			}
		}
		if c, b := e.sock(t, "POST", "/api/v1/projects/apply", e.applyBody(t)); c != 429 || v1Code(b) != "project_rate_limited" {
			t.Errorf("%s kept no slot: %d %s", code, c, b)
		}
	}
	// busy and forbidden (the broker did nothing) give it back.
	for _, code := range []string{"busy", "forbidden"} {
		e := newProjEnv(t)
		e.rb.mu.Lock()
		e.rb.reply = func(string, json.RawMessage) string { return brokerErrReply(code) }
		e.rb.mu.Unlock()
		for i := 0; i < ProjectApplyPerActorHour+3; i++ {
			if c, b := e.sock(t, "POST", "/api/v1/projects/apply", e.applyBody(t)); c != 503 {
				t.Fatalf("%s try %d: %d %s", code, i+1, c, b)
			}
		}
	}
}

// The registry list is all or nothing: one entry that does not look like a
// project makes the whole list broker_error (fail closed), never a partial list.
func TestRegistryListFailsClosedOnOneBadEntry(t *testing.T) {
	e := newProjEnv(t)
	e.rb.mu.Lock()
	e.rb.projects = append(e.rb.projects, map[string]any{"id": "BAD ID", "path": "/x", "kind": "docs", "ai_context": true,
		"provider": nil, "gitlab": nil, "auto_merge": map[string]any{"enabled": false}})
	e.rb.mu.Unlock()
	for _, path := range []string{"/api/v1/projects/registry", "/api/v1/projects/candidates"} {
		code, b := e.sock(t, "GET", path, "")
		if code != 502 || v1Code(b) != "broker_error" || strings.Contains(string(b), "BAD ID") {
			t.Errorf("%s: %d %s", path, code, b)
		}
	}
}
