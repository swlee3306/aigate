package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/policy/autonomy"
)

func localRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestProposeAndPolicyCommands(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	for _, a := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "i"}} {
		c := exec.Command("git", append([]string{"-C", repo}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	id, _, err := gate.RepoID(repo)
	if err != nil {
		t.Fatal(err)
	}
	led := gate.LedgerPath(home, id)
	for i := 0; i < 12; i++ {
		if err := ledger.Append(led, "run_result", id, map[string]any{"category": "ok", "task_class": "docs", "rules": []any{}},
			time.Now().UTC().Add(-time.Duration(20-i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	pf := filepath.Join(dir, "policy.yml")
	body := "version: 1\nclasses:\n  docs: {level: L0}\n"
	os.WriteFile(pf, []byte(body), 0o600)
	before, _ := os.ReadFile(pf)
	fi0, _ := os.Stat(pf)

	code, out, errs := localRun(t, "--gate-home", home, "propose", "--repo", repo, "--policy", pf, "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	var ps []map[string]any
	if err := json.Unmarshal([]byte(out), &ps); err != nil || len(ps) != 1 || ps[0]["class"] != "docs" || ps[0]["eligible"] != true || ps[0]["to"] != "L1" {
		t.Fatalf("%v %s", err, out)
	}
	code, out, _ = localRun(t, "--gate-home", home, "propose", "--repo", repo, "--days", "7")
	if code != 0 || !strings.Contains(out, "docs") || !strings.Contains(out, "정책 파일은 바뀌지 않았습니다") {
		t.Fatalf("%d %s", code, out)
	}
	after, _ := os.ReadFile(pf)
	fi1, _ := os.Stat(pf)
	if !bytes.Equal(before, after) || !fi0.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("propose changed the policy file")
	}
	if code, _, _ := localRun(t, "--gate-home", home, "propose", "--repo", repo, "--days", "0"); code == 0 {
		t.Fatal("--days 0 accepted")
	}

	// policy show with a demotion
	os.WriteFile(pf, []byte("version: 1\nclasses:\n  docs: {level: L2}\n"), 0o600)
	pol, _ := policy.Load(pf)
	if _, _, err := autonomy.Demote(pol, autonomy.Incident{Class: "docs", Task: 5, Reason: "test"}, led, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	code, out, errs = localRun(t, "policy", "show", "--policy", pf, "--json")
	if code != 0 || !strings.Contains(out, `"effective_level": "L1"`) || !strings.Contains(out, pol.Hash()) {
		t.Fatalf("%d %s %s", code, out, errs)
	}

	// clear-demotion: logs an event, the class is back at the file's level
	if code, _, errs := localRun(t, "--gate-home", home, "policy", "clear-demotion", "--policy", pf, "docs"); code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	pol, _ = policy.Load(pf)
	if l, _ := autonomy.Effective(pol, "docs"); l != autonomy.L2 {
		t.Fatalf("effective %s", l)
	}
	evs, err := ledger.Events(policy.OrchLedgerPath(home))
	if err != nil || len(evs) != 1 || evs[0].Type != "autonomy_clear_demotion" {
		t.Fatalf("%v %+v", err, evs)
	}
	// Nothing to clear the second time; missing class and missing policy are usage errors.
	if code, _, _ := localRun(t, "--gate-home", home, "policy", "clear-demotion", "--policy", pf, "docs"); code == 0 {
		t.Fatal("cleared twice")
	}
	if code, _, _ := localRun(t, "policy", "clear-demotion", "--policy", pf); code == 0 {
		t.Fatal("no class accepted")
	}
	if code, _, _ := localRun(t, "policy", "show"); code == 0 {
		t.Fatal("show without a policy accepted")
	}
	if code, _, _ := localRun(t, "policy"); code != 2 {
		t.Fatal("bare policy")
	}
}
