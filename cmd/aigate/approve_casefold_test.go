package main

import (
	"bytes"
	"strings"
	"testing"

	"aigate/internal/gate"
	"aigate/internal/gittest"
	"aigate/internal/gitx"
)

// FIX WAVE 7 (B): approve on a head whose tree has a case/Unicode-folding
// collision refuses like its other safety refusals: exit 64, nothing on
// stdout, escaped paths on stderr, ledger approval_refused with reason
// path_case_collision, nothing granted. Such a head is not checked out as
// committed, so no diff shown for it could be what the checks run on.
func TestApproveRefusesPathCaseCollision(t *testing.T) {
	repo, base := newRepo(t) // base has tests/t.txt
	head := gittest.Plumb(t, repo, base, map[string]string{"tests/T.txt": "weak\n", "x/\x1b[2JA": "1\n", "x/\x1b[2Ja": "2\n"})
	orig := requireTTY
	requireTTY = func() error { return nil }
	t.Cleanup(func() { requireTTY = orig })
	var o, e bytes.Buffer
	code := Main([]string{"approve", "--repo", repo, "--base", base, "--head", head}, strings.NewReader("yes\n"), &o, &e)
	errOut := e.String()
	if code != 64 || o.String() != "" || rawUnsafe(errOut) {
		t.Fatalf("want refusal: %d stdout=%q stderr=%q", code, o.String(), errOut)
	}
	for _, s := range []string{"path_case_collision", "tests/T.txt", "tests/t.txt", `x/\x1b[2JA`, "대소문자", "이름을 고친"} {
		if !strings.Contains(errOut, s) {
			t.Errorf("stderr lacks %q: %q", s, errOut)
		}
	}
	if lr := lastRefusal(t, repo); lr["reason"] != "path_case_collision" {
		t.Fatalf("ledger reason %v", lr["reason"])
	}
	noGrant(t)
}

// The loaded-code directory set (inLoadedDir, case-insensitive) and the
// built-in guard set (gate.BuiltinGuarded, icase since fix wave 7) agree on
// every case variant of the directories both cover; the loaded-code
// directories that are NOT built-in guarded are pinned here (documented in
// design section 4: guarding them is the repo config's job). The .bin
// component (hasBinDir) is case-insensitive too.
func TestLoadedDirsAndBuiltinGuardsAgreeCaseInsensitively(t *testing.T) {
	repo, base := newRepo(t)
	variants := func(d string) []string {
		return []string{d, strings.ToUpper(d), strings.ToUpper(d[:1]) + d[1:]}
	}
	files := map[string]string{}
	for _, d := range loadedCodeDirs {
		for i, v := range variants(d) {
			files["p"+string(rune('0'+i))+"/"+v+"/pkg/x.png"] = "x\n"
		}
	}
	head := gittest.Plumb(t, repo, base, files)
	added, err := gitx.AddedMatching(repo, base, head, gitx.ICaseGlobs(gate.BuiltinGuarded...))
	if err != nil {
		t.Fatal(err)
	}
	guarded := map[string]bool{}
	for _, a := range added {
		guarded[a] = true
	}
	var covered []string
	for _, d := range loadedCodeDirs {
		n := 0
		for i, v := range variants(d) {
			p := "p" + string(rune('0'+i)) + "/" + v + "/pkg/x.png"
			if !inLoadedDir(p) {
				t.Errorf("inLoadedDir(%q) = false", p)
			}
			if guarded[p] {
				n++
			}
		}
		switch n {
		case len(variants(d)):
			covered = append(covered, d)
		case 0:
		default:
			t.Errorf("%s: built-in guard matches only %d of %d case variants", d, n, len(variants(d)))
		}
	}
	if got := strings.Join(covered, ","); got != "node_modules,vendor" {
		t.Errorf("loaded-code dirs covered by built-in guards: %s (want node_modules,vendor; update design section 4 when this changes)", got)
	}
	for _, p := range []string{"node_modules/.BIN/tool", "node_modules/.Bin/tool", "x/.bin/tool"} {
		if !hasBinDir(p) {
			t.Errorf("hasBinDir(%q) = false", p)
		}
	}
}

// FIX WAVE 8 (R1): approve shows a folded alias of a guarded name (go.worK
// with the Kelvin sign) as a new guarded file with its diff, and the hash it
// grants is the one run checks: the same head is then ok.
func TestApproveShowsFoldedAlias(t *testing.T) {
	repo, base := newRepo(t)
	const alias = "go.worK"
	head := gittest.Plumb(t, repo, base, map[string]string{alias: "go 1.27\n\nuse .\n"})
	if c, out, e := run("run", "--repo", repo, "--base", base, "--head", head); c != 3 {
		t.Fatalf("alias must need approval: run exit %d %s %s", c, out, e)
	}
	orig := requireTTY
	requireTTY = func() error { return nil }
	t.Cleanup(func() { requireTTY = orig })
	var o, e bytes.Buffer
	code := Main([]string{"approve", "--repo", repo, "--base", base, "--head", head}, strings.NewReader("yes\n"), &o, &e)
	out := o.String()
	if code != 0 || rawUnsafe(out) || !strings.Contains(out, "approved.") {
		t.Fatalf("approve: %d %q %q", code, out, e.String())
	}
	for _, s := range []string{"+ " + alias, "b/" + alias, "+use ."} {
		if !strings.Contains(out, s) {
			t.Errorf("approve output lacks %q: %q", s, out)
		}
	}
	if c, out, e := run("run", "--repo", repo, "--base", base, "--head", head); c != 0 {
		t.Fatalf("after approval: %d %s %s", c, out, e)
	}
}
