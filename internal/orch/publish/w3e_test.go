package publish

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/orch/policy"
)

// sfix is rfix with a fake git that also records GIT_SSH_COMMAND (the only
// place the key path appears) and a known_hosts that pins gitlab and github.
type sfix struct {
	rfix
	perKey string // a per-project private key (mode 0600)
}

func newSFix(t *testing.T) sfix {
	t.Helper()
	r := newRFix(t)
	script := "#!/bin/sh\nprintf 'SSH=%s\\n' \"$GIT_SSH_COMMAND\" >> '" + r.log + "'\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + r.log + "'; done\nprintf -- '--end--\\n' >> '" + r.log + "'\nexit 0\n"
	if err := os.WriteFile(r.gitBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.known, []byte("gitlab.example "+khKey+"\ngithub.com "+khKey+"\n"), 0o644)
	d, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(d, "keys", "hello"), 0o700); err != nil {
		t.Fatal(err)
	}
	per := filepath.Join(d, "keys", "hello", "id_ed25519")
	os.WriteFile(per, []byte("k"), 0o600)
	return sfix{rfix: r, perKey: per}
}

const ghRemote = "ssh://git@github.com/Octo/Hello.git"

func (s sfix) sshLines() []string {
	b, _ := os.ReadFile(s.log)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "SSH=") && l != "SSH=" {
			out = append(out, l)
		}
	}
	return out
}

func TestPublishUsesPerProjectKey(t *testing.T) {
	s := newSFix(t)
	res := func(repo string) (string, string, bool) { return ghRemote, s.perKey, true }
	p := s.pub(t, Config{RemoteResolver: res})
	if err := s.push(p, s.repo); err != nil {
		t.Fatal(err)
	}
	lines := s.sshLines()
	if len(lines) != 1 || !strings.Contains(lines[0], " -i "+s.perKey+" ") || strings.Contains(lines[0], " -i "+s.key+" ") ||
		!strings.Contains(lines[0], "-o IdentitiesOnly=yes") || !strings.Contains(lines[0], "-o UpdateHostKeys=no") ||
		!strings.Contains(lines[0], "-o StrictHostKeyChecking=yes") || !strings.Contains(lines[0], "UserKnownHostsFile="+s.known) {
		t.Fatalf("ssh command: %v", lines)
	}
	if !s.pushedTo(ghRemote) {
		t.Fatalf("pushed %v", s.pushes())
	}
	// the shared key is used when the resolver names no key or names the shared one
	for name, k := range map[string]string{"empty": "", "shared": s.key} {
		os.Remove(s.log)
		p := s.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return resolved, k, true }})
		if err := s.push(p, s.repo); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if l := s.sshLines(); len(l) != 1 || !strings.Contains(l[0], " -i "+s.key+" ") {
			t.Errorf("%s: %v", name, l)
		}
	}
	// github.com must be pinned in the known_hosts file the publisher was given
	os.WriteFile(s.known, []byte("gitlab.example "+khKey+"\n"), 0o644)
	os.Remove(s.log)
	if err := s.push(s.pub(t, Config{RemoteResolver: res}), s.repo); err == nil {
		t.Error("github.com pushed without a pinned host")
	}
	if b, _ := os.ReadFile(s.log); len(b) != 0 {
		t.Errorf("git ran: %s", b)
	}
}

// A per-project key is looked at again at the moment of the push: whatever the
// resolver said a moment ago, a missing or loosened key stops the push.
func TestPublishRefusesUnusablePerProjectKey(t *testing.T) {
	s := newSFix(t)
	for name, mod := range map[string]func(){
		"missing":        func() { os.Remove(s.perKey) },
		"group readable": func() { os.Chmod(s.perKey, 0o640) },
		"symlink":        func() { os.Remove(s.perKey); os.Symlink(s.key, s.perKey) },
		"directory":      func() { os.Remove(s.perKey); os.Mkdir(s.perKey, 0o700) },
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
	// a key path with a character that could break out of the ssh command string
	for _, bad := range []string{s.perKey + " -o ProxyCommand=x", "relative/key", s.perKey + "'", s.perKey + "$x"} {
		p := s.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return ghRemote, bad, true }})
		os.RemoveAll(s.perKey)
		os.WriteFile(s.perKey, []byte("k"), 0o600)
		if err := s.push(p, s.repo); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Static remotes keep priority and never get a per-project key.
func TestStaticRemotesUnchangedW3e(t *testing.T) {
	s := newSFix(t)
	other := newRFix(t)
	asked := 0
	res := func(string) (string, string, bool) { asked++; return ghRemote, s.perKey, true }
	p, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Git: s.gitBin, Remote: s.bare, RepoRemotes: map[string]string{other.repo: other.bare},
		SSHKeyFile: s.key, SSHKnownHosts: s.known, RemoteResolver: res})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Do(context.Background(), Action{Kind: KindPushBranch, Task: 3, Repo: other.repo, Head: other.head}); err != nil {
		t.Fatal(err)
	}
	if asked != 0 || !s.pushedTo(other.bare) || len(s.sshLines()) != 0 {
		t.Fatalf("static remote: asked=%d pushes=%v ssh=%v", asked, s.pushes(), s.sshLines())
	}
	// the default remote for a repo the resolver does not know: unchanged
	q := s.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return "", "", false }})
	os.Remove(s.log)
	if err := s.push(q, s.repo); err != nil || !s.pushedTo(s.bare) || len(s.sshLines()) != 0 {
		t.Fatalf("default: %v %v", err, s.pushes())
	}
}
