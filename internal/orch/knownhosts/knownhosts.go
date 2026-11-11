// Package knownhosts answers one question for clone and publish: is this ssh
// host pinned in the known_hosts file orchd was given? It is pure standard
// library. Only plain host entries and hashed (|1|) entries match; wildcards,
// negations and markers (@cert-authority, @revoked) never pin a host, so a
// host is accepted only when somebody wrote exactly that host into the file.
package knownhosts

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // the known_hosts hashed format is HMAC-SHA1
	"crypto/subtle"
	"encoding/base64"
	"io"
	"os"
	"regexp"
	"strings"
)

// maxFile bounds the known_hosts file that is read.
const maxFile = 1 << 20

// remoteRE is the only remote shape clone and publish resolve: the ssh user is
// git, the host a lowercase DNS name, IPv4 or bracketed IPv6, an optional
// port, then a path of lowercase name segments ending in .git.
var remoteRE = regexp.MustCompile(`^ssh://git@(\[[0-9a-f:]+\]|[a-z0-9][a-z0-9.-]*)(?::([1-9][0-9]{0,4}))?/([a-z][a-z0-9-]*(?:/[a-z][a-z0-9-]*){0,4})\.git$`)

// SplitSSHRemote returns the host (IPv6 keeps its brackets) and the port ("" =
// none given) of a remote of the shape ssh://git@host[:port]/group/.../name.git.
func SplitSSHRemote(remote string) (host, port string, ok bool) {
	m := remoteRE.FindStringSubmatch(remote)
	if m == nil || strings.HasPrefix(m[1], "-") {
		return "", "", false
	}
	return m[1], m[2], true
}

// RemotePinned reports whether remote has the shape above and its host is
// pinned in the known_hosts file at path.
func RemotePinned(path, remote string) bool {
	host, port, ok := SplitSSHRemote(remote)
	return ok && Pinned(path, host, port)
}

// Pinned reports whether host (with port, "" or "22" = the default port) has a
// plain or hashed entry in the known_hosts file at path. A missing, oversize
// or non-regular file (a symlink included) pins nothing.
func Pinned(path, host, port string) bool {
	bare := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if path == "" || bare == "" || strings.ContainsAny(bare, " \t\r\n,*?!|@#") {
		return false
	}
	if port == "22" {
		port = ""
	}
	name := bare
	if port != "" {
		name = "[" + bare + "]:" + port
	}
	data, ok := readFile(path)
	if !ok {
		return false
	}
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		if lineMatches(sc.Text(), name) {
			return true
		}
	}
	return false
}

func readFile(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFile {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(b) > maxFile {
		return "", false
	}
	return string(b), true
}

func lineMatches(line, name string) bool {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' || line[0] == '@' {
		return false
	}
	fields := strings.Fields(line)
	if len(fields) < 3 { // hosts, key type, key
		return false
	}
	hosts := fields[0]
	if strings.HasPrefix(hosts, "|1|") {
		return hashedMatches(hosts, name)
	}
	match := false
	for _, h := range strings.Split(hosts, ",") {
		if strings.HasPrefix(h, "!") {
			if h[1:] == name {
				return false // an entry that excludes this host never applies
			}
			continue
		}
		if strings.ContainsAny(h, "*?") {
			continue
		}
		if h == name {
			match = true
		}
	}
	return match
}

func hashedMatches(field, name string) bool {
	parts := strings.Split(field, "|") // "", "1", salt, hash
	if len(parts) != 4 {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(salt) == 0 {
		return false
	}
	m := hmac.New(sha1.New, salt)
	m.Write([]byte(name))
	return subtle.ConstantTimeCompare(m.Sum(nil), want) == 1
}
