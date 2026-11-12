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

	"aigate/internal/orch/broker"
	"aigate/internal/orch/clone"
	"aigate/internal/orch/keys"
	"aigate/internal/orch/policy"
)

// ---- harness ----

const ghKnownLine = "github.com " + w3cKnownKey

// ghEnv is a W3c environment whose registry also holds a github provider "gh".
func ghEnv(t *testing.T, opts ...func(*Config)) *w3cEnv {
	t.Helper()
	e := newW3cEnv(t, opts...)
	e.wb.addGitHub()
	writeFile(t, e.known, "gitlab.example "+w3cKnownKey+"\n"+ghKnownLine+"\n")
	return e
}

func (e *w3cEnv) sshDir() string            { return filepath.Dir(e.key) }
func (e *w3cEnv) stagingDir() string        { return filepath.Join(e.sshDir(), "keys-staging") }
func (e *w3cEnv) keysDir() string           { return filepath.Join(e.sshDir(), "keys") }
func (e *w3cEnv) stagedCount() int          { ents, _ := os.ReadDir(e.stagingDir()); return len(ents) }
func (e *w3cEnv) promoted(id string) string { return filepath.Join(e.keysDir(), id, "id_ed25519") }

func ghCreateFields(vis string) map[string]any {
	return map[string]any{"provider": "gh", "group_path": "Octo-Cat", "template": "go", "description": "d", "ai_context": false, "publish": "write", "visibility": vis}
}

func ghLinkFields() map[string]any {
	return map[string]any{"provider": "gh", "remote_path": "Octo-Cat/app-one", "ai_context": false, "publish": "write"}
}

// proposeRaw posts a proposal and returns the status and body.
func (e *w3cEnv) proposeRaw(t *testing.T, action, subject string, fields map[string]any) (int, []byte) {
	t.Helper()
	return e.sockJSON(t, "POST", e.proposePath(action), jsonBody(map[string]any{"action": action, "subject": subject, "fields": fields}))
}

// stagedPriv returns the private key file of the only staged key.
func (e *w3cEnv) stagedPriv(t *testing.T) string {
	t.Helper()
	ents, err := os.ReadDir(e.stagingDir())
	if err != nil || len(ents) != 1 {
		t.Fatalf("staged keys: %v %v", ents, err)
	}
	return filepath.Join(e.stagingDir(), ents[0].Name(), "id_ed25519")
}

func (e *w3cEnv) addProject(id, prov string, remote string) {
	p := map[string]any{"id": id, "path": filepath.Join(e.root, id), "kind": "registered", "ai_context": false, "provider": nil, "remote": nil, "auto_merge": map[string]any{"enabled": false}}
	if prov != "" {
		p["provider"] = prov
	}
	if remote != "" {
		p["remote"] = map[string]any{"path": remote, "id": 5}
	}
	e.wb.mu.Lock()
	e.wb.projects = append(e.wb.projects, p)
	e.wb.mu.Unlock()
}

func lastCall(t *testing.T, e *w3cEnv, op string) map[string]any {
	t.Helper()
	calls := e.wb.opsNamed(op)
	if len(calls) == 0 {
		t.Fatalf("no %s call", op)
	}
	var m map[string]any
	if err := json.Unmarshal(calls[len(calls)-1].Args, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ---- keys at propose ----

func TestProposeCreatesNoGitHubSideEffect(t *testing.T) {
	e := ghEnv(t)
	p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	// nothing reached the broker (so neither the registry nor GitHub), nothing is in keys/, no ledger event
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("propose called the broker: %v", ops)
	}
	if ents, _ := os.ReadDir(e.keysDir()); len(ents) != 0 {
		t.Fatalf("a key was promoted by a proposal: %v", ents)
	}
	if e.stagedCount() != 1 {
		t.Fatalf("staged keys: %d", e.stagedCount())
	}
	for _, typ := range []string{"project_create", "provider_change", "project_link"} {
		if ev := e.ledgerPayloads(t, typ); len(ev) != 0 {
			t.Fatalf("%s recorded by a proposal: %v", typ, ev)
		}
	}
	if p.DeployKey == nil || sha256Hex(p.DeployKey.PublicKey) != p.Fields["deploy_key_sha256"] {
		t.Fatalf("hash is not that of the staged public key: %+v %v", p.DeployKey, p.Fields["deploy_key_sha256"])
	}
	if p.Fields["visibility"] != "private" || !strings.Contains(strings.Join(p.Preview, "\n"), "비공개 저장소") {
		t.Fatalf("%v %v", p.Fields, p.Preview)
	}
	// a link/attach proposal reads the registry (a read) and writes nothing
	e.wb.mu.Lock()
	e.wb.calls = nil
	e.wb.mu.Unlock()
	e.addProject("att-one", "", "")
	e.propose(t, "link_project", "app-one", ghLinkFields())
	af := ghLinkFields()
	af["remote_path"] = "Octo-Cat/att-one"
	delete(af, "ai_context")
	e.propose(t, "attach_remote", "att-one", af)
	for _, op := range e.wb.allOps() {
		if op != "projects" {
			t.Fatalf("link/attach propose called %s", op)
		}
	}
	if len(e.wb.writeCalls()) != 0 {
		t.Fatal("a write reached the broker")
	}
	if e.stagedCount() != 3 {
		t.Fatalf("staged keys: %d", e.stagedCount())
	}
}

func TestCreateProposeIncludesPerRepoKeyHash(t *testing.T) {
	e := ghEnv(t)
	p1 := e.propose(t, "create_project", "app-one", ghCreateFields("public"))
	p2 := e.propose(t, "create_project", "app-one", ghCreateFields("public"))
	if p1.Fields["deploy_key_sha256"] == p2.Fields["deploy_key_sha256"] || p1.DeployKey.PublicKey == p2.DeployKey.PublicKey {
		t.Fatal("a key was offered twice (a retry must never reuse a key that may be attached to a repository)")
	}
	if p1.Fields["deploy_key_sha256"] == sha256Hex(e.pubLine) {
		t.Fatal("a GitHub proposal carries the shared key's hash")
	}
	// public vs private is said plainly, and a public repository is flagged as important
	if pv := strings.Join(p1.Preview, "\n"); !strings.Contains(pv, "[중요] 공개 저장소로 만듭니다") || !strings.Contains(pv, "누구나") {
		t.Fatalf("public preview: %v", p1.Preview)
	}
	if pp := e.propose(t, "create_project", "app-pr", ghCreateFields("private")); strings.Contains(strings.Join(pp.Preview, "\n"), "[중요] 공개 저장소") || !strings.Contains(strings.Join(pp.Preview, "\n"), "비공개 저장소로 만듭니다") {
		t.Fatalf("private preview: %v", pp.Preview)
	}
	if !brokerDeployKeyRE.MatchString(p1.DeployKey.PublicKey) {
		t.Fatalf("the staged line would be refused by the broker: %q", p1.DeployKey.PublicKey)
	}
	if !strings.HasPrefix(p1.DeployKey.PublicKey, "ssh-ed25519 ") || !strings.HasPrefix(p1.DeployKey.Fingerprint, "SHA256:") {
		t.Fatalf("%+v", p1.DeployKey)
	}
	if e.stagedCount() != 3 {
		t.Fatalf("staged keys: %d", e.stagedCount())
	}
	// GitLab: the shared key's hash, exactly as before, and no staged key
	e2 := newW3cEnv(t)
	g := e2.proposeAction(t, "create_project")
	if g.Fields["deploy_key_sha256"] != sha256Hex(e2.pubLine) || g.DeployKey == nil || g.DeployKey.PublicKey != e2.pubLine {
		t.Fatalf("gitlab hash changed: %v", g.Fields)
	}
	if e2.stagedCount() != 0 {
		t.Fatal("a GitLab proposal staged a key")
	}
	if _, has := g.Fields["visibility"]; has {
		t.Fatal("visibility on a GitLab proposal")
	}
	// the client may not choose the hash
	f := ghCreateFields("private")
	f["deploy_key_sha256"] = strings.Repeat("a", 64)
	if c, b := e.proposeRaw(t, "create_project", "app-one", f); c != 400 {
		t.Fatalf("client-supplied hash: %d %s", c, b)
	}
}

func TestGitHubOwnerAndVisibilityRulesPerKind(t *testing.T) {
	e := ghEnv(t)
	n := 0
	for owner, want := range map[string]int{
		"SampleUser": 200, "octo_cat": 200, "9lives": 200, "a": 200, strings.Repeat("a", 39): 200,
		"a/b": 400, "-x": 400, "x-": 400, strings.Repeat("a", 40): 400, "a b": 400, "../x": 400, "a%2Fb": 400, "": 400,
	} {
		n++
		f := ghCreateFields("private")
		f["group_path"] = owner
		c, b := e.proposeRaw(t, "create_project", "app-"+string(rune('a'+n)), f)
		if c != want {
			t.Errorf("github owner %q: %d %s", owner, c, b)
		}
	}
	f := ghCreateFields("internal")
	if c, b := e.proposeRaw(t, "create_project", "app-one", f); c != 400 {
		t.Errorf("visibility internal: %d %s", c, b)
	}
	// GitLab (no visibility) keeps the W3c group-path rule
	for owner, want := range map[string]int{"team": 200, "team/sub": 200, "Team": 400, "octo_cat": 400, "a": 400, "team/": 400} {
		_, gf := e.fieldsFor("create_project")
		gf["group_path"] = owner
		c, b := e.proposeRaw(t, "create_project", "app-gl", gf)
		if c != want {
			t.Errorf("gitlab owner %q: %d %s", owner, c, b)
		}
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("broker called: %v", ops)
	}
}

func TestKeyExpiredAtApply(t *testing.T) {
	e := ghEnv(t)
	now := time.Now()
	e.d.handler.keys.Now = func() time.Time { return now }
	p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	now = now.Add(keys.StagingTTL - time.Minute)
	// just before expiry the same proposal is still applicable (and a refused-by-limit
	// apply would leave it) - checked below; here let it expire
	now = now.Add(2 * time.Minute)
	c, b := e.apply(t, p)
	if c != 409 || v1Code(b) != "key_expired" {
		t.Fatalf("%d %s", c, b)
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("broker called: %v", ops)
	}
	if e.stagedCount() != 0 {
		t.Fatal("the expired key was not deleted")
	}
	// a new proposal after expiry works; and the lazy sweep removed the old key meanwhile
	now = now.Add(time.Minute)
	p2 := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	if c, b := e.apply(t, p2); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	// after a restart (a new store) nothing staged can be applied
	p3 := e.propose(t, "create_project", "app-two", ghCreateFields("private"))
	e.d.handler.keys = &keys.Store{Root: e.sshDir()}
	e.d.handler.keys.SweepAll()
	if c, b := e.apply(t, p3); c != 409 || v1Code(b) != "key_expired" {
		t.Fatalf("after restart: %d %s", c, b)
	}
}

func TestStagedKeyChangedOrUnknownIsRefusedBeforeTheBroker(t *testing.T) {
	e := ghEnv(t)
	p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	// the staged public file is replaced by another valid key
	other, _ := sshPubLine(t, "orchd-deploy")
	ents, _ := os.ReadDir(e.stagingDir())
	pub := filepath.Join(e.stagingDir(), ents[0].Name(), "id_ed25519.pub")
	writeFile(t, pub, other+"\n")
	if c, b := e.apply(t, p); c != 409 || v1Code(b) != "deploy_key_mismatch" {
		t.Fatalf("swapped public key: %d %s", c, b)
	}
	// a hash nobody staged
	q := e.propose(t, "create_project", "app-two", ghCreateFields("private"))
	body := e.applyBody(q, webauthnSig)
	body["fields"].(map[string]any)["deploy_key_sha256"] = strings.Repeat("b", 64)
	// the change no longer matches the signature's change hash: the recomputed one differs
	if c, b := e.sockJSON(t, "POST", e.applyPath("create_project"), jsonBody(body)); c != 400 || v1Code(b) != "change_mismatch" {
		t.Fatalf("%d %s", c, b)
	}
	// the shared key's hash sent for a GitHub create is not a staged key
	s := e.propose(t, "create_project", "app-three", ghCreateFields("private"))
	s.Fields["deploy_key_sha256"] = sha256Hex(e.pubLine)
	ch, _ := changeOf(s)
	s.Change = ch
	if c, b := e.apply(t, s); c != 409 || v1Code(b) != "key_expired" {
		t.Fatalf("shared key hash on a GitHub create: %d %s", c, b)
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("broker called: %v", ops)
	}
}

func changeOf(p w3cProposal) (string, error) {
	return registryChange{Action: p.Action, Subject: p.Subject, Fields: p.Fields}.hash()
}

func TestExistingKeyDirBlocksAGitHubCreate(t *testing.T) {
	e := ghEnv(t)
	if err := os.MkdirAll(filepath.Join(e.keysDir(), "app-one"), 0o700); err != nil {
		t.Fatal(err)
	}
	if c, b := e.proposeRaw(t, "create_project", "app-one", ghCreateFields("private")); c != 503 || v1Code(b) != "key_staging_failed" {
		t.Fatalf("%d %s", c, b)
	}
	if e.stagedCount() != 0 {
		t.Fatal("a key was staged")
	}
	// the folder appearing between propose and apply
	p := e.propose(t, "create_project", "app-two", ghCreateFields("private"))
	os.MkdirAll(filepath.Join(e.keysDir(), "app-two"), 0o700)
	if c, b := e.apply(t, p); c != 503 || v1Code(b) != "key_staging_failed" {
		t.Fatalf("%d %s", c, b)
	}
	if len(e.wb.allOps()) != 0 {
		t.Fatal("broker called")
	}
}

// ---- the whole GitHub flow ----

func TestGitHubCreateApplyForwardsStagedKeyAndMovesIt(t *testing.T) {
	e := ghEnv(t)
	p := e.propose(t, "create_project", "app-one", ghCreateFields("public"))
	priv := e.stagedPriv(t)
	before, err := os.Lstat(priv)
	if err != nil {
		t.Fatal(err)
	}
	c, b := e.apply(t, p)
	if c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	args := lastCall(t, e, "create_project")
	if args["deploy_key"] != p.DeployKey.PublicKey || args["visibility"] != "public" || args["group_path"] != "Octo-Cat" || args["provider"] != "gh" {
		t.Fatalf("args: %v", args)
	}
	if e.stagedCount() != 0 {
		t.Fatal("the staged key is still staged")
	}
	after, err := os.Lstat(e.promoted("app-one"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the key was not moved by rename: %v", err)
	}
	if fi, _ := os.Lstat(filepath.Dir(e.promoted("app-one"))); fi.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode %o", fi.Mode().Perm())
	}
	var resp struct {
		Project ProjectView
		Clone   CloneResult
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Clone.Status != "cloned" || resp.Project.Provider == nil || *resp.Project.Provider != "gh" {
		t.Fatalf("%v %s", err, b)
	}
	// the clone ran with the project's own key, the GitHub remote shape and kind
	cl := e.cloneCalls()
	if len(cl) != 1 || cl[0].SSHKey != e.promoted("app-one") || cl[0].Kind != "github" || cl[0].Remote != "ssh://git@github.com/Octo-Cat/app-one.git" || cl[0].SSHKey == e.key {
		t.Fatalf("clone options: %+v", cl)
	}
	if ev := e.ledgerPayloads(t, "project_create"); len(ev) != 1 || ev[0]["ok"] != true {
		t.Fatalf("%v", ev)
	}
}

func TestGitHubOwnerSpellingFromBrokerIsAccepted(t *testing.T) {
	e := ghEnv(t)
	f := ghCreateFields("private")
	f["group_path"] = "octo-cat" // the signed spelling
	p := e.propose(t, "create_project", "app-one", f)
	e.wb.setReply(func(op string, args json.RawMessage) string {
		if op != "create_project" {
			return ""
		}
		entry := map[string]any{"id": "app-one", "path": filepath.Join(e.root, "app-one"), "kind": "registered", "ai_context": false, "provider": "gh",
			"remote": map[string]any{"path": "Octo-Cat/app-one", "id": 5}, "auto_merge": map[string]any{"enabled": false}}
		e.wb.mu.Lock()
		e.wb.projects = append(e.wb.projects, entry)
		e.wb.mu.Unlock()
		out, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"project": entry}})
		return string(out)
	})
	if c, b := e.apply(t, p); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	if cl := e.cloneCalls(); len(cl) != 1 || cl[0].Remote != "ssh://git@github.com/Octo-Cat/app-one.git" {
		t.Fatalf("%+v", cl)
	}
	// a different repository name or owner is NOT accepted
	e2 := ghEnv(t)
	p2 := e2.propose(t, "create_project", "app-one", f)
	e2.wb.setReply(func(op string, args json.RawMessage) string {
		if op != "create_project" {
			return ""
		}
		entry := map[string]any{"id": "app-one", "path": filepath.Join(e2.root, "app-one"), "kind": "registered", "ai_context": false, "provider": "gh",
			"remote": map[string]any{"path": "someone-else/app-one", "id": 5}, "auto_merge": map[string]any{"enabled": false}}
		out, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"project": entry}})
		return string(out)
	})
	if c, b := e2.apply(t, p2); c != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("%d %s", c, b)
	}
}

func TestPrivateKeyNeverInAnyOutput(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	e := ghEnv(t)
	var outputs []string
	capture := func(c int, b []byte) { outputs = append(outputs, string(b)) }
	// propose through the socket, apply, a GET, and a link flow too
	c, b := e.proposeRaw(t, "create_project", "app-one", ghCreateFields("private"))
	capture(c, b)
	priv, err := os.ReadFile(e.stagedPriv(t))
	if err != nil {
		t.Fatal(err)
	}
	var p w3cProposal
	json.Unmarshal(b, &p)
	capture(e.apply(t, p))
	capture(e.sockJSON(t, "GET", "/api/v1/projects/registry", ""))
	capture(e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""))
	lp := e.propose(t, "link_project", "app-two", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/app-two", "ai_context": false, "publish": "read"})
	lpriv := func() []byte {
		ents, _ := os.ReadDir(e.stagingDir())
		raw, _ := os.ReadFile(filepath.Join(e.stagingDir(), ents[0].Name(), "id_ed25519"))
		return raw
	}()
	capture(e.apply(t, lp))
	// the broker never got a private key: every call's args
	for _, call := range e.wb.opsNamed("create_project", "link_project", "projects") {
		outputs = append(outputs, string(call.Args))
	}
	ledger, _ := os.ReadFile(policy.OrchLedgerPath(e.home))
	outputs = append(outputs, string(ledger), logs.String())
	secrets := [][]byte{priv, lpriv}
	for _, sec := range secrets {
		if len(sec) < 100 {
			t.Fatal("control: no private key found")
		}
		var needles []string
		for _, line := range strings.Split(strings.TrimSpace(string(sec)), "\n") {
			if len(line) >= 20 && !strings.HasPrefix(line, "-----") {
				needles = append(needles, line)
			}
		}
		needles = append(needles, "PRIVATE KEY", "OPENSSH")
		for i, out := range outputs {
			for _, n := range needles {
				if strings.Contains(out, n) {
					t.Errorf("output %d contains %q", i, n)
				}
			}
		}
	}
	// the one place the private key's PATH may appear is the clone options (never a response)
	for i, out := range outputs[:5] {
		if strings.Contains(out, "id_ed25519") || strings.Contains(out, e.keysDir()) {
			t.Errorf("response %d names the key file", i)
		}
	}
}

// ---- providers and owners ----

func TestGitHubProviderEndpoints(t *testing.T) {
	e := newW3cEnv(t)
	gh := map[string]any{"kind": "github", "base_url": "https://api.github.com", "ssh_url": "ssh://git@github.com", "token_ref": "gh-main"}
	p := e.propose(t, "set_provider", "gh", gh)
	if !strings.Contains(strings.Join(p.Preview, "\n"), "GitHub API 주소") || strings.Contains(strings.Join(p.Preview, "\n"), "GitLab") {
		t.Fatalf("preview: %v", p.Preview)
	}
	if len(e.wb.allOps()) != 0 {
		t.Fatal("propose called the broker")
	}
	c, b := e.apply(t, p)
	if c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	var res struct{ Provider ProviderView }
	if err := json.Unmarshal(b, &res); err != nil || res.Provider.Kind != "github" || res.Provider.ID != "gh" {
		t.Fatalf("%v %s", err, b)
	}
	// owners: the account first, orgs after, with their kind; the truncated flag survives
	e.wb.setReply(func(op string, args json.RawMessage) string {
		if op == "list_groups" {
			return `{"ok":true,"result":{"groups":[{"path":"SampleUser","name":"SampleUser","kind":"user"},{"path":"octo-org","name":"Octo Org","kind":"org"}],"truncated":true}}`
		}
		return ""
	})
	code, body := e.sockJSON(t, "GET", "/api/v1/providers/gh/groups", "")
	var gr struct {
		Groups    []GroupView `json:"groups"`
		Truncated bool        `json:"truncated"`
	}
	if code != 200 || json.Unmarshal(body, &gr) != nil || len(gr.Groups) != 2 || !gr.Truncated ||
		gr.Groups[0] != (GroupView{Path: "SampleUser", Name: "SampleUser", Kind: "user"}) || gr.Groups[1].Kind != "org" {
		t.Fatalf("%d %s", code, body)
	}
	if !strings.Contains(string(body), `"kind":"user"`) {
		t.Fatalf("kind missing: %s", body)
	}
	// a GitLab row stays {path,name}
	e.wb.setReply(nil)
	_, body = e.sockJSON(t, "GET", "/api/v1/providers/gl/groups", "")
	if strings.Contains(string(body), `"kind"`) {
		t.Fatalf("gitlab rows grew a kind: %s", body)
	}
	// a malformed owner row is a broker error, not shown
	for name, row := range map[string]string{
		"bad kind":  `{"path":"x","name":"x","kind":"admin"}`,
		"bad path":  `{"path":"a b","name":"x","kind":"user"}`,
		"empty":     `{"path":"","name":"x","kind":"user"}`,
		"hostile":   "{\"path\":\"x\",\"name\":\"a\\u202eb\",\"kind\":\"user\"}",
		"slash end": `{"path":"x/","name":"x"}`,
	} {
		e.wb.setReply(func(op string, args json.RawMessage) string {
			if op == "list_groups" {
				return `{"ok":true,"result":{"groups":[` + row + `],"truncated":false}}`
			}
			return ""
		})
		if code, body := e.sockJSON(t, "GET", "/api/v1/providers/gh/groups", ""); code != 502 || v1Code(body) != "broker_error" {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	// create_group on a github provider: the broker's answer is a fixed 400
	e.wb.setReply(func(op string, args json.RawMessage) string {
		if op == "create_group" {
			return `{"ok":false,"code":"not_supported_for_provider","message":"RAW"}`
		}
		return ""
	})
	cg := e.propose(t, "create_group", "gh", map[string]any{"name": "newgroup", "path": "newgroup", "parent_path": ""})
	if c, b := e.apply(t, cg); c != 400 || v1Code(b) != "not_supported_for_provider" || strings.Contains(string(b), "RAW") {
		t.Fatalf("%d %s", c, b)
	}
}

// ---- link and attach ----

func TestLinkAttachProposeApply(t *testing.T) {
	e := ghEnv(t)
	// link (GitHub, case differing from the id)
	f := ghLinkFields()
	f["remote_path"] = "Octo-Cat/App-One"
	lp := e.propose(t, "link_project", "app-one", f)
	if lp.DeployKey == nil || lp.Fields["deploy_key_sha256"] != sha256Hex(lp.DeployKey.PublicKey) || lp.Fields["deploy_key_sha256"] == sha256Hex(e.pubLine) {
		t.Fatalf("%+v %v", lp.DeployKey, lp.Fields)
	}
	if !strings.Contains(strings.Join(lp.Preview, "\n"), "Octo-Cat/App-One") || !strings.Contains(strings.Join(lp.Preview, "\n"), "저장소 전용 배포 키") {
		t.Fatalf("%v", lp.Preview)
	}
	c, b := e.apply(t, lp)
	if c != 200 {
		t.Fatalf("link: %d %s", c, b)
	}
	args := lastCall(t, e, "link_project")
	if args["deploy_key"] != lp.DeployKey.PublicKey || args["remote_path"] != "Octo-Cat/App-One" || args["id"] != "app-one" {
		t.Fatalf("args %v", args)
	}
	if fi, err := os.Lstat(e.promoted("app-one")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("promoted key: %v", err)
	}
	cl := e.cloneCalls()
	if len(cl) != 1 || cl[0].Kind != "github" || cl[0].Remote != "ssh://git@github.com/Octo-Cat/App-One.git" || cl[0].SSHKey != e.promoted("app-one") {
		t.Fatalf("clone: %+v", cl)
	}
	var lr struct {
		Project ProjectView
		Clone   CloneResult
	}
	if json.Unmarshal(b, &lr) != nil || lr.Clone.Status != "cloned" || lr.Project.GitLab == nil || lr.Project.GitLab.Project != "Octo-Cat/App-One" {
		t.Fatalf("%s", b)
	}
	if ev := e.ledgerPayloads(t, "project_link"); len(ev) != 1 || ev[0]["subject"] != "app-one" || ev[0]["ok"] != true || ev[0]["code"] != "ok" {
		t.Fatalf("%v", ev)
	}
	// link (GitLab): the shared key, the shared key's hash, no staging
	g := e.propose(t, "link_project", "team-app", map[string]any{"provider": "gl", "remote_path": "team/team-app", "ai_context": true, "publish": "read"})
	if g.Fields["deploy_key_sha256"] != sha256Hex(e.pubLine) {
		t.Fatalf("gitlab link hash: %v", g.Fields)
	}
	if c, b := e.apply(t, g); c != 200 {
		t.Fatalf("gitlab link: %d %s", c, b)
	}
	if a := lastCall(t, e, "link_project"); a["deploy_key"] != e.pubLine {
		t.Fatalf("gitlab link key: %v", a)
	}
	if e.stagedCount() != 0 {
		t.Fatal("staged keys left")
	}
	if _, err := os.Lstat(filepath.Join(e.keysDir(), "team-app")); err == nil {
		t.Fatal("a GitLab project got a per-project key")
	}
	if cl := e.cloneCalls(); len(cl) != 2 || cl[1].SSHKey != e.key || cl[1].Kind != "gitlab" {
		t.Fatalf("gitlab clone: %+v", cl)
	}

	// attach (GitHub)
	e.addProject("att-one", "", "")
	af := map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"}
	ap := e.propose(t, "attach_remote", "att-one", af)
	if c, b := e.apply(t, ap); c != 200 {
		t.Fatalf("attach: %d %s", c, b)
	}
	if a := lastCall(t, e, "attach_remote"); a["deploy_key"] != ap.DeployKey.PublicKey || a["id"] != "att-one" {
		t.Fatalf("attach args %v", a)
	}
	if _, err := os.Lstat(e.promoted("att-one")); err != nil {
		t.Fatalf("attach key not in place: %v", err)
	}
	if len(e.cloneCalls()) != 2 {
		t.Fatalf("attach cloned: %d", len(e.cloneCalls()))
	}
	if ev := e.ledgerPayloads(t, "project_attach"); len(ev) != 1 || ev[0]["subject"] != "att-one" || ev[0]["ok"] != true {
		t.Fatalf("%v", ev)
	}
}

func TestLinkAttachRefusalsBeforeSignature(t *testing.T) {
	e := ghEnv(t)
	e.addProject("att-one", "", "")
	e.addProject("linked-one", "gl", "team/linked-one")
	e.addProject("legacy-one", "", "team/legacy-one")
	e.addProject("att-two", "", "")
	e.addProject("same-repo", "gh", "Octo-Cat/dup-repo")
	mkLink := func(sub, prov, path string) (int, []byte) {
		return e.proposeRaw(t, "link_project", sub, map[string]any{"provider": prov, "remote_path": path, "ai_context": false, "publish": "write"})
	}
	mkAttach := func(sub, prov, path string) (int, []byte) {
		return e.proposeRaw(t, "attach_remote", sub, map[string]any{"provider": prov, "remote_path": path, "publish": "write"})
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
	add("link: unknown provider", func() (int, []byte) { return mkLink("app-one", "nope", "team/app-one") }, 404, "provider_unknown")
	add("link: name differs from the repo", func() (int, []byte) { return mkLink("other-name", "gh", "Octo-Cat/app-one") }, 400, "remote_name_mismatch")
	add("link: github needs owner/repo", func() (int, []byte) { return mkLink("app-one", "gh", "a/b/app-one") }, 400, "bad_request")
	add("link: gitlab needs lowercase", func() (int, []byte) { return mkLink("app-one", "gl", "Team/app-one") }, 400, "bad_request")
	add("link: existing project id", func() (int, []byte) { return mkLink("att-one", "gh", "Octo-Cat/att-one") }, 409, "project_exists")
	add("link: id already linked", func() (int, []byte) { return mkLink("linked-one", "gh", "Octo-Cat/linked-one") }, 409, "already_linked")
	add("link: same repo linked by another (case)", func() (int, []byte) { return mkLink("dup-repo", "gh", "octo-cat/DUP-repo") }, 409, "already_linked")
	add("attach: unknown project", func() (int, []byte) { return mkAttach("nobody", "gh", "Octo-Cat/nobody") }, 404, "project_unknown")
	add("attach: already provider-linked", func() (int, []byte) { return mkAttach("linked-one", "gh", "Octo-Cat/linked-one") }, 409, "already_linked")
	add("attach: legacy remote mapping", func() (int, []byte) { return mkAttach("legacy-one", "gh", "Octo-Cat/legacy-one") }, 409, "already_linked")
	add("attach: name differs", func() (int, []byte) { return mkAttach("att-two", "gh", "Octo-Cat/else") }, 400, "remote_name_mismatch")
	add("attach: same repo linked elsewhere", func() (int, []byte) { return mkAttach("att-two", "gh", "octo-cat/dup-repo") }, 400, "remote_name_mismatch")
	for _, c := range cases {
		if c.c != c.status || v1Code(c.b) != c.code {
			t.Errorf("%s: %d %s", c.name, c.c, c.b)
		}
		if strings.Contains(string(c.b), `"change"`) || strings.Contains(string(c.b), `"request"`) {
			t.Errorf("%s: a signing message was handed out", c.name)
		}
	}
	if e.stagedCount() != 0 {
		t.Fatalf("a refused proposal left %d staged keys", e.stagedCount())
	}
	// the new project's folder is taken / the class is missing: before the registry is read
	os.Mkdir(filepath.Join(e.root, "app-one"), 0o700)
	before := len(e.wb.allOps())
	if c, b := mkLink("app-one", "gh", "Octo-Cat/app-one"); c != 409 || v1Code(b) != "path_exists" {
		t.Errorf("%d %s", c, b)
	}
	e.d.handler.policy = func() PolicyView { return PolicyView{Hash: "h", Classes: map[string]string{"docs": "L1"}} }
	if c, b := mkLink("app-two-new", "gh", "Octo-Cat/app-two-new"); c != 400 || v1Code(b) != "policy_class_missing" {
		t.Errorf("%d %s", c, b)
	}
	if len(e.wb.allOps()) != before {
		t.Error("the registry was read before the local checks")
	}
	// attach needs no policy class
	e.addProject("att-three", "", "")
	if c, b := mkAttach("att-three", "gh", "Octo-Cat/att-three"); c != 200 {
		t.Errorf("attach without class: %d %s", c, b)
	}
}

func TestAttachStaticRemoteWins(t *testing.T) {
	e := ghEnv(t)
	e.d.handler.staticRepos = map[string]bool{filepath.Join(e.root, "sample-app"): true}
	e.addProject("sample-app", "", "")
	e.addProject("plain-one", "", "")
	f := map[string]any{"provider": "gh", "remote_path": "Octo-Cat/sample-app", "publish": "write"}
	c, b := e.proposeRaw(t, "attach_remote", "sample-app", f)
	if c != 409 || v1Code(b) != "static_remote_wins" || strings.Contains(string(b), `"message":"aigate-registry`) || strings.Contains(string(b), `"change"`) {
		t.Fatalf("%d %s", c, b)
	}
	if e.stagedCount() != 0 {
		t.Fatal("a key was staged for a refused attach")
	}
	if len(e.wb.writeCalls()) != 0 {
		t.Fatal("write")
	}
	// the static entry appearing between propose and apply
	pf := map[string]any{"provider": "gh", "remote_path": "Octo-Cat/plain-one", "publish": "write"}
	p := e.propose(t, "attach_remote", "plain-one", pf)
	e.d.handler.staticRepos[filepath.Join(e.root, "plain-one")] = true
	if c, b := e.apply(t, p); c != 409 || v1Code(b) != "static_remote_wins" {
		t.Fatalf("apply: %d %s", c, b)
	}
	if len(e.wb.writeCalls()) != 0 {
		t.Fatal("write after static_remote_wins")
	}
	// link has no such rule (a NEW project's path is under the repo root; path collision is path_exists)
	e.d.handler.staticRepos[filepath.Join(e.root, "app-one")] = true
	if c, b := e.proposeRaw(t, "link_project", "app-one", ghLinkFields()); c != 200 {
		t.Fatalf("link: %d %s", c, b)
	}
}

func TestStaticRemoteReposComeFromConfig(t *testing.T) {
	e := ghEnv(t, func(c *Config) { c.StaticRemoteRepos = []string{"/somewhere/else", "/x/../x/contra"} })
	if !e.d.handler.staticRepos["/somewhere/else"] || !e.d.handler.staticRepos["/x/contra"] || len(e.d.handler.staticRepos) != 2 {
		t.Fatalf("%v", e.d.handler.staticRepos)
	}
}

func TestApplyMapsNewCodes(t *testing.T) {
	want := map[string]struct {
		status int
		wire   string
	}{
		"repo_not_found": {404, "repo_not_found"}, "already_linked": {409, "already_linked"}, "project_exists": {409, "project_exists"},
		"project_unknown": {404, "project_unknown"}, "github_refused": {502, "github_refused"}, "gitlab_refused": {502, "gitlab_refused"},
		"provider_rate_limited": {429, "provider_rate_limited"}, "not_supported_for_provider": {400, "not_supported_for_provider"},
		"visibility_required": {400, "visibility_required"}, "visibility_not_allowed": {400, "visibility_not_allowed"},
		"provider_unreachable": {502, "provider_unreachable"}, "group_unknown": {404, "group_unknown"}, "project_name_taken": {409, "project_name_taken"},
		"deploy_key_mismatch": {409, "deploy_key_mismatch"}, "rate_limited": {429, "broker_rate_limited"}, "path_not_allowed": {400, "path_not_allowed"},
	}
	for code, w := range want {
		for _, flow := range []string{"create-github", "create-gitlab", "link-github", "attach-github"} {
			e := ghEnv(t)
			e.addProject("att-one", "", "")
			var p w3cProposal
			switch flow {
			case "create-github":
				p = e.propose(t, "create_project", "app-one", ghCreateFields("private"))
			case "create-gitlab":
				p = e.proposeAction(t, "create_project")
			case "link-github":
				p = e.propose(t, "link_project", "app-one", ghLinkFields())
			default:
				p = e.propose(t, "attach_remote", "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"})
			}
			e.wb.setReply(func(op string, args json.RawMessage) string {
				if op == "create_project" || op == "link_project" || op == "attach_remote" {
					return `{"ok":false,"code":"` + code + `","message":"RAW-BROKER-TEXT github.com/secret"}`
				}
				return ""
			})
			c, b := e.apply(t, p)
			if c != w.status || v1Code(b) != w.wire || strings.Contains(string(b), "RAW-BROKER-TEXT") || strings.Contains(string(b), "secret") {
				t.Errorf("%s / %s: %d %s", code, flow, c, b)
			}
			if strings.HasPrefix(flow, "create-github") || strings.HasPrefix(flow, "link") || strings.HasPrefix(flow, "attach") {
				// a known refusal deletes the staged key and leaves no promoted one
				if e.stagedCount() != 0 {
					t.Errorf("%s / %s: staged key kept after a known refusal", code, flow)
				}
				if ents, _ := os.ReadDir(e.keysDir()); len(ents) != 0 {
					t.Errorf("%s / %s: key promoted after a refusal", code, flow)
				}
			}
		}
	}
	// wording: GitHub flows never say GitLab, GitLab flows keep the W3c sentences
	e := ghEnv(t)
	p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	for code, mustNot := range map[string]string{"provider_unreachable": "GitLab", "group_unknown": "GitLab", "project_name_taken": "GitLab"} {
		e.wb.setReply(func(op string, args json.RawMessage) string { return `{"ok":false,"code":"` + code + `"}` })
		// each refusal burns nothing in the fake broker; the staged key was deleted by the first: restage
		p = e.propose(t, "create_project", "app-one", ghCreateFields("private"))
		_, b := e.apply(t, p)
		if strings.Contains(string(b), mustNot) {
			t.Errorf("%s: GitHub flow answers with GitLab wording: %s", code, b)
		}
	}
	// partial: created vs linked orphans are told apart
	partial := func(code, origin string) string {
		return `{"ok":false,"code":"` + code + `","detail":{"provider":"gh","project":"Octo-Cat/app-one","project_id":5,"step":"deploy_key","orphan_recorded":true,"orphan_dropped":false,"origin":"` + origin + `"}}`
	}
	e.wb.setReply(func(op string, args json.RawMessage) string {
		switch op {
		case "create_project":
			return partial("create_partial", "created")
		case "link_project":
			return partial("link_partial", "linked")
		}
		return ""
	})
	p = e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	c, b := e.apply(t, p)
	var er struct {
		Error struct {
			Code    string
			Message string
			Detail  map[string]any
		}
	}
	if c != 502 || json.Unmarshal(b, &er) != nil || er.Error.Code != "create_partial" || er.Error.Detail["origin"] != "created" || er.Error.Detail["step"] != "deploy_key" ||
		strings.Contains(er.Error.Message, "GitLab") || !strings.Contains(er.Error.Message, "GitHub") {
		t.Fatalf("create_partial: %d %s", c, b)
	}
	lp := e.propose(t, "link_project", "app-two", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/app-two", "ai_context": false, "publish": "write"})
	c, b = e.apply(t, lp)
	er.Error.Detail = nil
	if c != 502 || json.Unmarshal(b, &er) != nil || er.Error.Code != "link_partial" || er.Error.Detail["origin"] != "linked" || er.Error.Detail["project"] != "Octo-Cat/app-one" ||
		!strings.Contains(er.Error.Message, "저장소는 지우지 마세요") {
		t.Fatalf("link_partial: %d %s", c, b)
	}
	if strings.Contains(er.Error.Message, "삭제하지 않으니") {
		t.Fatalf("a linked orphan got the created-orphan sentence: %s", er.Error.Message)
	}
	// a link_partial reply on a create (or a create_partial on a link) is not interpreted as a partial: unknown outcome
	e.wb.setReply(func(op string, args json.RawMessage) string {
		if op == "create_project" {
			return partial("link_partial", "linked")
		}
		return ""
	})
	cp := e.propose(t, "create_project", "app-three", ghCreateFields("private"))
	if c, b := e.apply(t, cp); c != 504 || v1Code(b) != "outcome_unknown" {
		t.Fatalf("link_partial on create: %d %s", c, b)
	}
}

func TestOrchdOwnCodesAreFixed(t *testing.T) {
	for _, he := range []*httpError{errKeyExpired, errKeyStagingFailed, errStaticRemoteWins, errNameMismatch, errKeyUnavailable, errRepoNotFound, errAlreadyLinked, errLinkPartial, errLinkUnknown,
		errGHRefused, errProviderRateLimited, errNotSupportedForProvider, errVisibilityRequired, errVisibilityNotAllowed} {
		if he.v1Code == "" || he.msg == "" || he.code < 400 || hostileText(he.msg) || strings.ContainsAny(he.msg, "{}<>") {
			t.Errorf("%+v", he)
		}
	}
	statuses := map[string]int{"key_expired": 409, "key_staging_failed": 503, "static_remote_wins": 409, "remote_name_mismatch": 400, "key_unavailable": 503, "link_partial": 502}
	for _, he := range []*httpError{errKeyExpired, errKeyStagingFailed, errStaticRemoteWins, errNameMismatch, errKeyUnavailable, errLinkPartial} {
		if statuses[he.v1Code] != he.code {
			t.Errorf("%s: %d", he.v1Code, he.code)
		}
	}
}

// ---- key disposition per outcome ----

func TestKeyDispositionPerOutcome(t *testing.T) {
	mkPartial := func(step string) string {
		return `{"ok":false,"code":"create_partial","detail":{"provider":"gh","project":"Octo-Cat/app-one","project_id":5,"step":"` + step + `","orphan_recorded":true,"orphan_dropped":false}}`
	}
	registered := func(e *w3cEnv) {
		e.addProject("app-one", "gh", "Octo-Cat/app-one")
	}
	for _, tc := range []struct {
		name       string
		reply      func(op string, args json.RawMessage) string
		before     func(e *w3cEnv)
		wantStaged int
		wantKey    bool
		status     int
	}{
		{"refused", func(op string, a json.RawMessage) string {
			if op == "create_project" {
				return `{"ok":false,"code":"github_refused"}`
			}
			return ""
		}, nil, 0, false, 502},
		{"partial, not registered", func(op string, a json.RawMessage) string {
			if op == "create_project" {
				return mkPartial("deploy_key")
			}
			return ""
		}, nil, 0, false, 502},
		{"partial, registry says registered", func(op string, a json.RawMessage) string {
			if op == "create_project" {
				return mkPartial("register")
			}
			return ""
		}, registered, 0, true, 502},
		{"partial, registry unreadable", func(op string, a json.RawMessage) string {
			if op == "create_project" {
				return mkPartial("deploy_key")
			}
			return "DROP"
		}, nil, 0, false, 502},
		{"unknown, registered", func(op string, a json.RawMessage) string {
			if op == "create_project" {
				return "DROP"
			}
			return ""
		}, registered, 0, true, 504},
		{"unknown, absent", func(op string, a json.RawMessage) string {
			if op == "create_project" {
				return "DROP"
			}
			return ""
		}, nil, 0, false, 504},
		{"unknown, registry unreadable", func(op string, a json.RawMessage) string { return "DROP" }, nil, 1, false, 504},
	} {
		e := ghEnv(t)
		p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
		if tc.before != nil {
			tc.before(e)
		}
		e.wb.setReply(tc.reply)
		c, b := e.apply(t, p)
		if c != tc.status {
			t.Errorf("%s: %d %s", tc.name, c, b)
		}
		if e.stagedCount() != tc.wantStaged {
			t.Errorf("%s: staged %d want %d", tc.name, e.stagedCount(), tc.wantStaged)
		}
		if _, err := os.Lstat(e.promoted("app-one")); (err == nil) != tc.wantKey {
			t.Errorf("%s: promoted=%v want %v", tc.name, err == nil, tc.wantKey)
		}
	}
}

func TestKeyKeptWhenNothingReachedTheBroker(t *testing.T) {
	// unsent: the broker is unreachable at apply
	e := ghEnv(t)
	p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	good := e.d.handler.broker
	e.d.handler.broker = &broker.Client{Socket: filepath.Join(shortDir(t), "none.sock")}
	if c, b := e.apply(t, p); c != 503 {
		t.Fatalf("%d %s", c, b)
	}
	if e.stagedCount() != 1 {
		t.Fatalf("an unsent apply deleted the staged key (%d)", e.stagedCount())
	}
	e.d.handler.broker = good
	if c, b := e.apply(t, p); c != 200 {
		t.Fatalf("the same signed change could not be applied after the broker came back: %d %s", c, b)
	}
	// limit hit: the slot is not available, nothing sent, key stays
	e2 := ghEnv(t)
	clk := &testClock{}
	e2.d.handler.now = clk.now
	for i := 0; i < CreateProjectPerActorHour; i++ {
		q := e2.propose(t, "create_project", "app-"+string(rune('a'+i))+"x", ghCreateFields("private"))
		if c, b := e2.apply(t, q); c != 200 {
			t.Fatalf("%d %s", c, b)
		}
	}
	q := e2.propose(t, "create_project", "app-zz", ghCreateFields("private"))
	if c, b := e2.apply(t, q); c != 429 {
		t.Fatalf("%d %s", c, b)
	}
	if e2.stagedCount() != 1 {
		t.Fatalf("a rate-limited apply deleted the staged key (%d)", e2.stagedCount())
	}
	clk.set(time.Hour + time.Minute)
	if c, b := e2.apply(t, q); c != 200 {
		t.Fatalf("after the hour: %d %s", c, b)
	}
}

// ---- limits, ledger, write guard, broker down ----

func TestLimitsW3e(t *testing.T) {
	if LinkAttachPerActorHour != 10 || CreateProjectPerActorHour != 5 {
		t.Fatal("limits changed")
	}
	e := ghEnv(t)
	clk := &testClock{}
	e.d.handler.now = clk.now
	n := 0
	link := func(call func(method, path, body string) (int, []byte)) (int, []byte) {
		n++
		name := "link-" + string(rune('a'+n/26)) + string(rune('a'+n%26))
		p := e.propose(t, "link_project", name, map[string]any{"provider": "gh", "remote_path": "Octo-Cat/" + name, "ai_context": false, "publish": "read"})
		return call("POST", e.applyPath("link_project"), jsonBody(e.applyBody(p, webauthnSig)))
	}
	attach := func() (int, []byte) {
		n++
		name := "att-" + string(rune('a'+n/26)) + string(rune('a'+n%26))
		e.addProject(name, "", "")
		p := e.propose(t, "attach_remote", name, map[string]any{"provider": "gh", "remote_path": "Octo-Cat/" + name, "publish": "read"})
		return e.sockJSON(t, "POST", e.applyPath("attach_remote"), jsonBody(e.applyBody(p, webauthnSig)))
	}
	sock := func(m, p, b string) (int, []byte) { return e.sockJSON(t, m, p, b) }
	// 5 links + 5 attaches share ONE bucket of 10
	for i := 0; i < 5; i++ {
		if c, b := link(sock); c != 200 {
			t.Fatalf("link %d: %d %s", i, c, b)
		}
		if c, b := attach(); c != 200 {
			t.Fatalf("attach %d: %d %s", i, c, b)
		}
	}
	before := len(e.wb.writeCalls())
	for name, f := range map[string]func() (int, []byte){"link": func() (int, []byte) { return link(sock) }, "attach": attach} {
		if c, b := f(); c != 429 || v1Code(b) != "project_rate_limited" || len(e.wb.writeCalls()) != before {
			t.Fatalf("%s over the limit: %d %s", name, c, b)
		}
	}
	// the create bucket is independent
	cp := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	if c, b := e.apply(t, cp); c != 200 {
		t.Fatalf("create after link limit: %d %s", c, b)
	}
	// the provider bucket (set_provider, delete_provider) is independent too
	sp := e.proposeAction(t, "set_provider")
	if c, b := e.apply(t, sp); c != 200 {
		t.Fatalf("set_provider after link limit: %d %s", c, b)
	}
	// a second actor has its own bucket
	other := e.tokenCallerW3c(t, "mac-ssh")
	if c, b := link(other); c != 200 {
		t.Fatalf("second actor: %d %s", c, b)
	}
	// refund rules: a known broker refusal keeps the slot spent; an unsent request gives it back
	clk.set(time.Hour + time.Minute)
	e.wb.setReply(func(op string, a json.RawMessage) string {
		if op == "link_project" {
			return `{"ok":false,"code":"repo_not_found"}`
		}
		return ""
	})
	for i := 0; i < LinkAttachPerActorHour; i++ {
		if c, _ := link(sock); c != 404 {
			t.Fatalf("refusal %d: %d", i, c)
		}
	}
	if c, b := link(sock); c != 429 {
		t.Fatalf("refused calls must keep their slot: %d %s", c, b)
	}
	clk.set(2*time.Hour + 2*time.Minute)
	e.wb.setReply(nil)
	good := e.d.handler.broker
	var props []w3cProposal
	for i := 0; i < LinkAttachPerActorHour+3; i++ {
		name := "unsent-" + string(rune('a'+i))
		props = append(props, e.propose(t, "link_project", name, map[string]any{"provider": "gh", "remote_path": "Octo-Cat/" + name, "ai_context": false, "publish": "read"}))
	}
	e.d.handler.broker = &broker.Client{Socket: filepath.Join(shortDir(t), "none.sock")}
	for i, p := range props {
		if c, b := e.sockJSON(t, "POST", e.applyPath("link_project"), jsonBody(e.applyBody(p, webauthnSig))); c != 503 {
			t.Fatalf("unsent %d: %d %s", i, c, b)
		}
	}
	e.d.handler.broker = good
	// no slot was lost to the unsent requests
	if c, b := e.apply(t, props[0]); c != 200 {
		t.Fatalf("after unsent requests: %d %s", c, b)
	}
}

func TestLedgerNoSecretsW3e(t *testing.T) {
	e := ghEnv(t)
	e.addProject("att-one", "", "")
	lp := e.propose(t, "link_project", "app-one", ghLinkFields())
	e.apply(t, lp)
	ap := e.propose(t, "attach_remote", "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"})
	e.apply(t, ap)
	e.wb.setReply(func(op string, a json.RawMessage) string {
		if op == "attach_remote" {
			return `{"ok":false,"code":"github_refused"}`
		}
		return ""
	})
	e.addProject("att-two", "", "")
	bp := e.propose(t, "attach_remote", "att-two", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-two", "publish": "write"})
	e.apply(t, bp)
	for typ, n := range map[string]int{"project_link": 1, "project_attach": 2} {
		ev := e.ledgerPayloads(t, typ)
		if len(ev) != n {
			t.Fatalf("%s: %v", typ, ev)
		}
		for _, p := range ev {
			if ledgerKeys(p) != "action,actor,code,ok,subject" {
				t.Errorf("%s keys: %v", typ, ledgerKeys(p))
			}
		}
	}
	if ev := e.ledgerPayloads(t, "project_attach"); ev[1]["ok"] != false || ev[1]["code"] != "github_refused" {
		t.Fatalf("%v", ev)
	}
	raw, _ := os.ReadFile(policy.OrchLedgerPath(e.home))
	for _, bad := range []string{"ssh-ed25519", "Octo-Cat", "id_ed25519", "deploy_key", "github.com", e.sshDir(), "AAAA"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the ledger contains %q", bad)
		}
	}
	// refused before the broker: no event (as W3a)
	n := len(e.ledgerPayloads(t, "project_link"))
	e.proposeRaw(t, "link_project", "other-name", ghLinkFields())
	if len(e.ledgerPayloads(t, "project_link")) != n {
		t.Error("an event for a proposal")
	}
}

func TestRequireWriteAndCSRFW3e(t *testing.T) {
	e := ghEnv(t)
	srv := e.httpServer(t)
	tok, csrf := e.newSession(t, "alice")
	token := e.newToken(t, "mac-ssh")
	e.addProject("att-one", "", "")
	lp := e.propose(t, "link_project", "app-one", ghLinkFields())
	ap := e.propose(t, "attach_remote", "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"})
	targets := []struct{ path, body string }{
		{"/api/v1/projects/link/propose", jsonBody(map[string]any{"action": "link_project", "subject": "app-one", "fields": ghLinkFields()})},
		{"/api/v1/projects/link/apply", jsonBody(e.applyBody(lp, webauthnSig))},
		{"/api/v1/projects/attach/propose", jsonBody(map[string]any{"action": "attach_remote", "subject": "att-one", "fields": map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"}})},
		{"/api/v1/projects/attach/apply", jsonBody(e.applyBody(ap, webauthnSig))},
	}
	// proposals first: an apply of link clones the folder, a later link proposal would be path_exists
	for _, tg := range targets {
		path, body := tg.path, tg.body
		before := len(e.wb.allOps())
		staged := e.stagedCount()
		for name, h := range map[string]hdr{
			"no credentials":     {},
			"session no csrf":    {"Cookie": cookie(tok)},
			"session wrong csrf": {"Cookie": cookie(tok), CSRFHeader: "csrf-wrong"},
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
		if len(e.wb.allOps()) != before || e.stagedCount() != staged {
			t.Errorf("%s: reached the broker or staged a key without authority", path)
		}
		if code, _, b := httpDo(t, srv, "POST", path, body, hdr{"Authorization": "Bearer " + token}); code != 200 {
			t.Errorf("%s with a token: %d %s", path, code, b)
		}
	}
	if code, _, _ := httpDo(t, srv, "GET", "/api/v1/projects/link/apply", "", hdr{"Authorization": "Bearer " + token}); code != 405 && code != 404 {
		t.Errorf("GET on apply: %d", code)
	}
	_ = csrf
}

func TestLinkAttachNeedBrokerCreateGitHubProposeDoesNot(t *testing.T) {
	e := ghEnv(t, func(c *Config) { c.BrokerSocket = "" })
	if c, b := e.proposeRaw(t, "link_project", "app-one", ghLinkFields()); c != 503 || v1Code(b) != "broker_unavailable" {
		t.Fatalf("link: %d %s", c, b)
	}
	if c, b := e.proposeRaw(t, "attach_remote", "att-one", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/att-one", "publish": "write"}); c != 503 {
		t.Fatalf("attach: %d %s", c, b)
	}
	// a GitHub create proposal needs no broker (nothing is looked up)
	if c, b := e.proposeRaw(t, "create_project", "app-one", ghCreateFields("private")); c != 200 {
		t.Fatalf("create: %d %s", c, b)
	}
	// an apply with the broker down is refused after nothing was sent and keeps the key
	p := e.propose(t, "create_project", "app-two", ghCreateFields("private"))
	if c, b := e.apply(t, p); c != 503 || v1Code(b) != "broker_unavailable" {
		t.Fatalf("apply: %d %s", c, b)
	}
}

// ---- clone of a GitHub project ----

func TestCloneEndpointUsesPerProjectKeyForGitHub(t *testing.T) {
	e := ghEnv(t)
	e.addProject("hello", "gh", "Octo-Cat/Hello")
	// no key yet: nothing is cloned, fixed code, no limiter slot spent
	c, b := e.sockJSON(t, "POST", "/api/v1/projects/hello/clone", "")
	if c != 503 || v1Code(b) != "key_unavailable" || len(e.cloneCalls()) != 0 {
		t.Fatalf("%d %s", c, b)
	}
	if err := os.MkdirAll(filepath.Join(e.keysDir(), "hello"), 0o700); err != nil {
		t.Fatal(err)
	}
	priv := e.promoted("hello")
	writeFile(t, priv, "k")
	os.Chmod(priv, 0o644)
	if c, b := e.sockJSON(t, "POST", "/api/v1/projects/hello/clone", ""); c != 503 || v1Code(b) != "key_unavailable" || len(e.cloneCalls()) != 0 {
		t.Fatalf("loose mode: %d %s", c, b)
	}
	os.Chmod(priv, 0o600)
	// the body is ignored: neither a key path nor a remote can come from the request
	c, b = e.sockJSON(t, "POST", "/api/v1/projects/hello/clone", `{"ssh_key":"/etc/passwd","key":"/tmp/x","remote":"ssh://git@evil.example/x/y.git","kind":"gitlab"}`)
	if c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	cl := e.cloneCalls()
	if len(cl) != 1 || cl[0].SSHKey != priv || cl[0].Kind != "github" || cl[0].Remote != "ssh://git@github.com/Octo-Cat/Hello.git" {
		t.Fatalf("%+v", cl)
	}
	// a GitLab project still uses the shared key
	e.addProject("glproj", "gl", "team/glproj")
	if c, b := e.sockJSON(t, "POST", "/api/v1/projects/glproj/clone", ""); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	if cl := e.cloneCalls(); len(cl) != 2 || cl[1].SSHKey != e.key || cl[1].Remote != "ssh://git@gitlab.example/team/glproj.git" {
		t.Fatalf("%+v", cl)
	}
	// the clone helper sees no request data in the key path
	if !strings.HasPrefix(cl[0].SSHKey, e.keysDir()+string(filepath.Separator)) {
		t.Fatal("key outside the keys dir")
	}
	_ = clone.DefaultTimeout
}

// A double submit of the same signed apply: while one is in flight the key is
// held, the other is refused before the broker (and cannot delete or move it).
func TestDoubleApplyOfOneStagedKeyIsRefused(t *testing.T) {
	e := ghEnv(t)
	p := e.propose(t, "create_project", "app-one", ghCreateFields("private"))
	if _, err := e.d.handler.keys.Staged(p.Fields["deploy_key_sha256"].(string)); err != nil { // the first apply holds it
		t.Fatal(err)
	}
	c, b := e.apply(t, p)
	if c != 409 || v1Code(b) != "request_reused" || len(e.wb.allOps()) != 0 {
		t.Fatalf("%d %s %v", c, b, e.wb.allOps())
	}
	if e.stagedCount() != 1 {
		t.Fatal("the refused second apply removed the key")
	}
}
