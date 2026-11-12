package api

import "testing"

// E1: registrymsg now accepts github providers and a visibility field, but this
// console has no GitHub flow yet. propose must refuse them (no GitLab-worded
// preview of a GitHub change) and never reach the broker; E3 lifts this.
func TestE1ProposeRefusesGitHubKindAndVisibilityUntilE3(t *testing.T) {
	e := newW3cEnv(t)
	post := func(path string, body any) (int, []byte) { return e.sockJSON(t, "POST", path, jsonBody(body)) }
	_, f := e.fieldsFor("create_project")
	for _, vis := range []string{"private", "public"} {
		m := map[string]any{}
		for k, v := range f {
			m[k] = v
		}
		m["visibility"] = vis
		if code, b := post("/api/v1/projects/create/propose", map[string]any{"action": "create_project", "subject": "app-one", "fields": m}); code != 400 {
			t.Errorf("create_project with visibility %s: %d %s", vis, code, b)
		}
	}
	gh := map[string]any{"kind": "github", "base_url": "https://api.github.com", "ssh_url": "ssh://git@github.com", "token_ref": "github-main"}
	if code, b := post("/api/v1/providers/propose", map[string]any{"action": "set_provider", "subject": "gh", "fields": gh}); code != 400 {
		t.Errorf("github set_provider: %d %s", code, b)
	}
	// the new actions have no endpoint here yet
	for _, path := range []string{"/api/v1/providers/propose", "/api/v1/projects/create/propose", "/api/v1/projects/propose"} {
		for _, action := range []string{"link_project", "attach_remote"} {
			body := map[string]any{"action": action, "subject": "app-one", "fields": map[string]any{"provider": "gh", "remote_path": "octo/hello", "publish": "read", "deploy_key_sha256": "ab"}}
			if code, b := post(path, body); code != 400 {
				t.Errorf("%s %s: %d %s", path, action, code, b)
			}
		}
	}
	if ops := e.wb.allOps(); len(ops) != 0 {
		t.Fatalf("broker called: %v", ops)
	}
}
