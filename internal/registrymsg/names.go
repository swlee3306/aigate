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
	// (owner/repo for GitHub; for GitLab a group path of at most MaxGroupDepth
	// segments plus the project name).
	MaxRemoteSegments = MaxGroupDepth + 1
	// MaxRemoteSegmentLen caps one segment, MaxRemotePathLen the whole path.
	MaxRemoteSegmentLen = 100
	MaxRemotePathLen    = 255
)

// ValidRemotePath reports whether s names a remote repository: 2 to
// MaxRemoteSegments segments of [A-Za-z0-9._-], no empty segment, no "." or
// ".." segment, no segment ending in ".git" (anywhere), each at most
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
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || len(seg) > MaxRemoteSegmentLen {
			return false
		}
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return false
			}
		}
		if len(seg) >= 4 && strings.EqualFold(seg[len(seg)-4:], ".git") {
			return false
		}
	}
	return true
}

// ValidRemoteID reports whether s is a repository id as a signed field: a
// decimal string of 1 to 15 digits, greater than zero, no sign, no leading zero
// (15 digits stay exact in a JavaScript number and in an int64).
func ValidRemoteID(s string) bool {
	if len(s) < 1 || len(s) > 15 || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ValidConfirmName reports whether s can be the typed confirmation name of a
// remote repository: one remote-path segment (the repository's own name),
// [A-Za-z0-9._-], 1 to MaxRemoteSegmentLen bytes, not "." or "..", not ending in
// ".git" in any case. Case is kept: the broker compares it exactly with the last
// segment of the signed remote_path.
func ValidConfirmName(s string) bool {
	if s == "" || len(s) > MaxRemoteSegmentLen || strings.Contains(s, "/") {
		return false
	}
	// A path of owner/<name> is valid exactly when <name> is a valid segment.
	return ValidRemotePath("x/" + s)
}

// RemoteLastSegment returns the last segment (the repository name) of a valid
// remote path.
func RemoteLastSegment(path string) (string, bool) {
	if !ValidRemotePath(path) {
		return "", false
	}
	return path[strings.LastIndexByte(path, '/')+1:], true
}

// ValidOrphanProject reports whether s is acceptable as the project of an
// orphan record (and of clear_orphan.project): a GitLab project (lowercase
// group path of at most MaxGroupDepth segments plus a lowercase name) or any
// ValidRemotePath (GitHub owner/repo, case kept).
func ValidOrphanProject(s string) bool {
	i := strings.LastIndexByte(s, '/')
	gitlabShape := i >= 0 && ValidGroupPath(s[:i]) && ValidName(s[i+1:]) && strings.Count(s, "/")+1 <= MaxGroupDepth+1
	return gitlabShape || ValidRemotePath(s)
}

// The GitHub name patterns, shared with the broker's request allowlist so a
// shape accepted here can never be refused there (or the other way round).
const (
	// GitHubLoginPattern: a login of 1-39 characters; `_` is for Enterprise
	// Managed Users logins.
	GitHubLoginPattern = `[A-Za-z0-9][A-Za-z0-9_-]{0,38}`
	// GitHubRepoPattern: a repository name.
	GitHubRepoPattern = `[A-Za-z0-9._-]{1,100}`
)

var (
	ghLoginRE = regexp.MustCompile(`^` + GitHubLoginPattern + `$`)
	ghRepoRE  = regexp.MustCompile(`^` + GitHubRepoPattern + `$`)
)

// ValidGitHubLogin reports whether s is a GitHub account or organization login.
func ValidGitHubLogin(s string) bool { return ghLoginRE.MatchString(s) }

// ValidGitHubOwner is the rule for the SIGNED owner of a GitHub create: one
// login, case kept, no trailing hyphen (a leading one is excluded by the
// pattern).
func ValidGitHubOwner(s string) bool {
	return ValidGitHubLogin(s) && !strings.HasSuffix(s, "-")
}

// ValidGitHubRepoName reports whether s is a GitHub repository name: not "." or
// "..", no ".git" ending in any case.
func ValidGitHubRepoName(s string) bool {
	if !ghRepoRE.MatchString(s) || s == "." || s == ".." {
		return false
	}
	return !(len(s) >= 4 && strings.EqualFold(s[len(s)-4:], ".git"))
}

// SplitGitHubPath splits "owner/repo" (exactly two valid segments).
func SplitGitHubPath(path string) (owner, repo string, ok bool) {
	owner, repo, found := strings.Cut(path, "/")
	if !found || !ValidGitHubLogin(owner) || !ValidGitHubRepoName(repo) {
		return "", "", false
	}
	return owner, repo, true
}

// ValidOwnerForKind is the provider-specific rule for the signed owner field of
// create_project (it is checked by the broker before the signature is looked
// at, and by orchd before anything is proposed): GitLab keeps the lowercase
// group-path rule, GitHub takes one login.
func ValidOwnerForKind(kind, owner string) bool {
	switch kind {
	case "gitlab":
		return ValidGroupPath(owner)
	case "github":
		return ValidGitHubOwner(owner)
	}
	return false
}

// ValidRemotePathForKind reports whether p is a remote path the provider's
// client accepts. GitHub: owner/repo, exactly two segments. GitLab: a group path
// (lowercase names, at most MaxGroupDepth segments) plus a lowercase project
// name.
func ValidRemotePathForKind(kind, p string) bool {
	if !ValidRemotePath(p) {
		return false
	}
	switch kind {
	case "github":
		_, _, ok := SplitGitHubPath(p)
		return ok && strings.Count(p, "/") == 1
	case "gitlab":
		i := strings.LastIndexByte(p, '/')
		return i > 0 && ValidGroupPath(p[:i]) && ValidName(p[i+1:])
	}
	return false
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

// ValidOwnerField reports whether s is acceptable as the SIGNED owner field of
// create_project (`group_path`): a superset of what both providers can name,
// at most MaxOwnerFieldLen bytes of
// [A-Za-z0-9._-] segments joined by "/", 1 to MaxRemoteSegments segments, none
// empty, "." or "..". The provider-specific rule (GitLab: ValidGroupPath;
// GitHub: one login) is enforced by the broker op in its shape stage, before
// the signature is checked.
func ValidOwnerField(s string) bool {
	if len(s) < 1 || len(s) > MaxOwnerFieldLen {
		return false
	}
	segs := strings.Split(s, "/")
	if len(segs) > MaxRemoteSegments {
		return false
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}

// MaxOwnerFieldLen caps the signed owner field.
const MaxOwnerFieldLen = 100

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

// SuggestProjectName turns a remote repository name into a suggestion for the
// project id of link_project (the user may change it; the project id is the
// folder name and key folder on the server and need not equal the repository
// name). Rule, in order: lower-case A-Z; every character outside [a-z0-9-]
// (`_`, `.`, anything else) becomes `-`; runs of `-` collapse; leading and
// trailing `-` are removed; nothing left gives "project"; a name that does not
// start with a letter gets the prefix "p-"; the result is cut to 40 characters
// and trailing `-` trimmed again; a single character gets "-x" appended. The
// result always satisfies ValidName. The browser has the same function
// (suggestProjectName in projects.js, pinned by a Go-generated vector file).
func SuggestProjectName(repo string) string {
	var b strings.Builder
	dash := true // swallows leading and repeated hyphens
	for i := 0; i < len(repo); i++ {
		c := repo[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		s = "project"
	}
	if s[0] < 'a' || s[0] > 'z' {
		s = "p-" + s
	}
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if len(s) < 2 {
		s += "-x"
	}
	return s
}
