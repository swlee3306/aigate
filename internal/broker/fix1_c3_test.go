package broker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aigate/internal/registrymsg"
)

// Fix round 1 of the C3 review.

func (c *c3env) callOn(s *Server, op string, args map[string]any) *OpError {
	c.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		c.t.Fatal(err)
	}
	_, e := s.Handlers[op](context.Background(), raw)
	return e
}

func (c *c3env) seamServer(mod func(*Server)) *Server {
	return c.restart(func(s *Server) {
		s.apiSeams = &apiSeams{policy: allowLoopback}
		if mod != nil {
			mod(s)
		}
	})
}

// 1: the store checks come BEFORE the signature, so a refusal burns no ID.
func TestStoreUsableCheckedBeforeIDBurn(t *testing.T) {
	c := newC3(t)
	setArgs := c.setProviderArgs("gl2", c.gl.URL)
	projArgs := c.projectArgs(c3Name, nil)
	usedBefore := c.usedFile()

	state := filepath.Join(c.e.Dir, "state")
	regPath := filepath.Join(state, RegistryFileName)
	good, _ := os.ReadFile(regPath)
	if err := os.WriteFile(regPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := c.seamServer(nil)
	for op, a := range map[string]map[string]any{"set_provider": setArgs, "create_project": projArgs} {
		if e := c.callOn(s2, op, a); codeOf(e) != "registry_unavailable" {
			t.Fatalf("%s with a broken registry: %v", op, e)
		}
	}
	if string(c.usedFile()) != string(usedBefore) {
		t.Fatal("a request ID was burnt although the registry is broken")
	}
	c.noGLHits("broken registry")
	os.WriteFile(regPath, good, 0o600)

	// broken token store: set_provider says tokens_unavailable, ID unburnt
	tokPath := filepath.Join(state, TokensFileName)
	if err := os.WriteFile(tokPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s3 := c.seamServer(nil)
	if e := c.callOn(s3, "set_provider", setArgs); codeOf(e) != "tokens_unavailable" {
		t.Fatalf("set_provider with a broken token store: %v", e)
	}
	if string(c.usedFile()) != string(usedBefore) {
		t.Fatal("a request ID was burnt although the token store is broken")
	}
}

// 2: path_not_allowed and deploy_key_mismatch refuse before the ID is burnt.
func TestPathAndKeyMismatchBurnNoID(t *testing.T) {
	c := newC3(t)
	before := c.snap()
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherKeyOtherKeyOtherKeyOtherKeyOtherKey orchd"
	c.wantCode("create_project", copyArgs(c.projectArgs(c3Name, nil), func(m map[string]any) { m["deploy_key"] = other }), "deploy_key_mismatch")
	c.mustEqual(before, "deploy_key_mismatch")
	c.noGLHits("deploy_key_mismatch")

	s2 := c.seamServer(func(s *Server) { s.Config.RepoRoots = nil })
	if e := c.callOn(s2, "create_project", c.projectArgs(c3Name, nil)); codeOf(e) != "path_not_allowed" {
		t.Fatalf("no repo root: %v", e)
	}
	c.mustEqual(before, "path_not_allowed")
	c.noGLHits("path_not_allowed")
}

// 3: a panic after POST /projects leaves an orphan with the current step.
func TestPanicAfterProjectWriteLeavesOrphan(t *testing.T) {
	c := newC3(t)
	n := 0
	c.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			n++
			if n == 1 { // the project entry write (step register)
				panic(c3Canary)
			}
		}
		return nil
	}
	r := c.wire("create_project", c.projectArgs(c3Name, nil))
	if r.Code != "create_partial" {
		t.Fatalf("panic reply: %+v", r)
	}
	os := c.orphans()
	if len(os) != 1 || os[0].Step != "register" || os[0].ProjectID == nil || *os[0].ProjectID != 21 {
		t.Fatalf("orphans: %+v", os)
	}
	if _, ok := c.find(c3Name); ok {
		t.Fatal("registered after a panic")
	}
	if strings.Contains(c.logs.String(), c3Canary) || strings.Contains(jsonOf(t, r), c3Canary) {
		t.Fatal("canary in log or reply")
	}
	// the name lock is released: the next attempt is refused by GitLab's existence check, not stuck
	if !c.s.lockName(c3Name) {
		t.Fatal("name lock still held after the panic")
	}
	c.s.unlockName(c3Name)
}

// 4: the provider must be unchanged when the entry is registered.
func TestProviderChangedDuringCreateIsRegisterFailure(t *testing.T) {
	c := newC3(t)
	var once sync.Once
	c.gl.mu.Lock()
	c.gl.onReq = func(r glReq) {
		if r.Method == "POST" && strings.HasSuffix(r.URI, "/deploy_keys") {
			once.Do(func() {
				p, _ := c.s.Registry.ProviderByID("gl")
				p.TokenRef = "other-token"
				if err := c.s.Registry.Update(func(reg *Registry) error { reg.Providers[findProvider(reg, "gl")] = p; return nil }); err != nil {
					t.Errorf("update: %v", err)
				}
			})
		}
	}
	c.gl.mu.Unlock()
	_, e := c.do("create_project", c.projectArgs(c3Name, nil))
	d := partialOf(t, e)
	if d.Step != "register" || !d.OrphanRecorded {
		t.Fatalf("detail: %+v", d)
	}
	if _, ok := c.find(c3Name); ok {
		t.Fatal("registered under a changed provider")
	}
}

// 5: the quota is spent only just before the first GitLab write.
func TestQuotaSpentOnlyBeforeFirstWrite(t *testing.T) {
	c := newC3(t)
	c.gl.existP["team/taken"] = true
	for i := 0; i < 12; i++ {
		c.wantCode("create_project", c.projectArgs("taken", nil), "project_name_taken")
		c.wantCode("create_project", c.projectArgs("lost", func(f map[string]any) { f["group_path"] = "nowhere" }), "group_unknown")
		c.wantCode("create_group", c.groupArgs("newg", "team", ""), "group_exists")
	}
	for i := 0; i < 10; i++ {
		name := "proj-" + string(rune('a'+i))
		c.gl.mu.Lock()
		c.gl.created = ""
		c.gl.mu.Unlock()
		if _, e := c.do("create_project", c.projectArgs(name, nil)); e != nil {
			t.Fatalf("%s: quota was consumed by refusals: %v", name, e.Code)
		}
		gname := "grp-" + string(rune('a'+i))
		if _, e := c.do("create_group", c.groupArgs(gname, gname, "")); e != nil {
			t.Fatalf("%s: group quota consumed by refusals: %v", gname, e.Code)
		}
	}
	c.wantCode("create_project", c.projectArgs("proj-z", nil), "rate_limited")
	c.wantCode("create_group", c.groupArgs("grp-z", "grp-z", ""), "rate_limited")
}

// 6: a registry write that failed only on the directory fsync kept the state:
// the project IS registered, so the create is a success.
func TestRegisterDirFsyncOnlyFailureIsSuccess(t *testing.T) {
	c := newC3(t)
	n := 0
	c.s.Registry.opts.failAt = func(stage string) error {
		if stage == "dir-synced" {
			n++
			if n == 1 {
				return errors.New("injected")
			}
		}
		return nil
	}
	res, e := c.do("create_project", c.projectArgs(c3Name, nil))
	if e != nil {
		t.Fatalf("dir-fsync-only failure: %v", e.Code)
	}
	if _, ok := c.find(c3Name); !ok || len(c.orphans()) != 0 {
		t.Fatalf("project present=%v orphans=%v", ok, c.orphans())
	}
	if !strings.Contains(jsonOf(t, res), `"clone_url"`) {
		t.Fatalf("result: %s", jsonOf(t, res))
	}
	if !strings.Contains(c.logs.String(), "fsync") {
		t.Fatalf("the fsync problem is not logged: %q", c.logs.String())
	}
	_ = registrymsg.ActionCreateProject
}
