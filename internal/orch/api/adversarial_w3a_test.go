package api

// Adversarial suite for W3a (Task 6) on the orchd side: the project endpoints
// propose/apply. A fake broker on a temp socket records every call; nothing
// reaches a real service. Every refusal test checks that nothing was sent to
// the broker (or exactly what was), and keeps an accepted control.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/registrymsg"
)

const w3aMarker = "W3A-MARKER-5c1e"

func (e *projEnv) assertBrokerUntouched(t *testing.T, what string) {
	t.Helper()
	if ops := e.rb.ops(); len(ops) != 0 {
		t.Fatalf("%s reached the broker: %v", what, ops)
	}
}

func (e *projEnv) apply(t *testing.T, body string) (int, []byte) {
	t.Helper()
	return e.sock(t, "POST", "/api/v1/projects/apply", body)
}

func TestProposeNeverWritesBroker(t *testing.T) {
	e := newProjEnv(t)
	good := e.registerFields()
	bodies := []string{
		jsonBody(map[string]any{"action": "register_project", "subject": "other", "fields": good}),
		jsonBody(map[string]any{"action": "register_project", "subject": "other", "fields": map[string]any{"path": e.other, "kind": "docs", "ai_context": true}}),
		jsonBody(map[string]any{"action": "set_flag", "subject": "reg", "fields": map[string]any{"flag": "ai_context", "value": true}}),
		jsonBody(map[string]any{"action": "set_flag", "subject": "no-such-project", "fields": map[string]any{"flag": "ai_context", "value": true}}),
		jsonBody(map[string]any{"action": "set_flag", "subject": "reg", "fields": map[string]any{"flag": "auto_merge", "value": true}}),
		jsonBody(map[string]any{"action": "register_project", "subject": "x", "fields": map[string]any{"path": e.outside, "kind": "docs", "ai_context": true}}),
		jsonBody(map[string]any{"action": "delete_project", "subject": "reg", "fields": map[string]any{}}),
		jsonBody(map[string]any{"action": "register_project", "subject": "../x", "fields": good}),
		`{"action":"register_project","subject":"other","fields":` + jsonBody(good) + `,"signature":{"signer":"a"}}`,
		`not json`, ``, `[]`,
	}
	for i, b := range bodies {
		e.sock(t, "POST", "/api/v1/projects/propose", b) // accepted or refused: both must stay local
		e.assertBrokerUntouched(t, "propose #"+itoa(int64(i)))
	}
	if w := e.rb.writes(); len(w) != 0 {
		t.Fatalf("writes %v", w)
	}
	// It also stores nothing in the ledger and creates no event.
	if raw, err := os.ReadFile(policy.OrchLedgerPath(e.home)); err == nil && strings.Contains(string(raw), "project_change") {
		t.Fatalf("propose wrote a project_change event:\n%s", raw)
	}
	// Control: the same proposal is accepted and only apply reaches the broker.
	p := e.propose(t, "register_project", "other", good)
	e.assertBrokerUntouched(t, "accepted propose")
	if code, b := e.apply(t, jsonBody(applyBodyOf(p, webauthnSig))); code != 200 {
		t.Fatalf("apply %d %s", code, b)
	}
	if w := e.rb.writes(); len(w) != 1 {
		t.Fatalf("writes after apply: %d", len(w))
	}
}

func TestApplyForgedFieldsRefusedBeforeBroker(t *testing.T) {
	e := newProjEnv(t)
	good := e.registerFields()
	base := e.propose(t, "register_project", "other", good)

	// forge builds an apply body for the given fields with a change that matches
	// them, so that only the server's own rules can refuse it.
	forge := func(action, subject string, fields map[string]any) string {
		h, err := registrymsg.ChangeHash(action, subject, fields)
		if err != nil {
			h = strings.Repeat("0", 64)
		}
		return jsonBody(map[string]any{"action": action, "subject": subject, "fields": fields, "change": h,
			"request": registrymsg.NewRequestID(), "issued": time.Now().UTC().Format("2006-01-02T15:04:05Z"), "signature": webauthnSig})
	}
	mod := func(f func(m map[string]any)) string {
		// deep copy: webauthnSig is shared by every test in the package
		var sig any
		if err := json.Unmarshal([]byte(jsonBody(webauthnSig)), &sig); err != nil {
			t.Fatal(err)
		}
		m := applyBodyOf(base, sig)
		f(m)
		return jsonBody(m)
	}
	raw := jsonBody(applyBodyOf(base, webauthnSig))
	link := filepath.Join(e.root, "lnk")
	if err := os.Symlink(e.other, link); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(e.root, "plain")
	os.Mkdir(plain, 0o755)
	cp := func(m map[string]any) map[string]any {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		return c
	}
	with := func(k string, v any) map[string]any { c := cp(good); c[k] = v; return c }
	cases := map[string]string{
		// shape forgeries
		"extra top-level key":        mod(func(m map[string]any) { m["admin"] = true }),
		"case-variant action":        strings.Replace(raw, `"action"`, `"Action"`, 1),
		"case-variant signature":     strings.Replace(raw, `"signature"`, `"Signature"`, 1),
		"duplicate subject":          strings.Replace(raw, `{`, `{"subject":"reg",`, 1),
		"duplicate change":           strings.Replace(raw, `{`, `{"change":"`+strings.Repeat("0", 64)+`",`, 1),
		"duplicate field key":        strings.Replace(raw, `"fields":{`, `"fields":{"path":"/etc",`, 1),
		"case-variant field key":     strings.Replace(raw, `"path"`, `"Path"`, 1),
		"extra field key":            mod(func(m map[string]any) { m["fields"].(map[string]any)["owner"] = "root" }),
		"missing field":              mod(func(m map[string]any) { delete(m["fields"].(map[string]any), "kind") }),
		"nested field value":         mod(func(m map[string]any) { m["fields"].(map[string]any)["kind"] = map[string]any{"a": 1} }),
		"string for bool":            mod(func(m map[string]any) { m["fields"].(map[string]any)["ai_context"] = "true" }),
		"null for bool":              mod(func(m map[string]any) { m["fields"].(map[string]any)["ai_context"] = nil }),
		"number for bool":            mod(func(m map[string]any) { m["fields"].(map[string]any)["ai_context"] = 1 }),
		"change missing":             mod(func(m map[string]any) { delete(m, "change") }),
		"change of other content":    mod(func(m map[string]any) { m["fields"].(map[string]any)["ai_context"] = true }),
		"change uppercase":           mod(func(m map[string]any) { m["change"] = strings.ToUpper(base.Change) }),
		"subject swapped":            mod(func(m map[string]any) { m["subject"] = "reg" }),
		"action swapped":             mod(func(m map[string]any) { m["action"] = "set_flag" }),
		"request uppercase":          mod(func(m map[string]any) { m["request"] = strings.ToUpper(base.Request) }),
		"request short":              mod(func(m map[string]any) { m["request"] = "abc" }),
		"issued not RFC 3339 Z":      mod(func(m map[string]any) { m["issued"] = "2026-11-09 12:00:00" }),
		"signature missing":          mod(func(m map[string]any) { delete(m, "signature") }),
		"signature null":             mod(func(m map[string]any) { m["signature"] = nil }),
		"signature string":           mod(func(m map[string]any) { m["signature"] = "c2ln" }),
		"signature extra key":        mod(func(m map[string]any) { m["signature"].(map[string]any)["issued"] = base.Issued }),
		"webauthn extra key":         mod(func(m map[string]any) { m["signature"].(map[string]any)["webauthn"].(map[string]any)["x"] = "y" }),
		"huge signature":             mod(func(m map[string]any) { m["signature"].(map[string]any)["sig"] = strings.Repeat("A", 1<<20) }),
		"unknown action":             forge("delete_project", "other", good),
		"path relative":              forge("register_project", "other", with("path", "other")),
		"path traversal":             forge("register_project", "other", with("path", e.root+"/other/../other")),
		"path unclean":               forge("register_project", "other", with("path", e.root+"//other")),
		"path outside the roots":     forge("register_project", "other", with("path", e.outside)),
		"path is a root":             forge("register_project", "other", with("path", e.root)),
		"path nested below a repo":   forge("register_project", "other", with("path", filepath.Join(e.other, "sub"))),
		"path is a symlink":          forge("register_project", "other", with("path", link)),
		"path is not a repository":   forge("register_project", "other", with("path", plain)),
		"path does not exist":        forge("register_project", "other", with("path", filepath.Join(e.root, "ghost"))),
		"path with a bidi override":  forge("register_project", "other", with("path", e.other+"‮")),
		"path with a newline":        forge("register_project", "other", with("path", e.other+"\n")),
		"kind not a policy class":    forge("register_project", "other", with("kind", "zzz")),
		"kind with other case":       forge("register_project", "other", with("kind", "Docs")),
		"kind hostile":               forge("register_project", "other", with("kind", "docs​")),
		"subject hostile":            forge("register_project", "Other Project", good),
		"subject too long":           forge("register_project", "a"+strings.Repeat("b", 40), good),
		"flag other than ai_context": forge("set_flag", "reg", map[string]any{"flag": "auto_merge", "value": true}),
		"flag value not bool":        forge("set_flag", "reg", map[string]any{"flag": "ai_context", "value": "yes"}),
		"flag with extra field":      forge("set_flag", "reg", map[string]any{"flag": "ai_context", "value": true, "path": "/x"}),
		"not json":                   `{"action":`,
		"empty body":                 ``,
		"array body":                 `[]`,
		"trailing data":              raw + ` {}`,
	}
	for name, body := range cases {
		code, b := e.apply(t, body)
		if code < 400 || code >= 500 {
			t.Errorf("%s: status %d %s", name, code, b)
		}
		if got := e.rb.ops(); len(got) != 0 {
			t.Fatalf("%s reached the broker: %v", name, got)
		}
	}
	// Control: the honest body passes the same gate and is forwarded.
	if code, b := e.apply(t, raw); code != 200 {
		t.Fatalf("honest apply %d %s", code, b)
	}
	if w := e.rb.writes(); len(w) != 1 {
		t.Fatalf("honest apply: %d broker writes", len(w))
	}
}

func TestProjectsLedgerNoPathsOrContent(t *testing.T) {
	e := newProjEnv(t)
	secretDir := filepath.Join(e.root, "dir-"+w3aMarker)
	mkRepo(t, secretDir)
	fields := map[string]any{"path": secretDir, "kind": "docs", "ai_context": true}
	replies := map[string]string{
		"ok":                "",
		"project_exists":    brokerErrReply("project_exists"),
		"bad_signature":     brokerErrReply("bad_signature"),
		"timeout":           brokerErrReply("timeout"),
		"drop":              "DROP",
		"garbage":           `{"ok":true,"result":"` + w3aMarker + `"}`,
		"text in a message": `{"ok":false,"code":"internal_error","message":"` + w3aMarker + `"}`,
	}
	n := 0
	for name, rep := range replies {
		e.rb.mu.Lock()
		e.rb.reply = func(string, json.RawMessage) string { return rep }
		e.rb.mu.Unlock()
		p := e.propose(t, "register_project", "m"+itoa(int64(n)), fields)
		n++
		code, b := e.apply(t, jsonBody(applyBodyOf(p, webauthnSig)))
		if strings.Contains(string(b), w3aMarker) && name != "ok" {
			t.Errorf("%s: broker text in the response: %d %s", name, code, b)
		}
	}
	lp := policy.OrchLedgerPath(e.home)
	raw, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{w3aMarker, secretDir, e.other, "dir-", "docs", "c2ln", "alice", "ai_context"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q in the ledger:\n%s", leak, raw)
		}
	}
	evs, err := ledger.Events(lp)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range evs {
		if ev.Type != "project_change" {
			continue
		}
		count++
		if len(ev.Payload) != 5 {
			t.Fatalf("payload %v", ev.Payload)
		}
		for _, k := range []string{"actor", "action", "subject", "ok", "code"} {
			if _, ok := ev.Payload[k]; !ok {
				t.Fatalf("payload lacks %s: %v", k, ev.Payload)
			}
		}
	}
	if count != len(replies) {
		t.Fatalf("%d project_change events for %d applies", count, len(replies))
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

func TestProjectWritesBoundedAndTimeoutIsUnknown(t *testing.T) {
	// A4 contract: a write that may have happened is never reported as a plain
	// failure and never retried, and a single actor cannot hammer the broker.
	for name, rep := range map[string]string{"timeout": brokerErrReply("timeout"), "connection dropped": "DROP"} {
		e := newProjEnv(t)
		e.rb.mu.Lock()
		e.rb.reply = func(string, json.RawMessage) string { return rep }
		e.rb.mu.Unlock()
		p := e.propose(t, "register_project", "other", e.registerFields())
		code, b := e.apply(t, jsonBody(applyBodyOf(p, webauthnSig)))
		if code != 504 || v1Code(b) != "outcome_unknown" {
			t.Fatalf("%s: %d %s", name, code, b)
		}
		if n := len(e.rb.writes()); n != 1 {
			t.Fatalf("%s: %d forwards (a write must not be retried)", name, n)
		}
	}
	e := newProjEnv(t)
	clk := &testClock{}
	e.d.handler.now = clk.now
	for i := 0; i < ProjectApplyPerActorHour+2; i++ {
		clk.set(time.Duration(i) * time.Millisecond)
		body := e.applyBody(t)
		code, b := e.apply(t, body)
		switch {
		case i < ProjectApplyPerActorHour && code != 200:
			t.Fatalf("apply %d: %d %s", i+1, code, b)
		case i >= ProjectApplyPerActorHour && (code != 429 || v1Code(b) != "project_rate_limited"):
			t.Fatalf("apply %d should be limited: %d %s", i+1, code, b)
		}
	}
	if n := len(e.rb.writes()); n != ProjectApplyPerActorHour {
		t.Fatalf("%d broker writes, want %d", n, ProjectApplyPerActorHour)
	}
}

func TestNoBrokerImportInOrchd(t *testing.T) {
	// The orchd binary must hold no broker code (no key handling, no
	// registry writer, no signature verifier path of its own).
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Skipf("no go tool: %v", err) // never silently passes where go is available
	}
	cmd := exec.Command("go", "list", "-deps", "./cmd/orchd")
	cmd.Dir = strings.TrimSpace(string(root))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		deps[l] = true
	}
	// Positive controls: the listing is real and holds what orchd must have.
	for _, must := range []string{"aigate/internal/orch/api", "aigate/internal/registrymsg", "aigate/internal/orch/broker"} {
		if !deps[must] {
			t.Fatalf("control: %s not among the dependencies of cmd/orchd (%d listed)", must, len(deps))
		}
	}
	for d := range deps {
		if d == "aigate/internal/broker" || strings.HasPrefix(d, "aigate/internal/broker/") || strings.HasPrefix(d, "aigate/cmd/orch-broker") {
			t.Fatalf("cmd/orchd depends on %s", d)
		}
		if strings.Contains(d, "openai") && strings.HasPrefix(d, "aigate/") {
			t.Fatalf("cmd/orchd depends on %s", d)
		}
	}
}

func TestOrchdForwardsSignatureAndNeverJudgesIt(t *testing.T) {
	// orchd checks the SHAPE of a signature only. Whether it is valid (algorithm,
	// signer, namespace) is the broker's call: an odd algorithm or signer id is
	// forwarded untouched and the broker's refusal is what the user sees.
	for name, mod := range map[string]func(sig map[string]any){
		"unknown algorithm":     func(s map[string]any) { s["alg"] = "rsa-sha1" },
		"odd signer id":         func(s map[string]any) { s["signer"] = "../../etc" },
		"ssh with webauthn":     func(s map[string]any) { s["alg"] = "ssh-ed25519" },
		"webauthn without data": func(s map[string]any) { delete(s, "webauthn") },
	} {
		e := newProjEnv(t)
		e.rb.mu.Lock()
		e.rb.reply = func(string, json.RawMessage) string { return brokerErrReply("bad_signature") }
		e.rb.mu.Unlock()
		var sig map[string]any
		if err := json.Unmarshal([]byte(jsonBody(webauthnSig)), &sig); err != nil {
			t.Fatal(err)
		}
		mod(sig)
		p := e.propose(t, "register_project", "other", e.registerFields())
		code, b := e.apply(t, jsonBody(applyBodyOf(p, sig)))
		if code != 403 || v1Code(b) != "bad_signature" {
			t.Fatalf("%s: %d %s", name, code, b)
		}
		w := e.rb.writes()
		if len(w) != 1 {
			t.Fatalf("%s: %d forwards", name, len(w))
		}
		var args struct {
			Signature map[string]any `json:"signature"`
		}
		if err := json.Unmarshal(w[0].Args, &args); err != nil || args.Signature["alg"] != sig["alg"] || args.Signature["signer"] != sig["signer"] {
			t.Fatalf("%s: signature not forwarded unchanged: %s", name, w[0].Args)
		}
	}
}
