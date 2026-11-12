package clone

import (
	"path/filepath"
	"testing"
)

// W3f F1b: for a linked repository the project id (key folder, path) and the repository name are
// unrelated. The remote is the registry path, the key follows the id.
func TestResolverBuildsRemoteFromRegistryPathNotId(t *testing.T) {
	lnk := func(p map[string]any) map[string]any { p["origin"] = "linked"; return p }
	f := startRegistry(t, ghView(
		lnk(proj("sulee-portfolio", "/r/sulee-portfolio", "gh", "swlee3306/Sulee_Portfolio")),
		lnk(proj("site", "/r/site", "gh", "swlee3306/Repo.js")),
		proj("unknown-origin", "/r/unknown-origin", "gh", "swlee3306/Other_Name"), // origin missing = not "created"
		lnk(proj("glname", "/r/glname", "gl", "team/real-name")),
	))
	r, _, keysDir := ghFix(t, f, "sulee-portfolio", "site", "unknown-origin")
	for path, want := range map[string]struct{ remote, key string }{
		"/r/sulee-portfolio": {"ssh://git@github.com/swlee3306/Sulee_Portfolio.git", filepath.Join(keysDir, "sulee-portfolio", "id_ed25519")},
		"/r/site":            {"ssh://git@github.com/swlee3306/Repo.js.git", filepath.Join(keysDir, "site", "id_ed25519")},
		"/r/unknown-origin":  {"ssh://git@github.com/swlee3306/Other_Name.git", filepath.Join(keysDir, "unknown-origin", "id_ed25519")},
		"/r/glname":          {"ssh://git@gitlab.example/team/real-name.git", ""},
	} {
		remote, key, ok := r.Resolve(path)
		if want.key == "" {
			want.key = r.SharedKey
		}
		if !ok || remote != want.remote || key != want.key {
			t.Errorf("%s: %q %q %v, want %q %q", path, remote, key, ok, want.remote, want.key)
		}
	}
	// the key follows the id even when it is named like the repository: no key under the repository name
	if _, key, _ := r.Resolve("/r/sulee-portfolio"); filepath.Base(filepath.Dir(key)) == "Sulee_Portfolio" {
		t.Fatal("key folder named after the repository")
	}
}

// A project this tool created is named by its id: a registry row that says otherwise is refused, in the
// resolver as before (defence in depth), for both providers; linked rows are not subject to it.
func TestCreatedOriginRowWithMismatchedNameRefusedByResolver(t *testing.T) {
	f := startRegistry(t, ghView(
		created(proj("made-gh", "/r/made-gh", "gh", "Octo/other-repo")),
		created(proj("made-gl", "/r/made-gl", "gl", "team/other-repo")),
		created(proj("made-ok", "/r/made-ok", "gh", "Octo/Made-OK")), // GitHub case folding still matches
	))
	r, _, keysDir := ghFix(t, f, "made-gh", "made-ok")
	for _, repo := range []string{"/r/made-gh", "/r/made-gl"} {
		if remote, key, ok := r.Resolve(repo); !ok || remote != "" || key != "" {
			t.Errorf("%s: %q %q %v (want refuse)", repo, remote, key, ok)
		}
	}
	if remote, key, ok := r.Resolve("/r/made-ok"); !ok || remote != "ssh://git@github.com/Octo/Made-OK.git" || key != filepath.Join(keysDir, "made-ok", "id_ed25519") {
		t.Errorf("control: %q %q %v", remote, key, ok)
	}
}
