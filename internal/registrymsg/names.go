package registrymsg

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file holds the pure validators of the provider and project-creation
// actions (decision D2). They check SHAPE only: which addresses a provider URL
// may reach (loopback, link-local, ...) is the broker's connection-time rule.

var (
	// providerIDRE: subject of set_provider, delete_provider and create_group.
	providerIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	// nameRE: a GitLab project/group name, a group path segment and the
	// subject of create_project (project id = GitLab name = folder name).
	// It needs at least two characters. localhost-like or reserved-looking
	// names are not special here.
	nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

const (
	// MaxGroupDepth is the most segments a group path may have.
	MaxGroupDepth = 4
	// MaxDescriptionRunes caps a project description, in Unicode code points
	// (a browser must count code points, not UTF-16 units).
	MaxDescriptionRunes = 200
	maxURLBytes         = 200
)

// providerKinds is the closed set of provider kinds (set_provider.kind).
var providerKinds = map[string]bool{"gitlab": true, "github": true}

// ValidProviderKind reports whether s is gitlab or github.
func ValidProviderKind(s string) bool { return providerKinds[s] }

const (
	// MaxRemoteSegments is the most segments a remote path may have
	// (owner/repo for GitHub, group/…/name for GitLab).
	MaxRemoteSegments = 4
	// MaxRemoteSegmentLen caps one segment, MaxRemotePathLen the whole path.
	MaxRemoteSegmentLen = 100
	MaxRemotePathLen    = 255
)

// ValidRemotePath reports whether s names a remote repository: 2 to
// MaxRemoteSegments segments of [A-Za-z0-9._-], no empty segment, no "." or
// ".." segment, no ".git" suffix on the last segment, each at most
// MaxRemoteSegmentLen bytes and the whole at most MaxRemotePathLen. Case is
// accepted as written: folding it (GitHub names are case-insensitive) is the
// broker's call, per provider.
func ValidRemotePath(s string) bool {
	if len(s) > MaxRemotePathLen {
		return false
	}
	segs := strings.Split(s, "/")
	if len(segs) < 2 || len(segs) > MaxRemoteSegments {
		return false
	}
	for i, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || len(seg) > MaxRemoteSegmentLen {
			return false
		}
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return false
			}
		}
		if i == len(segs)-1 && len(seg) >= 4 && strings.EqualFold(seg[len(seg)-4:], ".git") {
			return false
		}
	}
	return true
}

// ValidProviderID reports whether s is a provider id.
func ValidProviderID(s string) bool { return providerIDRE.MatchString(s) }

// ValidName reports whether s is a project or group name (and path segment):
// ^[a-z][a-z0-9-]{1,39}$. There is deliberately no list of GitLab reserved
// words: GitLab is the authority, it refuses such a name itself, and the
// broker reports that as gitlab_refused (C3). A local list would be stale and
// would be coupled to loading an existing registry.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidGroupPath reports whether s is a slash-separated group path of 1 to
// MaxGroupDepth segments, each a ValidName, with no leading, trailing or
// doubled slash.
func ValidGroupPath(s string) bool {
	segs := strings.Split(s, "/")
	if len(segs) < 1 || len(segs) > MaxGroupDepth {
		return false
	}
	for _, seg := range segs {
		if !ValidName(seg) {
			return false
		}
	}
	return true
}

// ValidDescription reports whether s is "" or text of at most
// MaxDescriptionRunes code points without control, format (including bidi
// controls and zero-width characters), line/paragraph separator or
// private-use characters.
func ValidDescription(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	n := 0
	for _, r := range s {
		n++
		if n > MaxDescriptionRunes {
			return false
		}
		if r == utf8.RuneError || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Co, unicode.Cs) {
			return false
		}
	}
	return true
}

// ValidBaseURL reports whether s has the shape http(s)://host[:port] and
// nothing else: lowercase scheme and host, no user information, path ("/"
// included), query or fragment. The broker adds /api/v4 itself.
func ValidBaseURL(s string) bool {
	if len(s) > maxURLBytes {
		return false
	}
	rest, ok := strings.CutPrefix(s, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(s, "https://")
	}
	return ok && validAuthority(rest)
}

// ValidSSHURL reports whether s has the shape ssh://git@host[:port] and
// nothing else.
func ValidSSHURL(s string) bool {
	if len(s) > maxURLBytes {
		return false
	}
	rest, ok := strings.CutPrefix(s, "ssh://git@")
	return ok && validAuthority(rest)
}

// validAuthority accepts host or host:port where host is a lowercase DNS name,
// a canonical IPv4 address or a bracketed canonical IPv6 address.
func validAuthority(a string) bool {
	if a == "" {
		return false
	}
	var host, port string
	if strings.HasPrefix(a, "[") {
		end := strings.IndexByte(a, ']')
		if end < 0 {
			return false
		}
		addr, err := netip.ParseAddr(a[1:end])
		if err != nil || !addr.Is6() || addr.Zone() != "" || addr.String() != a[1:end] {
			return false
		}
		tail := a[end+1:]
		if tail != "" {
			p, ok := strings.CutPrefix(tail, ":")
			if !ok {
				return false
			}
			port = p
			if port == "" {
				return false
			}
		}
		return port == "" || validPort(port)
	}
	if i := strings.IndexByte(a, ':'); i >= 0 {
		host, port = a[:i], a[i+1:]
		if !validPort(port) {
			return false
		}
	} else {
		host = a
	}
	return validHostname(host)
}

func validPort(p string) bool {
	if p == "" || len(p) > 5 || p[0] == '0' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if !hostLabelRE.MatchString(l) {
			return false
		}
	}
	// A name whose last label is all digits can only be an IPv4 address: it
	// must be the canonical dotted quad (no 2130706433, 0x7f.1 or 010.1.1.1).
	last := labels[len(labels)-1]
	allDigits := true
	for i := 0; i < len(last); i++ {
		allDigits = allDigits && last[i] >= '0' && last[i] <= '9'
	}
	if allDigits {
		a, err := netip.ParseAddr(h)
		return err == nil && a.Is4() && a.String() == h
	}
	return true
}
