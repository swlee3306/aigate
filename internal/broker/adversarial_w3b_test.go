package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// W3b adversarial suite (B6). Everything here must be refused or stay
// harmless. Equivalent checks written earlier (B2-B4) are referenced in
// docs/23 instead of being copied: TestNoValueInAnyResponseOrLog,
// TestNoValueInAnyOutput and TestPanicLogsTypeNotPayload cover "no value in
// logs, panics, status, tokens" (all success and failure paths).

const (
	advA = "adv-canary-NOT-A-REAL-TOKEN-AAAAAAAA-QZqz"
	advB = "adv-canary-NOT-A-REAL-TOKEN-BBBBBBBB-WXwx"
)

func (w *wenv) budgetFile() []byte {
	b, _ := os.ReadFile(filepath.Join(w.e.Dir, "state", "budget.json"))
	return b
}

// --- 1. secrets are never readable by other users -------------------------

func TestSecretsNeverReadableByOtherUIDs(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-a", "gitlab", "2027-01-31", advA)
	w.mustSet("github-a", "github", "", advB)
	w.mustSet("gitlab-a", "gitlab", "", advB) // replacement keeps the rules
	w.mustSet("openai", "openai", "", advA)
	if _, e := w.do("delete_token", w.delArgs(w.alice, "github-a")); e != nil {
		t.Fatal(e.Code)
	}
	w.wantCode("set_token", w.tokArgs(w.alice, "gitlab-b", "gitlab", "", "short"), "token_too_short") // failed write leaves no trace

	state := filepath.Join(w.e.Dir, "state")
	euid := uint32(os.Geteuid())
	var secrets []string
	err := filepath.Walk(state, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != euid {
			t.Errorf("%s owned by uid %d, not the broker (%d)", p, st.Uid, euid)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is a symlink", p)
		}
		// No group or other bit anywhere under the state directory.
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has group/other bits: %v", p, fi.Mode().Perm())
		}
		switch {
		case p == filepath.Join(state, "secrets"):
			if fi.Mode().Perm() != 0o700 || !fi.IsDir() {
				t.Errorf("secrets dir = %v, want 0700", fi.Mode())
			}
		case filepath.Dir(p) == filepath.Join(state, "secrets"):
			secrets = append(secrets, filepath.Base(p))
			if fi.Mode().Perm() != 0o400 || !fi.Mode().IsRegular() || st.Nlink != 1 {
				t.Errorf("secret %s = %v nlink=%d, want regular 0400 nlink 1", p, fi.Mode(), st.Nlink)
			}
		case filepath.Base(p) == "tokens.json":
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("tokens.json = %v, want 0600", fi.Mode().Perm())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(secrets, ",") != "gitlab-a,openai" {
		t.Fatalf("secrets/ holds %v: a deleted or failed token left a file", secrets)
	}

	// A file that anybody but the broker could read (or own) is not a token:
	// fail closed, the neighbours are unaffected, openai falls back to the config key.
	sp := func(ref string) string { return filepath.Join(state, "secrets", ref) }
	for _, mode := range []os.FileMode{0o440, 0o404, 0o444, 0o640, 0o600, 0o500, 0o700} {
		if err := os.Chmod(sp("openai"), mode); err != nil {
			t.Fatal(err)
		}
		if _, ok := w.s.Tokens.Value("openai"); ok {
			t.Errorf("mode %v: the stored key is still used", mode)
		}
		if k, ok := w.s.currentOpenAIKey(); !ok || k != testOpenAIKey {
			t.Errorf("mode %v: key = %q %v, want the config key", mode, k, ok)
		}
		if v, ok := w.s.Tokens.Value("gitlab-a"); !ok || v != advB {
			t.Errorf("mode %v on openai disturbed gitlab-a", mode)
		}
	}
	os.Chmod(sp("openai"), 0o400)
	if v, ok := w.s.Tokens.Value("openai"); !ok || v != advA {
		t.Fatal("control: a 0400 file must be usable again")
	}
	// A file not owned by the broker (simulated: the owner test rejects every uid).
	w.s.Tokens.Secrets.opts.fileOwnerOK = func(uint32) bool { return false }
	for _, ref := range []string{"gitlab-a", "openai"} {
		if _, ok := w.s.Tokens.Value(ref); ok {
			t.Errorf("foreign-owned %s is used", ref)
		}
	}
	if k, _ := w.s.currentOpenAIKey(); k != testOpenAIKey {
		t.Errorf("foreign-owned store key was used: %q", k)
	}
	w.s.Tokens.Secrets.opts.fileOwnerOK = nil
	// A readable secrets/ directory makes the whole store unusable and refuses writes.
	for _, mode := range []os.FileMode{0o750, 0o705, 0o770, 0o755} {
		if err := os.Chmod(filepath.Join(state, "secrets"), mode); err != nil {
			t.Fatal(err)
		}
		if _, ok := w.s.Tokens.Value("gitlab-a"); ok {
			t.Errorf("dir %v: a secret was read", mode)
		}
		w.wantCode("set_token", w.tokArgs(w.alice, "gitlab-c", "gitlab", "", advA), "tokens_unavailable")
		if _, err := os.Stat(sp("gitlab-c")); err == nil {
			t.Errorf("dir %v: a secret was written into a readable directory", mode)
		}
	}
	os.Chmod(filepath.Join(state, "secrets"), 0o700)
	// A state directory that other users can enter: the store refuses to start.
	os.Chmod(state, 0o755)
	if _, err := OpenTokenStore(state, RegistryOptions{}); err == nil {
		t.Error("a world-readable state dir was accepted")
	}
	os.Chmod(state, 0o700)
}

// --- 2. value swapped after the signature ----------------------------------

func TestValueSwapAfterSignatureRefused(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-a", "gitlab", "", advA)
	swaps := map[string]string{
		"other value":         advA,
		"first char":          "X" + advB[1:],
		"middle char":         advB[:20] + "#" + advB[21:],
		"last char":           advB[:len(advB)-1] + "!",
		"appended char":       advB + "x",
		"case":                strings.ToUpper(advB),
		"non-ascii lookalike": advB[:5] + "а" + advB[6:],
		"empty":               "",
		"trailing space":      advB + " ",
	}
	for name, v := range swaps {
		before := w.tokSnap()
		// a valid signature consumes its ID, so every attempt gets its own signed request
		a := copyArgs(w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advB), func(a map[string]any) { a["value"] = v })
		w.wantCode("set_token", a, "value_mismatch")
		after := w.tokSnap()
		if !before.sameStore(after) {
			t.Fatalf("%s: the store changed", name)
		}
	}
	// A value whose hash is right for ANOTHER request ID (replay of an old
	// value_hash with a fresh request) does not match either.
	other := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advB)
	hOther := other["value_hash"]
	args := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advB)
	args["value_hash"] = hOther
	w.wantCode("set_token", args, "change_mismatch") // the signed change names its own hash
	// A swapped attempt spends the signed request (a valid signature consumes
	// its ID): the honest value cannot be sent afterwards with the same signature.
	signed := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advB)
	w.wantCode("set_token", copyArgs(signed, func(a map[string]any) { a["value"] = advA }), "value_mismatch")
	w.wantCode("set_token", signed, "request_reused")
	if v, _ := w.s.Tokens.Value("gitlab-a"); v != advA {
		t.Fatalf("stored value = %q: a swapped request changed the store", v)
	}
	// Control: a new request with the honest value works.
	w.mustSet("gitlab-a", "gitlab", "", advB)
	if v, _ := w.s.Tokens.Value("gitlab-a"); v != advB {
		t.Fatal("control failed")
	}
}

// --- 3. replay across a restart -------------------------------------------

func TestSignatureReplayAcrossRestartRefused(t *testing.T) {
	w := newWEnv(t)
	set := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advA)
	del := w.delArgs(w.alice, "gitlab-a")
	w.wantCode("set_token", set, "")
	w.wantCode("delete_token", del, "")
	// The token is gone now. Replays must say request_reused, never recreate it.
	snap := w.tokSnap()
	cur := w.s
	for i := 0; i < 2; i++ {
		for _, c := range []struct {
			op   string
			args map[string]any
		}{
			{"set_token", set},
			{"delete_token", del},
			{"set_token", copyArgs(set, func(a map[string]any) { a["value"] = advB })}, // other value, same signature
		} {
			raw, _ := json.Marshal(c.args)
			if _, e := cur.Handlers[c.op](context.Background(), raw); codeOf(e) != "request_reused" {
				t.Fatalf("restart %d, %s: %v", i, c.op, e)
			}
		}
		cur = w.restart(nil)
	}
	if got := w.tokSnap(); !snap.sameStore(got) || !snap.sameUsed(got) {
		t.Fatal("a replay changed the store or the used-request file")
	}
	if _, ok := cur.Tokens.Value("gitlab-a"); ok {
		t.Fatal("a replay recreated the deleted token")
	}
	// Control: new signatures work on the restarted broker.
	raw, _ := json.Marshal(w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advB))
	if _, e := cur.Handlers["set_token"](context.Background(), raw); e != nil {
		t.Fatal(e.Code)
	}
}

// --- 4. a signature for one token cannot change another ---------------------

func TestSetTokenForOtherRefOrKindRefused(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-b", "gitlab", "", advA)
	signed := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advB)
	reSigned := func(ref, kind string) map[string]any { // attacker recomputes change/hash but cannot sign
		other := w.tokArgsReq(w.alice, ref, kind, "", advB, signed["request"].(string))
		a := copyArgs(other, func(a map[string]any) { a["signature"] = signed["signature"] })
		return a
	}
	cases := map[string]struct {
		args map[string]any
		want []string
	}{
		"other ref, rest as signed":  {copyArgs(signed, func(a map[string]any) { a["ref"] = "gitlab-b" }), []string{"bad_signature"}},
		"other ref, recomputed":      {reSigned("gitlab-b", "gitlab"), []string{"bad_signature"}},
		"other kind, rest as signed": {copyArgs(signed, func(a map[string]any) { a["kind"] = "github" }), []string{"change_mismatch"}},
		"other kind, recomputed":     {reSigned("gitlab-a", "github"), []string{"bad_signature"}},
		"openai name and kind":       {reSigned("openai", "openai"), []string{"bad_signature"}},
		"openai kind only":           {copyArgs(signed, func(a map[string]any) { a["kind"] = "openai" }), []string{"openai_singleton"}},
	}
	for name, c := range cases {
		before := w.tokSnap()
		_, e := w.do("set_token", c.args)
		ok := false
		for _, want := range c.want {
			ok = ok || codeOf(e) == want
		}
		if !ok {
			t.Errorf("%s: code %q, want one of %v", name, codeOf(e), c.want)
		}
		if got := w.tokSnap(); !before.sameStore(got) || !before.sameUsed(got) {
			t.Errorf("%s: the store changed or an ID was burned", name)
		}
	}
	// A delete signature for gitlab-a cannot delete gitlab-b.
	del := w.delArgs(w.alice, "gitlab-a")
	before := w.tokSnap()
	w.wantCode("delete_token", copyArgs(del, func(a map[string]any) { a["ref"] = "gitlab-b" }), "bad_signature")
	if got := w.tokSnap(); !before.sameStore(got) || !before.sameUsed(got) {
		t.Error("delete for another ref changed state")
	}
	if _, ok := w.s.Tokens.Value("gitlab-b"); !ok {
		t.Fatal("gitlab-b disappeared")
	}
	// Control: the honest requests still work (no ID was burned).
	w.wantCode("set_token", signed, "")
	w.wantCode("delete_token", del, "")
}

// --- 5. the three kinds of signature stay separate -------------------------

func TestApprovalAndRegistryMessagesStillSeparate(t *testing.T) {
	for _, mk := range []func() testSigner{
		func() testSigner { return newSSHTestSigner(t, "alice") },
		func() testSigner { return newWebTestSigner(t, "alice") },
	} {
		w := newWEnv(t)
		a := mk()
		w.writeSigners(a)
		w.mustSetBy(a, "gitlab-a", "gitlab", advA)
		req := registrymsg.NewRequestID()
		approval := func() signing.Signature {
			m := approvalMessage(w.now)
			if a.ssh != nil {
				return signing.Signature{Signer: a.id, Alg: "ssh-ed25519", Sig: sshsigArmored(t, a.ssh, signing.NamespaceApproval, []byte(m.Text()))}
			}
			return webSignDigest(t, a.web, a.id, m.Digest())
		}()
		projectSig := func(action string, subject string, fields map[string]any) signing.Signature {
			return w.signedArgsWith(a, action, subject, fields, req, w.now, signing.NamespaceRegistry)["signature"].(signing.Signature)
		}
		forged := map[string]signing.Signature{
			"approval":         approval,
			"register_project": projectSig("register_project", "gitlab-a", regFields(w.repo("gitlab-a"), "default", true)),
			"set_flag":         projectSig("set_flag", "gitlab-a", flagFields("ai_context", true)),
			"delete_token":     projectSig("delete_token", "gitlab-a", map[string]any{}),
		}
		for name, sig := range forged {
			// token change authorised by that signature
			set := w.tokArgsReq(a, "gitlab-a", "gitlab", "", advB, req)
			set["signature"] = sig
			before := w.tokSnap()
			w.wantCode("set_token", set, "bad_signature")
			if name != "delete_token" {
				del := w.delArgs(a, "gitlab-a")
				del["request"] = req
				del["signature"] = sig
				// the change in a delete args is for another request; recompute it for this one
				h, _ := registrymsg.ChangeHash(registrymsg.ActionDeleteToken, "gitlab-a", map[string]any{})
				del["change"] = h
				w.wantCode("delete_token", del, "bad_signature")
			}
			if got := w.tokSnap(); !before.sameStore(got) || !before.sameUsed(got) {
				t.Errorf("%s signature changed token state", name)
			}
		}
		// Reverse: a genuine set_token / delete_token signature authorises no project change.
		tokSig := w.tokArgsReq(a, "repo-a", "gitlab", "", advB, req)["signature"]
		delSig := w.delArgs(a, "repo-a")["signature"]
		regBefore := w.snap()
		for _, sig := range []any{tokSig, delSig} {
			sf := w.signedArgs(a, "set_flag", "repo-a", flagFields("ai_context", false))
			sf["signature"] = sig
			w.wantWire("set_flag", sf, false, "bad_signature")
			rp := w.register("proj-x", w.repo("proj-x"))
			rp["signature"] = sig
			w.wantWire("register_project", rp, false, "bad_signature")
		}
		w.mustEqual(regBefore, "token signature used for a project change")
		// A token signature is also no approval.
		reg, err := signing.LoadRegistry(w.signers, signing.DefaultOptions)
		if err != nil {
			t.Fatal(err)
		}
		tsig := tokSig.(signing.Signature)
		if err := signing.Verify(reg, approvalMessage(w.now), tsig, signing.VerifyOptions{Now: w.now}); err == nil {
			t.Fatal("a set_token signature verified as an approval")
		}
		// Controls: the honest requests work.
		w.wantCode("set_token", w.tokArgs(a, "gitlab-a", "gitlab", "", advB), "")
		w.wantWire("set_flag", w.signedArgs(a, "set_flag", "repo-a", flagFields("ai_context", false)), true, "")
	}
}

// mustSetBy is mustSet signed by who.
func (w *wenv) mustSetBy(who testSigner, ref, kind, value string) {
	w.t.Helper()
	if _, e := w.do("set_token", w.tokArgs(who, ref, kind, "", value)); e != nil {
		w.t.Fatalf("set_token %s: %v", ref, e.Code)
	}
}

// --- 6. token data never sits in the registry or the budget ----------------

func TestTokenFilesNeverInRegistryOrBudget(t *testing.T) {
	w := newWEnv(t)
	regBefore, budgetBefore := w.regFile(), w.budgetFile()
	w.mustSet("gitlab-tok", "gitlab", "2027-01-31", advA)
	w.mustSet("gitlab-tok", "gitlab", "", advB)
	w.mustSet("openai", "openai", "", advA)
	if _, e := w.do("delete_token", w.delArgs(w.alice, "gitlab-tok")); e != nil {
		t.Fatal(e.Code)
	}
	if !bytes.Equal(regBefore, w.regFile()) {
		t.Fatal("a token change touched registry.json")
	}
	if !bytes.Equal(budgetBefore, w.budgetFile()) {
		t.Fatal("a token change touched budget.json")
	}
	// And the other way round: project changes never touch the token files.
	secret, meta := w.tokenFiles()
	w.wantWire("register_project", w.register("proj-a", w.repo("proj-a")), true, "")
	w.wantWire("set_flag", w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", false)), true, "")
	secret2, meta2 := w.tokenFiles()
	if !bytes.Equal(meta, meta2) || len(secret) != len(secret2) {
		t.Fatal("a project change touched the token files")
	}
	hashes := []string{registrymsg.ValueHash("00000000000000000000000000000000", advA)}
	for _, f := range []string{RegistryFileName, UsedRequestsFileName, "budget.json"} {
		b, _ := os.ReadFile(filepath.Join(w.e.Dir, "state", f))
		for _, bad := range append([]string{advA, advB, "QZqz", "WXwx", "gitlab-tok", "secrets", "value_hash", "last4"}, hashes...) {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q", f, bad)
			}
		}
	}
	// tokens.json never holds a project, a path or a value.
	m, _ := os.ReadFile(filepath.Join(w.e.Dir, "state", TokensFileName))
	for _, bad := range []string{advA, advB, "proj-a", w.root, "repo-a"} {
		if strings.Contains(string(m), bad) {
			t.Errorf("tokens.json contains %q", bad)
		}
	}
	// The project listing and status carry no token (ref, last4 or value).
	for _, op := range []string{"projects", "status"} {
		_, out := call(t, w.sock, `{"v":1,"op":"`+op+`"}`)
		for _, bad := range []string{advA, advB, "QZqz", `"openai"`} {
			if op == "projects" && strings.Contains(string(out), bad) {
				t.Errorf("%s output contains %q", op, bad)
			}
			if op == "status" && (strings.Contains(string(out), advA) || strings.Contains(string(out), "QZqz")) {
				t.Errorf("%s output contains a value or last4", op)
			}
		}
	}
}

// --- 7. broken token metadata does not turn other features off -------------

func TestBrokenTokenMetaDoesNotDisableOtherFeatures(t *testing.T) {
	fake, auth, mu := openAIFake(t, nil)
	w := newWEnvMod(t, func(s *Server) { s.OpenAI.BaseURL = fake.URL })
	w.mustSet("openai", "openai", "", advA) // a stored key exists before the metadata breaks
	state := filepath.Join(w.e.Dir, "state")
	if err := os.WriteFile(filepath.Join(state, TokensFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := w.restart(func(s *Server) { s.OpenAI.BaseURL = fake.URL })
	if s2.Tokens.Err() == nil {
		t.Fatal("the broken metadata was accepted")
	}
	h := func(op string, a any) (any, *OpError) {
		raw, _ := json.Marshal(a)
		return s2.Handlers[op](context.Background(), raw)
	}
	// token ops: fixed refusal, nothing written, nothing consumed
	usedBefore := w.usedFile()
	for _, op := range []string{"set_token", "delete_token"} {
		args := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", advB)
		if op == "delete_token" {
			args = w.delArgs(w.alice, "openai")
		}
		if _, e := h(op, args); codeOf(e) != "tokens_unavailable" {
			t.Errorf("%s with broken metadata: %v", op, e)
		}
	}
	if _, e := h("tokens", nil); codeOf(e) != "tokens_unavailable" {
		t.Errorf("tokens: %v", e)
	}
	if !bytes.Equal(usedBefore, w.usedFile()) {
		t.Error("a refused token op consumed an ID")
	}
	if b, _ := os.ReadFile(filepath.Join(state, TokensFileName)); string(b) != "{broken" {
		t.Error("the broken file was touched")
	}
	// everything else keeps working
	if res, e := h("projects", nil); e != nil || res == nil {
		t.Errorf("projects: %v", e)
	}
	if _, e := h("status", nil); e != nil {
		t.Errorf("status: %v", e)
	}
	if _, e := h("register_project", w.register("proj-a", w.repo("proj-a"))); e != nil {
		t.Errorf("register_project: %v", e)
	}
	if _, e := h("set_flag", w.signedArgs(w.alice, "set_flag", "proj-a", flagFields("ai_context", false))); e != nil {
		t.Errorf("set_flag: %v", e)
	}
	// refine: the config key path is used; the stored key is NOT (its metadata is untrustworthy)
	r := &refineEnv{env: w.e, srv: s2, logs: w.logs, repo: filepath.Join(w.e.Dir, "repo-a")}
	if _, e := r.call(t, r.args()); e != nil && e.Code == "ai_key_missing" {
		t.Errorf("refine with a usable config key: %v", e)
	}
	got := lastAuth(auth, mu)
	if len(got) != 1 || got[0] != "Bearer "+testOpenAIKey {
		t.Errorf("authorization = %v, want the config key only", got)
	}
	// and with no config key at all it fails closed with the fixed code
	os.Remove(w.e.KeyFile)
	hits := fake.Hits()
	if _, e := r.call(t, r.args()); codeOf(e) != "ai_key_missing" || fake.Hits() != hits {
		t.Errorf("refine without any usable key: %v (hits %d -> %d)", e, hits, fake.Hits())
	}
}

// --- 8. delete, then refine: fails closed, no budget ----------------------

func TestDeleteThenRefineFailsClosedNoBudget(t *testing.T) {
	fake, auth, mu := openAIFake(t, nil)
	w := newWEnvMod(t, func(s *Server) { s.OpenAI.BaseURL = fake.URL })
	r := &refineEnv{env: w.e, srv: w.s, logs: w.logs, repo: filepath.Join(w.e.Dir, "repo-a")}
	// The only key is the stored one.
	if err := os.Remove(w.e.KeyFile); err != nil {
		t.Fatal(err)
	}
	w.mustSet("openai", "openai", "", advA)
	if _, e := r.call(t, r.args()); e != nil && e.Code == "ai_key_missing" {
		t.Fatal("the stored key was not used")
	}
	if got := lastAuth(auth, mu); len(got) != 1 || got[0] != "Bearer "+advA {
		t.Fatalf("authorization = %v", got)
	}
	if _, e := w.do("delete_token", w.delArgs(w.alice, "openai")); e != nil {
		t.Fatal(e.Code)
	}
	budget, slots, hits, used := w.s.State.Budget.RemainingMicros(), w.s.State.Refine.Remaining(), fake.Hits(), w.budgetFile()
	for i := 0; i < 5; i++ {
		if _, e := r.call(t, r.args()); codeOf(e) != "ai_key_missing" {
			t.Fatalf("call %d after delete: %v", i, e)
		}
	}
	if w.s.State.Budget.RemainingMicros() != budget || w.s.State.Refine.Remaining() != slots || fake.Hits() != hits || !bytes.Equal(used, w.budgetFile()) {
		t.Fatal("refine without a key used budget, a slot, the budget file or the upstream")
	}
	if _, ok := w.s.currentOpenAIKey(); ok {
		t.Fatal("a deleted key is still served")
	}
	// Opened again from disk (a restart needs the config key file at start, so
	// the store is reopened directly): no copy of the deleted key anywhere.
	ts, err := OpenTokenStore(filepath.Join(w.e.Dir, "state"), RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.Value("openai"); ok {
		t.Fatal("the deleted key came back after reopening the store")
	}
	ents, _ := os.ReadDir(filepath.Join(w.e.Dir, "state", "secrets"))
	if len(ents) != 0 {
		t.Fatalf("secrets/ not empty after delete: %v", ents)
	}
	// the over-the-wire answer is the fixed sentence
	rep, out := call(t, w.sock, `{"v":1,"op":"refine","args":`+tokJSON(t, r.args())+`}`)
	if rep.OK || rep.Code != "ai_key_missing" || rep.Message != fixedMessages["ai_key_missing"] || strings.Contains(string(out), advA) {
		t.Fatalf("reply: %s", out)
	}
}

// --- 9. replacement racing a call: never a torn key ------------------------

func TestHotReloadRaceNoTornKey(t *testing.T) {
	fake, auth, mu := openAIFake(t, nil)
	r := newRefineEnv(t, fake, nil)
	s := r.srv
	// Two long keys sharing a long prefix: a torn read would mix them.
	keyA := "race-key-NOT-REAL-" + strings.Repeat("A", 40)
	keyB := "race-key-NOT-REAL-" + strings.Repeat("B", 40)
	allowed := map[string]bool{"Bearer " + testOpenAIKey: true, "Bearer " + keyA: true, "Bearer " + keyB: true}
	if err := s.Tokens.Set("openai", "openai", "", keyA, tokNow); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	torn := make(chan string, 64)
	wg.Add(1)
	go func() { // the replacer
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k := keyA
			if i%2 == 1 {
				k = keyB
			}
			if err := s.Tokens.Set("openai", "openai", "", k, tokNow); err != nil {
				torn <- "set: " + err.Error()
				return
			}
			if i%7 == 0 {
				_ = s.Tokens.Delete("openai") // fall back to the config key for a moment
			}
		}
	}()
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() { // readers of the key itself
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if k, ok := s.currentOpenAIKey(); ok && k != keyA && k != keyB && k != testOpenAIKey {
					torn <- "torn key: " + k
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ { // real calls through refine
		r.call(t, r.args())
	}
	close(stop)
	wg.Wait()
	close(torn)
	for m := range torn {
		t.Error(m)
	}
	got := lastAuth(auth, mu)
	if len(got) == 0 {
		t.Fatal("no call reached the fake upstream")
	}
	for _, h := range got {
		if !allowed[h] {
			t.Errorf("a call carried a key that was never set: %q", h)
		}
	}
	// Quiet again: the next call uses exactly the last key set (the reload is real).
	if err := s.Tokens.Set("openai", "openai", "", keyB, tokNow); err != nil {
		t.Fatal(err)
	}
	before := len(got)
	r.call(t, r.args())
	got = lastAuth(auth, mu)
	if len(got) != before+1 || got[len(got)-1] != "Bearer "+keyB {
		t.Fatalf("the call after the last replacement used %q", got[len(got)-1])
	}
}
