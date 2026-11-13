package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/orch/broker"
)

// W3h (H3): adversarial tests for orchd's side of binding a key detach to the
// signed key hash of the registration, plus the H2 review minors (403 log path
// cap, the two deploy_key_mismatch sentences). The rest is mapped in docs/33.

// orchd's copy of the broker's key-line rule (keyLineMatchesRow) over the SAME
// vector table the broker's keyLineCode is pinned to
// (internal/broker/testdata/keyline_vectors_w3h.json, TestKeyLineRuleVectorsW3h):
// kind x hash present x hash equal. The console therefore never offers a detach
// the broker can only refuse, and never hides one the broker would take.
func TestKeyLineRuleVectorsAgreeWithBrokerW3h(t *testing.T) {
	raw, err := os.ReadFile("../../broker/testdata/keyline_vectors_w3h.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct {
		Kind   string `json:"kind"`
		Stored string `json:"stored"`
		Line   string `json:"line"`
		OK     bool   `json:"ok"`
		Why    string `json:"why"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) < 20 {
		t.Fatalf("vectors: %v %d", err, len(vs))
	}
	seen := map[string]bool{}
	for _, v := range vs {
		got := keyLineMatchesRow(v.Kind, &broker.RemoteLink{Path: "team/x", KeySHA256: v.Stored}, v.Line)
		if got != v.OK {
			t.Errorf("%s stored=%.8q line=%.40q: orchd says %v, table %v (%s)", v.Kind, v.Stored, v.Line, got, v.OK, v.Why)
		}
		c := "absent"
		if v.Stored != "" {
			c = "mismatch"
			if v.Stored == keyHash(v.Line) {
				c = "equal"
			}
		}
		seen[v.Kind+"/"+c] = true
	}
	for _, k := range []string{"github", "gitlab", "gitea"} {
		for _, c := range []string{"absent", "equal", "mismatch"} {
			if !seen[k+"/"+c] {
				t.Fatalf("the vector table lacks %s/%s", k, c)
			}
		}
	}
}

// Refusal-code priority when the remote changed AND the key line does not match
// the row's hash: remote_changed wins before any signature, like the broker
// (TestRemoteChangedOutranksKeyMismatchW3h in internal/broker). Nothing reaches
// the broker.
//
// One known difference is pinned here so a change is noticed: remove_deploy_key
// on a row whose remote has NO number (provider set, id null: no create/link
// path writes such a row; the registry allows it) is refused by orchd with
// deploy_key_mismatch when the line does not match, where the broker would say
// remote_changed. Both are definite refusals before the signature with nothing
// changed; only the code differs.
func TestRemoteChangedOutranksKeyMismatchW3h(t *testing.T) {
	e := newDelEnv(t)
	other := keyHash("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOtherOther other")
	// GitLab row without its remote number, carrying a hash the shared .pub does not match
	e.addDel(t, delProject{id: "gl-nonum", origin: "created", prov: "gl", remote: "team/gl-nonum", keySHA: other})
	// the same without any hash (an older GitLab row)
	e.addDel(t, delProject{id: "gl-nonum-old", origin: "created", prov: "gl", remote: "team/gl-nonum-old", noKeySHA: true})
	// GitHub row with a number and a mismatching hash (asked about another remote path)
	e.addDel(t, delProject{id: "gh-moved", origin: "created", prov: "gh", remote: "Octo-Cat/gh-moved", remoteID: 31, withKey: true, keySHA: other})
	e.addDel(t, delProject{id: "gl-moved", origin: "created", prov: "gl", remote: "team/gl-moved", remoteID: 32, keySHA: other})
	for _, id := range []string{"gl-nonum", "gl-nonum-old"} {
		if pv := e.preview(t, id); pv["detach_key_available"] != false {
			t.Errorf("%s: preview offers the detach: %v", id, pv)
		}
		if c, b := e.proposeDel(t, "remove_project", id, rmDetach(true)); c != 409 || v1Code(b) != "remote_changed" {
			t.Errorf("%s: propose detach %d %s (want 409 remote_changed)", id, c, b)
		}
	}
	for _, c := range []struct{ id, asked string }{{"gh-moved", "Octo-Cat/elsewhere"}, {"gl-moved", "team/elsewhere"}} {
		if code, b := e.proposeDel(t, "remove_deploy_key", c.id, map[string]any{"remote_path": c.asked}); code != 409 || v1Code(b) != "remote_changed" {
			t.Errorf("%s: remove_deploy_key %d %s (want 409 remote_changed)", c.id, code, b)
		}
		// the same path: the mismatch is what remains
		if code, b := e.proposeDel(t, "remove_deploy_key", c.id, map[string]any{"remote_path": strings.Replace(c.asked, "elsewhere", c.id, 1)}); code != 409 || v1Code(b) != "deploy_key_mismatch" {
			t.Errorf("%s: remove_deploy_key on its own path %d %s (want 409 deploy_key_mismatch)", c.id, code, b)
		}
	}
	// the pinned difference (see above)
	if code, b := e.proposeDel(t, "remove_deploy_key", "gl-nonum", map[string]any{"remote_path": "team/gl-nonum"}); code != 409 || v1Code(b) != "deploy_key_mismatch" {
		t.Errorf("gl-nonum remove_deploy_key: %d %s", code, b)
	}
	if n := len(e.writes()); n != 0 {
		t.Fatalf("the broker was asked %d times", n)
	}
}

// The W3g residual scenario, now refused from orchd's side: the key line orchd
// would send is not the one the row was registered with (the shared GitLab
// .pub, or a GitHub project's key folder .pub, replaced by another key). The
// preview does not offer the detach, propose is refused before any signature
// (409 deploy_key_mismatch, no change hash), and a proposal made while the line
// still matched is refused at apply BEFORE the broker is asked. Nothing reaches
// the broker, local files stay. (The broker's half, a valid signature with a
// forged line refused before any provider contact, is
// TestUnsignedKeyLineWideningNowRefusedW3h and TestKeyHashChangedBetweenShapeAndLockIsRefusedW3h.)
func TestForgedKeyLineRefusedBeforeSignatureAndBrokerW3h(t *testing.T) {
	forged := func(t *testing.T) string {
		l, _ := sshPubLine(t, "orchd-deploy")
		return l
	}
	t.Run("gitlab shared key", func(t *testing.T) {
		e := newDelEnv(t)
		path := e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 21})
		p := e.mustPropose(t, "remove_project", "gl-one", rmDetach(true))
		pk := e.mustPropose(t, "remove_deploy_key", "gl-one", map[string]any{"remote_path": "team/gl-one"})
		writeFile(t, e.key+".pub", forged(t)+"\n")
		forgedChecks(t, e, "gl-one", "team/gl-one", path, p, pk)
	})
	t.Run("github key folder", func(t *testing.T) {
		e := newDelEnv(t)
		path := e.addDel(t, delProject{id: "gh-one", origin: "created", prov: "gh", remote: "Octo-Cat/gh-one", remoteID: 31})
		line := e.addKeyDir(t, "gh-one")
		e.setKeySHA(t, "gh-one", keyHash(line))
		p := e.mustPropose(t, "remove_project", "gh-one", rmDetach(true))
		pk := e.mustPropose(t, "remove_deploy_key", "gh-one", map[string]any{"remote_path": "Octo-Cat/gh-one"})
		writeFile(t, filepath.Join(e.keysDir(), "gh-one", "id_ed25519.pub"), forged(t)+"\n")
		forgedChecks(t, e, "gh-one", "Octo-Cat/gh-one", path, p, pk)
	})
}

func forgedChecks(t *testing.T, e *delEnv, id, remote, path string, p, pk w3cProposal) {
	t.Helper()
	if pv := e.preview(t, id); pv["detach_key_available"] != false || pv["deploy_key_fingerprint"] != nil {
		t.Errorf("preview offers the detach: %v", pv)
	}
	if c, b := e.proposeDel(t, "remove_project", id, rmDetach(true)); c != 409 || v1Code(b) != "deploy_key_mismatch" || strings.Contains(string(b), `"change"`) {
		t.Errorf("propose detach: %d %s", c, b)
	}
	// each op answers with its own sentence: only the detach one says to turn the option off
	if c, b := e.proposeDel(t, "remove_deploy_key", id, map[string]any{"remote_path": remote}); c != 409 || v1Code(b) != "deploy_key_mismatch" || strings.Contains(string(b), "를 끄고") {
		t.Errorf("propose remove_deploy_key: %d %s", c, b)
	}
	if c, b := e.applyDel(t, p); c != 409 || v1Code(b) != "deploy_key_mismatch" || !strings.Contains(string(b), "를 끄고") {
		t.Errorf("apply detach: %d %s", c, b)
	}
	if c, b := e.applyDel(t, pk); c != 409 || v1Code(b) != "deploy_key_mismatch" || strings.Contains(string(b), "를 끄고") {
		t.Errorf("apply remove_deploy_key: %d %s", c, b)
	}
	if n := len(e.writes()); n != 0 {
		t.Fatalf("the broker was asked %d times", n)
	}
	if !exists(filepath.Join(path, ".git")) || !e.hasRow(id) {
		t.Fatal("local files or the row changed")
	}
	// turning the detach off still works (the documented way out)
	if c, b := e.proposeDel(t, "remove_project", id, rmDetach(false)); c != 200 {
		t.Errorf("detach off: %d %s", c, b)
	}
}

// H2 review minor 4: the logged path is capped at 256 bytes with a visible
// marker (a 300-byte path).
func TestRefusalLogTruncatesLongPathW3h(t *testing.T) {
	logs := captureLog(t)
	path := "/api/v1/" + strings.Repeat("a", 292)
	if len(path) != 300 {
		t.Fatalf("fixture %d", len(path))
	}
	before := len(refusalLines(logs.String()))
	logRefusals(v1Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeErr(w, errCSRF) }))).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil))
	got := refusalLines(logs.String())[before:]
	want := "orchd: refused 403 csrf_failed POST " + path[:256] + "..."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("log %q\nwant %q", got, want)
	}
	// exactly at the cap: no marker
	before = len(refusalLines(logs.String()))
	exact := path[:256]
	logRefusals(v1Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeErr(w, errCSRF) }))).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", exact, nil))
	if got := refusalLines(logs.String())[before:]; len(got) != 1 || got[0] != "orchd: refused 403 csrf_failed POST "+exact {
		t.Fatalf("at the cap: %q", got)
	}
}

// H2 review minor 5: the two deploy_key_mismatch answers are different
// sentences. Only the detach one tells the user to turn the detach option off;
// the remove_deploy_key one (there is no such option there) does not.
func TestKeyMismatchTextsDifferW3h(t *testing.T) {
	d, r := errKeyMismatchDetach, errKeyMismatchRemoval
	if d.msg == r.msg {
		t.Fatal("the two sentences are the same")
	}
	for _, e := range []*httpError{d, r} {
		if e.code != http.StatusConflict || e.v1Code != "deploy_key_mismatch" || !strings.HasSuffix(e.msg, "변경 없음") {
			t.Errorf("%+v", e)
		}
	}
	if !strings.Contains(d.msg, `"배포 키도 함께 떼기"를 끄고`) {
		t.Errorf("detach sentence lacks the way out: %s", d.msg)
	}
	for _, s := range []string{"함께 떼기", "끄고"} {
		if strings.Contains(r.msg, s) {
			t.Errorf("remove_deploy_key sentence mentions %q: %s", s, r.msg)
		}
	}
	if !strings.Contains(r.msg, "직접 제거") {
		t.Errorf("remove_deploy_key sentence lacks the manual way: %s", r.msg)
	}
}
