package broker

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"aigate/internal/registrymsg"
)

// W3f (F5): adversarial tests for the signed deletion ops, written on top of the
// F2 kit (newDel: fake GitHub and fake GitLab that record every request). Only
// properties that the F2 tests imply but never run over the whole flow are here;
// the rest is cited in docs/29.

type delAct struct {
	name string
	op   string
	args func(d *delEnv) map[string]any
}

// delActs: every signed action of every wave (the W3e set, with both kinds of
// create_project/set_provider) plus the four W3f deletion actions. The fixtures
// each act needs are built by prepDel.
func delActs() []delAct {
	var out []delAct
	for _, a := range w3eActs() {
		a := a
		out = append(out, delAct{a.name, a.op, func(d *delEnv) map[string]any { return a.args(d.e3env) }})
	}
	out = append(out,
		delAct{"remove_project", "remove_project", func(d *delEnv) map[string]any { return d.rmArgs("plain", false) }},
		delAct{"delete_remote_repo", "delete_remote_repo", func(d *delEnv) map[string]any { return d.delArgs("demo-gh", nil) }},
		delAct{"remove_deploy_key", "remove_deploy_key", func(d *delEnv) map[string]any { return d.keyArgs("demo-gh", "") }},
		delAct{"clear_orphan", "clear_orphan", func(d *delEnv) map[string]any {
			return d.signedArgs(d.alice, registrymsg.ActionClearOrphan, "dgh", map[string]any{"project": "alice/Demo"})
		}},
	)
	return out
}

func prepDel(t *testing.T) *delEnv {
	t.Helper()
	d := newDel(t)
	d.existing()
	d.withProvider("spare", d.gl.URL)
	d.ownLocal("plain", false, nil)
	if _, err := d.s.Registry.AddOrphan(Orphan{Provider: "dgh", Project: "alice/Demo", Step: "deploy_key", At: "2026-11-10T01:02:03Z", Origin: OriginCreated}); err != nil {
		t.Fatal(err)
	}
	d.gh.keyPages[1] = keysJSON([3]string{"2", "orchd demo-gh", delKeyB})
	return d
}

// A signature made for one action never works as another, over EVERY ordered
// pair of all signed actions including remove_project, delete_remote_repo,
// remove_deploy_key and clear_orphan: the signature alone, and together with its
// change, request ID and time, is grafted onto the other request. Every graft is
// refused, no provider is contacted (no GET, no DELETE), and the registry, the
// token store and the used-ID file are unchanged. The controls prove each of the
// four deletion actions works with its OWN signature (the matrix is not refusing
// everything): then the deletion really happens.
func TestDeletionNeedsItsOwnSignatureForAllActionPairsW3f(t *testing.T) {
	d := prepDel(t)
	acts := delActs()
	snapReg, snapTok := d.snap(), d.tokSnap()
	pairs := 0
	codes := map[string]int{}
	for ai, a := range acts {
		for bi, b := range acts {
			if ai == bi {
				continue
			}
			sigA := a.args(d)
			r1 := b.args(d)
			r1["signature"] = sigA["signature"]
			r2 := b.args(d)
			for _, k := range []string{"signature", "change", "request", "issued"} {
				r2[k] = sigA[k]
			}
			for i, r := range []map[string]any{r1, r2} {
				_, oe := d.do(b.op, r)
				if oe == nil {
					t.Errorf("%s signature accepted as %s (variant %d)", a.name, b.name, i+1)
					continue
				}
				codes[oe.Code]++
				switch oe.Code {
				case "bad_signature", "change_mismatch", "bad_request", "deploy_key_mismatch":
				default:
					t.Errorf("%s signature on %s (variant %d): code %s", a.name, b.name, i+1, oe.Code)
				}
			}
			pairs++
		}
	}
	if pairs != len(acts)*(len(acts)-1) || pairs < 200 {
		t.Fatalf("pairs %d", pairs)
	}
	d.mustEqual(snapReg, "cross-action signatures")
	if !d.tokSnap().sameStore(snapTok) || !d.tokSnap().sameUsed(snapTok) {
		t.Fatal("the token store or the used-ID file changed")
	}
	d.e3env.noGHHits("cross-action signatures")
	d.e3env.noGLHits("cross-action signatures")
	d.noHits("cross-action signatures")
	if codes["bad_signature"] == 0 || codes["change_mismatch"] == 0 {
		t.Fatalf("control: refusal codes %v (the signature layer was never what refused)", codes)
	}
	// controls: every deletion action works with its own signature, and deletes
	for _, name := range []string{"remove_deploy_key", "delete_remote_repo", "clear_orphan", "remove_project"} {
		for _, a := range acts {
			if a.name != name {
				continue
			}
			if _, oe := d.do(a.op, a.args(d)); oe != nil {
				t.Fatalf("control %s: %v", name, oe.Code)
			}
		}
	}
	if d.gh.deletes() != 2 { // the key and the repository
		t.Fatalf("control deletes: %v", d.gh.all())
	}
	if _, ok := d.find("plain"); ok {
		t.Fatal("control: remove_project did not remove the row")
	}
}

// A deletion without a valid signature does nothing, for each of the four ops:
// no signature, a garbage signature, a signature of an unknown signer, no change,
// no request ID, no time, and a valid signature of the same action over ANOTHER
// subject. The provider sees nothing, the registry is unchanged, no request ID is
// consumed (nothing is burnt by an invalid signature).
func TestUnsignedOrForeignSignedDeletionDoesNothingW3f(t *testing.T) {
	d := prepDel(t)
	bob := newWebTestSigner(t, "bob") // never added to the signer list
	other := func(a map[string]any, id string) map[string]any {
		o := copyArgs(a, func(m map[string]any) {})
		o["id"] = id
		return o
	}
	d.put("demo-gh2", "dgh", "alice/Demo2", 78, OriginCreated)
	type tc struct {
		name string
		op   string
		good func() map[string]any
		sign func(who testSigner) map[string]any
	}
	cases := []tc{
		{"remove_project", "remove_project", func() map[string]any { return d.rmArgs("plain", false) },
			func(w testSigner) map[string]any {
				return d.signedArgs(w, registrymsg.ActionRemoveProject, "plain", map[string]any{"scope": "local", "legacy_ack": false, "detach_key": false})
			}},
		{"delete_remote_repo", "delete_remote_repo", func() map[string]any { return d.delArgs("demo-gh", nil) },
			func(w testSigner) map[string]any {
				return d.signedArgs(w, registrymsg.ActionDeleteRemoteRepo, "demo-gh", d.delFields("alice/Demo", "77", "Demo"))
			}},
		{"remove_deploy_key", "remove_deploy_key", func() map[string]any { return d.keyArgs("demo-gh", "") },
			func(w testSigner) map[string]any {
				a := d.signedArgs(w, registrymsg.ActionRemoveDeployKey, "demo-gh", map[string]any{"remote_path": "alice/Demo"})
				a["deploy_key"] = delKeyB
				return a
			}},
		{"clear_orphan", "clear_orphan", func() map[string]any {
			return d.signedArgs(d.alice, registrymsg.ActionClearOrphan, "dgh", map[string]any{"project": "alice/Demo"})
		}, func(w testSigner) map[string]any {
			return d.signedArgs(w, registrymsg.ActionClearOrphan, "dgh", map[string]any{"project": "alice/Demo"})
		}},
	}
	before := d.snap()
	for _, c := range cases {
		variants := map[string]map[string]any{
			"garbage signature": copyArgs(c.good(), func(m map[string]any) {
				m["signature"] = map[string]any{"signer": "alice", "format": "ssh", "data": "AAAA"}
			}),
			"unknown signer": c.sign(bob),
		}
		for _, k := range []string{"signature", "change", "request", "issued"} {
			k := k
			variants["no "+k] = copyArgs(c.good(), func(m map[string]any) { delete(m, k) })
		}
		// a valid signature over another subject, carried to this subject
		switch c.name {
		case "remove_project":
			d.ownLocal("plain2", false, nil)
			variants["another subject"] = other(d.rmArgs("plain2", false), "plain")
		case "delete_remote_repo":
			variants["another subject"] = other(d.delArgs("demo-gh", nil), "demo-gh2")
		case "remove_deploy_key":
			variants["another subject"] = other(d.keyArgs("demo-gh", ""), "demo-gh2")
		case "clear_orphan":
			variants["another subject"] = other(d.signedArgs(d.alice, registrymsg.ActionClearOrphan, "dgl", map[string]any{"project": "alice/Demo"}), "dgh")
		}
		for name, a := range variants {
			_, oe := d.do(c.op, a)
			if oe == nil {
				t.Errorf("%s / %s: accepted", c.name, name)
				continue
			}
			switch oe.Code {
			case "bad_request", "bad_signature", "change_mismatch", "unknown_signer", "orphan_unknown", "project_unknown", "not_signed", "remote_changed":
			default:
				t.Errorf("%s / %s: code %s", c.name, name, oe.Code)
			}
		}
	}
	// (the "another subject" fixture for remove_project adds a row, so only the used-ID file is compared)
	if string(before.used) != string(d.snap().used) {
		t.Fatal("an invalid signature consumed a request ID")
	}
	d.noHits("unsigned deletion")
	if d.gh.deletes()+d.gl.deletes() != 0 {
		t.Fatal("a DELETE left the broker without a valid signature")
	}
	for _, id := range []string{"plain", "demo-gh", "demo-gh2", "demo-gl"} {
		if _, ok := d.find(id); !ok {
			t.Fatalf("project %s vanished", id)
		}
	}
	if len(d.orphans()) != 1 {
		t.Fatalf("an orphan record vanished: %v", d.orphans())
	}
}

// The signature of a delete_remote_repo is bound to the repository NUMBER. If the
// repository was recreated under the same name (new number), the old signature
// cannot delete the new repository: replaying it is refused (the request ID is
// spent), and a FRESH signature that still names the old number is refused as
// remote_changed, both when the registry knows the old number (shape stage) and
// when only the provider knows the new one (live check). No DELETE is ever sent.
func TestReplayedDeleteSignatureAfterRecreateIsRefusedW3f(t *testing.T) {
	// registry and signature say 77, GitHub now has a NEW repository with 78
	d := newDel(t)
	d.gh.repoBody = ghRepoBody(78, "alice/Demo")
	old := d.delArgs("demo-gh", nil) // signed for 77
	d.wantCode("delete_remote_repo", old, "remote_changed")
	d.wantCode("delete_remote_repo", old, "request_reused") // the refusal after a valid signature spent it
	d.wantCode("delete_remote_repo", d.delArgs("demo-gh", nil), "remote_changed")
	if d.gh.deletes() != 0 {
		t.Fatalf("a recreated repository was deleted: %v", d.gh.all())
	}

	// the repository was deleted, then recreated and linked again (number 78 in the registry):
	// the old signature (number 77) and a fresh one with 77 are both refused before any request
	d = newDel(t)
	first := d.delArgs("demo-gh", nil)
	if _, oe := d.do("delete_remote_repo", first); oe != nil {
		t.Fatal(oe.Code)
	}
	if d.gh.deletes() != 1 {
		t.Fatalf("%v", d.gh.all())
	}
	d.put("demo-gh", "dgh", "alice/Demo", 78, OriginCreated)
	d.gh.repoBody = ghRepoBody(78, "alice/Demo")
	n := len(d.gh.all())
	d.refusedNoBurn2("delete_remote_repo", first, "remote_changed")                     // replay
	d.refusedNoBurn2("delete_remote_repo", d.delArgs("demo-gh", nil), "remote_changed") // fresh signature, stale number
	if len(d.gh.all()) != n || d.gh.deletes() != 1 {
		t.Fatalf("the recreated repository was touched: %v", d.gh.all())
	}
	// a signature for the right number deletes it (control: the refusals were about the number)
	if _, oe := d.do("delete_remote_repo", d.delArgs("demo-gh", func(f map[string]any) { f["remote_id"] = "78" })); oe != nil {
		t.Fatalf("control: %v", oe.Code)
	}
	if d.gh.deletes() != 2 {
		t.Fatalf("%v", d.gh.all())
	}
}

// Every request the deletion flows send to GitHub or GitLab (success, refusal,
// unknown outcome, live mismatch, key removal, orphan, remove_project, the refused
// origins) is a member of the pre-F2 allowlist plus exactly the six F2 rows, as
// REWRITTEN here (not read from the production table); every DELETE names the
// repository or key bound to the signed project; groups, organisations, user and
// user keys, tokens, collaborators, the global deploy_keys list and other
// repositories never appear; and all four DELETE shapes are exercised (the check
// is not vacuous).
func TestEveryDeletionRequestIsAllowlistedAndBoundToTheProjectW3f(t *testing.T) {
	ghOK := []*regexp.Regexp{
		regexp.MustCompile(`^GET /repos/alice/Demo$`),
		regexp.MustCompile(`^DELETE /repos/alice/Demo$`),
		regexp.MustCompile(`^GET /repos/alice/Demo/keys\?page=[0-9]+&per_page=100$`),
		regexp.MustCompile(`^DELETE /repos/alice/Demo/keys/2$`),
	}
	glOK := []*regexp.Regexp{
		regexp.MustCompile(`^GET /api/v4/projects/team%2Fdemo$`),
		regexp.MustCompile(`^DELETE /api/v4/projects/21$`),
		regexp.MustCompile(`^GET /api/v4/projects/21/deploy_keys\?page=[0-9]+&per_page=100$`),
		regexp.MustCompile(`^DELETE /api/v4/projects/21/deploy_keys/9$`),
	}
	forbidden := []string{"/groups", "/orgs", "/user", "/tokens", "collaborators", "/hooks", "/branches", "/contents", "/protected_branches", "other", "/api/v4/deploy_keys"}
	seen := map[string]bool{}
	check := func(what string, reqs []string, ok []*regexp.Regexp) {
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
		}
	}
	type scenario struct {
		name string
		run  func(d *delEnv)
	}
	keysGH := keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"2", "orchd demo-gh", delKeyB}, [3]string{"3", "my laptop", delKeyB})
	keysGL := keysJSON([3]string{"8", "orchd demo-gl", delKeyA}, [3]string{"9", "orchd demo-gl", delKeyB}, [3]string{"10", "orchd other", delKeyB})
	scenarios := []scenario{
		{"github delete ok", func(d *delEnv) { d.do("delete_remote_repo", d.delArgs("demo-gh", nil)) }},
		{"gitlab delete ok", func(d *delEnv) { d.do("delete_remote_repo", d.delArgs("demo-gl", nil)) }},
		{"github delete refused 403", func(d *delEnv) { d.gh.delStatus = 403; d.do("delete_remote_repo", d.delArgs("demo-gh", nil)) }},
		{"github delete uncertain 500", func(d *delEnv) { d.gh.delStatus = 500; d.do("delete_remote_repo", d.delArgs("demo-gh", nil)) }},
		{"gitlab delete uncertain dropped", func(d *delEnv) { d.gl.delAbort = true; d.do("delete_remote_repo", d.delArgs("demo-gl", nil)) }},
		{"github live id differs", func(d *delEnv) {
			d.gh.repoBody = ghRepoBody(78, "alice/Demo")
			d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
		}},
		{"gitlab live name differs", func(d *delEnv) {
			d.gl.repoBody = glRepoBody(21, "team/renamed")
			d.do("delete_remote_repo", d.delArgs("demo-gl", nil))
		}},
		{"github key removal", func(d *delEnv) { d.gh.keyPages[1] = keysGH; d.do("remove_deploy_key", d.keyArgs("demo-gh", "")) }},
		{"gitlab key removal", func(d *delEnv) { d.gl.keyPages[1] = keysGL; d.do("remove_deploy_key", d.keyArgs("demo-gl", "")) }},
		{"github key not found", func(d *delEnv) {
			d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd other", delKeyA})
			d.do("remove_deploy_key", d.keyArgs("demo-gh", ""))
		}},
		{"orphan and remove_project", func(d *delEnv) {
			d.s.Registry.AddOrphan(Orphan{Provider: "dgh", Project: "alice/Demo", Step: "deploy_key", At: "2026-11-10T01:02:03Z", Origin: OriginCreated})
			d.do("clear_orphan", d.signedArgs(d.alice, registrymsg.ActionClearOrphan, "dgh", map[string]any{"project": "alice/Demo"}))
			d.do("remove_project", d.rmArgs("demo-gh", false))
			d.do("remove_project", d.rmArgs("demo-gl", false))
		}},
		{"refused origins", func(d *delEnv) {
			prov, rid := "dgh", int64(77)
			for _, o := range []string{OriginLinked, OriginUnknown} {
				d.putRaw(Project{ID: "demo-gh", Path: d.root + "/demo-gh", Kind: "registered", Provider: &prov, Remote: &RemoteLink{Path: "alice/Demo", ID: &rid}, Origin: o})
				d.do("delete_remote_repo", d.delArgs("demo-gh", nil))
			}
		}},
	}
	var all []string
	for _, sc := range scenarios {
		d := newDel(t)
		sc.run(d)
		check(sc.name+" (github)", d.gh.all(), ghOK)
		check(sc.name+" (gitlab)", d.gl.all(), glOK)
		all = append(all, d.gh.all()...)
		all = append(all, d.gl.all()...)
	}
	sort.Strings(all)
	for _, re := range append(append([]*regexp.Regexp{}, ghOK...), glOK...) {
		if !seen[re.String()] {
			t.Errorf("allowlist row never exercised by the scenarios: %s", re)
		}
	}
	for _, m := range []string{"DELETE /repos/alice/Demo", "DELETE /repos/alice/Demo/keys/2", "DELETE /api/v4/projects/21", "DELETE /api/v4/projects/21/deploy_keys/9"} {
		found := false
		for _, r := range all {
			if r == m {
				found = true
			}
		}
		if !found {
			t.Errorf("DELETE shape never sent: %s", m)
		}
	}
}
