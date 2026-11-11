package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
)

func TestW3cApplyRecomputesChangeAndRefusesMismatch(t *testing.T) {
	for _, action := range w3cActions {
		e := newW3cEnv(t)
		p := e.proposeAction(t, action)
		tamper := map[string]func(m map[string]any){
			"change replaced": func(m map[string]any) { m["change"] = strings.Repeat("0", 64) },
			"subject":         func(m map[string]any) { m["subject"] = "other-name" },
		}
		switch action {
		case "set_provider":
			tamper["field changed"] = func(m map[string]any) { m["fields"].(map[string]any)["base_url"] = "http://evil.example:18082" }
			tamper["token swapped"] = func(m map[string]any) { m["fields"].(map[string]any)["token_ref"] = "other-token" }
		case "create_group":
			tamper["field changed"] = func(m map[string]any) { m["fields"].(map[string]any)["parent_path"] = "" }
		case "create_project":
			tamper["field changed"] = func(m map[string]any) { m["fields"].(map[string]any)["publish"] = "read" }
			tamper["ai flipped"] = func(m map[string]any) { m["fields"].(map[string]any)["ai_context"] = true }
			tamper["hash swapped"] = func(m map[string]any) { m["fields"].(map[string]any)["deploy_key_sha256"] = strings.Repeat("b", 64) }
			tamper["template"] = func(m map[string]any) { m["fields"].(map[string]any)["template"] = "basic" }
		}
		for name, mod := range tamper {
			m := e.applyBody(p, webauthnSig)
			mod(m)
			code, b := e.sockJSON(t, "POST", e.applyPath(action), jsonBody(m))
			if code != 400 && code != 409 {
				t.Errorf("%s/%s: %d %s", action, name, code, b)
			}
			if c := v1Code(b); c != "change_mismatch" && c != "bad_request" && c != "deploy_key_mismatch" {
				t.Errorf("%s/%s: code %s", action, name, c)
			}
		}
		// structural refusals
		for name, mod := range map[string]func(m map[string]any){
			"unknown top key":   func(m map[string]any) { m["extra"] = 1 },
			"no issued":         func(m map[string]any) { delete(m, "issued") },
			"bad request id":    func(m map[string]any) { m["request"] = "xyz" },
			"bad issued format": func(m map[string]any) { m["issued"] = "2026-11-09 10:00:00" },
			"change not hex":    func(m map[string]any) { m["change"] = "ZZ" },
		} {
			m := e.applyBody(p, webauthnSig)
			mod(m)
			if code, b := e.sockJSON(t, "POST", e.applyPath(action), jsonBody(m)); code != 400 {
				t.Errorf("%s/%s: %d %s", action, name, code, b)
			}
		}
		// unsigned
		m := e.applyBody(p, nil)
		if code, b := e.sockJSON(t, "POST", e.applyPath(action), jsonBody(m)); code != 400 || v1Code(b) != "not_signed" {
			t.Errorf("%s unsigned: %d %s", action, code, b)
		}
		if n := len(e.wb.writeCalls()); n != 0 {
			t.Errorf("%s: %d writes reached the broker", action, n)
		}
		// the untouched body goes through
		if code, b := e.apply(t, p); code != 200 {
			t.Errorf("%s: good apply %d %s", action, code, b)
		}
	}
}

func TestApplyForwardsDeployKeyAndSignature(t *testing.T) {
	wantKeys := map[string]string{
		"set_provider":    "base_url,change,id,issued,kind,request,signature,ssh_url,token_ref",
		"delete_provider": "change,id,issued,request,signature",
		"create_group":    "change,id,issued,name,parent_path,path,request,signature",
		"create_project":  "ai_context,change,deploy_key,deploy_key_sha256,description,group_path,id,issued,provider,publish,request,signature,template",
	}
	for _, action := range w3cActions {
		e := newW3cEnv(t)
		p := e.proposeAction(t, action)
		if code, b := e.apply(t, p); code != 200 {
			t.Fatalf("%s: %d %s", action, code, b)
		}
		calls := e.wb.opsNamed(action)
		if len(calls) != 1 {
			t.Fatalf("%s: %d calls", action, len(calls))
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal(calls[0].Args, &args); err != nil {
			t.Fatal(err)
		}
		keys := []string{}
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != wantKeys[action] {
			t.Fatalf("%s args %v", action, keys)
		}
		str := func(k string) string { var s string; json.Unmarshal(args[k], &s); return s }
		if str("id") != p.Subject || str("change") != p.Change || str("request") != p.Request || str("issued") != p.Issued {
			t.Fatalf("%s: identity args differ: %s", action, calls[0].Args)
		}
		wantSig, _ := json.Marshal(webauthnSig)
		var a, b any
		json.Unmarshal(args["signature"], &a)
		json.Unmarshal(wantSig, &b)
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Fatalf("%s: signature\n got %s\nwant %s", action, ja, jb)
		}
		for k, v := range p.Fields {
			var got any
			json.Unmarshal(args[k], &got)
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(v)
			if string(gj) != string(wj) {
				t.Errorf("%s: arg %s = %s want %s", action, k, gj, wj)
			}
		}
		if action == "create_project" && str("deploy_key") != e.pubLine {
			t.Fatalf("deploy_key forwarded as %q, want the .pub line %q", str("deploy_key"), e.pubLine)
		}
	}
	// A rotated .pub between propose and apply: refused here, nothing sent.
	e := newW3cEnv(t)
	p := e.proposeAction(t, "create_project")
	line2, _ := sshPubLine(t, "rotated")
	writeFile(t, e.key+".pub", line2+"\n")
	code, b := e.apply(t, p)
	if code != 409 || v1Code(b) != "deploy_key_mismatch" {
		t.Fatalf("rotated key: %d %s", code, b)
	}
	if len(e.wb.allOps()) != 0 {
		t.Fatalf("broker called after a key mismatch: %v", e.wb.allOps())
	}
	// A .pub that disappeared: unavailable, nothing sent.
	writeFile(t, e.key+".pub", "")
	if code, b := e.apply(t, p); code != 503 || v1Code(b) != "deploy_key_unavailable" {
		t.Fatalf("missing key: %d %s", code, b)
	}
}

func TestApplyMapsAllNewBrokerCodes(t *testing.T) {
	type want struct {
		status int
		code   string
	}
	table := map[string]want{
		"not_signed": {400, "not_signed"}, "bad_signature": {403, "bad_signature"}, "unknown_signer": {403, "unknown_signer"},
		"stale_signature": {403, "stale_signature"}, "signers_unavailable": {503, "signers_unavailable"}, "request_reused": {409, "request_reused"},
		"change_mismatch": {400, "change_mismatch"}, "path_not_allowed": {400, "path_not_allowed"}, "project_exists": {409, "project_exists"},
		"provider_unknown": {404, "provider_unknown"}, "provider_exists": {409, "provider_exists"}, "provider_in_use": {409, "provider_in_use"},
		"bad_url": {400, "bad_url"}, "token_ref_unknown": {400, "token_ref_unknown"}, "provider_unreachable": {502, "provider_unreachable"},
		"gitlab_refused": {502, "gitlab_refused"}, "group_unknown": {404, "group_unknown"}, "group_exists": {409, "group_exists"},
		"project_name_taken": {409, "project_name_taken"}, "deploy_key_mismatch": {409, "deploy_key_mismatch"}, "template_unknown": {400, "template_unknown"},
		"rate_limited": {429, "broker_rate_limited"}, "tokens_unavailable": {503, "tokens_unavailable"},
		"bad_request": {400, "bad_request"}, "too_large": {400, "bad_request"},
		"busy": {503, "broker_unavailable"}, "forbidden": {503, "broker_unavailable"}, "unknown_op": {503, "broker_unavailable"}, "not_implemented": {503, "broker_unavailable"},
		"registry_unavailable": {504, "outcome_unknown"}, "timeout": {504, "outcome_unknown"}, "internal_error": {504, "outcome_unknown"},
		"some_new_code": {504, "outcome_unknown"},
	}
	newCodes := []string{"provider_unknown", "provider_exists", "provider_in_use", "bad_url", "token_ref_unknown", "provider_unreachable", "gitlab_refused",
		"group_unknown", "group_exists", "project_name_taken", "deploy_key_mismatch", "template_unknown", "rate_limited"}
	sentences := map[string]string{}
	for _, action := range w3cActions {
		for brokerCode, w := range table {
			e := newW3cEnv(t)
			bc := brokerCode
			e.wb.setReply(func(op string, _ json.RawMessage) string {
				if op == action {
					return brokerErrReply(bc)
				}
				return ""
			})
			p := e.proposeAction(t, action)
			code, b := e.apply(t, p)
			if code != w.status || v1Code(b) != w.code {
				t.Errorf("%s/%s: %d %s (want %d %s)", action, brokerCode, code, b, w.status, w.code)
			}
			for _, leak := range []string{"SECRET-BROKER-TEXT", "sk-live-abc", "evil.example"} {
				if strings.Contains(string(b), leak) {
					t.Errorf("%s/%s: broker text %q leaked: %s", action, brokerCode, leak, b)
				}
			}
			var env struct{ Error struct{ Message string } }
			json.Unmarshal(b, &env)
			if env.Error.Message == "" {
				t.Errorf("%s/%s: no message", action, brokerCode)
			}
			if action == "create_project" {
				sentences[brokerCode] = env.Error.Message
			}
		}
	}
	// Each new code has its own fixed sentence.
	seen := map[string]string{}
	for _, c := range newCodes {
		m := sentences[c]
		if prev, dup := seen[m]; dup {
			t.Errorf("codes %s and %s share a sentence", prev, c)
		}
		seen[m] = c
	}
	// create_partial carries the orphan facts (public values only).
	e := newW3cEnv(t)
	detail := `{"provider":"gl","project":"team/app-one","project_id":99,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false}`
	e.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "create_project" {
			return `{"ok":false,"code":"create_partial","message":"SECRET-BROKER-TEXT","detail":` + detail + `}`
		}
		return ""
	})
	p := e.proposeAction(t, "create_project")
	code, b := e.apply(t, p)
	var env struct {
		Error struct {
			Code, Message string
			Detail        map[string]any
		}
	}
	if err := json.Unmarshal(b, &env); err != nil || code != 502 || env.Error.Code != "create_partial" {
		t.Fatalf("create_partial: %d %s", code, b)
	}
	if ledgerKeys(env.Error.Detail) != "orphan_dropped,orphan_recorded,project,project_id,provider,step" || env.Error.Detail["project"] != "team/app-one" ||
		env.Error.Detail["step"] != "deploy_key" || env.Error.Detail["project_id"] != float64(99) || env.Error.Detail["orphan_recorded"] != true {
		t.Fatalf("detail %v", env.Error.Detail)
	}
	if strings.Contains(string(b), "SECRET-BROKER-TEXT") || !strings.Contains(env.Error.Message, "GitLab") {
		t.Fatalf("message %q", env.Error.Message)
	}
	// An invalid detail is dropped (the code and sentence stay).
	e2 := newW3cEnv(t)
	e2.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "create_project" {
			return `{"ok":false,"code":"create_partial","detail":{"provider":"gl","project":"../etc/x","project_id":1,"step":"project","orphan_recorded":true,"orphan_dropped":false}}`
		}
		return ""
	})
	code, b = e2.apply(t, e2.proposeAction(t, "create_project"))
	env.Error.Detail = nil
	json.Unmarshal(b, &env)
	if code != 502 || env.Error.Code != "create_partial" || env.Error.Detail != nil || strings.Contains(string(b), "etc/x") {
		t.Fatalf("invalid detail: %d %s", code, b)
	}
	// create_partial is only ever a create_project outcome: on another op it is an unknown outcome.
	e3 := newW3cEnv(t)
	e3.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "set_provider" {
			return `{"ok":false,"code":"create_partial","detail":` + detail + `}`
		}
		return ""
	})
	if code, b := e3.apply(t, e3.proposeAction(t, "set_provider")); code != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("create_partial on set_provider: %d %s", code, b)
	}
}

func TestW3cWriteTimeoutIsOutcomeUnknown(t *testing.T) {
	for name, reply := range map[string]func(op string, _ json.RawMessage) string{
		"client deadline": func(op string, _ json.RawMessage) string {
			if op == "create_project" {
				return "HANG"
			}
			return ""
		},
		"broker timeout reply": func(op string, _ json.RawMessage) string {
			if op == "create_project" {
				return brokerErrReply("timeout")
			}
			return ""
		},
	} {
		e := newW3cEnv(t)
		e.d.handler.broker.Timeout = 300 * time.Millisecond
		e.wb.setReply(reply)
		p := e.proposeAction(t, "create_project")
		code, b := e.apply(t, p)
		if code != 504 || v1Code(b) != "outcome_unknown" {
			t.Fatalf("%s: %d %s", name, code, b)
		}
		time.Sleep(200 * time.Millisecond)
		if n := len(e.wb.opsNamed("create_project")); n != 1 {
			t.Fatalf("%s: %d create_project calls (never retried)", name, n)
		}
		if n := len(e.cloneCalls()); n != 0 {
			t.Fatalf("%s: cloned after an unknown outcome", name)
		}
		ev := e.ledgerPayloads(t, "project_create")
		if len(ev) != 1 || ev[0]["ok"] != false || ev[0]["code"] != "timeout" || ev[0]["subject"] != "app-one" {
			t.Fatalf("%s: ledger %v", name, ev)
		}
		if e.changeCount() != 1 {
			t.Fatalf("%s: resolver cache not dropped after an unknown outcome (%d)", name, e.changeCount())
		}
		// the message says to look at GitLab and the list, and promises no retry
		var env struct{ Error struct{ Message string } }
		json.Unmarshal(b, &env)
		if !strings.Contains(env.Error.Message, "GitLab") || !strings.Contains(env.Error.Message, "다시 시도하지 않았습니다") {
			t.Fatalf("%s: message %q", name, env.Error.Message)
		}
	}
	// a dropped connection is unknown too, but not a "timeout"
	e := newW3cEnv(t)
	e.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "set_provider" {
			return "DROP"
		}
		return ""
	})
	if code, b := e.apply(t, e.proposeAction(t, "set_provider")); code != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("drop: %d %s", code, b)
	}
	if ev := e.ledgerPayloads(t, "provider_change"); len(ev) != 1 || ev[0]["ok"] != false || ev[0]["code"] != "outcome_unknown" {
		t.Fatalf("drop ledger %v", ev)
	}
}

func TestLimitsAndRefundRules(t *testing.T) {
	buckets := map[string]struct {
		actions []string
		n       int
	}{
		"create_project": {[]string{"create_project"}, CreateProjectPerActorHour},
		"create_group":   {[]string{"create_group"}, CreateGroupPerActorHour},
		"provider":       {[]string{"set_provider", "delete_provider"}, ProviderWritePerActorHour},
	}
	if CreateProjectPerActorHour != 5 || CreateGroupPerActorHour != 5 || ProviderWritePerActorHour != 10 || ListGroupsPerActorHour != 60 {
		t.Fatal("D9 limits changed")
	}
	// every successful call spends a slot; per actor; per bucket; per hour
	for name, bk := range buckets {
		e := newW3cEnv(t)
		clk := &testClock{}
		e.d.handler.now = clk.now
		i := 0
		next := func(h func(string, string) (int, []byte)) (int, []byte) {
			action := bk.actions[i%len(bk.actions)]
			i++
			s, f := e.fieldsFor(action)
			if action == "create_project" {
				s = "app-" + string(rune('a'+i))
			}
			c, b := e.sockJSON(t, "POST", e.proposePath(action), jsonBody(map[string]any{"action": action, "subject": s, "fields": f}))
			if c != 200 {
				t.Fatalf("%s propose: %d %s", name, c, b)
			}
			var p w3cProposal
			json.Unmarshal(b, &p)
			return h(e.applyPath(action), jsonBody(e.applyBody(p, webauthnSig)))
		}
		sock := func(path, body string) (int, []byte) { return e.sockJSON(t, "POST", path, body) }
		for k := 0; k < bk.n; k++ {
			if c, b := next(sock); c != 200 {
				t.Fatalf("%s apply %d: %d %s", name, k+1, c, b)
			}
		}
		before := len(e.wb.writeCalls())
		c, b := next(sock)
		if c != 429 || v1Code(b) != "project_rate_limited" || len(e.wb.writeCalls()) != before {
			t.Fatalf("%s: %d %s writes %d->%d", name, c, b, before, len(e.wb.writeCalls()))
		}
		other := e.tokenCallerW3c(t, "mac-ssh")
		if c, b := next(func(p, body string) (int, []byte) { return other("POST", p, body) }); c != 200 {
			t.Fatalf("%s second actor: %d %s", name, c, b)
		}
		// the other buckets are unaffected
		for other, obk := range buckets {
			if other == name {
				continue
			}
			a := obk.actions[0]
			s, f := e.fieldsFor(a)
			p := e.propose(t, a, s, f)
			if c, b := e.apply(t, p); c == 429 {
				t.Fatalf("%s exhausted spills into %s: %d %s", name, other, c, b)
			}
		}
		clk.set(time.Hour + time.Minute)
		if c, b := next(sock); c != 200 {
			t.Fatalf("%s after an hour: %d %s", name, c, b)
		}
	}
	// A request that provably cost nothing gives its slot back: dead socket, busy, forbidden.
	for _, action := range w3cActions {
		n := map[string]int{"create_project": CreateProjectPerActorHour, "create_group": CreateGroupPerActorHour, "set_provider": ProviderWritePerActorHour, "delete_provider": ProviderWritePerActorHour}[action]
		dead := newW3cEnv(t, func(c *Config) { c.BrokerSocket = filepath.Join(shortDir(t), "none.sock") })
		p := dead.proposeAction(t, action)
		for k := 0; k < n+4; k++ {
			if c, b := dead.apply(t, p); c != 503 || v1Code(b) != "broker_unavailable" {
				t.Fatalf("%s dead try %d: %d %s", action, k+1, c, b)
			}
		}
		for _, bc := range []string{"busy", "forbidden"} {
			e := newW3cEnv(t)
			code := bc
			e.wb.setReply(func(op string, _ json.RawMessage) string {
				if op == action {
					return brokerErrReply(code)
				}
				return ""
			})
			for k := 0; k < n+3; k++ {
				p := e.proposeAction(t, action)
				if c, b := e.apply(t, p); c != 503 {
					t.Fatalf("%s %s try %d: %d %s", action, bc, k+1, c, b)
				}
			}
		}
		// Anything the broker answered after reading the request keeps the slot.
		for _, bc := range []string{"bad_signature", "stale_signature", "unknown_signer", "request_reused", "change_mismatch", "timeout", "gitlab_refused",
			"provider_unreachable", "project_name_taken", "group_exists", "create_partial", "internal_error"} {
			e := newW3cEnv(t)
			code := bc
			e.wb.setReply(func(op string, _ json.RawMessage) string {
				if op == action {
					return brokerErrReply(code)
				}
				return ""
			})
			for k := 0; k < n; k++ {
				p := e.proposeAction(t, action)
				if c, b := e.apply(t, p); c == 429 {
					t.Fatalf("%s %s: limited early at %d: %s", action, bc, k+1, b)
				}
			}
			p := e.proposeAction(t, action)
			if c, b := e.apply(t, p); c != 429 || v1Code(b) != "project_rate_limited" {
				t.Errorf("%s %s kept no slot: %d %s", action, bc, c, b)
			}
		}
	}
	// A refusal before the broker (mismatch, policy, key) costs nothing.
	e := newW3cEnv(t)
	p := e.proposeAction(t, "create_group")
	m := e.applyBody(p, webauthnSig)
	m["fields"].(map[string]any)["name"] = "different"
	for k := 0; k < CreateGroupPerActorHour+3; k++ {
		if c, _ := e.sockJSON(t, "POST", e.applyPath("create_group"), jsonBody(m)); c != 400 {
			t.Fatalf("mismatch try %d: %d", k+1, c)
		}
	}
	if c, b := e.apply(t, p); c != 200 {
		t.Fatalf("good apply after refusals: %d %s", c, b)
	}
}

func TestLedgerEventsNoSecrets(t *testing.T) {
	e := newW3cEnv(t)
	const canary = "CANARY-DESC-한글"
	_, f := e.fieldsFor("create_project")
	f["description"] = canary
	p := e.propose(t, "create_project", "app-one", f)
	if c, b := e.apply(t, p); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	// a refused create and one provider write of each kind
	e.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "create_group" {
			return brokerErrReply("group_exists")
		}
		return ""
	})
	var last w3cProposal
	for _, a := range []string{"set_provider", "create_group", "delete_provider"} {
		last = e.proposeAction(t, a)
		e.apply(t, last)
	}
	p2 := e.propose(t, "create_project", "app-two", f)
	e.wb.setReply(func(op string, _ json.RawMessage) string {
		if op == "create_project" {
			return brokerErrReply("project_name_taken")
		}
		return ""
	})
	if c, _ := e.apply(t, p2); c != 409 {
		t.Fatal(c)
	}
	lp := policy.OrchLedgerPath(e.home)
	raw := readFile(t, lp)
	for _, leak := range []string{canary, e.pubLine, sha256Hex(e.pubLine), p.Request, p.Change, p.Message, "c2ln", "alice", "SECRET-BROKER-TEXT",
		"gitlab.example", "gl-admin", "ssh://", "team/app-one", "newgroup", "YXV0aA"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("%q in the ledger:\n%s", leak, raw)
		}
	}
	creates := e.ledgerPayloads(t, "project_create")
	provs := e.ledgerPayloads(t, "provider_change")
	if len(creates) != 2 || len(provs) != 3 {
		t.Fatalf("%d project_create, %d provider_change events:\n%s", len(creates), len(provs), raw)
	}
	for _, pl := range append(append([]map[string]any{}, creates...), provs...) {
		if ledgerKeys(pl) != "action,actor,code,ok,subject" || pl["actor"] != socketActor() {
			t.Fatalf("payload %v", pl)
		}
	}
	if creates[0]["ok"] != true || creates[0]["code"] != "ok" || creates[0]["action"] != "create_project" || creates[0]["subject"] != "app-one" ||
		creates[1]["ok"] != false || creates[1]["code"] != "project_name_taken" {
		t.Fatalf("%v", creates)
	}
	if provs[0]["action"] != "set_provider" || provs[1]["action"] != "create_group" || provs[1]["code"] != "group_exists" || provs[2]["action"] != "delete_provider" {
		t.Fatalf("%v", provs)
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

func TestRequireWriteAndCSRF(t *testing.T) {
	e := newW3cEnv(t)
	srv := e.httpServer(t)
	tok, csrf := e.newSession(t, "alice")
	token := e.newToken(t, "mac-ssh")
	type target struct{ path, body string }
	build := func() []target {
		var out []target
		for _, a := range []string{"set_provider", "create_project"} {
			p := e.proposeAction(t, a)
			s, f := e.fieldsFor(a)
			out = append(out, target{e.proposePath(a), jsonBody(map[string]any{"action": a, "subject": s, "fields": f})},
				target{e.applyPath(a), jsonBody(e.applyBody(p, webauthnSig))})
		}
		out = append(out, target{"/api/v1/projects/app-one/clone", ""})
		return out
	}
	e.wb.mu.Lock()
	e.wb.projects = []map[string]any{{"id": "app-one", "path": filepath.Join(e.root, "app-one"), "kind": "registered", "ai_context": false, "provider": "gl",
		"gitlab": map[string]any{"project": "team/app-one", "project_id": 99}, "auto_merge": map[string]any{"enabled": false}}}
	e.wb.mu.Unlock()
	for _, tg := range build() {
		before, cloneBefore := len(e.wb.allOps()), len(e.cloneCalls())
		for name, h := range map[string]hdr{
			"no credentials":     {},
			"session no csrf":    {"Cookie": cookie(tok)},
			"session wrong csrf": {"Cookie": cookie(tok), CSRFHeader: "csrf-wrong"},
			"session empty csrf": {"Cookie": cookie(tok), CSRFHeader: ""},
			"bad bearer":         {"Authorization": "Bearer nope"},
		} {
			code, _, b := httpDo(t, srv, "POST", tg.path, tg.body, h)
			want := 403
			if name == "no credentials" || name == "bad bearer" {
				want = 401
			}
			if code != want {
				t.Errorf("%s %s: %d %s", tg.path, name, code, b)
			}
		}
		if len(e.wb.allOps()) != before || len(e.cloneCalls()) != cloneBefore {
			t.Errorf("%s: reached the broker or git without authority", tg.path)
		}
		for name, h := range map[string]hdr{"session": {"Cookie": cookie(tok), CSRFHeader: csrf}, "token": {"Authorization": "Bearer " + token}} {
			if strings.HasSuffix(tg.path, "/create/apply") {
				os.RemoveAll(filepath.Join(e.root, "app-one")) // the first create left its folder: a second create of the name is refused (path_exists)
			}
			code, _, b := httpDo(t, srv, "POST", tg.path, tg.body, h)
			if code != 200 && !(strings.HasSuffix(tg.path, "/clone") && code == 409) {
				t.Errorf("%s %s: %d %s", tg.path, name, code, b)
			}
		}
	}
	for _, path := range []string{"/api/v1/providers", "/api/v1/providers/gl/groups"} {
		if code, _, _ := httpDo(t, srv, "GET", path, "", hdr{}); code != 401 {
			t.Errorf("GET %s without credentials: %d", path, code)
		}
	}
	// GET is not a write: a wrong method on the write paths is refused by the mux.
	if code, _, _ := httpDo(t, srv, "GET", "/api/v1/providers/apply", "", hdr{"Authorization": "Bearer " + token}); code != 405 && code != 404 {
		t.Errorf("GET on apply: %d", code)
	}
}
