package broker

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// W3e E3 fix round 1.

// A: the signed owner field is a superset; each provider's own rule is applied in
// the shape stage (before the signature, no ID burnt).
func TestOwnerFieldPerProviderRulesAtShape(t *testing.T) {
	// GitHub accepts real logins as they are written
	for _, login := range []string{"SampleUser", "alice_corp", "1abc", "a", "Acme-Org2"} {
		e := newE3(t)
		e.gh.login = login
		if _, oe := e.do("create_project", e.ghCreate(c3Name, login, "private", nil)); oe != nil {
			t.Fatalf("%q: %v", login, oe.Code)
		}
		p, _ := e.find(c3Name)
		if p.Remote == nil || p.Remote.Path != login+"/"+c3Name {
			t.Fatalf("%q: remote %+v", login, p.Remote)
		}
		if e.indexOf("POST /user/repos") < 0 || e.indexOf("PUT /repos/"+login+"/demo/contents/README.md") < 0 {
			t.Fatalf("%q: %v", login, e.trace())
		}
	}
	// the owner is looked up case-insensitively among the authenticated owners and the
	// API's spelling is what is stored (never the signed string)
	for _, signed := range []string{"sampleuser", "SAMPLEUSER", "SampleUser"} {
		e := newE3(t)
		e.gh.login = "SampleUser"
		if _, oe := e.do("create_project", e.ghCreate(c3Name, signed, "private", nil)); oe != nil {
			t.Fatalf("%q: %v", signed, oe.Code)
		}
		if p, _ := e.find(c3Name); p.Remote == nil || p.Remote.Path != "SampleUser/demo" {
			t.Fatalf("%q: stored %+v", signed, p.Remote)
		}
		if strings.Contains(strings.Join(e.trace(), "\n"), "/"+signed+"/") && signed != "SampleUser" {
			t.Fatalf("the signed spelling %q reached a request path: %v", signed, e.trace())
		}
	}
	// ... and an owner that is not among them is group_unknown, however it is spelled
	e := newE3(t)
	e.gh.login = "SampleUser"
	e.wantCode("create_project", e.ghCreate(c3Name, "someone-else", "private", nil), "group_unknown")
	if len(e.ghWrites()) != 0 {
		t.Fatal("write for an unknown owner")
	}
	// GitHub refuses what is not one login: path, dot, hyphen edges, too long
	e = newE3(t)
	before := e.snap()
	for _, bad := range []string{"-abc", "abc-", "a/b", "a.b", strings.Repeat("a", 40), "aa/bb/cc"} {
		args := e.ghCreate(c3Name, bad, "private", nil)
		e.wantCode("create_project", args, "bad_request")
		e.wantCode("create_project", args, "bad_request") // never request_reused
	}
	e.mustEqual(before, "github owner shape")
	e.noGHHits("github owner shape")
	// GitLab keeps the exact W3c lowercase group-path rule
	g := newE3(t)
	gbefore := g.snap()
	for _, bad := range []string{"SampleUser", "a", "1abc", "a_b", "Team", "a.b/cd", "team/Sub"} {
		args := g.projectArgs(c3Name, func(f map[string]any) { f["group_path"] = bad })
		g.wantCode("create_project", args, "bad_request")
		g.wantCode("create_project", args, "bad_request")
	}
	g.mustEqual(gbefore, "gitlab owner shape")
	g.noGLHits("gitlab owner shape")
	g.noGHHits("gitlab owner shape")
	if _, oe := g.do("create_project", g.projectArgs(c3Name, func(f map[string]any) { f["group_path"] = "team/sub" })); oe != nil {
		t.Fatalf("a GitLab group path: %v", oe.Code)
	}
}

// B: attach never adopts a project that has any remote mapping; create_mr keeps its mapping.
func TestAttachMappedProjectKeepsCreateMRMapping(t *testing.T) {
	e := newE3(t)
	e.existing()
	e.ownLocal("mapped", true, &RemoteLink{Path: "team/exist"})
	before := e.snap()
	// cross-kind and cross-host: a GitHub repo onto a project mapped to a GitLab path
	for _, a := range [][2]string{{"gh", "alice/existing"}, {"gl", "team/exist"}} {
		args := e.attachArgs("mapped", a[0], a[1], nil)
		e.wantCode("attach_remote", args, "already_linked")
		e.wantCode("attach_remote", args, "already_linked")
	}
	e.mustEqual(before, "attach to a mapped project")
	e.noGHHits("attach mapped")
	e.noGLHits("attach mapped")
	res, oe := e.s.Handlers["status"](nil, nil)
	if oe != nil {
		t.Fatal(oe.Code)
	}
	var found bool
	for _, p := range res.(map[string]any)["mr_projects"].([]string) {
		found = found || p == e.root+"/mapped"
	}
	if !found {
		t.Fatalf("create_mr no longer reaches the mapped project: %v", res)
	}
	if g, ok := e.s.Registry.GitLabProject(e.root + "/mapped"); !ok || g != "team/exist" {
		t.Fatalf("mapping: %q %v", g, ok)
	}
}

// C: origin on orphan records, link_partial sentence, wire detail.
func TestOrphanOriginAndLinkPartialSentence(t *testing.T) {
	// created
	e := newE3(t)
	e.gh.fail["PUT /repos/alice/demo/contents/README.md"] = 500
	_, oe := e.do("create_project", e.ghCreate(c3Name, "alice", "private", nil))
	if d := partialOf(t, oe); d.Origin != OriginCreated {
		t.Fatalf("%+v", d)
	}
	if os := e.orphans(); len(os) != 1 || os[0].Origin != OriginCreated {
		t.Fatalf("%+v", os)
	}
	// linked: its own code, sentence and wire detail
	l := newE3(t)
	l.existing()
	l.gh.fail["POST /repos/alice/Existing/keys"] = 500
	r := l.wire("link_project", l.linkArgs("linked", "gh", "alice/existing", true, nil))
	if r.Code != "link_partial" || r.Message != fixedMessages["link_partial"] || strings.Contains(r.Message, "REFLECT") {
		t.Fatalf("%+v", r)
	}
	if os := l.orphans(); len(os) != 1 || os[0].Origin != OriginLinked || os[0].Step != "deploy_key" {
		t.Fatalf("%+v", os)
	}
	raw := l.rawWire("link_project", l.linkArgs("linked2", "gh", "alice/missing", true, nil))
	if strings.Contains(raw, `"detail"`) {
		t.Fatalf("detail on a plain refusal: %s", raw)
	}
	l2 := newE3(t)
	l2.existing()
	l2.gh.fail["POST /repos/alice/Existing/keys"] = 500
	raw = l2.rawWire("attach_remote", func() map[string]any {
		l2.ownLocal("plain", false, nil)
		return l2.attachArgs("plain", "gh", "alice/existing", nil)
	}())
	if !strings.Contains(raw, `"code":"link_partial"`) || !strings.Contains(raw, `"origin":"linked"`) || !strings.Contains(raw, `"step":"deploy_key"`) {
		t.Fatalf("%s", raw)
	}
	// the sentence: provider-neutral Korean, says "remove the key by hand, do not delete the repository"
	m := fixedMessages["link_partial"]
	low := strings.ToLower(m)
	if strings.Contains(low, "gitlab") || strings.Contains(low, "github") || !strings.Contains(m, "지우지 마세요") || !strings.Contains(m, "배포 키") ||
		m == fixedMessages["create_partial"] {
		t.Fatalf("sentence: %q", m)
	}
	if !strings.Contains(fixedMessages["create_partial"], "삭제하지 않으니") {
		t.Fatal("create_partial sentence changed")
	}
}

func TestOrphanOriginInRegistryFile(t *testing.T) {
	const head = `{"version":3,"providers":[],"projects":[],"auto_merge_global":{"enabled":false,"max_per_day":5},"orphans":[`
	const tail = `]}` + "\n"
	rec := func(origin string) string {
		s := `{"provider":"gl","project":"team/x","project_id":null,"step":"project","at":"2026-11-11T01:02:03Z"`
		if origin != "" {
			s += `,"origin":"` + origin + `"`
		}
		return s + `}`
	}
	// a record without an origin is a "created" one
	dir := stateDirFor(t)
	p := writeRegFile(t, dir, head+rec("")+tail, 0o600)
	st := mustOpen(t, dir, LegacySource{})
	if os := st.Snapshot().Orphans; len(os) != 1 || os[0].Origin != OriginCreated {
		t.Fatalf("%+v", os)
	}
	// the next write persists it
	if _, err := st.AddOrphan(Orphan{Provider: "gl", Project: "team/y", Step: "project", At: "2026-11-11T01:02:04Z", Origin: OriginLinked}); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, p)); !strings.Contains(got, `"origin":"created"`) || !strings.Contains(got, `"origin":"linked"`) || strings.Count(got, `"origin"`) != 2 {
		t.Fatalf("%s", got)
	}
	// valid values load as written
	dir = stateDirFor(t)
	writeRegFile(t, dir, head+rec("linked")+","+rec("created")+tail, 0o600)
	if os := mustOpen(t, dir, LegacySource{}).Snapshot().Orphans; len(os) != 2 || os[0].Origin != "linked" || os[1].Origin != "created" {
		t.Fatalf("%+v", os)
	}
	// anything else breaks the registry (fail closed)
	for _, bad := range []string{"made", "Created", "linked ", "x"} {
		dir = stateDirFor(t)
		writeRegFile(t, dir, head+rec(bad)+tail, 0o600)
		st, err := OpenRegistry(dir, LegacySource{}, regOpts())
		if err == nil && (st == nil || st.Err() == nil) {
			t.Fatalf("origin %q accepted", bad)
		}
	}
	// a record without a step name from a v2 file migrates as created (TestRegistryV2MigratesToV3Atomically pins the bytes)
}

// D: one link/attach per remote at a time, whatever the project id.
func TestLinkAttachPerRemoteLockConcurrent(t *testing.T) {
	e := newE3(t)
	e.existing()
	started, release := make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	e.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" && r.URL.Path == "/repos/alice/existing" && n.Add(1) == 1 {
			close(started)
			<-release
		}
		return false
	}
	a := e.linkArgs("first-one", "gh", "alice/existing", true, nil)
	b := e.linkArgs("second-one", "gh", "Alice/EXISTING", true, nil) // another spelling, another id
	var wg sync.WaitGroup
	var oeA *OpError
	wg.Add(1)
	go func() { defer wg.Done(); _, oeA = e.do("link_project", a) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first link never reached GitHub")
	}
	_, oeB := e.do("link_project", b) // returns while the first is still in flight
	close(release)
	wg.Wait()
	if oeA != nil || codeOf(oeB) != "already_linked" {
		t.Fatalf("A=%v B=%v", codeOf(oeA), codeOf(oeB))
	}
	if keys := strings.Count(strings.Join(e.trace(), "\n"), "POST /repos/alice/Existing/keys"); keys != 1 {
		t.Fatalf("%d deploy keys: %v", keys, e.trace())
	}
	if len(e.orphans()) != 0 {
		t.Fatalf("orphans: %+v", e.orphans())
	}
	if _, ok := e.find("second-one"); ok {
		t.Fatal("the second id was registered")
	}
	if _, ok := e.find("first-one"); !ok {
		t.Fatal("the first was not registered")
	}
	// attach and link of the same remote collide the same way
	e2 := newE3(t)
	e2.existing()
	e2.ownLocal("plain", false, nil)
	var m atomic.Int32
	started2, release2 := make(chan struct{}), make(chan struct{})
	e2.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" && r.URL.Path == "/repos/alice/existing" && m.Add(1) == 1 {
			close(started2)
			<-release2
		}
		return false
	}
	var oe1 *OpError
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, oe1 = e2.do("attach_remote", e2.attachArgs("plain", "gh", "alice/existing", nil))
	}()
	<-started2
	_, oe2 := e2.do("link_project", e2.linkArgs("other-one", "gh", "alice/existing", false, nil))
	close(release2)
	wg.Wait()
	if oe1 != nil || codeOf(oe2) != "already_linked" || strings.Count(strings.Join(e2.trace(), "\n"), "/keys") != 1 {
		t.Fatalf("attach=%v link=%v %v", codeOf(oe1), codeOf(oe2), e2.trace())
	}
}

// E: the flows build their client from the provider entry they already read.
func TestFlowsBuildTheClientFromTheEntryAlreadyRead(t *testing.T) {
	for _, f := range []string{"create_project.go", "link_project.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "s.providerFor(") || strings.Contains(src, "RepoProviderFor(") {
			t.Errorf("%s reads the registry a second time for the client", f)
		}
		if !strings.Contains(src, "s.providerForEntry(prov)") {
			t.Errorf("%s does not build the client from prov", f)
		}
	}
}

// F: op timeouts.
func TestLinkAttachOpTimeouts(t *testing.T) {
	if defaultOpTimeouts["link_project"] != 45*time.Second || defaultOpTimeouts["attach_remote"] != 45*time.Second || defaultOpTimeouts["create_project"] != 60*time.Second {
		t.Fatalf("%v", defaultOpTimeouts)
	}
}

// G: panics, cancellation, a failing orphan write.
func TestGitHubCreatePanicIsOrphanAndNeverLeaks(t *testing.T) {
	e := newE3(t)
	n := 0
	e.s.Registry.opts.failAt = func(stage string) error {
		if stage == "temp-written" {
			if n++; n == 1 {
				panic(ghCanary)
			}
		}
		return nil
	}
	r := e.wire("create_project", e.ghCreate(c3Name, "alice", "private", nil))
	if r.Code != "create_partial" {
		t.Fatalf("%+v", r)
	}
	if o := e.orphans(); len(o) != 1 || o[0].Step != "register" || o[0].Origin != OriginCreated || o[0].ProjectID == nil {
		t.Fatalf("%+v", o)
	}
	if _, ok := e.find(c3Name); ok {
		t.Fatal("registered")
	}
	if strings.Contains(jsonOf(t, r)+e.logs.String()+string(e.regFile()), ghCanary) {
		t.Fatal("canary leaked")
	}
}

func waitOrphans(t *testing.T, e *e3env, n int) []Orphan {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if o := e.orphans(); len(o) >= n {
			return o
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no orphan record after the timeout (%d wanted)", n)
	return nil
}

func TestLinkAttachAndCreateTimeoutIsUnknownOutcomeWithOrphan(t *testing.T) {
	delay := func(e *e3env, method, suffix string) {
		e.gh.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == method && strings.HasSuffix(r.URL.Path, suffix) {
				time.Sleep(900 * time.Millisecond)
			}
			return false
		}
	}
	// link: the op timeout ends the call during the key POST; the handler goes on and records the orphan
	e := newE3(t)
	e.existing()
	e.s.OpTimeouts = map[string]time.Duration{"link_project": 300 * time.Millisecond}
	delay(e, "POST", "/keys")
	r := e.wire("link_project", e.linkArgs("linked", "gh", "alice/existing", true, nil))
	if r.Code != "timeout" {
		t.Fatalf("%+v", r)
	}
	o := waitOrphans(t, e, 1)
	if o[0].Step != "deploy_key" || o[0].Origin != OriginLinked {
		t.Fatalf("%+v", o)
	}
	if _, ok := e.find("linked"); ok {
		t.Fatal("registered after a timed-out key step")
	}
	// attach: a timeout before any write (the GET) leaves nothing behind
	a := newE3(t)
	a.existing()
	a.ownLocal("plain", false, nil)
	a.s.OpTimeouts = map[string]time.Duration{"attach_remote": 300 * time.Millisecond}
	delay(a, "GET", "/repos/alice/existing")
	if r := a.wire("attach_remote", a.attachArgs("plain", "gh", "alice/existing", nil)); r.Code != "timeout" {
		t.Fatalf("%+v", r)
	}
	time.Sleep(1200 * time.Millisecond)
	if len(a.orphans()) != 0 || len(a.ghWrites()) != 0 {
		t.Fatalf("orphans/writes after a timeout before the first write: %v %v", a.orphans(), a.ghWrites())
	}
	if p, _ := a.find("plain"); p.Provider != nil {
		t.Fatal("attached")
	}
	// create: the timeout during the first file leaves a created-orphan with the id
	c := newE3(t)
	c.s.OpTimeouts = map[string]time.Duration{"create_project": 300 * time.Millisecond}
	delay(c, "PUT", "/contents/README.md")
	if r := c.wire("create_project", c.ghCreate(c3Name, "alice", "private", nil)); r.Code != "timeout" {
		t.Fatalf("%+v", r)
	}
	co := waitOrphans(t, c, 1)
	if co[0].Step != "first_commit" || co[0].Origin != OriginCreated || co[0].ProjectID == nil {
		t.Fatalf("%+v", co)
	}
}

func TestLinkAttachFailingOrphanWriteStillAnswersPartial(t *testing.T) {
	for _, op := range []string{"link_project", "attach_remote"} {
		e := newE3(t)
		e.existing()
		var args map[string]any
		if op == "link_project" {
			args = e.linkArgs("linked", "gh", "alice/existing", true, nil)
		} else {
			e.ownLocal("plain", false, nil)
			args = e.attachArgs("plain", "gh", "alice/existing", nil)
		}
		e.s.Registry.opts.failAt = func(stage string) error {
			if stage == "temp-written" {
				return errTestInjected
			}
			return nil
		}
		_, oe := e.do(op, args)
		d := linkPartialOf(t, oe)
		if d.Step != "register" || d.OrphanRecorded || d.ProjectID == nil || *d.ProjectID != 31 {
			t.Fatalf("%s: %+v", op, d)
		}
		if len(e.orphans()) != 0 {
			t.Fatalf("%s: an orphan is recorded although every write failed", op)
		}
		if !strings.Contains(e.logs.String(), "orphan record could not be saved") {
			t.Fatalf("%s: not logged", op)
		}
	}
}

// the burn state: the request ID is not in the used-ID file after any refusal
// before verification, and is there after the honest request.
func TestChangeMismatchBurnStateInUsedFile(t *testing.T) {
	for _, sc := range e3SignedCases() {
		e := newE3(t)
		e.existing()
		e.ownLocal("plain", false, nil)
		good := sc.args(e, e.alice)
		req := good["request"].(string)
		used := func() bool { return strings.Contains(string(e.usedFile()), req) }
		if used() {
			t.Fatal("fresh id already used")
		}
		e.wantCode(sc.op, copyArgs(good, func(m map[string]any) { m["remote_path"] = "alice/other" }), "change_mismatch")
		e.wantCode(sc.op, copyArgs(good, func(m map[string]any) { m["change"] = "" }), "change_mismatch")
		if used() {
			t.Fatalf("%s: the ID was burnt by a change mismatch", sc.op)
		}
		if _, oe := e.do(sc.op, good); oe != nil {
			t.Fatal(oe.Code)
		}
		if !used() {
			t.Fatalf("%s: the ID is not recorded after the honest request", sc.op)
		}
	}
}

var errTestInjected = errors.New("injected")
