package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func reqID(c byte) string { return strings.Repeat(string(c), 32) }

func openUsed(t *testing.T, dir string) *UsedRequests {
	t.Helper()
	u, err := OpenUsedRequests(dir, regOpts())
	if err != nil {
		t.Fatalf("OpenUsedRequests: %v", err)
	}
	return u
}

func TestUsedRequestsOnceAndPersistAcrossRestart(t *testing.T) {
	dir := stateDirFor(t)
	u := openUsed(t, dir)
	if err := u.Mark(reqID('a'), t0); err != nil {
		t.Fatal(err)
	}
	if err := u.Mark(reqID('a'), t0.Add(time.Second)); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("second use: %v", err)
	}
	if err := u.Mark(reqID('b'), t0); err != nil {
		t.Fatalf("other id: %v", err)
	}
	// restart: still remembered
	u2 := openUsed(t, dir)
	if err := u2.Mark(reqID('a'), t0.Add(time.Minute)); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("after restart: %v", err)
	}
	fi, _ := os.Stat(filepath.Join(dir, UsedRequestsFileName))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 32), strings.Repeat("g", 32), reqID('a') + "0"} {
		if err := u2.Mark(bad, t0); !errors.Is(err, ErrBadRequestID) {
			t.Fatalf("id %q: %v", bad, err)
		}
	}
}

func TestUsedRequestsExpireAfterOneHour(t *testing.T) {
	dir := stateDirFor(t)
	u := openUsed(t, dir)
	u.Mark(reqID('a'), t0)
	if err := u.Mark(reqID('a'), t0.Add(59*time.Minute+59*time.Second)); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("just before an hour: %v", err)
	}
	if err := u.Mark(reqID('a'), t0.Add(time.Hour)); err != nil {
		t.Fatalf("after an hour: %v", err)
	}
	// expired entries are dropped from the file
	u.Mark(reqID('c'), t0.Add(3*time.Hour))
	b, _ := os.ReadFile(filepath.Join(dir, UsedRequestsFileName))
	if strings.Contains(string(b), reqID('a')) || !strings.Contains(string(b), reqID('c')) {
		t.Fatalf("file not pruned: %s", b)
	}
}

func TestUsedRequestsBrokenFileRefusesWrites(t *testing.T) {
	good := `{"version":1,"entries":{"` + reqID('a') + `":1}}`
	cases := map[string]struct {
		content string
		mode    os.FileMode
	}{
		"junk":        {"{{{", 0o600},
		"unknown key": {`{"version":1,"entries":{},"x":1}`, 0o600},
		"bad version": {`{"version":2,"entries":{}}`, 0o600},
		"bad id":      {`{"version":1,"entries":{"nothex":5}}`, 0o600},
		"bad time":    {`{"version":1,"entries":{"` + reqID('a') + `":0}}`, 0o600},
		"trailing":    {good + "{}", 0o600},
		"wrong mode":  {good, 0o644},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := stateDirFor(t)
			os.MkdirAll(dir, 0o700)
			p := filepath.Join(dir, UsedRequestsFileName)
			os.WriteFile(p, []byte(c.content), c.mode)
			os.Chmod(p, c.mode)
			u, err := OpenUsedRequests(dir, regOpts())
			if !errors.Is(err, ErrUsedRequestsBroken) || u == nil {
				t.Fatalf("err %v", err)
			}
			if err := u.Mark(reqID('z'), t0); !errors.Is(err, ErrUsedRequestsBroken) {
				t.Fatalf("Mark on broken store: %v", err)
			}
			if b, _ := os.ReadFile(p); string(b) != c.content {
				t.Fatal("broken file was rewritten")
			}
		})
	}
}

func TestUsedRequestsFailedWriteRecordsNothing(t *testing.T) {
	dir := stateDirFor(t)
	u := openUsed(t, dir)
	u.opts.failAt = func(string) error { return errors.New("injected") }
	if err := u.Mark(reqID('a'), t0); err == nil {
		t.Fatal("Mark succeeded although the write failed")
	}
	u.opts.failAt = nil
	if err := u.Mark(reqID('a'), t0); err != nil {
		t.Fatalf("id burned by a failed write: %v", err)
	}
}
