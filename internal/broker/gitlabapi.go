package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"aigate/internal/registrymsg"
)

// W3c GitLab API client (decisions D1-D4). It speaks to ONE provider (a
// registered GitLab) with the administrator token of that provider's token_ref.
//
// Every request goes through (*GitLabAPI).do, which checks (method, escaped
// path shape, query keys) against the table `allowlist` BEFORE anything is
// read or sent. The table is the whole reach of the broker's token:
//
//	GET  /api/v4/groups                                list (fixed query keys)
//	GET  /api/v4/groups/{group path}                   one group
//	POST /api/v4/groups                                create a group
//	GET  /api/v4/projects/{group path/name}            existence check
//	POST /api/v4/projects                              create a project
//	POST /api/v4/projects/{id}/repository/files/{path} first commit (see below)
//	GET  /api/v4/projects/{id}/protected_branches/{name}
//	POST /api/v4/projects/{id}/protected_branches
//	POST /api/v4/projects/{id}/deploy_keys
//
// There is no DELETE, no PUT/PATCH, no project/group update, no merge and no
// token endpoint anywhere in this file.
//
// Why the repository files API and not POST .../repository/commits: the
// official Commits API page says nothing about an EMPTY repository, and a
// public GitLab issue (gitlab-org/gitlab #577329, 18.4.2) reports that the
// commits endpoint answers 404 on an empty project while POST
// /projects/:id/repository/files/:file_path with branch=main works. So the
// first commit is one files-API request per template file (README.md first:
// the first request creates the branch). It is not atomic; a failure midway is
// a create_partial at the call site. Protected-branch updates (PATCH) are not
// used: if a rule for main exists but is weaker than required, EnsureProtectedMain
// refuses (ErrProtectionWeaker) instead of editing it.
//
// No GitLab response text is ever read into an error or a log: non-2xx bodies
// are not even read; 2xx bodies are decoded into small structs of the needed
// fields only. Error values carry fixed codes (CodeError).

const (
	apiConnTimeout = 10 * time.Second
	apiTotalTime   = 15 * time.Second
	apiMaxBody     = 1 << 20
	apiUserAgent   = "orch-broker-gitlab-api/1"
	apiBranch      = "main"
	apiMaxPages    = 5
	apiPerPage     = 100
	maxCommitFiles = 20
	maxFileBytes   = 64 << 10
)

// Fixed error codes of the provider layer (C3 registers their messages).
const (
	CodeProviderUnreachable = "provider_unreachable"
	CodeGitLabRefused       = "gitlab_refused"
	CodeNotFound            = "not_found" // GET 404 (provider-neutral, internal): the call site maps it (group_unknown, ...)
	CodeTokenRefUnknown     = "token_ref_unknown"
	CodeBadURL              = "bad_url"
	CodeProviderUnknown     = "provider_unknown"
	CodeProviderExists      = "provider_exists"
	CodeProviderInUse       = "provider_in_use"
	CodeBadInput            = "bad_request"
	CodeProtectionWeaker    = "protection_weaker"      // internal; WireCode maps it to gitlab_refused
	CodeNotAllowlisted      = "gitlab_not_allowlisted" // a bug, never a user error; not a registered code -> internal_error
)

// CodeError is a failure with a fixed code. Status is the HTTP status class
// GitLab answered (0 when there was no answer); it is a number, not text.
type CodeError struct {
	Code   string
	Status int
}

func (e *CodeError) Error() string { return "broker: " + e.Code }

// Is makes errors.Is(err, ErrX) true for every CodeError with the same code.
func (e *CodeError) Is(target error) bool {
	t, ok := target.(*CodeError)
	return ok && t.Code == e.Code
}

// ErrCode returns the fixed code of err ("" when err is not a CodeError).
func ErrCode(err error) string {
	var ce *CodeError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// WireCode is the code to send to the client: the internal protection_weaker
// is gitlab_refused on the wire. "" for a nil or non-CodeError error.
func WireCode(err error) string {
	c := ErrCode(err)
	if c == CodeProtectionWeaker {
		return CodeGitLabRefused
	}
	return c
}

// ErrStatus returns the HTTP status recorded in err (0 if none).
func ErrStatus(err error) int {
	var ce *CodeError
	if errors.As(err, &ce) {
		return ce.Status
	}
	return 0
}

var (
	ErrProviderUnreachable = &CodeError{Code: CodeProviderUnreachable}
	ErrGitLabRefused       = &CodeError{Code: CodeGitLabRefused}
	ErrNotFound            = &CodeError{Code: CodeNotFound}
	ErrTokenRefUnknown     = &CodeError{Code: CodeTokenRefUnknown}
	ErrBadURL              = &CodeError{Code: CodeBadURL}
	ErrProviderUnknown     = &CodeError{Code: CodeProviderUnknown}
	ErrProviderExists      = &CodeError{Code: CodeProviderExists}
	ErrProviderInUse       = &CodeError{Code: CodeProviderInUse}
	ErrBadInput            = &CodeError{Code: CodeBadInput}
	ErrNotAllowlisted      = &CodeError{Code: CodeNotAllowlisted}
	// ErrProtectionWeaker: a rule for main exists but allows more than
	// Maintainer or force pushes. The client never edits it.
	ErrProtectionWeaker = &CodeError{Code: CodeProtectionWeaker}
)

// ---------- address policy ----------

// ipPolicy reports whether the broker may connect to an address.
type ipPolicy func(netip.Addr) bool

// ipForbidden is the SSRF rule: loopback, unspecified, "this network",
// link-local (incl. 169.254.169.254 and fe80::/10), multicast, broadcast and
// reserved space are refused, also when written as IPv4-mapped IPv6, as
// NAT64 (64:ff9b::/96), 6to4 (2002::/16) or IPv4-compatible (::/96). Private
// (10/8, 172.16/12, 192.168/16, fc00::/7) and tailnet (100.64/10) addresses
// are ALLOWED (the company GitLab is on the tailnet); the only exceptions in
// those ranges are well-known metadata endpoints (100.100.100.200,
// fd00:ec2::254).
func ipForbidden(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" {
		return true
	}
	if a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	if a.Is4() {
		b := a.As4()
		if b[0] == 0 || b[0] >= 240 { // 0/8, 240/4 incl. 255.255.255.255
			return true
		}
		if (b[0] == 192 && b[1] == 0 && b[2] == 0) || (b[0] == 198 && (b[1] == 18 || b[1] == 19)) { // 192.0.0.0/24, 198.18.0.0/15
			return true
		}
		return a == netip.AddrFrom4([4]byte{100, 100, 100, 200})
	}
	b := a.As16()
	zero12 := true
	for _, x := range b[:12] {
		zero12 = zero12 && x == 0
	}
	if zero12 { // ::/96 IPv4-compatible
		return true
	}
	if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && allZero(b[4:12]) { // 64:ff9b::/96
		return ipForbidden(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if b[0] == 0xfe && b[1]&0xc0 == 0xc0 { // fec0::/10 site-local (deprecated)
		return true
	}
	if allZero(b[:8]) && b[8] == 0xff && b[9] == 0xff && b[10] == 0 && b[11] == 0 { // SIIT ::ffff:0:a.b.c.d
		return ipForbidden(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if b[0] == 0x20 && b[1] == 0x02 { // 2002::/16
		return ipForbidden(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return a == netip.MustParseAddr("fd00:ec2::254")
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func defaultIPPolicy(a netip.Addr) bool { return !ipForbidden(a) }

// authorityHost splits the host out of a validated authority (host[:port] or
// [v6][:port]).
func authorityHost(auth string) string {
	if strings.HasPrefix(auth, "[") {
		return auth[1:strings.IndexByte(auth, ']')]
	}
	if i := strings.IndexByte(auth, ':'); i >= 0 {
		return auth[:i]
	}
	return auth
}

var numericLabelRE = regexp.MustCompile(`^(?:0x[0-9a-f]*|[0-9]+)$`)

// checkHostStatic applies the address rules that need no DNS: IP literals go
// through the policy, "localhost" and "*.localhost" are refused.
func checkHostStatic(host string, allow ipPolicy) error {
	if a, err := netip.ParseAddr(host); err == nil {
		if !allow(a) {
			return ErrBadURL
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ErrBadURL
	}
	// A last label that is a number or 0x-hex ("0x7f000001") is read as an IPv4
	// address by some resolvers (inet_aton): never a host name here.
	last := host[strings.LastIndexByte(host, '.')+1:]
	if numericLabelRE.MatchString(last) {
		return ErrBadURL
	}
	return nil
}

// ValidateProviderURL checks a provider base_url: registrymsg.ValidBaseURL
// shape (http/https, canonical lowercase host, optional port, no userinfo,
// path, query or fragment; numeric host variants such as 0x7f.1 are refused
// there) plus the address rules. Host names are checked again when connecting.
func ValidateProviderURL(s string) error { return validateProviderURL(s, defaultIPPolicy, true) }

// plainHTTPHost: plain http is only for literal private (10/8, 172.16/12,
// 192.168/16, fc00::/7) and tailnet (100.64/10) addresses; host names and
// public addresses need https.
func plainHTTPHost(host string) bool {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsPrivate() || (a.Is4() && a.As4()[0] == 100 && a.As4()[1]&0xc0 == 64)
}

// strict: enforce the http-only-for-private-literals rule (production). Tests
// with a plain-http fake on loopback pass false through apiSeams.
func validateProviderURL(s string, allow ipPolicy, strict bool) error {
	if !registrymsg.ValidBaseURL(s) {
		return ErrBadURL
	}
	scheme, rest, _ := strings.Cut(s, "://")
	host := authorityHost(rest)
	if strict && scheme == "http" && !plainHTTPHost(host) {
		return ErrBadURL
	}
	return checkHostStatic(host, allow)
}

// ValidateSSHURL checks an ssh_url: ssh://git@host[:port] shape plus the same
// address rules (orchd connects there, not the broker).
func ValidateSSHURL(s string) error { return validateSSHURL(s, defaultIPPolicy) }

func validateSSHURL(s string, allow ipPolicy) error {
	if !registrymsg.ValidSSHURL(s) {
		return ErrBadURL
	}
	return checkHostStatic(authorityHost(strings.TrimPrefix(s, "ssh://git@")), allow)
}

// CheckProviderResolves resolves the host of a (validated) base URL with
// lookup and refuses it when no address is allowed. The connection itself
// re-checks every address (DNS rebinding).
func CheckProviderResolves(ctx context.Context, baseURL string, lookup func(context.Context, string) ([]netip.Addr, error)) error {
	return checkResolves(ctx, baseURL, lookup, defaultIPPolicy)
}

func checkResolves(ctx context.Context, baseURL string, lookup func(context.Context, string) ([]netip.Addr, error), allow ipPolicy) error {
	if err := validateProviderURL(baseURL, allow, true); err != nil {
		return err
	}
	_, rest, _ := strings.Cut(baseURL, "://")
	host := authorityHost(rest)
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	ips, err := lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return ErrProviderUnreachable
	}
	for _, ip := range ips {
		if allow(ip) {
			return nil
		}
	}
	return ErrBadURL
}

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// ---------- allowlist ----------

const (
	reSeg   = `[a-z][a-z0-9-]{1,39}`
	reGroup = reSeg + `(?:%2F` + reSeg + `){0,3}`
	reProj  = reSeg + `(?:%2F` + reSeg + `){1,4}`
	reID    = `[0-9]{1,18}`
	reFSeg  = `[A-Za-z0-9._-]{1,64}`
	reFile  = reFSeg + `(?:%2F` + reFSeg + `){0,3}`
	reBr    = `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`
)

type allowEntry struct {
	method string
	path   *regexp.Regexp
	query  []string // allowed query keys (none: no query at all)
}

func al(method, re string, q ...string) allowEntry {
	return allowEntry{method, regexp.MustCompile(`^/api/v4/` + re + `$`), q}
}

// allowlist is the complete set of (method, escaped path shape) the client may
// send. TestOnlyAllowlistedRequestsReachGitLab and TestDoRefusesOffListRequests
// pin it.
var allowlist = []allowEntry{
	al("GET", `groups`, "min_access_level", "order_by", "sort", "per_page", "page", "top_level_only"),
	al("GET", `groups/`+reGroup),
	al("POST", `groups`),
	al("GET", `projects/`+reProj),
	al("POST", `projects`),
	al("POST", `projects/`+reID+`/repository/files/`+reFile),
	al("GET", `projects/`+reID+`/protected_branches/`+reBr),
	al("POST", `projects/`+reID+`/protected_branches`),
	al("POST", `projects/`+reID+`/deploy_keys`),
}

func allowed(method, path string, q url.Values) bool {
	for _, e := range allowlist {
		if e.method != method || !e.path.MatchString(path) {
			continue
		}
		if strings.Contains(path, "/repository/files/") {
			// no "." or ".." segment may pass the file shape
			f := path[strings.Index(path, "/repository/files/")+len("/repository/files/"):]
			for _, seg := range strings.Split(f, "%2F") {
				if seg == "." || seg == ".." {
					return false
				}
			}
		}
		for k := range q {
			ok := false
			for _, a := range e.query {
				ok = ok || a == k
			}
			if !ok {
				return false
			}
		}
		return true
	}
	return false
}

// ---------- client ----------

// apiSeams are the test-only replacements of the address policy, the resolver
// and the timeouts. Production code never sets them: GitLabAPI built by
// NewGitLabAPI and by Server.GitLabAPIFor has no seams unless the Server's own
// unexported field was set by a test (TestProductionCannotEnableSeams).
type apiSeams struct {
	policy ipPolicy
	lookup func(context.Context, string) ([]netip.Addr, error)
	conn   time.Duration
	total  time.Duration
	// plainHTTP lifts the http-only-for-private-literals rule (the fakes are
	// http on loopback). A custom policy implies it.
	plainHTTP bool
}

// strictHTTP reports whether the plain-http rule applies (always in production).
func (sm *apiSeams) strictHTTP() bool { return sm == nil || (sm.policy == nil && !sm.plainHTTP) }

// GitLabAPI is the client of one provider. Safe for concurrent use. It holds
// no token: the value is read from the token store for every request.
type GitLabAPI struct {
	base     string
	tokenRef string
	tokens   *TokenStore
	policy   ipPolicy
	lookup   func(context.Context, string) ([]netip.Addr, error)
	hc       *http.Client
}

// NewGitLabAPI builds the client of provider p. The base URL must pass
// ValidateProviderURL.
func NewGitLabAPI(p Provider, tokens *TokenStore) (*GitLabAPI, error) {
	return newGitLabAPI(p, tokens, nil)
}

func newGitLabAPI(p Provider, tokens *TokenStore, sm *apiSeams) (*GitLabAPI, error) {
	a := &GitLabAPI{tokenRef: p.TokenRef, tokens: tokens, policy: defaultIPPolicy, lookup: defaultLookup}
	conn, total := apiConnTimeout, apiTotalTime
	if sm != nil {
		if sm.policy != nil {
			a.policy = sm.policy
		}
		if sm.lookup != nil {
			a.lookup = sm.lookup
		}
		if sm.conn > 0 {
			conn = sm.conn
		}
		if sm.total > 0 {
			total = sm.total
		}
	}
	if p.Kind != "gitlab" || !registrymsg.ValidTokenRef(p.TokenRef) {
		return nil, ErrBadInput
	}
	if err := validateProviderURL(p.BaseURL, a.policy, sm.strictHTTP()); err != nil {
		return nil, err
	}
	a.base = p.BaseURL
	dialer := &net.Dialer{
		Timeout: conn,
		// Second layer: whatever address the connection really goes to is
		// checked once more by the kernel-level hook.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !a.policy(ap.Addr()) {
				return ErrBadURL
			}
			return nil
		},
	}
	a.hc = &http.Client{
		Timeout: total,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         a.dialContext(dialer),
			TLSHandshakeTimeout: conn,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return a, nil
}

// dialContext resolves the host itself, re-checks EVERY resolved address at
// connect time (DNS rebinding: the name may have changed since set_provider)
// and connects to an address that passed.
func (a *GitLabAPI) dialContext(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, ErrBadURL
		}
		var ips []netip.Addr
		if ip, perr := netip.ParseAddr(host); perr == nil {
			ips = []netip.Addr{ip}
		} else if ips, err = a.lookup(ctx, host); err != nil {
			return nil, ErrProviderUnreachable
		}
		var last error = ErrBadURL
		for _, ip := range ips {
			if !a.policy(ip) {
				continue
			}
			c, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if derr == nil {
				return c, nil
			}
			last = derr
		}
		return nil, last
	}
}

// do is the ONLY place a request is built and sent. path is the escaped path
// (each group/project path joined with %2F). wantStatus is the one success
// status; anything else becomes a fixed CodeError. The body of a non-2xx
// answer is never read.
func (a *GitLabAPI) do(ctx context.Context, method, path string, q url.Values, body []byte, wantStatus int) ([]byte, error) {
	if !allowed(method, path, q) {
		return nil, ErrNotAllowlisted // before any token read or network access
	}
	token, ok := a.token()
	if !ok {
		return nil, ErrTokenRefUnknown
	}
	u := a.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, ErrProviderUnreachable
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", apiUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req) // url.Error would quote the URL: never returned
	if err != nil {
		if errors.Is(err, ErrBadURL) { // the dial-time address policy refused
			return nil, ErrBadURL
		}
		return nil, ErrProviderUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		return nil, statusError(resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, apiMaxBody+1))
	if err != nil || len(raw) > apiMaxBody {
		return nil, ErrProviderUnreachable
	}
	return raw, nil
}

// statusError maps an unwanted HTTP status to a fixed code.
func statusError(st int) error {
	switch {
	case st == http.StatusNotFound:
		return &CodeError{Code: CodeNotFound, Status: st}
	case st >= 500 || st == http.StatusTooManyRequests || (st >= 300 && st < 400):
		return &CodeError{Code: CodeProviderUnreachable, Status: st}
	default: // 401, 403, 400, 409, 422, other 4xx and unexpected 2xx
		return &CodeError{Code: CodeGitLabRefused, Status: st}
	}
}

// token reads the provider token for this request only.
func (a *GitLabAPI) token() (string, bool) {
	v, ok := a.tokens.ValueOfKind(a.tokenRef, "gitlab")
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// ValueOfKind is Value restricted to a token of the given kind.
func (t *TokenStore) ValueOfKind(ref, kind string) (string, bool) {
	if t == nil {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Meta.broken {
		return "", false
	}
	i := t.find(ref)
	if i < 0 || t.Meta.entries[i].Kind != kind {
		return "", false
	}
	return t.Secrets.Get(ref)
}

// ---------- decoding ----------

func decodeFields(raw []byte, v any) bool { return decodeOne(raw, v) }

type groupJSON struct {
	ID       *int64  `json:"id"`
	Name     *string `json:"name"`
	Path     *string `json:"path"`
	FullPath *string `json:"full_path"`
}

// Group is the part of a GitLab group the broker reads.
type Group struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	FullPath string `json:"full_path"`
}

func (g groupJSON) ok() (Group, bool) {
	if g.ID == nil || *g.ID <= 0 || g.Name == nil || g.Path == nil || g.FullPath == nil ||
		!registrymsg.ValidGroupPath(*g.FullPath) || !registrymsg.ValidName(*g.Path) ||
		!strings.HasSuffix("/"+*g.FullPath, "/"+*g.Path) || !printable(*g.Name) {
		return Group{}, false
	}
	return Group{ID: *g.ID, Name: *g.Name, Path: *g.Path, FullPath: *g.FullPath}, true
}

func printable(s string) bool {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ProjectInfo is the part of a GitLab project the broker reads.
type ProjectInfo struct {
	ID                int64
	PathWithNamespace string
	// DefaultBranch is the project's default branch when GitLab names one that
	// fits the branch rule ("" when absent, e.g. on an empty project).
	DefaultBranch string
}

type projectJSON struct {
	ID                *int64  `json:"id"`
	PathWithNamespace *string `json:"path_with_namespace"`
	DefaultBranch     *string `json:"default_branch"`
}

var branchNameRE = regexp.MustCompile(`^` + reBr + `$`)

func (p projectJSON) ok(want string) (ProjectInfo, bool) {
	if p.ID == nil || *p.ID <= 0 || p.PathWithNamespace == nil || *p.PathWithNamespace != want {
		return ProjectInfo{}, false
	}
	info := ProjectInfo{ID: *p.ID, PathWithNamespace: want}
	if p.DefaultBranch != nil && branchNameRE.MatchString(*p.DefaultBranch) {
		info.DefaultBranch = *p.DefaultBranch
	}
	return info, true
}

func esc(path string) string { return strings.ReplaceAll(path, "/", "%2F") }

func idPath(id int64) (string, bool) {
	if id <= 0 {
		return "", false
	}
	return strconv.FormatInt(id, 10), true
}

func badResponse() error { return &CodeError{Code: CodeGitLabRefused} }

func postJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// ---------- operations ----------

// Groups lists the groups the token is at least Maintainer in (pages of 100,
// at most 5 pages). Entries whose path does not fit our shape are skipped
// (the broker could not create a project there anyway).
func (a *GitLabAPI) Groups(ctx context.Context) ([]Group, error) {
	out, _, err := a.GroupsTruncated(ctx)
	return out, err
}

// GroupsTruncated is Groups plus a flag that tells the list is not complete:
// the 5-page cap was reached with a full last page, or an entry was skipped
// because its path does not fit our name rules.
func (a *GitLabAPI) GroupsTruncated(ctx context.Context) (out []Group, truncated bool, err error) {
	for page := 1; page <= apiMaxPages; page++ {
		q := url.Values{"min_access_level": {"40"}, "order_by": {"path"}, "sort": {"asc"},
			"per_page": {strconv.Itoa(apiPerPage)}, "page": {strconv.Itoa(page)}}
		raw, err := a.do(ctx, http.MethodGet, "/api/v4/groups", q, nil, http.StatusOK)
		if err != nil {
			return nil, false, err
		}
		var list []groupJSON
		if !decodeFields(raw, &list) {
			return nil, false, badResponse()
		}
		for _, g := range list {
			if gr, ok := g.ok(); ok {
				out = append(out, gr)
			} else {
				truncated = true
			}
		}
		if len(list) < apiPerPage {
			break
		}
		if page == apiMaxPages {
			truncated = true
		}
	}
	return out, truncated, nil
}

// Group returns the group at path (ErrNotFound when GitLab has none).
func (a *GitLabAPI) Group(ctx context.Context, path string) (Group, error) {
	if !registrymsg.ValidGroupPath(path) {
		return Group{}, ErrBadInput
	}
	raw, err := a.do(ctx, http.MethodGet, "/api/v4/groups/"+esc(path), nil, nil, http.StatusOK)
	if err != nil {
		return Group{}, err
	}
	var j groupJSON
	g, ok := Group{}, false
	if decodeFields(raw, &j) {
		g, ok = j.ok()
	}
	if !ok || g.FullPath != path {
		return Group{}, badResponse()
	}
	return g, nil
}

// CreateGroup creates a private group: path is one slug (it doubles as name
// when name is the same rule), parentPath "" means top level. The numeric
// parent id is looked up first (GET), never taken from the caller.
func (a *GitLabAPI) CreateGroup(ctx context.Context, name, path, parentPath string) (Group, error) {
	if !registrymsg.ValidName(name) || !registrymsg.ValidName(path) || (parentPath != "" && !registrymsg.ValidGroupPath(parentPath)) {
		return Group{}, ErrBadInput
	}
	body := map[string]any{"name": name, "path": path, "visibility": "private"}
	full := path
	if parentPath != "" {
		parent, err := a.Group(ctx, parentPath)
		if err != nil {
			return Group{}, err
		}
		body["parent_id"] = parent.ID
		full = parentPath + "/" + path
	}
	raw, err := a.do(ctx, http.MethodPost, "/api/v4/groups", nil, postJSON(body), http.StatusCreated)
	if err != nil {
		return Group{}, err
	}
	var j groupJSON
	g, ok := Group{}, false
	if decodeFields(raw, &j) {
		g, ok = j.ok()
	}
	if !ok || g.FullPath != full {
		return Group{}, badResponse()
	}
	return g, nil
}

// Project looks a project up by "group/path/name" (ErrNotFound when absent).
func (a *GitLabAPI) Project(ctx context.Context, fullPath string) (ProjectInfo, error) {
	i := strings.LastIndexByte(fullPath, '/')
	if i < 0 || !registrymsg.ValidGroupPath(fullPath[:i]) || !registrymsg.ValidName(fullPath[i+1:]) {
		return ProjectInfo{}, ErrBadInput
	}
	raw, err := a.do(ctx, http.MethodGet, "/api/v4/projects/"+esc(fullPath), nil, nil, http.StatusOK)
	if err != nil {
		return ProjectInfo{}, err
	}
	var j projectJSON
	if !decodeFields(raw, &j) {
		return ProjectInfo{}, badResponse()
	}
	p, ok := j.ok(fullPath)
	if !ok {
		return ProjectInfo{}, badResponse()
	}
	return p, nil
}

// CreateProject creates a private, empty project (no README) named name in the
// group with the given numeric id and path (groupPath is only used to verify
// the answer). default_branch is NOT sent: the docs say it needs
// initialize_with_readme; the first commit creates main.
func (a *GitLabAPI) CreateProject(ctx context.Context, groupPath string, namespaceID int64, name, description string) (ProjectInfo, error) {
	if !registrymsg.ValidGroupPath(groupPath) || namespaceID <= 0 || !registrymsg.ValidName(name) || !registrymsg.ValidDescription(description) {
		return ProjectInfo{}, ErrBadInput
	}
	body := map[string]any{"name": name, "path": name, "namespace_id": namespaceID,
		"visibility": "private", "initialize_with_readme": false, "description": description}
	raw, err := a.do(ctx, http.MethodPost, "/api/v4/projects", nil, postJSON(body), http.StatusCreated)
	if err != nil {
		return ProjectInfo{}, err
	}
	var j projectJSON
	if !decodeFields(raw, &j) {
		return ProjectInfo{}, badResponse()
	}
	p, ok := j.ok(groupPath + "/" + name)
	if !ok {
		return ProjectInfo{}, badResponse()
	}
	return p, nil
}

// CommitFile is one file of the first commit.
type CommitFile struct {
	Path    string
	Content string
}

var fileSegRE = regexp.MustCompile(`^` + reFSeg + `$`)

func validCommitFiles(files []CommitFile) bool {
	if len(files) == 0 || len(files) > maxCommitFiles {
		return false
	}
	seen := map[string]bool{}
	for _, f := range files {
		segs := strings.Split(f.Path, "/")
		if len(segs) > 4 || seen[f.Path] || len(f.Content) > maxFileBytes || !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) {
			return false
		}
		for _, s := range segs {
			if !fileSegRE.MatchString(s) || s == "." || s == ".." {
				return false
			}
		}
		seen[f.Path] = true
	}
	return true
}

// CommitFiles writes the files to branch main of an EMPTY project, one files-API
// request per file in the given order (the first creates the branch). Not
// atomic: an error means some files may already be committed.
func (a *GitLabAPI) CommitFiles(ctx context.Context, projectID int64, files []CommitFile) error {
	ids, ok := idPath(projectID)
	if !ok || !validCommitFiles(files) {
		return ErrBadInput
	}
	for _, f := range files {
		body := map[string]any{"branch": apiBranch, "content": f.Content, "commit_message": "Add " + f.Path}
		raw, err := a.do(ctx, http.MethodPost, "/api/v4/projects/"+ids+"/repository/files/"+esc(f.Path), nil, postJSON(body), http.StatusCreated)
		if err != nil {
			return err
		}
		var r struct {
			FilePath *string `json:"file_path"`
			Branch   *string `json:"branch"`
		}
		if !decodeFields(raw, &r) || r.FilePath == nil || *r.FilePath != f.Path || r.Branch == nil || *r.Branch != apiBranch {
			return badResponse()
		}
	}
	return nil
}

// ProtectedBranch is the part of a protected-branch rule the broker reads.
type ProtectedBranch struct {
	PushLevels     []int
	MergeLevels    []int
	AllowForcePush bool
	// Scoped: some push/merge entry names a user, group or deploy key (any
	// non-null, non-zero user_id/group_id/deploy_key_id): counts as weaker.
	Scoped bool
}

// Strong reports whether the rule is at least "Maintainer may push and merge,
// no force push": every access level is 40 or higher, or 0 (No one).
func (p ProtectedBranch) Strong() bool {
	good := func(ls []int) bool {
		if len(ls) == 0 {
			return false
		}
		for _, l := range ls {
			if l != 0 && l < 40 {
				return false
			}
		}
		return true
	}
	return !p.AllowForcePush && !p.Scoped && good(p.PushLevels) && good(p.MergeLevels)
}

type levelJSON struct {
	AccessLevel *int   `json:"access_level"`
	UserID      *int64 `json:"user_id"`
	GroupID     *int64 `json:"group_id"`
	DeployKeyID *int64 `json:"deploy_key_id"`
}

func (l levelJSON) scoped() bool {
	return (l.UserID != nil && *l.UserID != 0) || (l.GroupID != nil && *l.GroupID != 0) || (l.DeployKeyID != nil && *l.DeployKeyID != 0)
}

type protectedJSON struct {
	Name           *string     `json:"name"`
	AllowForcePush *bool       `json:"allow_force_push"`
	Push           []levelJSON `json:"push_access_levels"`
	Merge          []levelJSON `json:"merge_access_levels"`
}

func levels(l []levelJSON) ([]int, bool) {
	out := make([]int, 0, len(l))
	for _, x := range l {
		if x.AccessLevel == nil {
			return nil, false
		}
		out = append(out, *x.AccessLevel)
	}
	return out, true
}

// ProtectedMain reads the protection rule of main (ErrNotFound: none).
func (a *GitLabAPI) ProtectedMain(ctx context.Context, projectID int64) (ProtectedBranch, error) {
	ids, ok := idPath(projectID)
	if !ok {
		return ProtectedBranch{}, ErrBadInput
	}
	raw, err := a.do(ctx, http.MethodGet, "/api/v4/projects/"+ids+"/protected_branches/"+apiBranch, nil, nil, http.StatusOK)
	if err != nil {
		return ProtectedBranch{}, err
	}
	var j protectedJSON
	if !decodeFields(raw, &j) || j.Name == nil || *j.Name != apiBranch || j.AllowForcePush == nil {
		return ProtectedBranch{}, badResponse()
	}
	push, ok1 := levels(j.Push)
	merge, ok2 := levels(j.Merge)
	if !ok1 || !ok2 {
		return ProtectedBranch{}, badResponse()
	}
	scoped := false
	for _, l := range append(append([]levelJSON(nil), j.Push...), j.Merge...) {
		scoped = scoped || l.scoped()
	}
	return ProtectedBranch{PushLevels: push, MergeLevels: merge, AllowForcePush: *j.AllowForcePush, Scoped: scoped}, nil
}

// EnsureProtectedMain makes sure main needs Maintainer to push and merge and
// forbids force pushes: it reads the rule, creates it when there is none (404)
// and refuses (ErrProtectionWeaker) when an existing rule is weaker. It never
// edits or removes a rule.
func (a *GitLabAPI) EnsureProtectedMain(ctx context.Context, projectID int64) error {
	pb, err := a.ProtectedMain(ctx, projectID)
	if err == nil {
		if pb.Strong() {
			return nil
		}
		return ErrProtectionWeaker
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	ids, _ := idPath(projectID)
	body := map[string]any{"name": apiBranch, "push_access_level": 40, "merge_access_level": 40, "allow_force_push": false}
	raw, err := a.do(ctx, http.MethodPost, "/api/v4/projects/"+ids+"/protected_branches", nil, postJSON(body), http.StatusCreated)
	if err != nil {
		if st := ErrStatus(err); st == http.StatusConflict || st == http.StatusUnprocessableEntity {
			// GitLab may have created the rule meanwhile (first push): look once more.
			if pb, gerr := a.ProtectedMain(ctx, projectID); gerr == nil {
				if pb.Strong() {
					return nil
				}
				return ErrProtectionWeaker
			}
		}
		return err
	}
	var j protectedJSON
	if !decodeFields(raw, &j) || j.Name == nil || *j.Name != apiBranch {
		return badResponse()
	}
	return nil
}

var deployKeyRE = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521)|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/]{16,1000}={0,2}(?: [!-~]{1,100})?$`)

// AddDeployKey adds one public key line to the project as a deploy key.
func (a *GitLabAPI) AddDeployKey(ctx context.Context, projectID int64, title, key string, canPush bool) error {
	ids, ok := idPath(projectID)
	if !ok || !printable(title) || len(title) > 100 || !deployKeyRE.MatchString(key) {
		return ErrBadInput
	}
	body := map[string]any{"title": title, "key": key, "can_push": canPush}
	raw, err := a.do(ctx, http.MethodPost, "/api/v4/projects/"+ids+"/deploy_keys", nil, postJSON(body), http.StatusCreated)
	if err != nil {
		return err
	}
	var r struct {
		ID *int64 `json:"id"`
	}
	if !decodeFields(raw, &r) || r.ID == nil || *r.ID <= 0 {
		return badResponse()
	}
	return nil
}
