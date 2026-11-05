package autonomy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/rules"
)

var (
	levels   = []Level{L0, L1, L2, L3}
	verdicts = []rules.Category{rules.CategoryOK, rules.CategoryWeakened, rules.CategoryBehavior, rules.CategoryIncomplete}
	kinds    = []string{"push_branch", "draft_pr", "notify", "post_comment", "delete_branch", "merge", "force_push", "deploy"}
)

// want is the table of the package documentation, written out independently.
func want(l Level, v rules.Category, a string) Decision {
	switch a {
	case "delete_branch", "merge", "force_push", "deploy":
		return Deny
	}
	switch v {
	case rules.CategoryBehavior, rules.CategoryIncomplete:
		return Deny
	case rules.CategoryWeakened:
		return Park
	}
	if l == L0 {
		return Park
	}
	switch a {
	case "push_branch", "draft_pr", "notify":
		return Allow
	}
	return Park
}

func TestDecideMatrix(t *testing.T) {
	for _, l := range levels {
		for _, v := range verdicts {
			for _, a := range kinds {
				if got := Decide(l, v, Action(a)); got != want(l, v, a) {
					t.Errorf("Decide(%s, %s, %s) = %s, want %s", l, v, a, got, want(l, v, a))
				}
			}
		}
	}
}

func TestFloorFirstAtEveryLevel(t *testing.T) {
	for _, l := range levels {
		for _, v := range verdicts {
			for _, a := range []string{"delete_branch", "DeleteBranch", "merge_request_merge", "restart_service", "grant-permission", "rotate_token", "force_push"} {
				if got := Decide(l, v, Action(a)); got != Deny {
					t.Errorf("Decide(%s,%s,%s) = %s; the floor must deny", l, v, a, got)
				}
			}
		}
	}
	if Decide(L3, rules.CategoryOK, "") != Deny {
		t.Error("empty action must be denied")
	}
}

func TestNonOKNeverAllowed(t *testing.T) {
	for _, l := range []Level{L0, L1, L2, L3, Level(-1), Level(7)} {
		for _, v := range []rules.Category{rules.CategoryWeakened, rules.CategoryBehavior, rules.CategoryIncomplete, "", "bogus"} {
			for _, a := range kinds {
				if Decide(l, v, Action(a)) == Allow {
					t.Errorf("Decide(%s,%q,%s) allowed", l, v, a)
				}
			}
		}
	}
	if Decide(Level(9), rules.CategoryOK, "push_branch") != Deny {
		t.Error("invalid level must deny")
	}
}

func TestNotifyRules(t *testing.T) {
	if NotifyOnPark(L0) || NotifyOnPark(L1) || !NotifyOnPark(L2) || NotifyOnPark(L3) {
		t.Error("NotifyOnPark")
	}
	if NotifyAfterPublish(L0) || NotifyAfterPublish(L1) || NotifyAfterPublish(L2) || !NotifyAfterPublish(L3) {
		t.Error("NotifyAfterPublish")
	}
}

func loadPolicy(t *testing.T, dir, body string) policy.Policy {
	t.Helper()
	p := filepath.Join(dir, "policy.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pol, err := policy.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

const pol2 = "version: 1\nclasses:\n  docs: {level: L2}\n  bug: {level: L1}\n  safe: {level: L3}\n"

func TestDemoteLowersExactlyOneAndRecords(t *testing.T) {
	dir := t.TempDir()
	pol := loadPolicy(t, dir, pol2)
	led := filepath.Join(dir, "led.jsonl")
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	from, to, err := Demote(pol, Incident{Class: "safe", Task: 42, Reason: "verdict weakened after publish"}, led, "repo1", now)
	if err != nil || from != L3 || to != L2 {
		t.Fatalf("%s -> %s, %v", from, to, err)
	}
	if lvl, _ := Effective(pol, "safe"); lvl != L2 {
		t.Fatalf("effective %s", lvl)
	}
	if lvl, _ := Effective(pol, "docs"); lvl != L2 {
		t.Fatalf("other class changed: %s", lvl)
	}
	fi, err := os.Stat(pol.StatePath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", fi, err)
	}
	evs, err := ledger.Events(led)
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %v", evs, err)
	}
	e := evs[0]
	if e.Type != "autonomy_demote" || e.Repo != "repo1" || e.Payload["class"] != "safe" || e.Payload["from"] != "L3" ||
		e.Payload["to"] != "L2" || e.Payload["reason"] != "verdict weakened after publish" || fmt.Sprint(e.Payload["task"]) != "42" {
		t.Fatalf("event %+v", e)
	}
	if _, err := ledger.Verify(led); err != nil {
		t.Fatal(err)
	}
	// Second incident: one more step down, from the effective level.
	if _, to, _ := Demote(pol, Incident{Class: "safe", Task: 43, Reason: "again"}, led, "repo1", now); to != L1 {
		t.Fatalf("second demotion to %s", to)
	}
	Demote(pol, Incident{Class: "safe", Task: 44, Reason: "again"}, led, "repo1", now)
	// L0 is the floor of the ladder: nothing more to lower, no event.
	n0, _ := ledger.Events(led)
	f, to, err := Demote(pol, Incident{Class: "safe", Task: 45, Reason: "again"}, led, "repo1", now)
	if err != nil || f != L0 || to != L0 {
		t.Fatalf("%s %s %v", f, to, err)
	}
	if n1, _ := ledger.Events(led); len(n1) != len(n0) {
		t.Fatal("event written when nothing was lowered")
	}
	if _, _, err := Demote(pol, Incident{Class: "", Reason: "x"}, led, "r", now); err == nil {
		t.Fatal("empty class accepted")
	}
}

func TestEffectiveIsMin(t *testing.T) {
	dir := t.TempDir()
	pol := loadPolicy(t, dir, pol2)
	led := filepath.Join(dir, "led.jsonl")
	Demote(pol, Incident{Class: "docs", Task: 1, Reason: "x"}, led, "r", time.Now()) // L2 -> L1
	// The human lowers the policy below the state: the policy level wins.
	lower := loadPolicy(t, dir, "version: 1\nclasses:\n  docs: {level: L0}\n")
	if l, _ := Effective(lower, "docs"); l != L0 {
		t.Fatalf("min(L0, L1) = %s", l)
	}
	// The human raises the policy above the state: the demotion still holds.
	higher := loadPolicy(t, dir, "version: 1\nclasses:\n  docs: {level: L3}\n")
	if l, _ := Effective(higher, "docs"); l != L1 {
		t.Fatalf("min(L3, L1) = %s", l)
	}
}

func TestDemotionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	pol := loadPolicy(t, dir, pol2)
	Demote(pol, Incident{Class: "bug", Task: 1, Reason: "x"}, filepath.Join(dir, "l.jsonl"), "r", time.Now())
	// "Restart": a fresh policy load and a fresh read of the state file.
	again := loadPolicy(t, dir, pol2)
	if l, err := Effective(again, "bug"); err != nil || l != L0 {
		t.Fatalf("after restart: %s %v", l, err)
	}
	dem, err := Demoted(again)
	if err != nil || dem["bug"].Level != "L0" || dem["bug"].Task != 1 {
		t.Fatalf("%+v %v", dem, err)
	}
}

func TestClearDemotionIsExplicitAndLogged(t *testing.T) {
	dir := t.TempDir()
	pol := loadPolicy(t, dir, pol2)
	led := filepath.Join(dir, "l.jsonl")
	if err := ClearDemotion(pol, "docs", led, "orchd", time.Now()); err != ErrNoDemotion {
		t.Fatalf("clear without demotion: %v", err)
	}
	Demote(pol, Incident{Class: "docs", Task: 1, Reason: "x"}, led, "r", time.Now())
	if err := ClearDemotion(pol, "docs", led, "orchd", time.Now()); err != nil {
		t.Fatal(err)
	}
	if l, _ := Effective(pol, "docs"); l != L2 {
		t.Fatalf("after clear %s", l)
	}
	evs, _ := ledger.Events(led)
	if len(evs) != 2 || evs[1].Type != "autonomy_clear_demotion" || evs[1].Payload["class"] != "docs" {
		t.Fatalf("%+v", evs)
	}
}

func TestCorruptStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	pol := loadPolicy(t, dir, pol2)
	os.WriteFile(pol.StatePath(), []byte("{not json"), 0o600)
	if l, err := Effective(pol, "docs"); err == nil || l != L0 {
		t.Fatalf("%s %v", l, err)
	}
	if _, _, err := Demote(pol, Incident{Class: "docs", Reason: "x"}, filepath.Join(dir, "l"), "r", time.Now()); err == nil {
		t.Fatal("demote over a corrupt state file must report an error")
	}
}
