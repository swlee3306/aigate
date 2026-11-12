package api

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/keys"
	"aigate/internal/orch/policy"
)

// W3e (E6) adversarial suite for orchd: per-repo keys, GitHub/link/attach
// endpoints, key-aware clone. "The attacker's move is refused and nothing
// moved": no broker call, no staged key kept, no ledger event, nothing outside
// the key area. Everything runs on the E4 harness (fake broker over a socket,
// fake clone, temp ssh dir); none of it proves anything about real file owners,
// the real sandbox or GitHub.

func w3eLinkFor(name string) map[string]any {
	f := ghLinkFields()
	f["remote_path"] = "Octo-Cat/" + name
	return f
}

func w3eScan(t *testing.T, what string, outputs []string, secret []byte) {
	t.Helper()
	if len(secret) < 100 {
		t.Fatalf("control: %s: no private key content to look for", what)
	}
	needles := []string{"PRIVATE KEY", "OPENSSH PRIVATE"}
	for _, l := range strings.Split(strings.TrimSpace(string(secret)), "\n") {
		if len(l) >= 20 && !strings.HasPrefix(l, "-----") {
			needles = append(needles, l)
		}
	}
	for i, out := range outputs {
		for _, n := range needles {
			if strings.Contains(out, n) {
				t.Errorf("%s: output %d contains private key material %q", what, i, n)
			}
		}
	}
}

// The private key of a per-repo deploy key is in no response (propose, apply
// of every outcome, registry reads, clone), no broker argument (they carry only
// the public line), no ledger line and no log line, and the paths of the key
// area are not in responses either. Outcomes: success, provable refusal, partial
// (both origins), unknown outcome (the broker hangs up), retried apply
// (request_reused), expiry.
func TestPrivateKeyNeverLeavesOrchdAcrossAllOutcomesW3e(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	detail := `{"provider":"gh","project":"Octo-Cat/app-one","project_id":5,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false}`
	outcomes := map[string]func(op string, a json.RawMessage) string{
		"ok":      nil,
		"refused": func(op string, a json.RawMessage) string { return `{"ok":false,"code":"github_refused"}` },
		"partial": func(op string, a json.RawMessage) string {
			return `{"ok":false,"code":"create_partial","detail":` + detail + `}`
		},
		"linkpart": func(op string, a json.RawMessage) string {
			return `{"ok":false,"code":"link_partial","detail":` + detail + `}`
		},
		"unknown": func(op string, a json.RawMessage) string { return "DROP" },
		"reused":  func(op string, a json.RawMessage) string { return `{"ok":false,"code":"request_reused"}` },
	}
	for name, reply := range outcomes {
		for _, action := range []string{"create_project", "link_project", "attach_remote"} {
			e := ghEnv(t)
			e.addProject("att-one", "", "")
			var raw []byte
			var outputs []string
			var p w3cProposal
			var pc int
			var pb []byte
			switch action {
			case "create_project":
				pc, pb = e.proposeRaw(t, action, "app-one", ghCreateFields("private"))
			case "link_project":
				pc, pb = e.proposeRaw(t, action, "app-one", ghLinkFields())
			default:
				pc, pb = e.proposeRaw(t, action, "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"})
			}
			if pc != 200 {
				t.Fatalf("%s/%s propose: %d %s", name, action, pc, pb)
			}
			outputs = append(outputs, string(pb))
			json.Unmarshal(pb, &p)
			priv := e.stagedPriv(t)
			raw, _ = os.ReadFile(priv)
			if fi, err := os.Lstat(priv); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("staged private key mode: %v", err)
			}
			if reply != nil {
				e.wb.setReply(reply)
			}
			c, b := e.apply(t, p)
			outputs = append(outputs, string(b))
			if name == "ok" && c != 200 {
				t.Fatalf("%s/%s apply: %d %s", name, action, c, b)
			}
			c2, b2 := e.apply(t, p) // the same signed change again (a retry)
			_ = c2
			outputs = append(outputs, string(b2))
			_, rb := e.sockJSON(t, "GET", "/api/v1/projects/registry", "")
			outputs = append(outputs, string(rb))
			_, cb := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
			outputs = append(outputs, string(cb))
			for _, call := range e.wb.opsNamed("create_project", "link_project", "attach_remote", "projects") {
				outputs = append(outputs, string(call.Args))
			}
			led, _ := os.ReadFile(policy.OrchLedgerPath(e.home))
			outputs = append(outputs, string(led), logs.String())
			w3eScan(t, name+"/"+action, outputs, raw)
			for i, out := range outputs[:5] {
				if strings.Contains(out, e.keysDir()) || strings.Contains(out, e.stagingDir()) || strings.Contains(out, "id_ed25519") {
					t.Errorf("%s/%s: response %d names the key area", name, action, i)
				}
			}
			// the broker only ever received the public line (and its hash)
			for _, call := range e.wb.opsNamed("create_project", "link_project", "attach_remote") {
				var a map[string]any
				json.Unmarshal(call.Args, &a)
				if dk, _ := a["deploy_key"].(string); !strings.HasPrefix(dk, "ssh-ed25519 ") || strings.Contains(dk, "\n") || sha256Hex(dk) != a["deploy_key_sha256"] {
					t.Errorf("%s/%s: the broker got %q", name, action, dk)
				}
			}
		}
	}
	// expiry: the expired key is deleted, never answered
	e := ghEnv(t)
	now := time.Now()
	e.d.handler.keys.Now = func() time.Time { return now }
	p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	now = now.Add(keys.StagingTTL + time.Minute)
	if c, b := e.apply(t, p); c != 409 || v1Code(b) != "key_expired" || strings.Contains(string(b), "PRIVATE") {
		t.Fatalf("%d %s", c, b)
	}
}

// A staged key is bound to its own proposal's signed hash and used once: every
// proposal offers a new key (also after partial and unknown outcomes), a hash
// of a key that is already live under keys/ (another project's!) or of the
// shared key never stages or forwards anything, a changed file or an expired
// key stops before the broker, and whatever reaches the broker is exactly the
// public line whose hash was signed.
func TestStagedKeyBoundToItsHashNeverReusedNeverALiveKeyW3e(t *testing.T) {
	e := ghEnv(t)
	e.addProject("att-one", "", "")
	seenHash, seenKey := map[string]bool{}, map[string]bool{}
	offer := func(p w3cProposal) {
		h := p.Fields["deploy_key_sha256"].(string)
		if seenHash[h] || seenKey[p.DeployKey.PublicKey] {
			t.Fatalf("a key was offered twice: %s", h)
		}
		seenHash[h], seenKey[p.DeployKey.PublicKey] = true, true
	}
	for i := 0; i < 6; i++ {
		offer(e.propose(t, "create_project", "app-one", ghCreateFields("private")))
		offer(e.propose(t, "link_project", "app-two", w3eLinkFor("app-two")))
		offer(e.propose(t, "attach_remote", "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"}))
	}
	// a new proposal after an unknown outcome gets a NEW key although the old one is retained
	e2 := ghEnv(t)
	first := e2.propose(t, "create_project", "app-one", ghCreateFields("private"))
	e2.wb.setReply(dropAll)
	e2.apply(t, first)
	e2.wb.setReply(nil)
	second := e2.propose(t, "create_project", "app-one", ghCreateFields("private"))
	if second.Fields["deploy_key_sha256"] == first.Fields["deploy_key_sha256"] || second.DeployKey.PublicKey == first.DeployKey.PublicKey {
		t.Fatal("a retained key was offered again")
	}
	// a live key's hash on a new proposal's apply: refused before the broker, the live key untouched
	e3 := ghEnv(t)
	live := e3.propose(t, "create_project", "app-one", ghCreateFields("private"))
	if c, b := e3.apply(t, live); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	liveBefore, _ := os.ReadFile(e3.promoted("app-one"))
	steal := e3.propose(t, "link_project", "app-two", w3eLinkFor("app-two"))
	steal.Fields["deploy_key_sha256"] = live.Fields["deploy_key_sha256"]
	ch, _ := changeOf(steal)
	steal.Change = ch
	calls := len(e3.wb.writeCalls())
	if c, b := e3.apply(t, steal); c != 409 || v1Code(b) != "key_expired" {
		t.Fatalf("a live key's hash was accepted: %d %s", c, b)
	}
	if len(e3.wb.writeCalls()) != calls {
		t.Fatal("the broker was called for a stolen key hash")
	}
	if got, _ := os.ReadFile(e3.promoted("app-one")); !bytes.Equal(got, liveBefore) {
		t.Fatal("the live key changed")
	}
	if _, err := os.Lstat(e3.promoted("app-two")); err == nil {
		t.Fatal("a key appeared for the second project")
	}
	// another staged proposal's hash on this apply (consistent change): whatever is forwarded is exactly that hash's key
	a := e3.propose(t, "create_project", "app-three", ghCreateFields("private"))
	b := e3.propose(t, "create_project", "app-four", ghCreateFields("private"))
	a.Fields["deploy_key_sha256"] = b.Fields["deploy_key_sha256"]
	ch, _ = changeOf(a)
	a.Change = ch
	e3.apply(t, a)
	for _, call := range e3.wb.opsNamed("create_project", "link_project", "attach_remote") {
		var m map[string]any
		json.Unmarshal(call.Args, &m)
		if dk, _ := m["deploy_key"].(string); sha256Hex(dk) != m["deploy_key_sha256"] {
			t.Fatalf("the broker received a key that is not the signed hash's: %v", m)
		}
	}
	// the file of a staged key swapped for another valid key, or changed after the hash: refused before the broker
	e4 := ghEnv(t)
	p := e4.propose(t, "create_project", "app-one", ghCreateFields("private"))
	other, _ := sshPubLine(t, "orchd-deploy")
	ents, _ := os.ReadDir(e4.stagingDir())
	writeFile(t, filepath.Join(e4.stagingDir(), ents[0].Name(), "id_ed25519.pub"), other+"\n")
	if c, b := e4.apply(t, p); c != 409 || v1Code(b) != "deploy_key_mismatch" {
		t.Fatalf("%d %s", c, b)
	}
	if len(e4.wb.allOps()) != 0 {
		t.Fatalf("broker called: %v", e4.wb.allOps())
	}
	// a provable refusal removes the key: the same signed change is key_expired afterwards and the broker is not asked again
	e5 := ghEnv(t)
	p5 := e5.propose(t, "create_project", "app-one", ghCreateFields("private"))
	e5.wb.setReply(func(op string, a json.RawMessage) string { return `{"ok":false,"code":"github_refused"}` })
	e5.apply(t, p5)
	n := len(e5.wb.allOps())
	if c, b2 := e5.apply(t, p5); c != 409 || v1Code(b2) != "key_expired" || len(e5.wb.allOps()) != n {
		t.Fatalf("retry after a refusal: %d %s", c, b2)
	}
	if e5.stagedCount() != 0 {
		t.Fatal("a refused key was kept")
	}
}

// Symlink and permission games around the key area at the endpoint level: a
// keys/ that is a symlink, a key directory that is one, a private key that is a
// symlink to the shared key, group-readable, or a directory: clone is refused
// with key_unavailable and runs nothing; a GitHub create under a pre-made key
// directory or symlink is refused at propose or apply; nothing is ever written
// behind a symlink.
func TestKeyAreaSymlinksAndModesRefusedAtEndpointsW3e(t *testing.T) {
	foreign := t.TempDir()
	e := ghEnv(t)
	e.addProject("hello", "gh", "Octo-Cat/Hello")
	hostile := map[string]func(){
		"keys dir is a symlink": func() { os.RemoveAll(e.keysDir()); os.Symlink(foreign, e.keysDir()) },
		"key dir is a symlink": func() {
			os.RemoveAll(e.keysDir())
			os.MkdirAll(e.keysDir(), 0o700)
			os.WriteFile(filepath.Join(foreign, "id_ed25519"), []byte("k"), 0o600)
			os.Symlink(foreign, filepath.Join(e.keysDir(), "hello"))
		},
		"key is a symlink to the shared key": func() {
			os.RemoveAll(e.keysDir())
			os.MkdirAll(filepath.Join(e.keysDir(), "hello"), 0o700)
			os.WriteFile(e.key, []byte("shared"), 0o600)
			os.Symlink(e.key, e.promoted("hello"))
		},
		"key readable by the group": func() {
			os.RemoveAll(e.keysDir())
			os.MkdirAll(filepath.Join(e.keysDir(), "hello"), 0o700)
			os.WriteFile(e.promoted("hello"), []byte("k"), 0o640)
			os.Chmod(e.promoted("hello"), 0o640)
		},
		"key dir open to others": func() {
			os.RemoveAll(e.keysDir())
			os.MkdirAll(filepath.Join(e.keysDir(), "hello"), 0o755)
			os.Chmod(filepath.Join(e.keysDir(), "hello"), 0o755)
			os.WriteFile(e.promoted("hello"), []byte("k"), 0o600)
		},
		"key is a directory": func() { os.RemoveAll(e.keysDir()); os.MkdirAll(e.promoted("hello"), 0o700) },
		"key is empty": func() {
			os.RemoveAll(e.keysDir())
			os.MkdirAll(filepath.Join(e.keysDir(), "hello"), 0o700)
			os.WriteFile(e.promoted("hello"), nil, 0o600)
		},
	}
	for name, mod := range hostile {
		mod()
		if c, b := e.sockJSON(t, "POST", "/api/v1/projects/hello/clone", ""); c != 503 || v1Code(b) != "key_unavailable" {
			t.Errorf("%s: %d %s", name, c, b)
		}
		if n := len(e.cloneCalls()); n != 0 {
			t.Errorf("%s: clone ran %d times", name, n)
		}
	}
	if ents, _ := os.ReadDir(foreign); len(ents) > 1 {
		t.Fatalf("something was written behind a symlink: %v", ents)
	}
	// a GitHub create over a pre-made key directory or symlink: refused, nothing moved, no broker call
	for name, mk := range map[string]func(string){
		"directory": func(p string) { os.MkdirAll(p, 0o700) },
		"symlink":   func(p string) { os.MkdirAll(filepath.Dir(p), 0o700); os.Symlink(foreign, p) },
	} {
		e2 := ghEnv(t)
		mk(filepath.Join(e2.keysDir(), "app-one"))
		c, b := e2.proposeRaw(t, "create_project", "app-one", ghCreateFields("private"))
		if c == 200 {
			// (a symlinked keys/<id> may still stage: the apply must then refuse)
			var p w3cProposal
			json.Unmarshal(b, &p)
			c, b = e2.apply(t, p)
		}
		if c == 200 {
			t.Errorf("%s: a create over an existing key location succeeded", name)
		}
		if len(e2.wb.writeCalls()) != 0 {
			t.Errorf("%s: a write reached the broker", name)
		}
		if e2.stagedCount() != 0 && name == "directory" {
			t.Errorf("%s: a key stayed staged", name)
		}
	}
	if ents, _ := os.ReadDir(foreign); len(ents) > 1 {
		t.Fatalf("something was written behind a symlink: %v", ents)
	}
}

// Forged apply bodies for every new action (and GitHub create) are refused
// before the broker is called and before any key is promoted: extra top-level
// keys, extra or missing fields, wrong-case subjects, a client-supplied key
// hash, visibility on a GitLab provider's proposal, malformed issue time and
// request ID, a different action.
func TestForgedFieldsRefusedBeforeBrokerForNewActionsW3e(t *testing.T) {
	type act struct {
		action, subject string
		fields          map[string]any
		prep            func(e *w3cEnv)
	}
	acts := []act{
		{"create_project", "app-one", ghCreateFields("private"), nil},
		{"link_project", "app-one", ghLinkFields(), nil},
		{"attach_remote", "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"}, func(e *w3cEnv) { e.addProject("att-one", "", "") }},
	}
	for _, a := range acts {
		forge := map[string]func(m map[string]any){
			"extra top-level key":     func(m map[string]any) { m["deploy_key"] = "ssh-ed25519 AAAA evil" },
			"extra field":             func(m map[string]any) { m["fields"].(map[string]any)["extra"] = "x" },
			"missing field":           func(m map[string]any) { delete(m["fields"].(map[string]any), "publish") },
			"upper-case subject":      func(m map[string]any) { m["subject"] = strings.ToUpper(a.subject) },
			"path-like subject":       func(m map[string]any) { m["subject"] = "../" + a.subject },
			"owner/remote swapped":    func(m map[string]any) { f := m["fields"].(map[string]any); f["provider"] = "gl" },
			"bad issue time":          func(m map[string]any) { m["issued"] = "yesterday" },
			"bad request id":          func(m map[string]any) { m["request"] = "short" },
			"different action":        func(m map[string]any) { m["action"] = "set_provider" },
			"signature missing":       func(m map[string]any) { delete(m, "signature") },
			"empty subject":           func(m map[string]any) { m["subject"] = "" },
			"huge description":        func(m map[string]any) { m["fields"].(map[string]any)["description"] = strings.Repeat("x", 100000) },
			"client key hash":         func(m map[string]any) { m["fields"].(map[string]any)["deploy_key_sha256"] = strings.Repeat("a", 64) },
			"visibility not a string": func(m map[string]any) { m["fields"].(map[string]any)["visibility"] = true },
		}
		for name, f := range forge {
			e := ghEnv(t)
			if a.prep != nil {
				a.prep(e)
			}
			p := e.propose(t, a.action, a.subject, a.fields)
			body := e.applyBody(p, webauthnSig)
			f(body)
			c, b := e.sockJSON(t, "POST", e.applyPath(a.action), jsonBody(body))
			if c == 200 {
				t.Errorf("%s/%s: forged apply succeeded: %s", a.action, name, b)
			}
			// field edits that the proposal never offered must not reach the broker; only
			// the unchanged-fields variants may (and then only with the staged key)
			for _, call := range e.wb.writeCalls() {
				var m map[string]any
				json.Unmarshal(call.Args, &m)
				if dk, _ := m["deploy_key"].(string); dk != p.DeployKey.PublicKey {
					t.Errorf("%s/%s: the broker got a key that was not staged: %q", a.action, name, dk)
				}
			}
			if len(e.wb.writeCalls()) != 0 && name != "extra top-level key" && name != "signature missing" {
				t.Errorf("%s/%s: reached the broker", a.action, name)
			}
			if _, err := os.Lstat(filepath.Join(e.keysDir(), a.subject)); err == nil {
				t.Errorf("%s/%s: a key was promoted for a forged apply", a.action, name)
			}
		}
	}
	// a client-supplied hash or key on a PROPOSAL
	e := ghEnv(t)
	for _, extra := range []string{"deploy_key_sha256", "deploy_key", "public_key", "private_key", "key_path"} {
		f := ghCreateFields("private")
		f[extra] = strings.Repeat("a", 64)
		if c, b := e.proposeRaw(t, "create_project", "app-one", f); c != 400 {
			t.Errorf("proposal with %s: %d %s", extra, c, b)
		}
	}
	if e.stagedCount() != 0 || len(e.wb.allOps()) != 0 {
		t.Fatal("a refused proposal staged a key or called the broker")
	}
}

// Hostile text from the broker (error sentences, messages, details that
// contain the token, key lines, paths) never reaches a response, the ledger or
// the log of orchd: only fixed sentences and the values orchd itself
// validated. The ledger events of the new actions carry exactly
// actor,action,subject,ok,code and nothing from the key area or the provider.
func TestBrokerHostileSentencesNeverReachResponsesLedgerOrLogW3e(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	const canary = "ghp_canary-NOT-A-REAL-TOKEN-W3e"
	hostile := []string{
		`{"ok":false,"code":"github_refused","message":"` + canary + ` PRIVATE KEY /home/orch/orchd/ssh/keys/x"}`,
		`{"ok":false,"code":"link_partial","message":"` + canary + `","detail":{"provider":"gh","project":"` + canary + `","project_id":1,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false,"origin":"linked"}}`,
		`{"ok":false,"code":"create_partial","message":"` + canary + `","detail":{"provider":"gh","project":"Octo-Cat/app-one","project_id":1,"step":"` + canary + `","orphan_recorded":true,"orphan_dropped":false}}`,
		`{"ok":false,"code":"` + canary + `","message":"` + canary + `"}`,
		`{"ok":true,"result":{"project":{"id":"app-one","path":"/repos/app-one","kind":"registered","ai_context":false,"provider":"gh","remote":{"path":"` + canary + `","id":1},"auto_merge":{"enabled":false}}}}`,
		`not json ` + canary,
	}
	for i, h := range hostile {
		for _, action := range []string{"create_project", "link_project", "attach_remote"} {
			e := ghEnv(t)
			e.addProject("att-one", "", "")
			var p w3cProposal
			switch action {
			case "create_project":
				p = e.propose(t, action, "app-one", ghCreateFields("public"))
			case "link_project":
				p = e.propose(t, action, "app-one", ghLinkFields())
			default:
				p = e.propose(t, action, "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"})
			}
			e.wb.setReply(func(op string, a json.RawMessage) string {
				if op == action {
					return h
				}
				return ""
			})
			c, b := e.apply(t, p)
			if strings.Contains(string(b), canary) || strings.Contains(string(b), "PRIVATE KEY") || strings.Contains(string(b), "/home/orch") {
				t.Errorf("hostile %d/%s: response leaks (%d): %s", i, action, c, b)
			}
			led, _ := os.ReadFile(policy.OrchLedgerPath(e.home))
			if strings.Contains(string(led), canary) || strings.Contains(string(led), "Octo-Cat") || strings.Contains(string(led), "ssh-ed25519") || strings.Contains(string(led), "id_ed25519") {
				t.Errorf("hostile %d/%s: ledger leaks: %s", i, action, led)
			}
			for _, typ := range []string{"project_create", "project_link", "project_attach"} {
				for _, pl := range e.ledgerPayloads(t, typ) {
					if ledgerKeys(pl) != "action,actor,code,ok,subject" {
						t.Errorf("hostile %d/%s: %s event keys %s", i, action, typ, ledgerKeys(pl))
					}
				}
			}
			if strings.Contains(logs.String(), canary) {
				t.Errorf("hostile %d/%s: the log leaks the broker's text", i, action)
			}
		}
	}
}

// Refusals that must come before any signature, key or write, for the new
// actions: static_remote_wins (also appearing between propose and apply),
// already_linked (legacy mapping included), project
// exists, folder exists, policy class missing, github.com host not pinned for a
// GitHub link, no broker. No staged key remains, no write reaches the broker,
// no signing message is handed out, no ledger event is written.
func TestRefusalsBeforeSignatureLeaveNoKeyNoWriteNoEventW3e(t *testing.T) {
	e := ghEnv(t)
	e.d.handler.staticRepos = map[string]bool{filepath.Join(e.root, "sample-app"): true, filepath.Join(e.root, "app-static"): true}
	e.addProject("sample-app", "", "")
	e.addProject("linked-one", "gl", "team/linked-one")
	e.addProject("legacy-one", "", "team/legacy-one")
	e.addProject("plain-one", "", "")
	att := func(sub, prov, path string) (int, []byte) {
		return e.proposeRaw(t, "attach_remote", sub, map[string]any{"provider": prov, "remote_path": path, "publish": "write"})
	}
	lnk := func(sub, prov, path string) (int, []byte) {
		return e.proposeRaw(t, "link_project", sub, map[string]any{"provider": prov, "remote_path": path, "ai_context": false, "publish": "write"})
	}
	type tc struct {
		name   string
		c      int
		b      []byte
		status int
		code   string
	}
	var cases []tc
	add := func(name string, f func() (int, []byte), status int, code string) {
		c, b := f()
		cases = append(cases, tc{name, c, b, status, code})
	}
	add("attach to a static-remote project", func() (int, []byte) { return att("sample-app", "gh", "Octo-Cat/sample-app") }, 409, "static_remote_wins")
	add("link of a static-remote folder", func() (int, []byte) { return lnk("app-static", "gh", "Octo-Cat/app-static") }, 409, "static_remote_wins")
	add("attach to a provider-linked project", func() (int, []byte) { return att("linked-one", "gh", "Octo-Cat/linked-one") }, 409, "already_linked")
	add("attach to a legacy-mapped project", func() (int, []byte) { return att("legacy-one", "gh", "Octo-Cat/legacy-one") }, 409, "already_linked")
	add("link over an existing project id", func() (int, []byte) { return lnk("plain-one", "gh", "Octo-Cat/plain-one") }, 409, "project_exists")
	add("link, id already linked", func() (int, []byte) { return lnk("linked-one", "gh", "Octo-Cat/linked-one") }, 409, "already_linked")
	add("unknown provider", func() (int, []byte) { return lnk("app-one", "nope", "Octo-Cat/app-one") }, 404, "provider_unknown")
	add("attach to an unknown project", func() (int, []byte) { return att("ghost", "gh", "Octo-Cat/ghost") }, 404, "project_unknown")
	for _, c := range cases {
		if c.c != c.status || v1Code(c.b) != c.code {
			t.Errorf("%s: %d %s", c.name, c.c, c.b)
		}
		if strings.Contains(string(c.b), `"change"`) || strings.Contains(string(c.b), `"request"`) || strings.Contains(string(c.b), "aigate-registry") {
			t.Errorf("%s: a signing message was handed out", c.name)
		}
	}
	if e.stagedCount() != 0 {
		t.Fatalf("%d staged keys after refused proposals", e.stagedCount())
	}
	if len(e.wb.writeCalls()) != 0 {
		t.Fatalf("a write reached the broker: %v", e.wb.allOps())
	}
	for _, typ := range []string{"project_link", "project_attach", "project_create"} {
		if ev := e.ledgerPayloads(t, typ); len(ev) != 0 {
			t.Fatalf("%s event for a refused proposal: %v", typ, ev)
		}
	}
	// the static entry appears between propose and apply: refused at apply, nothing forwarded, the key goes away or stays unpromoted
	p := e.propose(t, "attach_remote", "plain-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/plain-one", "publish": "write"})
	e.d.handler.staticRepos[filepath.Join(e.root, "plain-one")] = true
	if c, b := e.apply(t, p); c != 409 || v1Code(b) != "static_remote_wins" {
		t.Fatalf("%d %s", c, b)
	}
	if len(e.wb.writeCalls()) != 0 {
		t.Fatal("a write after static_remote_wins")
	}
	if _, err := os.Lstat(e.promoted("plain-one")); err == nil {
		t.Fatal("a key was promoted for a refused attach")
	}
}

// ---------- final review M2 ----------

// A reconcile pass reads the broker's registry ONCE however many keys are
// retained (the pass used to cost one read, up to 20 s, per key).
func TestReconcileReadsTheRegistryOncePerPassW3e(t *testing.T) {
	e := ghEnv(t)
	for _, name := range []string{"app-one", "app-two", "app-three"} {
		p := e.propose(t, "create_project", name, ghCreateFields("private"))
		e.wb.setReply(dropAll) // unknown outcome: the key is retained
		e.apply(t, p)
		e.wb.setReply(nil)
	}
	if n := len(e.d.handler.keys.RetainedKeys()); n != 3 {
		t.Fatalf("retained %d, want 3", n)
	}
	e.wb.mu.Lock()
	e.wb.calls = nil
	e.wb.mu.Unlock()
	e.d.handler.reconcileKeys()
	if n := len(e.wb.opsNamed("projects")); n != 1 {
		t.Fatalf("a reconcile pass read the registry %d times, want 1", n)
	}
	// nothing retained: no read at all
	e2 := ghEnv(t)
	e2.d.handler.reconcileKeys()
	if len(e2.wb.allOps()) != 0 {
		t.Fatal("a pass without retained keys called the broker")
	}
}

// The orchd copy of the broker's deploy-key line rule behaves exactly like the
// broker's: same vector file as TestDeployKeyLineRuleVectorsW3e.
func TestBrokerDeployKeyRuleCopyMatchesVectorsW3e(t *testing.T) {
	raw, err := os.ReadFile("../../broker/testdata/deploykey_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Line string `json:"line"`
		OK   bool   `json:"ok"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) < 20 {
		t.Fatalf("vectors: %v %d", err, len(vs))
	}
	for _, v := range vs {
		if got := brokerDeployKeyRE.MatchString(v.Line); got != v.OK {
			t.Errorf("orchd brokerDeployKeyRE(%q) = %v, want %v", v.Line, got, v.OK)
		}
	}
}
