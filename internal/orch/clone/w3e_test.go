package clone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/keys"
)

const ghKnown = "github.com " + keyField

// ghView is a registry view with a github and a gitlab provider.
func ghView(projects ...map[string]any) map[string]any {
	v := view(projects...)
	v["providers"] = append(v["providers"].([]any),
		map[string]any{"id": "gh", "kind": "github", "base_url": "https://api.github.com", "ssh_url": "ssh://git@github.com", "token_ref": "gh-main"})
	return v
}

// ghFix: a resolver with a keys directory in which the given projects have a key.
func ghFix(t *testing.T, f *fakeRegistry, withKey ...string) (*Resolver, *time.Time, string) {
	t.Helper()
	r, now := newResolver(t, f)
	base := shortDir(t)
	r.KnownHosts = filepath.Join(base, "known_hosts")
	if err := os.WriteFile(r.KnownHosts, []byte("gitlab.example "+keyField+"\n"+ghKnown+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.KeysDir = filepath.Join(base, "keys")
	r.SharedKey = filepath.Join(base, "id_gitlab_deploy")
	if err := os.Mkdir(r.KeysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range withKey {
		d := filepath.Join(r.KeysDir, id)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, keys.PrivateName), []byte("not a real key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r, now, r.KeysDir
}

func TestRemoteForKind(t *testing.T) {
	for _, tc := range []struct {
		kind, ssh, path, want string
	}{
		{"github", "ssh://git@github.com", "Octo-Cat/Hello_World.js", "ssh://git@github.com/Octo-Cat/Hello_World.js.git"},
		{"github", "ssh://git@github.com", "octo/hello", "ssh://git@github.com/octo/hello.git"},
		{"github", "ssh://git@github.com", "octo/sub/hello", ""}, // exactly owner/repo
		{"github", "ssh://git@github.com", "octo", ""},
		{"github", "ssh://git@github.com", "octo/hello.git", ""},
		{"github", "ssh://git@github.com", "octo/..", ""},
		{"github", "ssh://git@github.com", "-o/hello", ""},
		{"github", "ssh://git@github.com", "octo/he llo", ""},
		{"github", "ssh://git@github.com/", "octo/hello", ""},
		{"github", "ssh://root@github.com", "octo/hello", ""},
		{"gitlab", "ssh://git@gitlab.example", "team/app-one", "ssh://git@gitlab.example/team/app-one.git"},
		{"gitlab", "ssh://git@gitlab.example", "Team/App", ""}, // GitLab stays lowercase-only
		{"", "ssh://git@github.com", "octo/hello", ""},
		{"bitbucket", "ssh://git@github.com", "octo/hello", ""},
	} {
		got, ok := RemoteForKind(tc.kind, tc.ssh, tc.path)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("RemoteForKind(%q,%q,%q) = %q,%v want %q", tc.kind, tc.ssh, tc.path, got, ok, tc.want)
		}
	}
	if !NameMatches("github", "Octo/Hello-World", "hello-world") || NameMatches("gitlab", "team/Hello", "hello") ||
		!NameMatches("gitlab", "team/hello", "hello") || NameMatches("github", "octo/other", "hello") || NameMatches("github", "nogroup", "x") {
		t.Error("NameMatches")
	}
}

func TestCloneUsesPerProjectKeyForGitHub(t *testing.T) {
	f := newFix(t)
	if err := os.WriteFile(f.known, []byte(ghKnown+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	perKey := filepath.Join(filepath.Dir(f.keyFile), "keys", "hello", "id_ed25519")
	sm, calls := recorder(nil, nil)
	o := f.opts("hello")
	o.Remote = "ssh://git@github.com/Octo/Hello.git"
	o.Kind = "github"
	o.SSHKey = perKey
	if err := clone(context.Background(), o, sm); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("%d runs", len(*calls))
	}
	c := (*calls)[0]
	var ssh string
	for _, e := range c.env {
		if v, ok := strings.CutPrefix(e, "GIT_SSH_COMMAND="); ok {
			ssh = v
		}
	}
	if !strings.Contains(ssh, " -i "+perKey+" ") || !strings.Contains(ssh, "-o IdentitiesOnly=yes") || !strings.Contains(ssh, "-o UpdateHostKeys=no") ||
		!strings.Contains(ssh, "-o StrictHostKeyChecking=yes") || strings.Contains(ssh, f.keyFile) {
		t.Fatalf("ssh command: %s", ssh)
	}
	if got := c.args[len(c.args)-2]; got != "ssh://git@github.com/Octo/Hello.git" {
		t.Fatalf("remote %q", got)
	}
	// GitLab (no kind, or "gitlab") keeps the strict lowercase shape
	for _, kind := range []string{"", "gitlab", "bitbucket"} {
		o2 := f.opts("hello-2")
		o2.Remote, o2.Kind = "ssh://git@github.com/Octo/Hello.git", kind
		if err := clone(context.Background(), o2, sm); !errors.Is(err, ErrBadRemote) {
			t.Errorf("kind %q accepted a GitHub spelling: %v", kind, err)
		}
	}
	// and the GitHub shape refuses everything that is not owner/repo.git
	for _, r := range []string{"ssh://git@github.com/Octo/Hello", "ssh://git@github.com/Octo/Hello.git ", "ssh://git@github.com/a/b/c.git",
		"ssh://git@github.com/-o/Hello.git", "ssh://git@github.com/Octo/..git", "ssh://git@github.com/Octo/x.git.git", "ssh://root@github.com/Octo/Hello.git",
		"ssh://git@github.com/Octo/Hello.git?x", "https://github.com/Octo/Hello.git", "ssh://git@-oProxyCommand=x/a/b.git", "ssh://git@github.com/Oc to/Hello.git"} {
		o3 := f.opts("hello-3")
		o3.Remote, o3.Kind = r, "github"
		if err := clone(context.Background(), o3, sm); !errors.Is(err, ErrBadRemote) {
			t.Errorf("%q: %v", r, err)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("git ran %d times", len(*calls))
	}
}

func TestCloneGitHubHostMustBePinned(t *testing.T) {
	f := newFix(t) // known_hosts pins only gitlab.example
	sm, calls := recorder(nil, nil)
	o := f.opts("hello")
	o.Remote, o.Kind = "ssh://git@github.com/Octo/Hello.git", "github"
	if err := clone(context.Background(), o, sm); !errors.Is(err, ErrHostNotPinned) {
		t.Fatalf("github.com is not pinned: %v", err)
	}
	// only exact or hashed entries count: wildcard and cert-authority never pin
	for _, line := range []string{"*.github.com " + keyField, "* " + keyField, "@cert-authority github.com " + keyField, "gitlab.example " + keyField + "\n!github.com,github.com " + keyField} {
		if err := os.WriteFile(f.known, []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := clone(context.Background(), o, sm); !errors.Is(err, ErrHostNotPinned) {
			t.Errorf("%q pinned github.com: %v", line, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("git ran %d times", len(*calls))
	}
	if err := os.WriteFile(f.known, []byte(ghKnown+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clone(context.Background(), o, sm); err != nil {
		t.Fatalf("pinned: %v", err)
	}
}

func TestResolverReturnsKeyPathFromRegistryOnly(t *testing.T) {
	f := startRegistry(t, ghView(
		proj("hello", "/r/hello", "gh", "Octo/Hello"),
		proj("app-one", "/r/app-one", "gl", "team/app-one"),
		proj("nokey", "/r/nokey", "gh", "Octo/nokey"),
		proj("odd-name", "/r/odd-name", "gh", "Octo/other"),
		proj("elsewhere", "/r/some-other-dir", "gh", "Octo/elsewhere"), // the folder name differs from the id: the key follows the id
		proj("badpath", "/r/badpath", "gh", "Octo/sub/badpath"),
	))
	r, _, keysDir := ghFix(t, f, "hello", "odd-name", "badpath", "elsewhere")
	remote, key, ok := r.Resolve("/r/hello")
	if !ok || remote != "ssh://git@github.com/Octo/Hello.git" || key != filepath.Join(keysDir, "hello", "id_ed25519") {
		t.Fatalf("github: %q %q %v", remote, key, ok)
	}
	// the key path is built from the registry's project id under KeysDir, whatever the path is
	if remote, key, ok = r.Resolve("/r/app-one"); !ok || remote != "ssh://git@gitlab.example/team/app-one.git" || key != r.SharedKey {
		t.Fatalf("gitlab: %q %q %v", remote, key, ok)
	}
	if remote, key, ok = r.Resolve("/r/some-other-dir"); !ok || remote != "ssh://git@github.com/Octo/elsewhere.git" || key != filepath.Join(keysDir, "elsewhere", "id_ed25519") {
		t.Fatalf("key must follow the registry id, not the repo path: %q %q %v", remote, key, ok)
	}
	for _, repo := range []string{"/r/nokey", "/r/odd-name", "/r/badpath"} {
		if remote, key, ok = r.Resolve(repo); !ok || remote != "" || key != "" {
			t.Errorf("%s: %q %q %v (want refuse)", repo, remote, key, ok)
		}
	}
	// a request-shaped repo (never listed) is not resolved, and nothing about it reaches the key dir
	for _, repo := range []string{"/r/hello/../../etc", "/r/unlisted", "hello", "/r/hello/"} {
		if remote, key, ok = r.Resolve(repo); remote != "" || key != "" || ok {
			t.Errorf("%q: %q %q %v", repo, remote, key, ok)
		}
	}
}

func TestResolverRefusesUnusableGitHubKey(t *testing.T) {
	f := startRegistry(t, ghView(proj("hello", "/r/hello", "gh", "Octo/Hello")))
	r, _, keysDir := ghFix(t, f, "hello")
	priv := filepath.Join(keysDir, "hello", "id_ed25519")
	good := func() {
		os.Chmod(filepath.Dir(priv), 0o700)
		os.RemoveAll(priv)
		os.WriteFile(priv, []byte("k"), 0o600)
	}
	for name, mod := range map[string]func(){
		"missing":        func() { os.Remove(priv) },
		"group readable": func() { os.Chmod(priv, 0o640) },
		"world readable": func() { os.Chmod(priv, 0o604) },
		"symlink": func() {
			os.Remove(priv)
			real := filepath.Join(shortDir(t), "k")
			os.WriteFile(real, []byte("k"), 0o600)
			os.Symlink(real, priv)
		},
		"a directory":   func() { os.Remove(priv); os.Mkdir(priv, 0o700) },
		"dir open":      func() { os.Chmod(filepath.Dir(priv), 0o755) },
		"empty":         func() { os.Truncate(priv, 0) },
		"keys dir gone": func() { os.RemoveAll(keysDir) },
	} {
		good()
		if _, key, ok := r.Resolve("/r/hello"); !ok || key == "" {
			t.Fatalf("control: %v %q", ok, key)
		}
		mod()
		// no cache invalidation is needed: the key is looked at on every call
		if remote, key, ok := r.Resolve("/r/hello"); !ok || remote != "" || key != "" {
			t.Errorf("%s: resolved %q %q %v", name, remote, key, ok)
		}
		if name == "keys dir gone" {
			os.Mkdir(keysDir, 0o700)
			os.Mkdir(filepath.Dir(priv), 0o700)
		}
	}
	// no KeysDir configured: GitHub projects are refused, never given the shared key
	r.KeysDir = ""
	good()
	if remote, key, ok := r.Resolve("/r/hello"); !ok || remote != "" || key != "" {
		t.Errorf("no keys dir: %q %q %v", remote, key, ok)
	}
}

func TestResolverGitHubHostMustBePinned(t *testing.T) {
	f := startRegistry(t, ghView(proj("hello", "/r/hello", "gh", "Octo/Hello")))
	r, now, _ := ghFix(t, f, "hello")
	if remote, _, _ := r.Resolve("/r/hello"); remote == "" {
		t.Fatal("control")
	}
	if err := os.WriteFile(r.KnownHosts, []byte("gitlab.example "+keyField+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour) // past the cache
	if remote, key, ok := r.Resolve("/r/hello"); !ok || remote != "" || key != "" {
		t.Fatalf("github.com unpinned: %q %q %v", remote, key, ok)
	}
}

// TestResolverGitLabUnchanged: GitLab projects resolve exactly as before and get
// the shared key; a registry without KeysDir/SharedKey (as W3c wired it) still works.
func TestResolverGitLabUnchanged(t *testing.T) {
	f := startRegistry(t, view(proj("app-one", "/r/app-one", "gl", "team/app-one"), proj("legacy", "/r/legacy", "", "")))
	r, _ := newResolver(t, f) // no KeysDir, no SharedKey: the W3c wiring
	remote, key, ok := r.Resolve("/r/app-one")
	if !ok || remote != "ssh://git@gitlab.example/team/app-one.git" || key != "" {
		t.Fatalf("%q %q %v", remote, key, ok)
	}
	if remote, key, ok = r.Resolve("/r/legacy"); ok || remote != "" || key != "" {
		t.Fatalf("legacy: %q %q %v", remote, key, ok)
	}
	r.SharedKey = "/home/orch/orchd/ssh/id_gitlab_deploy"
	r.Invalidate()
	if _, key, _ = r.Resolve("/r/app-one"); key != r.SharedKey {
		t.Fatalf("shared key %q", key)
	}
	// the shared key is NOT inspected here (publish.New checked it at start)
	var _ = broker.OriginCreated
}
