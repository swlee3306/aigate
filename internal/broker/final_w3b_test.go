package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Final fix round (F3, F4, F5).

// F3: the log redaction covers the config OpenAI key as it is NOW (refine
// re-reads the file on every call), not only the value present at start.
func TestLogRedactsCurrentConfigKey(t *testing.T) {
	r := newRefineEnv(t, nil, nil)
	newKey := "rotated-config-key-NOT-REAL-0123456789abcd"
	if err := os.Remove(r.env.KeyFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.env.KeyFile, []byte(newKey+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	r.srv.logf("upstream said: bad key %s end", newKey)
	logs := r.logs.String()
	if strings.Contains(logs, newKey) {
		t.Fatalf("the current config key reached the log: %s", logs)
	}
	if !strings.Contains(logs, "bad key") {
		t.Fatalf("the line itself is missing: %s", logs)
	}
}

// F4: day counting does not overflow time.Duration for far-future dates.
func TestDaysLeftFarFuture(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 59, 59, 0, time.UTC)
	cases := map[string]int{
		"2026-11-10": 0,
		"2026-11-11": 1,
		"2026-11-09": -1,
		"2028-02-29": 476,
		"3000-01-01": 0, // computed below
		"9999-12-31": 0,
	}
	day := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400 }
	cases["3000-01-01"] = int(day(3000, 1, 1) - day(2026, 11, 10))
	cases["9999-12-31"] = int(day(9999, 12, 31) - day(2026, 11, 10))
	if cases["3000-01-01"] != 355433 || cases["9999-12-31"] != 2912129 {
		t.Fatalf("test arithmetic off: %d %d", cases["3000-01-01"], cases["9999-12-31"])
	}
	for exp, want := range cases {
		got := DaysLeft(exp, now)
		if got == nil || *got != want {
			t.Errorf("DaysLeft(%s) = %v, want %d", exp, got, want)
		}
	}
	// beyond the Duration limit (106751 days, year ~2318)
	if d := DaysLeft("2500-01-01", now); d == nil || *d < 150000 {
		t.Errorf("2500-01-01: %v (Duration overflow?)", d)
	}
}

// F5: the broker's date rule is registrymsg.ValidExpires (year >= 2000): a
// hand-edited tokens.json with an earlier year is a broken file (fail closed:
// tokens_unavailable), exactly like any other invalid row, so orchd never sees
// a row it would have to turn into a 502 for the whole list.
func TestTokensJSONYearBefore2000IsBroken(t *testing.T) {
	for _, exp := range []string{"1999-12-31", "0001-01-01", "0000-01-01"} {
		dir := shortTempDir(t)
		if _, err := OpenTokenStore(dir, RegistryOptions{}); err != nil {
			t.Fatal(err)
		}
		content := `{"version":1,"tokens":[{"ref":"a","kind":"gitlab","last4":"abcd","expires":"` + exp + `","set_at":"2026-11-10T00:00:00Z"}]}`
		if err := os.WriteFile(filepath.Join(dir, TokensFileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		ts, err := OpenTokenStore(dir, RegistryOptions{})
		if !errors.Is(err, ErrTokensBroken) {
			t.Errorf("%s: err = %v, want ErrTokensBroken", exp, err)
		}
		if _, lerr := ts.List(tokNow); !errors.Is(lerr, ErrTokensBroken) {
			t.Errorf("%s: List err = %v", exp, lerr)
		}
	}
}

// The redaction list re-reads the config key file only when it changed
// ((mtime, size) differs), so a log line costs one stat, not a file read.
func TestLogKeyCacheAvoidsRereadingUnchangedFile(t *testing.T) {
	r := newRefineEnv(t, nil, nil)
	s := r.srv
	for i := 0; i < 20; i++ {
		s.logf("line %d", i)
	}
	if n := s.logKeyReads.Load(); n > 1 {
		t.Fatalf("unchanged key file read %d times for 20 lines", n)
	}
	base := s.logKeyReads.Load()
	rewrite := func(content string, mtime time.Time) {
		t.Helper()
		if err := os.Remove(r.env.KeyFile); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.env.KeyFile, []byte(content+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(r.env.KeyFile, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	k1 := "cache-key-one-NOT-REAL-0123456789abcdefgh"
	rewrite(k1, old) // different size
	s.logf("saw %s", k1)
	if strings.Contains(r.logs.String(), k1) {
		t.Fatal("a rewritten key (other size) was not picked up on the next line")
	}
	k2 := strings.Replace(k1, "one", "two", 1) // same size, same content length, other mtime
	rewrite(k2, old.Add(time.Minute))
	s.logf("saw %s", k2)
	if strings.Contains(r.logs.String(), k2) {
		t.Fatal("a rewritten key (same size, other mtime) was not picked up")
	}
	if s.logKeyReads.Load() <= base {
		t.Fatal("changes were never read")
	}
	reads := s.logKeyReads.Load()
	s.logf("again")
	s.logf("again")
	if s.logKeyReads.Load() != reads {
		t.Fatal("unchanged file re-read after the refresh")
	}
}
