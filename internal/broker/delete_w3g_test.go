package broker

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/registrymsg"
)

// W3g (G1): remove_project can take the project's deploy key off the remote
// (signed detach_key) and unregisters only when the key is definitely gone;
// delete_remote_repo with also_local=true removes the registry row. The
// providers are httptest fakes (delFake); nothing leaves the machine.

const (
	ghKeysPath    = "GET /repos/alice/Demo/keys?page=1&per_page=100"
	ghRepoGet     = "GET /repos/alice/Demo"
	glRepoGetW3g  = "GET /api/v4/projects/team%2Fdemo"
	glKeysPathW3g = "GET /api/v4/projects/21/deploy_keys?page=1&per_page=100"
)

func w3gKeys(d *delEnv) {
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"2", "orchd demo-gh", delKeyB + " orchd@host"}, [3]string{"3", "my laptop", delKeyB})
}

// legacyWithRemote: demo-gh as a legacy project that has a provider and a remote
// (the registry allows it: attach_remote works on legacy rows).
func legacyWithRemote(d *delEnv) {
	prov, rid := "dgh", int64(77)
	d.putRaw(Project{ID: "demo-gh", Path: d.root + "/demo-gh", Kind: "legacy", Provider: &prov, Remote: &RemoteLink{Path: "alice/Demo", ID: &rid}, Origin: OriginLegacy})
}

func (d *delEnv) rowExists(id string) bool { _, ok := d.find(id); return ok }

// What this proves: with detach_key signed, the request order is repository
// GET -> key list GET -> key DELETE -> registry write; the row still exists
// while the key DELETE is in flight and is gone only afterwards (GitHub and
// GitLab), and the answer says what happened to the key.
func TestRemoveProjectDetachesThenUnregisters(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	d.gh.onKeyDel = func() {
		if !d.rowExists("demo-gh") {
			t.Error("the row was unregistered BEFORE the key DELETE")
		}
	}
	res, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	if oe != nil {
		t.Fatal(oe.Code)
	}
	want := map[string]any{"id": "demo-gh", "removed": true, "key_state": "removed"}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("result %v", res)
	}
	if got := d.gh.all(); !reflect.DeepEqual(got, []string{ghRepoGet, ghKeysPath, "DELETE /repos/alice/Demo/keys/2"}) {
		t.Fatalf("trace %v", got)
	}
	if d.rowExists("demo-gh") {
		t.Fatal("the row was not unregistered")
	}
	if d.hits() != len(d.gh.all()) {
		t.Fatal("another provider was contacted")
	}
	// the provider's own token (never another stored one) on all three requests
	for _, c := range d.gh.creds() {
		if strings.Contains(c, delOtherGH) || !strings.Contains(c, ghCanary) {
			t.Fatalf("credential %q", c)
		}
	}
	// replay: the signature is spent; a fresh one finds no project
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "project_unknown")

	// GitLab: only THIS project's key, through the per-project route
	d = newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd demo-gl2", delKeyA}, [3]string{"9", "orchd demo-gl", delKeyB}, [3]string{"10", "shared deploy", delKeyC})
	d.gl.onKeyDel = func() {
		if !d.rowExists("demo-gl") {
			t.Error("gitlab: the row was unregistered BEFORE the key DELETE")
		}
	}
	if _, oe := d.do("remove_project", d.rmDetachArgs("demo-gl", false, delKeyB)); oe != nil {
		t.Fatal(oe.Code)
	}
	if got := d.gl.all(); !reflect.DeepEqual(got, []string{glRepoGetW3g, glKeysPathW3g, "DELETE /api/v4/projects/21/deploy_keys/9"}) {
		t.Fatalf("gitlab trace %v", got)
	}
	if d.rowExists("demo-gl") {
		t.Fatal("gitlab: the row was not unregistered")
	}
	// the other project's row is untouched
	if !d.rowExists("demo-gh") {
		t.Fatal("another row vanished")
	}
}

// A linked or unknown-origin row with a remote can detach too (the key is
// ours either way); a legacy row WITH provider and remote needs its ack.
func TestRemoveProjectDetachWorksForEveryOriginWithARemote(t *testing.T) {
	for _, origin := range []string{OriginCreated, OriginLinked, OriginUnknown} {
		d := newDel(t)
		d.put("demo-gh", "dgh", "alice/Demo", 77, origin)
		w3gKeys(d)
		if _, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB)); oe != nil {
			t.Fatalf("%s: %s", origin, oe.Code)
		}
		if d.rowExists("demo-gh") || d.gh.deletes() != 1 {
			t.Fatalf("%s: %v", origin, d.gh.all())
		}
	}
	d := newDel(t)
	legacyWithRemote(d)
	w3gKeys(d)
	d.refusedNoBurn("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "legacy_ack_required")
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", true, delKeyB), "")
	if d.rowExists("demo-gh") || d.gh.deletes() != 1 {
		t.Fatal("legacy row with a remote did not detach")
	}
}

// What this proves: everything the request and the registry alone decide is
// refused in the SHAPE stage: no provider contact, no request ID burnt (the
// same signed args refuse the same way twice), the registry unchanged.
func TestRemoveProjectDetachKeyShapeRules(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	d.putRaw(Project{ID: "norem", Path: d.root + "/norem", Kind: "registered", Origin: OriginUnknown})
	d.putRaw(Project{ID: "toy", Path: d.root + "/toy", Kind: "legacy", Origin: OriginLegacy})
	prov := "dgh"
	d.putRaw(Project{ID: "gone", Path: d.root + "/gone", Kind: "registered", Provider: &prov, Origin: OriginCreated, RemoteDeletedAt: strp("2026-11-10T01:02:03Z")})
	d.putRaw(Project{ID: "noprov", Path: d.root + "/noprov", Kind: "registered", Remote: &RemoteLink{Path: "alice/Demo"}, Origin: OriginUnknown}) // a legacy-style link: remote, no provider
	d.putRaw(Project{ID: "noid", Path: d.root + "/noid", Kind: "registered", Provider: &prov, Remote: &RemoteLink{Path: "alice/Demo"}, Origin: OriginCreated})
	for _, c := range []struct {
		name string
		a    map[string]any
		code string
	}{
		{"no remote", d.rmDetachArgs("norem", false, delKeyB), "bad_request"},
		{"legacy without remote", d.rmDetachArgs("toy", true, delKeyB), "bad_request"},
		{"remote_deleted_at set", d.rmDetachArgs("gone", false, delKeyB), "bad_request"},
		{"no provider", d.rmDetachArgs("noprov", false, delKeyB), "bad_request"},
		{"remote without a number", d.rmDetachArgs("noid", false, delKeyB), "remote_changed"},
		{"key line missing", d.rmDetachArgs("demo-gh", false, ""), "bad_request"},
		{"key line not a key", d.rmDetachArgs("demo-gh", false, "not a key"), "bad_request"},
		{"key line is only a type", d.rmDetachArgs("demo-gh", false, "ssh-ed25519"), "bad_request"},
		{"key line with a newline", d.rmDetachArgs("demo-gh", false, delKeyB+"\nssh-ed25519 AAAA"), "bad_request"},
		{"key line an empty string", copyArgs(d.rmDetachArgs("demo-gh", false, delKeyB), func(m map[string]any) { m["deploy_key"] = "" }), "bad_request"},
		{"key line is not a string", copyArgs(d.rmDetachArgs("demo-gh", false, delKeyB), func(m map[string]any) { m["deploy_key"] = 5 }), "bad_request"},
		{"key line null", copyArgs(d.rmDetachArgs("demo-gh", false, delKeyB), func(m map[string]any) { m["deploy_key"] = nil }), "bad_request"},
		{"key line sent without detach_key", func() map[string]any {
			a := d.rmArgs("demo-gh", false)
			a["deploy_key"] = delKeyB
			return a
		}(), "bad_request"},
		{"detach_key missing", copyArgs(d.rmArgs("demo-gh", false), func(m map[string]any) { delete(m, "detach_key") }), "bad_request"},
		{"detach_key not a bool", copyArgs(d.rmArgs("demo-gh", false), func(m map[string]any) { m["detach_key"] = "true" }), "bad_request"},
		{"detach_key null", copyArgs(d.rmArgs("demo-gh", false), func(m map[string]any) { m["detach_key"] = nil }), "bad_request"},
		{"unknown project", d.rmDetachArgs("ghost", false, delKeyB), "project_unknown"},
	} {
		before := d.snap()
		for i := 0; i < 2; i++ { // twice: the first refusal did not burn the request ID
			d.wantCode("remove_project", c.a, c.code)
		}
		d.noHits(c.name)
		d.mustEqual(before, c.name)
	}
}

// What this proves: detach_key is covered by the signature. Flipping it after
// signing (either way) is a change_mismatch and reaches no provider; a
// signature made with detach_key=false cannot detach and the reverse.
func TestRemoveProjectDetachKeyOutsideSignatureRefused(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	up := d.rmDetachArgs("demo-gh", false, delKeyB)
	up["detach_key"] = false
	delete(up, "deploy_key")
	d.wantCode("remove_project", up, "change_mismatch")
	down := d.rmArgs("demo-gh", false)
	down["detach_key"] = true
	down["deploy_key"] = delKeyB
	d.wantCode("remove_project", down, "change_mismatch")
	d.noHits("tampered detach_key")
	if !d.rowExists("demo-gh") {
		t.Fatal("row removed")
	}
	// the handler key list accepts the unsigned key line only as an argument, never as a signed field
	if !reflect.DeepEqual(removeProjectKeys, []string{"id", "scope", "legacy_ack", "detach_key", "deploy_key", "change", "request", "issued", "signature"}) {
		t.Fatalf("removeProjectKeys %v", removeProjectKeys)
	}
	// an unsigned key line changes nothing the signature covers: the title and body decide
	// (a supplied body that matches nothing while a key carries our title cannot be told from "already gone")
	a := d.rmDetachArgs("demo-gh", false, delKeyA)
	d.wantCode("remove_project", a, "remove_refused")
	if d.gh.deletes() != 0 || !d.rowExists("demo-gh") {
		t.Fatalf("a key was deleted for a body that does not match: %v", d.gh.all())
	}
}

// What this proves: a key DELETE that was refused or whose outcome is unknown
// keeps the registry row, is sent at most once, is never retried, and the
// signature cannot be replayed. Refusals before the DELETE (list/repo reads)
// keep the row too.
func TestDetachFailureKeepsRegistration(t *testing.T) {
	type tc struct {
		name  string
		setup func(d *delEnv)
		code  string
		dels  int
	}
	cases := []tc{
		{"key DELETE 403", func(d *delEnv) { d.gh.keyStatus = 403 }, "remove_refused", 1},
		{"key DELETE 404", func(d *delEnv) { d.gh.keyStatus = 404 }, "remove_refused", 1},
		{"key DELETE 422", func(d *delEnv) { d.gh.keyStatus = 422 }, "remove_refused", 1},
		{"key DELETE 429", func(d *delEnv) { d.gh.keyStatus = 429 }, "provider_rate_limited", 1},
		{"key DELETE 500", func(d *delEnv) { d.gh.keyStatus = 500 }, "delete_partial", 1},
		{"key DELETE 502", func(d *delEnv) { d.gh.keyStatus = 502 }, "delete_partial", 1},
		{"key DELETE 200 instead of 204", func(d *delEnv) { d.gh.keyStatus = 200 }, "delete_partial", 1},
		{"key DELETE redirect", func(d *delEnv) { d.gh.keyStatus = 307 }, "delete_partial", 1},
		{"key DELETE timeout", func(d *delEnv) {
			d.gh.onKeyDel = func() { time.Sleep(1500 * time.Millisecond) }
			d.s.apiSeams = &apiSeams{policy: allowLoopback, total: 300 * time.Millisecond}
		}, "delete_partial", 1},
		{"key list 500", func(d *delEnv) { d.gh.keyListSt = 500 }, "provider_unreachable", 0},
		{"key list 403", func(d *delEnv) { d.gh.keyListSt = 403 }, "github_refused", 0},
		{"repository gone (404)", func(d *delEnv) { d.gh.repoStatus = 404 }, "remote_changed", 0},
		{"repository is another number", func(d *delEnv) { d.gh.repoBody = ghRepoBody(99, "alice/Demo") }, "remote_changed", 0},
		{"repository renamed", func(d *delEnv) { d.gh.repoBody = ghRepoBody(77, "alice/Other") }, "remote_changed", 0},
		{"repository read 500", func(d *delEnv) { d.gh.repoStatus = 500 }, "provider_unreachable", 0},
	}
	for _, c := range cases {
		d := newDel(t)
		w3gKeys(d)
		c.setup(d)
		before := d.proj("demo-gh")
		regBefore := d.regFile()
		a := d.rmDetachArgs("demo-gh", false, delKeyB)
		_, oe := d.do("remove_project", a)
		if oe == nil || oe.Code != c.code {
			t.Fatalf("%s: got %v want %s", c.name, oe, c.code)
		}
		if d.gh.deletes() != c.dels {
			t.Fatalf("%s: %d DELETE requests: %v", c.name, d.gh.deletes(), d.gh.all())
		}
		if !reflect.DeepEqual(before, d.proj("demo-gh")) || string(regBefore) != string(d.regFile()) {
			t.Fatalf("%s: the registry changed although the key is not known to be gone", c.name)
		}
		d.wantCode("remove_project", a, "request_reused")
		if d.gh.deletes() != c.dels {
			t.Fatalf("%s: the replay reached the provider", c.name)
		}
	}
	// the connection dropped while the key DELETE was in flight
	d := newDel(t)
	w3gKeys(d)
	d.gh.keyStatus = 500
	d.gh.onKeyDel = func() {} // (a hijacked drop is covered by the repository DELETE test; a 5xx is the same class)
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "delete_partial")
	if !d.rowExists("demo-gh") {
		t.Fatal("row removed after an uncertain key DELETE")
	}
	// GitLab: a refused and an uncertain key DELETE
	for st, code := range map[int]string{403: "remove_refused", 500: "delete_partial"} {
		d = newDel(t)
		d.gl.keyPages[1] = keysJSON([3]string{"9", "orchd demo-gl", delKeyB})
		d.gl.keyStatus = st
		d.wantCode("remove_project", d.rmDetachArgs("demo-gl", false, delKeyB), code)
		if !d.rowExists("demo-gl") || d.gl.deletes() != 1 {
			t.Fatalf("gitlab %d: %v", st, d.gl.all())
		}
	}
}

// A refused or uncertain detach never ends in the "partial" shape the user
// cannot recover from: after a refusal (nothing deleted) the user can read the
// state and sign again; after an uncertain outcome the NEXT signed request
// reads the key list first, so a key that did go is treated as already gone
// and the row is then removed.
func TestDetachUncertainThenFreshSignatureRecovers(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	d.gh.keyStatus = 500
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "delete_partial")
	// the provider did delete it after all (the answer was lost): the list is now empty of our key
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd other", delKeyA})
	d.gh.keyStatus = 0
	n := d.gh.deletes()
	res, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	if oe != nil {
		t.Fatal(oe.Code)
	}
	if res.(map[string]any)["key_state"] != "already_gone" || d.gh.deletes() != n || d.rowExists("demo-gh") {
		t.Fatalf("%v %v", res, d.gh.all())
	}
}

// What this proves: "already gone" (nothing deleted, row unregistered, answer
// says so) happens ONLY when no listed key has our title or our body. Any
// look-alike (our title with another body, our body under another title,
// title variants) cannot be told from "gone": remove_refused, nothing deleted,
// the row stays.
func TestDetachKeyNotFoundContinues(t *testing.T) {
	for _, list := range []string{
		`[]`,
		keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"3", "my laptop", delKeyC}), // neither our title nor our body
	} {
		d := newDel(t)
		d.gh.keyPages[1] = list
		res, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
		if oe != nil {
			t.Fatalf("%s: %s", list, oe.Code)
		}
		if !reflect.DeepEqual(res, map[string]any{"id": "demo-gh", "removed": true, "key_state": "already_gone"}) {
			t.Fatalf("result %v", res)
		}
		if d.gh.deletes() != 0 || d.rowExists("demo-gh") {
			t.Fatalf("%s: deletes %d, row %v", list, d.gh.deletes(), d.rowExists("demo-gh"))
		}
	}
	for _, list := range []string{
		keysJSON([3]string{"1", "orchd demo-gh", delKeyA}),                                                                                       // our title, other body
		keysJSON([3]string{"1", "orchd other", delKeyA}, [3]string{"3", "my laptop", delKeyB}),                                                   // our body, other title
		keysJSON([3]string{"1", "ORCHD demo-gh", delKeyB}, [3]string{"2", "orchd demo-gh ", delKeyB}, [3]string{"3", "xorchd demo-gh", delKeyB}), // title variants with our body
	} {
		d := newDel(t)
		d.gh.keyPages[1] = list
		d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "remove_refused")
		if d.gh.deletes() != 0 || !d.rowExists("demo-gh") {
			t.Fatalf("%s: a look-alike was treated as gone or deleted: %v", list, d.gh.all())
		}
	}
	// GitLab: only keys that have neither our body nor our title -> already gone; our title with another body -> refused
	d := newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd other", delKeyA}, [3]string{"10", "shared deploy", delKeyC})
	res, oe := d.do("remove_project", d.rmDetachArgs("demo-gl", false, delKeyB))
	if oe != nil || res.(map[string]any)["key_state"] != "already_gone" || d.gl.deletes() != 0 || d.rowExists("demo-gl") {
		t.Fatalf("gitlab other keys only: %v %v %v", res, oe, d.gl.all())
	}
	d = newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"9", "orchd demo-gl", delKeyA})
	d.wantCode("remove_project", d.rmDetachArgs("demo-gl", false, delKeyB), "remove_refused")
	if d.gl.deletes() != 0 || !d.rowExists("demo-gl") {
		t.Fatalf("gitlab look-alike: %v", d.gl.all())
	}
	// a list that may be incomplete is never used: the row stays
	d = newDel(t)
	full := make([][3]string, 100)
	for i := range full {
		full[i] = [3]string{fmt.Sprint(i + 10), "k" + fmt.Sprint(i), delKeyA}
	}
	for p := 1; p <= apiMaxPages; p++ {
		d.gh.keyPages[p] = keysJSON(full...)
	}
	_, oe = d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	if oe == nil || !d.rowExists("demo-gh") || d.gh.deletes() != 0 {
		t.Fatalf("a truncated list let the row go: %v", oe)
	}
	// a malformed entry spoils the whole list
	d = newDel(t)
	d.gh.keyPages[1] = `[{"id":2,"title":"orchd demo-gh","key":"` + delKeyB + `"},{"id":3,"title":"x"}]`
	_, oe = d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	if oe == nil || !d.rowExists("demo-gh") || d.gh.deletes() != 0 {
		t.Fatalf("a malformed list was used: %v", oe)
	}
}

// What this proves: two keys that both carry the title AND the body stop the
// operation (remove_refused), nothing is deleted and the row stays; with the
// same title and different bodies only the one with the supplied body goes.
func TestDetachMultipleOrMismatchedKeyNeverDeleted(t *testing.T) {
	d := newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd demo-gh", delKeyB}, [3]string{"2", "orchd demo-gh", delKeyB + " other-comment"})
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "remove_refused")
	if d.gh.deletes() != 0 || !d.rowExists("demo-gh") {
		t.Fatalf("ambiguous match: %v", d.gh.all())
	}
	d = newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd demo-gl", delKeyB}, [3]string{"9", "orchd demo-gl", delKeyB})
	d.wantCode("remove_project", d.rmDetachArgs("demo-gl", false, delKeyB), "remove_refused")
	if d.gl.deletes() != 0 || !d.rowExists("demo-gl") {
		t.Fatalf("gitlab ambiguous match: %v", d.gl.all())
	}
	// same title, different bodies: the key line tells them apart, exactly one DELETE
	d = newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd demo-gh", delKeyA}, [3]string{"2", "orchd demo-gh", delKeyB}, [3]string{"3", "my laptop", delKeyB})
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "")
	if got := d.gh.all(); got[len(got)-1] != "DELETE /repos/alice/Demo/keys/2" || d.gh.deletes() != 1 {
		t.Fatalf("%v", got)
	}
	// a stale same-titled key with ANOTHER body never goes, and is not "gone" either: refused, row kept
	d = newDel(t)
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd demo-gh", delKeyA})
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "remove_refused")
	if d.gh.deletes() != 0 || !d.rowExists("demo-gh") {
		t.Fatalf("a key with another body was deleted: %v", d.gh.all())
	}
}

// What this proves (GitLab, G-D2): a shared deploy key keeps the title of the
// FIRST project it was created for. On a later project the listing shows
// "orchd <first-id>" with OUR body: it is matched by body, only that key id is
// deleted through this project's route, never globally; the title does not
// matter; two body matches are refused.
func TestDetachGitLabSharedKeyMatchedByBody(t *testing.T) {
	d := newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd first-id", delKeyB}, [3]string{"9", "orchd demo-gl", delKeyA}, [3]string{"10", "shared deploy", delKeyC})
	d.gl.onKeyDel = func() {
		if !d.rowExists("demo-gl") {
			t.Error("unregistered before the key DELETE")
		}
	}
	res, oe := d.do("remove_project", d.rmDetachArgs("demo-gl", false, delKeyB))
	if oe != nil || res.(map[string]any)["key_state"] != "removed" {
		t.Fatalf("%v %v", res, oe)
	}
	if got := d.gl.all(); !reflect.DeepEqual(got, []string{glRepoGetW3g, glKeysPathW3g, "DELETE /api/v4/projects/21/deploy_keys/8"}) {
		t.Fatalf("trace %v", got)
	}
	for _, r := range d.gl.all() {
		if strings.Contains(r, "/api/v4/deploy_keys") {
			t.Fatalf("a global key route was used: %s", r)
		}
	}
	if d.rowExists("demo-gl") {
		t.Fatal("row kept")
	}
	// remove_deploy_key uses the same rule
	d = newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd first-id", delKeyB}, [3]string{"10", "shared deploy", delKeyC})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gl", delKeyB), "")
	if got := d.gl.all(); got[len(got)-1] != "DELETE /api/v4/projects/21/deploy_keys/8" || d.gl.deletes() != 1 {
		t.Fatalf("remove_deploy_key trace %v", got)
	}
	// two keys with our body: refused (any titles)
	d = newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"8", "orchd first-id", delKeyB}, [3]string{"9", "orchd demo-gl", delKeyB})
	d.wantCode("remove_project", d.rmDetachArgs("demo-gl", false, delKeyB), "remove_refused")
	if d.gl.deletes() != 0 || !d.rowExists("demo-gl") {
		t.Fatalf("%v", d.gl.all())
	}
	// remove_deploy_key: nothing at all -> key_not_found; a look-alike -> remove_refused
	d = newDel(t)
	d.gl.keyPages[1] = keysJSON([3]string{"10", "shared deploy", delKeyC})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gl", delKeyB), "key_not_found")
	d.gl.keyPages[1] = keysJSON([3]string{"9", "orchd demo-gl", delKeyA})
	d.wantCode("remove_deploy_key", d.keyArgs("demo-gl", delKeyB), "remove_refused")
	if d.gl.deletes() != 0 {
		t.Fatal("deleted")
	}
}

// One fixed-wording log line per successful removal, with the project id only
// (no path, no key, no secret).
func TestSuccessLogsCarryOnlyTheProjectId(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "")
	d.wantCode("delete_remote_repo", alsoArgs(d, "demo-gl", true), "")
	logs := d.logs.String()
	if strings.Count(logs, "remove_project: removed project demo-gh from the registry") != 1 ||
		strings.Count(logs, "delete_remote_repo: removed the remote repository and the registry row of project demo-gl") != 1 {
		t.Fatalf("logs %q", logs)
	}
	for _, bad := range []string{d.root, delKeyB, "AAAA", ghCanary, c3Canary, "alice/Demo", "team/demo"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("%q in the logs: %q", bad, logs)
		}
	}
	// a refused or uncertain outcome logs no "removed" line
	d = newDel(t)
	w3gKeys(d)
	d.gh.keyStatus = 500
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "delete_partial")
	d.wantCode("delete_remote_repo", d.delArgs("demo-gl", func(f map[string]any) { f["also_local"] = true; f["remote_id"] = "99" }), "remote_changed")
	if strings.Contains(d.logs.String(), "removed project") || strings.Contains(d.logs.String(), "removed the remote") {
		t.Fatalf("a success line for a failure: %q", d.logs.String())
	}
}

// W3f behaviour kept: detach_key=false unregisters only, with no provider
// contact at all, and the answer is exactly the W3f one.
func TestRemoveProjectWithoutDetachUnchanged(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	res, oe := d.do("remove_project", d.rmArgs("demo-gh", false))
	if oe != nil {
		t.Fatal(oe.Code)
	}
	if !reflect.DeepEqual(res, map[string]any{"id": "demo-gh", "removed": true}) {
		t.Fatalf("result %v", res)
	}
	d.noHits("remove_project without detach_key")
	if d.rowExists("demo-gh") {
		t.Fatal("row not removed")
	}
	// rows without a remote and legacy rows: as before
	d.putRaw(Project{ID: "toy", Path: d.root + "/toy", Kind: "legacy", Origin: OriginLegacy})
	d.wantCode("remove_project", d.rmArgs("toy", true), "")
	d.noHits("legacy remove_project")
	// a row whose remote is already deleted can still be unregistered without detaching
	prov := "dgh"
	d.putRaw(Project{ID: "gone", Path: d.root + "/gone", Kind: "registered", Provider: &prov, Origin: OriginCreated, RemoteDeletedAt: strp("2026-11-10T01:02:03Z")})
	d.wantCode("remove_project", d.rmArgs("gone", false), "")
	d.noHits("remote_deleted_at remove_project")
	// the rate limiter is the same one
	d = newDel(t)
	d.s.State.DeleteLocal = NewHourlyLimiter(maxLocalDeletePerHour, time.Now)
	for i := 0; i < maxLocalDeletePerHour; i++ {
		id := fmt.Sprintf("q%d", i)
		d.putRaw(Project{ID: id, Path: d.root + "/" + id, Kind: "registered", Origin: OriginUnknown})
		d.wantCode("remove_project", d.rmArgs(id, false), "")
	}
	d.wantCode("remove_project", d.rmArgs("demo-gh", false), "rate_limited")
}

// The limiter is spent once per op, right before the first external write; at
// the cap nothing is sent and the row stays. A held remote lock is "busy"
// (nothing sent, row kept): two requests cannot detach one remote at once.
func TestDetachRateLimitAndLocks(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	d.s.State.DeleteLocal = NewHourlyLimiter(1, time.Now)
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "")
	if d.s.State.DeleteLocal.Remaining() != 0 {
		t.Fatalf("one detach spent %d slots", 1-d.s.State.DeleteLocal.Remaining())
	}
	d.put("demo-gh", "dgh", "alice/Demo", 77, OriginCreated)
	d.gh.reqs = nil
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "rate_limited")
	if d.gh.deletes() != 0 || !d.rowExists("demo-gh") {
		t.Fatalf("past the cap: %v", d.gh.all())
	}
	// "key already gone" also spends exactly one slot (the registry write is the external write then)
	d = newDel(t)
	d.gh.keyPages[1] = `[]`
	d.s.State.DeleteLocal = NewHourlyLimiter(1, time.Now)
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "")
	if d.s.State.DeleteLocal.Remaining() != 0 {
		t.Fatal("already_gone did not spend the slot")
	}
	// the remote lock
	d = newDel(t)
	w3gKeys(d)
	if !d.s.lockName(remoteLockKey("github", "dgh", "alice/Demo")) {
		t.Fatal("lock")
	}
	d.wantCode("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "busy")
	d.s.unlockName(remoteLockKey("github", "dgh", "alice/Demo"))
	if d.hits() != 0 || !d.rowExists("demo-gh") {
		t.Fatal("a busy remote was touched")
	}
}

// The key was taken off but the registry could not be written: delete_partial,
// the row is kept, the DELETE is not repeated by the broker; a fresh signed
// request finds the key gone and finishes the unregistration.
func TestDetachRegistryWriteFailureIsPartialAndRecoverable(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	d.s.Registry.opts.failAt = func(stage string) error {
		if d.gh.deletes() > 0 && stage != "" {
			return os.ErrPermission
		}
		return nil
	}
	_, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	d.s.Registry.opts.failAt = nil
	if oe == nil || oe.Code != "delete_partial" {
		t.Fatalf("%v", oe)
	}
	if d.gh.deletes() != 1 || !d.rowExists("demo-gh") {
		t.Fatalf("deletes %d row %v", d.gh.deletes(), d.rowExists("demo-gh"))
	}
	d.gh.keyPages[1] = keysJSON([3]string{"1", "orchd other", delKeyA}) // the key is gone now
	res, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	if oe != nil || res.(map[string]any)["key_state"] != "already_gone" || d.rowExists("demo-gh") || d.gh.deletes() != 1 {
		t.Fatalf("recovery: %v %v", res, oe)
	}
}

// A panic after the key DELETE was sent never records a success or a failure
// that did not happen: delete_partial, the row kept, nothing leaks into the
// log. A panic before anything was sent is an internal_error.
func TestDetachPanicNeverRecordsFalseOutcome(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	d.s.Registry.opts.failAt = func(stage string) error {
		if d.gh.deletes() > 0 {
			panic("boom " + ghCanary)
		}
		return nil
	}
	_, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	d.s.Registry.opts.failAt = nil
	if oe == nil || oe.Code != "delete_partial" {
		t.Fatalf("%v", oe)
	}
	if d.gh.deletes() != 1 || !d.rowExists("demo-gh") || strings.Contains(d.logs.String(), "boom") || strings.Contains(d.logs.String(), ghCanary) {
		t.Fatalf("deletes %d row %v logs %q", d.gh.deletes(), d.rowExists("demo-gh"), d.logs.String())
	}
}

// ---------- delete_remote_repo + also_local ----------

func alsoArgs(d *delEnv, id string, also bool) map[string]any {
	return d.delArgs(id, func(f map[string]any) { f["also_local"] = also })
}

// What this proves: with also_local=true the repository DELETE is followed by
// REMOVING the registry row (no remote-null leftover), the answer says
// registry_removed, and the rest of the registry is untouched.
func TestDeleteRemoteAlsoLocalRemovesRow(t *testing.T) {
	d := newDel(t)
	res, oe := d.do("delete_remote_repo", alsoArgs(d, "demo-gh", true))
	if oe != nil {
		t.Fatal(oe.Code)
	}
	m := res.(map[string]any)
	if m["id"] != "demo-gh" || m["remote_state"] != "deleted" || m["registry_removed"] != true || len(m) != 4 {
		t.Fatalf("result %v", m)
	}
	if _, err := time.Parse(orphanTimeLayout, fmt.Sprint(m["remote_deleted_at"])); err != nil {
		t.Fatalf("remote_deleted_at %v", m["remote_deleted_at"])
	}
	if got := d.gh.all(); !reflect.DeepEqual(got, []string{ghRepoGet, "DELETE /repos/alice/Demo"}) {
		t.Fatalf("trace %v", got)
	}
	if d.rowExists("demo-gh") || !d.rowExists("demo-gl") {
		t.Fatal("wrong rows after also_local")
	}
	if len(d.orphans()) != 0 {
		t.Fatalf("orphans %v", d.orphans())
	}
	// a second request: no project, no provider contact
	n := len(d.gh.all())
	d.refusedNoBurn2("delete_remote_repo", alsoArgs(d, "demo-gh", true), "project_unknown")
	if len(d.gh.all()) != n {
		t.Fatal("the provider was contacted for a removed project")
	}
	// GitLab: 202 is "requested", the row goes as well
	res, oe = d.do("delete_remote_repo", alsoArgs(d, "demo-gl", true))
	if oe != nil || res.(map[string]any)["remote_state"] != "requested" || res.(map[string]any)["registry_removed"] != true || d.rowExists("demo-gl") {
		t.Fatalf("gitlab: %v %v", res, oe)
	}
	// refusals and uncertain outcomes keep the row exactly as before (also_local does not weaken them)
	for _, c := range []struct {
		st   int
		code string
	}{{403, "remove_refused"}, {500, "delete_partial"}, {200, "delete_partial"}} {
		d := newDel(t)
		d.gh.delStatus = c.st
		before := d.proj("demo-gh")
		d.wantCode("delete_remote_repo", alsoArgs(d, "demo-gh", true), c.code)
		if !reflect.DeepEqual(before, d.proj("demo-gh")) || d.gh.deletes() != 1 {
			t.Fatalf("%d: row changed or retried", c.st)
		}
	}
	// the safety rules still hold with also_local: linked, unknown and legacy origins and a wrong name are refused first
	for _, origin := range []string{OriginLinked, OriginUnknown, OriginLegacy} {
		d := newDel(t)
		if origin == OriginLegacy {
			legacyWithRemote(d)
		} else {
			d.put("demo-gh", "dgh", "alice/Demo", 77, origin)
		}
		d.refusedNoBurn("delete_remote_repo", alsoArgs(d, "demo-gh", true), map[string]string{OriginLinked: "not_created_by_tool", OriginUnknown: "origin_unknown", OriginLegacy: "origin_unknown"}[origin])
	}
	d = newDel(t)
	d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", func(f map[string]any) { f["also_local"] = true; f["confirm_name"] = "demo" }), "confirm_mismatch")
	d.refusedNoBurn("delete_remote_repo", d.delArgs("demo-gh", func(f map[string]any) { f["also_local"] = true; f["remote_id"] = "78" }), "remote_changed")
}

// A set_flag between the live check and the registry write must not turn a
// finished also_local deletion into delete_partial; a REAL change of the row
// (another remote number) does (and the row is kept for a human).
func TestAlsoLocalSurvivesFlagFlipButNotARealRowChange(t *testing.T) {
	for _, real := range []bool{false, true} {
		d := newDel(t)
		d.gh.delHold = make(chan struct{})
		d.gh.delSeen = make(chan struct{}, 1)
		done := make(chan string, 1)
		go func() {
			_, oe := d.do("delete_remote_repo", alsoArgs(d, "demo-gh", true))
			done <- codeOf(oe)
		}()
		select {
		case <-d.gh.delSeen:
		case <-time.After(5 * time.Second):
			t.Fatal("the delete did not reach the provider")
		}
		if err := d.s.Registry.Update(func(r *Registry) error {
			for i := range r.Projects {
				if r.Projects[i].ID == "demo-gh" {
					if real {
						n := int64(99)
						r.Projects[i].Remote = &RemoteLink{Path: "alice/Demo", ID: &n}
					} else {
						r.Projects[i].AIContext = !r.Projects[i].AIContext
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		close(d.gh.delHold)
		code := <-done
		if real {
			if code != "delete_partial" || !d.rowExists("demo-gh") {
				t.Fatalf("a changed row: %q row %v", code, d.rowExists("demo-gh"))
			}
		} else if code != "" || d.rowExists("demo-gh") {
			t.Fatalf("a flag flip: %q row %v", code, d.rowExists("demo-gh"))
		}
	}
}

// also_local=false is W3f: the row stays with remote null + remote_deleted_at,
// Provider is kept (attach_remote stays blocked), and the answer has no
// registry_removed field.
func TestDeleteRemoteWithoutAlsoLocalKeepsRowAndProvider(t *testing.T) {
	d := newDel(t)
	res, oe := d.do("delete_remote_repo", alsoArgs(d, "demo-gh", false))
	if oe != nil {
		t.Fatal(oe.Code)
	}
	m := res.(map[string]any)
	if _, has := m["registry_removed"]; has || len(m) != 3 || m["remote_state"] != "deleted" {
		t.Fatalf("result %v", m)
	}
	p := d.proj("demo-gh")
	if p.Remote != nil || p.RemoteDeletedAt == nil || p.Provider == nil || *p.Provider != "dgh" || p.Origin != OriginCreated {
		t.Fatalf("row %+v", p)
	}
	d.wantCode("attach_remote", d.attachArgs("demo-gh", "dgh", "alice/existing", nil), "already_linked")
	// the follow-up (a remove_project of the remote-deleted row) still works, without detaching
	d.wantCode("remove_project", d.rmArgs("demo-gh", false), "")
	// and detach on it is refused in the shape stage
	d = newDel(t)
	if _, oe := d.do("delete_remote_repo", alsoArgs(d, "demo-gh", false)); oe != nil {
		t.Fatal(oe.Code)
	}
	n := len(d.gh.all())
	d.refusedNoBurn2("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB), "bad_request")
	if len(d.gh.all()) != n {
		t.Fatal("contacted the provider")
	}
}

// The repository is gone but the row could not be removed: delete_partial,
// the row is KEPT as it was (remote still set: the registry was not written),
// no retry.
func TestAlsoLocalRegistryWriteFailureIsPartial(t *testing.T) {
	d := newDel(t)
	d.s.Registry.opts.failAt = func(stage string) error {
		if d.gh.deletes() > 0 && stage != "" {
			return os.ErrPermission
		}
		return nil
	}
	before := d.proj("demo-gh")
	_, oe := d.do("delete_remote_repo", alsoArgs(d, "demo-gh", true))
	d.s.Registry.opts.failAt = nil
	if oe == nil || oe.Code != "delete_partial" {
		t.Fatalf("%v", oe)
	}
	if d.gh.deletes() != 1 || !reflect.DeepEqual(before, d.proj("demo-gh")) {
		t.Fatalf("deletes %d, row changed", d.gh.deletes())
	}
	// a panic after the DELETE is delete_partial too
	d = newDel(t)
	d.s.Registry.opts.failAt = func(stage string) error {
		if d.gh.deletes() > 0 {
			panic("boom " + ghCanary)
		}
		return nil
	}
	d.wantCode("delete_remote_repo", alsoArgs(d, "demo-gh", true), "delete_partial")
	d.s.Registry.opts.failAt = nil
	if d.gh.deletes() != 1 || !d.rowExists("demo-gh") || strings.Contains(d.logs.String(), ghCanary) {
		t.Fatal("panic handling")
	}
}

// ---------- cross-action signatures ----------

// What this proves: with the detach act added, a signature made for one action
// never works as another over EVERY ordered pair (the W3f matrix plus
// remove_project with detach_key=true), and the same-subject graft of a
// detach_key=false signature onto a detach_key=true request (and back) is a
// change_mismatch. No provider is contacted, nothing changes.
func TestCrossActionSignaturesStillRefusedW3g(t *testing.T) {
	d := prepDel(t)
	acts := append(delActs(), delAct{"remove_project detach", "remove_project", func(d *delEnv) map[string]any {
		return d.rmDetachArgs("demo-gh", false, delKeyB)
	}})
	snapReg, snapTok := d.snap(), d.tokSnap()
	pairs := 0
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
				switch oe.Code {
				case "bad_signature", "change_mismatch", "bad_request", "deploy_key_mismatch":
				default:
					t.Errorf("%s signature on %s (variant %d): code %s", a.name, b.name, i+1, oe.Code)
				}
			}
			pairs++
		}
	}
	if pairs != len(acts)*(len(acts)-1) || pairs < 270 {
		t.Fatalf("pairs %d", pairs)
	}
	d.mustEqual(snapReg, "cross-action signatures")
	if !d.tokSnap().sameStore(snapTok) || !d.tokSnap().sameUsed(snapTok) {
		t.Fatal("the token store or the used-ID file changed")
	}
	d.noHits("cross-action signatures")
	// same subject, same action, the other detach_key value
	off := d.rmArgs("demo-gh", false)
	on := d.rmDetachArgs("demo-gh", false, delKeyB)
	for _, k := range []string{"signature", "change", "request", "issued"} {
		on[k] = off[k]
	}
	d.wantCode("remove_project", on, "change_mismatch")
	on2 := d.rmDetachArgs("demo-gh", false, delKeyB)
	off2 := d.rmArgs("demo-gh", false)
	for _, k := range []string{"signature", "change", "request", "issued"} {
		off2[k] = on2[k]
	}
	d.wantCode("remove_project", off2, "change_mismatch")
	d.noHits("detach_key graft")
	if !d.rowExists("demo-gh") {
		t.Fatal("a grafted signature removed the row")
	}
	// control: the detach act works with its own signature and removes the key
	if _, oe := d.do("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB)); oe != nil {
		t.Fatal(oe.Code)
	}
	if d.gh.deletes() != 1 || d.rowExists("demo-gh") {
		t.Fatalf("control: %v", d.gh.all())
	}
}

// ---------- allowlists, canary, fixed sentences ----------

// W3g adds NO provider request row: the tables are exactly the W3f ones.
func TestNoNewDeleteRowsInAllowlist(t *testing.T) {
	gh := []string{
		"GET ^/user$ q=", "GET ^/user/orgs$ q=per_page,page", "GET ^/repos/" + ghOwner + "/" + ghRepo + "$ q=",
		"POST ^/user/repos$ q=", "POST ^/orgs/" + ghOwner + "/repos$ q=",
		"PUT ^/repos/" + ghOwner + "/" + ghRepo + "/contents/" + ghFile + "$ q=",
		"GET ^/repos/" + ghOwner + "/" + ghRepo + "/branches/" + reBr + "/protection$ q=",
		"PUT ^/repos/" + ghOwner + "/" + ghRepo + "/branches/" + reBr + "/protection$ q=",
		"POST ^/repos/" + ghOwner + "/" + ghRepo + "/keys$ q=",
		"DELETE ^/repos/" + ghOwner + "/" + ghRepo + "$ q=",
		"GET ^/repos/" + ghOwner + "/" + ghRepo + "/keys$ q=per_page,page",
		"DELETE ^/repos/" + ghOwner + "/" + ghRepo + "/keys/" + reID + "$ q=",
	}
	if got := append(rowStrings(githubAllowlist), rowStrings(githubDeleteRows)...); !reflect.DeepEqual(got, gh) {
		t.Fatalf("GitHub rows changed:\n%v", got)
	}
	gl := []string{
		"GET ^/api/v4/groups$ q=min_access_level,order_by,sort,per_page,page,top_level_only", "GET ^/api/v4/groups/" + reGroup + "$ q=",
		"POST ^/api/v4/groups$ q=", "GET ^/api/v4/projects/" + reProj + "$ q=", "POST ^/api/v4/projects$ q=",
		"POST ^/api/v4/projects/" + reID + "/repository/files/" + reFile + "$ q=",
		"GET ^/api/v4/projects/" + reID + "/protected_branches/" + reBr + "$ q=",
		"POST ^/api/v4/projects/" + reID + "/protected_branches$ q=",
		"POST ^/api/v4/projects/" + reID + "/deploy_keys$ q=",
		"DELETE ^/api/v4/projects/" + reID + "$ q=",
		"GET ^/api/v4/projects/" + reID + "/deploy_keys$ q=per_page,page",
		"DELETE ^/api/v4/projects/" + reID + "/deploy_keys/" + reID + "$ q=",
	}
	if got := append(rowStrings(allowlist), rowStrings(gitlabDeleteRows)...); !reflect.DeepEqual(got, gl) {
		t.Fatalf("GitLab rows changed:\n%v", got)
	}
	// the detach flow sends only rows of those tables (every request above passed the allowlist: asserted by the traces)
	if defaultOpTimeouts["remove_project"] < 45*time.Second {
		t.Fatalf("remove_project makes three provider calls: its time limit is %v", defaultOpTimeouts["remove_project"])
	}
}

// The token (and what the provider reflects) never reaches an answer, a reply
// or a log line in any detach / also_local outcome.
func TestCanaryTokenNeverInAnyOutputW3g(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	var outs []string
	for _, st := range []int{403, 500} {
		d.gh.keyStatus = st
		r := d.wire("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
		if r.OK || r.Message != fixedMessages[r.Code] {
			t.Fatalf("%+v", r)
		}
		outs = append(outs, fmt.Sprintf("%+v", r))
	}
	d.gh.keyStatus = 0
	d.gh.keyListSt = 500
	r := d.wire("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	outs = append(outs, fmt.Sprintf("%+v", r))
	d.gh.keyListSt = 0
	d.gh.delStatus = 500
	r = d.wire("delete_remote_repo", alsoArgs(d, "demo-gh", true))
	outs = append(outs, fmt.Sprintf("%+v", r))
	d.gh.delStatus = 0
	r = d.wire("remove_project", d.rmDetachArgs("demo-gh", false, delKeyB))
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	outs = append(outs, fmt.Sprintf("%+v", r))
	r = d.wire("delete_remote_repo", alsoArgs(d, "demo-gl", true))
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	outs = append(outs, fmt.Sprintf("%+v", r), d.logs.String())
	for _, o := range outs {
		for _, bad := range []string{ghCanary, c3Canary, delOtherGH, delOtherGL, "REFLECT"} {
			if strings.Contains(o, bad) {
				t.Fatalf("%q leaked into %q", bad, o)
			}
		}
	}
}

// Concurrent detach requests for one project send at most one key DELETE.
func TestConcurrentDetachSendsOneKeyDelete(t *testing.T) {
	d := newDel(t)
	w3gKeys(d)
	var wg sync.WaitGroup
	codes := make([]string, 4)
	args := make([]map[string]any, 4)
	for i := range args {
		args[i] = d.rmDetachArgs("demo-gh", false, delKeyB)
	}
	for i := range args {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, oe := d.do("remove_project", args[i])
			codes[i] = codeOf(oe)
		}(i)
	}
	wg.Wait()
	if d.gh.deletes() != 1 || d.rowExists("demo-gh") {
		t.Fatalf("deletes %d codes %v", d.gh.deletes(), codes)
	}
	_ = registrymsg.ActionRemoveProject
}
