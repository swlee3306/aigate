package publish

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/policy"
)

func TestSSHRemoteValidation(t *testing.T) {
	d := t.TempDir()
	key := filepath.Join(d, "key")
	kh := filepath.Join(d, "known_hosts")
	os.WriteFile(key, []byte("k"), 0o600)
	os.WriteFile(kh, []byte("h"), 0o644)
	loose := filepath.Join(d, "loose")
	os.WriteFile(loose, []byte("k"), 0o644)
	openKH := filepath.Join(d, "openkh")
	os.WriteFile(openKH, []byte("h"), 0o666)
	os.Chmod(openKH, 0o666)
	ok := func(c Config) {
		t.Helper()
		c.Allow = policy.NewAllowlist(KindPushBranch)
		if _, err := New(c); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	bad := func(name string, c Config) {
		t.Helper()
		c.Allow = policy.NewAllowlist(KindPushBranch)
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	const r = "ssh://git@100.1.2.3/group/proj.git"
	ok(Config{Remote: r, SSHKeyFile: key, SSHKnownHosts: kh})
	ok(Config{Remote: "ssh://git@host:2222/g/p.git", SSHKeyFile: key, SSHKnownHosts: kh})
	bad("no key", Config{Remote: r, SSHKnownHosts: kh})
	bad("no known_hosts", Config{Remote: r, SSHKeyFile: key})
	bad("loose key", Config{Remote: r, SSHKeyFile: loose, SSHKnownHosts: kh})
	bad("writable known_hosts", Config{Remote: r, SSHKeyFile: key, SSHKnownHosts: openKH})
	bad("missing key file", Config{Remote: r, SSHKeyFile: filepath.Join(d, "nope"), SSHKnownHosts: kh})
	bad("no user", Config{Remote: "ssh://100.1.2.3/g/p.git", SSHKeyFile: key, SSHKnownHosts: kh})
	bad("password", Config{Remote: "ssh://git:pw@100.1.2.3/g/p.git", SSHKeyFile: key, SSHKnownHosts: kh})
	bad("option-like host", Config{Remote: "ssh://git@-oProxyCommand=x/g/p.git", SSHKeyFile: key, SSHKnownHosts: kh})
	bad("no path", Config{Remote: "ssh://git@host", SSHKeyFile: key, SSHKnownHosts: kh})
	bad("query", Config{Remote: r + "?x=1", SSHKeyFile: key, SSHKnownHosts: kh})
	bad("ssh settings on local remote", Config{Remote: d, SSHKeyFile: key, SSHKnownHosts: kh})
	bad("space in key path", Config{Remote: r, SSHKeyFile: key + " x", SSHKnownHosts: kh})
	if c := sshCommand("/k", "/h"); !strings.Contains(c, "StrictHostKeyChecking=yes") || !strings.Contains(c, "IdentityAgent=none") ||
		!strings.Contains(c, "-F /dev/null") || !strings.Contains(c, "UpdateHostKeys=no") || !strings.Contains(c, "GlobalKnownHostsFile=/dev/null") {
		t.Fatalf("weak ssh command: %s", c)
	}
}

// startSSHD runs a throwaway user-level sshd on localhost; skipped when the
// platform cannot run one.
func startSSHD(t *testing.T) (port int, hostPub, clientKey string) {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		if _, serr := os.Stat("/usr/sbin/sshd"); serr != nil {
			t.Skip("no sshd")
		}
		sshd = "/usr/sbin/sshd"
	}
	d, _ := filepath.EvalSymlinks(t.TempDir())
	for _, n := range []string{"hostkey", "client"} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(d, n)).CombinedOutput(); err != nil {
			t.Skipf("ssh-keygen: %v %s", err, out)
		}
	}
	pub, _ := os.ReadFile(filepath.Join(d, "client.pub"))
	os.WriteFile(filepath.Join(d, "authorized_keys"), pub, 0o600)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	port = l.Addr().(*net.TCPAddr).Port
	l.Close()
	cfg := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s/hostkey\nAuthorizedKeysFile %s/authorized_keys\nPidFile %s/sshd.pid\nUsePAM no\nPasswordAuthentication no\nStrictModes no\n", port, d, d, d)
	cfgPath := filepath.Join(d, "sshd_config")
	os.WriteFile(cfgPath, []byte(cfg), 0o600)
	cmd := exec.Command(sshd, "-D", "-e", "-f", cfgPath)
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 50; i++ {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			c.Close()
			hp, _ := os.ReadFile(filepath.Join(d, "hostkey.pub"))
			return port, strings.TrimSpace(string(hp)), filepath.Join(d, "client")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Skip("sshd did not start")
	return
}

// A real push over ssh to a throwaway sshd, with the host key pinned: it works
// with the right known_hosts and is refused with a different host key.
func TestSSHPushPinsHostKey(t *testing.T) {
	port, hostPub, clientKey := startSSHD(t)
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	fx := newFixture(t)
	d := t.TempDir()
	goodKH := filepath.Join(d, "good")
	os.WriteFile(goodKH, []byte(fmt.Sprintf("[127.0.0.1]:%d %s\n", port, hostPub)), 0o600)
	// a different ed25519 key stands in for a spoofed server
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(d, "other")).CombinedOutput(); err != nil {
		t.Skipf("%v %s", err, out)
	}
	op, _ := os.ReadFile(filepath.Join(d, "other.pub"))
	badKH := filepath.Join(d, "bad")
	os.WriteFile(badKH, []byte(fmt.Sprintf("[127.0.0.1]:%d %s\n", port, strings.TrimSpace(string(op)))), 0o600)
	remote := fmt.Sprintf("ssh://%s@127.0.0.1:%d%s", u.Username, port, fx.bare)

	bad, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: remote, SSHKeyFile: clientKey, SSHKnownHosts: badKH})
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Do(context.Background(), Action{Kind: KindPushBranch, Task: 7, Repo: fx.repo, Head: fx.head}); err == nil {
		t.Fatal("push succeeded against an unpinned host key")
	}
	if got := refs(t, fx.bare); got != "" {
		t.Fatalf("remote changed after refused push: %q", got)
	}
	good, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: remote, SSHKeyFile: clientKey, SSHKnownHosts: goodKH})
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Do(context.Background(), Action{Kind: KindPushBranch, Task: 7, Repo: fx.repo, Head: fx.head}); err != nil {
		t.Fatal(err)
	}
	if got := refs(t, fx.bare); got != "refs/heads/orch/7 "+fx.head {
		t.Fatalf("refs %q", got)
	}
}
