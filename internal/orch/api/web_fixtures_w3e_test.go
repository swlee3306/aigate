package api

import (
	"testing"

	"aigate/internal/orch/broker"
)

// TestWebFixturesW3e pins the JSON of the W3e answers the console reads (GitHub
// provider and owner rows, a GitHub create, link and attach answers, the
// link_partial envelope, the registry with GitHub projects and both kinds of
// orphan). Every fixture is built from the real Go types.
func TestWebFixturesW3e(t *testing.T) {
	yes, no := true, false
	gl := ProviderView{ID: "gl", Kind: "gitlab", BaseURL: "http://gitlab.example:18082", SSHURL: "ssh://git@gitlab.example", TokenRef: "gl-admin", InUse: &yes}
	gh := ProviderView{ID: "gh", Kind: "github", BaseURL: "https://api.github.com", SSHURL: "ssh://git@github.com", TokenRef: "github-main", InUse: &no}
	ghUsed := gh
	ghUsed.InUse = &yes
	checkFixture(t, "providers-w3e.json", map[string]any{"providers": []ProviderView{gl, gh}})
	checkFixture(t, "provider-owners-github.json", map[string]any{"groups": []GroupView{
		{Path: "SampleUser", Name: "SampleUser", Kind: "user"}, {Path: "acme-org", Name: "acme-org", Kind: "org"}}, "truncated": false})
	ghID := gh.ID
	created := ProjectView{ID: "hello", Path: "/home/orch/repos/hello", Kind: "registered", Provider: &ghID,
		GitLab: &projectGitLab{Project: "SampleUser/hello"}, Cloned: true}
	checkFixture(t, "project-create-github-ok.json", map[string]any{"project": created, "clone": CloneResult{Status: "cloned", Code: "ok"}})
	linked := created
	linked.ID, linked.Path, linked.GitLab = "hello-world", "/home/orch/repos/hello-world", &projectGitLab{Project: "octo/Hello-World"}
	checkFixture(t, "project-link-ok.json", map[string]any{"project": linked, "clone": CloneResult{Status: "cloned", Code: "ok"}})
	local := ProjectView{ID: "plain-app", Path: "/home/orch/repos/plain-app", Kind: "registered", Cloned: true}
	attached := local
	attached.Provider, attached.GitLab = &ghID, &projectGitLab{Project: "SampleUser/plain-app"}
	checkFixture(t, "project-attach-ok.json", map[string]any{"project": attached})
	checkFixture(t, "project-attach-key-failed.json", map[string]any{"project": attached, "key": map[string]string{"status": "failed", "code": errKeyUnavailable.v1Code}})
	pid := int64(7)
	lp := linkPartialError(&broker.PartialDetail{Provider: "gh", Project: "octo/Hello-World", Step: "deploy_key", Recorded: true, Origin: "linked", OriginGiven: true})
	checkFixture(t, "project-link-partial.json", map[string]any{"error": map[string]any{"code": lp.v1Code, "message": lp.msg, "detail": lp.detail}})
	cp := partialError(&broker.PartialDetail{Provider: "gh", Project: "SampleUser/hello", ProjectID: &pid, Step: "protect", Recorded: true, Origin: "created", OriginGiven: true})
	checkFixture(t, "project-create-github-partial.json", map[string]any{"error": map[string]any{"code": cp.v1Code, "message": cp.msg, "detail": cp.detail}})
	checkFixture(t, "registry-w3e.json", map[string]any{
		"projects":  []ProjectView{linked, local, {ID: "app-one", Path: "/home/orch/repos/app-one", Kind: "registered", Provider: &gl.ID, GitLab: &projectGitLab{Project: "team/app-one", ProjectID: &pid}, Cloned: true}},
		"providers": []ProviderView{gl, ghUsed},
		"orphans": []OrphanView{
			{Provider: "gh", Project: "SampleUser/half-made", Step: "protect", At: "2026-11-12T12:00:00Z", Origin: "created"},
			{Provider: "gh", Project: "octo/Existing-Repo", Step: "deploy_key", At: "2026-11-12T13:00:00Z", Origin: "linked"}},
		"auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5},
	})
}
