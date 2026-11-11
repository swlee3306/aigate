package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/clone"
	"aigate/internal/orch/policy"
)

func TestIsClonedNeverCountsStagingDirs(t *testing.T) {
	e := newW3cEnv(t)
	stage := filepath.Join(e.root, ".clone-abc123")
	os.MkdirAll(filepath.Join(stage, ".git"), 0o755)
	if e.d.handler.isCloned(stage) {
		t.Fatal("a half-cloned staging dir counts as cloned")
	}
	real := filepath.Join(e.root, "done-one")
	os.MkdirAll(filepath.Join(real, ".git"), 0o755)
	if !e.d.handler.isCloned(real) {
		t.Fatal("control: a real clone does not count")
	}
	// Nor is it offered as a repository, a candidate or a registrable path.
	mkRepo(t, filepath.Join(e.root, ".clone-gitrepo"))
	for _, path := range []string{"/api/v1/repos", "/api/v1/projects/candidates"} {
		code, b := e.sockJSON(t, "GET", path, "")
		if code != 200 || strings.Contains(string(b), ".clone-") {
			t.Errorf("%s lists a staging dir: %d %s", path, code, b)
		}
	}
	if e.d.handler.directUnderRoot(filepath.Join(e.root, ".clone-gitrepo")) {
		t.Error("a staging dir is a registrable path")
	}
}

func TestStartupSweepsStaleStagingDirs(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("k"), 0o644)
	var root string
	newW3cEnv(t, func(c *Config) {
		root = c.AllowedRepos[0]
		os.MkdirAll(filepath.Join(root, ".clone-crashed", ".git"), 0o755)
		os.Symlink(outside, filepath.Join(root, ".clone-link"))
		os.MkdirAll(filepath.Join(root, "proj-keep"), 0o755)
	})
	if _, err := os.Lstat(filepath.Join(root, ".clone-crashed")); err == nil {
		t.Error("the leftover staging dir survived startup")
	}
	for _, k := range []string{".clone-link", "proj-keep"} {
		if _, err := os.Lstat(filepath.Join(root, k)); err != nil {
			t.Errorf("%s was removed: %v", k, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "keep")); string(b) != "k" {
		t.Error("the sweep followed a symlink")
	}
}

func TestCloneLimitAndRefund(t *testing.T) {
	if CloneProjectPerActorHour != 10 {
		t.Fatal("documented cap changed")
	}
	e := newW3cEnv(t)
	clk := &testClock{}
	e.d.handler.now = clk.now
	e.setProjects(e.linked("app-one"))
	e.cmu.Lock()
	e.cloneErr = clone.ErrFailed // git ran and failed: the slot is spent
	e.cmu.Unlock()
	for i := 0; i < CloneProjectPerActorHour; i++ {
		if c, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); c != 502 {
			t.Fatalf("try %d: %d %s", i+1, c, b)
		}
	}
	before := len(e.cloneCalls())
	c, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
	if c != 429 || v1Code(b) != "project_rate_limited" || len(e.cloneCalls()) != before {
		t.Fatalf("%d %s", c, b)
	}
	other := e.tokenCallerW3c(t, "mac-ssh")
	if c, b := other("POST", "/api/v1/projects/app-one/clone", ""); c != 502 {
		t.Fatalf("second actor: %d %s", c, b)
	}
	clk.set(time.Hour + time.Minute)
	if c, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); c != 502 {
		t.Fatalf("after an hour: %d %s", c, b)
	}
	// Anything that stopped before git ran gives the slot back.
	e2 := newW3cEnv(t)
	for i := 0; i < CloneProjectPerActorHour+3; i++ {
		if c, _ := e2.sockJSON(t, "POST", "/api/v1/projects/nope-x/clone", ""); c != 404 {
			t.Fatalf("unknown project try %d: %d", i+1, c)
		}
	}
	e2.setProjects(func() map[string]any { m := e2.linked("app-one"); m["provider"] = nil; return m }())
	for i := 0; i < CloneProjectPerActorHour+3; i++ {
		if c, _ := e2.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); c != 400 {
			t.Fatalf("not clonable try %d: %d", i+1, c)
		}
	}
	dead := newW3cEnv(t, func(c *Config) { c.BrokerSocket = filepath.Join(shortDir(t), "none.sock") })
	for i := 0; i < CloneProjectPerActorHour+3; i++ {
		if c, _ := dead.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); c != 503 {
			t.Fatalf("dead broker try %d: %d", i+1, c)
		}
	}
}

func TestLedgerProjectCloneEventsNoSecrets(t *testing.T) {
	e := newW3cEnv(t)
	e.setProjects(e.linked("app-one"))
	e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "") // cloned
	e.cmu.Lock()
	e.cloneErr = clone.ErrHostNotPinned
	e.cmu.Unlock()
	os.RemoveAll(filepath.Join(e.root, "app-one"))
	e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "") // failed
	e.sockJSON(t, "POST", "/api/v1/projects/nope-x/clone", "")  // refused before git: no event
	// the automatic clone after a create is recorded too
	e.cmu.Lock()
	e.cloneErr = nil
	e.cmu.Unlock()
	e.apply(t, e.proposeAction(t, "create_project"))
	ev := e.ledgerPayloads(t, "project_clone")
	if len(ev) != 3 {
		t.Fatalf("%d project_clone events: %v", len(ev), ev)
	}
	for _, pl := range ev {
		if ledgerKeys(pl) != "actor,code,ok,subject" || pl["actor"] != socketActor() || pl["subject"] != "app-one" {
			t.Fatalf("payload %v", pl)
		}
	}
	if ev[0]["ok"] != true || ev[0]["code"] != "ok" || ev[1]["ok"] != false || ev[1]["code"] != "host_not_pinned" || ev[2]["ok"] != true {
		t.Fatalf("%v", ev)
	}
	all, _ := json.Marshal(ev)
	all = append(all, readFile(t, policy.OrchLedgerPath(e.home))...)
	for _, leak := range []string{"gitlab.example", "ssh://", "team/app-one", e.root, e.key, e.known, "id_gitlab"} {
		if strings.Contains(string(all), leak) {
			t.Errorf("%q in a project_clone event", leak)
		}
	}
}

func TestGitAndSSHResolvedOnceAndMissingHasOwnCodes(t *testing.T) {
	e := newW3cEnv(t)
	e.setProjects(e.linked("app-one"))
	if c, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", ""); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	o := e.cloneCalls()[0]
	if o.Git != w3cFakeGit || o.SSH != w3cFakeSSH {
		t.Fatalf("clone did not get the startup-resolved tools: %+v", o)
	}
	for name, tc := range map[string]struct{ git, ssh, code string }{
		"no git": {"", w3cFakeSSH, "git_missing"},
		"no ssh": {w3cFakeGit, "", "ssh_missing"},
	} {
		e := newW3cEnv(t)
		e.d.handler.gitPath, e.d.handler.sshPath = tc.git, tc.ssh
		e.setProjects(e.linked("app-one"))
		c, b := e.sockJSON(t, "POST", "/api/v1/projects/app-one/clone", "")
		if c != 503 || v1Code(b) != tc.code || len(e.cloneCalls()) != 0 || v1Code(b) == "clone_failed" {
			t.Errorf("%s: %d %s", name, c, b)
		}
		// the automatic clone reports the same fixed code, the create stands
		c, b = e.apply(t, e.proposeAction(t, "create_project"))
		var out cloneOut
		json.Unmarshal(b, &out)
		if c != 200 || out.Clone.Status != "failed" || out.Clone.Code != tc.code {
			t.Errorf("%s auto: %d %s", name, c, b)
		}
	}
	// startup resolves them from the fixed directories only
	d := newEnv(t, noSteps{}, 1, func(c *Config) {})
	if d.d.handler.gitPath != clone.FindTool("git") || d.d.handler.sshPath != clone.FindTool("ssh") {
		t.Errorf("startup tools: %q %q", d.d.handler.gitPath, d.d.handler.sshPath)
	}
}
