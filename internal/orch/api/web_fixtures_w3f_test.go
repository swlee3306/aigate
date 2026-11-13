package api

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// TestWebFixturesW3fVectors writes the golden vectors for the W3f delete
// actions (remove_project, delete_remote_repo, remove_deploy_key, clear_orphan)
// from the real Go code. Nothing in the page reads this file yet: it is the
// contract F4's JS must reproduce.
func TestWebFixturesW3fVectors(t *testing.T) {
	type vec struct {
		Name      string         `json:"name"`
		Action    string         `json:"action"`
		Subject   string         `json:"subject"`
		Fields    map[string]any `json:"fields"`
		Canonical string         `json:"canonical,omitempty"`
		Change    string         `json:"change,omitempty"`
	}
	rp := func(over map[string]any) map[string]any {
		m := map[string]any{"scope": "local", "legacy_ack": false, "detach_key": false}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	dr := func(over map[string]any) map[string]any {
		m := map[string]any{"remote_path": "octo/Hello-World", "remote_id": "123456789", "confirm_name": "Hello-World", "also_local": true}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	dk := func(over map[string]any) map[string]any {
		m := map[string]any{"remote_path": "octo/Hello-World"}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	co := func(over map[string]any) map[string]any {
		m := map[string]any{"project": "team/sub/half-made"}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	good := []vec{
		{Name: "remove_project local", Action: "remove_project", Subject: "my-app", Fields: rp(nil)},
		{Name: "remove_project legacy acknowledged", Action: "remove_project", Subject: "toy", Fields: rp(map[string]any{"legacy_ack": true})},
		// W3g (G-D1): detach_key is signed
		{Name: "remove_project detach key", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"detach_key": true})},
		{Name: "remove_project legacy acknowledged detach key", Action: "remove_project", Subject: "toy", Fields: rp(map[string]any{"legacy_ack": true, "detach_key": true})},
		{Name: "delete_remote_repo also local", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(nil)},
		{Name: "delete_remote_repo remote only", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"also_local": false})},
		{Name: "delete_remote_repo gitlab nested", Action: "delete_remote_repo", Subject: "my-app",
			Fields: dr(map[string]any{"remote_path": "team/sub/app-one", "remote_id": "21", "confirm_name": "app-one"})},
		{Name: "delete_remote_repo max id", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"remote_id": "999999999999999"})},
		{Name: "remove_deploy_key", Action: "remove_deploy_key", Subject: "my-app", Fields: dk(nil)},
		{Name: "clear_orphan gitlab", Action: "clear_orphan", Subject: "gl", Fields: co(nil)},
		{Name: "clear_orphan github case kept", Action: "clear_orphan", Subject: "gh", Fields: co(map[string]any{"project": "Octo-Cat/half-made"})},
	}
	for i := range good {
		c, err := registrymsg.CanonicalChange(good[i].Action, good[i].Subject, good[i].Fields)
		if err != nil {
			t.Fatalf("%s: %v", good[i].Name, err)
		}
		h, _ := registrymsg.ChangeHash(good[i].Action, good[i].Subject, good[i].Fields)
		good[i].Canonical, good[i].Change = string(c), h
	}
	refused := []vec{
		{Name: "remove_project scope remote", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"scope": "remote"})},
		{Name: "remove_project scope upper", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"scope": "Local"})},
		{Name: "remove_project legacy_ack string", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"legacy_ack": "true"})},
		{Name: "remove_project without legacy_ack", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"legacy_ack": nil})},
		{Name: "remove_project detach_key string", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"detach_key": "true"})},
		{Name: "remove_project without detach_key", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"detach_key": nil})},
		{Name: "remove_project extra field", Action: "remove_project", Subject: "my-app", Fields: rp(map[string]any{"also_local": true})},
		{Name: "remove_project subject upper", Action: "remove_project", Subject: "My-App", Fields: rp(nil)},
		{Name: "remove_project subject empty", Action: "remove_project", Subject: "", Fields: rp(nil)},
		{Name: "delete_remote_repo id zero", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"remote_id": "0"})},
		{Name: "delete_remote_repo id leading zero", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"remote_id": "012"})},
		{Name: "delete_remote_repo id sixteen digits", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"remote_id": "1234567890123456"})},
		{Name: "delete_remote_repo id signed", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"remote_id": "-5"})},
		{Name: "delete_remote_repo id number", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"remote_id": true})},
		{Name: "delete_remote_repo confirm_name empty", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"confirm_name": ""})},
		{Name: "delete_remote_repo confirm_name slash", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"confirm_name": "octo/Hello-World"})},
		{Name: "delete_remote_repo confirm_name dot git", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"confirm_name": "repo.git"})},
		{Name: "delete_remote_repo confirm_name unicode", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"confirm_name": "저장소"})},
		{Name: "delete_remote_repo remote_path one segment", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"remote_path": "solo"})},
		{Name: "delete_remote_repo also_local string", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"also_local": "yes"})},
		{Name: "delete_remote_repo without also_local", Action: "delete_remote_repo", Subject: "my-app", Fields: dr(map[string]any{"also_local": nil})},
		{Name: "remove_deploy_key remote_path dotdot", Action: "remove_deploy_key", Subject: "my-app", Fields: dk(map[string]any{"remote_path": "a/../b"})},
		{Name: "remove_deploy_key with remote_id", Action: "remove_deploy_key", Subject: "my-app", Fields: dk(map[string]any{"remote_id": "5"})},
		{Name: "clear_orphan project dotdot", Action: "clear_orphan", Subject: "gl", Fields: co(map[string]any{"project": "o/../x"})},
		{Name: "clear_orphan project one segment", Action: "clear_orphan", Subject: "gl", Fields: co(map[string]any{"project": "solo"})},
		{Name: "clear_orphan project percent", Action: "clear_orphan", Subject: "gl", Fields: co(map[string]any{"project": "o/r%2f"})},
		{Name: "clear_orphan subject is a project id with underscore", Action: "clear_orphan", Subject: "a_b", Fields: co(nil)},
	}
	for i := range refused {
		if _, err := registrymsg.CanonicalChange(refused[i].Action, refused[i].Subject, refused[i].Fields); err == nil {
			t.Fatalf("Go accepts %q", refused[i].Name)
		}
		for k, v := range refused[i].Fields { // nil entries in the override maps mean "absent"
			if v == nil {
				delete(refused[i].Fields, k)
			}
		}
	}
	issued := time.Date(2026, 11, 12, 10, 4, 5, 0, time.UTC)
	request := "00112233445566778899aabbccddeeff"
	type msgVec struct {
		Name    string `json:"name"`
		Action  string `json:"action"`
		Subject string `json:"subject"`
		Change  string `json:"change"`
		Request string `json:"request"`
		Issued  string `json:"issued"`
		Text    string `json:"text"`
		Digest  string `json:"digest"`
	}
	var messages []msgVec
	for _, g := range good {
		m := signing.RegistryMessage{Action: g.Action, Subject: g.Subject, Change: g.Change, Request: request, Issued: issued}
		if err := m.Validate(); err != nil {
			t.Fatalf("%s: %v", g.Name, err)
		}
		d := m.Digest()
		messages = append(messages, msgVec{Name: g.Name, Action: m.Action, Subject: m.Subject, Change: m.Change, Request: request,
			Issued: issued.Format(registryTimeFormat), Text: m.Text(), Digest: hex.EncodeToString(d[:])})
	}
	// the same fields and subject under every action name never share a canonical text
	seen := map[string]string{}
	for _, g := range good {
		if prev, dup := seen[g.Change]; dup {
			t.Fatalf("%s and %s share a change hash", prev, g.Name)
		}
		seen[g.Change] = g.Name
	}
	type strOK struct {
		Value string `json:"value"`
		OK    bool   `json:"ok"`
	}
	check := func(f func(string) bool, vals ...string) []strOK {
		var out []strOK
		for _, v := range vals {
			out = append(out, strOK{Value: v, OK: f(v)})
		}
		return out
	}
	remoteIDs := check(registrymsg.ValidRemoteID, "1", "42", "123456789012345", "1234567890123456", "0", "00", "01", "-1", "+1", "1.0", "1e3", " 1", "1 ", "", "abc", "１２")
	confirmNames := check(registrymsg.ValidConfirmName, "Hello-World", "my_repo", "Repo.js", "a", "x.gitx", "UPPER", "", ".", "..", "a/b", "x.git", "x.GIT", "a b", "é",
		strings.Repeat("a", 100), strings.Repeat("a", 101))
	orphanProjects := check(registrymsg.ValidOrphanProject, "team/half", "Octo-Cat/half-made", "a/b/c/d/e", "grp/sub/proj", "solo", "o/../x", "o/r%2f", "o/r.git",
		"a/b/c/d/e/f", "o/r s", "/a/b", "a/b/")
	for _, set := range [][]strOK{remoteIDs, confirmNames, orphanProjects} {
		sawOK, sawNo := false, false
		for _, s := range set {
			sawOK, sawNo = sawOK || s.OK, sawNo || !s.OK
		}
		if !sawOK || !sawNo {
			t.Fatal("every validator vector set needs accepted and refused values")
		}
	}
	checkFixture(t, "w3f-vectors.json", map[string]any{
		"good": good, "refused": refused, "messages": messages,
		"remote_ids": remoteIDs, "confirm_names": confirmNames, "orphan_projects": orphanProjects,
	})
}
