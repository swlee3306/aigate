package knownhosts

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const keyField = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hashed(name string) string {
	salt := []byte("0123456789abcdef0123")
	m := hmac.New(sha1.New, salt)
	m.Write([]byte(name))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestPinnedMatchesExactHostsOnly(t *testing.T) {
	kh := write(t, strings.Join([]string{
		"# a comment",
		"",
		"gitlab.example " + keyField,
		"other.example,10.0.0.5 " + keyField,
		"[odd.example]:2222 " + keyField,
		"*.wild.example " + keyField,
		"* " + keyField,
		"@cert-authority ca.example " + keyField,
		"@revoked revoked.example " + keyField,
		"short.example", // no key field
		"!neg.example,neg.example " + keyField,
		hashed("hashed.example") + " " + keyField,
		hashed("[hport.example]:2200") + " " + keyField,
		"::1 " + keyField,
		"[fd00::5]:2022 " + keyField,
	}, "\n")+"\n")
	for _, tc := range []struct {
		host, port string
		want       bool
	}{
		{"gitlab.example", "", true},
		{"gitlab.example", "22", true},
		{"gitlab.example", "2222", false}, // different port needs its own [host]:port entry
		{"other.example", "", true},
		{"10.0.0.5", "", true},
		{"odd.example", "2222", true},
		{"odd.example", "", false},
		{"odd.example", "22", false},
		{"gitlab.example.evil", "", false},
		{"evil.gitlab.example", "", false},
		{"gitlab.exampl", "", false},
		{"a.wild.example", "", false}, // wildcard entries never pin
		{"anything.example", "", false},
		{"ca.example", "", false},
		{"revoked.example", "", false},
		{"short.example", "", false},
		{"neg.example", "", false},
		{"hashed.example", "", true},
		{"hashed.example", "22", true},
		{"hport.example", "2200", true},
		{"hport.example", "", false},
		{"::1", "", true},
		{"[::1]", "", true},
		{"fd00::5", "2022", true},
		{"[fd00::5]", "2022", true},
		{"", "", false},
		{"GITLAB.EXAMPLE", "", false},
	} {
		if got := Pinned(kh, tc.host, tc.port); got != tc.want {
			t.Errorf("Pinned(%q,%q) = %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}
}

func TestPinnedFailsClosedOnBadFile(t *testing.T) {
	d := t.TempDir()
	if Pinned(filepath.Join(d, "none"), "gitlab.example", "") {
		t.Error("missing file pins")
	}
	if Pinned("", "gitlab.example", "") {
		t.Error("empty path pins")
	}
	if Pinned(d, "gitlab.example", "") {
		t.Error("directory pins")
	}
	real := write(t, "gitlab.example "+keyField+"\n")
	link := filepath.Join(d, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if Pinned(link, "gitlab.example", "") {
		t.Error("symlinked file pins")
	}
	big := filepath.Join(d, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("#"+strings.Repeat("x", 1000)+"\n", 2000)+"gitlab.example "+keyField+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Pinned(big, "gitlab.example", "") {
		t.Error("oversize file pins")
	}
}

func TestSplitSSHRemote(t *testing.T) {
	for _, tc := range []struct {
		in         string
		host, port string
		ok         bool
	}{
		{"ssh://git@gitlab.example/g/p.git", "gitlab.example", "", true},
		{"ssh://git@gitlab.example:2222/g/p.git", "gitlab.example", "2222", true},
		{"ssh://git@100.1.2.3/g/s/p.git", "100.1.2.3", "", true},
		{"ssh://git@[fd00::5]:2022/g/p.git", "[fd00::5]", "2022", true},
		{"ssh://root@gitlab.example/g/p.git", "", "", false},
		{"ssh://git@gitlab.example/g/p", "", "", false},
		{"ssh://git@gitlab.example/g/p.git?x=1", "", "", false},
		{"ssh://git@-oProxyCommand=x/g/p.git", "", "", false},
		{"https://git@gitlab.example/g/p.git", "", "", false},
		{"/local/path", "", "", false},
		{"ssh://git@gitlab.example/../p.git", "", "", false},
		{"ssh://git@gitlab.example//p.git", "", "", false},
		{"ssh://git@gitlab.example/g/P.git", "", "", false},
	} {
		h, p, ok := SplitSSHRemote(tc.in)
		if ok != tc.ok || h != tc.host || p != tc.port {
			t.Errorf("SplitSSHRemote(%q) = %q %q %v", tc.in, h, p, ok)
		}
	}
}

func TestRemotePinned(t *testing.T) {
	kh := write(t, "gitlab.example "+keyField+"\n[other.example]:2222 "+keyField+"\n")
	if !RemotePinned(kh, "ssh://git@gitlab.example/g/p.git") || !RemotePinned(kh, "ssh://git@other.example:2222/g/p.git") {
		t.Error("pinned remotes refused")
	}
	if RemotePinned(kh, "ssh://git@other.example/g/p.git") || RemotePinned(kh, "ssh://git@evil.example/g/p.git") || RemotePinned(kh, "/local") {
		t.Error("unpinned remote accepted")
	}
}
