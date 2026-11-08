package api

import (
	"encoding/json"
	"net/http"
	"strings"
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
