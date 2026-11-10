package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// Policy pinned by these tests (same as W3a: "once a valid signature has
// passed, the request ID is consumed"):
//   - Refused WITHOUT consuming the ID: strict decode, tokens store unusable,
//     not_signed, missing change, request-shape rules checked before the
//     signature (bad ref, bad kind, openai_singleton, bad expires format),
//     unknown signer, bad/stale signature, signers unavailable, change mismatch.
//   - Refused AFTER consuming the ID (the signature was valid): value_hash
//     mismatch, the value rules (charset, length), expiry in the past, and any
//     token store refusal (other kind, store error, unknown ref on delete).

const (
	tokenOK  = "canary-NOT-A-REAL-TOKEN-AAAAAAAAAAAA1111" // 40 chars
	tokenOK2 = "canary-NOT-A-REAL-TOKEN-BBBBBBBBBBBB2222"
)

// tokArgs builds fully valid set_token args signed by who at w.now.
func (w *wenv) tokArgs(who testSigner, ref, kind, expires, value string) map[string]any {
	w.t.Helper()
	return w.tokArgsReq(who, ref, kind, expires, value, registrymsg.NewRequestID())
}

func (w *wenv) tokArgsReq(who testSigner, ref, kind, expires, value, request string) map[string]any {
	w.t.Helper()
	fields := map[string]any{"kind": kind, "expires": expires, "value_hash": registrymsg.ValueHash(request, value)}
	args := w.signedArgsWith(who, registrymsg.ActionSetToken, ref, fields, request, w.now, signing.NamespaceRegistry)
	delete(args, "id")
	args["ref"] = ref
	args["value"] = value
	return args
}

func (w *wenv) delArgs(who testSigner, ref string) map[string]any {
	w.t.Helper()
	args := w.signedArgs(who, registrymsg.ActionDeleteToken, ref, map[string]any{})
	delete(args, "id")
	args["ref"] = ref
	return args
}

func (w *wenv) tokenFiles() (secret map[string]string, meta []byte) {
	w.t.Helper()
	secret = map[string]string{}
	ents, _ := os.ReadDir(filepath.Join(w.e.Dir, "state", "secrets"))
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(w.e.Dir, "state", "secrets", e.Name()))
		secret[e.Name()] = string(b)
	}
	meta, _ = os.ReadFile(filepath.Join(w.e.Dir, "state", "tokens.json"))
	return
}

type tokSnap struct {
	secret map[string]string
	meta   []byte
	used   []byte
}

func (w *wenv) tokSnap() tokSnap {
	s, m := w.tokenFiles()
	return tokSnap{s, m, w.usedFile()}
}

func (a tokSnap) sameStore(b tokSnap) bool {
	if !bytes.Equal(a.meta, b.meta) || len(a.secret) != len(b.secret) {
		return false
	}
	for k, v := range a.secret {
		if b.secret[k] != v {
			return false
		}
	}
	return true
}

func (a tokSnap) sameUsed(b tokSnap) bool { return bytes.Equal(a.used, b.used) }

func (w *wenv) mustSet(ref, kind, expires, value string) {
	w.t.Helper()
	if _, e := w.do("set_token", w.tokArgs(w.alice, ref, kind, expires, value)); e != nil {
		w.t.Fatalf("set_token %s: %v", ref, e.Code)
	}
}

func TestTokensReadSecretFree(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-a", "gitlab", "2027-01-31", tokenOK)
	r, out := call(t, w.sock, `{"v":1,"op":"tokens"}`)
	if !r.OK {
		t.Fatalf("tokens: %+v", r)
	}
	var res struct {
		Result struct {
			Tokens   []map[string]any `json:"tokens"`
			WarnDays int              `json:"warn_days"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	for _, tk := range res.Result.Tokens {
		if tk["ref"] == "gitlab-a" {
			row = tk
		}
	}
	if row == nil || row["last4"] != "1111" || row["expires"] != "2027-01-31" || row["source"] != "store" || row["kind"] != "gitlab" {
		t.Fatalf("row = %v", row)
	}
	if res.Result.WarnDays != 14 {
		t.Fatalf("warn_days = %d", res.Result.WarnDays)
	}
	for k := range row {
		switch k {
		case "ref", "kind", "last4", "expires", "set_at", "source", "days_left", "usable":
		default:
			t.Fatalf("unexpected tokens field %q", k)
		}
	}
	reqHash := registrymsg.ValueHash(registrymsg.NewRequestID(), tokenOK)
	all := string(out)
	if strings.Contains(all, tokenOK) || strings.Contains(all, "canary") || strings.Contains(all, "value_hash") || strings.Contains(all, reqHash) {
		t.Fatalf("tokens output leaks: %s", all)
	}
}

func TestSetTokenRequiresValidSignature(t *testing.T) {
	w := newWEnv(t)
	good := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
	flip := func(a map[string]any) {
		s := a["signature"].(signing.Signature)
		raw := []byte(s.Sig)
		i := len(raw) / 2
		if raw[i] == 'A' {
			raw[i] = 'B'
		} else {
			raw[i] = 'A'
		}
		s.Sig = string(raw)
		a["signature"] = s
	}
	stale := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
	stale = w.tokArgsIssued(stale, "gitlab-a", "gitlab", "", tokenOK, w.now.Add(-6*time.Minute))
	cases := []struct {
		name string
		args map[string]any
		code string
		pre  func()
		undo func()
	}{
		{name: "no signature", args: copyArgs(good, func(a map[string]any) { delete(a, "signature") }), code: "not_signed"},
		{name: "null signature", args: copyArgs(good, func(a map[string]any) { a["signature"] = nil }), code: "not_signed"},
		{name: "unknown signer", args: copyArgs(good, func(a map[string]any) {
			s := a["signature"].(signing.Signature)
			s.Signer = "mallory"
			a["signature"] = s
		}), code: "unknown_signer"},
		{name: "flipped signature", args: copyArgs(good, flip), code: "bad_signature"},
		{name: "stale signature", args: stale, code: "stale_signature"},
		{name: "signers unavailable", args: good, code: "signers_unavailable",
			pre:  func() { os.Rename(w.signers, w.signers+".off") },
			undo: func() { os.Rename(w.signers+".off", w.signers) }},
	}
	for _, c := range cases {
		before := w.tokSnap()
		if c.pre != nil {
			c.pre()
		}
		_, e := w.do("set_token", c.args)
		if c.undo != nil {
			c.undo()
		}
		if codeOf(e) != c.code {
			t.Fatalf("%s: code = %q, want %q", c.name, codeOf(e), c.code)
		}
		after := w.tokSnap()
		if !before.sameStore(after) || !before.sameUsed(after) {
			t.Fatalf("%s: the store or the used-request IDs changed", c.name)
		}
	}
	// nothing was burned: the good request still works once
	if _, e := w.do("set_token", good); e != nil {
		t.Fatalf("good request after refusals: %v", e.Code)
	}
}

// tokArgsIssued re-signs args with another issued time (same request).
func (w *wenv) tokArgsIssued(base map[string]any, ref, kind, expires, value string, issued time.Time) map[string]any {
	w.t.Helper()
	request := base["request"].(string)
	fields := map[string]any{"kind": kind, "expires": expires, "value_hash": registrymsg.ValueHash(request, value)}
	args := w.signedArgsWith(w.alice, registrymsg.ActionSetToken, ref, fields, request, issued, signing.NamespaceRegistry)
	delete(args, "id")
	args["ref"] = ref
	args["value"] = value
	return args
}

func TestSetTokenChangeRequired(t *testing.T) {
	w := newWEnv(t)
	good := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
	before := w.tokSnap()
	for _, name := range []string{"absent", "empty"} {
		a := copyArgs(good, func(a map[string]any) {
			if name == "absent" {
				delete(a, "change")
			} else {
				a["change"] = ""
			}
		})
		w.wantCode("set_token", a, "change_mismatch")
	}
	a := copyArgs(good, func(a map[string]any) { a["change"] = strings.Repeat("0", 64) })
	w.wantCode("set_token", a, "bad_signature") // the signature was over another change
	after := w.tokSnap()
	if !before.sameStore(after) || !before.sameUsed(after) {
		t.Fatal("a refused request changed the store or consumed the ID")
	}
	if _, e := w.do("set_token", good); e != nil {
		t.Fatalf("same request ID after missing-change refusals: %v", e.Code)
	}
}

func TestValueMismatchRefused(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-a", "gitlab", "2027-01-01", tokenOK)
	before := w.tokSnap()
	// valid signature over the hash of tokenOK2, but another value is sent
	signed := w.tokArgs(w.alice, "gitlab-a", "gitlab", "2027-02-02", tokenOK2)
	sent := copyArgs(signed, func(a map[string]any) { a["value"] = tokenOK })
	w.wantCode("set_token", sent, "value_mismatch")
	after := w.tokSnap()
	if !before.sameStore(after) {
		t.Fatal("a value mismatch changed the store or metadata")
	}
	// the signature was valid, so the request ID is spent (pinned policy):
	// the right value under the same signature is refused as reused
	w.wantCode("set_token", signed, "request_reused")
	if before.sameUsed(after) {
		t.Fatal("request ID not consumed after a valid signature")
	}
	// matching value and hash: applies (a fresh request ID)
	if _, e := w.do("set_token", w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK2)); e != nil {
		t.Fatalf("matching value: %v", e.Code)
	}
}

func TestValueRules(t *testing.T) {
	w := newWEnv(t)
	long := strings.Repeat("a", 512)
	cases := []struct {
		name, value, code string
	}{
		{"empty", "", "token_too_short"},
		{"31 chars", strings.Repeat("a", 31), "token_too_short"},
		{"513 chars", strings.Repeat("a", 513), "token_bad_format"},
		{"inner space", strings.Repeat("a", 20) + " " + strings.Repeat("b", 20), "token_bad_format"},
		{"newline", strings.Repeat("a", 40) + "\n", "token_bad_format"},
		{"inner newline", strings.Repeat("a", 20) + "\n" + strings.Repeat("b", 20), "token_bad_format"},
		{"tab", strings.Repeat("a", 40) + "\t", "token_bad_format"},
		{"leading space", " " + strings.Repeat("a", 40), "token_bad_format"},
		{"trailing space", strings.Repeat("a", 40) + " ", "token_bad_format"},
		{"non-ascii", strings.Repeat("a", 40) + "토큰", "token_bad_format"},
		{"del char", strings.Repeat("a", 40) + "\x7f", "token_bad_format"},
		{"nul", strings.Repeat("a", 40) + "\x00", "token_bad_format"},
		{"short with space (format wins)", "abc def", "token_bad_format"},
	}
	for _, c := range cases {
		before := w.tokSnap()
		args := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", c.value)
		w.wantCode("set_token", args, c.code)
		after := w.tokSnap()
		if !before.sameStore(after) {
			t.Fatalf("%s: store changed", c.name)
		}
		// pinned: the signature was valid, so the ID is consumed
		if before.sameUsed(after) {
			t.Fatalf("%s: request ID was not consumed", c.name)
		}
		w.wantCode("set_token", args, "request_reused")
	}
	// the boundaries are accepted
	for _, v := range []string{strings.Repeat("a", 32), long, "!" + strings.Repeat("~", 40)} {
		w.mustSet("gitlab-b", "gitlab", "", v)
		if got, ok := w.s.Tokens.Value("gitlab-b"); !ok || got != v {
			t.Fatalf("stored value differs for len %d", len(v))
		}
	}
}

func TestKindAndNameRules(t *testing.T) {
	w := newWEnv(t)
	type tc struct {
		name, ref, kind, code string
		consumed              bool
	}
	cases := []tc{
		{"bad kind", "gitlab-a", "ssh", "bad_kind", false},
		{"empty kind", "gitlab-a", "", "bad_kind", false},
		{"upper kind", "gitlab-a", "GitLab", "bad_kind", false},
		{"openai kind other name", "my-ai", "openai", "openai_singleton", false},
		{"gitlab kind named openai", "openai", "gitlab", "openai_singleton", false},
		{"github kind named openai", "openai", "github", "openai_singleton", false},
		{"bad ref", "Bad_Ref", "gitlab", "bad_request", false},
		{"ref too long", strings.Repeat("a", 41), "gitlab", "bad_request", false},
	}
	for _, c := range cases {
		before := w.tokSnap()
		// signed over a valid kind so that the shape rule, not the signature, is what refuses
		args := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
		args["ref"], args["kind"] = c.ref, c.kind
		w.wantCode("set_token", args, c.code)
		after := w.tokSnap()
		if !before.sameStore(after) || before.sameUsed(after) != !c.consumed {
			t.Fatalf("%s: store changed or consumed mismatch", c.name)
		}
	}
	// a validly signed openai-kind request with another name is refused too
	a := w.tokArgs(w.alice, "my-ai", "openai", "", tokenOK)
	w.wantCode("set_token", a, "openai_singleton")
	// same ref, other kind: refused by the store after a valid signature (ID consumed)
	w.mustSet("gitlab-a", "gitlab", "", tokenOK)
	before := w.tokSnap()
	a = w.tokArgs(w.alice, "gitlab-a", "github", "", tokenOK2)
	w.wantCode("set_token", a, "token_exists_other_kind")
	after := w.tokSnap()
	if !before.sameStore(after) || before.sameUsed(after) {
		t.Fatal("other-kind refusal: store changed or the ID was not consumed")
	}
	if got, _ := w.s.Tokens.Value("gitlab-a"); got != tokenOK {
		t.Fatal("value changed by a refused request")
	}
	// the openai name with the openai kind works
	w.mustSet("openai", "openai", "", tokenOK2)
}

func TestSetTokenReplacesAtomically(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-a", "gitlab", "2027-01-01", tokenOK)
	w.now = w.now.Add(time.Hour)
	res, e := w.do("set_token", w.tokArgs(w.alice, "gitlab-a", "gitlab", "2028-02-02", tokenOK2))
	if e != nil {
		t.Fatal(e.Code)
	}
	if got, ok := w.s.Tokens.Value("gitlab-a"); !ok || got != tokenOK2 {
		t.Fatalf("value after replace = %q", got)
	}
	list, _ := w.s.Tokens.List(w.now)
	var row TokenInfo
	for _, l := range list {
		if l.Ref == "gitlab-a" {
			row = l
		}
	}
	if row.Last4 != "2222" || row.Expires != "2028-02-02" || row.SetAt != w.now.UTC().Format(time.RFC3339) {
		t.Fatalf("row = %+v", row)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "1111") || strings.Contains(string(b), tokenOK2) || strings.Contains(string(b), "canary") {
		t.Fatalf("response carries an old or full value: %s", b)
	}
	sec, meta := w.tokenFiles()
	if sec["gitlab-a"] != tokenOK2 || strings.Contains(string(meta), "1111") {
		t.Fatal("files disagree with the new value")
	}
	fi, err := os.Stat(filepath.Join(w.e.Dir, "state", "secrets", "gitlab-a"))
	if err != nil || fi.Mode().Perm() != 0o400 {
		t.Fatalf("secret file mode: %v %v", fi, err)
	}
}

func TestSetTokenExpiresInPastRefused(t *testing.T) {
	w := newWEnv(t) // broker clock: 2026-11-09 12:00 UTC
	for _, c := range []struct {
		expires, code string
	}{
		{"2026-11-08", "bad_request"},
		{"2000-01-01", "bad_request"},
		{"2026-11-09", ""}, // today is allowed
		{"2026-11-10", ""},
		{"", ""},
		{"2026-13-01", "bad_request"},
		{"20261110", "bad_request"},
	} {
		before := w.tokSnap()
		var args map[string]any
		if c.expires == "2026-13-01" || c.expires == "20261110" {
			// not even signable (the message refuses the format): sign "" and send the bad one
			args = w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
			args["expires"] = c.expires
		} else {
			args = w.tokArgs(w.alice, "gitlab-a", "gitlab", c.expires, tokenOK)
		}
		_, e := w.do("set_token", args)
		if codeOf(e) != c.code {
			t.Fatalf("expires %q: code %q, want %q", c.expires, codeOf(e), c.code)
		}
		after := w.tokSnap()
		if c.code != "" && !before.sameStore(after) {
			t.Fatalf("expires %q: store changed", c.expires)
		}
		if c.code == "bad_request" && c.expires == "2026-11-08" && before.sameUsed(after) {
			t.Fatal("past expiry is checked after a valid signature: the ID must be consumed (pinned)")
		}
	}
}

func TestDeleteTokenSignedOnlyAndUnknownRef(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-a", "gitlab", "", tokenOK)
	before := w.tokSnap()
	d := w.delArgs(w.alice, "gitlab-a")
	w.wantCode("delete_token", copyArgs(d, func(a map[string]any) { delete(a, "signature") }), "not_signed")
	w.wantCode("delete_token", copyArgs(d, func(a map[string]any) { delete(a, "change") }), "change_mismatch")
	if after := w.tokSnap(); !before.sameStore(after) || !before.sameUsed(after) {
		t.Fatal("refused delete changed state or burned the ID")
	}
	// a set_token signature cannot delete
	set := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK2)
	forged := map[string]any{"ref": "gitlab-a", "request": set["request"], "issued": set["issued"],
		"change": set["change"], "signature": set["signature"]}
	if _, e := w.do("delete_token", forged); e == nil {
		t.Fatal("a set_token signature deleted a token")
	}
	if got, _ := w.s.Tokens.Value("gitlab-a"); got != tokenOK {
		t.Fatal("token changed")
	}
	// unknown ref: fixed code, valid signature, ID consumed
	un := w.delArgs(w.alice, "nothere")
	w.wantCode("delete_token", un, "token_unknown")
	w.wantCode("delete_token", un, "request_reused")
	// the real delete
	if _, e := w.do("delete_token", d); e != nil {
		t.Fatal(e.Code)
	}
	sec, meta := w.tokenFiles()
	if _, ok := sec["gitlab-a"]; ok || strings.Contains(string(meta), "gitlab-a") {
		t.Fatal("token still stored after delete")
	}
	// delete with extra fields is a bad request
	bad := copyArgs(w.delArgs(w.alice, "gitlab-a"), func(a map[string]any) { a["kind"] = "gitlab" })
	w.wantCode("delete_token", bad, "bad_request")
}

func TestDeleteOpenAIFallsBackToConfigKey(t *testing.T) {
	w := newWEnv(t)
	if v, ok := w.s.currentOpenAIKey(); !ok || v != testOpenAIKey {
		t.Fatalf("config key = %q %v", v, ok)
	}
	w.mustSet("openai", "openai", "", tokenOK)
	if v, _ := w.s.currentOpenAIKey(); v != tokenOK {
		t.Fatalf("after set: key = %q", v)
	}
	if _, e := w.do("delete_token", w.delArgs(w.alice, "openai")); e != nil {
		t.Fatal(e.Code)
	}
	if v, ok := w.s.currentOpenAIKey(); !ok || v != testOpenAIKey {
		t.Fatalf("after delete the config key must be used again: %q %v", v, ok)
	}
	// nothing left: delete still allowed, the key is then missing
	w.mustSet("openai", "openai", "", tokenOK)
	if err := os.Remove(w.e.KeyFile); err != nil {
		t.Fatal(err)
	}
	if _, e := w.do("delete_token", w.delArgs(w.alice, "openai")); e != nil {
		t.Fatalf("deleting the last key: %v", e.Code)
	}
	if _, ok := w.s.currentOpenAIKey(); ok {
		t.Fatal("a key is still in effect")
	}
}

func TestRequestIDOnceAcrossRestart(t *testing.T) {
	w := newWEnv(t)
	args := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
	if _, e := w.do("set_token", args); e != nil {
		t.Fatal(e.Code)
	}
	s2 := w.restart(nil)
	raw, _ := json.Marshal(args)
	if _, e := s2.Handlers["set_token"](context.Background(), raw); codeOf(e) != "request_reused" {
		t.Fatalf("replay after restart: %v", e)
	}
	// the delete signature is a different request ID and works on the restarted broker
	d := w.delArgs(w.alice, "gitlab-a")
	raw, _ = json.Marshal(d)
	if _, e := s2.Handlers["delete_token"](context.Background(), raw); e != nil {
		t.Fatal(e.Code)
	}
}

func TestExactCaseKeysAndNoUnknownFields(t *testing.T) {
	w := newWEnv(t)
	good := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
	before := w.tokSnap()
	raw, _ := json.Marshal(good)
	rawS := string(raw)
	variants := map[string]string{
		"capital Value":   strings.Replace(rawS, `"value":`, `"Value":`, 1),
		"capital Ref":     strings.Replace(rawS, `"ref":`, `"Ref":`, 1),
		"capital Kind":    strings.Replace(rawS, `"kind":`, `"KIND":`, 1),
		"duplicate value": strings.Replace(rawS, `{`, `{"value":"`+tokenOK2+`",`, 1),
		"duplicate ref":   strings.Replace(rawS, `{`, `{"ref":"gitlab-b",`, 1),
		"unknown field":   strings.Replace(rawS, `{`, `{"extra":1,`, 1),
		"trailing data":   rawS + `{}`,
		"not an object":   `[1]`,
	}
	for name, v := range variants {
		_, e := w.s.Handlers["set_token"](context.Background(), json.RawMessage(v))
		if codeOf(e) != "bad_request" {
			t.Fatalf("%s: code %q", name, codeOf(e))
		}
	}
	dd, _ := json.Marshal(w.delArgs(w.alice, "gitlab-a"))
	for name, v := range map[string]string{
		"delete with value": strings.Replace(string(dd), `{`, `{"value":"x",`, 1),
		"delete capital":    strings.Replace(string(dd), `"ref":`, `"Ref":`, 1),
	} {
		_, e := w.s.Handlers["delete_token"](context.Background(), json.RawMessage(v))
		if codeOf(e) != "bad_request" {
			t.Fatalf("%s: code %q", name, codeOf(e))
		}
	}
	if after := w.tokSnap(); !before.sameStore(after) || !before.sameUsed(after) {
		t.Fatal("a malformed request changed state or burned the ID")
	}
	if _, e := w.do("set_token", good); e != nil {
		t.Fatal(e.Code)
	}
}

func TestConcurrentSetSameRefOneWins(t *testing.T) {
	w := newWEnv(t)
	args := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK)
	raw, _ := json.Marshal(args)
	const n = 12
	codes := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := w.s.Handlers["set_token"](context.Background(), raw)
			codes[i] = codeOf(e)
		}(i)
	}
	wg.Wait()
	ok, reused := 0, 0
	for _, c := range codes {
		switch c {
		case "":
			ok++
		case "request_reused":
			reused++
		default:
			t.Fatalf("unexpected code %q", c)
		}
	}
	if ok != 1 || reused != n-1 {
		t.Fatalf("ok=%d reused=%d", ok, reused)
	}
	// different request IDs for the same ref: each valid one applies, the last write wins consistently
	var wg2 sync.WaitGroup
	vals := []string{tokenOK, tokenOK2, strings.Repeat("z", 40)}
	for _, v := range vals {
		a := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", v)
		r, _ := json.Marshal(a)
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if _, e := w.s.Handlers["set_token"](context.Background(), r); e != nil {
				t.Errorf("concurrent set: %v", e.Code)
			}
		}()
	}
	wg2.Wait()
	got, ok2 := w.s.Tokens.Value("gitlab-a")
	list, _ := w.s.Tokens.List(w.now)
	if !ok2 || len(list) != 1 || list[0].Last4 != got[len(got)-4:] {
		t.Fatalf("store and metadata disagree: %q %+v", got, list)
	}
}

func TestNoValueInAnyResponseOrLog(t *testing.T) {
	canary := "canary-NOT-A-REAL-TOKEN-ZZZZZZZZZZZZ9876"
	w := newWEnv(t)
	var seen []string
	wire := func(op string, args map[string]any) {
		r, out := call(t, w.sock, line(t, rpc(op, args)))
		_ = r
		seen = append(seen, string(out))
	}
	// success
	wire("set_token", w.tokArgs(w.alice, "gitlab-a", "gitlab", "2027-01-01", canary))
	wire("set_token", w.tokArgs(w.alice, "gitlab-a", "gitlab", "2027-01-01", canary+"x")) // replace (value suffix differs)
	// failures that carry the value
	wire("set_token", copyArgs(w.tokArgs(w.alice, "gitlab-a", "gitlab", "", canary), func(a map[string]any) { delete(a, "signature") }))
	wire("set_token", copyArgs(w.tokArgs(w.alice, "gitlab-a", "gitlab", "", canary), func(a map[string]any) { delete(a, "change") }))
	wire("set_token", copyArgs(w.tokArgs(w.alice, "gitlab-a", "gitlab", "", canary+"y"), func(a map[string]any) { a["value"] = canary }))
	wire("set_token", w.tokArgs(w.alice, "gitlab-a", "gitlab", "", canary+" "))
	wire("set_token", w.tokArgs(w.alice, "gitlab-a", "github", "", canary))
	wire("set_token", w.tokArgs(w.alice, "my-ai", "openai", "", canary))
	wire("set_token", w.tokArgs(w.alice, "gitlab-a", "gitlab", "2001-01-01", canary))
	wire("set_token", copyArgs(w.tokArgs(w.alice, "gitlab-a", "gitlab", "", canary), func(a map[string]any) { a["Value"] = canary }))
	bad := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", canary)
	bad["kind"] = "bad" + canary
	wire("set_token", bad)
	wire("set_token", w.tokArgs(w.alice, "gitlab-a", "gitlab", "", canary[:20]))
	wire("delete_token", w.delArgs(w.alice, "gitlab-a"))
	wire("delete_token", w.delArgs(w.alice, "gitlab-a"))
	seen = append(seen, w.logs.String())
	for _, op := range []string{"tokens", "status"} {
		_, out := call(t, w.sock, `{"v":1,"op":"`+op+`"}`)
		seen = append(seen, string(out))
	}
	// a panic after the value is in hand must not print it
	real := w.s.Handlers["set_token"]
	w.s.Handlers["set_token"] = func(ctx context.Context, raw json.RawMessage) (any, *OpError) {
		real(ctx, raw)
		panic("boom " + canary)
	}
	wire("set_token", w.tokArgs(w.alice, "gitlab-b", "gitlab", "", canary))
	w.s.Handlers["set_token"] = real
	seen = append(seen, w.logs.String())
	// every non-secret file in the state dir
	filepath.Walk(filepath.Join(w.e.Dir, "state"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && filepath.Base(filepath.Dir(p)) != "secrets" {
			b, _ := os.ReadFile(p)
			seen = append(seen, string(b))
		}
		return nil
	})
	all := strings.Join(seen, "\n")
	if strings.Contains(all, "canary") {
		i := strings.Index(all, "canary")
		t.Fatalf("the value reached an output near %q", all[max(0, i-80):min(len(all), i+80)])
	}
	if h := registrymsg.ValueHash("00000000000000000000000000000000", canary); strings.Contains(all, h) {
		t.Fatal("unexpected hash")
	}
}

func TestWriteOpsRefuseWhenTokensBroken(t *testing.T) {
	w := newWEnv(t)
	w.mustSet("gitlab-a", "gitlab", "", tokenOK)
	metaPath := filepath.Join(w.e.Dir, "state", "tokens.json")
	if err := os.WriteFile(metaPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := w.restart(nil)
	usedBefore := w.usedFile()
	secBefore, _ := w.tokenFiles()
	for _, c := range []struct {
		op   string
		args map[string]any
	}{
		{"set_token", w.tokArgs(w.alice, "gitlab-b", "gitlab", "", tokenOK2)},
		{"delete_token", w.delArgs(w.alice, "gitlab-a")},
		{"tokens", nil},
	} {
		raw, _ := json.Marshal(c.args)
		if _, e := s2.Handlers[c.op](context.Background(), raw); codeOf(e) != "tokens_unavailable" {
			t.Fatalf("%s with broken tokens: %v", c.op, e)
		}
	}
	if b, _ := os.ReadFile(metaPath); string(b) != "{broken" {
		t.Fatalf("broken metadata was touched: %q", b)
	}
	secAfter, _ := w.tokenFiles()
	if len(secAfter) != len(secBefore) || !bytes.Equal(usedBefore, w.usedFile()) {
		t.Fatal("state changed or an ID was consumed")
	}
	// the project registry still works: token trouble does not disable project ops
	if _, e := s2.Handlers["projects"](context.Background(), nil); e != nil {
		t.Fatalf("projects: %v", e.Code)
	}
}

func TestWriteOpsRefuseWhenUsedRequestsBroken(t *testing.T) {
	w := newWEnv(t)
	os.WriteFile(filepath.Join(w.e.Dir, "state", UsedRequestsFileName), []byte("{broken"), 0o600)
	s2 := w.restart(nil)
	raw, _ := json.Marshal(w.tokArgs(w.alice, "gitlab-a", "gitlab", "", tokenOK))
	if _, e := s2.Handlers["set_token"](context.Background(), raw); codeOf(e) != "registry_unavailable" {
		t.Fatalf("set_token: %v", e)
	}
	if sec, _ := w.tokenFiles(); len(sec) != 0 {
		t.Fatal("a secret was written")
	}
}

func TestOpAllowlistDeleteProjectStillUnknown(t *testing.T) {
	w := newWEnv(t)
	for _, op := range []string{"delete_project", "unregister_project", "remove_project", "delete", "delete_secret",
		"remove_token", "unset_token", "set_secret", "get_token", "update_token", "delete_tokens", "register", "delete_Token", "set_Token"} {
		r, _ := call(t, w.sock, `{"v":1,"op":"`+op+`","args":{}}`)
		if r.OK || r.Code != "unknown_op" {
			t.Fatalf("op %s: %+v", op, r)
		}
	}
	for _, op := range []string{"tokens", "set_token", "delete_token"} {
		r, _ := call(t, w.sock, `{"v":1,"op":"`+op+`","args":{}}`)
		if r.Code == "unknown_op" {
			t.Fatalf("op %s is not registered", op)
		}
	}
}

func TestTokenFixedMessages(t *testing.T) {
	codes := []string{"token_unknown", "bad_kind", "token_too_short", "token_bad_format", "value_mismatch",
		"token_exists_other_kind", "tokens_unavailable", "openai_singleton"}
	seen := map[string]string{}
	for _, c := range codes {
		m, ok := fixedMessages[c]
		if !ok || strings.TrimSpace(m) == "" {
			t.Fatalf("code %s has no fixed message", c)
		}
		if prev, dup := seen[m]; dup {
			t.Fatalf("codes %s and %s share a sentence", prev, c)
		}
		seen[m] = c
	}
	w := newWEnv(t)
	a := w.tokArgs(w.alice, "gitlab-a", "gitlab", "", "REFLECT-ME "+tokenOK)
	r, _ := call(t, w.sock, line(t, rpc("set_token", a)))
	if r.OK || r.Code != "token_bad_format" || r.Message != fixedMessages["token_bad_format"] || strings.Contains(r.Message, "REFLECT") {
		t.Fatalf("reply: %+v", r)
	}
}

func TestHandlerTimeoutsExplicit(t *testing.T) {
	for _, op := range []string{"set_token", "delete_token"} {
		if d, ok := defaultOpTimeouts[op]; !ok || d != 10*time.Second {
			t.Fatalf("op %s timeout = %v, %v", op, d, ok)
		}
	}
	if d, ok := defaultOpTimeouts["tokens"]; !ok || d != 5*time.Second {
		t.Fatalf("tokens timeout = %v, %v", d, ok)
	}
}

// A valid signature over one change cannot carry other fields: the signer's
// change must equal the hash recomputed from the request.
func TestSetTokenFieldsMustMatchSignedChange(t *testing.T) {
	w := newWEnv(t)
	good := w.tokArgs(w.alice, "gitlab-a", "gitlab", "2027-01-01", tokenOK)
	before := w.tokSnap()
	for name, mod := range map[string]func(map[string]any){
		"other expires":    func(a map[string]any) { a["expires"] = "2028-01-01" },
		"dropped expires":  func(a map[string]any) { a["expires"] = "" },
		"other kind":       func(a map[string]any) { a["kind"] = "github" },
		"other value_hash": func(a map[string]any) { a["value_hash"] = registrymsg.ValueHash(registrymsg.NewRequestID(), tokenOK2) },
	} {
		args := copyArgs(good, mod)
		w.wantCode("set_token", args, "change_mismatch")
		if after := w.tokSnap(); !before.sameStore(after) {
			t.Fatalf("%s: store changed", name)
		}
	}
	w.wantCode("set_token", copyArgs(good, func(a map[string]any) { delete(a, "value_hash") }), "bad_request")
}
