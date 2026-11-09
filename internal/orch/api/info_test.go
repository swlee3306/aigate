package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProjectsAggregatedFromTasks(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	// Second repo under the same allowed root.
	repo2 := e.root + "/other"
	git(t, e.root, "init", "-q", repo2)
	git(t, repo2, "commit", "-q", "--allow-empty", "-m", "init")
	for _, s := range []struct{ repo, class string }{
		{e.repo, "버그수정"}, {repo2, "기능"}, {e.repo, "기능"},
	} {
		if code, b := e.submit(t, s.repo, "HEAD", "p", s.class, "claude"); code != http.StatusCreated {
			t.Fatalf("submit: %d %s", code, b)
		}
	}
	code, b := e.do(t, "GET", "/api/v1/projects", "")
	if code != 200 {
		t.Fatalf("code %d %s", code, b)
	}
	var got []struct {
		Repo     string `json:"repo"`
		Class    string `json:"class"`
		Tasks    int    `json:"tasks"`
		LastTask int64  `json:"last_task"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 projects, got %s", b)
	}
	// newest repo first: e.repo owns the newest task (#3, class 기능)
	if got[0].Repo != e.repo || got[0].Tasks != 2 || got[0].LastTask != 3 || got[0].Class != "기능" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Repo != repo2 || got[1].Tasks != 1 || got[1].LastTask != 2 || got[1].Class != "기능" {
		t.Errorf("second = %+v", got[1])
	}
}

func TestProjectsEmptyIsArray(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	_, b := e.do(t, "GET", "/api/v1/projects", "")
	if strings.TrimSpace(string(b)) != "[]" {
		t.Errorf("got %s", b)
	}
}

func TestPolicyEndpoint(t *testing.T) {
	want := PolicyView{Hash: "abc123", Classes: map[string]string{"버그수정": "L2"},
		Allowlist: []string{"git_push"}, Floor: []string{"delete", "merge"}, BaseCache: true}
	e := newEnv(t, noSteps{}, 1, func(c *Config) { c.Policy = func() PolicyView { return want } })
	code, b := e.do(t, "GET", "/api/v1/policy", "")
	if code != 200 {
		t.Fatalf("code %d %s", code, b)
	}
	var got PolicyView
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Hash != "abc123" || got.Classes["버그수정"] != "L2" || len(got.Allowlist) != 1 ||
		len(got.Floor) != 2 || !got.BaseCache {
		t.Errorf("got %s", b)
	}
}

func TestOpsEndpoint(t *testing.T) {
	started := time.Date(2026, 11, 9, 1, 2, 3, 0, time.UTC)
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.Ops = func() OpsView {
			return OpsView{Build: "rev1", Started: started, BaseCacheEntries: 4, PolicyHash: "h",
				LastRestore: &RestoreView{Seconds: 12.5, At: started, OK: true,
					Phases: []RestorePhase{{Name: "boot", Seconds: 10, OK: true}}}}
		}
	})
	code, b := e.do(t, "GET", "/api/v1/ops", "")
	if code != 200 {
		t.Fatalf("code %d %s", code, b)
	}
	var got OpsView
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Build != "rev1" || !got.Started.Equal(started) || got.BaseCacheEntries != 4 ||
		got.LastRestore == nil || got.LastRestore.Seconds != 12.5 || len(got.LastRestore.Phases) != 1 {
		t.Errorf("got %s", b)
	}
}

func TestOpsNilProvidersGiveEmptyObject(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	for _, p := range []string{"/api/v1/ops", "/api/v1/policy"} {
		code, b := e.do(t, "GET", p, "")
		if code != 200 || strings.TrimSpace(string(b)) != "{}" {
			t.Errorf("%s: %d %q", p, code, b)
		}
	}
}

func TestOpsNoSecrets(t *testing.T) {
	// The suppliers are handed only view types; marshal both views and make
	// sure none of the secret-looking config values could appear.
	secrets := []string{"/etc/aigate/token", "/home/orch/.ssh/id_ed25519", "https://oauth2:glpat-SECRET@gitlab.example", "signers.json"}
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.DB = secrets[0]
		c.GateHome = secrets[1]
		c.ReportDir = secrets[2]
		c.Policy = func() PolicyView { return PolicyView{Hash: "h"} }
		c.Ops = func() OpsView { return OpsView{Build: "rev1", Started: time.Now()} }
	})
	for _, p := range []string{"/api/v1/ops", "/api/v1/policy"} {
		_, b := e.do(t, "GET", p, "")
		for _, s := range secrets {
			if strings.Contains(string(b), s) || strings.Contains(string(b), "glpat") {
				t.Errorf("%s leaks %q: %s", p, s, b)
			}
		}
	}
}

func okCloud() CloudView {
	return CloudView{OK: true,
		Quota:  &QuotaView{Gigabytes: CloudLimit{Limit: 300, InUse: 180, Reserved: 30}, Volumes: CloudLimit{Limit: -1, InUse: 7, Unlimited: true}},
		TestVM: &VMState{Name: "test-vm", Status: "ACTIVE"}, RestoreVolumes: 2,
		History: []RestoreView{}, CheckedAt: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}
}

func TestOpsCloudBlock(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.Ops = func() OpsView { return OpsView{Build: "rev1", Started: time.Now()} }
		c.CloudStatus = func(context.Context) CloudView { return okCloud() }
	})
	code, b := e.do(t, "GET", "/api/v1/ops", "")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var raw struct {
		Cloud map[string]json.RawMessage `json:"cloud"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ok", "code", "quota", "test_vm", "restore_volumes", "history", "checked_at"} {
		if _, ok := raw.Cloud[k]; !ok {
			t.Errorf("cloud.%s missing: %s", k, b)
		}
	}
}

func TestOpsWithoutCloudSupplierStillWorks(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.Ops = func() OpsView { return OpsView{Build: "rev1", Started: time.Now(), BaseCacheEntries: 3} }
	})
	code, b := e.do(t, "GET", "/api/v1/ops", "")
	if code != 200 || strings.Contains(string(b), `"cloud"`) || !strings.Contains(string(b), `"build":"rev1"`) {
		t.Fatalf("%d %s", code, b)
	}
}

func TestCloudStatusFailureIsPartial(t *testing.T) {
	const secret = "ssh: connect to host jump.internal.example port 22: Connection refused"
	for _, code := range []string{CloudUnavailable, CloudTimeout} {
		e := newEnv(t, noSteps{}, 1, func(c *Config) {
			c.Ops = func() OpsView {
				return OpsView{Build: "rev1", Started: time.Now(), PolicyHash: "ph", BaseCacheEntries: 4,
					LastRestore: &RestoreView{Seconds: 12.5, OK: true, Phases: []RestorePhase{}}}
			}
			c.CloudStatus = func(context.Context) CloudView { return CloudView{Code: code} }
		})
		status, b := e.do(t, "GET", "/api/v1/ops", "")
		if status != 200 {
			t.Fatalf("%s: %d %s", code, status, b)
		}
		var got OpsView
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.Build != "rev1" || got.PolicyHash != "ph" || got.BaseCacheEntries != 4 || got.LastRestore == nil {
			t.Errorf("%s: rest of /ops changed: %s", code, b)
		}
		if got.Cloud == nil || got.Cloud.OK || got.Cloud.Code != code || got.Cloud.Quota != nil || got.Cloud.TestVM != nil {
			t.Errorf("%s: cloud %+v", code, got.Cloud)
		}
		if strings.Contains(string(b), secret) || strings.Contains(string(b), "jump.internal") {
			t.Errorf("error text leaked: %s", b)
		}
	}
}

func TestCloudStatusPanicIsPartial(t *testing.T) {
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.Ops = func() OpsView { return OpsView{Build: "rev1", Started: time.Now()} }
		c.CloudStatus = func(context.Context) CloudView { panic("boom: secret-host") }
	})
	code, b := e.do(t, "GET", "/api/v1/ops", "")
	if code != 200 || !strings.Contains(string(b), CloudUnavailable) || strings.Contains(string(b), "secret-host") {
		t.Fatalf("%d %s", code, b)
	}
}

func TestOpsCloudRequiresAuth(t *testing.T) {
	var calls atomic.Int32
	f := newLoginFixtureWith(t, func(c *Config) {
		c.Ops = func() OpsView { return OpsView{Build: "rev1", Started: time.Now()} }
		c.CloudStatus = func(context.Context) CloudView { calls.Add(1); return okCloud() }
	})
	code, _, b := httpDo(t, f.srv, "GET", "/api/v1/ops", "", nil)
	if code != http.StatusUnauthorized || strings.Contains(string(b), "quota") {
		t.Fatalf("%d %s", code, b)
	}
	if calls.Load() != 0 {
		t.Errorf("the cloud supplier ran for an unauthenticated request")
	}
}

func TestOpsReturnsWithinBudgetWhenCloudHangs(t *testing.T) {
	old := cloudBudget
	cloudBudget = 100 * time.Millisecond
	t.Cleanup(func() { cloudBudget = old })
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var ready atomic.Bool
	// A detached refresh like vm.TestVM.CloudStatus: shared, outlives callers.
	var mu sync.Mutex
	var res *CloudView
	started := false
	supplier := func(ctx context.Context) CloudView {
		mu.Lock()
		if res != nil {
			v := *res
			mu.Unlock()
			return v
		}
		if !started {
			started = true
			go func() {
				<-release
				v := okCloud()
				mu.Lock()
				res = &v
				mu.Unlock()
				ready.Store(true)
			}()
		}
		mu.Unlock()
		select {
		case <-ctx.Done():
			return CloudView{Code: CloudTimeout}
		case <-time.After(5 * time.Second):
			return CloudView{Code: CloudUnavailable}
		}
	}
	e := newEnv(t, noSteps{}, 1, func(c *Config) {
		c.Ops = func() OpsView {
			return OpsView{Build: "rev1", Started: time.Now(), PolicyHash: "ph", BaseCacheEntries: 4}
		}
		c.CloudStatus = supplier
	})
	start := time.Now()
	code, b := e.do(t, "GET", "/api/v1/ops", "")
	if code != 200 || time.Since(start) > 2*time.Second {
		t.Fatalf("%d after %v: %s", code, time.Since(start), b)
	}
	var got OpsView
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Build != "rev1" || got.PolicyHash != "ph" || got.BaseCacheEntries != 4 ||
		got.Cloud == nil || got.Cloud.OK || got.Cloud.Code != CloudTimeout {
		t.Fatalf("%s", b)
	}
	once.Do(func() { close(release) })
	for i := 0; i < 100 && !ready.Load(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	_, b = e.do(t, "GET", "/api/v1/ops", "")
	var later OpsView
	_ = json.Unmarshal(b, &later)
	if later.Cloud == nil || !later.Cloud.OK || later.Cloud.Quota == nil {
		t.Errorf("later /ops: %s", b)
	}
}
