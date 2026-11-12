package clone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The resolver fails closed for GitHub rows: provider-linked but unresolvable
// in any way (name differs from the id, hostile or unspellable remote path,
// unknown provider, github.com not pinned (also when only a wildcard, a
// cert-authority line or a look-alike host is there), project id that cannot
// name a key directory, no key) is REFUSED ("", "", true), never the caller's
// default remote ("", "", false) and never the shared GitLab key. When the
// broker is down and the table is stale the answer is refuse for every
// repository, known or unknown.
func TestResolverGitHubFailClosedMatrixW3e(t *testing.T) {
	f := startRegistry(t, ghView(
		proj("hello", "/r/hello", "gh", "Octo/Hello"),
		proj("renamed", "/r/renamed", "gh", "Octo/other"),
		proj("dotgit", "/r/dotgit", "gh", "Octo/dotgit.git"),
		proj("travers", "/r/travers", "gh", "Octo/../travers"),
		proj("optionlike", "/r/optionlike", "gh", "-oProxyCommand=x/optionlike"),
		proj("deep", "/r/deep", "gh", "a/b/deep"),
		proj("ghostprov", "/r/ghostprov", "nope", "Octo/ghostprov"),
		proj("noremote", "/r/noremote", "gh", ""),
		proj("Upper-Id", "/r/upper", "gh", "Octo/Upper-Id"),
		proj("legacy", "/r/legacy", "", ""),
	))
	r, now, keysDir := ghFix(t, f, "hello", "renamed", "dotgit", "travers", "optionlike", "deep", "ghostprov", "noremote", "Upper-Id")
	// control: the one fully valid row resolves with its own key, never the shared key
	remote, key, ok := r.Resolve("/r/hello")
	if !ok || remote != "ssh://git@github.com/Octo/Hello.git" || key != filepath.Join(keysDir, "hello", "id_ed25519") || key == r.SharedKey {
		t.Fatalf("control: %q %q %v", remote, key, ok)
	}
	for _, repo := range []string{"/r/renamed", "/r/dotgit", "/r/travers", "/r/optionlike", "/r/deep", "/r/ghostprov", "/r/noremote", "/r/Upper-Id", "/r/upper"} {
		rem, k, ok := r.Resolve(repo)
		if repo == "/r/Upper-Id" {
			continue // the registry path is /r/upper
		}
		if !ok || rem != "" || k != "" {
			t.Errorf("%s: resolved %q %q %v (must be refuse, not default)", repo, rem, k, ok)
		}
	}
	// a legacy project and an unknown path are not ours: the caller's default applies
	for _, repo := range []string{"/r/legacy", "/r/unknown"} {
		if rem, k, ok := r.Resolve(repo); ok || rem != "" || k != "" {
			t.Errorf("%s: %q %q %v", repo, rem, k, ok)
		}
	}
	// github.com pinned only by lines that do not count
	for name, content := range map[string]string{
		"wildcard":      "*.github.com " + keyField + "\n",
		"star":          "* " + keyField + "\n",
		"cert":          "@cert-authority github.com " + keyField + "\n",
		"revoked":       "@revoked github.com " + keyField + "\n",
		"negated":       "!github.com " + keyField + "\n",
		"look-alike":    "github.com.evil.example " + keyField + "\n",
		"other gitlab":  "gitlab.example " + keyField + "\n",
		"empty":         "",
		"comment":       "# github.com " + keyField + "\n",
		"pattern right": "github.co? " + keyField + "\n",
	} {
		os.WriteFile(r.KnownHosts, []byte(content), 0o644)
		r.Invalidate()
		if rem, k, ok := r.Resolve("/r/hello"); !ok || rem != "" || k != "" {
			t.Errorf("known_hosts %s: %q %q %v", name, rem, k, ok)
		}
	}
	os.WriteFile(r.KnownHosts, []byte("gitlab.example "+keyField+"\n"+ghKnown+"\n"), 0o644)
	r.Invalidate()
	if rem, _, _ := r.Resolve("/r/hello"); rem == "" {
		t.Fatal("control after restoring known_hosts")
	}
	// broker down and the table stale: everything is refused, also a repository the table never listed
	f.stop()
	*now = now.Add(time.Hour)
	for _, repo := range []string{"/r/hello", "/r/legacy", "/r/unknown", "/r/new-project"} {
		if rem, k, ok := r.Resolve(repo); !ok || rem != "" || k != "" {
			t.Errorf("broker down, stale table, %s: %q %q %v", repo, rem, k, ok)
		}
	}
}

// Clone and the ssh command line: whatever the process environment holds
// (GIT_SSH_COMMAND, an agent socket, a proxy command) and whatever the
// options claim, a GitHub clone runs git exactly once with the registry's
// remote, the one per-project key (`-i <that key>`, IdentitiesOnly, strict
// host checking against the given known_hosts, no host-key update) and never
// the shared key. Hostile remotes, kinds, key paths and unpinned hosts run
// nothing at all.
func TestCloneGitHubCommandLineAndEnvironmentW3e(t *testing.T) {
	for k, v := range map[string]string{
		"GIT_SSH_COMMAND": "evil", "GIT_SSH": "/evil/ssh", "SSH_AUTH_SOCK": "/evil/agent", "GIT_PROXY_COMMAND": "evil",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.sshCommand", "GIT_CONFIG_VALUE_0": "evil", "GIT_TEMPLATE_DIR": "/evil",
	} {
		t.Setenv(k, v)
	}
	f := newFix(t)
	os.WriteFile(f.known, []byte(ghKnown+"\n"), 0o644)
	perKey := filepath.Join(filepath.Dir(f.keyFile), "keys", "hello", "id_ed25519")
	sm, calls := recorder(nil, nil)
	o := f.opts("hello")
	o.Remote, o.Kind, o.SSHKey = "ssh://git@github.com/Octo/Hello.git", "github", perKey
	if err := clone(context.Background(), o, sm); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("%d runs", len(*calls))
	}
	c := (*calls)[0]
	var sshCmds []string
	for _, e := range c.env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			sshCmds = append(sshCmds, strings.TrimPrefix(e, "GIT_SSH_COMMAND="))
		}
		for _, bad := range []string{"SSH_AUTH_SOCK=", "GIT_SSH=", "GIT_PROXY_COMMAND=", "GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=", "GIT_TEMPLATE_DIR="} {
			if strings.HasPrefix(e, bad) {
				t.Errorf("the environment of git carries %s", e)
			}
		}
	}
	if len(sshCmds) != 1 || strings.Contains(sshCmds[0], "evil") || strings.Count(sshCmds[0], " -i ") != 1 || !strings.Contains(sshCmds[0], " -i "+perKey+" ") ||
		strings.Contains(sshCmds[0], f.keyFile) || !strings.Contains(sshCmds[0], "-o IdentitiesOnly=yes") || !strings.Contains(sshCmds[0], "-o StrictHostKeyChecking=yes") ||
		!strings.Contains(sshCmds[0], "-o UpdateHostKeys=no") || !strings.Contains(sshCmds[0], "UserKnownHostsFile="+f.known) {
		t.Fatalf("ssh command: %v", sshCmds)
	}
	joined := strings.Join(c.args, "\n")
	if strings.Contains(joined, "evil") || !strings.Contains(joined, "ssh://git@github.com/Octo/Hello.git") {
		t.Fatalf("argv: %v", c.args)
	}
	// refusals run nothing
	hostileRemotes := []string{"ssh://git@github.com/Octo/Hello.git -oProxyCommand=x", "ssh://git@github.com/Octo/Hello.git\n", "ext::sh -c id",
		"file:///etc/passwd", "git@github.com:Octo/Hello.git", "ssh://git@github.com:2222/Octo/Hello.git", "ssh://git@evil.example/Octo/Hello.git",
		"ssh://git@github.com/-o/Hello.git", "ssh://git@github.com/Octo/Hello.git/../x", "ssh://git@github.com/Octo/%2e%2e.git",
		"-oProxyCommand=x", "--upload-pack=x", "ssh://git@github.com/Octo/Hel lo.git", "ssh://git@github.com//Octo/Hello.git"}
	for _, r := range hostileRemotes {
		o := f.opts("x1")
		o.Remote, o.Kind, o.SSHKey = r, "github", perKey
		if err := clone(context.Background(), o, sm); err == nil {
			t.Errorf("remote %q accepted", r)
		}
	}
	for _, key := range []string{perKey + " -o ProxyCommand=x", "relative/key", perKey + "'", perKey + "$x", perKey + "\n", "", "-oProxyCommand=x"} {
		o := f.opts("x2")
		o.Remote, o.Kind, o.SSHKey = "ssh://git@github.com/Octo/Hello.git", "github", key
		if err := clone(context.Background(), o, sm); err == nil && key != "" {
			t.Errorf("key path %q accepted", key)
		}
	}
	for name, content := range map[string]string{
		"wildcard": "*.github.com " + keyField + "\n", "cert": "@cert-authority github.com " + keyField + "\n", "gitlab only": "gitlab.example " + keyField + "\n", "empty": "",
	} {
		os.WriteFile(f.known, []byte(content), 0o644)
		o := f.opts("x3")
		o.Remote, o.Kind, o.SSHKey = "ssh://git@github.com/Octo/Hello.git", "github", perKey
		if err := clone(context.Background(), o, sm); err == nil {
			t.Errorf("known_hosts %s: cloned without a pinned github.com", name)
		}
	}
	// the destination must stay a new folder directly under an allowed root
	os.WriteFile(f.known, []byte(ghKnown+"\n"), 0o644)
	for _, dest := range []string{filepath.Join(f.root, "a", "b"), filepath.Join(f.root, ".."), "/tmp/x-w3e", f.root} {
		o := f.opts("x4")
		o.Dest, o.Remote, o.Kind, o.SSHKey = dest, "ssh://git@github.com/Octo/Hello.git", "github", perKey
		if err := clone(context.Background(), o, sm); err == nil {
			t.Errorf("destination %q accepted", dest)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("git ran %d times in total (the control only)", len(*calls))
	}
}
