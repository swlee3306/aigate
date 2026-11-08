package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/orch/vm"
)

func TestCountJSON(t *testing.T) {
	if n := countJSON(filepath.Join(t.TempDir(), "missing")); n != 0 {
		t.Errorf("missing dir: %d", n)
	}
	d := t.TempDir()
	for _, f := range []string{"a.json", "b.json", "c.txt"} {
		os.WriteFile(filepath.Join(d, f), []byte("{}"), 0o600)
	}
	os.Mkdir(filepath.Join(d, "dir.json"), 0o700)
	if n := countJSON(d); n != 2 {
		t.Errorf("want 2, got %d", n)
	}
}

func TestOpsSupplier(t *testing.T) {
	at := time.Unix(1000, 0)
	rec := vm.RestoreRecord{At: at, Duration: 3 * time.Second, Phases: []vm.Phase{{Name: "boot", Dur: time.Second}, {Name: "ssh", Dur: 2 * time.Second, Err: "x"}}}
	v := opsSupplier("rev", at, func() string { return "h" }, func() (vm.RestoreRecord, bool) { return rec, true }, t.TempDir())()
	if v.Build != "rev" || v.PolicyHash != "h" || v.LastRestore == nil || v.LastRestore.Seconds != 3 ||
		len(v.LastRestore.Phases) != 2 || v.LastRestore.Phases[1].OK || !v.LastRestore.OK {
		t.Errorf("got %+v", v)
	}
	if v := opsSupplier("rev", at, nil, nil, "")(); v.LastRestore != nil || v.PolicyHash != "" {
		t.Errorf("no providers: %+v", v)
	}
	if v := policySupplier(nil, true)(); v.Hash != "" {
		t.Errorf("nil live: %+v", v)
	}
}
