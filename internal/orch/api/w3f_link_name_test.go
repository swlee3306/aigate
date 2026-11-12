package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addProjectOrigin is addProject with a registry origin (v4).
func (e *w3cEnv) addProjectOrigin(id, prov, remote, origin string) {
	e.addProject(id, prov, remote)
	e.wb.mu.Lock()
	e.wb.projects[len(e.wb.projects)-1]["origin"] = origin
	e.wb.mu.Unlock()
}

// W3f F1b: the project id (folder, keys/<id>, registry id) is the user's choice and
// independent of the repository name.
func TestLinkAllowsProjectIdDifferentFromRepoName(t *testing.T) {
	e := ghEnv(t)
	f := ghLinkFields()
	f["remote_path"] = "swlee3306/Sulee_Portfolio"
	lp := e.propose(t, "link_project", "sulee-portfolio", f)
	if lp.Subject != "sulee-portfolio" || lp.Fields["remote_path"] != "swlee3306/Sulee_Portfolio" {
		t.Fatalf("%+v", lp)
	}
	if c, b := e.apply(t, lp); c != 200 {
		t.Fatalf("link: %d %s", c, b)
	}
	if args := lastCall(t, e, "link_project"); args["id"] != "sulee-portfolio" || args["remote_path"] != "swlee3306/Sulee_Portfolio" {
		t.Fatalf("broker args %v", args)
	}
	// the key folder follows the id, the remote follows the registry path
	if _, err := os.Lstat(e.promoted("sulee-portfolio")); err != nil {
		t.Fatalf("key not under the project id: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(e.keysDir(), "Sulee_Portfolio")); err == nil {
		t.Fatal("a key folder named after the repository")
	}
	// attach: the existing project keeps its id, the repository has another name
	e.addProject("plain-one", "", "")
	ap := e.propose(t, "attach_remote", "plain-one", map[string]any{"provider": "gh", "remote_path": "swlee3306/Other_Repo.js", "publish": "read"})
	if c, b := e.apply(t, ap); c != 200 {
		t.Fatalf("attach: %d %s", c, b)
	}
}

func TestLinkRepoNameWithUnderscoreAndDotAccepted(t *testing.T) {
	for _, repo := range []string{"swlee3306/sulee_portfolio", "swlee3306/Repo.js", "swlee3306/a.b_c-D", "swlee3306/9lives", "swlee3306/X"} {
		e := ghEnv(t)
		f := ghLinkFields()
		f["remote_path"] = repo
		c, b := e.proposeRaw(t, "link_project", "my-project", f)
		if c != 200 {
			t.Errorf("%s: %d %s", repo, c, b)
		}
	}
	// the provider's own path rule still decides: GitLab keeps its lowercase name rule, a bad id is still refused
	e := ghEnv(t)
	for _, tc := range []struct {
		name, id, prov, path string
	}{
		{"gitlab upper", "my-project", "gl", "team/Sulee_Portfolio"},
		{"gitlab underscore", "my-project", "gl", "team/sulee_portfolio"},
		{"github three segments", "my-project", "gh", "a/b/c"},
		{"github .git", "my-project", "gh", "o/r.git"},
		{"id upper", "My-Project", "gh", "o/r"},
		{"id underscore", "my_project", "gh", "o/r"},
		{"id digit first", "9lives", "gh", "o/r"},
		{"id one char", "a", "gh", "o/r"},
		{"id too long", strings.Repeat("a", 41), "gh", "o/r"},
	} {
		f := ghLinkFields()
		f["provider"], f["remote_path"] = tc.prov, tc.path
		if c, b := e.proposeRaw(t, "link_project", tc.id, f); c != 400 || v1Code(b) != "bad_request" {
			t.Errorf("%s: %d %s", tc.name, c, b)
		}
	}
}

func TestRemoteNameMismatchCodeGone(t *testing.T) {
	e := ghEnv(t)
	for _, tc := range []struct {
		action, sub string
		f           map[string]any
	}{
		{"link_project", "other-name", ghLinkFields()},
		{"attach_remote", "att-two", map[string]any{"provider": "gh", "remote_path": "Octo-Cat/else", "publish": "write"}},
	} {
		if tc.action == "attach_remote" {
			e.addProject(tc.sub, "", "")
		}
		if c, b := e.proposeRaw(t, tc.action, tc.sub, tc.f); c != 200 || strings.Contains(string(b), "remote_name_mismatch") {
			t.Errorf("%s: %d %s", tc.action, c, b)
		}
	}
	// no source file of the module mentions the code any more
	for _, dir := range []string{".", "../clone", "../web/static", "../../broker"} {
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || strings.HasSuffix(p, "_test.go") || strings.Contains(p, "/test/") {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), "remote_name_mismatch") {
				t.Errorf("%s still mentions remote_name_mismatch", p)
			}
			return nil
		})
	}
}

func TestCloneDestIsProjectIdNotRepoName(t *testing.T) {
	e := ghEnv(t)
	e.addProjectOrigin("sulee-portfolio", "gh", "swlee3306/Sulee_Portfolio", "linked")
	priv := e.promoted("sulee-portfolio")
	if err := os.MkdirAll(filepath.Dir(priv), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, priv, "k")
	os.Chmod(priv, 0o600)
	if c, b := e.sockJSON(t, "POST", "/api/v1/projects/sulee-portfolio/clone", ""); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	cl := e.cloneCalls()
	if len(cl) != 1 || cl[0].Dest != filepath.Join(e.root, "sulee-portfolio") || cl[0].SSHKey != priv ||
		cl[0].Remote != "ssh://git@github.com/swlee3306/Sulee_Portfolio.git" || cl[0].Kind != "github" {
		t.Fatalf("%+v", cl)
	}
	// a registry row whose folder is not the id (a hand-edited registry) is still refused
	e.addProjectOrigin("elsewhere", "gh", "swlee3306/Elsewhere", "linked")
	e.wb.mu.Lock()
	e.wb.projects[len(e.wb.projects)-1]["path"] = filepath.Join(e.root, "not-the-id")
	e.wb.mu.Unlock()
	if c, b := e.sockJSON(t, "POST", "/api/v1/projects/elsewhere/clone", ""); c != 400 || v1Code(b) != "not_clonable" || len(e.cloneCalls()) != 1 {
		t.Fatalf("folder differs from id: %d %s", c, b)
	}
}

// create_project is unchanged: the tool names the repository, so id == name.
func TestCreateProjectStillRequiresIdEqualsName(t *testing.T) {
	e := ghEnv(t)
	// the repository name IS the project id: a GitHub create has no separate name field
	cp := e.propose(t, "create_project", "app-three", ghCreateFields("private"))
	if _, ok := cp.Fields["remote_path"]; ok {
		t.Fatalf("create_project grew a repository-name field: %v", cp.Fields)
	}
	if c, b := e.apply(t, cp); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	if args := lastCall(t, e, "create_project"); args["id"] != "app-three" {
		t.Fatalf("%v", args)
	}
	// a created row whose remote name is not its id is not clonable (registry out of step)
	before := len(e.cloneCalls())
	e.addProjectOrigin("made-here", "gh", "Octo-Cat/another-name", "created")
	if c, b := e.sockJSON(t, "POST", "/api/v1/projects/made-here/clone", ""); c != 400 || v1Code(b) != "not_clonable" || len(e.cloneCalls()) != before {
		t.Fatalf("%d %s", c, b)
	}
}
