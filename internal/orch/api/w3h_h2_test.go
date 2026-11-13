package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/broker"
)

// W3h Task 2 (H2): orchd reads the remote's key_sha256 (registry v5), offers a
// key detach only when its key line hashes to it (GitLab rows without one: no;
// GitHub rows without one: the W3g rule), maps the broker's deploy_key_mismatch
// of a deletion to a definite refusal, and logs one fixed line per HTTP 403.

func TestProjectsViewAcceptsKeySHA256(t *testing.T) {
	h := keyHash("ssh-ed25519 AAAA x")
	var v broker.RegistryView
	raw := `{"projects":[{"id":"gl-one","path":"/r/gl-one","kind":"registered","ai_context":false,"provider":"gl",` +
		`"remote":{"path":"team/gl-one","id":21,"key_sha256":"` + h + `","later_field":1},"auto_merge":{"enabled":false},"origin":"created"},` +
		`{"id":"old-one","path":"/r/old-one","kind":"registered","ai_context":false,"provider":"gl","remote":{"path":"team/old-one","id":22},` +
		`"auto_merge":{"enabled":false},"origin":"created"}],"providers":[],"orphans":[]}`
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Projects) != 2 || v.Projects[0].Remote == nil || v.Projects[0].Remote.KeySHA256 != h || v.Projects[1].Remote.KeySHA256 != "" {
		t.Fatalf("%+v", v.Projects)
	}
	// the console's registry view is unchanged by it (and never carries it)
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 21, keySHA: h})
	e.addDel(t, delProject{id: "gl-old", origin: "created", prov: "gl", remote: "team/gl-old", remoteID: 22, noKeySHA: true})
	c, b := e.sockJSON(t, "GET", "/api/v1/projects/registry", "")
	if c != 200 || !strings.Contains(string(b), `"gl-one"`) || !strings.Contains(string(b), `"gl-old"`) || strings.Contains(string(b), "key_sha256") || strings.Contains(string(b), h) {
		t.Fatalf("registry view: %d %s", c, b)
	}
}

// preview reads the removal preview of id.
func (d *delEnv) preview(t *testing.T, id string) map[string]any {
	t.Helper()
	d.resetDeleteLimits()
	c, b := d.sockJSON(t, "GET", "/api/v1/projects/"+id+"/removal-preview", "")
	if c != 200 {
		t.Fatalf("preview %s: %d %s", id, c, b)
	}
	var pv map[string]any
	if err := json.Unmarshal(b, &pv); err != nil {
		t.Fatal(err)
	}
	return pv
}

func TestDetachKeyAvailableNeedsMatchingHash(t *testing.T) {
	e := newDelEnv(t)
	other := keyHash("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOtherOther other")
	shared := keyHash(e.pubLine)
	e.addDel(t, delProject{id: "gl-same", origin: "created", prov: "gl", remote: "team/gl-same", remoteID: 21, keySHA: shared})
	e.addDel(t, delProject{id: "gl-diff", origin: "created", prov: "gl", remote: "team/gl-diff", remoteID: 22, keySHA: other})
	e.addDel(t, delProject{id: "gl-none", origin: "created", prov: "gl", remote: "team/gl-none", remoteID: 23, noKeySHA: true})
	// the comment is part of the line: the same key body with another comment is another line
	e.addDel(t, delProject{id: "gl-comment", origin: "created", prov: "gl", remote: "team/gl-comment", remoteID: 24,
		keySHA: keyHash(strings.Join(strings.Fields(e.pubLine)[:2], " ") + " another-comment")})
	e.addDel(t, delProject{id: "gh-none", origin: "linked", prov: "gh", remote: "Octo-Cat/gh-none", remoteID: 31, withKey: true})
	e.addDel(t, delProject{id: "gh-same", origin: "created", prov: "gh", remote: "Octo-Cat/gh-same", remoteID: 32})
	ghLine := e.addKeyDir(t, "gh-same")
	e.setKeySHA(t, "gh-same", keyHash(ghLine))
	e.addDel(t, delProject{id: "gh-diff", origin: "created", prov: "gh", remote: "Octo-Cat/gh-diff", remoteID: 33, withKey: true, keySHA: other})
	for _, c := range []struct {
		id    string
		avail bool
	}{
		{"gl-same", true}, {"gl-diff", false}, {"gl-none", false}, {"gl-comment", false},
		{"gh-none", true}, {"gh-same", true}, {"gh-diff", false},
	} {
		pv := e.preview(t, c.id)
		if pv["detach_key_available"] != c.avail {
			t.Errorf("%s: preview detach_key_available %v, want %v", c.id, pv["detach_key_available"], c.avail)
		}
		if !c.avail && (pv["deploy_key_fingerprint"] != nil || pv["deploy_key_title"] != nil || pv["deploy_key_shared"] != false) {
			t.Errorf("%s: a key identity for a detach that is not offered: %v", c.id, pv)
		}
		// propose with detach_key=false says the same
		code, b := e.proposeDel(t, "remove_project", c.id, rmDetach(false))
		var resp map[string]any
		_ = json.Unmarshal(b, &resp)
		if code != 200 || resp["detach_key_available"] != c.avail {
			t.Errorf("%s: propose detach off: %d %s", c.id, code, b)
		}
		// detach_key=true: refused before any signature when the hash does not match
		code, b = e.proposeDel(t, "remove_project", c.id, rmDetach(true))
		if c.avail && code != 200 {
			t.Errorf("%s: propose detach: %d %s", c.id, code, b)
		}
		if !c.avail && (code != 409 || v1Code(b) != "deploy_key_mismatch" || strings.Contains(string(b), `"change"`)) {
			t.Errorf("%s: propose detach: %d %s (want 409 deploy_key_mismatch)", c.id, code, b)
		}
		// remove_deploy_key follows the same rule before the signature
		remote := map[string]string{"gl-same": "team/gl-same", "gl-diff": "team/gl-diff", "gl-none": "team/gl-none", "gl-comment": "team/gl-comment",
			"gh-none": "Octo-Cat/gh-none", "gh-same": "Octo-Cat/gh-same", "gh-diff": "Octo-Cat/gh-diff"}[c.id]
		code, b = e.proposeDel(t, "remove_deploy_key", c.id, map[string]any{"remote_path": remote})
		if c.avail && code != 200 {
			t.Errorf("%s: propose remove_deploy_key: %d %s", c.id, code, b)
		}
		if !c.avail && (code != 409 || v1Code(b) != "deploy_key_mismatch") {
			t.Errorf("%s: propose remove_deploy_key: %d %s (want 409 deploy_key_mismatch)", c.id, code, b)
		}
	}
	// a mismatch found again at apply (the hash changed after propose) is refused before the broker is asked
	p := e.mustPropose(t, "remove_project", "gl-same", rmDetach(true))
	pk := e.mustPropose(t, "remove_deploy_key", "gl-same", map[string]any{"remote_path": "team/gl-same"})
	e.setKeySHA(t, "gl-same", other)
	if c, b := e.applyDel(t, p); c != 409 || v1Code(b) != "deploy_key_mismatch" {
		t.Fatalf("apply after the hash changed: %d %s", c, b)
	}
	if c, b := e.applyDel(t, pk); c != 409 || v1Code(b) != "deploy_key_mismatch" {
		t.Fatalf("remove_deploy_key apply after the hash changed: %d %s", c, b)
	}
	if n := len(e.writes()); n != 0 {
		t.Fatalf("the broker was asked %d times", n)
	}
	if !exists(filepath.Join(e.root, "gl-same", ".git")) {
		t.Fatal("the clone was touched")
	}
}

func TestDeployKeyMismatchIsDefiniteRefusalNotUnknownOutcome(t *testing.T) {
	e := newDelEnv(t)
	// the row's hash matches orchd's line, so orchd sends; the broker still refuses
	// (e.g. it holds another hash than the projects read showed)
	path := e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 21, withKey: true})
	ghPath := e.addDel(t, delProject{id: "gh-one", origin: "created", prov: "gh", remote: "Octo-Cat/gh-one", remoteID: 31, withKey: true})
	e.setOverride(func(op string, _ json.RawMessage) string {
		if op == "remove_project" || op == "remove_deploy_key" {
			return errReply("deploy_key_mismatch")
		}
		return ""
	})
	for _, c := range []struct {
		action, id, path string
		fields           map[string]any
	}{
		{"remove_project", "gl-one", path, rmDetach(true)},
		{"remove_project", "gh-one", ghPath, rmDetach(true)},
		{"remove_deploy_key", "gl-one", path, map[string]any{"remote_path": "team/gl-one"}},
		{"remove_deploy_key", "gh-one", ghPath, map[string]any{"remote_path": "Octo-Cat/gh-one"}},
	} {
		e.resetDeleteLimits()
		before := len(e.writes())
		p := e.mustPropose(t, c.action, c.id, c.fields)
		code, b := e.applyDel(t, p)
		if code != http.StatusConflict || v1Code(b) != "deploy_key_mismatch" {
			t.Fatalf("%s %s: %d %s (want a definite 409 deploy_key_mismatch)", c.action, c.id, code, b)
		}
		var env struct {
			Error struct{ Message string }
		}
		_ = json.Unmarshal(b, &env)
		if !strings.Contains(env.Error.Message, "변경 없음") || strings.Contains(env.Error.Message, "알 수 없") {
			t.Fatalf("%s %s: message %q", c.action, c.id, env.Error.Message)
		}
		if n := len(e.writes()) - before; n != 1 {
			t.Fatalf("%s %s: %d broker calls", c.action, c.id, n)
		}
		// nothing local was removed
		if !exists(filepath.Join(c.path, ".git")) || !exists(filepath.Join(e.keysDir(), c.id, "id_ed25519.pub")) {
			t.Fatalf("%s %s: local files were touched", c.action, c.id)
		}
	}
	// the mapping is for these two actions only: elsewhere an unknown code stays an unknown outcome
	if he := deleteBrokerError("delete_remote_repo", &broker.OpError{Code: "deploy_key_mismatch"}); he.v1Code != "outcome_unknown" {
		t.Fatalf("delete_remote_repo: %+v", he)
	}
	for _, a := range []string{"remove_project", "remove_deploy_key"} {
		if he := deleteBrokerError(a, &broker.OpError{Code: "deploy_key_mismatch"}); he.code != http.StatusConflict || he.v1Code != "deploy_key_mismatch" {
			t.Fatalf("%s: %+v", a, he)
		}
	}
}

// ---- H-D6: the 403 refusal log ----

// refusalLines returns the "orchd: refused" lines of the captured log, without the log's time prefix.
func refusalLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if i := strings.Index(l, "orchd: refused"); i >= 0 {
			out = append(out, l[i:])
		}
	}
	return out
}

// waitRefusal waits (bounded) until the log holds n refusal lines.
func waitRefusal(t *testing.T, logs *syncBuf, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := refusalLines(logs.String())
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// every403 is each 403 answer orchd can give, as its error value, with the code the log must name.
func every403() []struct {
	err  error
	code string
} {
	return []struct {
		err  error
		code string
	}{
		{errBadSignature, "bad_signature"}, {errProjBadSignature, "bad_signature"},
		{errStale, "stale_signature"}, {errProjStale, "stale_signature"},
		{errUnknownSigner, "unknown_signer"}, {errProjUnknownSigner, "unknown_signer"},
		{errCSRF, "csrf_failed"}, {errForbiddenUID, "forbidden_uid"},
		{errDestructive, "forbidden"}, {errRefineRepoNotAllowed, "repo_not_allowed"}, {errSocketOnly, "socket_only"},
		// the code-less ones (repository roots, api.go): the envelope's "forbidden"
		{&httpError{code: http.StatusForbidden, msg: "허용된 저장소 루트 밖의 경로입니다"}, "forbidden"},
	}
}

func TestRefused403LogsReasonAndPathOnly(t *testing.T) {
	logs := captureLog(t)
	// every 403 value, through the same layers as a real request: the v1 envelope and the legacy flat body
	for _, c := range every403() {
		for _, path := range []string{"/api/v1/projects/apply", "/tasks"} {
			before := len(refusalLines(logs.String()))
			var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeErr(w, c.err) })
			if isV1Path(path) {
				inner = v1Wrap(inner)
			}
			rec := httptest.NewRecorder()
			logRefusals(inner).ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s: status %d", c.code, path, rec.Code)
			}
			got := refusalLines(logs.String())[before:]
			if want := "orchd: refused 403 " + c.code + " POST " + path; len(got) != 1 || got[0] != want {
				t.Errorf("%s %s: log %q, want %q", c.code, path, got, want)
			}
		}
	}
	// refuse() (bad_origin) writes outside the handler: the same line
	before := len(refusalLines(logs.String()))
	rec := httptest.NewRecorder()
	logRefusals(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refuse(w, r, http.StatusForbidden, "bad_origin", "허용되지 않은 출처의 요청입니다")
	})).ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/auth/session", nil))
	if got := refusalLines(logs.String())[before:]; len(got) != 1 || got[0] != "orchd: refused 403 bad_origin GET /api/v1/auth/session" {
		t.Errorf("bad_origin: %q", got)
	}
	// other statuses are not logged by it
	before = len(refusalLines(logs.String()))
	for _, he := range []*httpError{errUnauthenticated, errProjectBusy, errRemoveUnknownRes, errProjBadBody} {
		logRefusals(v1Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeErr(w, he) }))).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/x", nil))
	}
	logRefusals(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"code": "bad_signature"})
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/x", nil))
	if got := refusalLines(logs.String())[before:]; len(got) != 0 {
		t.Errorf("non-403 answers were logged: %q", got)
	}

	// end to end, on both listeners
	// (1) the socket: a peer whose uid is not allowed
	t.Run("socket forbidden_uid", func(t *testing.T) {
		foreign := uint32(os.Getuid()) + 1
		usePeerUID(t, func(net.Conn) (uint32, error) { return foreign, nil })
		e := newEnv(t, noSteps{}, 1)
		before := len(refusalLines(logs.String()))
		if c, b := e.do(t, "GET", "/api/v1/tasks?state=queued", ""); c != 403 || v1Code(b) != "forbidden_uid" {
			t.Fatalf("%d %s", c, b)
		}
		if got := waitRefusal(t, logs, before+1)[before:]; len(got) != 1 || got[0] != "orchd: refused 403 forbidden_uid GET /api/v1/tasks" {
			t.Fatalf("%q", got)
		}
	})
	// (2) the HTTP listener: CSRF and origin refusals
	t.Run("http csrf_failed and bad_origin", func(t *testing.T) {
		w := newWebEnv(t)
		tok, _ := w.newSession(t, "alice")
		before := len(refusalLines(logs.String()))
		if c, _, b := w.web(t, "POST", "/api/v1/tasks", `{}`, hdr{"Cookie": cookie(tok), "Origin": webOrigin, "Content-Type": "application/json", CSRFHeader: "wrong"}); c != 403 || v1Code(b) != "csrf_failed" {
			t.Fatalf("csrf: %d %s", c, b)
		}
		if c, _, b := w.web(t, "POST", "/api/v1/tasks", `{}`, hdr{"Cookie": cookie(tok), "Origin": "https://evil.example", "Content-Type": "application/json"}); c != 403 || v1Code(b) != "bad_origin" {
			t.Fatalf("origin: %d %s", c, b)
		}
		got := waitRefusal(t, logs, before+2)[before:]
		if len(got) != 2 || got[0] != "orchd: refused 403 csrf_failed POST /api/v1/tasks" || got[1] != "orchd: refused 403 bad_origin POST /api/v1/tasks" {
			t.Fatalf("%q", got)
		}
	})
	// (3) the broker's signature refusals of a signed apply
	t.Run("signature refusals", func(t *testing.T) {
		e := newDelEnv(t)
		e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 3})
		for _, code := range []string{"bad_signature", "stale_signature", "unknown_signer"} {
			e.resetDeleteLimits()
			e.setOverride(func(op string, _ json.RawMessage) string {
				if op == "remove_project" {
					return errReply(code)
				}
				return ""
			})
			p := e.mustPropose(t, "remove_project", "app-one", rmDetach(false))
			before := len(refusalLines(logs.String()))
			if c, b := e.applyDel(t, p); c != 403 || v1Code(b) != code {
				t.Fatalf("%s: %d %s", code, c, b)
			}
			if got := waitRefusal(t, logs, before+1)[before:]; len(got) != 1 || got[0] != "orchd: refused 403 "+code+" POST /api/v1/projects/remove/apply" {
				t.Fatalf("%s: %q", code, got)
			}
		}
	})
}

func TestRefusalLogHasNoSecrets(t *testing.T) {
	logs := captureLog(t)
	const marker = "SECRETMARKER"
	w := newWebEnv(t)
	tok, csrf := w.newSession(t, "alice")
	cases := []struct {
		name, method, path, body, want string
		h                              hdr
	}{
		{"csrf with a query, headers and a body", "POST", "/api/v1/tasks?token=" + marker + "&sig=" + marker, `{"prompt":"` + marker + `","sig":"` + marker + `"}`,
			"orchd: refused 403 csrf_failed POST /api/v1/tasks",
			hdr{"Cookie": cookie(tok), "Origin": webOrigin, "Content-Type": "application/json", CSRFHeader: marker, "X-Extra": marker}},
		{"origin with a query", "GET", "/api/v1/auth/session?k=" + marker, "", "orchd: refused 403 bad_origin GET /api/v1/auth/session",
			hdr{"Cookie": cookie(tok), "Origin": "https://" + strings.ToLower(marker) + ".example", "X-Extra": marker}},
		// a path cannot smuggle a line break or a marker-free form into the line: it is logged escaped
		{"encoded line break in the path", "POST", "/api/v1/tasks/%0aorchd:%20refused%20403%20x/cancel", "", "orchd: refused 403 csrf_failed POST /api/v1/tasks/%0aorchd:%20refused%20403%20x/cancel",
			hdr{"Cookie": cookie(tok), "Origin": webOrigin}},
	}
	for _, c := range cases {
		before := len(refusalLines(logs.String()))
		if code, _, b := w.web(t, c.method, c.path, c.body, c.h); code != 403 {
			t.Fatalf("%s: %d %s", c.name, code, b)
		}
		got := waitRefusal(t, logs, before+1)[before:]
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// the signed apply of a deletion: the signature, change, request and key line never reach the log
	e := newDelEnv(t)
	line := ""
	e.addDel(t, delProject{id: "gh-one", origin: "created", prov: "gh", remote: "Octo-Cat/gh-one", remoteID: 3})
	line = e.addKeyDir(t, "gh-one")
	e.setOverride(func(op string, _ json.RawMessage) string {
		if op == "remove_project" {
			return errReply("bad_signature")
		}
		return ""
	})
	p := e.mustPropose(t, "remove_project", "gh-one", rmDetach(true))
	before := len(refusalLines(logs.String()))
	if c, b := e.applyDel(t, p); c != 403 {
		t.Fatalf("apply: %d %s", c, b)
	}
	waitRefusal(t, logs, before+1)
	all := logs.String()
	for _, s := range []string{marker, strings.ToLower(marker), csrf, tok, p.Change, p.Request, line, strings.Fields(line)[1], e.pubLine, "token="} {
		if strings.Contains(all, s) {
			t.Errorf("the log holds %q:\n%s", s, all)
		}
	}
	for _, l := range refusalLines(all) {
		if strings.Count(l, " ") != 5 || strings.ContainsAny(l, "?\r") {
			t.Errorf("not the fixed shape: %q", l)
		}
	}
}
