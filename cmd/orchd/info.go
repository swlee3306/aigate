package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"aigate/internal/orch/api"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/vm"
)

// policySupplier builds the /policy view from the live policy; without a
// policy file (live == nil) it returns the zero view (Hash "").
func policySupplier(live *policy.Live, baseCache bool) func() api.PolicyView {
	return func() api.PolicyView {
		if live == nil {
			return api.PolicyView{}
		}
		p := live.Current()
		classes := map[string]string{}
		for c := range p.Classes {
			classes[c] = p.LevelFor(c).String()
		}
		return api.PolicyView{Hash: p.Hash(), Classes: classes,
			Allowlist: append([]string(nil), p.Allowlist...), Floor: policy.FloorWords(), BaseCache: baseCache}
	}
}

// opsSupplier builds the /ops view. restore reports the test VM's last
// restore (nil without a VM); cacheDir is the base-cache directory ("" none).
func opsSupplier(build string, started time.Time, policyHash func() string,
	restore func() (vm.RestoreRecord, bool), cacheDir string) func() api.OpsView {
	return func() api.OpsView {
		v := api.OpsView{Build: build, Started: started, BaseCacheEntries: countJSON(cacheDir)}
		if policyHash != nil {
			v.PolicyHash = policyHash()
		}
		if restore != nil {
			if rec, ok := restore(); ok {
				rv := restoreView(rec)
				v.LastRestore = rv
			}
		}
		return v
	}
}

// restoreView is the API shape of a restore record (no error text, no ids).
func restoreView(rec vm.RestoreRecord) *api.RestoreView {
	rv := &api.RestoreView{Seconds: rec.Duration.Seconds(), At: rec.At, OK: rec.Err == "",
		Phases: []api.RestorePhase{}}
	for _, ph := range rec.Phases {
		rv.Phases = append(rv.Phases, api.RestorePhase{Name: ph.Name, Seconds: ph.Dur.Seconds(), OK: ph.Err == ""})
	}
	return rv
}

// cloudSupplier adapts the (cached, single-flight, time-bounded)
// TestVM.CloudStatus to the API's CloudView. status is nil without a VM.
func cloudSupplier(status func(context.Context) vm.CloudInfo) func(context.Context) api.CloudView {
	if status == nil {
		return nil
	}
	return func(ctx context.Context) api.CloudView {
		in := status(ctx)
		if !in.OK {
			code := in.Code
			if code != api.CloudTimeout {
				code = api.CloudUnavailable
			}
			return api.CloudView{Code: code, History: []api.RestoreView{}, CheckedAt: in.CheckedAt}
		}
		lim := func(l vm.CloudLimit) api.CloudLimit {
			return api.CloudLimit{Limit: l.Limit, InUse: l.InUse, Reserved: l.Reserved, Unlimited: l.Unlimited}
		}
		out := api.CloudView{OK: true, RestoreVolumes: in.RestoreVolumes, CheckedAt: in.CheckedAt,
			Quota:   &api.QuotaView{Gigabytes: lim(in.Gigabytes), Volumes: lim(in.Volumes)},
			TestVM:  &api.VMState{Name: in.VMName, Status: in.VMStatus},
			History: []api.RestoreView{}}
		for _, rec := range in.History {
			out.History = append(out.History, *restoreView(rec))
		}
		return out
	}
}

// countJSON counts the regular *.json files in dir (0 when it does not exist).
func countJSON(dir string) int {
	if dir == "" {
		return 0
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range es {
		if e.Type().IsRegular() && filepath.Ext(e.Name()) == ".json" {
			n++
		}
	}
	return n
}
