package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/broker"
)

// W3g (G4): adversarial tests for orchd's side of the detach-with-removal and
// the one-signature also_local. Only properties the G2 tests imply but never
// run over the whole matrix are here; the rest is mapped in docs/31.

// Local files go ONLY after the broker's ok answer that matches what was
// signed. Every signed mode (remove_project with and without detach_key,
// delete_remote_repo with and without also_local) is answered with the ok
// answer of EVERY mode, with answers about another project, and with the
// refusal codes that mean "maybe something happened". Only the diagonal (the
// answer of the mode that was signed) removes anything, and then exactly the
// project's clone and key folder; a bystander project's files are byte-for-byte
// unchanged in every case; an off-diagonal answer is never a ledger success.
func TestLocalFilesOnlyAfterTheAnswerThatMatchesTheSignatureW3g(t *testing.T) {
	e := newDelEnv(t)
	path := e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	keyDir := filepath.Join(e.keysDir(), "app-one")
	other := e.addDel(t, delProject{id: "app-two", origin: "created", prov: "gh", remote: "Octo-Cat/app-two", remoteID: 124, withKey: true})
	otherKey := filepath.Join(e.keysDir(), "app-two")
	bystander := fileTree(t, other, otherKey)
	restore := func() {
		if !exists(path) {
			if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(path, "README.md"), "hello")
		}
		if !exists(keyDir) {
			e.addKeyDir(t, "app-one")
		}
	}
	ok := func(v string) string { return `{"ok":true,"result":` + v + `}` }
	answers := map[string]string{
		"rp detach":      ok(`{"id":"app-one","removed":true,"key_state":"removed"}`),
		"rp detach gone": ok(`{"id":"app-one","removed":true,"key_state":"already_gone"}`),
		"rp plain":       ok(`{"id":"app-one","removed":true}`),
		"dr also":        ok(`{"id":"app-one","remote_state":"deleted","remote_deleted_at":"` + delStamp + `","registry_removed":true}`),
		"dr only":        ok(`{"id":"app-one","remote_state":"deleted","remote_deleted_at":"` + delStamp + `"}`),
		"rp other id":    ok(`{"id":"app-two","removed":true,"key_state":"removed"}`),
		"dr other id":    ok(`{"id":"app-two","remote_state":"deleted","remote_deleted_at":"` + delStamp + `","registry_removed":true}`),
		// members of both ops at once: orchd reads only the members of the op it asked for, so this is the
		// detach answer for a detach and the also_local answer for an also_local (it contradicts nothing
		// signed there), and a contradiction for the plain removal (key_state) and the remote-only deletion
		"both members":   ok(`{"id":"app-one","removed":true,"key_state":"removed","registry_removed":true,"remote_state":"deleted","remote_deleted_at":"` + delStamp + `"}`),
		"delete_partial": errReply("delete_partial"),
		"remove_refused": errReply("remove_refused"),
		"request_reused": errReply("request_reused"),
		"internal_error": errReply("internal_error"),
	}
	type mode struct {
		name, action string
		fields       map[string]any
		ownAnswers   []string // answers that are this mode's ok answer
		removes      bool     // the ok answer removes the local files
	}
	modes := []mode{
		{"remove_project detach", "remove_project", rmDetach(true), []string{"rp detach", "rp detach gone", "both members"}, true},
		{"remove_project plain", "remove_project", rmDetach(false), []string{"rp plain"}, true},
		{"delete_remote_repo also_local", "delete_remote_repo", drFields("Octo-Cat/app-one", 123, "app-one", true), []string{"dr also", "both members"}, true},
		{"delete_remote_repo only", "delete_remote_repo", drFields("Octo-Cat/app-one", 123, "app-one", false), []string{"dr only"}, false},
	}
	cases := 0
	for _, m := range modes {
		for name, answer := range answers {
			restore()
			e.resetDeleteLimits()
			answer := answer
			e.setOverride(func(op string, a json.RawMessage) string {
				if op == m.action {
					return answer
				}
				return ""
			})
			before := fileTree(t, path, keyDir)
			okBefore := 0
			for _, ev := range e.delLedger(t) {
				if ev["ok"] == true {
					okBefore++
				}
			}
			p := e.mustPropose(t, m.action, "app-one", m.fields)
			c, b := e.applyDel(t, p)
			own := false
			for _, o := range m.ownAnswers {
				own = own || o == name
			}
			okAfter := 0
			for _, ev := range e.delLedger(t) {
				if ev["ok"] == true {
					okAfter++
				}
			}
			switch {
			case own:
				if c != 200 {
					t.Errorf("%s / %s: %d %s", m.name, name, c, b)
				}
				if m.removes == (exists(path) || exists(keyDir)) {
					t.Errorf("%s / %s: removes=%v but clone %v key %v", m.name, name, m.removes, exists(path), exists(keyDir))
				}
			default:
				if c == 200 {
					t.Errorf("%s / %s: an answer to something else was taken as done: %s", m.name, name, b)
				}
				if after := fileTree(t, path, keyDir); after != before {
					t.Errorf("%s / %s: local files changed on a non-matching answer", m.name, name)
				}
				if okAfter != okBefore {
					t.Errorf("%s / %s: a ledger success for a non-matching answer", m.name, name)
				}
			}
			if fileTree(t, other, otherKey) != bystander {
				t.Fatalf("%s / %s: the bystander project's files changed", m.name, name)
			}
			cases++
		}
	}
	if cases != len(modes)*len(answers) || cases < 40 {
		t.Fatalf("cases %d", cases)
	}
}

// The per-actor hourly slots of a detaching removal (the local-removal bucket
// AND the key-removal bucket, G-D5) come back only when the request provably
// did nothing: it never reached the broker, or the broker answered busy or
// forbidden. Every outcome where something may have happened (a refusal after
// the signature was checked, an unknown or partial outcome, a reused request,
// a version mismatch, an ok answer orchd does not trust, success) spends BOTH.
// A removal without detach never touches the key bucket.
func TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "app-one", origin: "created", prov: "gh", remote: "Octo-Cat/app-one", remoteID: 123, withKey: true})
	h := e.d.handler
	type outcome struct {
		name   string
		reply  string // "" with down: the broker cannot be reached
		down   bool
		refund bool
	}
	outcomes := []outcome{
		{"broker down", "", true, true},
		{"busy", errReply("busy"), false, true},
		{"forbidden", errReply("forbidden"), false, true},
		{"remove_refused", errReply("remove_refused"), false, false},
		{"delete_partial", errReply("delete_partial"), false, false},
		{"request_reused", errReply("request_reused"), false, false},
		{"bad_request (W3f broker)", errReply("bad_request"), false, false},
		{"unknown_op (W3f broker)", errReply("unknown_op"), false, false},
		{"rate_limited at the broker", errReply("rate_limited"), false, false},
		{"untrusted ok answer", "untrusted", false, false},
	}
	live := h.broker
	for _, o := range outcomes {
		for _, detach := range []bool{true, false} {
			e.resetDeleteLimits()
			h.broker = live
			e.setOverride(func(op string, a json.RawMessage) string {
				if op != "remove_project" {
					return ""
				}
				if o.reply == "untrusted" { // the answer of the other detach value
					if detach {
						return `{"ok":true,"result":{"id":"app-one","removed":true}}`
					}
					return `{"ok":true,"result":{"id":"app-one","removed":true,"key_state":"removed"}}`
				}
				return o.reply
			})
			p := e.mustPropose(t, "remove_project", "app-one", rmDetach(detach))
			h.removeLimit, h.removeKeyLimit = newActorLimiter(100, time.Hour), newActorLimiter(100, time.Hour)
			if o.down {
				h.broker = &broker.Client{Socket: filepath.Join(shortDir(t), "none.sock")}
			}
			c, b := e.applyDel(t, p)
			h.broker = live
			if c == 200 {
				t.Fatalf("%s: %d %s", o.name, c, b)
			}
			wantLocal, wantKey := 1, 0
			if detach {
				wantKey = 1
			}
			if o.refund {
				wantLocal, wantKey = 0, 0
			}
			if got := h.removeLimit.total(); got != wantLocal {
				t.Errorf("%s detach=%v: local bucket holds %d, want %d", o.name, detach, got, wantLocal)
			}
			if got := h.removeKeyLimit.total(); got != wantKey {
				t.Errorf("%s detach=%v: key bucket holds %d, want %d", o.name, detach, got, wantKey)
			}
		}
	}
	// the key bucket alone is full: the local slot taken first is handed back (no half-spent request)
	e.setOverride(nil)
	e.resetDeleteLimits()
	p := e.mustPropose(t, "remove_project", "app-one", rmDetach(true))
	h.removeLimit, h.removeKeyLimit = newActorLimiter(100, time.Hour), newActorLimiter(0, time.Hour)
	n := len(e.writes())
	if c, b := e.applyDel(t, p); c != 429 || v1Code(b) != "project_rate_limited" {
		t.Fatalf("key bucket full: %d %s", c, b)
	}
	if h.removeLimit.total() != 0 || len(e.writes()) != n {
		t.Fatalf("a refused request kept a local slot (%d) or reached the broker", h.removeLimit.total())
	}
}

// The GitLab shared key's .pub is the line orchd sends for a GitLab detach.
// A comment with spaces is outside the broker's key-line rule, so the line is
// unavailable: remove_project with detach_key is refused BEFORE any signature
// (key_unavailable_for_removal), the removal preview does not offer the option,
// nothing reaches the broker; turning the option off still works. A one-word
// comment is fine. (Runbook 17.0: check the server's shared .pub first.)
func TestSharedKeyLineWithASpacedCommentIsUnavailableBeforeSignatureW3g(t *testing.T) {
	e := newDelEnv(t)
	e.addDel(t, delProject{id: "gl-one", origin: "created", prov: "gl", remote: "team/gl-one", remoteID: 23})
	parts := strings.Fields(e.pubLine)
	for _, c := range []struct {
		line  string
		avail bool
	}{
		{parts[0] + " " + parts[1] + " orchd deploy key", false},
		{parts[0] + " " + parts[1] + " two\twords", false},
		{parts[0] + " " + parts[1] + " orchd@deploy", true},
		{parts[0] + " " + parts[1], true},
	} {
		writeFile(t, e.key+".pub", c.line+"\n")
		// W3h: the row holds the hash of the line it was registered with; this
		// test is about the line's shape, so each case is its own registered line
		e.setKeySHA(t, "gl-one", keyHash(c.line))
		e.resetDeleteLimits()
		code, b := e.sockJSON(t, "GET", "/api/v1/projects/gl-one/removal-preview", "")
		var pv map[string]any
		_ = json.Unmarshal(b, &pv)
		if code != 200 || pv["detach_key_available"] != c.avail {
			t.Errorf("%q: preview %d %s", c.line, code, b)
		}
		code, b = e.proposeDel(t, "remove_project", "gl-one", rmDetach(true))
		if c.avail != (code == 200) || (!c.avail && (code != 409 || v1Code(b) != "key_unavailable_for_removal")) {
			t.Errorf("%q: propose %d %s", c.line, code, b)
		}
		if code, b := e.proposeDel(t, "remove_project", "gl-one", rmDetach(false)); code != 200 {
			t.Errorf("%q: detach off %d %s", c.line, code, b)
		}
	}
	if n := len(e.writes()); n != 0 {
		t.Fatalf("the broker was asked %d times", n)
	}
}
