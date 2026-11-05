package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/orch/api"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
)

func policyFile(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "policy.yml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

// parseApply parses args like run does and applies the policy.
func parseApply(t *testing.T, dir, policyPath string, args ...string) (api.Config, vmFlags, error) {
	t.Helper()
	fs := flag.NewFlagSet("orchd", flag.ContinueOnError)
	var cfg api.Config
	var repos stringList
	fs.StringVar(&cfg.DB, "db", filepath.Join(dir, "o.db"), "")
	fs.IntVar(&cfg.Concurrency, "concurrency", 1, "")
	fs.Var(&repos, "allow-repo", "")
	var vf vmFlags
	vf.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	cfg.AllowedRepos = repos
	_, err := applyPolicy(fs, policyPath, &cfg, &vf)
	return cfg, vf, err
}

const limitsPolicy = `version: 1
classes: {docs: {level: L1}}
allowlist: [push_branch]
limits: {agent_timeout: 7m, max_concurrent: 3, max_commits: 11, max_fetch_bytes: 2048}
repo_roots: [/srv/repos]
`

func TestPolicyProvidesLimitsAndRoots(t *testing.T) {
	dir := t.TempDir()
	cfg, vf, err := parseApply(t, dir, policyFile(t, dir, limitsPolicy, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != 3 || vf.AgentTimeout != 7*time.Minute || len(cfg.AllowedRepos) != 1 || cfg.AllowedRepos[0] != "/srv/repos" {
		t.Fatalf("%+v %+v", cfg, vf)
	}
}

func TestFlagPolicyConflictsRefuse(t *testing.T) {
	for name, args := range map[string][]string{
		"concurrency":   {"--concurrency", "2"},
		"agent timeout": {"--agent-timeout", "9m"},
		"repo roots":    {"--allow-repo", "/elsewhere"},
		"publish allow": {"--publish-allow", "notify"},
	} {
		dir := t.TempDir()
		_, _, err := parseApply(t, dir, policyFile(t, dir, limitsPolicy, 0o600), args...)
		if err == nil {
			t.Fatalf("%s: conflict accepted", name)
		}
	}
	// The same value on both sides is not a conflict.
	dir := t.TempDir()
	if _, _, err := parseApply(t, dir, policyFile(t, dir, limitsPolicy, 0o600),
		"--concurrency", "3", "--agent-timeout", "7m", "--allow-repo", "/srv/repos"); err != nil {
		t.Fatalf("equal values refused: %v", err)
	}
	// A flag the policy does not specify stays in force.
	dir = t.TempDir()
	cfg, _, err := parseApply(t, dir, policyFile(t, dir, "version: 1\n", 0o600), "--concurrency", "2", "--allow-repo", "/x")
	if err != nil || cfg.Concurrency != 2 || len(cfg.AllowedRepos) != 1 {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestRunRefusesBadPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		mode os.FileMode
	}{
		"loose mode":    {limitsPolicy, 0o666},
		"unknown key":   {"version: 1\nzzz: 1\n", 0o600},
		"floor allowed": {"version: 1\nallowlist: [delete_branch]\n", 0o600},
	} {
		dir := t.TempDir()
		p := policyFile(t, dir, tc.body, tc.mode)
		code := run([]string{"--db", filepath.Join(dir, "o.db"), "--socket", filepath.Join(dir, "s"), "--policy", p})
		if code == 0 {
			t.Fatalf("%s: orchd started", name)
		}
	}
}

func TestProviderBuildsWithPolicyAndPublisher(t *testing.T) {
	dir := t.TempDir()
	pf := policyFile(t, dir, limitsPolicy, 0o600)
	fs := flag.NewFlagSet("orchd", flag.ContinueOnError)
	var cfg api.Config
	cfg.DB = filepath.Join(dir, "db", "o.db")
	var vf vmFlags
	vf.register(fs)
	if err := fs.Parse(append(fullVMArgs(""), "--git-remote", filepath.Join(dir, "remote.git"))); err != nil {
		t.Fatal(err)
	}
	live, err := applyPolicy(fs, pf, &cfg, &vf)
	if err != nil {
		t.Fatal(err)
	}
	vf.live = live
	st, err := store.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := vf.provider(st, cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*steps.Provider); !ok {
		t.Fatalf("%T", p)
	}
	// Without a publishing flag a policy still builds (LocalOnlyPublisher plans nothing).
	var vf2 vmFlags
	fs2 := flag.NewFlagSet("orchd", flag.ContinueOnError)
	vf2.register(fs2)
	if err := fs2.Parse(fullVMArgs("")); err != nil {
		t.Fatal(err)
	}
	vf2.live = live
	if _, err := vf2.provider(st, cfg.DB); err != nil {
		t.Fatal(err)
	}
}
