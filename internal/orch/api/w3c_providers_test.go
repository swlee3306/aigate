package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

func TestProvidersListAndGroupsViaBroker(t *testing.T) {
	e := newW3cEnv(t)
	code, b := e.sockJSON(t, "GET", "/api/v1/providers", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var got struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(b, &got); err != nil || len(got.Providers) != 1 {
		t.Fatalf("%v %s", err, b)
	}
	p := got.Providers[0]
	if ledgerKeys(p) != "base_url,id,in_use,kind,ssh_url,token_ref" || p["id"] != "gl" || p["token_ref"] != "gl-admin" || p["in_use"] != false {
		t.Fatalf("provider row %v", p)
	}
	code, b = e.sockJSON(t, "GET", "/api/v1/providers/gl/groups", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var gr struct {
		Groups    []map[string]string `json:"groups"`
		Truncated bool                `json:"truncated"`
	}
	if err := json.Unmarshal(b, &gr); err != nil || len(gr.Groups) != 2 || gr.Groups[1]["path"] != "team/sub" || gr.Groups[1]["name"] != "sub" || gr.Truncated {
		t.Fatalf("%v %s", err, b)
	}
	lg := e.wb.opsNamed("list_groups")
	if len(lg) != 1 || string(lg[0].Args) != `{"provider":"gl"}` {
		t.Fatalf("list_groups args %+v", lg)
	}
	// Reads send no signed write and never a token.
	if len(e.wb.writeCalls()) != 0 {
		t.Fatal("a write reached the broker")
	}
	for _, bad := range []string{"GL", "..", "a%2Fb", "x y", strings.Repeat("a", 41), "1a"} {
		before := len(e.wb.allOps())
		code, b := e.sockJSON(t, "GET", "/api/v1/providers/"+bad+"/groups", "")
		if code != 400 && code != 404 {
			t.Errorf("%q: %d %s", bad, code, b)
		}
		if len(e.wb.allOps()) != before {
			t.Errorf("%q reached the broker", bad)
		}
	}
	// Broker refusals are fixed sentences without the broker's text.
	for brokerCode, want := range map[string]struct {
		status int
		code   string
	}{
		"provider_unknown":     {404, "provider_unknown"},
		"provider_unreachable": {502, "provider_unreachable"},
		"gitlab_refused":       {502, "gitlab_refused"},
		"token_ref_unknown":    {400, "token_ref_unknown"},
		"bad_url":              {400, "bad_url"},
		"registry_unavailable": {503, "registry_unavailable"},
		"tokens_unavailable":   {503, "tokens_unavailable"},
		"busy":                 {503, "broker_unavailable"},
		"timeout":              {502, "broker_error"},
		"internal_error":       {502, "broker_error"},
	} {
		bc := brokerCode
		e.wb.setReply(func(op string, _ json.RawMessage) string {
			if op == "list_groups" {
				return brokerErrReply(bc)
			}
			return ""
		})
		code, b := e.sockJSON(t, "GET", "/api/v1/providers/gl/groups", "")
		if code != want.status || v1Code(b) != want.code || strings.Contains(string(b), "SECRET-BROKER-TEXT") || strings.Contains(string(b), "evil.example") {
			t.Errorf("%s: %d %s", bc, code, b)
		}
	}
	// A broken reply is broker_error, never a partial list.
	for name, reply := range map[string]string{
		"hostile path": `{"ok":true,"result":{"groups":[{"path":"team/../x","name":"x"}],"truncated":false}}`,
		"hostile name": `{"ok":true,"result":{"groups":[{"path":"team","name":"a‮b"}],"truncated":false}}`,
		"long name":    `{"ok":true,"result":{"groups":[{"path":"team","name":"` + strings.Repeat("x", 300) + `"}],"truncated":false}}`,
		"not a list":   `{"ok":true,"result":{"groups":"x","truncated":false}}`,
	} {
		r := reply
		e.wb.setReply(func(op string, _ json.RawMessage) string {
			if op == "list_groups" {
				return r
			}
			return ""
		})
		if code, b := e.sockJSON(t, "GET", "/api/v1/providers/gl/groups", ""); code != 502 || v1Code(b) != "broker_error" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	e.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "providers" {
			return `{"ok":true,"result":{"providers":[{"id":"gl","kind":"gitlab","base_url":"javascript:x","ssh_url":"ssh://git@h","token_ref":"t","in_use":false}]}}`
		}
		return ""
	})
	if code, b := e.sockJSON(t, "GET", "/api/v1/providers", ""); code != 502 || v1Code(b) != "broker_error" {
		t.Errorf("bad provider row: %d %s", code, b)
	}
}

func TestListGroupsLimitAndRefund(t *testing.T) {
	e := newW3cEnv(t)
	clk := &testClock{}
	e.d.handler.now = clk.now
	for i := 0; i < ListGroupsPerActorHour; i++ {
		if code, b := e.sockJSON(t, "GET", "/api/v1/providers/gl/groups", ""); code != 200 {
			t.Fatalf("%d: %d %s", i+1, code, b)
		}
	}
	code, b := e.sockJSON(t, "GET", "/api/v1/providers/gl/groups", "")
	if code != 429 || v1Code(b) != "project_rate_limited" {
		t.Fatalf("%d %s", code, b)
	}
	other := e.tokenCallerW3c(t, "mac-ssh")
	if code, b := other("GET", "/api/v1/providers/gl/groups", ""); code != 200 {
		t.Fatalf("second actor: %d %s", code, b)
	}
	clk.set(time.Hour + time.Minute)
	if code, b := e.sockJSON(t, "GET", "/api/v1/providers/gl/groups", ""); code != 200 {
		t.Fatalf("after an hour: %d %s", code, b)
	}
	// A dead broker costs nothing: unlimited tries.
	dead := newEnv(t, noSteps{}, 1, func(c *Config) { c.BrokerSocket = filepath.Join(shortDir(t), "none.sock") })
	for i := 0; i < ListGroupsPerActorHour+5; i++ {
		if c, _, b := dead.sockDo(t, "GET", "/api/v1/providers/gl/groups", "", nil); c != 503 || v1Code(b) != "broker_unavailable" {
			t.Fatalf("dead try %d: %d %s", i+1, c, b)
		}
	}
}

func (e *w3cEnv) tokenCallerW3c(t *testing.T, name string) func(method, path, body string) (int, []byte) {
	srv := e.httpServer(t)
	tok := e.newToken(t, name)
	return func(method, path, body string) (int, []byte) {
		c, _, b := httpDo(t, srv, method, path, body, hdr{"Authorization": "Bearer " + tok})
		return c, b
	}
}

func TestProposeBuildsMessageStoresNothingSendsNoWrite(t *testing.T) {
	e := newW3cEnv(t)
	seen := map[string]bool{}
	for _, action := range w3cActions {
		s, f := e.fieldsFor(action)
		p := e.proposeAction(t, action)
		if p.Action != action || p.Subject != s {
			t.Fatalf("%s: %+v", action, p)
		}
		want := map[string]any{}
		for k, v := range f {
			want[k] = v
		}
		if action == "create_project" {
			want["deploy_key_sha256"] = sha256Hex(e.pubLine)
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(p.Fields)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("%s fields:\n got %s\nwant %s", action, gotJSON, wantJSON)
		}
		ch, err := registrymsg.ChangeHash(action, s, want)
		if err != nil || p.Change != ch {
			t.Fatalf("%s: change %s want %s (%v)", action, p.Change, ch, err)
		}
		issued, err := time.Parse(registryTimeFormat, p.Issued)
		if err != nil || time.Since(issued) > time.Minute {
			t.Fatalf("%s: issued %q", action, p.Issued)
		}
		msg := signing.RegistryMessage{Action: action, Subject: s, Change: p.Change, Request: p.Request, Issued: issued}
		if msg.Validate() != nil || p.Message != msg.Text() {
			t.Fatalf("%s: message\n%s", action, p.Message)
		}
		if len(p.Preview) < 3 || !strings.Contains(strings.Join(p.Preview, "\n"), s) {
			t.Fatalf("%s: preview %v", action, p.Preview)
		}
		if seen[p.Request] {
			t.Fatalf("%s: request id repeated", action)
		}
		seen[p.Request] = true
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("propose called the broker: %v", ops)
	}
	if ev := e.ledgerPayloads(t, "provider_change"); len(ev) != 0 {
		t.Fatalf("propose wrote ledger events: %v", ev)
	}
	if ev := e.ledgerPayloads(t, "project_create"); len(ev) != 0 {
		t.Fatalf("propose wrote ledger events: %v", ev)
	}
}

func TestW3cProposeRejectsBadFieldsAndHostileText(t *testing.T) {
	e := newW3cEnv(t)
	post := func(path string, body any) (int, []byte) {
		return e.sockJSON(t, "POST", path, jsonBody(body))
	}
	_, f := e.fieldsFor("create_project")
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range f {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for name, fields := range map[string]map[string]any{
		"bad provider":      with("provider", "G L"),
		"bad group path":    with("group_path", "team/../x"),
		"group depth":       with("group_path", "aa/bb/cc/dd/ee"),
		"template":          with("template", "rust"),
		"publish":           with("publish", "admin"),
		"description ctl":   with("description", "a\nb"),
		"description bidi":  with("description", "a‮b"),
		"description long":  with("description", strings.Repeat("가", 201)),
		"ai string":         with("ai_context", "false"),
		"missing template":  with("template", nil),
		"extra field":       with("x", "y"),
		"client sets hash":  with("deploy_key_sha256", strings.Repeat("a", 64)),
		"empty provider":    with("provider", ""),
		"hostile group":     with("group_path", "team​"),
		"description 0x7f":  with("description", "a\x7fb"),
		"description 2028":  with("description", "a b"),
		"description nonch": with("description", "a\ufdd0b"),
		"description ffff":  with("description", "a\U0010ffffb"),
		"nonascii provider": with("provider", "깃랩"),
	} {
		code, b := post("/api/v1/projects/create/propose", map[string]any{"action": "create_project", "subject": "app-one", "fields": fields})
		if code != 400 {
			t.Errorf("create %s: %d %s", name, code, b)
		}
	}
	for _, sub := range []string{"A", "a", "-a", "a_b", "app one", "../x", strings.Repeat("a", 41), ""} {
		if code, b := post("/api/v1/projects/create/propose", map[string]any{"action": "create_project", "subject": sub, "fields": f}); code != 400 {
			t.Errorf("subject %q: %d %s", sub, code, b)
		}
	}
	// wrong action on the wrong endpoint, unknown top keys, missing keys
	for name, tc := range map[string]struct {
		path string
		body map[string]any
	}{
		"provider endpoint, create_project": {"/api/v1/providers/propose", map[string]any{"action": "create_project", "subject": "app-one", "fields": f}},
		"create endpoint, set_provider":     {"/api/v1/projects/create/propose", map[string]any{"action": "set_provider", "subject": "gl", "fields": map[string]any{}}},
		"register_project on providers":     {"/api/v1/providers/propose", map[string]any{"action": "register_project", "subject": "x", "fields": map[string]any{}}},
		"unknown top key":                   {"/api/v1/providers/propose", map[string]any{"action": "delete_provider", "subject": "gl", "fields": map[string]any{}, "extra": 1}},
		"no fields":                         {"/api/v1/providers/propose", map[string]any{"action": "delete_provider", "subject": "gl"}},
		"delete with fields":                {"/api/v1/providers/propose", map[string]any{"action": "delete_provider", "subject": "gl", "fields": map[string]any{"x": "y"}}},
		"set_provider bad url":              {"/api/v1/providers/propose", map[string]any{"action": "set_provider", "subject": "gl", "fields": map[string]any{"kind": "gitlab", "base_url": "http://u:p@h", "ssh_url": "ssh://git@h", "token_ref": "t"}}},
		"set_provider bad kind":             {"/api/v1/providers/propose", map[string]any{"action": "set_provider", "subject": "gl", "fields": map[string]any{"kind": "github", "base_url": "http://h", "ssh_url": "ssh://git@h", "token_ref": "t"}}},
		"create_group bad parent":           {"/api/v1/providers/propose", map[string]any{"action": "create_group", "subject": "gl", "fields": map[string]any{"name": "ab", "path": "ab", "parent_path": "../x"}}},
	} {
		if code, b := post(tc.path, tc.body); code != 400 {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("broker called: %v", ops)
	}
	if code, _ := e.sockJSON(t, "POST", "/api/v1/providers/propose", `{"action":"set_provider","subject":"gl","fields":{"kind":"gitlab","kind":"gitlab","base_url":"http://h","ssh_url":"ssh://git@h","token_ref":"t"}}`); code != 400 {
		t.Errorf("duplicate field key: %d", code)
	}
}

func TestCreateProposeIncludesDeployKeyHash(t *testing.T) {
	e := newW3cEnv(t)
	// The private key is never opened: it does not exist at all here, and a
	// mode-0 file in its place changes nothing.
	if _, err := os.Stat(e.key); err == nil {
		t.Fatal("test setup: the private key file exists")
	}
	p := e.proposeAction(t, "create_project")
	if p.Fields["deploy_key_sha256"] != sha256Hex(e.pubLine) {
		t.Fatalf("hash %v want %s", p.Fields["deploy_key_sha256"], sha256Hex(e.pubLine))
	}
	if p.DeployKey == nil || p.DeployKey.PublicKey != e.pubLine || p.DeployKey.Fingerprint != e.pubFP {
		t.Fatalf("deploy_key view %+v", p.DeployKey)
	}
	if err := os.WriteFile(e.key, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nCANARY-PRIVATE\n"), 0); err != nil {
		t.Fatal(err)
	}
	code, b := e.sockJSON(t, "POST", e.proposePath("create_project"), jsonBody(map[string]any{"action": "create_project", "subject": "app-one", "fields": func() map[string]any { _, f := e.fieldsFor("create_project"); return f }()}))
	if code != 200 || strings.Contains(string(b), "CANARY-PRIVATE") {
		t.Fatalf("%d %s", code, b)
	}
	// A rotated .pub gives a different hash.
	line2, _ := sshPubLine(t, "second")
	if err := os.WriteFile(e.key+".pub", []byte(line2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := e.proposeAction(t, "create_project")
	if p2.Fields["deploy_key_sha256"] != sha256Hex(line2) || p2.Fields["deploy_key_sha256"] == p.Fields["deploy_key_sha256"] {
		t.Fatal("hash did not follow the .pub file")
	}
	// A key the broker would refuse (a comment with a space, too long, no
	// public key at all) is refused here, before anyone signs it.
	for name, content := range map[string]string{
		"comment with space": strings.Fields(line2)[0] + " " + strings.Fields(line2)[1] + " two words\n",
		"comment too long":   strings.Fields(line2)[0] + " " + strings.Fields(line2)[1] + " " + strings.Repeat("c", 150) + "\n",
		"empty":              "",
		"private key text":   "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n",
	} {
		os.WriteFile(e.key+".pub", []byte(content), 0o644)
		_, f := e.fieldsFor("create_project")
		code, b := e.sockJSON(t, "POST", e.proposePath("create_project"), jsonBody(map[string]any{"action": "create_project", "subject": "app-one", "fields": f}))
		if code != 503 || v1Code(b) != "deploy_key_unavailable" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// No key configured at all.
	e2 := newW3cEnv(t, func(c *Config) { c.GitSSHKey = "" })
	_, f := e2.fieldsFor("create_project")
	if code, b := e2.sockJSON(t, "POST", e2.proposePath("create_project"), jsonBody(map[string]any{"action": "create_project", "subject": "app-one", "fields": f})); code != 503 || v1Code(b) != "deploy_key_unavailable" {
		t.Errorf("no key: %d %s", code, b)
	}
}

func TestCreateProposePolicyClassMissing(t *testing.T) {
	for name, pol := range map[string]func() PolicyView{
		"class absent": func() PolicyView {
			return PolicyView{Hash: "h", Classes: map[string]string{"docs": "L1", "code": "L2"}}
		},
		"no classes": func() PolicyView { return PolicyView{Hash: "h"} },
	} {
		pol := pol
		e := newW3cEnv(t, func(c *Config) { c.Policy = pol })
		_, f := e.fieldsFor("create_project")
		before := time.Now()
		code, b := e.sockJSON(t, "POST", e.proposePath("create_project"), jsonBody(map[string]any{"action": "create_project", "subject": "app-one", "fields": f}))
		if code != 400 || v1Code(b) != "policy_class_missing" || strings.Contains(string(b), "message\":\"aigate-registry") {
			t.Errorf("%s: %d %s", name, code, b)
		}
		if strings.Contains(string(b), `"request"`) || strings.Contains(string(b), `"change"`) {
			t.Errorf("%s: a signing message was handed out: %s", name, b)
		}
		_ = before
		if len(e.wb.allOps()) != 0 {
			t.Errorf("%s: broker called", name)
		}
	}
	// No policy supplier at all (policy off): the class cannot exist either.
	e := newW3cEnv(t, func(c *Config) { c.Policy = nil })
	_, f := e.fieldsFor("create_project")
	if code, b := e.sockJSON(t, "POST", e.proposePath("create_project"), jsonBody(map[string]any{"action": "create_project", "subject": "app-one", "fields": f})); code != 400 || v1Code(b) != "policy_class_missing" {
		t.Errorf("no policy: %d %s", code, b)
	}
	// Provider and group actions do not depend on the class.
	if code, b := e.sockJSON(t, "POST", "/api/v1/providers/propose", jsonBody(map[string]any{"action": "delete_provider", "subject": "gl", "fields": map[string]any{}})); code != 200 {
		t.Errorf("delete_provider propose: %d %s", code, b)
	}
	// The class disappearing between propose and apply is caught at apply:
	// nothing is sent and no limiter slot is spent.
	cur := map[string]string{"docs": "L1", "registered": "L1"}
	e3 := newW3cEnv(t, func(c *Config) { c.Policy = func() PolicyView { return PolicyView{Hash: "h", Classes: cur} } })
	p := e3.proposeAction(t, "create_project")
	delete(cur, "registered")
	for i := 0; i < CreateProjectPerActorHour+3; i++ {
		if code, b := e3.apply(t, p); code != 400 || v1Code(b) != "policy_class_missing" {
			t.Fatalf("apply %d: %d %s", i+1, code, b)
		}
	}
	if len(e3.wb.allOps()) != 0 {
		t.Fatalf("broker called: %v", e3.wb.allOps())
	}
}
