package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/clone"
	"aigate/internal/orch/policy"
	"aigate/internal/registrymsg"
)

const w3cKnownKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// w3cBroker is a stand-in orch-broker for the W3c ops on a temp unix socket.
// It records every call; nothing here reaches a real service.
type w3cBroker struct {
	mu        sync.Mutex
	calls     []regCall
	projects  []map[string]any
	providers []map[string]any
	orphans   []map[string]any
	// reply overrides an op's answer: "" = the default, "DROP" = close, "HANG" = never answer.
	reply func(op string, args json.RawMessage) string
	// createClone, when set, is merged into the default create_project result.
	createClone map[string]any
	// pathRoot is the directory new projects are registered under.
	pathRoot string
}

func (b *w3cBroker) opsNamed(names ...string) []regCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []regCall
	for _, c := range b.calls {
		for _, n := range names {
			if c.Op == n {
				out = append(out, c)
			}
		}
	}
	return out
}

func (b *w3cBroker) allOps() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, c := range b.calls {
		out = append(out, c.Op)
	}
	return out
}

func (b *w3cBroker) writeCalls() []regCall {
	return b.opsNamed("set_provider", "delete_provider", "create_group", "create_project", "register_project", "set_flag")
}

func (b *w3cBroker) setReply(fn func(op string, args json.RawMessage) string) {
	b.mu.Lock()
	b.reply = fn
	b.mu.Unlock()
}

func startW3cBroker(t *testing.T) (*w3cBroker, string) {
	t.Helper()
	b := &w3cBroker{providers: []map[string]any{
		{"id": "gl", "kind": "gitlab", "base_url": "http://gitlab.example:18082", "ssh_url": "ssh://git@gitlab.example", "token_ref": "gl-admin"},
	}}
	sock := filepath.Join(shortDir(t), "w.sock")
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

func (b *w3cBroker) serve(c net.Conn) {
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
	fn := b.reply
	projects := append([]map[string]any{}, b.projects...)
	providers := append([]map[string]any{}, b.providers...)
	orphans := append([]map[string]any{}, b.orphans...)
	b.mu.Unlock()
	if fn != nil {
		switch r := fn(req.Op, req.Args); r {
		case "":
		case "DROP":
			return
		case "HANG":
			time.Sleep(5 * time.Second)
			return
		default:
			c.Write([]byte(r + "\n"))
			return
		}
	}
	ok := func(v any) {
		out, _ := json.Marshal(map[string]any{"ok": true, "result": v})
		c.Write(append(out, '\n'))
	}
	var a map[string]any
	_ = json.Unmarshal(req.Args, &a)
	switch req.Op {
	case "projects":
		ok(map[string]any{"projects": projects, "providers": providers, "orphans": orphans,
			"auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5}})
	case "providers":
		rows := []map[string]any{}
		for _, p := range providers {
			r := map[string]any{}
			for k, v := range p {
				r[k] = v
			}
			r["in_use"] = false
			rows = append(rows, r)
		}
		ok(map[string]any{"providers": rows})
	case "list_groups":
		ok(map[string]any{"groups": []map[string]any{{"path": "team", "name": "team"}, {"path": "team/sub", "name": "sub"}}, "truncated": false})
	case "set_provider":
		ok(map[string]any{"id": a["id"], "kind": a["kind"], "base_url": a["base_url"], "ssh_url": a["ssh_url"], "token_ref": a["token_ref"]})
	case "delete_provider":
		ok(map[string]any{"id": a["id"], "deleted": true})
	case "create_group":
		path := a["path"].(string)
		if p, _ := a["parent_path"].(string); p != "" {
			path = p + "/" + path
		}
		ok(map[string]any{"provider": a["id"], "group": map[string]any{"path": path, "name": a["name"]}})
	case "create_project":
		name, _ := a["id"].(string)
		prov, _ := a["provider"].(string)
		full := a["group_path"].(string) + "/" + name
		entry := map[string]any{"id": name, "path": "/repos/" + name, "kind": "registered", "ai_context": a["ai_context"], "provider": prov,
			"gitlab": map[string]any{"project": full, "project_id": 99}, "auto_merge": map[string]any{"enabled": false}}
		b.mu.Lock()
		if p, ok := b.createPathOverride(name); ok {
			entry["path"] = p
		}
		b.projects = append(b.projects, entry)
		extra := b.createClone
		b.mu.Unlock()
		res := map[string]any{"project": entry, "ssh_url": "ssh://git@evil.example", "clone_url": "ssh://git@evil.example/x/y.git", "cloned_hint": false}
		for k, v := range extra {
			res[k] = v
		}
		ok(res)
	default:
		c.Write([]byte("{\"ok\":false,\"code\":\"unknown_op\"}\n"))
	}
}

func (b *w3cBroker) createPathOverride(name string) (string, bool) {
	if b.pathRoot == "" {
		return "", false
	}
	return filepath.Join(b.pathRoot, name), true
}

type w3cEnv struct {
	*env
	wb      *w3cBroker
	home    string
	key     string // the PRIVATE key path (never created: only <key>.pub exists)
	known   string
	pubLine string
	pubFP   string

	cmu    sync.Mutex
	cloned []clone.Options
	// cloneErr, when set, is what the fake clone returns (after recording).
	cloneErr error
	// registryChanges counts OnRegistryChange calls.
	chgMu   sync.Mutex
	changes int
}

func (e *w3cEnv) cloneCalls() []clone.Options {
	e.cmu.Lock()
	defer e.cmu.Unlock()
	return append([]clone.Options(nil), e.cloned...)
}

func (e *w3cEnv) changeCount() int {
	e.chgMu.Lock()
	defer e.chgMu.Unlock()
	return e.changes
}

func newW3cEnv(t *testing.T, opts ...func(*Config)) *w3cEnv {
	t.Helper()
	wb, sock := startW3cBroker(t)
	home := t.TempDir()
	sshDir := shortDir(t)
	line, fp := sshPubLine(t, "orchd@deploy")
	we := &w3cEnv{wb: wb, home: home, key: filepath.Join(sshDir, "id_gitlab_deploy"), known: filepath.Join(sshDir, "known_hosts"), pubLine: line, pubFP: fp}
	if err := os.WriteFile(we.key+".pub", []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(we.known, []byte("gitlab.example "+w3cKnownKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.BrokerSocket = sock
		c.GateHome = home
		c.GitSSHKey = we.key
		c.GitSSHKnownHosts = we.known
		c.OnRegistryChange = func() { we.chgMu.Lock(); we.changes++; we.chgMu.Unlock() }
		c.Policy = func() PolicyView {
			return PolicyView{Hash: "h", Classes: map[string]string{"docs": "L1", "code": "L2", "registered": "L1"}}
		}
		for _, o := range opts {
			o(c)
		}
	})
	e.d.pauseWorkers()
	we.env = e
	wb.pathRoot = e.root
	e.d.handler.cloneFn = func(ctx context.Context, o clone.Options) error {
		we.cmu.Lock()
		we.cloned = append(we.cloned, o)
		err := we.cloneErr
		we.cmu.Unlock()
		if err != nil {
			return err
		}
		if err := os.Mkdir(o.Dest, 0o700); err != nil {
			return clone.ErrExists
		}
		return os.Mkdir(filepath.Join(o.Dest, ".git"), 0o700)
	}
	return we
}

func (e *w3cEnv) sockJSON(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	c, _, b := e.sockDo(t, method, path, body, nil)
	return c, b
}

type w3cProposal struct {
	Action    string         `json:"action"`
	Subject   string         `json:"subject"`
	Fields    map[string]any `json:"fields"`
	Message   string         `json:"message"`
	Change    string         `json:"change"`
	Request   string         `json:"request"`
	Issued    string         `json:"issued"`
	Preview   []string       `json:"preview"`
	DeployKey *struct {
		PublicKey   string `json:"public_key"`
		Fingerprint string `json:"fingerprint"`
	} `json:"deploy_key"`
}

func (e *w3cEnv) proposePath(action string) string {
	if action == "create_project" {
		return "/api/v1/projects/create/propose"
	}
	return "/api/v1/providers/propose"
}

func (e *w3cEnv) applyPath(action string) string {
	if action == "create_project" {
		return "/api/v1/projects/create/apply"
	}
	return "/api/v1/providers/apply"
}

func (e *w3cEnv) propose(t *testing.T, action, subject string, fields map[string]any) w3cProposal {
	t.Helper()
	code, b := e.sockJSON(t, "POST", e.proposePath(action), jsonBody(map[string]any{"action": action, "subject": subject, "fields": fields}))
	if code != 200 {
		t.Fatalf("propose %s: %d %s", action, code, b)
	}
	var p w3cProposal
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *w3cEnv) applyBody(p w3cProposal, sig any) map[string]any {
	fields := map[string]any{}
	for k, v := range p.Fields {
		fields[k] = v
	}
	m := map[string]any{"action": p.Action, "subject": p.Subject, "fields": fields, "change": p.Change, "request": p.Request, "issued": p.Issued}
	if sig != nil {
		m["signature"] = sig
	}
	return m
}

func (e *w3cEnv) apply(t *testing.T, p w3cProposal) (int, []byte) {
	t.Helper()
	return e.sockJSON(t, "POST", e.applyPath(p.Action), jsonBody(e.applyBody(p, webauthnSig)))
}

// fieldsFor returns valid proposal fields for each W3c action.
func (e *w3cEnv) fieldsFor(action string) (subject string, fields map[string]any) {
	switch action {
	case "set_provider":
		return "gl", map[string]any{"kind": "gitlab", "base_url": "http://gitlab.example:18082", "ssh_url": "ssh://git@gitlab.example", "token_ref": "gl-admin"}
	case "delete_provider":
		return "gl", map[string]any{}
	case "create_group":
		return "gl", map[string]any{"name": "newgroup", "path": "newgroup", "parent_path": "team"}
	default:
		return "app-one", map[string]any{"provider": "gl", "group_path": "team", "template": "go", "description": "첫 프로젝트", "ai_context": false, "publish": "write"}
	}
}

func (e *w3cEnv) proposeAction(t *testing.T, action string) w3cProposal {
	s, f := e.fieldsFor(action)
	return e.propose(t, action, s, f)
}

var w3cActions = []string{"set_provider", "delete_provider", "create_group", "create_project"}

func sha256Hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// ledgerEvents returns the payloads of one ledger event type.
func (e *w3cEnv) ledgerPayloads(t *testing.T, typ string) []map[string]any {
	t.Helper()
	return ledgerPayloadsOf(t, policy.OrchLedgerPath(e.home), typ)
}

func ledgerKeys(m map[string]any) string {
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func ledgerPayloadsOf(t *testing.T, path, typ string) []map[string]any {
	t.Helper()
	evs, err := ledger.Events(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev.Payload)
		}
	}
	return out
}

var _ = registrymsg.ActionCreateProject

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
