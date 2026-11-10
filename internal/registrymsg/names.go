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
	// It needs at least two characters.
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

// reservedNames are path words GitLab keeps for itself (top-level and
// project-level routes) that also fit nameRE. Group path segments and project
// names equal to one of them are refused, so the broker never asks GitLab to
// create something it will reject or, worse, that shadows a route.
var reservedNames = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`admin api assets dashboard explore groups help import jwt login oauth profile
		projects public search snippets unsubscribes uploads users sitemap s v2
		badges blame blob branches builds commit commits compare create edit environments files forks graph graphs
		hooks issues jobs labels new pipelines raw refs repository runners services settings tags tree update wiki wikis`) {
		m[w] = true
	}
	return m
}()

// ValidProviderID reports whether s is a provider id.
func ValidProviderID(s string) bool { return providerIDRE.MatchString(s) }

// ValidName reports whether s is a project or group name (and path segment):
// ^[a-z][a-z0-9-]{1,39}$ and not a GitLab reserved word.
func ValidName(s string) bool { return nameRE.MatchString(s) && !reservedNames[s] }

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
