package broker

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"aigate/internal/registrymsg"
)

// W3e (E3): link_project and attach_remote.

func TestLinkProjectExistingOnly(t *testing.T) {
	e := newE3(t)
	e.existing()
	before := e.regFile()
	a := e.linkArgs("linked", "gh", "alice/missing", true, nil)
	e.wantCode("link_project", a, "repo_not_found")
	if got := e.trace(); !reflect.DeepEqual(got, []string{"GET /repos/alice/missing"}) {
		t.Fatalf("trace: %v", got)
	}
	if len(e.ghWrites()) != 0 || len(e.orphans()) != 0 || string(before) != string(e.regFile()) {
		t.Fatal("something was written for a repository that does not exist")
	}
	// the signature was valid: the ID is burnt
	e.wantCode("link_project", a, "request_reused")
	// the same for GitLab
	g := e.linkArgs("linked", "gl", "team/missing", true, nil)
	e.wantCode("link_project", g, "repo_not_found")
	for _, r := range e.gl.writes() {
		t.Fatalf("GitLab write: %v", r)
	}
	// only a 404 means absent: every other failure is a provider error, nothing written
	for _, tc := range []struct {
		status int
		code   string
	}{{500, "provider_unreachable"}, {401, "github_refused"}, {403, "github_refused"}, {422, "github_refused"}, {429, "provider_rate_limited"}} {
		e2 := newE3(t)
		e2.existing()
		e2.gh.fail["GET /repos/alice/existing"] = tc.status
		e2.wantCode("link_project", e2.linkArgs("linked", "gh", "alice/existing", true, nil), tc.code)
		if len(e2.ghWrites()) != 0 || len(e2.orphans()) != 0 {
			t.Fatalf("%d: written %v %v", tc.status, e2.trace(), e2.orphans())
		}
	}
	e3 := newE3(t)
	e3.existing()
	e3.gl.fail["GET /api/v4/projects/"] = 403
	e3.wantCode("link_project", e3.linkArgs("linked", "gl", "team/exist", true, nil), "gitlab_refused")
	if len(e3.gl.writes()) != 0 {
		t.Fatal("GitLab write after a refused read")
	}
}

func TestLinkProjectNeverCreatesOrDeletes(t *testing.T) {
	e := newE3(t)
	e.existing()
	if _, oe := e.do("link_project", e.linkArgs("linked", "gh", "alice/existing", true, nil)); oe != nil {
		t.Fatalf("link github: %v", oe.Code)
	}
	// one read of the remote, one key: nothing is created, protected, committed or deleted
	want := []string{"GET /repos/alice/existing", "POST /repos/alice/Existing/keys"}
	if got := e.trace(); !reflect.DeepEqual(got, want) {
		t.Fatalf("github trace %v, want %v", got, want)
	}
	k := e.bodyAt(1)
	if k["title"] != "orchd linked" || k["key"] != c3Key() || k["read_only"] != false {
		t.Fatalf("key body: %v", k)
	}
	if _, oe := e.do("link_project", e.linkArgs("linked-gl", "gl", "team/exist", false, func(f map[string]any) { f["publish"] = "read" })); oe != nil {
		t.Fatalf("link gitlab: %v", oe.Code)
	}
	if got := e.gl.trace(); !reflect.DeepEqual(got, []string{"GET /projects/team%2Fexist", "POST /projects/21/deploy_keys"}) {
		t.Fatalf("gitlab trace: %v", got)
	}
	if w := e.gl.writes(); len(w) != 1 || !strings.Contains(w[0].Body, `"can_push":false`) {
		t.Fatalf("gitlab writes: %v", w)
	}
	e.noDestructive()
}

func TestLinkProjectRegistersANewProject(t *testing.T) {
	e := newE3(t)
	e.existing()
	res, oe := e.do("link_project", e.linkArgs("linked", "gh", "Alice/EXISTING", true, nil))
	if oe != nil {
		t.Fatalf("link: %v", oe.Code)
	}
	p, ok := e.find("linked")
	if !ok || p.Path != e.root+"/linked" || p.Kind != "registered" || !p.AIContext || p.Provider == nil || *p.Provider != "gh" ||
		p.Remote == nil || p.Remote.Path != "alice/Existing" /* GitHub's spelling */ || p.Remote.ID == nil || *p.Remote.ID != 31 {
		t.Fatalf("entry: %+v", p)
	}
	if p.AutoMerge.Enabled {
		t.Fatal("auto merge on")
	}
	m := res.(map[string]any)
	if m["clone_url"] != "ssh://git@github.com/alice/Existing.git" || m["cloned_hint"] != false || m["ssh_url"] != "ssh://git@github.com" {
		t.Fatalf("result: %v", m)
	}
	// ai_context false is kept false
	if _, oe := e.do("link_project", e.linkArgs("linked-gl", "gl", "team/exist", false, nil)); oe != nil {
		t.Fatal(oe.Code)
	}
	if p, _ := e.find("linked-gl"); p.AIContext || p.Remote == nil || p.Remote.ID == nil || *p.Remote.ID != 21 {
		t.Fatalf("gitlab entry: %+v", p)
	}
	if len(e.orphans()) != 0 {
		t.Fatal("orphans on success")
	}
}

func TestAttachRemoteSetsProviderAndRemoteOnly(t *testing.T) {
	for _, ai := range []bool{false, true} {
		e := newE3(t)
		e.existing()
		e.ownLocal("legacy-app", ai, nil)
		before, _ := e.find("legacy-app")
		res, oe := e.do("attach_remote", e.attachArgs("legacy-app", "gh", "alice/existing", nil))
		if oe != nil {
			t.Fatalf("attach: %v", oe.Code)
		}
		after, _ := e.find("legacy-app")
		if after.Provider == nil || *after.Provider != "gh" || after.Remote == nil || after.Remote.Path != "alice/Existing" || after.Remote.ID == nil || *after.Remote.ID != 31 {
			t.Fatalf("not attached: %+v", after)
		}
		// everything else is exactly what it was: never widened, never narrowed
		after.Provider, after.Remote = nil, nil
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("attach changed more than provider and remote:\n before %+v\n after  %+v", before, after)
		}
		if after.AIContext != ai {
			t.Fatalf("ai_context %v -> %v", ai, after.AIContext)
		}
		if res.(map[string]any)["project"] == nil {
			t.Fatal("no project in the result")
		}
		want := []string{"GET /repos/alice/existing", "POST /repos/alice/Existing/keys"}
		if got := e.trace(); !reflect.DeepEqual(got, want) {
			t.Fatalf("trace %v", got)
		}
		e.noDestructive()
	}
	// there is no way to ask for ai_context: the wire has no such key
	e := newE3(t)
	e.existing()
	e.ownLocal("legacy-app", false, nil)
	before := e.snap()
	a := e.attachArgs("legacy-app", "gh", "alice/existing", nil)
	a["ai_context"] = true
	e.wantCode("attach_remote", a, "bad_request")
	e.mustEqual(before, "attach with ai_context")
	e.noGHHits("attach with ai_context")
	// an unknown project is refused before the signature is consumed
	e.wantCode("attach_remote", e.attachArgs("ghost-app", "gh", "alice/existing", nil), "project_unknown")
	e.mustEqual(before, "attach unknown project")
	// a project with a legacy remote mapping (broker.json) is never adopted: same
	// repository, another repository, another kind or provider, all refused in the shape stage
	e.ownLocal("mapped", true, &RemoteLink{Path: "team/exist"})
	before = e.snap()
	for _, a := range [][2]string{{"gl", "team/exist"}, {"gl", "team/other"}, {"gh", "alice/existing"}, {"gh", "team/exist"}} {
		args := e.attachArgs("mapped", a[0], a[1], nil)
		e.wantCode("attach_remote", args, "already_linked")
		e.wantCode("attach_remote", args, "already_linked") // never request_reused
	}
	e.mustEqual(before, "attach to a mapped project")
	if g, ok := e.s.Registry.GitLabProject(e.root + "/mapped"); !ok || g != "team/exist" {
		t.Fatalf("the create_mr mapping changed: %q %v", g, ok)
	}
}

// The static --git-remote-for entries are orchd's: the broker knows only the
// registry rule (a project without a provider link can be attached).
func linkPartialOf(t *testing.T, e *OpError) PartialDetail {
	t.Helper()
	if e == nil || e.Code != "link_partial" {
		t.Fatalf("want link_partial, got %v", e)
	}
	d, ok := e.Data.(PartialDetail)
	if !ok || d.Origin != OriginLinked {
		t.Fatalf("link_partial detail: %T %+v", e.Data, e.Data)
	}
	return d
}

func TestAttachRemoteStaticWins(t *testing.T) {
	if _, known := fixedMessages["static_remote_wins"]; known {
		t.Fatal("the broker must not know static_remote_wins (orchd's refusal)")
	}
	e := newE3(t)
	e.existing()
	e.ownLocal("sample-app", false, nil) // a project orchd may have a static remote for: the broker cannot tell
	if _, oe := e.do("attach_remote", e.attachArgs("sample-app", "gh", "alice/existing", nil)); oe != nil {
		t.Fatalf("registry rule only: %v", oe.Code)
	}
}

// The W3c table row "burned after a valid signature", for both new actions: a
// refusal after the signature was verified (here: the remote is absent) has
// consumed the request ID.
func TestLinkAttachRequestIDBurnedAfterValidSignature(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.ownLocal("plain", false, nil)
	a := e.attachArgs("plain", "gh", "alice/missing", nil)
	e.wantCode("attach_remote", a, "repo_not_found")
	e.wantCode("attach_remote", a, "request_reused")
	l := e.linkArgs("linked", "gh", "alice/missing", true, nil)
	e.wantCode("link_project", l, "repo_not_found")
	e.wantCode("link_project", l, "request_reused")
	if p, _ := e.find("plain"); p.Provider != nil {
		t.Fatal("attached")
	}
}

func TestAlreadyLinked(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.ownLinked("linked-a", "gh", "alice/Existing")
	e.ownLinked("linked-g", "gl", "team/exist")
	e.ownLocal("plain", false, nil)
	e.ownLocal("plain-two", false, nil)
	before := e.snap()
	for _, tc := range []struct {
		op   string
		args map[string]any
		want string
	}{
		// attach to a project that already has a provider link
		{"attach_remote", e.attachArgs("linked-a", "gh", "alice/existing", nil), "already_linked"},
		{"attach_remote", e.attachArgs("linked-g", "gl", "team/exist", nil), "already_linked"},
		// link with an id that is already provider-linked
		{"link_project", e.linkArgs("linked-a", "gh", "alice/existing", true, nil), "already_linked"},
		// the same remote, linked by ANOTHER project: GitHub names fold case, GitLab's do not need to
		{"link_project", e.linkArgs("fresh", "gh", "ALICE/existing", true, nil), "already_linked"},
		{"link_project", e.linkArgs("fresh", "gh", "alice/EXISTING", true, nil), "already_linked"},
		{"link_project", e.linkArgs("fresh", "gl", "team/exist", true, nil), "already_linked"},
		{"attach_remote", e.attachArgs("plain", "gh", "Alice/Existing", nil), "already_linked"},
		{"attach_remote", e.attachArgs("plain", "gl", "team/exist", nil), "already_linked"},
		// link onto an id or path that is a registered project without a link
		{"link_project", e.linkArgs("plain", "gh", "alice/other", true, nil), "project_exists"},
		// attach to nothing
		{"attach_remote", e.attachArgs("nothing-here", "gh", "alice/other", nil), "project_unknown"},
	} {
		e.wantCode(tc.op, tc.args, tc.want)
		e.wantCode(tc.op, tc.args, tc.want) // refused before the signature is consumed: not request_reused
	}
	e.mustEqual(before, "already linked")
	e.noGHHits("already linked")
	e.noGLHits("already linked")
	// the same remote on a DIFFERENT provider id is another repository
	if err := e.s.AddProviderChecked(Provider{ID: "gh2", Kind: "github", BaseURL: e.s.mustProvider("gh").BaseURL, SSHURL: "ssh://git@github.com", TokenRef: "gh-tok"}); err != nil {
		t.Fatal(err)
	}
	if _, oe := e.do("link_project", e.linkArgs("elsewhere", "gh2", "alice/existing", true, nil)); oe != nil {
		t.Fatalf("same path on another provider id: %v", oe.Code)
	}
	// two concurrent-looking links of the same remote: the second is already_linked
	e2 := newE3(t)
	e2.existing()
	if _, oe := e2.do("link_project", e2.linkArgs("first-one", "gh", "alice/existing", true, nil)); oe != nil {
		t.Fatal(oe.Code)
	}
	w := len(e2.gh.recorded())
	e2.wantCode("link_project", e2.linkArgs("second-one", "gh", "alice/existing", true, nil), "already_linked")
	if len(e2.gh.recorded()) != w {
		t.Fatal("GitHub was called for an already linked remote")
	}
}

func (s *Server) mustProvider(id string) Provider {
	p, _ := s.Registry.ProviderByID(id)
	return p
}

func TestLinkKeyAlreadyInUseNotSuccess(t *testing.T) {
	// GitHub answers 422 for a key that is already in use: a refusal, not a link
	e := newE3(t)
	e.existing()
	e.gh.fail["POST /repos/alice/Existing/keys"] = 422
	r := e.wire("link_project", e.linkArgs("linked", "gh", "alice/existing", true, nil))
	if r.Code != "github_refused" || r.Message != fixedMessages["github_refused"] || strings.Contains(r.Message, "REFLECT") {
		t.Fatalf("%+v", r)
	}
	if _, ok := e.find("linked"); ok || len(e.orphans()) != 0 {
		t.Fatalf("registered or orphaned after a refused key: %v", e.orphans())
	}
	// attach: the same
	e.ownLocal("plain", false, nil)
	e.wantCode("attach_remote", e.attachArgs("plain", "gh", "alice/existing", nil), "github_refused")
	if p, _ := e.find("plain"); p.Provider != nil || p.Remote != nil {
		t.Fatalf("attached after a refused key: %+v", p)
	}
	// GitLab says 400 ("has already been taken") for the same key on another project
	e2 := newE3(t)
	e2.existing()
	e2.gl.fail["POST /api/v4/projects/21/deploy_keys"] = 400
	e2.wantCode("link_project", e2.linkArgs("linked", "gl", "team/exist", true, nil), "gitlab_refused")
	if _, ok := e2.find("linked"); ok || len(e2.orphans()) != 0 {
		t.Fatal("registered or orphaned after a refused GitLab key")
	}
	// a rate limit is not an accepted key either
	e3 := newE3(t)
	e3.existing()
	e3.gh.fail["POST /repos/alice/Existing/keys"] = 403
	e3.gh.failHeader = map[string]string{"X-RateLimit-Remaining": "0"}
	e3.wantCode("link_project", e3.linkArgs("linked", "gh", "alice/existing", true, nil), "provider_rate_limited")
	// a 201 that does not show the permission we asked for is NOT a clean success: the key may
	// be attached with the wrong permission, so it is an orphan (uncertain outcome)
	e4 := newE3(t)
	e4.existing()
	e4.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys") {
			w.WriteHeader(201)
			w.Write([]byte(`{"id":5,"read_only":true}`)) // asked for write
			return true
		}
		return false
	}
	_, oe := e4.do("link_project", e4.linkArgs("linked", "gh", "alice/existing", true, nil))
	if d := linkPartialOf(t, oe); d.Step != "deploy_key" || !d.OrphanRecorded {
		t.Fatalf("%+v", d)
	}
	if _, ok := e4.find("linked"); ok {
		t.Fatal("registered")
	}
}

func TestLinkAttachKeyStepUncertainIsOrphan(t *testing.T) {
	for _, op := range []string{"link_project", "attach_remote"} {
		for _, st := range []int{500, 502, 302} {
			e := newE3(t)
			e.existing()
			e.ownLocal("plain", true, nil)
			e.gh.fail["POST /repos/alice/Existing/keys"] = st
			var args map[string]any
			if op == "link_project" {
				args = e.linkArgs("linked", "gh", "alice/existing", true, nil)
			} else {
				args = e.attachArgs("plain", "gh", "alice/existing", nil)
			}
			_, oe := e.do(op, args)
			d := linkPartialOf(t, oe)
			if d.Step != "deploy_key" || !d.OrphanRecorded || d.ProjectID == nil || *d.ProjectID != 31 || d.Project != "alice/Existing" || d.Provider != "gh" {
				t.Fatalf("%s %d: %+v", op, st, d)
			}
			if os := e.orphans(); len(os) != 1 || os[0].Step != "deploy_key" {
				t.Fatalf("orphans %+v", os)
			}
			if _, ok := e.find("linked"); ok {
				t.Fatal("registered")
			}
			if p, _ := e.find("plain"); p.Provider != nil {
				t.Fatal("attached")
			}
			e.noDestructive()
		}
	}
}

func TestLinkAttachRegistryFailureIsOrphan(t *testing.T) {
	for _, op := range []string{"link_project", "attach_remote"} {
		mk := func(e *e3env) map[string]any {
			if op == "link_project" {
				return e.linkArgs("linked", "gh", "alice/existing", true, nil)
			}
			e.ownLocal("plain", true, nil)
			return e.attachArgs("plain", "gh", "alice/existing", nil)
		}
		linked := func(e *e3env) bool {
			id := "linked"
			if op == "attach_remote" {
				id = "plain"
			}
			p, ok := e.find(id)
			return ok && p.Provider != nil
		}
		// the write of the entry fails: the key IS attached, so an orphan with step register
		e := newE3(t)
		e.existing()
		args := mk(e)
		n := 0
		e.s.Registry.opts.failAt = func(stage string) error {
			if stage == "temp-written" {
				if n++; n == 1 {
					return errors.New("injected")
				}
			}
			return nil
		}
		_, oe := e.do(op, args)
		d := linkPartialOf(t, oe)
		if d.Step != "register" || d.ProjectID == nil || *d.ProjectID != 31 || !d.OrphanRecorded {
			t.Fatalf("%s: %+v", op, d)
		}
		if linked(e) {
			t.Fatalf("%s: registered although the write failed", op)
		}
		if os := e.orphans(); len(os) != 1 || os[0].Step != "register" {
			t.Fatalf("%s: orphans %+v", op, os)
		}
		if e.indexOf("POST /repos/alice/Existing/keys") < 0 {
			t.Fatal("the key step did not run")
		}
		// directory-fsync-only failure keeps the state: linked, no orphan
		e2 := newE3(t)
		e2.existing()
		args = mk(e2)
		m := 0
		e2.s.Registry.opts.failAt = func(stage string) error {
			if stage == "dir-synced" {
				if m++; m == 1 {
					return errors.New("injected")
				}
			}
			return nil
		}
		if _, oe := e2.do(op, args); oe != nil {
			t.Fatalf("%s: dir-fsync-only failure: %v", op, oe.Code)
		}
		if !linked(e2) || len(e2.orphans()) != 0 {
			t.Fatalf("%s: linked=%v orphans=%+v", op, linked(e2), e2.orphans())
		}
		// the provider entry changed in the middle of the flow: not registered, orphan
		e3 := newE3(t)
		e3.existing()
		args = mk(e3)
		e3.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys") {
				e3.s.Registry.Update(func(reg *Registry) error {
					reg.Providers[findProvider(reg, "gh")].TokenRef = "gh-other"
					return nil
				})
			}
			return false
		}
		_, oe = e3.do(op, args)
		if d := linkPartialOf(t, oe); d.Step != "register" {
			t.Fatalf("%s: %+v", op, d)
		}
		if linked(e3) {
			t.Fatalf("%s: linked although the provider entry changed", op)
		}
		// attach: the project changed in the middle of the flow (ai_context flipped): refused, orphan
		if op == "attach_remote" {
			e4 := newE3(t)
			e4.existing()
			args = mk(e4)
			e4.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys") {
					e4.s.Registry.Update(func(reg *Registry) error {
						for i := range reg.Projects {
							if reg.Projects[i].ID == "plain" {
								reg.Projects[i].AIContext = false
							}
						}
						return nil
					})
				}
				return false
			}
			_, oe = e4.do(op, args)
			if d := linkPartialOf(t, oe); d.Step != "register" {
				t.Fatalf("%+v", d)
			}
			if p, _ := e4.find("plain"); p.Provider != nil {
				t.Fatal("attached to a project that changed under us")
			}
		}
		// a panic in the registry write (with the token as its value): orphan, nothing leaks
		e5 := newE3(t)
		e5.existing()
		args = mk(e5)
		k := 0
		e5.s.Registry.opts.failAt = func(stage string) error {
			if stage == "temp-written" {
				if k++; k == 1 {
					panic(ghCanary)
				}
			}
			return nil
		}
		r := e5.wire(op, args)
		if r.Code != "link_partial" {
			t.Fatalf("%s panic reply: %+v", op, r)
		}
		if o := e5.orphans(); len(o) != 1 || o[0].Step != "register" {
			t.Fatalf("%s: orphans after panic: %+v", op, o)
		}
		if strings.Contains(jsonOf(t, r)+e5.logs.String()+string(e5.regFile()), ghCanary) {
			t.Fatal("canary leaked through the panic path")
		}
	}
}

// --- signing, shape and limits (the W3a/W3b/W3c table, repeated for the new actions) ---

func e3SignedCases() []struct {
	op   string
	args func(e *e3env, who testSigner) map[string]any
} {
	return []struct {
		op   string
		args func(e *e3env, who testSigner) map[string]any
	}{
		{"link_project", func(e *e3env, who testSigner) map[string]any {
			a := e.signedArgs(who, registrymsg.ActionLinkProject, "linked", e.linkFields("gh", "alice/existing", true))
			a["deploy_key"] = c3Key()
			return a
		}},
		{"attach_remote", func(e *e3env, who testSigner) map[string]any {
			a := e.signedArgs(who, registrymsg.ActionAttachRemote, "plain", e.attachFields("gh", "alice/existing"))
			a["deploy_key"] = c3Key()
			return a
		}},
	}
}

func TestLinkAttachRequireSignature(t *testing.T) {
	for _, sc := range e3SignedCases() {
		t.Run(sc.op, func(t *testing.T) {
			e := newE3(t)
			e.existing()
			e.ownLocal("plain", false, nil)
			before := e.snap()
			e.wantCode(sc.op, copyArgs(sc.args(e, e.alice), func(m map[string]any) { delete(m, "signature") }), "not_signed")
			e.wantCode(sc.op, copyArgs(sc.args(e, e.alice), func(m map[string]any) { m["signature"] = nil }), "not_signed")
			e.wantCode(sc.op, sc.args(e, newSSHTestSigner(t, "mallory")), "unknown_signer")
			// a valid signature of the same action over ANOTHER request (other request ID and time)
			other := sc.args(e, e.alice)
			e.wantCode(sc.op, copyArgs(sc.args(e, e.alice), func(m map[string]any) { m["signature"] = other["signature"] }), "bad_signature")
			e.mustEqual(before, sc.op)
			e.noGHHits(sc.op)
			e.noGLHits(sc.op)
		})
	}
}

func TestChangeMismatchBurnsNoID(t *testing.T) {
	for _, sc := range e3SignedCases() {
		t.Run(sc.op, func(t *testing.T) {
			e := newE3(t)
			e.existing()
			e.ownLocal("plain", false, nil)
			before := e.snap()
			good := sc.args(e, e.alice)
			e.wantCode(sc.op, copyArgs(good, func(m map[string]any) { m["change"] = "" }), "change_mismatch")
			e.wantCode(sc.op, copyArgs(good, func(m map[string]any) { delete(m, "change") }), "change_mismatch")
			// every signed field: swapped after signing -> change_mismatch
			swaps := map[string]any{"remote_path": "alice/other", "publish": "read", "provider": "gl"}
			if sc.op == "link_project" {
				swaps["ai_context"] = false
			}
			for k, v := range swaps {
				e.wantCode(sc.op, copyArgs(good, func(m map[string]any) { m[k] = v }), "change_mismatch")
			}
			// another valid key line with its own matching hash: the signed hash differs
			other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOtherOtherOth"
			e.wantCode(sc.op, copyArgs(good, func(m map[string]any) { m["deploy_key"] = other; m["deploy_key_sha256"] = c3Hash(other) }), "change_mismatch")
			e.mustEqual(before, sc.op)
			e.noGHHits(sc.op)
			// nothing above burnt the ID: the honest request still works
			if _, oe := e.do(sc.op, good); oe != nil {
				t.Fatalf("honest request after refusals: %v", oe.Code)
			}
			e.wantCode(sc.op, good, "already_linked") // and now the registry rule answers first (shape stage)
		})
	}
}

func TestLinkAttachShapeErrorsBurnNoID(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.ownLocal("plain", false, nil)
	before := e.snap()
	link := func(mod func(m map[string]any)) map[string]any {
		return copyArgs(e.linkArgs("linked", "gh", "alice/existing", true, nil), mod)
	}
	attach := func(mod func(m map[string]any)) map[string]any {
		return copyArgs(e.attachArgs("plain", "gh", "alice/existing", nil), mod)
	}
	cases := []struct {
		op   string
		args map[string]any
		want string
	}{
		{"link_project", link(func(m map[string]any) { m["deploy_key"] = "ssh-rsa x" }), "bad_request"},
		{"link_project", link(func(m map[string]any) { delete(m, "deploy_key") }), "bad_request"},
		{"link_project", link(func(m map[string]any) { delete(m, "ai_context") }), "bad_request"},
		{"link_project", link(func(m map[string]any) { m["namespace_id"] = 3 }), "bad_request"},
		{"link_project", link(func(m map[string]any) { m["id"] = "Bad_Name" }), "bad_request"},
		{"link_project", link(func(m map[string]any) { m["deploy_key_sha256"] = strings.Repeat("0", 64) }), "deploy_key_mismatch"},
		{"link_project", link(func(m map[string]any) { m["provider"] = "nope" }), "provider_unknown"},
		{"attach_remote", attach(func(m map[string]any) { m["deploy_key"] = "ssh-rsa x" }), "bad_request"},
		{"attach_remote", attach(func(m map[string]any) { m["ai_context"] = true }), "bad_request"},
		{"attach_remote", attach(func(m map[string]any) { m["deploy_key_sha256"] = strings.Repeat("0", 64) }), "deploy_key_mismatch"},
		{"attach_remote", attach(func(m map[string]any) { m["id"] = "Bad_Name" }), "bad_request"},
		// remote paths the provider client would reject: refused here, not after the ID is burnt
		{"link_project", e.linkArgs("linked", "gh", "alice/existing/extra", true, nil), "bad_request"},
		{"link_project", e.linkArgs("linked", "gl", "team/Exist", true, nil), "bad_request"},
		{"link_project", e.linkArgs("linked", "gl", "team/my_repo", true, nil), "bad_request"},
		{"attach_remote", e.attachArgs("plain", "gh", "a/b/c", nil), "bad_request"},
	}
	for _, tc := range cases {
		e.wantCode(tc.op, tc.args, tc.want)
		e.wantCode(tc.op, tc.args, tc.want) // twice: never request_reused
	}
	e.mustEqual(before, "shape errors")
	e.noGHHits("shape errors")
	e.noGLHits("shape errors")
}

func TestLinkAttachSharedQuotaBeforeFirstWrite(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.ownLocal("plain", false, nil)
	e.s.State.LinkRemote = NewHourlyLimiter(1, func() time.Time { return wNow })
	// refusals before the write do not spend the quota
	e.wantCode("link_project", e.linkArgs("linked", "gh", "alice/missing", true, nil), "repo_not_found")
	e.wantCode("link_project", e.linkArgs("linked", "gh", "alice/existing", true, func(f map[string]any) {}), "")
	w := len(e.ghWrites())
	e.wantCode("attach_remote", e.attachArgs("plain", "gh", "alice/existing", nil), "already_linked") // same remote, shape-stage
	if len(e.ghWrites()) != w {
		t.Fatal("write after a shape refusal")
	}
	e.gh.repos["alice/second"] = "alice/second"
	e.wantCode("attach_remote", e.attachArgs("plain", "gh", "alice/second", nil), "rate_limited") // one cap for both ops
	if len(e.ghWrites()) != w {
		t.Fatalf("a write happened after the limit: %v", e.ghWrites())
	}
	if p, _ := e.find("plain"); p.Provider != nil {
		t.Fatal("attached beyond the limit")
	}
	if e.s.State.LinkRemote == nil {
		t.Fatal("no default limiter")
	}
}

func TestCrossActionSignaturesRefusedW3e(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.withProvider("spare", e.gl.URL)
	e.ownLocal("plain", false, nil)
	type act struct {
		op   string
		args func() map[string]any
	}
	acts := []act{
		{"register_project", func() map[string]any {
			return e.signedArgs(e.alice, registrymsg.ActionRegisterProject, "xproj", regFields(e.root+"/xproj", "default", true))
		}},
		{"set_flag", func() map[string]any {
			return e.signedArgs(e.alice, registrymsg.ActionSetFlag, "repo-a", flagFields("ai_context", false))
		}},
		{"set_provider", func() map[string]any {
			return e.signedArgs(e.alice, registrymsg.ActionSetProvider, "gl", e.providerFields(e.gl.URL))
		}},
		{"delete_provider", func() map[string]any { return e.deleteProviderArgs("spare") }},
		{"create_group", func() map[string]any { return e.groupArgs("newg", "newg", "") }},
		{"create_project", func() map[string]any { return e.projectArgs(c3Name, nil) }},
		{"create_project", func() map[string]any { return e.ghCreate(c3Name, "alice", "private", nil) }},
		{"link_project", func() map[string]any { return e.linkArgs("linked", "gh", "alice/existing", true, nil) }},
		{"attach_remote", func() map[string]any { return e.attachArgs("plain", "gh", "alice/existing", nil) }},
		{"set_token", func() map[string]any { return e.tokArgs(e.alice, "other-gitlab", "gitlab", "", tokenOK) }},
		{"delete_token", func() map[string]any { return e.delArgs(e.alice, "gl-admin") }},
	}
	snapReg, snapTok := e.snap(), e.tokSnap()
	pairs := 0
	for ai, a := range acts {
		for bi, b := range acts {
			if ai == bi || (a.op == b.op && a.op != "create_project") {
				continue
			}
			if a.op == "create_project" && b.op == "create_project" {
				continue
			}
			isNew := func(op string) bool { return op == "link_project" || op == "attach_remote" }
			if !isNew(a.op) && !isNew(b.op) {
				continue // the W3c pairs are TestCrossActionSignaturesRefused
			}
			sigA := a.args()
			r1 := b.args()
			r1["signature"] = sigA["signature"]
			r2 := b.args()
			for _, k := range []string{"signature", "change", "request", "issued"} {
				r2[k] = sigA[k]
			}
			for i, r := range []map[string]any{r1, r2} {
				if _, oe := e.do(b.op, r); oe == nil {
					t.Errorf("%s signature accepted as %s (variant %d)", a.op, b.op, i+1)
				} else if oe.Code != "bad_signature" && oe.Code != "change_mismatch" && oe.Code != "bad_request" {
					t.Errorf("%s signature on %s (variant %d): code %s", a.op, b.op, i+1, oe.Code)
				}
			}
			pairs++
		}
	}
	if pairs < 30 {
		t.Fatalf("only %d pairs tried", pairs)
	}
	e.mustEqual(snapReg, "cross-action signatures")
	if !e.tokSnap().sameStore(snapTok) || !e.tokSnap().sameUsed(snapTok) {
		t.Fatal("the token store or the used-ID file changed")
	}
	e.noGHHits("cross-action signatures")
	e.noGLHits("cross-action signatures")
	// control: each new action works with its own signature
	if _, oe := e.do("link_project", e.linkArgs("linked", "gh", "alice/existing", true, nil)); oe != nil {
		t.Fatalf("control link: %v", oe.Code)
	}
	e.gh.mu.Lock()
	e.gh.repos["alice/second"] = "alice/second"
	e.gh.mu.Unlock()
	if _, oe := e.do("attach_remote", e.attachArgs("plain", "gh", "alice/second", nil)); oe != nil {
		t.Fatalf("control attach: %v", oe.Code)
	}
}

func TestCanaryTokenNeverInAnyOutputW3e(t *testing.T) {
	var leaks []string
	check := func(what, s string) {
		if strings.Contains(s, ghCanary) || strings.Contains(s, c3Canary) || strings.Contains(s, "REFLECT") {
			leaks = append(leaks, what)
		}
	}
	type scenario struct {
		inject func(e *e3env)
		op     string
		args   func(e *e3env) map[string]any
	}
	create := func(e *e3env) map[string]any { return e.ghCreate(c3Name, "alice", "private", nil) }
	link := func(e *e3env) map[string]any { return e.linkArgs("linked", "gh", "alice/existing", true, nil) }
	attach := func(e *e3env) map[string]any {
		e.ownLocal("plain", false, nil)
		return e.attachArgs("plain", "gh", "alice/existing", nil)
	}
	scenarios := map[string]scenario{
		"create ok":           {func(e *e3env) {}, "create_project", create},
		"create refused":      {func(e *e3env) { e.gh.fail["POST /user/repos"] = 422 }, "create_project", create},
		"create 5xx":          {func(e *e3env) { e.gh.fail["POST /user/repos"] = 500 }, "create_project", create},
		"create files":        {func(e *e3env) { e.gh.fail["PUT /repos/alice/demo/contents/README.md"] = 403 }, "create_project", create},
		"create protect":      {func(e *e3env) { e.gh.protection = ghWeakProtection }, "create_project", create},
		"create paid plan":    {func(e *e3env) { e.gh.fail["PUT /repos/alice/demo/branches/main/protection"] = 403 }, "create_project", create},
		"create key":          {func(e *e3env) { e.gh.fail["POST /repos/alice/demo/keys"] = 401 }, "create_project", create},
		"create owner":        {func(e *e3env) { e.gh.fail["GET /user"] = 403 }, "create_project", create},
		"link ok":             {func(e *e3env) {}, "link_project", link},
		"link missing":        {func(e *e3env) { e.gh.repos = map[string]string{} }, "link_project", link},
		"link read failure":   {func(e *e3env) { e.gh.fail["GET /repos/alice/existing"] = 500 }, "link_project", link},
		"link key refused":    {func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 422 }, "link_project", link},
		"link key 5xx":        {func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 502 }, "link_project", link},
		"attach ok":           {func(e *e3env) {}, "attach_remote", attach},
		"attach key refused":  {func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 422 }, "attach_remote", attach},
		"attach key 5xx":      {func(e *e3env) { e.gh.fail["POST /repos/alice/Existing/keys"] = 500 }, "attach_remote", attach},
		"attach rate limited": {func(e *e3env) { e.gh.fail["GET /repos/alice/existing"] = 429 }, "attach_remote", attach},
	}
	for name, sc := range scenarios {
		e := newE3(t)
		e.existing()
		sc.inject(e)
		r := e.wire(sc.op, sc.args(e))
		check(name+" reply", jsonOf(t, r))
		check(name+" log", e.logs.String())
		check(name+" registry", string(e.regFile()))
		check(name+" used", string(e.usedFile()))
		for i, rq := range e.gh.recorded() {
			check(name+" request line", rq)
			e.gh.mu.Lock()
			body := e.gh.bodies[i]
			e.gh.mu.Unlock()
			check(name+" request body", body)
		}
		// the token only travels in the Authorization header
		e.gh.mu.Lock()
		for _, h := range e.gh.headers {
			for k, v := range h {
				if k != "Authorization" && strings.Contains(strings.Join(v, ","), ghCanary) {
					leaks = append(leaks, name+" header "+k)
				}
			}
		}
		e.gh.mu.Unlock()
		rg := e.wire("list_groups", map[string]any{"provider": "gh"})
		check(name+" list_groups", jsonOf(t, rg)+e.logs.String())
		rp := e.wire("providers", nil)
		check(name+" providers", jsonOf(t, rp))
	}
	if len(leaks) != 0 {
		t.Fatalf("canary leaked: %v", leaks)
	}
}

func TestNewCodesFixedSentencesW3e(t *testing.T) {
	seen := map[string]string{}
	for _, code := range []string{"repo_not_found", "already_linked", "github_refused", "provider_rate_limited", "not_supported_for_provider",
		"visibility_required", "visibility_not_allowed"} {
		m, ok := fixedMessages[code]
		if !ok || strings.TrimSpace(m) == "" {
			t.Fatalf("no fixed sentence for %s", code)
		}
		hangul := false
		for _, r := range m {
			if r >= 0xAC00 && r <= 0xD7A3 {
				hangul = true
			}
		}
		if !hangul || strings.ContainsAny(m, "\n\r{}%") {
			t.Fatalf("%s: not a plain Korean sentence: %q", code, m)
		}
		if prev, dup := seen[m]; dup {
			t.Fatalf("%s and %s share a sentence", code, prev)
		}
		seen[m] = code
	}
	// the two link codes are provider-neutral: they serve GitLab and GitHub alike
	for _, code := range []string{"repo_not_found", "already_linked"} {
		if m := strings.ToLower(fixedMessages[code]); strings.Contains(m, "gitlab") || strings.Contains(m, "github") {
			t.Fatalf("%s names a provider: %q", code, fixedMessages[code])
		}
	}
	// codes owned by orchd (E4) are not the broker's
	for _, code := range []string{"static_remote_wins", "key_staging_failed", "key_expired", CodeDefaultBranchMismatch} {
		if _, ok := fixedMessages[code]; ok {
			t.Fatalf("%s must not be a broker wire code", code)
		}
	}
}

func TestOpsRegisteredW3e(t *testing.T) {
	e := newE3(t)
	for _, op := range []string{"link_project", "attach_remote"} {
		if e.s.Handlers[op] == nil {
			t.Fatalf("%s not registered", op)
		}
		if d := defaultOpTimeouts[op]; d <= 0 {
			t.Fatalf("%s has no op timeout", op)
		}
	}
	// a destructive twin never exists
	for _, op := range []string{"unlink_project", "detach_remote", "delete_repo", "create_repo", "link", "attach"} {
		r, _ := call(t, e.sock, `{"v":1,"op":"`+op+`","args":{}}`)
		if r.Code != "unknown_op" {
			t.Fatalf("%s: %+v", op, r)
		}
	}
}
