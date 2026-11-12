package publish

import (
	"os"
	"strings"
	"testing"
)

// Publishing never leaves the registry-derived remote and the per-project key:
// a provider-linked project that cannot be resolved ("", "", true) is refused
// and the default remote is never pushed to; a resolver answer that is not a
// safe ssh remote on a pinned host (option injection, other host, other port,
// a path walking out) runs no git; the process environment's ssh settings
// never reach the child; and the key named by the resolver is the only key
// (not the shared one) for GitHub, with the shared key and no explicit key
// for GitLab rows only.
func TestPublishNeverLeavesRegistryRemoteAndPerProjectKeyW3e(t *testing.T) {
	for k, v := range map[string]string{"GIT_SSH_COMMAND": "evil-ssh", "SSH_AUTH_SOCK": "/evil/agent", "GIT_SSH": "/evil/ssh", "GIT_PROXY_COMMAND": "evil"} {
		t.Setenv(k, v)
	}
	s := newSFix(t)
	// unresolved provider-linked project: refuse, the default remote (s.bare) is never pushed to
	p := s.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return "", "", true }})
	if err := s.push(p, s.repo); err == nil {
		t.Fatal("pushed although the project could not be resolved")
	}
	if b, _ := os.ReadFile(s.log); len(b) != 0 || s.pushedTo(s.bare) {
		t.Fatalf("git ran: %s", b)
	}
	// resolver answers that are not a safe remote on a pinned host
	for _, bad := range []string{
		"ssh://git@github.com/Octo/Hello.git -oProxyCommand=x", "ssh://git@github.com/Octo/Hello.git\n", "ext::sh -c id", "file:///etc/passwd",
		"/home/orch/repos/remote.git", "ssh://git@evil.example/Octo/Hello.git", "ssh://git@github.com:2222/Octo/Hello.git", "ssh://root@github.com/Octo/Hello.git",
		"-oProxyCommand=x", "ssh://git@github.com/-o/Hello.git", "https://github.com/Octo/Hello.git", "",
	} {
		os.Remove(s.log)
		p := s.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return bad, s.perKey, true }})
		if err := s.push(p, s.repo); err == nil {
			t.Errorf("remote %q was pushed to", bad)
		}
		if b, _ := os.ReadFile(s.log); len(b) != 0 {
			t.Errorf("remote %q: git ran: %s", bad, b)
		}
	}
	// the one good answer: exactly one run, the per-project key only, none of the environment's ssh settings
	os.Remove(s.log)
	p = s.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return ghRemote, s.perKey, true }})
	if err := s.push(p, s.repo); err != nil {
		t.Fatal(err)
	}
	lines := s.sshLines()
	if len(lines) != 1 || strings.Contains(lines[0], "evil") || strings.Count(lines[0], " -i ") != 1 || !strings.Contains(lines[0], " -i "+s.perKey+" ") || strings.Contains(lines[0], s.key) {
		t.Fatalf("ssh command: %v", lines)
	}
	if !s.pushedTo(ghRemote) || s.pushedTo(s.bare) {
		t.Fatalf("pushes: %v", s.pushes())
	}
	// a per-project key that belongs to ANOTHER project's directory is just a path: it must still be a safe private file
	for name, mod := range map[string]func(){
		"loosened after the resolver answered":    func() { os.Chmod(s.perKey, 0o644) },
		"replaced by a symlink to the shared key": func() { os.Remove(s.perKey); os.Symlink(s.key, s.perKey) },
	} {
		os.RemoveAll(s.perKey)
		os.WriteFile(s.perKey, []byte("k"), 0o600)
		mod()
		os.Remove(s.log)
		p := s.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return ghRemote, s.perKey, true }})
		if err := s.push(p, s.repo); err == nil {
			t.Errorf("%s: pushed", name)
		}
		if b, _ := os.ReadFile(s.log); len(b) != 0 {
			t.Errorf("%s: git ran: %s", name, b)
		}
	}
}
