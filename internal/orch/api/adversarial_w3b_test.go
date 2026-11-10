package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W3b adversarial suite for orchd (B6). Equivalent checks from B4 are
// referenced in docs/23 instead of being copied:
//   TestApplyForwardsValueExactlyOnceAndNeverLogsIt  (value never in ledger, logs, errors, responses on any path)
//   TestTokensWorkWithBrokerDown / TestOrchdWorksWithBrokerDown (W2b)
//   TestNoBrokerImportInOrchd (W3a, go list -deps of cmd/orchd)

// TestBeginAndMessageNeverWriteBroker: the two preparation endpoints make no
// broker call at all (not even a read), write no ledger line and no file, and
// never carry a value, whatever is asked of them.
func TestBeginAndMessageNeverWriteBroker(t *testing.T) {
	e := newTokEnv(t)
	ledgerBefore := e.ledgerRaw(t)
	for i := 0; i < 6; i++ {
		e.flow(t, "set_token", "gitlab-main", "gitlab", canaryValue)
		e.flow(t, "set_token", "github-one", "github", canaryValue)
		e.flow(t, "set_token", "openai", "openai", canaryValue)
		e.flow(t, "delete_token", "gitlab-main", "", "")
	}
	// refused requests too
	for _, body := range []string{
		`{"action":"set_token","subject":"gitlab-main","fields":{"kind":"gitlab","expires":""},"value":"` + canaryValue + `"}`,
		`{"action":"register_project","subject":"gitlab-main","fields":{}}`,
		`{"action":"set_token","subject":"Bad Ref","fields":{"kind":"gitlab","expires":""}}`,
		`not json`,
	} {
		for _, ep := range []string{"begin", "message"} {
			e.call(t, ep, body)
		}
	}
	if ops := e.rb.ops(); len(ops) != 0 {
		t.Fatalf("begin/message called the broker: %v", ops)
	}
	if w := e.rb.tokenWrites(); len(w) != 0 {
		t.Fatalf("begin/message wrote tokens: %v", w)
	}
	if got := e.ledgerRaw(t); got != ledgerBefore {
		t.Fatalf("begin/message changed the ledger:\n%s", got)
	}
	if strings.Contains(e.logs.String(), canaryValue) {
		t.Fatal("value in the log")
	}
	// The same is true for the real apply afterwards: exactly one write for one apply.
	f := e.setFlow(t)
	if code, b := e.apply(t, f); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if w := e.rb.tokenWrites(); len(w) != 1 {
		t.Fatalf("one apply produced %d broker writes", len(w))
	}
}

// TestTokensApplyForgedFieldsRefusedBeforeBroker: every forged or malformed
// apply body is refused by orchd itself; the broker is never reached and
// nothing about the value is echoed or logged.
func TestTokensApplyForgedFieldsRefusedBeforeBroker(t *testing.T) {
	e := newTokEnv(t)
	f := e.setFlow(t)
	other := e.setFlow(t)
	clone := func() map[string]any {
		m := f.body(webauthnSig)
		fm := map[string]any{}
		for k, v := range f.Fields {
			fm[k] = v
		}
		m["fields"] = fm
		return m
	}
	fieldsOf := func(m map[string]any) map[string]any { return m["fields"].(map[string]any) }
	cases := map[string]func(m map[string]any){
		"extra top-level key":     func(m map[string]any) { m["extra"] = "x" },
		"extra field":             func(m map[string]any) { fieldsOf(m)["x"] = "y" },
		"missing value_hash":      func(m map[string]any) { delete(fieldsOf(m), "value_hash") },
		"upper-case value_hash":   func(m map[string]any) { fieldsOf(m)["value_hash"] = strings.ToUpper(f.Fields["value_hash"].(string)) },
		"hash of another request": func(m map[string]any) { fieldsOf(m)["value_hash"] = other.Fields["value_hash"] },
		"request of another flow": func(m map[string]any) { m["request"] = other.Request },
		"short request":           func(m map[string]any) { m["request"] = f.Request[:31] },
		"upper-case request":      func(m map[string]any) { m["request"] = strings.ToUpper(f.Request) },
		"garbled issued":          func(m map[string]any) { m["issued"] = "yesterday" },
		"no signature":            func(m map[string]any) { delete(m, "signature") },
		"signature as string":     func(m map[string]any) { m["signature"] = "c2ln" },
		"signature with extra field": func(m map[string]any) {
			m["signature"] = map[string]any{"signer": "alice", "alg": "webauthn-es256", "sig": "c2ln", "x": 1}
		},
		"set_flag action":            func(m map[string]any) { m["action"] = "set_flag" },
		"register_project action":    func(m map[string]any) { m["action"] = "register_project" },
		"unknown action":             func(m map[string]any) { m["action"] = "rotate_token" },
		"subject with a space":       func(m map[string]any) { m["subject"] = "gitlab main" },
		"subject upper case":         func(m map[string]any) { m["subject"] = "Gitlab-Main" },
		"openai kind, other subject": func(m map[string]any) { fieldsOf(m)["kind"] = "openai" },
		"value missing":              func(m map[string]any) { delete(m, "value") },
		"value null":                 func(m map[string]any) { m["value"] = nil },
		"value as number":            func(m map[string]any) { m["value"] = 12345 },
		"value with a space":         func(m map[string]any) { m["value"] = canaryValue + " " },
		"value with a newline":       func(m map[string]any) { m["value"] = canaryValue + "\n" },
		"value too short":            func(m map[string]any) { m["value"] = canaryValue[:20] },
		"value of another flow":      func(m map[string]any) { m["value"] = canaryValue + "-other" },
		"expires in another format":  func(m map[string]any) { fieldsOf(m)["expires"] = "31.12.2099" },
	}
	for name, mod := range cases {
		m := clone()
		mod(m)
		code, b := e.call(t, "apply", jsonBody(m))
		if code < 400 || code > 499 {
			t.Errorf("%s: %d %s", name, code, b)
		}
		if strings.Contains(string(b), canaryValue) {
			t.Errorf("%s: value echoed: %s", name, b)
		}
	}
	// A duplicate key is refused too (raw JSON, a marshaller would drop it).
	good := jsonBody(f.body(webauthnSig))
	dup := strings.Replace(good, `"value":"`+canaryValue+`"`, `"value":"`+canaryValue+`","value":"`+canaryValue+`"`, 1)
	if dup == good {
		t.Fatal("test bug: duplicate not built")
	}
	if code, b := e.call(t, "apply", dup); code < 400 || code > 499 || strings.Contains(string(b), canaryValue) {
		t.Errorf("duplicate key: %d %s", code, b)
	}
	if ops := e.rb.ops(); len(ops) != 0 {
		t.Fatalf("a forged apply reached the broker: %v", ops)
	}
	if strings.Contains(e.logs.String(), canaryValue) || strings.Contains(e.ledgerRaw(t), canaryValue) {
		t.Fatal("value in the log or the ledger")
	}
	// No refusal burned anything: the honest request still passes once.
	if code, b := e.apply(t, f); code != 200 {
		t.Fatalf("control: %d %s", code, b)
	}
	if w := e.rb.tokenWrites(); len(w) != 1 {
		t.Fatalf("%d broker writes", len(w))
	}
}

// TestDeployKeyNeverReadsPrivateKey: the endpoint reads only <key>.pub. A
// private key that is the only thing present, a .pub that is secretly the
// private key (hard link, symlink) or a directory never leaks a byte.
func TestDeployKeyNeverReadsPrivateKey(t *testing.T) {
	get := func(keyPath string) (int, []byte) {
		e := newDeployKeyEnv(t, keyPath)
		c, _, b := e.sockDo(t, "GET", "/api/v1/settings/deploy-key", "", nil)
		return c, b
	}
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nPRIVATE-CANARY-NOT-A-REAL-KEY-W3B\n-----END OPENSSH PRIVATE KEY-----\n"
	noLeak := func(what string, code int, b []byte) {
		t.Helper()
		if code != 503 || v1Code(b) != "deploy_key_unavailable" {
			t.Errorf("%s: %d %s", what, code, b)
		}
		if strings.Contains(string(b), "PRIVATE-CANARY") || strings.Contains(string(b), "OPENSSH") {
			t.Errorf("%s leaked private bytes: %s", what, b)
		}
	}
	// only the private key exists (no .pub)
	d1 := shortDir(t)
	k1 := filepath.Join(d1, "id")
	os.WriteFile(k1, []byte(pem), 0o600)
	c, b := get(k1)
	noLeak("private only", c, b)
	// .pub is a hard link to the private key (same bytes, same inode)
	d2 := shortDir(t)
	k2 := filepath.Join(d2, "id")
	os.WriteFile(k2, []byte(pem), 0o600)
	if err := os.Link(k2, k2+".pub"); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	c, b = get(k2)
	noLeak("hard-linked .pub", c, b)
	// .pub is a symlink to the private key
	d3 := shortDir(t)
	k3 := filepath.Join(d3, "id")
	os.WriteFile(k3, []byte(pem), 0o600)
	os.Symlink(k3, k3+".pub")
	c, b = get(k3)
	noLeak("symlinked .pub", c, b)
	// .pub is a directory
	d4 := shortDir(t)
	k4 := filepath.Join(d4, "id")
	os.WriteFile(k4, []byte(pem), 0o600)
	os.Mkdir(k4+".pub", 0o755)
	c, b = get(k4)
	noLeak(".pub directory", c, b)
	// the flag points at a .pub-looking name whose private sibling is the secret: the private one is not tried
	d5 := shortDir(t)
	k5 := filepath.Join(d5, "id.pub")
	os.WriteFile(k5, []byte("ssh-ed25519 AAAA bad\n"), 0o644)
	os.WriteFile(filepath.Join(d5, "id"), []byte(pem), 0o600)
	c, b = get(k5)
	noLeak("flag names a .pub file", c, b)
	// the response carries exactly the two documented keys on success
	line, _ := sshPubLine(t, "adv@deploy")
	d6 := shortDir(t)
	k6 := filepath.Join(d6, "id")
	os.WriteFile(k6+".pub", []byte(line+"\n"), 0o644)
	os.WriteFile(k6, []byte(pem), 0o000)
	c, b = get(k6)
	var out map[string]string
	_ = json.Unmarshal(b, &out)
	if c != 200 || len(out) != 2 || out["public_key"] != line || strings.Contains(string(b), "PRIVATE-CANARY") {
		t.Fatalf("control: %d %s", c, b)
	}
}

// TestTokensApplyValueRulesBeforeBroker: a value that breaks the D3 rules is
// refused by orchd itself, with the broker's code, even when its salted hash
// matches (a client that signed the hash of a bad value). Nothing reaches the
// broker, so no one-time ID is burned there. The boundaries 32 and 512 pass.
func TestTokensApplyValueRulesBeforeBroker(t *testing.T) {
	e := newTokEnv(t)
	pad := func(n int) string { return strings.Repeat("v", n) }
	bad := map[string]struct{ value, code string }{
		"31 chars":         {pad(31), "token_too_short"},
		"empty-ish 1 char": {"v", "token_too_short"},
		"513 chars":        {pad(513), "token_bad_format"},
		"leading space":    {" " + pad(40), "token_bad_format"},
		"trailing space":   {pad(40) + " ", "token_bad_format"},
		"inner space":      {pad(20) + " " + pad(20), "token_bad_format"},
		"newline":          {pad(20) + "\n" + pad(20), "token_bad_format"},
		"tab":              {pad(20) + "\t" + pad(20), "token_bad_format"},
		"NUL":              {pad(20) + "\x00" + pad(20), "token_bad_format"},
		"DEL":              {pad(20) + "\x7f" + pad(20), "token_bad_format"},
		"non-ascii":        {pad(20) + "é" + pad(20), "token_bad_format"},
		"short with space": {"a b", "token_bad_format"},
	}
	for name, c := range bad {
		f := e.flow(t, "set_token", "gitlab-main", "gitlab", c.value) // the hash matches this value
		code, b := e.apply(t, f)
		if code != 400 || v1Code(b) != c.code {
			t.Errorf("%s: %d %s, want 400 %s", name, code, b, c.code)
		}
		if strings.Contains(string(b), c.value) && len(c.value) > 8 {
			t.Errorf("%s: value echoed", name)
		}
	}
	if ops := e.rb.ops(); len(ops) != 0 {
		t.Fatalf("a rule-breaking value reached the broker: %v", ops)
	}
	for name, v := range map[string]string{"32 chars": pad(32), "512 chars": pad(512)} {
		f := e.flow(t, "set_token", "gitlab-main", "gitlab", v)
		if code, b := e.apply(t, f); code != 200 {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if w := e.rb.tokenWrites(); len(w) != 2 {
		t.Fatalf("%d broker writes for the two boundary values", len(w))
	}
}
