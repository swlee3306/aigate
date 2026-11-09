package api

// Adversarial suite for W2b (Task 7) on the orchd side: the AI plan path
// (preview, selection, refine), the merge request retry and the case that the
// broker is gone. Fake broker only; nothing reaches a real service. Every
// test keeps an accepted control next to its refusals and checks that a
// refusal sent nothing to the broker.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/refine"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/refinesize"
)

const outsideMarker = "OUTSIDE-REPO-SECRET-MARKER-9f3a"

// outsideRepo is a git repository outside the allowed roots holding a marker.
func outsideRepo(t *testing.T) string {
	t.Helper()
	d := shortDir(t)
	if err := os.WriteFile(filepath.Join(d, "README.md"), []byte("# other\n"+outsideMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "health.txt"), []byte(outsideMarker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, d, "init", "-q")
	git(t, d, "add", "-A")
	git(t, d, "commit", "-q", "-m", "x")
	return d
}

func (e *refineEnv) noMarker(t *testing.T, what string, b []byte) {
	t.Helper()
	if strings.Contains(string(b), outsideMarker) {
		t.Fatalf("%s: outside content in a response: %s", what, b)
	}
}

func (e *refineEnv) assertNothingSent(t *testing.T, what string) {
	t.Helper()
	if n := e.fb.refineCount(); n != 0 {
		t.Fatalf("%s: %d refine calls reached the broker", what, n)
	}
	e.fb.mu.Lock()
	defer e.fb.mu.Unlock()
	for _, c := range e.fb.refines {
		for _, f := range c.Files {
			if strings.Contains(f.Content, outsideMarker) {
				t.Fatalf("%s: outside content sent to the broker", what)
			}
		}
	}
}

func TestRefineCannotLeakOutsideRoots(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	out := outsideRepo(t)
	e.fb.setRepos(e.repo, out) // even a broker that allows it cannot widen the roots
	sock := e.socket(t)
	text := "add a health check"
	post := func(repo string) (int, []byte) {
		return sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": repo, "text": text}))
	}

	// a symlink inside the root that points to a repository outside it
	link := filepath.Join(e.root, "linked")
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	// a directory in the root whose .git points at the outside repository
	stolen := filepath.Join(e.root, "stolen")
	os.MkdirAll(stolen, 0o755)
	if err := os.Symlink(filepath.Join(out, ".git"), filepath.Join(stolen, ".git")); err != nil {
		t.Fatal(err)
	}
	// a plain sub-directory of the real repository (not a work tree top)
	sub := filepath.Join(e.repo, "src")

	for name, tc := range map[string]struct {
		repo   string
		status int
		code   string
	}{
		"other repository":       {out, 403, "repo_not_allowed"},
		"symlink to outside":     {link, 403, "repo_not_allowed"},
		"dotdot out":             {e.repo + "/../../" + filepath.Base(out), 400, ""},
		"dotdot inside":          {e.repo + "/../proj", 400, ""},
		"relative":               {"proj", 400, ""},
		"git dir stolen":         {stolen, 400, ""},
		"sub directory":          {sub, 400, ""},
		"the root itself":        {e.root, 400, ""},
		"nonexistent outside":    {out + "-nope", 403, "repo_not_allowed"},
		"file instead of a repo": {filepath.Join(e.repo, "README.md"), 400, ""},
		"empty":                  {"", 400, ""},
	} {
		code, b := post(tc.repo)
		e.noMarker(t, name, b)
		if code != tc.status || (tc.code != "" && v1Code(b) != tc.code) {
			t.Errorf("%s: %d %s, want %d %s", name, code, b, tc.status, tc.code)
		}
		if strings.Contains(string(b), `"allowed":true`) || strings.Contains(string(b), "selection_id\":\"") && !strings.Contains(string(b), "selection_id\":\"\"") {
			t.Errorf("%s: a selection was created: %s", name, b)
		}
	}
	e.assertNothingSent(t, "preview refusals")

	// a selection made for one repository cannot be used for another one
	second := filepath.Join(e.root, "proj2")
	os.MkdirAll(second, 0o755)
	git(t, second, "init", "-q")
	os.WriteFile(filepath.Join(second, "README.md"), []byte("# second "+outsideMarker+"\n"), 0o644)
	git(t, second, "add", "-A")
	git(t, second, "commit", "-q", "-m", "x")
	e.fb.setRepos(e.repo, second)
	p := e.preview(t, sock, text)
	code, b := sock("/api/v1/refine", jsonBody(map[string]string{"repo": second, "text": text, "selection_id": p.SelectionID}))
	if code == 200 || e.fb.refineCount() != 0 {
		t.Fatalf("selection of proj reused for proj2: %d %s", code, b)
	}

	// inside the repository: symlinks, submodules and path tricks in HEAD
	e2 := newRefineEnv(t, baseFiles)
	e2.fb.setRepos(e2.repo)
	outFile := filepath.Join(shortDir(t), "hostname.txt")
	os.WriteFile(outFile, []byte(outsideMarker), 0o644)
	if err := os.Symlink(outFile, filepath.Join(e2.repo, "health-link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../etc/hosts", filepath.Join(e2.repo, "health-rel.go")); err != nil {
		t.Fatal(err)
	}
	git(t, e2.repo, "add", "-A")
	// a gitlink (submodule) and a path with ".." written straight into the index
	git(t, e2.repo, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",health-sub")
	git(t, e2.repo, "commit", "-q", "-m", "links")
	s2 := e2.socket(t)
	pv := e2.preview(t, s2, "add a health check, health-link health-rel health-sub")
	for _, f := range pv.Files {
		if strings.HasPrefix(f.Path, "health-") {
			t.Errorf("link or submodule offered for sending: %+v", f)
		}
	}
	reasons := map[string]string{}
	for _, x := range pv.Excluded {
		reasons[x.Path] = x.Reason
	}
	if reasons["health-link.go"] != "symlink" || reasons["health-rel.go"] != "symlink" || reasons["health-sub"] != "submodule" {
		t.Errorf("exclusion reasons %v", reasons)
	}
	if code, b := e2.refine(s2, "add a health check, health-link health-rel health-sub", pv.SelectionID); code != 200 {
		t.Fatalf("control refine: %d %s", code, b)
	}
	e2.fb.mu.Lock()
	defer e2.fb.mu.Unlock()
	for _, c := range e2.fb.refines {
		for _, f := range c.Files {
			if strings.Contains(f.Content, outsideMarker) || strings.HasPrefix(f.Path, "health-") || strings.Contains(f.Path, "..") {
				t.Errorf("sent %+v", f)
			}
		}
	}
}

func TestRefinePreviewForbiddenRepoLeaksNothing(t *testing.T) {
	// The broker allows no repository: two repositories with very different
	// contents must give byte-identical answers that name no file.
	e := newRefineEnv(t, baseFiles)
	second := filepath.Join(e.root, "proj2")
	os.MkdirAll(second, 0o755)
	git(t, second, "init", "-q")
	os.WriteFile(filepath.Join(second, "health_check_secret_name.go"), []byte("package x // "+outsideMarker+"\n"), 0o644)
	git(t, second, "add", "-A")
	git(t, second, "commit", "-q", "-m", "x")
	e.fb.setRepos("/nowhere")
	sock := e.socket(t)
	text := "add a health check secret name"
	var bodies [][]byte
	for _, repo := range []string{e.repo, second} {
		code, b := sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": repo, "text": text}))
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		e.noMarker(t, repo, b)
		for _, leak := range []string{"README", "health", "secret_name", "proj"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%q in %s", leak, b)
			}
		}
		bodies = append(bodies, b)
	}
	if string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("the answer depends on the repository:\n%s\n%s", bodies[0], bodies[1])
	}
	// no existence oracle outside the roots: existing and missing look the same
	out := outsideRepo(t)
	c1, b1 := sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": out, "text": text}))
	c2, b2 := sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": out + "-missing", "text": text}))
	if c1 != c2 || string(b1) != string(b2) || c1 != 403 {
		t.Fatalf("existing %d %s vs missing %d %s", c1, b1, c2, b2)
	}
	// a broker that fails or is unreachable gives a fixed error and no files
	e.fb.mu.Lock()
	e.fb.statusFail = true
	e.fb.mu.Unlock()
	code, b := sock("/api/v1/refine/preview", jsonBody(map[string]string{"repo": e.repo, "text": text}))
	if code != 503 && code != 502 {
		t.Fatalf("broker failure: %d %s", code, b)
	}
	for _, leak := range []string{"README", "health", "SECRET-BROKER-TEXT", `"files":[{`} {
		if strings.Contains(string(b), leak) {
			t.Errorf("%q in %s", leak, b)
		}
	}
	// nothing from a forbidden preview can be sent afterwards
	for _, id := range []string{"", "x", "deadbeef"} {
		if code, _ := e.refine(sock, text, id); code == 200 {
			t.Fatalf("refine with id %q succeeded", id)
		}
	}
	e.assertNothingSent(t, "forbidden preview")
	// control: allowed again, the same request shows files
	e.fb.mu.Lock()
	e.fb.statusFail = false
	e.fb.mu.Unlock()
	e.fb.setRepos(e.repo)
	if p := e.preview(t, sock, "add a health check"); !p.Allowed || len(p.Files) == 0 {
		t.Fatalf("%+v", p)
	}
}

func TestRefineSelectionTamperAcrossPrincipals(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	e.d.handler.previewLimit = newActorLimiter(1000, 3600e9)
	alice, mallory, sock := e.token(t, "alice"), e.token(t, "mallory"), e.socket(t)
	text := "add a health check"
	pa := e.preview(t, alice, text)
	pm := e.preview(t, mallory, text)
	ps := e.preview(t, sock, text)
	if pa.SelectionID == pm.SelectionID || pa.SelectionID == ps.SelectionID || pm.SelectionID == ps.SelectionID {
		t.Fatal("two principals got the same selection id")
	}
	id := pa.SelectionID
	rev := func(s string) string {
		b := []byte(s)
		for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
			b[i], b[j] = b[j], b[i]
		}
		return string(b)
	}
	flip := id[:len(id)-1] + string("0123456789abcdef"[(strings.IndexByte("0123456789abcdef", id[len(id)-1])+1)%16])
	tampered := []string{
		pm.SelectionID, ps.SelectionID, // someone else's valid ids
		strings.ToUpper(id), rev(id), id[:len(id)-1], id + "0", id + " ", " " + id, id + "\x00", flip,
		"", "0", strings.Repeat("a", len(id)), strings.Repeat("a", 20000), "../" + id, id + "/../" + id,
		"' OR '1'='1", "%00" + id, id[:8] + "*",
	}
	for _, tid := range tampered {
		for who, c := range map[string]caller{"alice": alice, "mallory": mallory, "socket": sock} {
			if who == "alice" && tid == pm.SelectionID || who == "mallory" && tid == pm.SelectionID || who == "socket" && tid == ps.SelectionID {
				continue // that is the owner using its own id
			}
			code, b := e.refine(c, text, tid)
			if code == 200 || (code != 400 && code != 409 && code != 410 && code != 413) {
				t.Errorf("%s with id %.12q: %d %s", who, tid, code, b)
			}
		}
	}
	// the owner's text is bound to the selection too
	for _, other := range []string{"add a health check!", "ADD A HEALTH CHECK", text + " ", "delete everything"} {
		if code, b := e.refine(alice, other, id); code != 409 {
			t.Errorf("text %q: %d %s", other, code, b)
		}
	}
	e.assertNothingSent(t, "tampering")
	// failed attempts used up nothing: the owner's id still works, once per call
	if code, b := e.refine(alice, text, id); code != 200 {
		t.Fatalf("owner after the attempts: %d %s", code, b)
	}
	if n := e.fb.refineCount(); n != 1 {
		t.Fatalf("broker calls %d, want 1", n)
	}
	// the broker only ever saw the owner's selection id
	if got := e.fb.lastRefine(t).Selection; got != id {
		t.Fatalf("selection forwarded to the broker: %q", got)
	}
}

// taskIn makes a task in state with the given merge request columns.
func (e *mrEnv) taskIn(t *testing.T, st store.State, mrState string) int64 {
	t.Helper()
	id, err := e.st.CreateTask(context.Background(), store.Task{Repo: e.repo, Base: e.sha, Prompt: "p", Class: "docs", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET state=?, head_sha=?, mr_state=NULLIF(?,'') WHERE id=?`, string(st), strings.Repeat("a", 40), mrState, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMRRequiresPublishedDone(t *testing.T) {
	e := newMREnv(t, true)
	post := func(path string) (int, []byte) {
		c, _, b := e.sockDo(t, "POST", path, "", nil)
		return c, b
	}
	// Only a done task with a failed/skipped attempt may be retried: whatever
	// merge request state a task in any other state carries, no call is made.
	for _, st := range []store.State{store.StateQueued, store.StatePreparing, store.StateAgentRunning, store.StateCollecting,
		store.StateVerifying, store.StateNeedsApproval, store.StatePassed, store.StatePublishing,
		store.StateFailed, store.StateCancelled, store.StateError} {
		for _, mr := range []string{"", "failed", "skipped"} {
			id := e.taskIn(t, st, mr)
			if c, b := post(mrPath(id)); c != 409 || v1Code(b) != "mr_not_applicable" {
				t.Errorf("state %s mr %q: %d %s", st, mr, c, b)
			}
		}
	}
	if c, b := post(mrPath(e.taskIn(t, store.StateDone, ""))); c != 409 || v1Code(b) != "mr_not_applicable" {
		t.Errorf("done without an attempt: %d %s", c, b)
	}
	if e.fake.calls != 0 {
		t.Fatalf("the creator was called %d times for refused retries", e.fake.calls)
	}
	// malformed ids never reach it either
	for _, p := range []string{"/api/v1/tasks/abc/mr", "/api/v1/tasks/-1/mr", "/api/v1/tasks/0/mr", "/api/v1/tasks/1e3/mr",
		"/api/v1/tasks/99999999999999999999/mr", "/api/v1/tasks/1%2f..%2f2/mr", "/api/v1/tasks/ 1/mr"} {
		if c, b := post(p); c < 400 || c >= 500 {
			t.Errorf("%s: %d %s", p, c, b)
		}
	}
	// a created merge request is never overwritten, a published failure may be retried once
	created := e.taskIn(t, store.StateDone, "created")
	if c, b := post(mrPath(created)); c != 409 || v1Code(b) != "mr_already_created" {
		t.Errorf("created: %d %s", c, b)
	}
	if e.fake.calls != 0 {
		t.Fatalf("calls %d", e.fake.calls)
	}
	ok := e.taskIn(t, store.StateDone, "failed")
	if c, b := post(mrPath(ok)); c != 200 {
		t.Fatalf("control: %d %s", c, b)
	}
	if e.fake.calls != 1 {
		t.Fatalf("calls %d", e.fake.calls)
	}
}

func TestOrchdWorksWithBrokerDown(t *testing.T) {
	missing := filepath.Join(shortDir(t), "none.sock")
	deadSock := filepath.Join(shortDir(t), "dead.sock")
	ln, err := net.Listen("unix", deadSock)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // a socket file with nobody listening
	for name, sock := range map[string]string{"socket missing": missing, "listener gone": deadSock} {
		t.Run(name, func(t *testing.T) {
			prov := &mrProv{}
			e := newEnv(t, prov, 1, func(c *Config) { c.BrokerSocket = sock; c.GateHome = t.TempDir() })
			e.d.pauseWorkers()
			cfg := steps.DefaultConfig()
			cfg.Agent, cfg.Test = nopAgent{}, nopTest{}
			cfg.ReportDir, cfg.GateHome = t.TempDir(), t.TempDir()
			cfg.MR = &steps.BrokerMR{Client: &broker.Client{Socket: sock}}
			p, err := steps.NewProvider(e.st, cfg)
			if err != nil {
				t.Fatal(err)
			}
			prov.p = p

			// the everyday paths do not depend on the broker
			code, b := e.sockDo2(t, "POST", "/api/v1/tasks", e.submitBody("works without the broker"))
			if code != 201 && code != 200 {
				t.Fatalf("submit: %d %s", code, b)
			}
			if c, b := e.sockDo2(t, "GET", "/api/v1/tasks", ""); c != 200 {
				t.Fatalf("list: %d %s", c, b)
			}
			id := queuedTask(t, e)
			if c, b := e.sockDo2(t, "GET", "/api/v1/tasks/"+itoa(id), ""); c != 200 {
				t.Fatalf("get: %d %s", c, b)
			}
			if c, b := e.sockDo2(t, "POST", "/api/v1/tasks/"+itoa(id)+"/cancel", ""); c != 200 {
				t.Fatalf("cancel: %d %s", c, b)
			}
			// approval path: a parked task is released by a grant
			tid, aid, home := parked(t, e, store.KindProtectedModified, "hash1")
			grant(t, e, home, e.sha, "hash1")
			if c, b := e.sockDo2(t, "POST", "/api/v1/approvals/"+itoa(aid)+"/reverify", ""); c != 200 {
				t.Fatalf("approve: %d %s", c, b)
			}
			if state(t, e, tid) != store.StateVerifying {
				t.Fatalf("task state %s", state(t, e, tid))
			}

			// the AI path: fixed error, nothing stored, nothing leaked
			c, b := e.sockDo2(t, "POST", "/api/v1/refine/preview", jsonBody(map[string]string{"repo": e.repo, "text": "add a check"}))
			if c != 503 || v1Code(b) != "broker_unavailable" {
				t.Fatalf("preview: %d %s", c, b)
			}
			if strings.Contains(string(b), sock) || strings.Contains(string(b), "connect") {
				t.Fatalf("the error names the socket: %s", b)
			}
			c, b = e.sockDo2(t, "POST", "/api/v1/refine", jsonBody(map[string]string{"repo": e.repo, "text": "add a check", "selection_id": "abc"}))
			if c == 200 || c >= 500 && v1Code(b) != "broker_unavailable" {
				t.Fatalf("refine: %d %s", c, b)
			}

			// the MR path: the task stays done, the attempt is recorded as failed
			mre := &mrEnv{env: e, prov: prov, fake: &mrFake{}}
			done := mre.taskIn(t, store.StateDone, "failed")
			c, b = e.sockDo2(t, "POST", mrPath(done), "")
			if c != 502 || v1Code(b) != "upstream_error" {
				t.Fatalf("mr retry: %d %s", c, b)
			}
			tk, err := e.st.GetTask(context.Background(), done)
			if err != nil || tk.State != store.StateDone || tk.MRState != store.MRFailed || tk.MRCode != "broker_unavailable" || tk.MRURL != "" {
				t.Fatalf("task after the failed retry: %+v %v", tk, err)
			}
			// and the daemon is still fine afterwards
			if c, b := e.sockDo2(t, "GET", "/api/v1/tasks", ""); c != 200 {
				t.Fatalf("list after: %d %s", c, b)
			}
		})
	}
}

// sockDo2 is sockDo without the header argument and header result.
func (e *env) sockDo2(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	c, _, b := e.sockDo(t, method, path, body, nil)
	return c, b
}

func TestRefineAndMRLedgerHaveNoSecrets(t *testing.T) {
	fb, sock := startFakeBroker(t)
	home := t.TempDir()
	prov := &mrProv{}
	e := newEnv(t, prov, 1, func(c *Config) { c.BrokerSocket = sock; c.GateHome = home })
	e.d.pauseWorkers()
	fake := &mrFake{}
	cfg := steps.DefaultConfig()
	cfg.Agent, cfg.Test = nopAgent{}, nopTest{}
	cfg.ReportDir, cfg.GateHome = t.TempDir(), home
	cfg.MR = fake
	p, err := steps.NewProvider(e.st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	prov.p = p
	re := &refineEnv{env: e, fb: fb, home: home, clk: &testClock{}}
	e.d.handler.now = re.clk.now
	e.d.handler.refineLimit = newActorLimiter(1000, 3600e9)
	fb.setRepos(e.repo)
	re.commit(t, baseFiles)
	mre := &mrEnv{env: e, prov: prov, fake: fake}
	s := re.socket(t)

	const sk, gl = "sk-live-abcdef0123456789", "glpat-abcdefghij0123456789"
	text := "add a health check PROMPT-MARKER-77"
	pv := re.preview(t, s, text)
	// a refine that works, then broker failures that carry hostile text
	if c, b := re.refine(s, text, pv.SelectionID); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	for _, code := range []string{"upstream_error", "budget_exceeded", "bad_plan", "rate_limited"} {
		code := code
		fb.mu.Lock()
		fb.refineReply = func(fakeRefineCall) string {
			return fmt.Sprintf(`{"ok":false,"code":%q,"message":"%s %s http://evil.example PROMPT-MARKER-77"}`, code, sk, gl)
		}
		fb.mu.Unlock()
		re.refine(s, text, pv.SelectionID)
	}
	// merge request attempts: failures with hostile error text, then success
	for _, mk := range []func() error{
		func() error { return errors.New("upstream said " + sk + " " + gl) },
		func() error { return &steps.MRError{Code: "upstream_error"} },
		func() error { return &steps.MRError{Code: sk} }, // a code that is not a code
	} {
		id := mre.taskIn(t, store.StateDone, "failed")
		fake.set(steps.MRResult{}, mk())
		e.sockDo(t, "POST", mrPath(id), "", nil)
		tk, _ := e.st.GetTask(context.Background(), id)
		if strings.Contains(tk.MRCode, "sk-") || strings.Contains(tk.MRCode, "glpat") {
			t.Fatalf("mr_code %q", tk.MRCode)
		}
		evs, _ := e.st.Events(context.Background(), id)
		for _, ev := range evs {
			if strings.Contains(ev.Note, "sk-") || strings.Contains(ev.Note, "glpat") || strings.Contains(ev.Note, "PROMPT-MARKER") {
				t.Fatalf("event note %q", ev.Note)
			}
		}
	}
	id := mre.taskIn(t, store.StateDone, "failed")
	fake.set(steps.MRResult{IID: 9, URL: "https://gl.example/g/p/-/merge_requests/9", Created: true}, nil)
	if c, _, b := e.sockDo(t, "POST", mrPath(id), "", nil); c != 200 {
		t.Fatalf("%d %s", c, b)
	}

	lp := policy.OrchLedgerPath(home)
	raw, err := os.ReadFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{sk, gl, "sk-", "glpat", "evil.example", "PROMPT-MARKER", "SECRET-BROKER-TEXT", "health"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%q in the ledger:\n%s", leak, raw)
		}
	}
	evs, err := ledger.Events(lp)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, ev := range evs {
		kinds[ev.Type]++
	}
	if kinds["refine"] < 2 || kinds["merge_request"] < 3 {
		t.Fatalf("ledger kinds %v:\n%s", kinds, raw)
	}
	if _, err := ledger.Verify(lp); err != nil {
		t.Fatal(err)
	}
}

// A preview that says "allowed" must never be refused by the broker as
// too_large: the selection is trimmed with the broker's own size formula.
func TestPreviewSelectionFitsBrokerLimits(t *testing.T) {
	files := map[string]string{"README.md": "# proj\n"}
	long := strings.Repeat("d", 60)
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("alpha/%s/%s/%s/alpha_%02d.go", long, long, long, i)] = strings.Repeat("x", 6500)
	}
	e := newRefineEnv(t, files)
	sock := e.socket(t)
	text := "alpha " + strings.Repeat("가", 1990)
	if n := len([]rune(text)); n > 2000 {
		t.Fatalf("fixture text %d runes", n)
	}
	p := e.preview(t, sock, text)
	if !p.Allowed || len(p.Files) == 0 || len(p.Files) >= 41 {
		t.Fatalf("preview kept %d files", len(p.Files))
	}
	capped := 0
	for _, x := range p.Excluded {
		if x.Reason == refine.ReasonTotalCap {
			capped++
		}
	}
	if capped == 0 {
		t.Fatalf("nothing trimmed: %d files, excluded %+v", len(p.Files), p.Excluded)
	}
	if code, b := e.refine(sock, text, p.SelectionID); code != 200 {
		t.Fatalf("refine %d %s", code, b)
	}
	call := e.fb.lastRefine(t)
	var fs []refinesize.File
	for _, f := range call.Files {
		fs = append(fs, refinesize.File{Path: f.Path, Content: f.Content})
	}
	if in := refinesize.InputBytes(call.Requirement, fs); in > refinesize.DefaultMaxInputBytes {
		t.Fatalf("input %d over the broker's %d", in, refinesize.DefaultMaxInputBytes)
	}
	enc, err := broker.Encode("refine", map[string]any{"repo": call.Repo, "requirement": call.Requirement, "files": call.Files, "selection": call.Selection})
	if err != nil || len(enc) > broker.MaxRequestBytes {
		t.Fatalf("request %d bytes (%v)", len(enc), err)
	}
}

func (l *actorLimiter) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ts := range l.m {
		n += len(ts)
	}
	return n
}

func refineEvents(t *testing.T, home string) []ledger.Event {
	t.Helper()
	evs, err := ledger.Events(policy.OrchLedgerPath(home))
	if err != nil {
		return nil
	}
	var out []ledger.Event
	for _, ev := range evs {
		if ev.Type == "refine" {
			out = append(out, ev)
		}
	}
	return out
}

// A browser that gives up (cancelled request, closed tab) must not get its
// slot back while the broker keeps working: the call goes on without the
// request's cancellation, the outcome is ledgered and the slot stays used.
// Only a failure before the request was written refunds.
func TestRefineCancelAfterWriteKeepsSlotAndLedgers(t *testing.T) {
	e := newRefineEnv(t, baseFiles)
	srv := e.httpServer(t)
	tok := e.newToken(t, "alice")
	text := "add a health check"
	call := e.token(t, "alice")
	p := e.preview(t, call, text)
	release := make(chan struct{})
	e.fb.mu.Lock()
	e.fb.refineReply = func(c fakeRefineCall) string { <-release; return okPlan(c, nil) }
	e.fb.mu.Unlock()
	before := e.d.handler.refineLimit.total()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/v1/refine",
		strings.NewReader(jsonBody(map[string]string{"repo": e.repo, "text": text, "selection_id": p.SelectionID})))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	for i := 0; i < 200 && e.fb.refineCount() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if e.fb.refineCount() != 1 {
		t.Fatal("the broker never received the request")
	}
	cancel() // the browser goes away while the broker is working
	if err := <-done; err == nil {
		t.Fatal("request was not cancelled")
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.d.handler.refineLimit.total(); n != before+1 {
		t.Fatalf("slots %d -> %d: a cancelled request must keep its slot", before, n)
	}
	close(release)
	var evs []ledger.Event
	for i := 0; i < 300; i++ {
		if evs = refineEvents(t, e.home); len(evs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(evs) != 1 || evs[0].Payload["ok"] != true || fmt.Sprint(evs[0].Payload["input_tokens"]) != "1200" {
		t.Fatalf("ledger after the cancelled request: %+v", evs)
	}
	if n := e.d.handler.refineLimit.total(); n != before+1 {
		t.Fatalf("slots %d", n)
	}
}

func TestRefineRefundsOnlyWhenNothingWasSent(t *testing.T) {
	text := "add a health check"
	// dial failure: the socket vanished after the preview
	e := newRefineEnv(t, baseFiles)
	sock := e.socket(t)
	p := e.preview(t, sock, text)
	e.d.handler.broker.Socket = filepath.Join(shortDir(t), "gone.sock")
	before := e.d.handler.refineLimit.total()
	code, b := e.refine(sock, text, p.SelectionID)
	if code != 503 || v1Code(b) != "broker_unavailable" {
		t.Fatalf("%d %s", code, b)
	}
	if n := e.d.handler.refineLimit.total(); n != before {
		t.Fatalf("dial failure kept a slot: %d -> %d", before, n)
	}
	// broker too_large: the broker refused before spending anything
	e2 := newRefineEnv(t, baseFiles)
	s2 := e2.socket(t)
	p2 := e2.preview(t, s2, text)
	e2.fb.mu.Lock()
	e2.fb.refineReply = func(fakeRefineCall) string { return brokerErrReply("too_large") }
	e2.fb.mu.Unlock()
	before = e2.d.handler.refineLimit.total()
	if code, b := e2.refine(s2, text, p2.SelectionID); code != 400 {
		t.Fatalf("too_large: %d %s", code, b)
	}
	if n := e2.d.handler.refineLimit.total(); n != before {
		t.Fatalf("too_large kept a slot: %d -> %d", before, n)
	}
	// the broker went away after the request was written: slot stays, event says ok:false
	e3 := newRefineEnv(t, baseFiles)
	s3 := e3.socket(t)
	p3 := e3.preview(t, s3, text)
	e3.fb.mu.Lock()
	e3.fb.refineReply = func(fakeRefineCall) string { return "garbage, not json" }
	e3.fb.mu.Unlock()
	before = e3.d.handler.refineLimit.total()
	if code, _ := e3.refine(s3, text, p3.SelectionID); code != 503 {
		t.Fatalf("garbage reply: %d", code)
	}
	if n := e3.d.handler.refineLimit.total(); n != before+1 {
		t.Fatalf("a failure after the write refunded: %d -> %d", before, n)
	}
	evs := refineEvents(t, e3.home)
	if len(evs) != 1 || evs[0].Payload["ok"] != false {
		t.Fatalf("ledger: %+v", evs)
	}
}

func TestMRRetryBadURLIsTerminalOnTheServer(t *testing.T) {
	e := newMREnv(t, true)
	id := e.doneTask(t, "failed", "bad_url")
	c, _, b := e.sockDo(t, "POST", mrPath(id), "", nil)
	if c != 409 || v1Code(b) != "mr_not_applicable" || e.fake.calls != 0 {
		t.Fatalf("%d %s calls %d", c, b, e.fake.calls)
	}
	other := e.doneTask(t, "failed", "upstream_error")
	if c, _, b := e.sockDo(t, "POST", mrPath(other), "", nil); c != 200 || e.fake.calls != 1 {
		t.Fatalf("control: %d %s calls %d", c, b, e.fake.calls)
	}
}
