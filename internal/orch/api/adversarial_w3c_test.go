package api

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/clone"
	"aigate/internal/orch/policy"
)

// W3c (C6) adversarial suite for orchd. orchd never speaks to GitLab over
// HTTP and never writes the registry: it asks the broker, which verifies
// signatures. These tests attack what orchd itself does: it clones from a
// REGISTRY entry (never from request data), runs git with a closed option set,
// proposes without side effects, applies only what was proposed, and keeps
// working (read-only for W3c) when the broker is gone.
//
// The names TestOrchdWorksWithBrokerDown (W2b), TestNoBrokerImportInOrchd,
// TestProposeNeverWritesBroker and TestApplyForgedFieldsRefusedBeforeBroker (W3a)
// already exist with their older scopes; the W3c scopes are the *W3c tests
// below. The real-git hook test needs the clone package's seams and lives in
// internal/orch/clone/adversarial_w3c_test.go.

const expectedRemote = "ssh://git@gitlab.example/team/app-one.git"

// ---------- clone: where the remote comes from ----------

// Whatever a request carries (body, query, headers), and whatever the broker's
// create_project reply claims (its ssh_url and clone_url point at an evil
// host here), the clone goes to provider.ssh_url + project path from the
// registry read, into the registry entry's folder, with the configured key.
func TestCloneNeverUsesRequestSuppliedURL(t *testing.T) {
	e := newW3cEnv(t)
	e.d.handler.cloneLimit = newActorLimiter(10000, time.Hour) // the loop below is far above the per-actor limit
	e.setProjects(e.linked("app-one"))
	evil := `"ssh://git@evil.example/x/y.git"`
	bodies := map[string]string{
		"url":        `{"url":` + evil + `}`,
		"remote":     `{"remote":` + evil + `,"dest":"/etc/cron.d/x","path":"/tmp/x","provider":"evil"}`,
		"ssh_url":    `{"ssh_url":"ssh://git@evil.example","clone_url":` + evil + `,"project":"x/y"}`,
		"options":    `{"git":"/tmp/evil-git","ssh":"/tmp/evil-ssh","key":"/etc/shadow","known_hosts":"/dev/null","args":["--upload-pack=x"]}`,
		"form":       `url=ssh://git@evil.example/x/y.git`,
		"nested":     `{"a":{"b":{"remote":` + evil + `}}}`,
		"array":      `[` + evil + `]`,
		"nul":        "{\"url\":\"ssh://git@evil.example/x\x00\"}",
		"empty":      ``,
		"git config": `{"core.sshCommand":"sh -c id","protocol.allow":"always","GIT_SSH_COMMAND":"x"}`,
	}
	queries := []string{"", "?url=ssh://git@evil.example/x/y.git", "?remote=" + "ssh%3A%2F%2Fgit%40evil.example%2Fx%2Fy.git&dest=%2Fetc", "?id=other&provider=evil"}
	headers := []hdr{nil, {"X-Forwarded-Host": "evil.example", "X-Remote-Url": "ssh://git@evil.example/x/y.git", "Referer": "http://evil.example/", "X-Original-URL": "/api/v1/projects/other/clone"}}
	n := 0
	for name, body := range bodies {
		for _, q := range queries {
			for _, h := range headers {
				os.RemoveAll(filepath.Join(e.root, "app-one"))
				code, _, b := e.sockDo(t, "POST", "/api/v1/projects/app-one/clone"+q, body, h)
				if code != 200 {
					t.Fatalf("%s %s: %d %s", name, q, code, b)
				}
				n++
			}
		}
	}
	calls := e.cloneCalls()
	if len(calls) != n {
		t.Fatalf("%d clones for %d requests", len(calls), n)
	}
	for i, o := range calls {
		if o.Remote != expectedRemote || o.Dest != filepath.Join(e.root, "app-one") || o.SSHKey != e.key || o.KnownHosts != e.known ||
			o.Git != w3cFakeGit || o.SSH != w3cFakeSSH || len(o.Roots) != 1 {
			t.Fatalf("clone %d took something from the request: %+v", i, o)
		}
	}
	// (a shifted `issued` time or a replaced request ID is not orchd's to judge:
	// it forwards them and the broker's signature check refuses them, see the
	// broker suite and TestOrchdForwardsSignatureAndNeverJudgesIt)
	// the create flow: the broker reply names an evil ssh_url/clone_url and an evil
	// project; the automatic clone still uses the registry (provider + project)
	e2 := newW3cEnv(t)
	code, b := e2.apply(t, e2.proposeAction(t, "create_project"))
	if code != 200 {
		t.Fatalf("create: %d %s", code, b)
	}
	for _, o := range e2.cloneCalls() {
		if o.Remote != expectedRemote || strings.Contains(o.Remote, "evil") {
			t.Fatalf("auto clone: %+v", o)
		}
	}
	if len(e2.cloneCalls()) != 1 {
		t.Fatalf("%d auto clones", len(e2.cloneCalls()))
	}
	// the registry says the new project lives outside the allowed roots: no clone
	e3 := newW3cEnv(t)
	e3.wb.mu.Lock()
	e3.wb.pathRoot = "/tmp/not-a-root"
	e3.wb.mu.Unlock()
	code, b = e3.apply(t, e3.proposeAction(t, "create_project"))
	var out cloneOut
	if code != 200 || json.Unmarshal(b, &out) != nil || out.Clone.Status != "failed" || len(e3.cloneCalls()) != 0 {
		t.Fatalf("path outside the roots: %d %s (clones %d)", code, b, len(e3.cloneCalls()))
	}
	// and the forged-request shapes of the create endpoints are refused before any clone
	for name, mut := range map[string]func(m map[string]any){
		"top-level remote":     func(m map[string]any) { m["remote"] = "ssh://git@evil.example/x/y.git" },
		"top-level ssh_url":    func(m map[string]any) { m["ssh_url"] = "ssh://git@evil.example" },
		"top-level clone_url":  func(m map[string]any) { m["clone_url"] = "ssh://git@evil.example/x/y.git" },
		"fields remote":        func(m map[string]any) { m["fields"].(map[string]any)["remote"] = "ssh://git@evil.example/x/y.git" },
		"fields clone":         func(m map[string]any) { m["fields"].(map[string]any)["clone"] = true },
		"fields ssh_url":       func(m map[string]any) { m["fields"].(map[string]any)["ssh_url"] = "ssh://git@evil.example" },
		"fields deploy_key":    func(m map[string]any) { m["fields"].(map[string]any)["deploy_key"] = "ssh-ed25519 AAAA evil" },
		"fields provider swap": func(m map[string]any) { m["fields"].(map[string]any)["provider"] = "evil" },
	} {
		e4 := newW3cEnv(t)
		p := e4.proposeAction(t, "create_project")
		m := e4.applyBody(p, webauthnSig)
		mut(m)
		if c, b := e4.sockJSON(t, "POST", "/api/v1/projects/create/apply", jsonBody(m)); c == 200 || len(e4.cloneCalls()) != 0 || len(e4.wb.writeCalls()) != 0 {
			t.Errorf("%s: %d %s (clones %d, writes %d)", name, c, b, len(e4.cloneCalls()), len(e4.wb.writeCalls()))
		}
	}
}

// ---------- clone: option injection ----------

// Everything that could turn the clone command line into something else is
// refused before git is started: option-looking or odd remotes (-o...,
// --upload-pack=, ext::, file://, scp-style, https, other users or hosts, extra
// path), destinations that are not a plain new folder directly under a root,
// key and known_hosts paths with shell or option characters. A recording
// "git" proves nothing ran and nothing was created. At the API, hostile
// registry rows are refused the same way and never reach the clone helper.
func TestCloneOptionInjectionRefused(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(dir, "repos")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "git-ran")
	shim := filepath.Join(dir, "git-shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho \"$@\" >> '"+marker+"'\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	key, known := filepath.Join(dir, "id_deploy"), filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("gitlab.example "+w3cKnownKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := func(remote, dest string) clone.Options {
		return clone.Options{Git: shim, Remote: remote, Dest: dest, Roots: []string{root}, SSHKey: key, KnownHosts: known}
	}
	good := "ssh://git@gitlab.example/team/app-one.git"
	for _, remote := range []string{
		"-oProxyCommand=touch /tmp/pwn", "--upload-pack=touch /tmp/pwn", "-o", "--", "ext::sh -c touch% /tmp/pwn", "file:///etc", "/etc/passwd", "../x",
		"git@gitlab.example:team/app-one.git", "https://gitlab.example/team/app-one.git", "http://gitlab.example/x.git", "git://gitlab.example/x.git",
		"ssh://root@gitlab.example/team/app-one.git", "ssh://git@gitlab.example", "ssh://git@gitlab.example/", "ssh://git@gitlab.example/team/app-one",
		"ssh://git@gitlab.example/team/app-one.git?x=1", "ssh://git@gitlab.example/team/app-one.git#x", "ssh://git@gitlab.example//team/app-one.git",
		"ssh://git@gitlab.example/../app-one.git", "ssh://git@gitlab.example/team/--upload-pack=x.git", "ssh://git@gitlab.example/-oProxyCommand=x/a.git",
		"ssh://-oProxyCommand=x@gitlab.example/team/app-one.git", "ssh://git@-oProxyCommand=x/team/app-one.git", "ssh://git@gitlab.example -oProxyCommand=x/team/app-one.git",
		"ssh://git@gitlab.example\n/team/app-one.git", "ssh://git:pw@gitlab.example/team/app-one.git", "ssh://git@gitlab.example:99999/team/app-one.git",
		"ssh://git@gitlab.example:22:22/team/app-one.git", "ssh://git@[::1]/team/app-one.git", "ssh://git@127.0.0.1/team/app-one.git", "",
		" " + good, good + " ", good + "\x00", "ssh://git@gitlab.example/team/app one.git", "ssh://git@gitlab.example/team/app%2Fone.git",
	} {
		err := clone.Clone(context.Background(), opts(remote, filepath.Join(root, "app-one")))
		if err == nil {
			t.Errorf("remote %q was accepted", remote)
		}
	}
	// a pinned-looking host is not enough: it must be in the file, exactly
	for _, remote := range []string{"ssh://git@evil.example/team/app-one.git", "ssh://git@gitlab.example:2222/team/app-one.git", "ssh://git@GITLAB.example/team/app-one.git", "ssh://git@gitlab.example.evil.example/team/app-one.git"} {
		if err := clone.Clone(context.Background(), opts(remote, filepath.Join(root, "app-one"))); err == nil {
			t.Errorf("unpinned remote %q was accepted", remote)
		}
	}
	for _, dest := range []string{
		root, filepath.Join(root, "-x"), filepath.Join(root, "--upload-pack=x"), filepath.Join(root, "a b"), filepath.Join(root, ".."), filepath.Join(root, "."),
		filepath.Join(root, "x", "y"), root + "//app-one", "app-one", "../app-one", filepath.Join(dir, "app-one"), "/etc/app-one", filepath.Join(root, "App"),
		filepath.Join(root, ".clone-abc"), filepath.Join(root, "app-one") + "/", filepath.Join(root, "a;b"), filepath.Join(root, "a$b"), "",
	} {
		if err := clone.Clone(context.Background(), opts(good, dest)); err == nil {
			t.Errorf("destination %q was accepted", dest)
		}
	}
	for _, k := range []string{"-oProxyCommand=x", "relative/key", "/tmp/a b", "/tmp/a;b", "/tmp/a$(id)", "/tmp/a`id`", "/tmp/a'b", "/tmp/a\"b", "/tmp/a\nb", "/tmp/a|b", "/tmp/a&b", "/tmp/a>b", "/tmp/../etc/key", ""} {
		o := opts(good, filepath.Join(root, "app-one"))
		o.SSHKey = k
		if err := clone.Clone(context.Background(), o); err == nil {
			t.Errorf("ssh key path %q was accepted", k)
		}
		o = opts(good, filepath.Join(root, "app-one"))
		o.KnownHosts = k
		if err := clone.Clone(context.Background(), o); err == nil {
			t.Errorf("known_hosts path %q was accepted", k)
		}
		o = opts(good, filepath.Join(root, "app-one"))
		o.SSH = k
		if k != "" {
			if err := clone.Clone(context.Background(), o); err == nil {
				t.Errorf("ssh binary path %q was accepted", k)
			}
		}
	}
	if _, err := os.Stat(marker); err == nil {
		b, _ := os.ReadFile(marker)
		t.Fatalf("git was started by a hostile option: %s", b)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Fatalf("something was created under the root: %v", ents)
	}

	// the API: hostile registry rows (a tampered or newer broker) never reach the helper
	e := newW3cEnv(t)
	type row struct {
		name string
		mod  func(p map[string]any, prov map[string]any)
	}
	rows := []row{}
	for _, proj := range []string{"team/-oProxyCommand=x", "team/app-one --upload-pack=x", "team/app-one\n", "../app-one", "ext::sh -c x", "team/app-one.git",
		"team/app%2Fone", "-team/app-one", "team/app-one/", "/team/app-one", "team//app-one", "team/./app-one", "a/b/c/d/e/app-one", "app-one"} {
		proj := proj
		rows = append(rows, row{"project " + proj, func(p, _ map[string]any) { p["remote"] = map[string]any{"path": proj, "id": 1} }})
	}
	for _, ssh := range []string{"ssh://git@gitlab.example -oProxyCommand=x", "ssh://-oProxyCommand=x@gitlab.example", "ssh://git@-oProxyCommand=x", "ext::sh -c x",
		"file:///etc", "ssh://git@gitlab.example/x", "ssh://git@gitlab.example:22:22", "https://gitlab.example", "ssh://root@gitlab.example", "", "--upload-pack=x"} {
		ssh := ssh
		rows = append(rows, row{"ssh_url " + ssh, func(_, prov map[string]any) { prov["ssh_url"] = ssh }})
	}
	for _, r := range rows {
		prov := map[string]any{"id": "gl", "kind": "gitlab", "base_url": "http://gitlab.example:18082", "ssh_url": "ssh://git@gitlab.example", "token_ref": "gl-admin"}
		p := e.linked("app-one")
		r.mod(p, prov)
		e.wb.mu.Lock()
		e.wb.providers = []map[string]any{prov}
		e.wb.mu.Unlock()
		e.setProjects(p)
		os.RemoveAll(filepath.Join(e.root, "app-one"))
		if code, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); code == 200 {
			t.Errorf("%s: cloned (%s)", r.name, b)
		}
	}
	if n := len(e.cloneCalls()); n != 0 {
		t.Fatalf("the clone helper was called %d times for hostile registry rows: %+v", n, e.cloneCalls())
	}
}

// ---------- resolver ----------

// The publish remote resolver knows only what the registry (read through the
// broker) says and which hosts the pinning file lists. The repository string
// it is handed (which stands for task data) selects a row, it never becomes a
// remote: URLs, traversal and odd spellings find no row. Unpinned hosts and
// rows that are not clean resolve to "refuse"; with the broker gone and the
// table stale nothing is guessed.
func TestResolverNeverTrustsRequestData(t *testing.T) {
	e := newW3cEnv(t)
	good := e.linked("app-one")
	unpinned := e.linked("app-two")
	unpinned["provider"] = "evil"
	badName := e.linked("app-three")
	badName["remote"] = map[string]any{"path": "team/other-name", "id": 3}
	e.wb.mu.Lock()
	e.wb.providers = append(e.wb.providers, map[string]any{"id": "evil", "kind": "gitlab", "base_url": "http://evil.example", "ssh_url": "ssh://git@evil.example", "token_ref": "gl-admin"})
	e.wb.mu.Unlock()
	e.setProjects(good, unpinned, badName)
	r := &clone.Resolver{Client: &broker.Client{Socket: e.d.handler.broker.Socket, Timeout: 2 * time.Second}, KnownHosts: e.known}

	if remote, ok := r.Resolve(good["path"].(string)); !ok || remote != expectedRemote {
		t.Fatalf("control: %q %v", remote, ok)
	}
	for _, p := range []map[string]any{unpinned, badName} {
		if remote, ok := r.Resolve(p["path"].(string)); !ok || remote != "" {
			t.Errorf("%v: remote %q ok=%v, want refuse", p["id"], remote, ok)
		}
	}
	gp := good["path"].(string)
	for _, repo := range []string{
		"ssh://git@evil.example/x/y.git", "git@evil.example:x/y.git", "https://evil.example/x.git", "-oProxyCommand=x", "ext::sh -c id",
		gp + "/", gp + "/..", gp + "/../app-one", filepath.Dir(gp), "app-one", strings.ToUpper(gp), gp + "\x00", gp + "\n", " " + gp, "../" + filepath.Base(gp),
		gp + "/sub", "/etc/passwd", "",
	} {
		if remote, ok := r.Resolve(repo); remote != "" || (ok && repo == "") {
			t.Errorf("repo %q resolved to %q (ok=%v)", repo, remote, ok)
		} else if ok && strings.Contains(remote, "evil") {
			t.Errorf("repo %q: %q", repo, remote)
		}
	}
	// broker gone, table stale: the repository that WAS linked is refused, and so is an unknown one
	r.Invalidate()
	e.wb.setReply(func(string, json.RawMessage) string { return "DROP" })
	for _, repo := range []string{gp, filepath.Join(e.root, "never-seen")} {
		if remote, ok := r.Resolve(repo); remote != "" || !ok {
			t.Errorf("stale table, broker down: %q -> %q ok=%v (want refuse)", repo, remote, ok)
		}
	}
}

// ---------- apply: forged fields ----------

// TestApplyForgedFieldsRefusedBeforeBrokerW3c: between the proposal the user saw and the call to the broker, nothing can
// be added or changed: every forged apply is refused before the broker is
// contacted at all (no read, no write), the deploy key hash is computed by
// the server (a client-supplied one is refused at propose), and a rotated or
// vanished .pub, or a policy without the `registered` class, stops a create.
func TestApplyForgedFieldsRefusedBeforeBrokerW3c(t *testing.T) {
	swapAction := map[string]string{"set_provider": "delete_provider", "delete_provider": "create_group", "create_group": "set_provider", "create_project": "set_provider"}
	forge := map[string]func(m map[string]any){
		"extra top-level key":     func(m map[string]any) { m["deploy_key"] = "ssh-ed25519 AAAA evil" },
		"extra top-level token":   func(m map[string]any) { m["token"] = "x" },
		"extra field":             func(m map[string]any) { m["fields"].(map[string]any)["admin"] = true },
		"subject case":            func(m map[string]any) { m["subject"] = strings.ToUpper(m["subject"].(string)) },
		"issued not a time":       func(m map[string]any) { m["issued"] = "yesterday" },
		"request not an id":       func(m map[string]any) { m["request"] = "../../etc" },
		"action swapped":          func(m map[string]any) { m["action"] = swapAction[m["action"].(string)] },
		"signature with extra":    func(m map[string]any) { m["signature"].(map[string]any)["extra"] = 1 },
		"change of the other one": func(m map[string]any) { m["change"] = strings.Repeat("f", 64) },
	}
	for _, action := range w3cActions {
		for name, mod := range forge {
			e := newW3cEnv(t)
			p := e.proposeAction(t, action)
			m := e.applyBody(p, copySig())
			mod(m)
			code, b := e.sockJSON(t, "POST", e.applyPath(action), jsonBody(m))
			if code == 200 {
				t.Errorf("%s / %s: accepted: %s", action, name, b)
			}
			if ops := e.wb.allOps(); len(ops) != 0 {
				t.Errorf("%s / %s: the broker was contacted: %v", action, name, ops)
			}
		}
	}
	// create_project fields that only the server may compute or the user may not choose
	e := newW3cEnv(t)
	_, f := e.fieldsFor("create_project")
	f["deploy_key_sha256"] = strings.Repeat("a", 64)
	if c, b := e.sockJSON(t, "POST", e.proposePath("create_project"), jsonBody(map[string]any{"action": "create_project", "subject": "app-one", "fields": f})); c != 400 {
		t.Errorf("client-supplied deploy key hash: %d %s", c, b)
	}
	p := e.proposeAction(t, "create_project")
	if p.Fields["deploy_key_sha256"] != sha256Hex(e.pubLine) {
		t.Fatalf("the proposal's key hash is not that of the .pub line: %v", p.Fields["deploy_key_sha256"])
	}
	// .pub rotated or gone, class missing: the apply is refused without a broker call
	line2, _ := sshPubLine(t, "rotated")
	writeFile(t, e.key+".pub", line2+"\n")
	if c, b := e.apply(t, p); c != 409 || v1Code(b) != "deploy_key_mismatch" {
		t.Errorf("rotated key: %d %s", c, b)
	}
	writeFile(t, e.key+".pub", e.pubLine+"\n")
	e.d.handler.policy = func() PolicyView { return PolicyView{Hash: "h", Classes: map[string]string{"docs": "L1"}} }
	if c, b := e.apply(t, p); c != 409 && c != 400 && c != 422 || v1Code(b) != "policy_class_missing" {
		t.Errorf("class missing: %d %s", c, b)
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Errorf("the broker was contacted: %v", ops)
	}
	if n := len(e.cloneCalls()); n != 0 {
		t.Errorf("%d clones", n)
	}
}

func copySig() map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonBody(webauthnSig)), &m); err != nil {
		panic(err)
	}
	return m
}

// ---------- propose: no side effects ----------

// TestProposeNeverWritesBrokerW3c: a proposal, valid or hostile, repeated: stores nothing, writes no ledger
// event, creates nothing under the repo root (no clone, no folder), makes no
// write at the broker and in fact no broker call at all, and leaves the
// deploy key and pinning files untouched.
func TestProposeNeverWritesBrokerW3c(t *testing.T) {
	e := newW3cEnv(t)
	pubBefore, knownBefore := readFile(t, e.key+".pub"), readFile(t, e.known)
	rootBefore, _ := os.ReadDir(e.root)
	for i := 0; i < 4; i++ {
		for _, action := range w3cActions {
			s, f := e.fieldsFor(action)
			e.propose(t, action, s, f)
			// hostile variants: all refused, still no side effect
			bad := map[string]any{}
			for k, v := range f {
				bad[k] = v
			}
			bad["zz"] = "x"
			e.sockJSON(t, "POST", e.proposePath(action), jsonBody(map[string]any{"action": action, "subject": s, "fields": bad}))
			e.sockJSON(t, "POST", e.proposePath(action), jsonBody(map[string]any{"action": action, "subject": "../" + s, "fields": f}))
			e.sockJSON(t, "POST", e.proposePath(action), `{"action":`)
		}
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("a proposal contacted the broker: %v", ops)
	}
	for _, typ := range []string{"provider_change", "project_create", "project_clone", "project_change"} {
		if ev := e.ledgerPayloads(t, typ); len(ev) != 0 {
			t.Fatalf("a proposal wrote %d %s event(s)", len(ev), typ)
		}
	}
	if len(e.cloneCalls()) != 0 || e.changeCount() != 0 {
		t.Fatalf("clones %d, registry-change hook %d", len(e.cloneCalls()), e.changeCount())
	}
	if rootAfter, _ := os.ReadDir(e.root); len(rootAfter) != len(rootBefore) {
		t.Fatalf("the repo root changed: %d -> %d entries", len(rootBefore), len(rootAfter))
	}
	if readFile(t, e.key+".pub") != pubBefore || readFile(t, e.known) != knownBefore {
		t.Fatal("a key or pinning file was touched")
	}
}

// ---------- ledger and log ----------

// After every W3c flow (successes, refusals with hostile broker text, clone
// failures), the ledger and the process log hold no secret, no value and no
// broker or clone text: only actor, action, subject (a name), outcome and a
// fixed code. The chain verifies.
func TestProvidersLedgerNoSecrets(t *testing.T) {
	logs := captureLog(t)
	e := newW3cEnv(t)
	const canary = "CANARY-DESC-한글-W3C"
	_, f := e.fieldsFor("create_project")
	f["description"] = canary
	var reqs []string
	clk := &testClock{}
	e.d.handler.now = clk.now
	runAll := func() {
		clk.advance(2 * time.Hour) // the per-actor hourly limits are not what this test is about
		for _, a := range w3cActions {
			s, ff := e.fieldsFor(a)
			if a == "create_project" {
				ff = f
			}
			os.RemoveAll(filepath.Join(e.root, s)) // a created project leaves its folder; a second create of the name would be refused before the broker
			p := e.propose(t, a, s, ff)
			reqs = append(reqs, p.Request, p.Change, p.Message)
			e.apply(t, p)
		}
	}
	runAll() // success
	// hostile broker text on every op, then clone failures
	for _, code := range []string{"gitlab_refused", "provider_unreachable", "create_partial", "project_name_taken", "deploy_key_mismatch", "bad_signature"} {
		code := code
		e.wb.setReply(func(op string, _ json.RawMessage) string {
			if op == "projects" || op == "providers" || op == "list_groups" {
				return ""
			}
			return brokerErrReply(code)
		})
		runAll()
	}
	e.wb.setReply(nil)
	e.cmu.Lock()
	e.cloneErr = clone.ErrFailed
	e.cmu.Unlock()
	e.setProjects(e.linked("app-one"))
	e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
	raw := readFile(t, policy.OrchLedgerPath(e.home))
	leaks := append([]string{canary, e.pubLine, sha256Hex(e.pubLine), e.key, e.known, "SECRET-BROKER-TEXT", "sk-live", "evil.example", "c2ln", "gl-admin", "gitlab.example", "ssh://", "team/app-one", "newgroup"}, reqs...)
	for _, l := range leaks {
		if l != "" && strings.Contains(raw, l) {
			t.Errorf("%q in the ledger", l)
		}
		if l != "" && l != "gitlab.example" && l != "gl-admin" && strings.Contains(logs.String(), l) {
			t.Errorf("%q in the log", l)
		}
	}
	for _, typ := range []string{"provider_change", "project_create", "project_clone"} {
		for _, pl := range e.ledgerPayloads(t, typ) {
			for k, v := range pl {
				if s, ok := v.(string); ok && len(s) > 80 {
					t.Errorf("%s.%s is a long string (%d bytes)", typ, k, len(s))
				}
			}
		}
	}
	if len(e.ledgerPayloads(t, "project_create")) < 7 || len(e.ledgerPayloads(t, "project_clone")) < 1 {
		t.Fatal("control: the flows did not write the expected events")
	}
}

// ---------- broker down ----------

// With the broker socket missing or dead, the W3c endpoints that need it
// answer a fixed broker_unavailable (no socket name, no connect text), the
// proposals (which need no broker) still work, nothing is cloned, and the rest
// of orchd (tasks, policy, repos, project list) is unaffected.
func TestOrchdWorksWithBrokerDownW3c(t *testing.T) {
	missing := filepath.Join(shortDir(t), "none.sock")
	deadSock := filepath.Join(shortDir(t), "dead.sock")
	ln, err := net.Listen("unix", deadSock)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	for name, sock := range map[string]string{"socket missing": missing, "listener gone": deadSock} {
		sock := sock
		t.Run(name, func(t *testing.T) {
			e := newW3cEnv(t, func(c *Config) { c.BrokerSocket = sock })
			for _, path := range []string{"/api/v1/providers", "/api/v1/providers/gl/groups"} {
				c, b := e.sockJSON(t, "GET", path, "")
				if c != 503 || v1Code(b) != "broker_unavailable" || strings.Contains(string(b), sock) || strings.Contains(string(b), "connect") {
					t.Errorf("%s: %d %s", path, c, b)
				}
			}
			for _, a := range w3cActions {
				p := e.proposeAction(t, a) // no broker needed
				c, b := e.apply(t, p)
				if c != 503 || v1Code(b) != "broker_unavailable" {
					t.Errorf("apply %s: %d %s", a, c, b)
				}
			}
			e.setProjects()
			if c, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); c != 503 || v1Code(b) != "broker_unavailable" {
				t.Errorf("clone: %d %s", c, b)
			}
			if n := len(e.cloneCalls()); n != 0 {
				t.Errorf("%d clones", n)
			}
			// everything else keeps working
			if c, b := e.sockJSON(t, "GET", "/api/v1/policy", ""); c != 200 {
				t.Errorf("policy: %d %s", c, b)
			}
			if c, b := e.sockJSON(t, "GET", "/api/v1/projects", ""); c != 200 {
				t.Errorf("projects: %d %s", c, b)
			}
			if c, b := e.sockJSON(t, "GET", "/api/v1/tasks", ""); c != 200 {
				t.Errorf("tasks: %d %s", c, b)
			}
			if c, b := e.sockJSON(t, "GET", "/api/v1/repos", ""); c != 200 || !strings.Contains(string(b), e.repo) {
				t.Errorf("repos: %d %s", c, b)
			}
			if c, b := e.submit(t, e.repo, "HEAD", "p", "docs", "claude"); c != 201 {
				t.Errorf("submit: %d %s", c, b)
			}
		})
	}
}

// ---------- imports and GitLab HTTP ----------

// The orchd binary links no broker code and no OpenAI code (W3a), and in
// W3c also none of the GitLab client: no package of cmd/orchd's dependency
// closure holds the group/project/deploy-key/protection REST paths or the
// provider address policy, and none but the old draft-MR code of the publish
// package holds the token header. orchd's only way to the new GitLab projects
// is ssh through git with the deploy key.
func TestNoBrokerImportInOrchdW3c(t *testing.T) {
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Skipf("no go tool: %v", err)
	}
	dir := strings.TrimSpace(string(root))
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}|{{.Dir}}{{end}}", "./cmd/orchd")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	seen := 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		imp, pkgDir, ok := strings.Cut(l, "|")
		if !ok || !strings.HasPrefix(imp, "aigate/") {
			continue
		}
		seen++
		if imp == "aigate/internal/broker" || strings.HasPrefix(imp, "aigate/internal/broker/") || strings.HasPrefix(imp, "aigate/cmd/orch-broker") {
			t.Errorf("cmd/orchd depends on %s", imp)
		}
		ents, _ := os.ReadDir(pkgDir)
		for _, en := range ents {
			if en.IsDir() || !strings.HasSuffix(en.Name(), ".go") || strings.HasSuffix(en.Name(), "_test.go") {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(pkgDir, en.Name()))
			bads := []string{"/api/v4/groups", "deploy_keys", "protected_branches", "repository/files", "personal_access_tokens", "ipForbidden", "169.254.169.254"}
			// the pre-W3c draft-MR path of the publish package (its own opt-in
			// --gitlab-* flags, a merge-request POST only) is the one older user
			// of the token header and of /api/v4/projects; W3d moves it to the broker
			if imp != "aigate/internal/orch/publish" {
				bads = append(bads, "PRIVATE-TOKEN", "/api/v4/")
			}
			for _, bad := range bads {
				if strings.Contains(string(b), bad) {
					t.Errorf("%s/%s (linked into orchd) contains %q", imp, en.Name(), bad)
				}
			}
		}
	}
	if seen < 8 {
		t.Fatalf("control: only %d aigate packages listed", seen)
	}
}

// ---------- the address shown to the signer ----------

// Re-pointing an existing provider is a signed change whose confirmation text
// the user reads: the proposal carries the new address in `fields`, the
// `change` hash covers it, and the preview sentences name it.
func TestProviderBaseURLChangeShownInPreview(t *testing.T) {
	e := newW3cEnv(t)
	_, f := e.fieldsFor("set_provider")
	f["base_url"] = "http://10.9.9.9:18082"
	p := e.propose(t, "set_provider", "gl", f)
	if p.Fields["base_url"] != "http://10.9.9.9:18082" || !strings.Contains(strings.Join(p.Preview, "\n"), "10.9.9.9:18082") {
		t.Fatalf("the new address is not shown: %+v", p)
	}
	_, old := e.fieldsFor("set_provider")
	po := e.propose(t, "set_provider", "gl", old)
	if po.Change == p.Change {
		t.Fatal("the change hash does not cover base_url")
	}
	if !strings.Contains(p.Message, p.Change) {
		t.Fatal("the message to sign does not carry the change")
	}
}
