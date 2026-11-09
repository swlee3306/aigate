package main

import (
	"context"
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

func TestCloudSupplierMapsAndHidesText(t *testing.T) {
	if cloudSupplier(nil) != nil {
		t.Fatal("no VM must give a nil supplier (no cloud block in /ops)")
	}
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	ok := cloudSupplier(func(context.Context) vm.CloudInfo {
		return vm.CloudInfo{OK: true, Gigabytes: vm.CloudLimit{Limit: 300, InUse: 180, Reserved: 30},
			Volumes: vm.CloudLimit{Limit: -1, InUse: 7, Unlimited: true}, VMName: "test-vm", VMStatus: "ACTIVE",
			RestoreVolumes: 2, CheckedAt: at,
			History: []vm.RestoreRecord{{At: at, Duration: 53 * time.Second, Err: "secret host text"}}}
	})(context.Background())
	if !ok.OK || ok.Quota.Gigabytes.Limit != 300 || !ok.Quota.Volumes.Unlimited || ok.TestVM.Status != "ACTIVE" ||
		ok.RestoreVolumes != 2 || len(ok.History) != 1 || ok.History[0].OK || ok.History[0].Seconds != 53 {
		t.Errorf("%+v", ok)
	}
	for in, want := range map[string]string{vm.CloudTimeout: "cloud_timeout", vm.CloudUnavailable: "cloud_unavailable", "weird: host x": "cloud_unavailable"} {
		v := cloudSupplier(func(context.Context) vm.CloudInfo { return vm.CloudInfo{Code: in} })(context.Background())
		if v.OK || v.Code != want || v.Quota != nil || v.TestVM != nil {
			t.Errorf("%q -> %+v", in, v)
		}
	}
}
