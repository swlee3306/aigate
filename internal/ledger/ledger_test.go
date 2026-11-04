package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC)

func newPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sub", "ledger", "r.jsonl")
}

func seed(t *testing.T, p string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := Append(p, "gate", "repo1", map[string]any{"i": i, "k": "v"}, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func writeLines(t *testing.T, p string, ls []string) {
	t.Helper()
	s := strings.Join(ls, "\n") + "\n"
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustFail(t *testing.T, p string, wantSubs ...string) {
	t.Helper()
	_, err := Verify(p)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, s := range wantSubs {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("error %q missing %q", err, s)
		}
	}
}

func TestVerifyMissingAndEmpty(t *testing.T) {
	p := newPath(t)
	if n, err := Verify(p); n != 0 || err != nil {
		t.Fatalf("missing: %d %v", n, err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := Verify(p); n != 0 || err != nil {
		t.Fatalf("empty: %d %v", n, err)
	}
}

func TestAppendVerifyAndPerms(t *testing.T) {
	p := newPath(t)
	seed(t, p, 3)
	n, err := Verify(p)
	if n != 3 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file perm %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(p))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm %v", di.Mode().Perm())
	}
	ls := readLines(t, p)
	if !strings.Contains(ls[0], `"prev_hash":"`+strings.Repeat("0", 64)+`"`) || !strings.Contains(ls[0], `"seq":1`) || !strings.Contains(ls[0], `"ts":"2026-11-04T01:02:03Z"`) {
		t.Fatalf("genesis line: %s", ls[0])
	}
}

func TestAppendValidation(t *testing.T) {
	p := newPath(t)
	if Append(p, "", "r", nil, t0) == nil || Append(p, "t", "", nil, t0) == nil {
		t.Fatal("empty typ/repo must fail")
	}
	if err := Append(p, "t", "r", nil, t0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLines(t, p)[0], `"payload":{}`) {
		t.Fatal("nil payload should serialize as {}")
	}
}

func TestPayloadOrderIndependentHash(t *testing.T) {
	a := map[string]any{}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		a[k] = k
	}
	b := map[string]any{}
	for _, k := range []string{"e", "d", "c", "b", "a"} {
		b[k] = k
	}
	h1, err := computeHash(1, "ts", "p", "t", "r", a)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := computeHash(1, "ts", "p", "t", "r", b)
	if h1 != h2 {
		t.Fatal("hash depends on map insertion order")
	}
}

func TestHashNoPipeAmbiguity(t *testing.T) {
	h1, _ := computeHash(1, "ts", "p", "a|b", "c", nil)
	h2, _ := computeHash(1, "ts", "p", "a", "b|c", nil)
	if h1 == h2 {
		t.Fatal("pipe ambiguity")
	}
}

func TestTamperPayload(t *testing.T) {
	p := newPath(t)
	seed(t, p, 3)
	ls := readLines(t, p)
	ls[1] = strings.Replace(ls[1], `"i":1`, `"i":99`, 1)
	writeLines(t, p, ls)
	n, err := Verify(p)
	if err == nil || n != 1 || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "seq 2") || !strings.Contains(err.Error(), "tamper") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestDeleteMiddleLine(t *testing.T) {
	p := newPath(t)
	seed(t, p, 3)
	ls := readLines(t, p)
	writeLines(t, p, []string{ls[0], ls[2]})
	mustFail(t, p, "line 2", "gap")
}

func TestSwapLines(t *testing.T) {
	p := newPath(t)
	seed(t, p, 3)
	ls := readLines(t, p)
	writeLines(t, p, []string{ls[0], ls[2], ls[1]})
	mustFail(t, p, "line 2")
}

func TestTruncatedLastLine(t *testing.T) {
	p := newPath(t)
	seed(t, p, 3)
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(p, b[:len(b)-20], 0o600); err != nil {
		t.Fatal(err)
	}
	mustFail(t, p, "line 3", "trailing newline")
	// Append must refuse too.
	if Append(p, "t", "r", nil, t0) == nil {
		t.Fatal("append onto partial line must fail")
	}
}

func TestMissingNewlineOnValidLastLine(t *testing.T) {
	p := newPath(t)
	seed(t, p, 2)
	b, _ := os.ReadFile(p)
	os.WriteFile(p, b[:len(b)-1], 0o600)
	mustFail(t, p, "line 2", "trailing newline")
}

func TestForgedLineWrongPrevHash(t *testing.T) {
	p := newPath(t)
	seed(t, p, 2)
	h, _ := computeHash(3, "2026-11-04T01:02:03Z", strings.Repeat("a", 64), "gate", "repo1", map[string]any{})
	forged := `{"seq":3,"ts":"2026-11-04T01:02:03Z","prev_hash":"` + strings.Repeat("a", 64) + `","hash":"` + h + `","type":"gate","repo":"repo1","payload":{}}`
	ls := readLines(t, p)
	writeLines(t, p, append(ls, forged))
	n, err := Verify(p)
	if err == nil || n != 2 || !strings.Contains(err.Error(), "line 3") || !strings.Contains(err.Error(), "prev_hash") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestBadJSONAndAppendRefusesCorruptTail(t *testing.T) {
	p := newPath(t)
	seed(t, p, 2)
	ls := readLines(t, p)
	writeLines(t, p, append(ls, "not json"))
	mustFail(t, p, "line 3", "bad json")
	if Append(p, "t", "r", nil, t0) == nil {
		t.Fatal("append onto corrupt tail must fail")
	}
}

func TestLongLine(t *testing.T) {
	p := newPath(t)
	big := strings.Repeat("x", 1<<20)
	if err := Append(p, "t", "r", map[string]any{"big": big}, t0); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, "t", "r", nil, t0); err != nil {
		t.Fatal(err)
	}
	if n, err := Verify(p); n != 2 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestConcurrentAppend(t *testing.T) {
	p := newPath(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Append(p, "gate", "repo1", map[string]any{"i": i}, t0)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n, err := Verify(p); n != 20 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
