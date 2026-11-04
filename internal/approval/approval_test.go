package approval

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)

func newStore(t *testing.T) Store {
	t.Helper()
	return Store{Path: filepath.Join(t.TempDir(), "sub", "approvals.json")}
}

func TestGrantThenValidExactTripleOnly(t *testing.T) {
	s := newStore(t)
	if err := s.Grant("r", "b", "h", t0); err != nil {
		t.Fatal(err)
	}
	if !s.Valid("r", "b", "h") {
		t.Fatal("exact triple should be valid")
	}
	for _, c := range [][3]string{{"r", "b", "x"}, {"r", "x", "h"}, {"x", "b", "h"}, {"", "b", "h"}, {"r", "", "h"}, {"r", "b", ""}} {
		if s.Valid(c[0], c[1], c[2]) {
			t.Fatalf("%v must not be valid", c)
		}
	}
}

func TestValidMissingFileFalse(t *testing.T) {
	if newStore(t).Valid("r", "b", "h") {
		t.Fatal("missing file must be false")
	}
}

func TestCorruptOrUnknownVersion(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":   "{not json",
		"version":   `{"version":2,"approvals":[{"repo":"r","base":"b","hash":"h","granted_at":"2026-11-04T00:00:00Z"}]}`,
		"noversion": `{"approvals":[{"repo":"r","base":"b","hash":"h"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.Path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if s.Valid("r", "b", "h") {
				t.Fatal("must be false")
			}
			if err := s.Grant("r2", "b", "h", t0); err == nil {
				t.Fatal("Grant must error")
			}
			got, _ := os.ReadFile(s.Path)
			if string(got) != content {
				t.Fatal("file was modified")
			}
		})
	}
}

func TestGrantEmptyArgsRefused(t *testing.T) {
	s := newStore(t)
	for _, c := range [][3]string{{"", "b", "h"}, {"r", "", "h"}, {"r", "b", ""}} {
		if err := s.Grant(c[0], c[1], c[2], t0); err == nil {
			t.Fatalf("%v should error", c)
		}
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatal("file must not be created")
	}
}

func TestGrantIdempotentKeepsOriginalTime(t *testing.T) {
	s := newStore(t)
	if err := s.Grant("r", "b", "h", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Grant("r", "b", "h", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(s.Path)
	if n := strings.Count(string(data), `"hash"`); n != 1 {
		t.Fatalf("want 1 entry, got %d: %s", n, data)
	}
	if !strings.Contains(string(data), "2026-11-04T01:02:03Z") {
		t.Fatalf("original granted_at lost: %s", data)
	}
}

func TestPermissions(t *testing.T) {
	s := newStore(t)
	if err := s.Grant("r", "b", "h", t0); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file perm: %v %v", fi, err)
	}
	di, err := os.Stat(filepath.Dir(s.Path))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm: %v %v", di, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(s.Path))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestConcurrentGrantsNoLostEntries(t *testing.T) {
	s := newStore(t)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Grant("r", "b", string(rune('a'+i)), t0); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if !s.Valid("r", "b", string(rune('a'+i))) {
			t.Fatalf("entry %d lost", i)
		}
	}
}

func TestRequireTTYRejectsNonTerminals(t *testing.T) {
	if err := RequireTTY(nil); err == nil {
		t.Fatal("nil must error")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := RequireTTY(r); err == nil {
		t.Fatal("pipe must error")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if err := RequireTTY(null); err == nil {
		t.Fatal("/dev/null must error")
	}
}

func TestRequireTTYPositiveViaInjection(t *testing.T) {
	old := isTerminal
	defer func() { isTerminal = old }()
	isTerminal = func(int) bool { return true }
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if err := RequireTTY(r); err != nil {
		t.Fatalf("injected terminal should pass: %v", err)
	}
}
