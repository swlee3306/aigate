package broker

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// W3g (G4): adversarial tests for the detach-with-removal and the one-signature
// also_local, written on top of the F2/G1 kit (newDel: fake GitHub and GitLab
// that record every request). Only properties that the G1 tests imply but never
// run over the whole flow are here; the rest is mapped in docs/31.

// keyBodyOf is the base64 part of a key line (what must never appear in a log).
func keyBodyOf(line string) string { return strings.Fields(line)[1] }

// Every request that a detaching remove_project or an also_local
// delete_remote_repo sends, in success, refusal, unknown outcome, look-alike,
// already-gone, ambiguous and live-mismatch flows, is one of the eight rows
// REWRITTEN here (not read from the production tables); every DELETE names the
// signed project's repository or a key of the signed project's list whose
// identity the scenario allows; the number of DELETEs is exactly what the
// scenario allows (0 or 1, never a second one); the registry row is gone ONLY
// on an ok answer; and no log line carries a key body, the remote path, the
// state folder or a token. All five DELETE shapes (GitHub repo and key, GitLab
// project and two key ids) are actually exercised, so the check is not vacuous.
func TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g(t *testing.T) {
	ghOK := []*regexp.Regexp{
		regexp.MustCompile(`^GET /repos/alice/Demo$`),
		regexp.MustCompile(`^DELETE /repos/alice/Demo$`),
		regexp.MustCompile(`^GET /repos/alice/Demo/keys\?page=1&per_page=100$`),
		regexp.MustCompile(`^DELETE /repos/alice/Demo/keys/2$`),
	}
	glOK := []*regexp.Regexp{
		regexp.MustCompile(`^GET /api/v4/projects/team%2Fdemo$`),
		regexp.MustCompile(`^DELETE /api/v4/projects/21$`),
		regexp.MustCompile(`^GET /api/v4/projects/21/deploy_keys\?page=1&per_page=100$`),
		regexp.MustCompile(`^DELETE /api/v4/projects/21/deploy_keys/(8|9)$`),
	}
	forbidden := []string{"/groups", "/orgs", "/user", "/tokens", "collaborators", "/hooks", "/branches", "/contents", "/protected_branches", "other", "/api/v4/deploy_keys"}
	seen := map[string]bool{}
	var shapes []string
	check := func(what string, reqs []string, ok []*regexp.Regexp) {
		t.Helper()
		for _, r := range reqs {
			matched := false
			for _, re := range ok {
				if re.MatchString(r) {
					matched = true
					seen[re.String()] = true
				}
			}
			if !matched {
				t.Errorf("%s: request outside the allowlist: %s", what, r)
			}
			for _, f := range forbidden {
				if strings.Contains(r, f) {
					t.Errorf("%s: forbidden fragment %q in %s", what, f, r)
				}
			}
			if strings.HasPrefix(r, "DELETE ") {
				shapes = append(shapes, r)
			}
		}
	}
	ghMine := keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"2", "orchd demo-gh", delKeyB}, [3]string{"3", "my laptop", delKeyC})
	glShared := keysJSON([3]string{"8", "orchd first-id", delKeyB}, [3]string{"10", "shared deploy", delKeyC}) // our key, the first project's title
	glOwn := keysJSON([3]string{"9", "orchd demo-gl", delKeyB}, [3]string{"10", "shared deploy", delKeyC})
	type scenario struct {
		name    string
		id      string
		setup   func(d *delEnv)
		run     func(d *delEnv) (any, *OpError)
		code    string // "" = ok
		deletes int    // DELETE requests allowed and required
		rowGone bool
	}
	detach := func(id string) func(d *delEnv) (any, *OpError) {
		return func(d *delEnv) (any, *OpError) { return d.do("remove_project", d.rmDetachArgs(id, false, delKeyB)) }
	}
	also := func(id string, on bool) func(d *delEnv) (any, *OpError) {
		return func(d *delEnv) (any, *OpError) { return d.do("delete_remote_repo", alsoArgs(d, id, on)) }
	}
	scenarios := []scenario{
		{"github detach ok", "demo-gh", func(d *delEnv) { d.gh.keyPages[1] = ghMine }, detach("demo-gh"), "", 1, true},
		{"github detach refused 403", "demo-gh", func(d *delEnv) { d.gh.keyPages[1] = ghMine; d.gh.keyStatus = 403 }, detach("demo-gh"), "remove_refused", 1, false},
		{"github detach uncertain 500", "demo-gh", func(d *delEnv) { d.gh.keyPages[1] = ghMine; d.gh.keyStatus = 500 }, detach("demo-gh"), "delete_partial", 1, false},
		{"github detach uncertain 200", "demo-gh", func(d *delEnv) { d.gh.keyPages[1] = ghMine; d.gh.keyStatus = 200 }, detach("demo-gh"), "delete_partial", 1, false},
		{"github look-alike: our title, another body", "demo-gh", func(d *delEnv) {
			d.gh.keyPages[1] = keysJSON([3]string{"2", "orchd demo-gh", delKeyA})
		}, detach("demo-gh"), "remove_refused", 0, false},
		{"github look-alike: our body, another title", "demo-gh", func(d *delEnv) {
			d.gh.keyPages[1] = keysJSON([3]string{"3", "my laptop", delKeyB})
		}, detach("demo-gh"), "remove_refused", 0, false},
		{"github two keys with title and body", "demo-gh", func(d *delEnv) {
			d.gh.keyPages[1] = keysJSON([3]string{"2", "orchd demo-gh", delKeyB}, [3]string{"4", "orchd demo-gh", delKeyB + " again"})
		}, detach("demo-gh"), "remove_refused", 0, false},
		{"github already gone", "demo-gh", func(d *delEnv) {
			d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd other", delKeyA})
		}, detach("demo-gh"), "", 0, true},
		{"github key list 500", "demo-gh", func(d *delEnv) { d.gh.keyListSt = 500 }, detach("demo-gh"), "provider_unreachable", 0, false},
		{"github live number differs", "demo-gh", func(d *delEnv) { d.gh.keyPages[1] = ghMine; d.gh.repoBody = ghRepoBody(78, "alice/Demo") }, detach("demo-gh"), "remote_changed", 0, false},
		{"gitlab shared key matched by body", "demo-gl", func(d *delEnv) { d.gl.keyPages[1] = glShared }, detach("demo-gl"), "", 1, true},
		{"gitlab own key", "demo-gl", func(d *delEnv) { d.gl.keyPages[1] = glOwn }, detach("demo-gl"), "", 1, true},
		{"gitlab two body matches", "demo-gl", func(d *delEnv) {
			d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd first-id", delKeyB}, [3]string{"9", "orchd demo-gl", delKeyB})
		}, detach("demo-gl"), "remove_refused", 0, false},
		{"gitlab look-alike: our title, another body", "demo-gl", func(d *delEnv) {
			d.gl.keyPages[1] = keysJSON([3]string{"9", "orchd demo-gl", delKeyA})
		}, detach("demo-gl"), "remove_refused", 0, false},
		{"gitlab uncertain 500", "demo-gl", func(d *delEnv) { d.gl.keyPages[1] = glOwn; d.gl.keyStatus = 500 }, detach("demo-gl"), "delete_partial", 1, false},
		{"github also_local ok", "demo-gh", nil, also("demo-gh", true), "", 1, true},
		{"gitlab also_local ok", "demo-gl", nil, also("demo-gl", true), "", 1, true},
		{"github also_local uncertain", "demo-gh", func(d *delEnv) { d.gh.delStatus = 500 }, also("demo-gh", true), "delete_partial", 1, false},
		{"github also_local refused", "demo-gh", func(d *delEnv) { d.gh.delStatus = 403 }, also("demo-gh", true), "remove_refused", 1, false},
		{"github also_local live name differs", "demo-gh", func(d *delEnv) { d.gh.repoBody = ghRepoBody(77, "alice/Other") }, also("demo-gh", true), "remote_changed", 0, false},
		{"github without also_local (row kept)", "demo-gh", nil, also("demo-gh", false), "", 1, false},
	}
	for _, sc := range scenarios {
		d := newDel(t)
		if sc.setup != nil {
			sc.setup(d)
		}
		_, oe := sc.run(d)
		if got := codeOf(oe); got != sc.code {
			t.Errorf("%s: code %q, want %q", sc.name, got, sc.code)
		}
		check(sc.name+" (github)", d.gh.all(), ghOK)
		check(sc.name+" (gitlab)", d.gl.all(), glOK)
		if n := d.gh.deletes() + d.gl.deletes(); n != sc.deletes {
			t.Errorf("%s: %d DELETE requests, want %d: %v %v", sc.name, n, sc.deletes, d.gh.all(), d.gl.all())
		}
		// the other project's provider never hears of this one
		if strings.HasSuffix(sc.id, "-gh") && len(d.gl.all()) != 0 || strings.HasSuffix(sc.id, "-gl") && len(d.gh.all()) != 0 {
			t.Errorf("%s: the other provider was contacted", sc.name)
		}
		if gone := !d.rowExists(sc.id); gone != sc.rowGone {
			t.Errorf("%s: row gone %v, want %v", sc.name, gone, sc.rowGone)
		}
		other := map[string]string{"demo-gh": "demo-gl", "demo-gl": "demo-gh"}[sc.id]
		if !d.rowExists(other) {
			t.Errorf("%s: the other project's row vanished", sc.name)
		}
		logs := d.logs.String()
		for _, bad := range []string{keyBodyOf(delKeyA), keyBodyOf(delKeyB), keyBodyOf(delKeyC), "alice/Demo", "team/demo", d.root, ghCanary, c3Canary, delOtherGH, delOtherGL} {
			if strings.Contains(logs, bad) {
				t.Errorf("%s: %q in the logs: %q", sc.name, bad, logs)
			}
		}
	}
	for _, re := range append(append([]*regexp.Regexp{}, ghOK...), glOK...) {
		if !seen[re.String()] {
			t.Errorf("allowlist row never exercised by the scenarios: %s", re)
		}
	}
	want := map[string]bool{"DELETE /repos/alice/Demo": false, "DELETE /repos/alice/Demo/keys/2": false, "DELETE /api/v4/projects/21": false,
		"DELETE /api/v4/projects/21/deploy_keys/8": false, "DELETE /api/v4/projects/21/deploy_keys/9": false}
	for _, s := range shapes {
		if _, ok := want[s]; !ok {
			t.Errorf("an unexpected DELETE: %s", s)
		}
		want[s] = true
	}
	for s, sent := range want {
		if !sent {
			t.Errorf("DELETE shape never sent: %s", s)
		}
	}
}

// The key line is NOT signed (orchd supplies it from its key folder). With a
// VALID detach signature for one project, any unsigned line, whether another
// key of the same repository (another tool's key, the user's laptop key, a key
// with our title but another body) or a key that is not listed at all, can
// never make the broker:
//   - send a DELETE outside the signed project's own key route,
//   - send more than one DELETE,
//   - (GitHub) take off a key that does not carry the title "orchd <id>",
//   - (both) take off a key whose body differs from the line it was given,
//   - (GitHub) do anything but refuse: every foreign line is a look-alike
//     (our title is listed), nothing is deleted and the row stays.
//
// The request ID is spent by the first attempt (replay is request_reused).
// Known residual (docs/31 §7, pinned here so a change is noticed): on GitLab
// the title is ignored (G-D2), so the unsigned line alone chooses WHICH key of
// the signed project is taken off (here the "my laptop" key), and the row is
// then unregistered although the key titled "orchd demo-gl" stays listed. The
// route stays the signed project's, at most one key, never a global key route.
// Only an orchd that sends a line other than its own .pub can do this.
func TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g(t *testing.T) {
	ghList := [][3]string{{"1", "orchd other", delKeyA}, {"2", "orchd demo-gh", delKeyB}, {"3", "my laptop", delKeyC}, {"4", "ci", delKeyD}}
	glList := [][3]string{{"8", "orchd demo-gl", delKeyB}, {"10", "my laptop", delKeyC}, {"11", "ci", delKeyD}}
	notListed := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKeyEKeyEKeyEKeyEKeyEKeyEKeyEKeyEKeyEKeyE"
	type probe struct {
		kind, id string
		list     [][3]string
		route    string
		title    string
	}
	for _, p := range []probe{
		{"github", "demo-gh", ghList, "DELETE /repos/alice/Demo/keys/", "orchd demo-gh"},
		{"gitlab", "demo-gl", glList, "DELETE /api/v4/projects/21/deploy_keys/", ""},
	} {
		for _, line := range []string{delKeyA, delKeyC, delKeyD, notListed, delKeyC + " planted-comment"} {
			d := newDel(t)
			fake := d.gh
			if p.kind == "gitlab" {
				fake = d.gl
			}
			fake.keyPages[1] = keysJSON(p.list...)
			a := d.rmDetachArgs(p.id, false, line)
			_, oe := d.do("remove_project", a)
			dels := []string{}
			for _, r := range fake.all() {
				if strings.HasPrefix(r, "DELETE ") {
					dels = append(dels, r)
				}
			}
			if len(dels) > 1 {
				t.Fatalf("%s %s: more than one DELETE: %v", p.kind, line, dels)
			}
			for _, r := range dels {
				if !strings.HasPrefix(r, p.route) {
					t.Fatalf("%s: DELETE outside the signed project's key route: %s", p.kind, r)
				}
				id := strings.TrimPrefix(r, p.route)
				var hit [3]string
				for _, k := range p.list {
					if k[0] == id {
						hit = k
					}
				}
				if hit[0] == "" || hit[2] != strings.Join(strings.Fields(line)[:2], " ") {
					t.Fatalf("%s: key %s taken off for line %q (listed %v)", p.kind, id, line, hit)
				}
				if p.title != "" && hit[1] != p.title {
					t.Fatalf("%s: a key titled %q was taken off", p.kind, hit[1])
				}
			}
			// the row goes only when the outcome is ok and no listed key keeps our title or the given body
			if oe == nil {
				if len(dels) == 0 {
					t.Fatalf("%s %s: ok without a DELETE although keys with our title are listed", p.kind, line)
				}
			} else if !d.rowExists(p.id) {
				t.Fatalf("%s %s: refused (%s) but the row is gone", p.kind, line, oe.Code)
			}
			if p.kind == "github" && (oe == nil || oe.Code != "remove_refused" || len(dels) != 0 || !d.rowExists(p.id)) {
				t.Fatalf("github %s: a foreign line must be a look-alike refusal: %v %v", line, oe, dels)
			}
			if p.kind == "gitlab" && strings.HasPrefix(line, delKeyC) && (oe != nil || fmt.Sprint(dels) != "[DELETE /api/v4/projects/21/deploy_keys/10]" || d.rowExists(p.id)) {
				t.Fatalf("the pinned GitLab residual changed (update docs/31 §7): %v %v", oe, dels)
			}
			n := len(fake.all())
			replay := "request_reused"
			if oe == nil {
				replay = "project_unknown" // the row is gone: refused in the shape stage
			}
			d.wantCode("remove_project", a, replay)
			if len(fake.all()) != n {
				t.Fatalf("%s: the replay reached the provider", p.kind)
			}
			if len(d.gh.all())+len(d.gl.all()) != n {
				t.Fatalf("%s: the other provider was contacted", p.kind)
			}
		}
	}
	// control: the right line does take our key off, on both providers
	for _, c := range []struct {
		id   string
		list [][3]string
		del  string
	}{{"demo-gh", ghList, "DELETE /repos/alice/Demo/keys/2"}, {"demo-gl", glList, "DELETE /api/v4/projects/21/deploy_keys/8"}} {
		d := newDel(t)
		d.gh.keyPages[1], d.gl.keyPages[1] = keysJSON(c.list...), keysJSON(c.list...)
		d.wantCode("remove_project", d.rmDetachArgs(c.id, false, delKeyB), "")
		all := append(d.gh.all(), d.gl.all()...)
		if fmt.Sprint(all[len(all)-1]) != c.del || d.rowExists(c.id) {
			t.Fatalf("control %s: %v", c.id, all)
		}
	}
}
