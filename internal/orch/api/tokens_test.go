package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"aigate/internal/approval/signing"
	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/registrymsg"
)

// Token endpoints (W3b B4). The fake broker is regBroker (projects_test.go): it
// enforces the REAL wire key sets of set_token/delete_token, records every
// call and answers from a script. Nothing here reaches a real service. The
// canary below is an obvious dummy; its last four characters (ZQxw) are not
// hex so a hash chain in a ledger cannot contain them by accident.

const (
	canaryValue = "canary-NOT-A-REAL-TOKEN-0123456789abcdefghij-ZQxw"
	canaryLast4 = "ZQxw"
	tokExpires  = "2099-12-31"
)

var (
	setTokenWireKeys    = []string{"ref", "kind", "expires", "value", "value_hash", "change", "request", "issued", "signature"}
	deleteTokenWireKeys = []string{"ref", "change", "request", "issued", "signature"}
)

// serveTokenWrite answers set_token/delete_token like the broker's strict
// decoder: exactly the wire keys, otherwise bad_request.
func (b *regBroker) serveTokenWrite(c interface{ Write([]byte) (int, error) }, op string, args json.RawMessage) {
	b.mu.Lock()
	fn := b.tokenReply
	b.mu.Unlock()
	var m map[string]json.RawMessage
	want := setTokenWireKeys
	if op == "delete_token" {
		want = deleteTokenWireKeys
	}
	bad := json.Unmarshal(args, &m) != nil || len(m) != len(want)
	for _, k := range want {
		if _, ok := m[k]; !ok {
			bad = true
		}
	}
	if bad {
		c.Write([]byte(`{"ok":false,"code":"bad_request"}` + "\n"))
		return
	}
	if fn != nil {
		r := fn(op, args)
		if r == "DROP" {
			if cl, ok := c.(interface{ Close() error }); ok {
				cl.Close()
			}
			return
		}
		if r != "" {
			c.Write([]byte(r + "\n"))
			return
		}
	}
	var res any
	if op == "set_token" {
		var a struct{ Ref, Kind, Expires, Value string }
		_ = json.Unmarshal(args, &a)
		l4 := a.Value
		if len(l4) > 4 {
			l4 = l4[len(l4)-4:]
		}
		res = map[string]any{"ref": a.Ref, "kind": a.Kind, "last4": l4, "expires": a.Expires, "set_at": "2026-11-10T01:02:03Z",
			"source": "store", "days_left": 30, "usable": true}
	} else {
		var a struct{ Ref string }
		_ = json.Unmarshal(args, &a)
		res = map[string]any{"ref": a.Ref, "deleted": true}
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "result": res})
	c.Write(append(out, '\n'))
}

func (b *regBroker) tokenWrites() []regCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []regCall
	for _, c := range b.calls {
		if c.Op == "set_token" || c.Op == "delete_token" {
			out = append(out, c)
		}
	}
	return out
}

func (b *regBroker) setTokenReply(fn func(op string, args json.RawMessage) string) {
	b.mu.Lock()
	b.tokenReply = fn
	b.mu.Unlock()
}

// syncBuf is a log sink safe for the daemon's goroutines.
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

func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

type tokEnv struct {
	*projEnv
	logs *syncBuf
	clk  *testClock
}

func newTokEnv(t *testing.T) *tokEnv {
	t.Helper()
	logs := captureLog(t)
	pe := newProjEnv(t)
	clk := &testClock{}
	pe.d.handler.now = clk.now
	return &tokEnv{projEnv: pe, logs: logs, clk: clk}
}

func (e *tokEnv) call(t *testing.T, path, body string) (int, []byte) {
	t.Helper()
	return e.sock(t, "POST", "/api/v1/tokens/"+path, body)
}

type tokFlow struct {
	Action, Subject, Change, Message, Request, Issued string
	Fields                                            map[string]any
	Value                                             string
	Preview                                           []string
}

// flow runs begin and message for set_token (value != "") or delete_token.
func (e *tokEnv) flow(t *testing.T, action, subject, kind, value string) tokFlow {
	t.Helper()
	f := tokFlow{Action: action, Subject: subject, Value: value}
	beginFields := map[string]any{}
	if action == "set_token" {
		beginFields = map[string]any{"kind": kind, "expires": tokExpires}
	}
	code, b := e.call(t, "begin", jsonBody(map[string]any{"action": action, "subject": subject, "fields": beginFields}))
	if code != 200 {
		t.Fatalf("begin %d %s", code, b)
	}
	var bo struct{ Request, Issued string }
	if err := json.Unmarshal(b, &bo); err != nil {
		t.Fatal(err)
	}
	f.Request, f.Issued = bo.Request, bo.Issued
	f.Fields = beginFields
	if action == "set_token" {
		f.Fields = map[string]any{"kind": kind, "expires": tokExpires, "value_hash": registrymsg.ValueHash(f.Request, value)}
	}
	code, b = e.call(t, "message", jsonBody(map[string]any{"action": action, "subject": subject, "fields": f.Fields,
		"request": f.Request, "issued": f.Issued}))
	if code != 200 {
		t.Fatalf("message %d %s", code, b)
	}
	var mo struct {
		Message, Change, Request, Issued string
		Preview                          []string
	}
	if err := json.Unmarshal(b, &mo); err != nil {
		t.Fatal(err)
	}
	f.Message, f.Change, f.Preview = mo.Message, mo.Change, mo.Preview
	return f
}

func (f tokFlow) body(sig any) map[string]any {
	m := map[string]any{"action": f.Action, "subject": f.Subject, "fields": f.Fields, "change": f.Change,
		"request": f.Request, "issued": f.Issued}
	if sig != nil {
		m["signature"] = sig
	}
	if f.Action == "set_token" {
		m["value"] = f.Value
	}
	return m
}

func (e *tokEnv) setFlow(t *testing.T) tokFlow {
	t.Helper()
	return e.flow(t, "set_token", "gitlab-main", "gitlab", canaryValue)
}

func (e *tokEnv) apply(t *testing.T, f tokFlow) (int, []byte) {
	t.Helper()
	return e.call(t, "apply", jsonBody(f.body(webauthnSig)))
}

func (e *tokEnv) ledgerRaw(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(policy.OrchLedgerPath(e.home))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(raw)
}

func TestTokensListMapsBrokerAndHidesNothingExtra(t *testing.T) {
	e := newTokEnv(t)
	e.rb.mu.Lock()
	e.rb.tokenRows = []map[string]any{
		{"ref": "openai", "kind": "openai", "last4": "", "expires": "", "set_at": "", "source": "config", "days_left": nil, "usable": true,
			"future_field": "SHOULD-NOT-PASS"},
		{"ref": "gitlab-main", "kind": "gitlab", "last4": "ZQxw", "expires": "2026-11-20", "set_at": "2026-11-10T01:02:03Z",
			"source": "store", "days_left": 10, "usable": false},
	}
	e.rb.mu.Unlock()
	code, b := e.sock(t, "GET", "/api/v1/settings/tokens", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var out struct {
		Tokens   []map[string]any `json:"tokens"`
		WarnDays int              `json:"warn_days"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.WarnDays != 14 || len(out.Tokens) != 2 {
		t.Fatalf("%s", b)
	}
	if strings.Contains(string(b), "SHOULD-NOT-PASS") || strings.Contains(string(b), "future_field") {
		t.Fatalf("unknown broker field passed through: %s", b)
	}
	r0, r1 := out.Tokens[0], out.Tokens[1]
	if r0["source"] != "config" || r0["usable"] != true || r0["days_left"] != nil || r0["ref"] != "openai" {
		t.Errorf("row0 %v", r0)
	}
	if r1["source"] != "store" || r1["usable"] != false || r1["days_left"] != float64(10) || r1["last4"] != "ZQxw" || r1["expires"] != "2026-11-20" {
		t.Errorf("row1 %v", r1)
	}
	for _, r := range out.Tokens {
		keys := []string{}
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "days_left,expires,kind,last4,ref,set_at,source,usable" {
			t.Errorf("row keys %v", keys)
		}
	}
	// A row that does not look like a token makes the list broker_error (all or nothing).
	for name, bad := range map[string]map[string]any{
		"bad ref":    {"ref": "BAD REF", "kind": "gitlab", "last4": "", "expires": "", "set_at": "", "source": "store", "days_left": nil, "usable": true},
		"bad kind":   {"ref": "a", "kind": "zzz", "last4": "", "expires": "", "set_at": "", "source": "store", "days_left": nil, "usable": true},
		"bad source": {"ref": "a", "kind": "gitlab", "last4": "", "expires": "", "set_at": "", "source": "evil", "days_left": nil, "usable": true},
		"long last4": {"ref": "a", "kind": "gitlab", "last4": "abcdefgh", "expires": "", "set_at": "", "source": "store", "days_left": nil, "usable": true},
		"bad date":   {"ref": "a", "kind": "gitlab", "last4": "", "expires": "tomorrow", "set_at": "", "source": "store", "days_left": nil, "usable": true},
	} {
		e.rb.mu.Lock()
		e.rb.tokenRows = []map[string]any{bad}
		e.rb.mu.Unlock()
		if code, b := e.sock(t, "GET", "/api/v1/settings/tokens", ""); code != 502 || v1Code(b) != "broker_error" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	e.rb.mu.Lock()
	e.rb.tokensFail = "tokens_unavailable"
	e.rb.mu.Unlock()
	if code, b := e.sock(t, "GET", "/api/v1/settings/tokens", ""); code != 503 || v1Code(b) != "tokens_unavailable" || strings.Contains(string(b), "SECRET-BROKER-TEXT") {
		t.Errorf("tokens_unavailable: %d %s", code, b)
	}
	for _, p := range []string{"/api/v1/settings/tokens", "/api/v1/settings/deploy-key"} {
		if code, _, _ := httpDo(t, e.httpServer(t), "GET", p, "", hdr{}); code != 401 {
			t.Errorf("%s without credentials: %d", p, code)
		}
	}
}

func TestBeginAndMessageNeverReceiveValue(t *testing.T) {
	e := newTokEnv(t)
	req := registrymsg.NewRequestID()
	issued := time.Now().UTC().Format(registryTimeFormat)
	bodies := map[string]map[string]string{
		"begin": {
			"top-level value":     `{"action":"set_token","subject":"gitlab-main","fields":{"kind":"gitlab","expires":""},"value":"` + canaryValue + `"}`,
			"value in fields":     `{"action":"set_token","subject":"gitlab-main","fields":{"kind":"gitlab","expires":"","value":"` + canaryValue + `"}}`,
			"upper-case Value":    `{"action":"set_token","subject":"gitlab-main","fields":{"kind":"gitlab","expires":""},"Value":"` + canaryValue + `"}`,
			"value on delete":     `{"action":"delete_token","subject":"gitlab-main","fields":{},"value":"` + canaryValue + `"}`,
			"null value":          `{"action":"delete_token","subject":"gitlab-main","fields":{},"value":null}`,
			"value in fields del": `{"action":"delete_token","subject":"gitlab-main","fields":{"value":"` + canaryValue + `"}}`,
		},
		"message": {
			"top-level value": `{"action":"set_token","subject":"gitlab-main","fields":{"kind":"gitlab","expires":"","value_hash":"` + strings.Repeat("a", 64) + `"},"request":"` + req + `","issued":"` + issued + `","value":"` + canaryValue + `"}`,
			"value in fields": `{"action":"set_token","subject":"gitlab-main","fields":{"kind":"gitlab","expires":"","value_hash":"` + strings.Repeat("a", 64) + `","value":"` + canaryValue + `"},"request":"` + req + `","issued":"` + issued + `"}`,
			"value on delete": `{"action":"delete_token","subject":"gitlab-main","fields":{},"request":"` + req + `","issued":"` + issued + `","value":"` + canaryValue + `"}`,
		},
	}
	for ep, cases := range bodies {
		for name, body := range cases {
			code, b := e.call(t, ep, body)
			if code != 400 || v1Code(b) != "bad_request" {
				t.Errorf("%s %s: %d %s", ep, name, code, b)
			}
			if strings.Contains(string(b), canaryValue) {
				t.Errorf("%s %s echoed the value: %s", ep, name, b)
			}
		}
	}
	if n := len(e.rb.ops()); n != 0 {
		t.Errorf("broker called: %v", e.rb.ops())
	}
	if strings.Contains(e.logs.String(), canaryValue) || strings.Contains(e.ledgerRaw(t), canaryValue) {
		t.Error("value in log or ledger")
	}
}

func TestBeginReturnsOnlyRequestAndIssued(t *testing.T) {
	e := newTokEnv(t)
	// A clock with a fractional second: issued must be whole seconds, UTC.
	at := time.Date(2026, 11, 10, 3, 4, 5, 987654321, time.FixedZone("KST", 9*3600))
	e.d.handler.now = func() time.Time { return at }
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		code, b := e.call(t, "begin", jsonBody(map[string]any{"action": "set_token", "subject": "gitlab-main",
			"fields": map[string]any{"kind": "gitlab", "expires": tokExpires}}))
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if len(m) != 2 || m["request"] == nil || m["issued"] == nil {
			t.Fatalf("keys: %s", b)
		}
		req, _ := m["request"].(string)
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(req) || seen[req] {
			t.Fatalf("request %q (repeat=%v)", req, seen[req])
		}
		seen[req] = true
		if m["issued"] != "2026-11-09T18:04:05Z" {
			t.Fatalf("issued %v", m["issued"])
		}
	}
	// delete_token takes {} fields.
	if code, b := e.call(t, "begin", jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}})); code != 200 {
		t.Fatalf("delete begin %d %s", code, b)
	}
	if n := len(e.rb.ops()); n != 0 {
		t.Errorf("begin called the broker: %v", e.rb.ops())
	}
	if raw := e.ledgerRaw(t); raw != "" {
		t.Errorf("begin wrote the ledger: %s", raw)
	}
	// Nothing is stored: the store tables hold no trace, and the daemon directory has no new file.
	bad := map[string]string{
		"unknown kind":        `{"action":"set_token","subject":"a","fields":{"kind":"zzz","expires":""}}`,
		"openai other name":   `{"action":"set_token","subject":"a","fields":{"kind":"openai","expires":""}}`,
		"gitlab named openai": `{"action":"set_token","subject":"openai","fields":{"kind":"gitlab","expires":""}}`,
		"past expiry":         `{"action":"set_token","subject":"a","fields":{"kind":"gitlab","expires":"2020-01-01"}}`,
		"bad expiry":          `{"action":"set_token","subject":"a","fields":{"kind":"gitlab","expires":"2099-02-30"}}`,
		"missing expires":     `{"action":"set_token","subject":"a","fields":{"kind":"gitlab"}}`,
		"value_hash at begin": `{"action":"set_token","subject":"a","fields":{"kind":"gitlab","expires":"","value_hash":"` + strings.Repeat("a", 64) + `"}}`,
		"delete with fields":  `{"action":"delete_token","subject":"a","fields":{"kind":"gitlab"}}`,
		"project action":      `{"action":"set_flag","subject":"a","fields":{"flag":"ai_context","value":true}}`,
		"bad subject":         `{"action":"set_token","subject":"A b","fields":{"kind":"gitlab","expires":""}}`,
		"hostile subject":     "{\"action\":\"delete_token\",\"subject\":\"a\\u202eb\",\"fields\":{}}",
		"unknown top key":     `{"action":"delete_token","subject":"a","fields":{},"extra":1}`,
		"duplicate key":       `{"action":"delete_token","subject":"a","subject":"b","fields":{}}`,
		"upper-case key":      `{"Action":"delete_token","subject":"a","fields":{}}`,
		"missing fields":      `{"action":"delete_token","subject":"a"}`,
		"trailing data":       `{"action":"delete_token","subject":"a","fields":{}} {}`,
		"not json":            `nope`,
		"empty":               ``,
	}
	for name, body := range bad {
		code, b := e.call(t, "begin", body)
		want := 400
		if code != want {
			t.Errorf("%s: %d %s", name, code, b)
		}
		wantCode := "bad_request"
		switch name {
		case "unknown kind":
			wantCode = "bad_kind"
		case "openai other name", "gitlab named openai":
			wantCode = "openai_singleton"
		}
		if v1Code(b) != wantCode {
			t.Errorf("%s: code %s (want %s): %s", name, v1Code(b), wantCode, b)
		}
	}
}

func TestMessageBuildsCanonicalAndStoresNothing(t *testing.T) {
	e := newTokEnv(t)
	f := e.setFlow(t)
	want, err := registrymsg.ChangeHash("set_token", "gitlab-main", f.Fields)
	if err != nil || f.Change != want {
		t.Fatalf("change %q, want %q (%v)", f.Change, want, err)
	}
	issued, _ := time.Parse(registryTimeFormat, f.Issued)
	wantMsg := signing.RegistryMessage{Action: "set_token", Subject: "gitlab-main", Change: want, Request: f.Request, Issued: issued}.Text()
	if f.Message != wantMsg {
		t.Fatalf("message %q != %q", f.Message, wantMsg)
	}
	hash := f.Fields["value_hash"].(string)
	all := strings.Join(f.Preview, "\n")
	if len(f.Preview) < 3 || !strings.Contains(all, "gitlab-main") || !strings.Contains(all, "gitlab") || !strings.Contains(all, tokExpires) {
		t.Fatalf("preview:\n%s", all)
	}
	for _, leak := range []string{canaryValue, canaryLast4, hash, "canary"} {
		if strings.Contains(all, leak) || strings.Contains(f.Message, leak) {
			t.Errorf("%q in the preview or message", leak)
		}
	}
	// Exact response keys.
	code, b := e.call(t, "message", jsonBody(map[string]any{"action": "set_token", "subject": "gitlab-main", "fields": f.Fields, "request": f.Request, "issued": f.Issued}))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if code != 200 || strings.Join(keys, ",") != "change,issued,message,preview,request" {
		t.Fatalf("%d %s", code, b)
	}
	if strings.Contains(string(b), hash) {
		t.Errorf("the value hash is echoed: %s", b)
	}
	// delete: names the ref, says the signature carries no kind.
	d := e.flow(t, "delete_token", "gitlab-main", "", "")
	dall := strings.Join(d.Preview, "\n")
	if !strings.Contains(dall, "gitlab-main") || !strings.Contains(dall, "종류") {
		t.Fatalf("delete preview:\n%s", dall)
	}
	dwant, _ := registrymsg.ChangeHash("delete_token", "gitlab-main", map[string]any{})
	if d.Change != dwant {
		t.Fatalf("delete change %q != %q", d.Change, dwant)
	}
	if o := e.d.handler; o == nil {
		t.Fatal("no handler")
	}
	if n := len(e.rb.ops()); n != 0 {
		t.Errorf("broker called: %v", e.rb.ops())
	}
	if raw := e.ledgerRaw(t); raw != "" {
		t.Errorf("ledger written: %s", raw)
	}
	// Refused inputs of message.
	for name, body := range map[string]string{
		"bad request id":  jsonBody(map[string]any{"action": "set_token", "subject": "a", "fields": f.Fields, "request": "XYZ", "issued": f.Issued}),
		"bad issued":      jsonBody(map[string]any{"action": "set_token", "subject": "a", "fields": f.Fields, "request": f.Request, "issued": "2026-11-10 01:02:03"}),
		"missing hash":    jsonBody(map[string]any{"action": "set_token", "subject": "a", "fields": map[string]any{"kind": "gitlab", "expires": ""}, "request": f.Request, "issued": f.Issued}),
		"short hash":      jsonBody(map[string]any{"action": "set_token", "subject": "a", "fields": map[string]any{"kind": "gitlab", "expires": "", "value_hash": "ab"}, "request": f.Request, "issued": f.Issued}),
		"openai mismatch": jsonBody(map[string]any{"action": "set_token", "subject": "a", "fields": map[string]any{"kind": "openai", "expires": "", "value_hash": hash}, "request": f.Request, "issued": f.Issued}),
		"past expiry":     jsonBody(map[string]any{"action": "set_token", "subject": "a", "fields": map[string]any{"kind": "gitlab", "expires": "2020-01-01", "value_hash": hash}, "request": f.Request, "issued": f.Issued}),
		"no request":      jsonBody(map[string]any{"action": "set_token", "subject": "a", "fields": f.Fields, "issued": f.Issued}),
	} {
		if code, b := e.call(t, "message", body); code != 400 {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
}

func TestTokensApplyRecomputesChangeAndRefusesMismatch(t *testing.T) {
	e := newTokEnv(t)
	f := e.setFlow(t)
	cases := map[string]func(m map[string]any){
		"other change":      func(m map[string]any) { m["change"] = strings.Repeat("0", 64) },
		"other expires":     func(m map[string]any) { m["fields"].(map[string]any)["expires"] = "2099-12-30" },
		"other kind":        func(m map[string]any) { m["fields"].(map[string]any)["kind"] = "github" },
		"other subject":     func(m map[string]any) { m["subject"] = "gitlab-other" },
		"other value_hash":  func(m map[string]any) { m["fields"].(map[string]any)["value_hash"] = strings.Repeat("1", 64) },
		"change of delete":  func(m map[string]any) { m["action"] = "delete_token"; m["fields"] = map[string]any{} },
		"upper-case change": func(m map[string]any) { m["change"] = strings.ToUpper(f.Change) },
		"short change":      func(m map[string]any) { m["change"] = f.Change[:63] },
	}
	for name, mod := range cases {
		fl := f
		m := fl.body(webauthnSig)
		fm := map[string]any{}
		for k, v := range f.Fields {
			fm[k] = v
		}
		m["fields"] = fm
		mod(m)
		code, b := e.call(t, "apply", jsonBody(m))
		if code != 400 || (v1Code(b) != "change_mismatch" && v1Code(b) != "bad_request") {
			t.Errorf("%s: %d %s", name, code, b)
		}
		if strings.Contains(string(b), canaryValue) {
			t.Errorf("%s: value echoed", name)
		}
	}
	if n := len(e.rb.ops()); n != 0 {
		t.Fatalf("broker called for a mismatching change: %v", e.rb.ops())
	}
	// A client-sent change that differs from the recomputed one is change_mismatch exactly.
	m := f.body(webauthnSig)
	m["change"] = strings.Repeat("0", 64)
	if code, b := e.call(t, "apply", jsonBody(m)); code != 400 || v1Code(b) != "change_mismatch" {
		t.Errorf("%d %s", code, b)
	}
	// A value that is not the signed one is refused before the broker (no ID burned).
	g := e.setFlow(t)
	g.Value = canaryValue + "-other"
	if code, b := e.apply(t, g); code != 400 || v1Code(b) != "value_mismatch" {
		t.Errorf("value vs value_hash: %d %s", code, b)
	}
	if n := len(e.rb.ops()); n != 0 {
		t.Fatalf("broker called for a wrong value: %v", e.rb.ops())
	}
	// The honest request goes through.
	if code, b := e.apply(t, f); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	// The change in the signed message equals what the server recomputed.
	if ws := e.rb.tokenWrites(); len(ws) != 1 {
		t.Fatalf("%d writes", len(ws))
	} else {
		var a map[string]any
		_ = json.Unmarshal(ws[0].Args, &a)
		if a["change"] != f.Change {
			t.Errorf("forwarded change %v != %s", a["change"], f.Change)
		}
	}
}

// TestSetTokenForwardsExactArgs pins the real wire contract: ref, kind,
// expires, value, value_hash (verbatim from fields), request, issued, change
// and the signature, nothing else; delete_token carries no value.
func TestSetTokenForwardsExactArgs(t *testing.T) {
	e := newTokEnv(t)
	f := e.setFlow(t)
	if code, b := e.apply(t, f); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	d := e.flow(t, "delete_token", "gitlab-main", "", "")
	if code, b := e.apply(t, d); code != 200 || !strings.Contains(string(b), `"deleted":true`) {
		t.Fatalf("%d %s", code, b)
	}
	ws := e.rb.tokenWrites()
	if len(ws) != 2 || ws[0].Op != "set_token" || ws[1].Op != "delete_token" {
		t.Fatalf("%v", ws)
	}
	var a map[string]any
	_ = json.Unmarshal(ws[0].Args, &a)
	for k, want := range map[string]any{"ref": "gitlab-main", "kind": "gitlab", "expires": tokExpires, "value": canaryValue,
		"value_hash": f.Fields["value_hash"], "request": f.Request, "issued": f.Issued, "change": f.Change} {
		if a[k] != want {
			t.Errorf("set_token arg %s = %v, want %v", k, a[k], want)
		}
	}
	if a["value_hash"] != registrymsg.ValueHash(f.Request, canaryValue) {
		t.Error("value_hash is not the hash of request:value")
	}
	if sig, _ := a["signature"].(map[string]any); sig["signer"] != "alice" || sig["alg"] != "webauthn-es256" {
		t.Errorf("signature %v", a["signature"])
	}
	var da map[string]any
	_ = json.Unmarshal(ws[1].Args, &da)
	if _, has := da["value"]; has || len(da) != 5 || da["ref"] != "gitlab-main" {
		t.Errorf("delete_token args %v", da)
	}
	// Response of set_token: the row of the new state, no value, no hash.
	g := e.setFlow(t)
	code, b := e.apply(t, g)
	var row map[string]any
	_ = json.Unmarshal(b, &row)
	if code != 200 || row["ref"] != "gitlab-main" || row["kind"] != "gitlab" || row["last4"] != canaryLast4 || row["usable"] != true || row["source"] != "store" {
		t.Fatalf("%d %s", code, b)
	}
	for _, leak := range []string{canaryValue, g.Fields["value_hash"].(string), g.Change, g.Request} {
		if strings.Contains(string(b), leak) {
			t.Errorf("%q in the response", leak)
		}
	}
}

// brokerCodes every apply error path of the canary test goes through.
var tokenBrokerCodes = []string{"not_signed", "bad_signature", "unknown_signer", "stale_signature", "signers_unavailable", "request_reused",
	"change_mismatch", "token_unknown", "bad_kind", "token_too_short", "token_bad_format", "value_mismatch", "token_exists_other_kind",
	"tokens_unavailable", "openai_singleton", "registry_unavailable", "bad_request", "too_large", "busy", "forbidden", "unknown_op",
	"timeout", "internal_error", "totally_unknown"}

func TestApplyForwardsValueExactlyOnceAndNeverLogsIt(t *testing.T) {
	e := newTokEnv(t)
	e.d.handler.tokenPrepLimit = newActorLimiter(10000, time.Hour)
	e.d.handler.tokenApplyLimit = newActorLimiter(10000, time.Hour)
	var responses [][]byte
	record := func(b []byte) { responses = append(responses, append([]byte(nil), b...)) }

	// Success: the broker receives the exact bytes, once.
	f := e.setFlow(t)
	code, b := e.apply(t, f)
	record(b)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	ws := e.rb.tokenWrites()
	if len(ws) != 1 {
		t.Fatalf("%d writes", len(ws))
	}
	var a struct{ Value string }
	_ = json.Unmarshal(ws[0].Args, &a)
	if a.Value != canaryValue || bytes.Count(ws[0].Args, []byte(canaryValue)) != 1 {
		t.Fatalf("the broker saw %q (count %d)", a.Value, bytes.Count(ws[0].Args, []byte(canaryValue)))
	}
	// A value with JSON-escaped characters arrives byte-identical too.
	odd := `ab"cd\ef/gh<i>&j` + strings.Repeat("Z", 20)
	g := e.flow(t, "set_token", "gitlab-odd", "gitlab", odd)
	if code, b := e.apply(t, g); code != 200 {
		t.Fatalf("%d %s", code, b)
	} else {
		record(b)
	}
	ws = e.rb.tokenWrites()
	var ao struct{ Value string }
	_ = json.Unmarshal(ws[len(ws)-1].Args, &ao)
	if ao.Value != odd {
		t.Fatalf("escaped value arrived as %q", ao.Value)
	}

	// Every broker error code, on set_token and delete_token.
	for _, code := range tokenBrokerCodes {
		code := code
		e.rb.setTokenReply(func(string, json.RawMessage) string { return brokerErrReply(code) })
		for _, fl := range []tokFlow{e.setFlow(t), e.flow(t, "delete_token", "gitlab-main", "", "")} {
			c, b := e.apply(t, fl)
			record(b)
			if c == 200 {
				t.Errorf("%s: unexpected success", code)
			}
		}
	}
	// A dropped connection and garbage from the broker.
	e.rb.setTokenReply(func(string, json.RawMessage) string { return "DROP" })
	_, b = e.apply(t, e.setFlow(t))
	record(b)
	e.rb.setTokenReply(func(string, json.RawMessage) string { return "SECRET-BROKER-TEXT " + canaryValue + " not json" })
	_, b = e.apply(t, e.setFlow(t))
	record(b)
	e.rb.setTokenReply(func(string, json.RawMessage) string {
		return `{"ok":true,"result":{"ref":"gitlab-main","kind":"gitlab","last4":"` + canaryValue + `","expires":"","set_at":"","source":"store","days_left":null,"usable":true}}`
	})
	_, b = e.apply(t, e.setFlow(t))
	record(b)
	e.rb.setTokenReply(nil)

	// Local refusals.
	h := e.setFlow(t)
	for name, mod := range map[string]func(m map[string]any){
		"no signature":    func(m map[string]any) { delete(m, "signature") },
		"null signature":  func(m map[string]any) { m["signature"] = nil },
		"bad change":      func(m map[string]any) { m["change"] = strings.Repeat("0", 64) },
		"missing value":   func(m map[string]any) { delete(m, "value") },
		"empty value":     func(m map[string]any) { m["value"] = "" },
		"short value":     func(m map[string]any) { m["value"] = canaryValue[:20] },
		"space in value":  func(m map[string]any) { m["value"] = canaryValue + " " },
		"unicode value":   func(m map[string]any) { m["value"] = canaryValue + "한" },
		"newline value":   func(m map[string]any) { m["value"] = canaryValue + "\n" },
		"long value":      func(m map[string]any) { m["value"] = canaryValue + strings.Repeat("x", 600) },
		"number value":    func(m map[string]any) { m["value"] = 12345 },
		"array value":     func(m map[string]any) { m["value"] = []string{canaryValue} },
		"unknown key":     func(m map[string]any) { m["extra"] = canaryValue },
		"value in fields": func(m map[string]any) { m["fields"].(map[string]any)["value"] = canaryValue },
		"other value":     func(m map[string]any) { m["value"] = canaryValue + "-other" },
	} {
		m := h.body(webauthnSig)
		fm := map[string]any{}
		for k, v := range h.Fields {
			fm[k] = v
		}
		m["fields"] = fm
		mod(m)
		c, b := e.call(t, "apply", jsonBody(m))
		record(b)
		if c == 200 || c >= 500 {
			t.Errorf("%s: %d %s", name, c, b)
		}
	}
	// Too big, not JSON, bad UTF-8.
	_, b = e.call(t, "apply", `{"value":"`+canaryValue+strings.Repeat("a", 5000)+`"}`)
	record(b)
	_, b = e.call(t, "apply", canaryValue+"{")
	record(b)
	_, b = e.call(t, "apply", `{"value":"`+canaryValue+"\xff\xfe\"}")
	record(b)
	// A delete with a value is refused.
	d := e.flow(t, "delete_token", "gitlab-main", "", "")
	dm := d.body(webauthnSig)
	dm["value"] = canaryValue
	c, b := e.call(t, "apply", jsonBody(dm))
	record(b)
	if c != 400 {
		t.Errorf("delete with value: %d %s", c, b)
	}
	// Rate limited.
	e.d.handler.tokenApplyLimit = newActorLimiter(0, time.Hour)
	_, b = e.apply(t, e.setFlow(t))
	record(b)
	e.d.handler.tokenApplyLimit = newActorLimiter(10000, time.Hour)
	// Panic recovery with the value on the stack.
	e.d.handler.tokenSeams.panicHook = func() { panic("boom " + canaryValue) }
	c, b = e.apply(t, e.setFlow(t))
	record(b)
	if c != 500 || v1Code(b) != "internal_error" {
		t.Errorf("panic: %d %s", c, b)
	}
	e.d.handler.tokenSeams.panicHook = nil
	if !strings.Contains(e.logs.String(), "tokens apply panic") {
		t.Errorf("the panic left no trace (type only) in the log:\n%s", e.logs.String())
	}
	// Broker down (never reached).
	dead := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.BrokerSocket = filepath.Join(shortDir(t), "none.sock")
		c.GateHome = t.TempDir()
	})
	dead.d.pauseWorkers()
	dm2 := jsonBody(f.body(webauthnSig))
	_, _, b = dead.sockDo(t, "POST", "/api/v1/tokens/apply", dm2, nil)
	record(b)

	// Nothing leaks anywhere orchd writes: responses, logs, ledger, home.
	for i, r := range responses {
		if bytes.Contains(r, []byte(canaryValue)) || bytes.Contains(r, []byte(canaryValue[:30])) || bytes.Contains(r, []byte("SECRET-BROKER-TEXT")) {
			t.Errorf("response %d leaks: %s", i, r)
		}
	}
	for name, hay := range map[string]string{"log": e.logs.String(), "ledger": e.ledgerRaw(t)} {
		for _, needle := range []string{canaryValue, canaryValue[:30], odd, "SECRET-BROKER-TEXT"} {
			if strings.Contains(hay, needle) {
				t.Errorf("%s contains %q", name, needle)
			}
		}
	}
	filepath.Walk(e.home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			raw, _ := os.ReadFile(p)
			if bytes.Contains(raw, []byte(canaryValue)) {
				t.Errorf("%s contains the value", p)
			}
		}
		return nil
	})
}

func TestSecretValueRedactsEveryFormat(t *testing.T) {
	v := secretValue(canaryValue)
	req := tokenApplyRequest{Action: "set_token", Subject: "a", Value: v}
	outs := []string{
		fmt.Sprintf("%v|%+v|%#v|%s|%q|%x|%X|%d|%T", v, v, v, v, v, v, v, v, v),
		fmt.Sprintf("%v|%+v|%#v", req, req, req),
		fmt.Sprintf("%v|%+v|%#v", &req, &req, &req),
		fmt.Sprint(v), fmt.Sprintln(v), v.String(), v.GoString(),
	}
	if b, err := json.Marshal(req); err == nil {
		outs = append(outs, string(b))
	}
	if b, err := json.Marshal(map[string]any{"v": v}); err == nil {
		outs = append(outs, string(b))
	}
	if b, err := v.MarshalText(); err == nil {
		outs = append(outs, string(b))
	}
	for i, o := range outs {
		if strings.Contains(o, canaryValue) || strings.Contains(o, canaryLast4) {
			t.Errorf("format %d leaks: %s", i, o)
		}
	}
	if !strings.Contains(v.String(), "[redacted]") {
		t.Errorf("String() = %q", v.String())
	}
}

func TestTokensApplyBodyCap4KiB(t *testing.T) {
	e := newTokEnv(t)
	f := e.setFlow(t)
	raw, _ := json.Marshal(f.body(webauthnSig))
	if len(raw) > 4096 {
		t.Fatalf("a normal apply body is %d bytes: the cap would break it", len(raw))
	}
	// Exactly 4096 bytes (padding spaces after the object) is read.
	exact := append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), 4096-len(raw))...)
	if code, b := e.call(t, "apply", string(exact)); code != 200 {
		t.Fatalf("4096 bytes: %d %s", code, b)
	}
	// One more byte is 413, before any broker call.
	g := e.setFlow(t)
	graw, _ := json.Marshal(g.body(webauthnSig))
	over := append(append([]byte{}, graw...), bytes.Repeat([]byte(" "), 4097-len(graw))...)
	before := len(e.rb.tokenWrites())
	code, b := e.call(t, "apply", string(over))
	if code != 413 || v1Code(b) != "body_too_large" || strings.Contains(string(b), canaryValue) {
		t.Fatalf("4097 bytes: %d %s", code, b)
	}
	if len(e.rb.tokenWrites()) != before {
		t.Fatal("broker called for an oversized body")
	}
	// A hugely oversized body with the value at the front.
	if code, b := e.call(t, "apply", `{"value":"`+canaryValue+`"`+strings.Repeat(" ", 1<<20)+`}`); code != 413 {
		t.Fatalf("huge body: %d %s", code, b)
	}
	// begin and message keep their own small cap (not 4 KiB of value space).
	if code, _ := e.call(t, "begin", `{"action":"delete_token","subject":"a","fields":{}}`+strings.Repeat(" ", 70<<10)); code != 413 {
		t.Fatalf("begin cap: %d", code)
	}
}

func TestRequestBodyNotRetainedAfterApply(t *testing.T) {
	e := newTokEnv(t)
	var mu sync.Mutex
	var bodyBuf, valueBuf []byte
	var bodyHeld, valueHeld []byte
	e.d.handler.tokenSeams.buffers = func(body, value []byte) {
		mu.Lock()
		bodyBuf, valueBuf = body, value
		bodyHeld = append([]byte(nil), body...)
		mu.Unlock()
	}
	f := e.setFlow(t)
	if code, b := e.apply(t, f); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	mu.Lock()
	if bodyBuf == nil || valueBuf == nil {
		t.Fatal("the seam was not called: the handler does not use the buffers it should wipe")
	}
	// The value buffer is filled only after the seam call: take its content from what the broker saw.
	valueHeld = []byte(canaryValue)
	_ = bodyHeld
	for name, buf := range map[string][]byte{"request body": bodyBuf, "value": valueBuf} {
		for i, c := range buf {
			if c != 0 {
				t.Fatalf("%s buffer not wiped: byte %d = %q", name, i, c)
			}
		}
		if bytes.Contains(buf, valueHeld) {
			t.Errorf("%s buffer still holds the value", name)
		}
	}
	if len(valueBuf) < len(canaryValue) {
		t.Errorf("value buffer %d bytes: too small to have held the value", len(valueBuf))
	}
	mu.Unlock()
	// Wiped on the refusal paths as well.
	g := e.setFlow(t)
	g.Value = canaryValue + "-other"
	if code, _ := e.apply(t, g); code != 400 {
		t.Fatal(code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range bodyBuf {
		if c != 0 {
			t.Fatal("request body not wiped after a refusal")
		}
	}
}

func TestTokensApplyRequiresWriteTierAndCSRF(t *testing.T) {
	e := newTokEnv(t)
	srv := e.httpServer(t)
	tok, csrf := e.newSession(t, "alice")
	token := e.newToken(t, "mac-ssh")
	for _, ep := range []string{"begin", "message", "apply"} {
		path := "/api/v1/tokens/" + ep
		mk := func() string {
			f := e.flow(t, "delete_token", "gitlab-main", "", "")
			switch ep {
			case "begin":
				return jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}})
			case "message":
				return jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}, "request": f.Request, "issued": f.Issued})
			}
			return jsonBody(f.body(webauthnSig))
		}
		body := mk()
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
				t.Errorf("%s %s: %d %s", ep, name, code, b)
			}
		}
		if len(e.rb.ops()) != before {
			t.Errorf("%s: the broker was called without authority", ep)
		}
		for name, h := range map[string]hdr{
			"session": {"Cookie": cookie(tok), CSRFHeader: csrf},
			"token":   {"Authorization": "Bearer " + token},
		} {
			if code, _, rb := httpDo(t, srv, "POST", path, mk(), h); code != 200 {
				t.Errorf("%s %s: %d %s", ep, name, code, rb)
			}
		}
	}
}

func TestErrorMappingAllNewCodesFixed(t *testing.T) {
	type want struct {
		status int
		code   string
	}
	write := map[string]want{
		"not_signed":              {400, "not_signed"},
		"bad_signature":           {403, "bad_signature"},
		"unknown_signer":          {403, "unknown_signer"},
		"stale_signature":         {403, "stale_signature"},
		"signers_unavailable":     {503, "signers_unavailable"},
		"request_reused":          {409, "request_reused"},
		"change_mismatch":         {400, "change_mismatch"},
		"token_unknown":           {404, "token_unknown"},
		"bad_kind":                {400, "bad_kind"},
		"token_too_short":         {400, "token_too_short"},
		"token_bad_format":        {400, "token_bad_format"},
		"value_mismatch":          {400, "value_mismatch"},
		"token_exists_other_kind": {409, "token_exists_other_kind"},
		"openai_singleton":        {400, "openai_singleton"},
		"tokens_unavailable":      {504, "outcome_unknown"},
		"registry_unavailable":    {504, "outcome_unknown"},
		"timeout":                 {504, "outcome_unknown"},
		"internal_error":          {504, "outcome_unknown"},
		"totally_unknown":         {504, "outcome_unknown"},
		"bad_request":             {400, "bad_request"},
		"too_large":               {400, "bad_request"},
		"busy":                    {503, "broker_unavailable"},
		"forbidden":               {503, "broker_unavailable"},
		"unknown_op":              {503, "broker_unavailable"},
		"not_implemented":         {503, "broker_unavailable"},
	}
	e := newTokEnv(t)
	e.d.handler.tokenApplyLimit = newActorLimiter(1000, time.Hour)
	e.d.handler.tokenPrepLimit = newActorLimiter(1000, time.Hour)
	sentence := map[string]string{}
	for code, w := range write {
		code, w := code, w
		e.rb.setTokenReply(func(string, json.RawMessage) string { return brokerErrReply(code) })
		for _, fl := range []tokFlow{e.setFlow(t), e.flow(t, "delete_token", "gitlab-main", "", "")} {
			c, b := e.apply(t, fl)
			if c != w.status || v1Code(b) != w.code {
				t.Errorf("%s (%s): %d %s, want %d %s", code, fl.Action, c, b, w.status, w.code)
			}
			for _, leak := range []string{"SECRET-BROKER-TEXT", "sk-live", "evil.example", "totally_unknown", canaryValue} {
				if strings.Contains(string(b), leak) {
					t.Errorf("%s: %q echoed: %s", code, leak, b)
				}
			}
			var m struct{ Error struct{ Message string } }
			_ = json.Unmarshal(b, &m)
			if m.Error.Message == "" {
				t.Errorf("%s: no sentence", code)
			}
			if prev, ok := sentence[w.code]; ok && w.code != "bad_request" && prev != m.Error.Message {
				t.Errorf("code %s has two sentences: %q / %q", w.code, prev, m.Error.Message)
			}
			sentence[w.code] = m.Error.Message
		}
	}
	// Every new broker code has its OWN sentence (none is the generic one).
	for _, c := range []string{"token_unknown", "bad_kind", "token_too_short", "token_bad_format", "value_mismatch", "token_exists_other_kind", "openai_singleton"} {
		if sentence[c] == "" || sentence[c] == sentence["bad_request"] {
			t.Errorf("%s has no own sentence", c)
		}
	}
	// A broker whose "ok" reply makes no sense: the write may have happened.
	for name, reply := range map[string]string{
		"not an object":  `{"ok":true,"result":{"ref":5}}`,
		"wrong ref":      `{"ok":true,"result":{"ref":"other","kind":"gitlab","last4":"","expires":"","set_at":"","source":"store","days_left":null,"usable":true}}`,
		"bad row":        `{"ok":true,"result":{"ref":"gitlab-main","kind":"zzz","last4":"","expires":"","set_at":"","source":"store","days_left":null,"usable":true}}`,
		"wrong deletion": `{"ok":true,"result":{"ref":"gitlab-main","deleted":false}}`,
	} {
		reply := reply
		e.rb.setTokenReply(func(string, json.RawMessage) string { return reply })
		fl := e.setFlow(t)
		if name == "wrong deletion" {
			fl = e.flow(t, "delete_token", "gitlab-main", "", "")
		}
		if c, b := e.apply(t, fl); c != 504 || v1Code(b) != "outcome_unknown" {
			t.Errorf("%s: %d %s", name, c, b)
		}
	}
	// The read.
	for code, w := range map[string]want{
		"tokens_unavailable": {503, "tokens_unavailable"}, "internal_error": {502, "broker_error"}, "timeout": {502, "broker_error"},
		"busy": {503, "broker_unavailable"}, "forbidden": {503, "broker_unavailable"}, "totally_unknown": {502, "broker_error"},
	} {
		e.rb.mu.Lock()
		e.rb.tokensFail = code
		e.rb.mu.Unlock()
		if c, b := e.sock(t, "GET", "/api/v1/settings/tokens", ""); c != w.status || v1Code(b) != w.code || strings.Contains(string(b), "SECRET") {
			t.Errorf("read %s: %d %s, want %d %s", code, c, b, w.status, w.code)
		}
	}
}

func TestWriteTimeoutIsOutcomeUnknown(t *testing.T) {
	e := newTokEnv(t)
	e.d.handler.broker.Timeout = 300 * time.Millisecond
	// The broker accepts the request and never answers in time.
	e.rb.setTokenReply(func(string, json.RawMessage) string { time.Sleep(900 * time.Millisecond); return "DROP" })
	c, b := e.apply(t, e.setFlow(t))
	if c != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("client timeout: %d %s", c, b)
	}
	if n := len(e.rb.tokenWrites()); n != 1 {
		t.Fatalf("%d broker writes: a write timeout must never be retried", n)
	}
	time.Sleep(700 * time.Millisecond)
	if n := len(e.rb.tokenWrites()); n != 1 {
		t.Fatalf("a retry arrived later: %d writes", n)
	}
	// The broker's own `timeout` reply.
	e.rb.setTokenReply(func(string, json.RawMessage) string { return brokerErrReply("timeout") })
	c, b = e.apply(t, e.flow(t, "delete_token", "gitlab-main", "", ""))
	if c != 504 || v1Code(b) != "outcome_unknown" || strings.Contains(string(b), "SECRET") {
		t.Fatalf("broker timeout: %d %s", c, b)
	}
	if n := len(e.rb.tokenWrites()); n != 2 {
		t.Fatalf("%d broker writes", n)
	}
	// It is in the ledger as a failure with that code.
	if raw := e.ledgerRaw(t); !strings.Contains(raw, "outcome_unknown") {
		t.Errorf("ledger:\n%s", raw)
	}
}

func TestRegistryUnavailableOnWriteIsOutcomeUnknown(t *testing.T) {
	e := newTokEnv(t)
	for _, code := range []string{"registry_unavailable", "tokens_unavailable"} {
		code := code
		e.rb.setTokenReply(func(string, json.RawMessage) string { return brokerErrReply(code) })
		for _, fl := range []tokFlow{e.setFlow(t), e.flow(t, "delete_token", "gitlab-main", "", "")} {
			c, b := e.apply(t, fl)
			if c != 504 || v1Code(b) != "outcome_unknown" {
				t.Errorf("%s %s: %d %s", code, fl.Action, c, b)
			}
		}
	}
	// ... but on the READ it is a plain unavailable store.
	e.rb.mu.Lock()
	e.rb.tokensFail = "tokens_unavailable"
	e.rb.mu.Unlock()
	if c, b := e.sock(t, "GET", "/api/v1/settings/tokens", ""); c != 503 || v1Code(b) != "tokens_unavailable" {
		t.Errorf("read: %d %s", c, b)
	}
}

func TestTokenRateLimits(t *testing.T) {
	e := newTokEnv(t)
	prep := func(call func(string, string) (int, []byte), n int) (int, []byte) {
		var code int
		var b []byte
		for i := 0; i < n; i++ {
			ep := "begin"
			body := jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}})
			if i%2 == 1 {
				ep = "message"
				body = jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{},
					"request": registrymsg.NewRequestID(), "issued": e.clk.now().UTC().Format(registryTimeFormat)})
			}
			code, b = call("/api/v1/tokens/"+ep, body)
			if code != 200 {
				return code, b
			}
		}
		return 200, nil
	}
	sockCall := func(p, body string) (int, []byte) { return e.sock(t, "POST", p, body) }
	// A refused body takes no slot.
	if c, _ := e.call(t, "begin", `{"action":"nope"}`); c != 400 {
		t.Fatal(c)
	}
	if c, b := prep(sockCall, TokenPrepPerActorHour); c != 200 {
		t.Fatalf("within the limit: %d %s", c, b)
	}
	for _, ep := range []string{"begin", "message"} {
		body := jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}})
		if ep == "message" {
			body = jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{},
				"request": registrymsg.NewRequestID(), "issued": e.clk.now().UTC().Format(registryTimeFormat)})
		}
		if c, b := e.call(t, ep, body); c != 429 || v1Code(b) != "token_rate_limited" {
			t.Fatalf("%s over the combined limit: %d %s", ep, c, b)
		}
	}
	if TokenPrepPerActorHour != 60 || TokenApplyPerActorHour != 20 {
		t.Fatalf("limits %d/%d, D7 says 60/20", TokenPrepPerActorHour, TokenApplyPerActorHour)
	}
	// Another actor has its own bucket.
	other := e.tokenCaller(t, "mac-ssh")
	if c, b := other("/api/v1/tokens/begin", jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}})); c != 200 {
		t.Fatalf("second actor: %d %s", c, b)
	}
	// Apply is a separate bucket: exhausted begin/message do not stop it.
	e.d.handler.tokenPrepLimit = newActorLimiter(1000, time.Hour)
	flows := make([]tokFlow, TokenApplyPerActorHour+2)
	for i := range flows {
		flows[i] = e.flow(t, "delete_token", "gitlab-main", "", "")
	}
	e.d.handler.tokenPrepLimit = newActorLimiter(0, time.Hour)
	for i := 0; i < TokenApplyPerActorHour; i++ {
		if c, b := e.apply(t, flows[i]); c != 200 {
			t.Fatalf("apply %d: %d %s", i+1, c, b)
		}
	}
	before := len(e.rb.tokenWrites())
	if c, b := e.apply(t, flows[TokenApplyPerActorHour]); c != 429 || v1Code(b) != "token_rate_limited" || len(e.rb.tokenWrites()) != before {
		t.Fatalf("apply over the limit: %d %s", c, b)
	}
	if c, b := other("/api/v1/tokens/apply", jsonBody(flows[TokenApplyPerActorHour+1].body(webauthnSig))); c != 200 {
		t.Fatalf("second actor apply: %d %s", c, b)
	}
	// Apply exhausted does not stop begin (separate bucket).
	e.d.handler.tokenPrepLimit = newActorLimiter(1000, time.Hour)
	if c, b := e.call(t, "begin", jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}})); c != 200 {
		t.Fatalf("begin with exhausted apply bucket: %d %s", c, b)
	}
	// An hour later both are free again (injected clock).
	e.clk.set(time.Hour + time.Minute)
	g := e.flow(t, "delete_token", "gitlab-main", "", "")
	if c, b := e.apply(t, g); c != 200 {
		t.Fatalf("after an hour: %d %s", c, b)
	}
}

func TestTokenApplyRefundRules(t *testing.T) {
	// Dead socket: nothing left orchd, so no number of tries is limited.
	dead := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.BrokerSocket = filepath.Join(shortDir(t), "none.sock")
		c.GateHome = t.TempDir()
	})
	dead.d.pauseWorkers()
	req := registrymsg.NewRequestID()
	chg, _ := registrymsg.ChangeHash("delete_token", "gitlab-main", map[string]any{})
	body := jsonBody(map[string]any{"action": "delete_token", "subject": "gitlab-main", "fields": map[string]any{}, "change": chg,
		"request": req, "issued": time.Now().UTC().Format(registryTimeFormat), "signature": webauthnSig})
	for i := 0; i < TokenApplyPerActorHour+5; i++ {
		c, _, b := dead.sockDo(t, "POST", "/api/v1/tokens/apply", body, nil)
		if c != 503 || v1Code(b) != "broker_unavailable" {
			t.Fatalf("try %d: %d %s", i+1, c, b)
		}
	}
	// A refusal the broker made after it looked at the signature keeps the slot.
	for _, code := range []string{"bad_signature", "stale_signature", "unknown_signer", "request_reused", "change_mismatch", "value_mismatch", "token_unknown", "timeout", "tokens_unavailable"} {
		e := newTokEnv(t)
		e.d.handler.tokenPrepLimit = newActorLimiter(1000, time.Hour)
		code := code
		e.rb.setTokenReply(func(string, json.RawMessage) string { return brokerErrReply(code) })
		for i := 0; i < TokenApplyPerActorHour; i++ {
			if c, b := e.apply(t, e.flow(t, "delete_token", "gitlab-main", "", "")); c == 429 {
				t.Fatalf("%s: limited early at %d: %s", code, i+1, b)
			}
		}
		if c, b := e.apply(t, e.flow(t, "delete_token", "gitlab-main", "", "")); c != 429 || v1Code(b) != "token_rate_limited" {
			t.Errorf("%s kept no slot: %d %s", code, c, b)
		}
	}
	// busy and forbidden (the broker did nothing) give it back.
	for _, code := range []string{"busy", "forbidden"} {
		e := newTokEnv(t)
		e.d.handler.tokenPrepLimit = newActorLimiter(1000, time.Hour)
		code := code
		e.rb.setTokenReply(func(string, json.RawMessage) string { return brokerErrReply(code) })
		for i := 0; i < TokenApplyPerActorHour+3; i++ {
			if c, b := e.apply(t, e.flow(t, "delete_token", "gitlab-main", "", "")); c != 503 {
				t.Fatalf("%s try %d: %d %s", code, i+1, c, b)
			}
		}
	}
}

func TestLedgerTokenChangeHasNoValueHashOrLast4(t *testing.T) {
	e := newTokEnv(t)
	f := e.setFlow(t)
	if c, b := e.apply(t, f); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	e.rb.setTokenReply(func(string, json.RawMessage) string { return brokerErrReply("token_exists_other_kind") })
	g := e.flow(t, "set_token", "gitlab-main", "github", canaryValue)
	if c, _ := e.apply(t, g); c != 409 {
		t.Fatal(c)
	}
	e.rb.setTokenReply(nil)
	d := e.flow(t, "delete_token", "gitlab-main", "", "")
	if c, _ := e.apply(t, d); c != 200 {
		t.Fatal(c)
	}
	raw := e.ledgerRaw(t)
	for _, leak := range []string{canaryValue, canaryLast4, f.Fields["value_hash"].(string), g.Fields["value_hash"].(string), f.Request, f.Change, "c2ln", "alice",
		"SECRET-BROKER-TEXT", f.Message, tokExpires} {
		if strings.Contains(raw, leak) {
			t.Fatalf("%q in the ledger:\n%s", leak, raw)
		}
	}
	evs, err := ledger.Events(policy.OrchLedgerPath(e.home))
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	for _, ev := range evs {
		if ev.Type == "project_change" {
			t.Errorf("a token change was recorded as a project change")
		}
		if ev.Type == "token_change" {
			got = append(got, ev.Payload)
		}
	}
	if len(got) != 3 {
		t.Fatalf("%d token_change events:\n%s", len(got), raw)
	}
	for _, pl := range got {
		keys := []string{}
		for k := range pl {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "action,actor,code,kind,ok,subject" {
			t.Fatalf("payload keys %v", keys)
		}
		if pl["actor"] != socketActor() || pl["subject"] != "gitlab-main" {
			t.Fatalf("%v", pl)
		}
	}
	if got[0]["ok"] != true || got[0]["kind"] != "gitlab" || got[0]["action"] != "set_token" ||
		got[1]["ok"] != false || got[1]["code"] != "token_exists_other_kind" || got[1]["kind"] != "github" ||
		got[2]["ok"] != true || got[2]["action"] != "delete_token" || got[2]["kind"] != "" {
		t.Fatalf("%v", got)
	}
	if _, err := ledger.Verify(policy.OrchLedgerPath(e.home)); err != nil {
		t.Fatal(err)
	}
}

func TestTokensWorkWithBrokerDown(t *testing.T) {
	for name, opt := range map[string]func(c *Config){
		"no socket configured": func(c *Config) {},
		"nothing listening":    func(c *Config) { c.BrokerSocket = filepath.Join(shortDir(t), "none.sock") },
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			e := newEnv(t, noSteps{}, 1, func(c *Config) {
				c.GateHome = t.TempDir()
				c.Policy = func() PolicyView { return PolicyView{Classes: map[string]string{"docs": "L1"}} }
				opt(c)
			})
			e.d.pauseWorkers()
			do := func(m, p, body string) (int, []byte) { c, _, b := e.sockDo(t, m, p, body, nil); return c, b }
			if code, b := do("GET", "/api/v1/settings/tokens", ""); code != 503 || v1Code(b) != "broker_unavailable" {
				t.Errorf("list: %d %s", code, b)
			}
			// begin and message need no broker.
			code, b := do("POST", "/api/v1/tokens/begin", jsonBody(map[string]any{"action": "set_token", "subject": "gitlab-main", "fields": map[string]any{"kind": "gitlab", "expires": ""}}))
			if code != 200 {
				t.Fatalf("begin %d %s", code, b)
			}
			var bo struct{ Request, Issued string }
			_ = json.Unmarshal(b, &bo)
			fields := map[string]any{"kind": "gitlab", "expires": "", "value_hash": registrymsg.ValueHash(bo.Request, canaryValue)}
			code, b = do("POST", "/api/v1/tokens/message", jsonBody(map[string]any{"action": "set_token", "subject": "gitlab-main", "fields": fields, "request": bo.Request, "issued": bo.Issued}))
			if code != 200 {
				t.Fatalf("message %d %s", code, b)
			}
			var mo struct{ Change string }
			_ = json.Unmarshal(b, &mo)
			ap := jsonBody(map[string]any{"action": "set_token", "subject": "gitlab-main", "fields": fields, "change": mo.Change,
				"request": bo.Request, "issued": bo.Issued, "signature": webauthnSig, "value": canaryValue})
			if code, b := do("POST", "/api/v1/tokens/apply", ap); code != 503 || v1Code(b) != "broker_unavailable" || strings.Contains(string(b), canaryValue) {
				t.Errorf("apply: %d %s", code, b)
			}
			if strings.Contains(logs.String(), canaryValue) {
				t.Error("value in the log")
			}
			// Everything that worked before still works.
			if code, b := do("GET", "/api/v1/policy", ""); code != 200 {
				t.Errorf("policy %d %s", code, b)
			}
			if code, b := do("GET", "/api/v1/projects", ""); code != 200 {
				t.Errorf("projects %d %s", code, b)
			}
			if code, b := do("GET", "/api/v1/projects/registry", ""); code != 503 || v1Code(b) != "broker_unavailable" {
				t.Errorf("registry %d %s", code, b)
			}
			if code, b := e.submit(t, e.repo, "HEAD", "p", "docs", "claude"); code != 201 {
				t.Errorf("submit %d %s", code, b)
			}
		})
	}
}

// ---- deploy key ----

func sshPubLine(t *testing.T, comment string) (line string, fp string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if comment != "" {
		line += " " + comment
	}
	return line, ssh.FingerprintSHA256(pk)
}

func newDeployKeyEnv(t *testing.T, keyPath string) *env {
	t.Helper()
	e := newEnv(t, noSteps{}, 1, func(c *Config) { c.GitSSHKey = keyPath; c.GateHome = t.TempDir() })
	e.d.pauseWorkers()
	return e
}

func TestDeployKeyEndpoint(t *testing.T) {
	get := func(e *env) (int, []byte) {
		c, _, b := e.sockDo(t, "GET", "/api/v1/settings/deploy-key", "", nil)
		return c, b
	}
	line, fp := sshPubLine(t, "orch@deploy")
	rsaLine := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7 not-checked-as-rsa"
	_ = rsaLine

	// (a) a valid .pub and NO private key file at all: 200.
	dir := shortDir(t)
	priv := filepath.Join(dir, "id_gitlab_deploy")
	if err := os.WriteFile(priv+".pub", []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, b := get(newDeployKeyEnv(t, priv))
	var out map[string]string
	_ = json.Unmarshal(b, &out)
	if c != 200 || out["public_key"] != line || out["fingerprint"] != fp || len(out) != 2 {
		t.Fatalf("(a) %d %s", c, b)
	}
	// (b) a private key that is unreadable (mode 0000) and holds a canary: 200, bytes never read.
	privCanary := "-----BEGIN OPENSSH PRIVATE KEY-----\nPRIVATE-CANARY-NOT-A-REAL-KEY\n-----END OPENSSH PRIVATE KEY-----\n"
	if err := os.WriteFile(priv, []byte(privCanary), 0o000); err != nil {
		t.Fatal(err)
	}
	c, b = get(newDeployKeyEnv(t, priv))
	if c != 200 || strings.Contains(string(b), "PRIVATE-CANARY") || strings.Contains(string(b), "OPENSSH PRIVATE") {
		t.Fatalf("(b) %d %s", c, b)
	}
	// (c) no .pub but a readable private key: deploy_key_unavailable, none of its bytes.
	dir2 := shortDir(t)
	priv2 := filepath.Join(dir2, "id_gitlab_deploy")
	// The private path holds a perfectly valid public-key line: if it were read
	// (as a fallback for the missing .pub, say) the endpoint would answer 200.
	if err := os.WriteFile(priv2, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, b = get(newDeployKeyEnv(t, priv2))
	if c != 503 || v1Code(b) != "deploy_key_unavailable" || strings.Contains(string(b), "PRIVATE-CANARY") || strings.Contains(string(b), line) {
		t.Fatalf("(c) %d %s", c, b)
	}
	// A .pub that is a symlink (to the private key) is not followed.
	dir3 := shortDir(t)
	priv3 := filepath.Join(dir3, "k")
	os.WriteFile(priv3, []byte(line+"\n"), 0o600)
	os.Symlink(priv3, priv3+".pub")
	if c, b := get(newDeployKeyEnv(t, priv3)); c != 503 || v1Code(b) != "deploy_key_unavailable" {
		t.Errorf("symlinked .pub: %d %s", c, b)
	}
	// Not configured at all.
	if c, b := get(newDeployKeyEnv(t, "")); c != 503 || v1Code(b) != "deploy_key_unavailable" {
		t.Errorf("not configured: %d %s", c, b)
	}
	// Format checks: every bad content is the same fixed refusal and is never echoed.
	good2, _ := sshPubLine(t, "")
	blob := strings.Fields(line)[1]
	for name, content := range map[string]string{
		"empty":            "",
		"only newline":     "\n",
		"two lines":        line + "\n" + line + "\n",
		"crlf":             line + "\r\n",
		"leading space":    " " + line + "\n",
		"ecdsa":            "ecdsa-sha2-nistp256 " + blob + " x\n",
		"type mismatch":    "ssh-rsa " + blob + " x\n",
		"not base64":       "ssh-ed25519 !!!notbase64!!! x\n",
		"truncated blob":   "ssh-ed25519 " + blob[:20] + "\n",
		"private key":      "-----BEGIN OPENSSH PRIVATE KEY-----\nPRIVATE-CANARY\n-----END OPENSSH PRIVATE KEY-----\n",
		"control in comm":  good2 + " a\x01b\n",
		"bidi in comment":  good2 + " a‮b\n",
		"nul":              good2 + "\x00\n",
		"too long comment": good2 + " " + strings.Repeat("c", 5000) + "\n",
		"no key part":      "ssh-ed25519\n",
		"options prefix":   "command=\"x\" " + good2 + "\n",
	} {
		d := shortDir(t)
		p := filepath.Join(d, "k")
		os.WriteFile(p+".pub", []byte(content), 0o644)
		c, b := get(newDeployKeyEnv(t, p))
		if c != 503 || v1Code(b) != "deploy_key_unavailable" || strings.Contains(string(b), "PRIVATE-CANARY") || strings.Contains(string(b), blob[:20]) {
			t.Errorf("%s: %d %s", name, c, b)
		}
	}
	// A valid key without comment, and one with spaces in the comment, are fine.
	for _, l := range []string{good2, good2 + " a comment with spaces"} {
		d := shortDir(t)
		p := filepath.Join(d, "k")
		os.WriteFile(p+".pub", []byte(l+"\n"), 0o644)
		if c, b := get(newDeployKeyEnv(t, p)); c != 200 || !strings.Contains(string(b), strings.Fields(l)[1]) {
			t.Errorf("%q: %d %s", l, c, b)
		}
	}
	// Without trailing newline is fine as well.
	d := shortDir(t)
	p := filepath.Join(d, "k")
	os.WriteFile(p+".pub", []byte(good2), 0o644)
	if c, b := get(newDeployKeyEnv(t, p)); c != 200 {
		t.Errorf("no newline: %d %s", c, b)
	}
	// Oversize file.
	d = shortDir(t)
	p = filepath.Join(d, "k")
	os.WriteFile(p+".pub", []byte(good2+" "+strings.Repeat("c", 10000)), 0o644)
	if c, b := get(newDeployKeyEnv(t, p)); c != 503 || v1Code(b) != "deploy_key_unavailable" {
		t.Errorf("oversize: %d %s", c, b)
	}
	// Reachable with the token tier too (read endpoint), and needs authentication.
	e := newDeployKeyEnv(t, priv)
	if c, _, _ := httpDo(t, e.httpServer(t), "GET", "/api/v1/settings/deploy-key", "", hdr{}); c != 401 {
		t.Errorf("without credentials: %d", c)
	}
	// A relative --git-ssh-key never serves anything (and startup is not refused).
	if c, b := get(newDeployKeyEnv(t, "relative/key")); c != 503 || v1Code(b) != "deploy_key_unavailable" {
		t.Errorf("relative path: %d %s", c, b)
	}
	_ = strconv.Itoa
}

// ---- refine: ai_key_missing ----

func TestRefineAIKeyMissingIsFixed(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	a := e.token(t, "alice")
	text := "add a health check"
	p := e.preview(t, a, text)
	e.fb.mu.Lock()
	e.fb.refineReply = func(fakeRefineCall) string { return brokerErrReply("ai_key_missing") }
	e.fb.mu.Unlock()
	var sentence string
	for i := 0; i < 3; i++ {
		code, b := e.refine(a, text, p.SelectionID)
		if code != 503 || v1Code(b) != "ai_key_missing" {
			t.Fatalf("try %d: %d %s", i+1, code, b)
		}
		if strings.Contains(string(b), "SECRET-BROKER-TEXT") || strings.Contains(string(b), "sk-live") {
			t.Fatalf("broker text echoed: %s", b)
		}
		var m struct{ Error struct{ Message string } }
		_ = json.Unmarshal(b, &m)
		if m.Error.Message == "" || (sentence != "" && sentence != m.Error.Message) {
			t.Fatalf("sentence %q / %q", sentence, m.Error.Message)
		}
		sentence = m.Error.Message
	}
	if !strings.Contains(sentence, "openai") && !strings.Contains(sentence, "AI") {
		t.Errorf("sentence %q", sentence)
	}
}

// checkTokenConstants ties the token constants of the browser (views/tokens.js)
// to the Go rules. Until the console view exists (B5) it SKIPS, but only while
// no web file mentions tokens.js; once the shell references tokens.js a missing
// file is a failure. B5 must change this to an unconditional t.Fatal.
func checkTokenConstants(t *testing.T) {
	static := "../web/static"
	referenced := false
	_ = filepath.Walk(static, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && (strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".html")) {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "tokens.js") {
				referenced = true
			}
		}
		return nil
	})
	src, err := os.ReadFile(filepath.Join(static, "views", "tokens.js"))
	if err != nil {
		if referenced {
			t.Fatalf("the shell references tokens.js but views/tokens.js is unreadable: %v", err)
		}
		t.Skip("views/tokens.js does not exist yet (B5): token constants not compared; B5 must make this a hard failure")
	}
	js := string(src)
	jsRegex := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = /(.*)/;\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS regex constant %s not found", name)
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
	jsString := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = '([^']*)';\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS string constant %s not found", name)
		}
		return m[1]
	}
	// Name (ref): the Go rule is registrymsg's; the browser carries the same text.
	if got, want := jsRegex("TOKEN_REF_RE"), subjectPattern; got != want {
		t.Errorf("TOKEN_REF_RE %q != %q", got, want)
	}
	for s, want := range map[string]bool{"a": true, "openai": true, "gitlab-main": true, strings.Repeat("a", 40): true,
		"": false, "A": false, "1a": false, "a_b": false, strings.Repeat("a", 41): false, "a b": false, "한글": false} {
		if registrymsg.ValidTokenRef(s) != want {
			t.Errorf("ValidTokenRef(%q) != %v", s, want)
		}
	}
	if got := jsInt("TOKEN_REF_MAX"); got != 40 || !registrymsg.ValidTokenRef("a"+strings.Repeat("b", got-1)) || registrymsg.ValidTokenRef("a"+strings.Repeat("b", got)) {
		t.Errorf("TOKEN_REF_MAX %d does not match registrymsg", got)
	}
	// Kinds: exactly the closed set, in the same order the browser shows them.
	km := regexp.MustCompile(`(?m)^(?:export )?const TOKEN_KINDS = \[([^\]]*)\];\s*$`).FindStringSubmatch(js)
	if km == nil {
		t.Fatal("JS TOKEN_KINDS not found")
	}
	var kinds []string
	for _, part := range strings.Split(km[1], ",") {
		kinds = append(kinds, strings.Trim(strings.TrimSpace(part), "'"))
	}
	if strings.Join(kinds, ",") != "openai,gitlab,github" {
		t.Errorf("TOKEN_KINDS %v", kinds)
	}
	for _, k := range kinds {
		if !registrymsg.ValidTokenKind(k) {
			t.Errorf("kind %q is not valid in Go", k)
		}
	}
	for _, k := range []string{"", "OpenAI", "slack", "openai ", "gitlab\n"} {
		if registrymsg.ValidTokenKind(k) {
			t.Errorf("kind %q must not be valid", k)
		}
	}
	if got := jsString("TOKEN_SINGLETON_KIND"); got != "openai" {
		t.Errorf("TOKEN_SINGLETON_KIND %q", got)
	}
	// Value: length bounds (the broker's constants are read from its source),
	// character set, and the apply body cap.
	bsrc, berr := os.ReadFile("../../broker/tokens.go")
	if berr != nil {
		t.Fatal(berr)
	}
	for name, got := range map[string]int{"minTokenValue": jsInt("TOKEN_VALUE_MIN"), "maxTokenValue": jsInt("TOKEN_VALUE_MAX")} {
		m := regexp.MustCompile(name + `\s*=\s*([0-9]+)`).FindSubmatch(bsrc)
		if m == nil {
			t.Fatalf("%s not found in internal/broker/tokens.go", name)
		}
		if want, _ := strconv.Atoi(string(m[1])); got != want {
			t.Errorf("JS %s = %d, broker %d", name, got, want)
		}
	}
	if jsInt("TOKEN_VALUE_MIN") != minTokenValue || jsInt("TOKEN_VALUE_MAX") != maxTokenValue {
		t.Errorf("JS value bounds differ from orchd's %d/%d", minTokenValue, maxTokenValue)
	}
	if got, want := jsRegex("TOKEN_VALUE_RE"), `^[\x21-\x7e]+$`; got != want {
		t.Errorf("TOKEN_VALUE_RE %q != %q", got, want)
	}
	if got := jsInt("TOKEN_APPLY_MAX_BYTES"); got != maxTokenApplyBody {
		t.Errorf("TOKEN_APPLY_MAX_BYTES %d != %d", got, maxTokenApplyBody)
	}
	// orchd's own check of the value is the same rule, byte by byte and at the boundaries.
	probe := func(raw string) *httpError {
		dst := make([]byte, 0, maxTokenValue+1)
		_, e := unquoteSecret([]byte(`"`+raw+`"`), dst)
		return e
	}
	base := strings.Repeat("a", minTokenValue)
	for c := 0; c < 256; c++ {
		esc := string([]byte{byte(c)})
		switch c {
		case '"':
			esc = `\"`
		case '\\':
			esc = `\\`
		}
		e := probe(base + esc)
		allowed := c >= 0x21 && c <= 0x7e
		if allowed && e != nil || !allowed && e != errTokBadFormat {
			t.Errorf("byte 0x%02x: %v (allowed=%v)", c, e, allowed)
		}
	}
	for n, want := range map[int]*httpError{minTokenValue - 1: errTokTooShort, minTokenValue: nil, maxTokenValue: nil, maxTokenValue + 1: errTokBadFormat} {
		if e := probe(strings.Repeat("a", n)); e != want {
			t.Errorf("length %d: %v, want %v", n, e, want)
		}
	}
	// Expiry: the format and the year range.
	if got, want := jsRegex("EXPIRES_RE"), `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`; got != want {
		t.Errorf("EXPIRES_RE %q != %q", got, want)
	}
	ymin, ymax := jsInt("EXPIRES_YEAR_MIN"), jsInt("EXPIRES_YEAR_MAX")
	if ymin != 2000 || ymax != 9999 {
		t.Errorf("year range %d..%d, want 2000..9999", ymin, ymax)
	}
	for s, want := range map[string]bool{"": true, "2000-01-01": true, "1999-12-31": false, "9999-12-31": true, "10000-01-01": false,
		"2026-02-29": false, "2028-02-29": true, "2026-13-01": false, "2026-1-1": false, " 2026-01-01": false, "2026-01-01 ": false} {
		if registrymsg.ValidExpires(s) != want {
			t.Errorf("ValidExpires(%q) != %v", s, want)
		}
	}
	// Actions and the endpoints the browser calls.
	if got := jsString("ACTION_SET_TOKEN"); got != registrymsg.ActionSetToken {
		t.Errorf("ACTION_SET_TOKEN %q", got)
	}
	if got := jsString("ACTION_DELETE_TOKEN"); got != registrymsg.ActionDeleteToken {
		t.Errorf("ACTION_DELETE_TOKEN %q", got)
	}
	// The salted value hash: the browser hashes request + ':' + value.
	if !strings.Contains(js, "':'") {
		t.Error("tokens.js does not build the salted value hash request + ':' + value")
	}
	// The error codes the view shows fixed texts for must be codes the API returns.
	goSrc := ""
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			b, _ := os.ReadFile(f)
			goSrc += string(b)
		}
	}
	for _, c := range []string{"token_unknown", "bad_kind", "token_too_short", "token_bad_format", "value_mismatch",
		"token_exists_other_kind", "tokens_unavailable", "openai_singleton", "token_rate_limited", "deploy_key_unavailable", "ai_key_missing"} {
		if !strings.Contains(goSrc, `"`+c+`"`) {
			t.Errorf("no api source returns %q", c)
		}
	}
}
