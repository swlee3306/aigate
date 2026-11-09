package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/ledger"
)

// statsRepo makes a repo and returns it with its ledger path.
func statsRepo(t *testing.T) (repo, ledgerPath, id string) {
	t.Helper()
	repo, _ = newRepo(t)
	id, _, err := gate.RepoID(repo)
	if err != nil {
		t.Fatal(err)
	}
	home, err := gate.Home("")
	if err != nil {
		t.Fatal(err)
	}
	return repo, gate.LedgerPath(home, id), id
}

func fixClock(t *testing.T, now time.Time) {
	t.Helper()
	orig := statsNow
	statsNow = func() time.Time { return now }
	t.Cleanup(func() { statsNow = orig })
}

func appendEv(t *testing.T, path, typ, repo string, payload map[string]any, at time.Time) {
	t.Helper()
	if err := ledger.Append(path, typ, repo, payload, at); err != nil {
		t.Fatal(err)
	}
}

// norm collapses the column padding of every line to single spaces.
func norm(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		b.WriteString(strings.Join(strings.Fields(l), " ") + "\n")
	}
	return b.String()
}

func runRes(cat string, rules ...string) map[string]any {
	if rules == nil {
		rules = []string{}
	}
	return map[string]any{"category": cat, "head": "h", "base": "b", "rules": rules}
}

// writeSynthetic writes events over three UTC days (Nov 3, 4, 5 2026) plus
// one old event (Oct 1) and one event of another repo. Refusals: a legacy one
// without needed_approval (counted), one with needed_approval true (counted)
// and one with false (recorded, not counted).
func writeSynthetic(t *testing.T, path, id string) {
	d3 := time.Date(2026, 11, 3, 23, 59, 0, 0, time.UTC)
	d4 := time.Date(2026, 11, 4, 0, 1, 0, 0, time.UTC)
	d5 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	old := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	appendEv(t, path, "run_result", id, runRes("ok"), old)
	appendEv(t, path, "run_result", id, runRes("ok"), d3)
	appendEv(t, path, "run_result", id, runRes("check_weakened", "protected_modified"), d3)
	appendEv(t, path, "approval_refused", id, map[string]any{"base": "b", "reason": "unsafe_bytes"}, d3)
	appendEv(t, path, "run_result", id, runRes("behavior_failure", "regression"), d4)
	appendEv(t, path, "run_result", id, runRes("verification_incomplete", "dirty_tree", "protected_modified"), d4)
	appendEv(t, path, "approval_granted", id, map[string]any{"base": "b", "hash": "0123456789abcdef", "files_changed": 1}, d4)
	appendEv(t, path, "approval_refused", id, map[string]any{"base": "b", "reason": "binary_unknown_type", "needed_approval": true}, d4)
	appendEv(t, path, "run_result", id, runRes("check_weakened", "test_missing"), d5)
	appendEv(t, path, "run_result", id, runRes("ok"), d5)
	appendEv(t, path, "approval_refused", id, map[string]any{"base": "b", "reason": "unsafe_bytes", "needed_approval": false}, d5)
	appendEv(t, path, "run_result", "otherrepo000", runRes("ok"), d5)
}

func TestStatsTable(t *testing.T) {
	repo, path, id := statsRepo(t)
	writeSynthetic(t, path, id)
	fixClock(t, time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC))
	c, out, e := run("stats", "--repo", repo)
	if c != 0 {
		t.Fatalf("stats: %d %q %q", c, out, e)
	}
	out = norm(out)
	for _, want := range []string{
		"날짜 runs ok behavior incomplete weakened 승인필요 승인수 안전검사 거부\n",
		"2026-11-03 2 1 0 0 1 1 0 1\n",
		"2026-11-04 2 0 1 1 0 1 1 1\n",
		"2026-11-05 2 1 0 0 1 0 0 0\n",
		"합계 6 2 1 1 2 2 1 2\n",
		"approvals_granted / runs = 1/6 = 0.17\n",
		"필요율 needed_approval / runs = 2/6 = 0.33\n",
		"서로 다른 head",
		"'no'",
		"규칙별 횟수\n규칙 횟수\nprotected_modified 2\ndirty_tree 1\nregression 1\ntest_missing 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "2026-10-01") {
		t.Errorf("event older than 14 days counted:\n%s", out)
	}
}

func TestStatsJSONAndDays(t *testing.T) {
	repo, path, id := statsRepo(t)
	writeSynthetic(t, path, id)
	fixClock(t, time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC))
	c, out, e := run("stats", "--repo", repo, "--days", "2", "--json")
	if c != 0 {
		t.Fatalf("stats: %d %q %q", c, out, e)
	}
	var got struct {
		Days   int    `json:"days"`
		From   string `json:"from"`
		To     string `json:"to"`
		PerDay []struct {
			Date string `json:"date"`
			statsRow
		} `json:"per_day"`
		Totals statsRow       `json:"totals"`
		Ratio  *float64       `json:"approval_ratio"`
		Needed *float64       `json:"approval_needed_ratio"`
		Rules  map[string]int `json:"rules"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if got.Days != 2 || got.From != "2026-11-04" || got.To != "2026-11-05" || len(got.PerDay) != 2 || got.PerDay[0].Date != "2026-11-04" {
		t.Fatalf("window: %+v", got)
	}
	want := statsRow{Runs: 4, OK: 1, Behavior: 1, Incomplete: 1, Weakened: 1, NeededApproval: 1, Granted: 1, Refused: 1}
	if got.Totals != want || got.Ratio == nil || *got.Ratio != 0.25 || got.Needed == nil || *got.Needed != 0.25 {
		t.Fatalf("totals %+v ratio %v needed %v, want %+v 0.25 0.25", got.Totals, got.Ratio, got.Needed, want)
	}
	wantRules := map[string]int{"regression": 1, "dirty_tree": 1, "protected_modified": 1, "test_missing": 1}
	if len(got.Rules) != len(wantRules) {
		t.Fatalf("rules %v, want %v", got.Rules, wantRules)
	}
	for k, v := range wantRules {
		if got.Rules[k] != v {
			t.Fatalf("rules %v, want %v", got.Rules, wantRules)
		}
	}
	// --days 1: only today
	c, out, _ = run("stats", "--repo", repo, "--days", "1")
	out = norm(out)
	if c != 0 || strings.Contains(out, "2026-11-04") || !strings.Contains(out, "합계 2 1 0 0 1 0 0 0\n") {
		t.Fatalf("--days 1: %d\n%s", c, out)
	}
	if c, _, _ := run("stats", "--repo", repo, "--days", "0"); c != 64 {
		t.Fatalf("--days 0 must be a usage error: %d", c)
	}
}

func TestStatsEmptyAndTampered(t *testing.T) {
	repo, path, id := statsRepo(t)
	fixClock(t, time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC))
	if c, out, e := run("stats", "--repo", repo); c != 0 || strings.TrimSpace(out) != "no events" {
		t.Fatalf("missing ledger: %d %q %q", c, out, e)
	}
	c, out, _ := run("stats", "--repo", repo, "--json")
	if c != 0 || !strings.Contains(out, `"approval_ratio": null`) || !strings.Contains(out, `"approval_needed_ratio": null`) || !strings.Contains(out, `"rules": {}`) {
		t.Fatalf("missing ledger json: %d %q", c, out)
	}
	writeSynthetic(t, path, id)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`"category":"behavior_failure"`), []byte(`"category":"ok"`), 1)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, out, e := run("stats", "--repo", repo)
	if c != 1 || !strings.Contains(out+e, "tamper") || strings.Contains(out, "합계") {
		t.Fatalf("tampered: %d %q %q", c, out, e)
	}
}

// approve writes approval_granted after a grant and approval_refused (reason
// code, no raw bytes) when the safety scan refuses.
func TestApproveWritesLedgerEvents(t *testing.T) {
	repo, base := newRepo(t)
	id, _, _ := gate.RepoID(repo)
	home, _ := gate.Home("")
	path := gate.LedgerPath(home, id)
	write(t, repo, "tests/t.txt", "one\ntwo\nthree\n")
	commit(t, repo, "edit")
	if c, out, e := approveTTY(t, repo, base, "yes\n"); c != 0 {
		t.Fatalf("approve: %d %q %q", c, out, e)
	}
	// a human "no" is not a refusal event
	write(t, repo, "tests/t.txt", "one\ntwo\nfour\n")
	commit(t, repo, "edit2")
	if c, _, _ := approveTTY(t, repo, base, "no\n"); c != 64 {
		t.Fatalf("no: %d", c)
	}
	write(t, repo, "tests/t.txt", "one\ntwo\nx\x1b[2K\n")
	commit(t, repo, "esc")
	if c, _, _ := approveTTY(t, repo, base, "yes\n"); c != 64 {
		t.Fatalf("unsafe approve: %d", c)
	}
	write(t, repo, "tests/t.txt", "one\ntwo\n\x00\n")
	commit(t, repo, "nul")
	if c, _, _ := approveTTY(t, repo, base, "yes\n"); c != 64 {
		t.Fatalf("nul approve: %d", c)
	}
	if n, err := ledger.Verify(path); err != nil || n != 3 {
		t.Fatalf("verify %d %v", n, err)
	}
	evs, _ := ledger.Events(path)
	if evs[0].Type != "approval_granted" || evs[0].Payload["base"] != base || len(evs[0].Payload["hash"].(string)) != 64 ||
		evs[0].Payload["files_changed"] != json.Number("1") {
		t.Fatalf("granted %+v", evs[0])
	}
	if evs[1].Type != "approval_refused" || evs[1].Payload["reason"] != "unsafe_bytes" || evs[1].Payload["base"] != base ||
		evs[2].Type != "approval_refused" || evs[2].Payload["reason"] != "text_became_binary" ||
		evs[1].Payload["needed_approval"] != true || evs[2].Payload["needed_approval"] != true {
		t.Fatalf("refused %+v %+v", evs[1], evs[2])
	}
	raw, _ := os.ReadFile(path)
	if bytes.ContainsAny(raw, "\x1b\x00") || bytes.Contains(raw, []byte(`\u001b`)) || bytes.Contains(raw, []byte("tests/t.txt")) {
		t.Fatalf("ledger holds raw bytes or paths: %q", raw)
	}
}

// A ledger failure after a successful grant says the approval WAS recorded
// (exit 64); a ledger failure on a refusal is a gate error too.
func TestApproveLedgerFailure(t *testing.T) {
	repo, base := newRepo(t)
	id, _, _ := gate.RepoID(repo)
	home, _ := gate.Home("")
	if err := os.MkdirAll(gate.LedgerPath(home, id), 0o700); err != nil { // a directory: append fails
		t.Fatal(err)
	}
	write(t, repo, "tests/t.txt", "one\ntwo\nthree\n")
	commit(t, repo, "edit")
	c, out, e := approveTTY(t, repo, base, "yes\n")
	if c != 64 || !strings.Contains(out, "approved.") || !strings.Contains(e, "approvals.json") || !strings.Contains(e, "ledger") {
		t.Fatalf("grant + ledger failure: %d %q %q", c, out, e)
	}
	i := strings.Index(out, "hash: ")
	if i < 0 || !(approval.Store{Path: gate.ApprovalsPath(home)}).Valid(id, base, out[i+6:i+70]) {
		t.Fatalf("the grant must stand in approvals.json: %q", out)
	}
	write(t, repo, "tests/t.txt", "one\ntwo\nx\x1b[2K\n")
	commit(t, repo, "esc")
	c, out, e = approveTTY(t, repo, base, "yes\n")
	if c != 64 || out != "" || !strings.Contains(e, "ledger") || !strings.Contains(e, `\x1b`) {
		t.Fatalf("refusal + ledger failure: %d %q %q", c, out, e)
	}
}

// A refusal where nothing needs approval (a new file under protected only,
// spec item [6]) is recorded with needed_approval false.
func TestApproveRefusalNeededApprovalFalse(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/new.dat2", "x\x00\n")
	commit(t, repo, "new binary under protected only")
	if c, _, _ := approveTTY(t, repo, base, "yes\n"); c != 64 {
		t.Fatalf("approve %d", c)
	}
	if p := lastRefusal(t, repo); p["needed_approval"] != false || p["reason"] != "binary_unknown_type" {
		t.Fatalf("payload %+v", p)
	}
}

// Real events: a protected edit (run needs approval), approve, run again.
// The clock is fixed to the last event's time and only the totals row is
// checked, so the test cannot fail across UTC midnight.
func TestStatsFromRealRuns(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "tests/t.txt", "one\ntwo\nthree\n")
	commit(t, repo, "edit")
	if c, _, _ := run("run", "--repo", repo, "--base", base); c != 3 {
		t.Fatalf("first run %d", c)
	}
	if c, _, _ := approveTTY(t, repo, base, "yes\n"); c != 0 {
		t.Fatalf("approve %d", c)
	}
	if c, _, _ := run("run", "--repo", repo, "--base", base); c != 0 {
		t.Fatalf("second run %d", c)
	}
	id, _, _ := gate.RepoID(repo)
	home, _ := gate.Home("")
	evs, err := ledger.Events(gate.LedgerPath(home, id))
	if err != nil || len(evs) != 3 {
		t.Fatalf("events %d %v", len(evs), err)
	}
	last, err := time.Parse(time.RFC3339, evs[len(evs)-1].TS)
	if err != nil {
		t.Fatal(err)
	}
	fixClock(t, last)
	c, out, e := run("stats", "--repo", repo)
	if out = norm(out); c != 0 || !strings.Contains(out, "합계 2 1 0 0 1 1 1 0\n") || !strings.Contains(out, "= 1/2 = 0.50") ||
		!strings.Contains(out, "규칙별 횟수\n규칙 횟수\nprotected_modified 1\n") {
		t.Fatalf("stats: %d %q %q", c, out, e)
	}
}
