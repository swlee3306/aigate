package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/orch/store"
)

// The web console's grant request (internal/orch/web/static/webauthn.js
// signApproval) against the real verifier and the real grant handler.
//
// fixtures/grant_request.json is PRODUCED by the node test
// internal/orch/web/test/approvals.test.js ("grant request golden shared with
// Go") from the JS builder over fixtures/approval.json, with a fixed test-only
// P-256 key, and CONSUMED here. The request shape is:
//
//	{"signer": <signer id>, "alg": "webauthn-es256",
//	 "sig": <base64url, no padding: DER ECDSA signature as the authenticator returned it>,
//	 "issued": <detail.issued, unchanged>,
//	 "webauthn": {"authenticator_data": <base64url>, "client_data_json": <base64url>}}
//
// where the WebAuthn challenge is the 32 raw bytes of detail.digest
// (= signing.Message.Digest()). Drift fails either side: the node test
// compares its fresh output with the committed file, and this file decodes it
// strictly and requires the live body below to have the same key set.

type goldenGrant struct {
	Authenticator struct {
		Signer       string `json:"signer"`
		CredentialID string `json:"credential_id"`
		RPID         string `json:"rp_id"`
		Origin       string `json:"origin"`
		PublicKey    string `json:"public_key"`
	} `json:"authenticator"`
	Detail struct {
		Message string `json:"message"`
		Digest  string `json:"digest"`
		Issued  string `json:"issued"`
	} `json:"detail"`
	Request json.RawMessage `json:"request"`
}

// webGrantBody is the exact JS request shape (unknown fields refused).
type webGrantBody struct {
	Signer   string `json:"signer"`
	Alg      string `json:"alg"`
	Sig      string `json:"sig"`
	Issued   string `json:"issued"`
	WebAuthn struct {
		AuthenticatorData string `json:"authenticator_data"`
		ClientDataJSON    string `json:"client_data_json"`
	} `json:"webauthn"`
}

func readGoldenGrant(t *testing.T) goldenGrant {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, "grant_request.json"))
	if err != nil {
		t.Fatalf("grant_request.json: %v (produced by the node test with ORCH_UPDATE_FIXTURES=1)", err)
	}
	var g goldenGrant
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func strictGrantBody(t *testing.T, raw []byte) webGrantBody {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w webGrantBody
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("JS grant body does not have the documented shape: %v\n%s", err, raw)
	}
	return w
}

// keyShape is the recursive key set of a JSON object.
func keyShape(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any) any
	walk = func(v any) any {
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Sprintf("%T", v)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []any{}
		for _, k := range keys {
			out = append(out, k, walk(m[k]))
		}
		return out
	}
	b, _ := json.Marshal(walk(v))
	return string(b)
}

// TestWebGrantRequestGolden: the body the JS builder produced is accepted by
// the real verifier (signing.Verify) for the message it was built from.
func TestWebGrantRequestGolden(t *testing.T) {
	g := readGoldenGrant(t)
	w := strictGrantBody(t, g.Request)
	var req grantRequest // the handler's own type
	if err := json.Unmarshal(g.Request, &req); err != nil {
		t.Fatal(err)
	}
	if req.Signer != w.Signer || req.Alg != signing.TypeWebAuthnES256 || req.Sig != w.Sig || req.Issued != w.Issued ||
		req.WebAuthn == nil || req.WebAuthn.AuthenticatorData != w.WebAuthn.AuthenticatorData ||
		req.WebAuthn.ClientDataJSON != w.WebAuthn.ClientDataJSON {
		t.Fatalf("handler type reads the body differently: %+v vs %+v", req, w)
	}
	msg, err := signing.ParseMessage(g.Detail.Message)
	if err != nil {
		t.Fatal(err)
	}
	d := msg.Digest()
	if hex.EncodeToString(d[:]) != g.Detail.Digest {
		t.Fatal("golden digest is not sha256(message)")
	}
	if issued, ok := parseIssued(req.Issued); !ok || !issued.Equal(msg.Issued) || req.Issued != g.Detail.Issued {
		t.Fatalf("issued %q is not the message's", req.Issued)
	}
	a := g.Authenticator
	reg := signing.Registry{Signers: []signing.Signer{{ID: a.Signer, Type: signing.TypeWebAuthnES256, PublicKey: a.PublicKey,
		CredentialID: a.CredentialID, RPID: a.RPID, Origin: a.Origin}}}
	if err := signing.Verify(reg, msg, req.Signature, signing.VerifyOptions{SkipFreshness: true}); err != nil {
		t.Fatalf("real verifier refused the JS body: %v", err)
	}
	// Control: the same signature over another message (one second later) fails.
	other := msg
	other.Issued = msg.Issued.Add(time.Second)
	if err := signing.Verify(reg, other, req.Signature, signing.VerifyOptions{SkipFreshness: true}); err == nil {
		t.Fatal("signature accepted for a different message")
	}
}

// jsSignApproval mirrors webauthn.js signApproval: the canonical message must
// parse and match the detail, sha256(message) must equal digest, and the
// challenge is the digest's raw bytes. It returns the body in the JS shape.
func jsSignApproval(t *testing.T, d ApprovalDetail, signer string, assert func(challenge []byte) map[string]any) map[string]any {
	t.Helper()
	m, err := signing.ParseMessage(d.Message)
	if err != nil || m.Kind != d.Kind || m.Task != d.TaskID || m.Base != d.Base || m.Hash != d.Hash ||
		m.Issued.Format(time.RFC3339) != d.Issued {
		t.Fatalf("message does not match the detail: %v", err)
	}
	if fixtureRepoID(d.Repo) != m.Repo { // webauthn.js repoIdOf
		t.Fatalf("message repo %s is not sha256(%q)[:12]", m.Repo, d.Repo)
	}
	sum := sha256.Sum256([]byte(d.Message))
	challenge, err := hex.DecodeString(d.Digest)
	if err != nil || len(challenge) != 32 || !bytes.Equal(challenge, sum[:]) {
		t.Fatal("digest is not sha256(message)")
	}
	r := assert(challenge)["response"].(map[string]any)
	return map[string]any{
		"signer": signer, "alg": "webauthn-es256", "sig": r["signature"], "issued": d.Issued,
		"webauthn": map[string]any{"authenticator_data": r["authenticatorData"], "client_data_json": r["clientDataJSON"]},
	}
}

// TestWebSignedGrantEndToEnd: browser path (passkey session + CSRF) -> detail
// -> software authenticator signs the digest -> grant with exactly the JS
// request shape -> the real handler grants and the task moves on.
func TestWebSignedGrantEndToEnd(t *testing.T) {
	g := readGoldenGrant(t)
	gf := newGrantFixture(t)
	pk := newPasskey(t)
	phone := pk.signer("phone", loginRPID, loginOrigin)
	writeRegistry(t, gf.reg, gf.alice.signer, phone)
	h := gf.e.d.handler
	h.rpID, h.origin = regRPID, regOrigin // before the first request
	f := &waFixture{e: gf.e, srv: gf.e.httpServer(t), clock: &testClock{}, reg: gf.reg, pk: pk, phone: phone, alice: gf.alice}
	s := f.login(t)
	auth := hdr{"Cookie": cookie(s.token), CSRFHeader: s.csrf}

	tid, aid, _ := gf.protectedTask(t, "line1\nWEB\n")
	code, _, b := httpDo(t, f.srv, "GET", fmt.Sprintf("/api/v1/approvals/%d", aid), "", hdr{"Cookie": cookie(s.token)})
	var d ApprovalDetail
	if code != 200 || json.Unmarshal(b, &d) != nil || !d.RequiresSignature || d.Message == "" {
		t.Fatalf("detail %d %s", code, b)
	}
	sign := func(ch []byte) map[string]any {
		return pk.waCredential(t, waOpts{challenge: ch, rpID: loginRPID, origin: loginOrigin})
	}
	path := fmt.Sprintf("/api/v1/approvals/%d/grant", aid)

	// issued taken from the clock instead of the detail: refused, nothing changes
	clockBody := jsSignApproval(t, d, "phone", sign)
	issued, _ := parseIssued(d.Issued)
	clockBody["issued"] = issued.Add(time.Second).Format(time.RFC3339)
	bb, _ := json.Marshal(clockBody)
	if code, _, rb := httpDo(t, f.srv, "POST", path, string(bb), auth); code != http.StatusForbidden || !bytes.Contains(rb, []byte("bad_signature")) {
		t.Fatalf("clock issued: %d %s", code, rb)
	}
	gf.unchanged(t, tid, aid)

	body := jsSignApproval(t, d, "phone", sign)
	bb, _ = json.Marshal(body)
	strictGrantBody(t, bb)
	if got, want := keyShape(t, bb), keyShape(t, g.Request); got != want {
		t.Fatalf("live body shape %s differs from the JS golden %s", got, want)
	}
	if !reflect.DeepEqual(body["alg"], "webauthn-es256") {
		t.Fatal("alg")
	}
	code, _, rb := httpDo(t, f.srv, "POST", path, string(bb), auth)
	if code != 200 {
		t.Fatalf("signed web grant: %d %s", code, rb)
	}
	if st := state(t, gf.e, tid); st != store.StateVerifying {
		t.Fatalf("task %s", st)
	}
	// the same body again: the entry is no longer pending
	if code, _, rb := httpDo(t, f.srv, "POST", path, string(bb), auth); code != http.StatusConflict {
		t.Fatalf("replay: %d %s", code, rb)
	}
}

// TestRepoIDIsHashOfStoredPath: the daemon stores a task's repo as the
// symlink-resolved path of a directory with .git at its top (validate), and
// for such a path gate.RepoID (sha256 of git's toplevel) equals
// sha256(path)[:12], which is what the console recomputes.
func TestRepoIDIsHashOfStoredPath(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	real, err := filepath.EvalSymlinks(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	id, top, err := gate.RepoID(real)
	if err != nil {
		t.Fatal(err)
	}
	if top != real || id != fixtureRepoID(real) {
		t.Fatalf("RepoID(%q) = %s (top %q), sha256(path) = %s", real, id, top, fixtureRepoID(real))
	}
}
