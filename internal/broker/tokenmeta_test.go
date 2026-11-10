package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var tokNow = time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC)

func newTestTokenStore(t *testing.T) (*TokenStore, string) {
	t.Helper()
	dir := shortTempDir(t)
	ts, err := OpenTokenStore(dir, RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ts, dir
}

func TestTokenMetaBrokenIsFailClosed(t *testing.T) {
	dir := shortTempDir(t)
	good, err := OpenTokenStore(dir, RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Set("openai", "openai", "", canaryValue, tokNow); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, TokensFileName)
	secPath := filepath.Join(dir, "secrets", "openai")
	for name, content := range map[string]string{
		"garbage":   "{not json",
		"version":   `{"version":2,"tokens":[]}`,
		"unknown":   `{"version":1,"tokens":[],"extra":1}`,
		"bad ref":   `{"version":1,"tokens":[{"ref":"../x","kind":"gitlab","last4":"abcd","expires":"","set_at":"2026-11-10T00:00:00Z"}]}`,
		"bad kind":  `{"version":1,"tokens":[{"ref":"a","kind":"slack","last4":"abcd","expires":"","set_at":"2026-11-10T00:00:00Z"}]}`,
		"duplicate": `{"version":1,"tokens":[{"ref":"a","kind":"gitlab","last4":"abcd","expires":"","set_at":"2026-11-10T00:00:00Z"},{"ref":"a","kind":"gitlab","last4":"abcd","expires":"","set_at":"2026-11-10T00:00:00Z"}]}`,
		"openai x":  `{"version":1,"tokens":[{"ref":"other","kind":"openai","last4":"abcd","expires":"","set_at":"2026-11-10T00:00:00Z"}]}`,
		"bad date":  `{"version":1,"tokens":[{"ref":"a","kind":"gitlab","last4":"abcd","expires":"2026-13-40","set_at":"2026-11-10T00:00:00Z"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.WriteFile(metaPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			ts, err := OpenTokenStore(dir, RegistryOptions{})
			if !errors.Is(err, ErrTokensBroken) {
				t.Fatalf("Open err = %v, want ErrTokensBroken", err)
			}
			if ts == nil {
				t.Fatal("a broken store must still be returned (the broker keeps running)")
			}
			if !errors.Is(ts.Err(), ErrTokensBroken) {
				t.Fatal("Err() lost")
			}
			if _, ok := ts.Value("openai"); ok {
				t.Fatal("a secret was read while the metadata is broken")
			}
			if !errors.Is(ts.Set("gitlab-a", "gitlab", "", canaryValue, tokNow), ErrTokensBroken) {
				t.Fatal("Set must be refused")
			}
			if !errors.Is(ts.Delete("openai"), ErrTokensBroken) {
				t.Fatal("Delete must be refused")
			}
			if _, err := ts.List(tokNow); !errors.Is(err, ErrTokensBroken) {
				t.Fatalf("List err = %v", err)
			}
			if b, _ := os.ReadFile(metaPath); string(b) != content {
				t.Fatal("tokens.json was changed")
			}
			if _, err := os.Lstat(filepath.Join(dir, "secrets", "gitlab-a")); err == nil {
				t.Fatal("a secret was written under broken metadata")
			}
			if _, err := os.Stat(secPath); err != nil {
				t.Fatal("an existing secret was touched")
			}
		})
	}
	// wrong mode and symlinked tokens.json are broken too
	os.Remove(metaPath)
	os.WriteFile(metaPath, []byte(`{"version":1,"tokens":[]}`), 0o644)
	if _, err := OpenTokenStore(dir, RegistryOptions{}); !errors.Is(err, ErrTokensBroken) {
		t.Fatalf("0644 tokens.json accepted: %v", err)
	}
	os.Remove(metaPath)
	real := filepath.Join(dir, "real.json")
	os.WriteFile(real, []byte(`{"version":1,"tokens":[]}`), 0o600)
	os.Symlink(real, metaPath)
	if _, err := OpenTokenStore(dir, RegistryOptions{}); !errors.Is(err, ErrTokensBroken) {
		t.Fatalf("symlinked tokens.json accepted: %v", err)
	}
}

func TestTokenMetaMissingFileMeansEmptyOnlyWhenAbsent(t *testing.T) {
	ts, dir := newTestTokenStore(t)
	if ts.Err() != nil {
		t.Fatal(ts.Err())
	}
	list, err := ts.List(tokNow)
	if err != nil || len(list) != 0 {
		t.Fatalf("absent file: %v %v", list, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, TokensFileName)); err == nil {
		t.Fatal("opening must not create tokens.json (only the first Set does)")
	}
	di, err := os.Lstat(filepath.Join(dir, "secrets"))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("secrets/ not created 0700: %v %v", di, err)
	}
	if err := ts.Set("gitlab-a", "gitlab", "2027-01-31", canaryValue, tokNow); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dir, TokensFileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("tokens.json mode: %v %v", fi, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, TokensFileName))
	if strings.Contains(string(b), canaryValue) {
		t.Fatal("tokens.json carries the value")
	}
	var f struct {
		Version int
		Tokens  []map[string]string
	}
	if json.Unmarshal(b, &f) != nil || f.Version != 1 || len(f.Tokens) != 1 {
		t.Fatalf("tokens.json = %s", b)
	}
	e := f.Tokens[0]
	if e["ref"] != "gitlab-a" || e["kind"] != "gitlab" || e["last4"] != canaryValue[len(canaryValue)-4:] || e["expires"] != "2027-01-31" || e["set_at"] != "2026-11-10T15:04:05Z" {
		t.Fatalf("entry = %v", e)
	}
	// a second open (restart) sees it
	ts2, err := OpenTokenStore(dir, RegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := ts2.Value("gitlab-a"); !ok || v != canaryValue {
		t.Fatal("restart lost the token")
	}
	// naming rules
	if ts.Set("openai", "gitlab", "", canaryValue, tokNow) == nil {
		t.Error("openai name with another kind accepted")
	}
	if ts.Set("other", "openai", "", canaryValue, tokNow) == nil {
		t.Error("openai kind with another name accepted")
	}
	if !errors.Is(ts.Set("gitlab-a", "github", "", canaryValue, tokNow), ErrTokenKindMismatch) {
		t.Error("same ref, other kind accepted")
	}
	if ts.Set("x", "slack", "", canaryValue, tokNow) == nil {
		t.Error("unknown kind accepted")
	}
	if ts.Set("x", "gitlab", "2027-02-30", canaryValue, tokNow) == nil {
		t.Error("impossible date accepted")
	}
	if !errors.Is(ts.Delete("nope"), ErrTokenUnknown) {
		t.Error("deleting an unknown token must say so")
	}
	if err := ts.Delete("gitlab-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "secrets", "gitlab-a")); err == nil {
		t.Fatal("secret file left after delete")
	}
	if list, _ := ts.List(tokNow); len(list) != 0 {
		t.Fatalf("meta left after delete: %v", list)
	}
}

func TestMetaAndFileMismatchHandled(t *testing.T) {
	ts, dir := newTestTokenStore(t)
	// file without metadata (orphan): never used, never listed, reported, replaceable
	if err := ts.Secrets.Put("openai", canaryValue); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.Value("openai"); ok {
		t.Fatal("a secret without metadata was used")
	}
	list, _ := ts.List(tokNow)
	if len(list) != 0 {
		t.Fatalf("orphan listed: %v", list)
	}
	if o := ts.Orphans(); len(o) != 1 || o[0] != "openai" {
		t.Fatalf("Orphans = %v", o)
	}
	if err := ts.Set("openai", "openai", "", canaryValue+"new", tokNow); err != nil {
		t.Fatal(err)
	}
	if v, ok := ts.Value("openai"); !ok || v != canaryValue+"new" {
		t.Fatal("orphan not replaced")
	}
	if len(ts.Orphans()) != 0 {
		t.Fatal("still orphan")
	}
	// metadata without a file: unusable, listed as unusable, deletable
	if err := os.Remove(filepath.Join(dir, "secrets", "openai")); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.Value("openai"); ok {
		t.Fatal("a missing file was used")
	}
	list, _ = ts.List(tokNow)
	if len(list) != 1 || list[0].Ref != "openai" || list[0].Usable {
		t.Fatalf("meta-only entry = %+v", list)
	}
	if err := ts.Delete("openai"); err != nil {
		t.Fatalf("delete of a meta-only entry: %v", err)
	}
	if list, _ := ts.List(tokNow); len(list) != 0 {
		t.Fatal("meta-only entry survived delete")
	}
	// an untrusted file counts as unusable, not as a startup failure
	ts.Set("gitlab-a", "gitlab", "", canaryValue, tokNow)
	os.Chmod(filepath.Join(dir, "secrets", "gitlab-a"), 0o644)
	if _, ok := ts.Value("gitlab-a"); ok {
		t.Fatal("untrusted file used")
	}
	list, _ = ts.List(tokNow)
	if len(list) != 1 || list[0].Usable {
		t.Fatalf("untrusted entry listed usable: %+v", list)
	}
}

func TestSetMetaFailureRestoresPreviousValue(t *testing.T) {
	ts, _ := newTestTokenStore(t)
	if err := ts.Set("openai", "openai", "", canaryValue+"old", tokNow); err != nil {
		t.Fatal(err)
	}
	ts.Meta.opts.failAt = func(string) error { return errors.New("injected") }
	if ts.Set("openai", "openai", "", canaryValue+"new", tokNow) == nil {
		t.Fatal("meta failure hidden")
	}
	ts.Meta.opts.failAt = nil
	if v, _ := ts.Value("openai"); v != canaryValue+"old" {
		t.Fatalf("value not rolled back: %q", v)
	}
	// first token: rollback removes the file
	ts.Meta.opts.failAt = func(string) error { return errors.New("injected") }
	if ts.Set("gitlab-a", "gitlab", "", canaryValue, tokNow) == nil {
		t.Fatal("no error")
	}
	ts.Meta.opts.failAt = nil
	if ts.Secrets.Lookup("gitlab-a") != SecretAbsent {
		t.Fatal("secret left behind after failed set")
	}
}

// --- openai key: precedence, hot reload, missing key ---

func openAIFake(t *testing.T, hold chan struct{}) (*fakeHTTP, *[]string, *sync.Mutex) {
	var mu sync.Mutex
	var auth []string
	f := newFakeHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		n := len(auth)
		mu.Unlock()
		io.Copy(io.Discard, r.Body)
		if hold != nil && n == 1 {
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	})
	return f, &auth, &mu
}

func lastAuth(auth *[]string, mu *sync.Mutex) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), (*auth)...)
}

func TestOpenAIKeyPrecedenceStoreOverConfig(t *testing.T) {
	r := newRefineEnv(t, nil, nil)
	s := r.srv
	if k, ok := s.currentOpenAIKey(); !ok || k != testOpenAIKey {
		t.Fatalf("config key not used when the store has none: %q %v", k, ok)
	}
	storeKey := "store-key-NOT-REAL-0123456789abcdef0123"
	if err := s.Tokens.Set("openai", "openai", "", storeKey, tokNow); err != nil {
		t.Fatal(err)
	}
	if k, ok := s.currentOpenAIKey(); !ok || k != storeKey {
		t.Fatalf("store key must win: %q %v", k, ok)
	}
	// untrusted store file: fall back to the config key
	p := filepath.Join(s.Tokens.Secrets.Dir, "openai")
	os.Chmod(p, 0o644)
	if k, ok := s.currentOpenAIKey(); !ok || k != testOpenAIKey {
		t.Fatalf("untrusted store file: got %q %v", k, ok)
	}
	os.Chmod(p, 0o400)
	if k, _ := s.currentOpenAIKey(); k != storeKey {
		t.Fatal("trust restored but key not used")
	}
	if err := s.Tokens.Delete("openai"); err != nil {
		t.Fatal(err)
	}
	if k, ok := s.currentOpenAIKey(); !ok || k != testOpenAIKey {
		t.Fatalf("after delete the config key must be used again: %q %v", k, ok)
	}
}

func TestOpenAIKeyHotReloadNextCall(t *testing.T) {
	hold := make(chan struct{})
	fake, auth, mu := openAIFake(t, hold)
	r := newRefineEnv(t, fake, nil)
	s := r.srv
	newKey := "hot-key-NOT-REAL-0123456789abcdef012345"
	done := make(chan struct{})
	go func() { r.call(t, r.args()); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(lastAuth(auth, mu)) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// the first call is in flight with the config key; replace the key now
	if err := s.Tokens.Set("openai", "openai", "", newKey, tokNow); err != nil {
		t.Fatal(err)
	}
	close(hold)
	<-done
	r.call(t, r.args())
	got := lastAuth(auth, mu)
	if len(got) != 2 {
		t.Fatalf("calls = %v", got)
	}
	if got[0] != "Bearer "+testOpenAIKey {
		t.Errorf("in-flight call changed key: %q", got[0])
	}
	if got[1] != "Bearer "+newKey {
		t.Errorf("next call did not use the new key: %q", got[1])
	}
}

func TestAIKeyMissingIsFixedErrorAndNoBudgetUse(t *testing.T) {
	fake, auth, mu := openAIFake(t, nil)
	r := newRefineEnv(t, fake, nil)
	s := r.srv
	if err := s.Tokens.Set("openai", "openai", "", "store-key-NOT-REAL-0123456789abcdef0123", tokNow); err != nil {
		t.Fatal(err)
	}
	if err := s.Tokens.Delete("openai"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.env.KeyFile); err != nil {
		t.Fatal(err)
	}
	if _, ok := fixedMessages["ai_key_missing"]; !ok {
		t.Fatal("ai_key_missing is not a registered fixed code")
	}
	budgetBefore := s.State.Budget.RemainingMicros()
	slotsBefore := s.State.Refine.Remaining()
	for i := 0; i < 3; i++ {
		_, oerr := r.call(t, r.args())
		if oerr == nil || oerr.Code != "ai_key_missing" {
			t.Fatalf("call %d: %+v, want ai_key_missing", i, oerr)
		}
	}
	if len(lastAuth(auth, mu)) != 0 || fake.Hits() != 0 {
		t.Fatal("a call without a key reached upstream")
	}
	if s.State.Budget.RemainingMicros() != budgetBefore {
		t.Fatal("a missing key reserved budget")
	}
	if s.State.Refine.Remaining() != slotsBefore {
		t.Fatal("a missing key consumed an hourly slot")
	}
	// over the socket the code reaches the caller with the fixed sentence
	rep, _ := call(t, r.env.Raw["socket"].(string), `{"v":1,"op":"refine","args":`+tokJSON(t, r.args())+`}`)
	if rep.OK || rep.Code != "ai_key_missing" || rep.Message != fixedMessages["ai_key_missing"] {
		t.Fatalf("reply = %+v", rep)
	}
	// putting a key back makes refine work again
	if err := s.Tokens.Set("openai", "openai", "", "store-key-NOT-REAL-0123456789abcdef0123", tokNow); err != nil {
		t.Fatal(err)
	}
	// the fake answers "{}" (a bad plan): what matters is that the call went out
	if _, oerr := r.call(t, r.args()); oerr != nil && oerr.Code == "ai_key_missing" {
		t.Fatalf("after restoring a key: %+v", oerr)
	}
	if fake.Hits() != 1 {
		t.Fatalf("restored key did not reach upstream (%d hits)", fake.Hits())
	}
}

func tokJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- expiry ---

func TestStatusAndTokensExpiry(t *testing.T) {
	d := func(p *int) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprint(*p)
	}
	for _, c := range []struct {
		expires, want string
	}{
		{"", "nil"},
		{"garbage", "nil"},
		{"2026-11-10", "0"},  // today
		{"2026-11-09", "-1"}, // yesterday: expired
		{"2026-11-11", "1"},
		{"2026-11-24", "14"}, // boundary: still warns
		{"2026-11-25", "15"}, // one day past the boundary
		{"2027-11-10", "365"},
	} {
		if got := d(DaysLeft(c.expires, tokNow)); got != c.want {
			t.Errorf("DaysLeft(%q) = %s, want %s", c.expires, got, c.want)
		}
	}
	// the date is compared as a UTC calendar day, the clock's time of day does not matter
	late := time.Date(2026, 11, 10, 23, 59, 59, 0, time.UTC)
	early := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	if *DaysLeft("2026-11-10", late) != 0 || *DaysLeft("2026-11-10", early) != 0 {
		t.Error("time of day leaked into the day count")
	}
	if !Expiring(ptrInt(14)) || !Expiring(ptrInt(0)) || !Expiring(ptrInt(-3)) || Expiring(ptrInt(15)) || Expiring(nil) {
		t.Error("Expiring boundary wrong")
	}

	r := newRefineEnv(t, nil, nil)
	s := r.srv
	s.Now = func() time.Time { return tokNow }
	for _, tk := range []struct{ ref, kind, exp string }{
		{"gitlab-soon", "gitlab", "2026-11-24"},
		{"gitlab-late", "gitlab", "2026-11-25"},
		{"github-gone", "github", "2026-11-09"},
		{"gitlab-none", "gitlab", ""},
	} {
		if err := s.Tokens.Set(tk.ref, tk.kind, tk.exp, canaryValue, tokNow); err != nil {
			t.Fatal(err)
		}
	}
	sock := r.env.Raw["socket"].(string)
	rep, out := call(t, sock, `{"v":1,"op":"tokens"}`)
	if !rep.OK {
		t.Fatalf("tokens: %s", out)
	}
	var tr struct {
		WarnDays int `json:"warn_days"`
		Tokens   []struct {
			Ref      string `json:"ref"`
			Kind     string `json:"kind"`
			Last4    string `json:"last4"`
			Expires  string `json:"expires"`
			SetAt    string `json:"set_at"`
			Source   string `json:"source"`
			DaysLeft *int   `json:"days_left"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rep.Result, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.WarnDays != 14 {
		t.Errorf("warn_days = %d", tr.WarnDays)
	}
	byRef := map[string]int{}
	for i, tk := range tr.Tokens {
		byRef[tk.Ref] = i
	}
	if i, ok := byRef["gitlab-soon"]; !ok || tr.Tokens[i].DaysLeft == nil || *tr.Tokens[i].DaysLeft != 14 || tr.Tokens[i].Source != "store" || tr.Tokens[i].Last4 != canaryValue[len(canaryValue)-4:] {
		t.Errorf("gitlab-soon = %+v", tr.Tokens)
	}
	if i := byRef["github-gone"]; *tr.Tokens[i].DaysLeft != -1 {
		t.Error("expired token days_left wrong")
	}
	if i := byRef["gitlab-none"]; tr.Tokens[i].DaysLeft != nil {
		t.Error("empty expiry must give null days_left")
	}
	if i, ok := byRef["openai"]; !ok || tr.Tokens[i].Source != "config" || tr.Tokens[i].DaysLeft != nil {
		t.Errorf("config key not listed as source config: %+v", tr.Tokens)
	}
	if strings.Contains(string(out), canaryValue) || strings.Contains(string(out), testOpenAIKey) {
		t.Fatal("tokens output carries a value")
	}

	rep, out = call(t, sock, `{"v":1,"op":"status"}`)
	if !rep.OK {
		t.Fatalf("status: %s", out)
	}
	var st struct {
		WarnDays int `json:"warn_days"`
		Expiring []struct {
			Ref      string `json:"ref"`
			DaysLeft int    `json:"days_left"`
		} `json:"expiring"`
		TokensAvailable bool `json:"tokens_available"`
	}
	if err := json.Unmarshal(rep.Result, &st); err != nil {
		t.Fatal(err)
	}
	if !st.TokensAvailable || st.WarnDays != 14 || len(st.Expiring) != 2 {
		t.Fatalf("status expiry summary = %s", rep.Result)
	}
	got := map[string]int{}
	for _, e := range st.Expiring {
		got[e.Ref] = e.DaysLeft
	}
	if got["gitlab-soon"] != 14 || got["github-gone"] != -1 {
		t.Errorf("expiring = %v", got)
	}
	if strings.Contains(string(out), canaryValue[len(canaryValue)-4:]) {
		t.Error("status leaks last4")
	}
}

func ptrInt(i int) *int { return &i }

// --- no value anywhere ---

func TestNoValueInAnyOutput(t *testing.T) {
	e := newTestEnv(t)
	s, logsp := e.newServer(t, func(s *Server) { s.Now = func() time.Time { return tokNow } })
	logs := logsp
	r := &refineEnv{env: e, srv: s}
	var seen []string
	collect := func(v ...any) {
		for _, x := range v {
			seen = append(seen, fmt.Sprintf("%v|%+v|%#v|%s", x, x, x, x))
		}
	}
	err := s.Tokens.Set("gitlab-a", "gitlab", "2027-01-01", canaryValue, tokNow)
	collect(err)
	// failing paths: every error string must be free of the value
	for _, bad := range []error{
		s.Tokens.Set("Bad Name", "gitlab", "", canaryValue, tokNow),
		s.Tokens.Set("gitlab-a", "github", "", canaryValue, tokNow),
		s.Tokens.Set("openai", "gitlab", "", canaryValue, tokNow),
		s.Tokens.Set("gitlab-b", "gitlab", "nope", canaryValue, tokNow),
		s.Tokens.Delete("nothere"),
	} {
		collect(bad)
	}
	s.Tokens.Meta.opts.failAt = func(string) error { return errors.New("injected " + "x") }
	collect(s.Tokens.Set("gitlab-c", "gitlab", "", canaryValue, tokNow))
	s.Tokens.Meta.opts.failAt = nil
	s.Tokens.Secrets.opts.failAt = func(string) error { return errors.New("injected") }
	collect(s.Tokens.Secrets.Put("gitlab-d", canaryValue))
	s.Tokens.Secrets.opts.failAt = nil
	list, lerr := s.Tokens.List(tokNow)
	collect(list, lerr, s.Tokens, s.Tokens.Secrets, s.Tokens.Meta, s.Tokens.Orphans(), s.Tokens.Secrets.Refs())
	sock := r.env.Raw["socket"].(string)
	for _, op := range []string{"tokens", "status", "unknown"} {
		_, out := call(t, sock, `{"v":1,"op":"`+op+`"}`)
		seen = append(seen, string(out))
	}
	// a panicking handler must not print the value either
	s.Handlers["boom"] = func(context.Context, json.RawMessage) (any, *OpError) { panic("boom " + canaryValue) }
	_, out := call(t, sock, `{"v":1,"op":"boom"}`)
	seen = append(seen, string(out))
	// a log line that does contain the value is scrubbed by the logger
	s.logf("oops %s", canaryValue)
	seen = append(seen, logs.String())
	// every file except the secret file itself
	filepath.Walk(filepath.Join(r.env.Dir, "state"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && filepath.Base(filepath.Dir(p)) != "secrets" {
			b, _ := os.ReadFile(p)
			seen = append(seen, string(b))
		}
		return nil
	})
	all := strings.Join(seen, "\n")
	if strings.Contains(all, canaryValue) || strings.Contains(all, "canary-NOT-A-REAL") {
		i := strings.Index(all, "canary")
		t.Fatalf("the value reached an output near %q", all[max(0, i-60):min(len(all), i+60)])
	}
	if !bytes.Contains([]byte(logs.String()), []byte("[redacted]")) {
		t.Fatal("the stored value is not part of the log redaction")
	}
}

func TestSetFailureKeepsPreviousLiveAndMetaAgree(t *testing.T) {
	old := "old-value-0123456789012345678901234567"
	for _, target := range []string{"secret", "meta"} {
		for _, stage := range []string{"temp-written", "synced", "dir-synced"} {
			for _, replace := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/replace=%v", target, stage, replace)
				t.Run(name, func(t *testing.T) {
					ts, _ := newTestTokenStore(t)
					if replace {
						if err := ts.Set("gitlab-a", "gitlab", "2027-01-01", old, tokNow); err != nil {
							t.Fatal(err)
						}
					}
					inj := func(st string) error {
						if st == stage {
							return errors.New("injected")
						}
						return nil
					}
					if target == "secret" {
						ts.Secrets.opts.failAt = inj
					} else {
						ts.Meta.opts.failAt = inj
					}
					if ts.Set("gitlab-a", "gitlab", "2028-02-02", canaryValue, tokNow.Add(time.Hour)) == nil {
						t.Fatal("injected failure hidden")
					}
					ts.Secrets.opts.failAt, ts.Meta.opts.failAt = nil, nil
					list, _ := ts.List(tokNow)
					if !replace {
						if len(list) != 0 {
							t.Fatalf("meta of a failed first set: %+v", list)
						}
						if _, ok := ts.Value("gitlab-a"); ok || ts.Secrets.Lookup("gitlab-a") != SecretAbsent {
							t.Fatal("a failed first set left a live value")
						}
						return
					}
					if len(list) != 1 || list[0].Expires != "2027-01-01" || list[0].Last4 != old[len(old)-4:] || !list[0].Usable {
						t.Fatalf("metadata changed by a failed set: %+v", list)
					}
					if v, _ := ts.Value("gitlab-a"); v != old {
						t.Fatalf("live value is not the previous one: %q", v)
					}
					// and what is on disk agrees after a restart
					ts2, err := OpenTokenStore(filepath.Dir(ts.Meta.path), RegistryOptions{})
					if err != nil {
						t.Fatal(err)
					}
					l2, _ := ts2.List(tokNow)
					if v, _ := ts2.Value("gitlab-a"); v != old || len(l2) != 1 || l2[0].Expires != "2027-01-01" {
						t.Fatalf("disk disagrees: %q %+v", v, l2)
					}
				})
			}
		}
	}
}

func TestTokensHandlerShowsBrokenStoredAndConfigRows(t *testing.T) {
	r := newRefineEnv(t, nil, nil)
	s := r.srv
	s.Now = func() time.Time { return tokNow }
	if err := s.Tokens.Set("openai", "openai", "2026-11-20", "store-key-NOT-REAL-0123456789abcdef0123", tokNow); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(s.Tokens.Secrets.Dir, "openai"), 0o644)
	res, oerr := s.tokensHandler(context.Background(), nil)
	if oerr != nil {
		t.Fatal(oerr)
	}
	rows := res.(map[string]any)["tokens"].([]map[string]any)
	var stored, cfg map[string]any
	for _, row := range rows {
		if row["ref"] != "openai" {
			continue
		}
		if row["source"] == "store" {
			stored = row
		} else {
			cfg = row
		}
	}
	if stored == nil || stored["usable"] != false || stored["expires"] != "2026-11-20" {
		t.Fatalf("the broken stored row is hidden: %+v", rows)
	}
	if cfg == nil || cfg["source"] != "config" || cfg["usable"] != true {
		t.Fatalf("the config row is missing: %+v", rows)
	}
}

func TestConcurrentPutDeleteGetConsistent(t *testing.T) {
	ts, _ := newTestTokenStore(t)
	known := map[string]bool{}
	var vals []string
	for i := 0; i < 8; i++ {
		v := fmt.Sprintf("concurrent-value-%02d-%s", i, strings.Repeat("x", 30))
		vals = append(vals, v)
		known[v] = true
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				switch (g + i) % 3 {
				case 0:
					ts.Set("gitlab-a", "gitlab", "", vals[g], tokNow)
				case 1:
					ts.Delete("gitlab-a")
				default:
					if v, ok := ts.Value("gitlab-a"); ok && !known[v] {
						t.Errorf("torn value %q", v)
					}
					ts.List(tokNow)
				}
			}
		}(g)
	}
	wg.Wait()
	list, err := ts.List(tokNow)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := ts.Value("gitlab-a")
	if ok != (len(list) == 1 && list[0].Usable) {
		t.Fatalf("value and metadata disagree: ok=%v list=%+v", ok, list)
	}
	if ok && (!known[v] || list[0].Last4 != v[len(v)-4:]) {
		t.Fatalf("final value %q does not match metadata %+v", v, list)
	}
	if len(ts.Orphans()) != 0 {
		t.Fatal("orphan after the race")
	}
}
