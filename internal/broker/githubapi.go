package broker

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"aigate/internal/registrymsg"
)

// W3e GitHub API client (E2). Same shape as the GitLab client: it speaks to ONE
// provider (a registered GitHub or GitHub Enterprise Server) with the token of
// that provider's token_ref, and every request goes through (*GitHubAPI).do,
// which checks (method, path shape, query keys) against `githubAllowlist`
// BEFORE the token is read or anything is sent. The table is the whole reach
// of the broker's GitHub token:
//
//	GET  /user                                          the token's own account
//	GET  /user/orgs                                     organizations (page, per_page)
//	GET  /repos/{owner}/{repo}                          existence, default branch
//	POST /user/repos                                    create a repository (own account)
//	POST /orgs/{org}/repos                              create a repository (organization)
//	PUT  /repos/{owner}/{repo}/contents/{path}          first commit, one file per call
//	GET  /repos/{owner}/{repo}/branches/{branch}/protection
//	PUT  /repos/{owner}/{repo}/branches/{branch}/protection   only for a repo created in THIS flow
//	POST /repos/{owner}/{repo}/keys                     deploy key
//
// There is no DELETE, no PATCH, no merge, no fork/transfer/hook and no token
// endpoint anywhere in this file. (PUT appears twice, both create-only:
// a new file without `sha`, and the protection of a repository this flow just
// created; an existing protection rule is only ever read.)
//
// No GitHub response text is read into an error or a log: non-2xx bodies are not
// read at all; the status class decides the fixed code. 2xx bodies are decoded
// into small structs of the needed fields and cross-checked against what was
// asked. Only the rate-limit HEADERS of an error answer are looked at (to tell
// a rate limit from a refusal), never their text. Pagination: page=N up to
// apiMaxPages; Link headers are never read or followed.
//
// Names are used as written in the path: the owner and repository charsets
// exclude every character that would need percent-encoding, so nothing is
// encoded (and nothing can be encoded twice or smuggle a separator). GitHub
// treats names case-insensitively: a case variant that exists counts as taken.

const (
	githubAPIVersion = "2022-11-28" // supported until 2028-03-10 per the official versions page
	githubAccept     = "application/vnd.github+json"
	githubUserAgent  = "orch-broker-github-api/1"
	githubHost       = "api.github.com"
)

// Fixed codes of the GitHub client (registered in fixedMessages).
const (
	CodeGitHubRefused       = "github_refused"
	CodeProviderRateLimited = "provider_rate_limited"
)

var (
	ErrGitHubRefused       = &CodeError{Code: CodeGitHubRefused}
	ErrProviderRateLimited = &CodeError{Code: CodeProviderRateLimited}
)

// ---------- allowlist ----------

const (
	ghOwner = `[A-Za-z0-9][A-Za-z0-9-]{0,38}`
	ghRepo  = `[A-Za-z0-9._-]{1,100}`
	ghFile  = reFSeg + `(?:/` + reFSeg + `){0,3}`
)

func gal(method, re string, q ...string) allowEntry {
	return allowEntry{method, regexp.MustCompile(`^/` + re + `$`), q}
}

// githubAllowlist is the complete set of (method, path shape) the client may
// send, relative to the API root. TestGitHubOnlyAllowlistedRequests and
// TestGitHubDoRefusesOffList pin it.
var githubAllowlist = []allowEntry{
	gal("GET", `user`),
	gal("GET", `user/orgs`, "per_page", "page"),
	gal("GET", `repos/`+ghOwner+`/`+ghRepo),
	gal("POST", `user/repos`),
	gal("POST", `orgs/`+ghOwner+`/repos`),
	gal("PUT", `repos/`+ghOwner+`/`+ghRepo+`/contents/`+ghFile),
	gal("GET", `repos/`+ghOwner+`/`+ghRepo+`/branches/`+reBr+`/protection`),
	gal("PUT", `repos/`+ghOwner+`/`+ghRepo+`/branches/`+reBr+`/protection`),
	gal("POST", `repos/`+ghOwner+`/`+ghRepo+`/keys`),
}

func githubAllowed(method, path string, q url.Values) bool {
	for _, e := range githubAllowlist {
		if e.method != method || !e.path.MatchString(path) {
			continue
		}
		segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
		if segs[0] == "repos" {
			if segs[2] == "." || segs[2] == ".." {
				return false
			}
			if len(segs) > 4 {
				for _, s := range segs[4:] {
					if s == "." || s == ".." {
						return false
					}
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

// GitHubAPI is the client of one GitHub provider. Safe for concurrent use. It
// holds no token: the value is read from the token store for every request.
type GitHubAPI struct {
	base     string
	prefix   string // "" for api.github.com, "/api/v3" for an Enterprise Server host
	tokenRef string
	tokens   *TokenStore
	policy   ipPolicy
	lookup   func(context.Context, string) ([]netip.Addr, error)
	hc       *http.Client
}

// githubAPIPrefix is the kind-aware part of the API address: github.com serves
// the API on api.github.com, an Enterprise Server host under /api/v3.
func githubAPIPrefix(base string) string {
	_, rest, _ := strings.Cut(base, "://")
	if authorityHost(rest) == githubHost {
		return ""
	}
	return "/api/v3"
}

// NewGitHubAPI builds the client of provider p. The base URL must pass the
// provider URL rules (https; http only for private/tailnet literals).
func NewGitHubAPI(p Provider, tokens *TokenStore) (*GitHubAPI, error) {
	return newGitHubAPI(p, tokens, nil)
}

func newGitHubAPI(p Provider, tokens *TokenStore, sm *apiSeams) (*GitHubAPI, error) {
	a := &GitHubAPI{tokenRef: p.TokenRef, tokens: tokens, policy: defaultIPPolicy, lookup: defaultLookup}
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
	if p.Kind != "github" || !registrymsg.ValidTokenRef(p.TokenRef) {
		return nil, ErrBadInput
	}
	if err := validateProviderURL(p.BaseURL, a.policy, sm.strictHTTP()); err != nil {
		return nil, err
	}
	a.base = p.BaseURL
	a.prefix = githubAPIPrefix(p.BaseURL)
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
// connect time (DNS rebinding) and connects to an address that passed. Public
// addresses (api.github.com) pass the policy; loopback, link-local and
// metadata addresses never do.
func (a *GitHubAPI) dialContext(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
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

// do is the ONLY place a request is built and sent. path is the unprefixed API
// path. wantStatus is the one success status; anything else becomes a fixed
// CodeError. The body of a non-2xx answer is never read.
func (a *GitHubAPI) do(ctx context.Context, method, path string, q url.Values, body []byte, wantStatus int) ([]byte, error) {
	if !githubAllowed(method, path, q) {
		return nil, ErrNotAllowlisted // before any token read or network access
	}
	token, ok := a.token()
	if !ok {
		return nil, ErrTokenRefUnknown
	}
	u := a.base + a.prefix + path
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", githubAccept)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)
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
		return nil, githubStatusError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, apiMaxBody+1))
	if err != nil || len(raw) > apiMaxBody {
		return nil, ErrProviderUnreachable
	}
	return raw, nil
}

// githubStatusError maps an unwanted answer to a fixed code from the status
// class (and, for 403, the rate-limit headers; never the body).
func githubStatusError(resp *http.Response) error {
	st := resp.StatusCode
	switch {
	case st == http.StatusNotFound:
		return &CodeError{Code: CodeNotFound, Status: st}
	case st == http.StatusTooManyRequests:
		return &CodeError{Code: CodeProviderRateLimited, Status: st}
	case st == http.StatusForbidden && (resp.Header.Get("Retry-After") != "" || strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0"):
		return &CodeError{Code: CodeProviderRateLimited, Status: st}
	case st >= 500 || (st >= 300 && st < 400):
		return &CodeError{Code: CodeProviderUnreachable, Status: st}
	default: // 401, 403, 400, 409, 422, other 4xx and unexpected 2xx
		return &CodeError{Code: CodeGitHubRefused, Status: st}
	}
}

func ghBadResponse() error { return &CodeError{Code: CodeGitHubRefused} }

// token reads the provider token for this request only.
func (a *GitHubAPI) token() (string, bool) {
	v, ok := a.tokens.ValueOfKind(a.tokenRef, "github")
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// ---------- names ----------

var (
	ghLoginRE = regexp.MustCompile(`^` + ghOwner + `$`)
	ghRepoRE  = regexp.MustCompile(`^` + ghRepo + `$`)
)

func validGHLogin(s string) bool { return ghLoginRE.MatchString(s) }

func validGHRepoName(s string) bool {
	if !ghRepoRE.MatchString(s) || s == "." || s == ".." {
		return false
	}
	return !(len(s) >= 4 && strings.EqualFold(s[len(s)-4:], ".git"))
}

// splitGH splits "owner/repo" (exactly two valid segments).
func splitGH(path string) (owner, repo string, ok bool) {
	owner, repo, found := strings.Cut(path, "/")
	if !found || !validGHLogin(owner) || !validGHRepoName(repo) {
		return "", "", false
	}
	return owner, repo, true
}

func validGHRepoInfo(r RepoInfo) (owner, repo string, ok bool) {
	owner, repo, ok = splitGH(r.Path)
	return owner, repo, ok && r.ID > 0
}

// ---------- decoding ----------

type ghRepoJSON struct {
	ID            *int64  `json:"id"`
	FullName      *string `json:"full_name"`
	DefaultBranch *string `json:"default_branch"`
	Private       *bool   `json:"private"`
}

func (j ghRepoJSON) ok(want string) (RepoInfo, bool) {
	if j.ID == nil || *j.ID <= 0 || j.FullName == nil || !strings.EqualFold(*j.FullName, want) {
		return RepoInfo{}, false
	}
	if _, _, ok := splitGH(*j.FullName); !ok {
		return RepoInfo{}, false
	}
	info := RepoInfo{ID: *j.ID, Path: *j.FullName}
	if j.DefaultBranch != nil && branchNameRE.MatchString(*j.DefaultBranch) {
		info.DefaultBranch = *j.DefaultBranch
	}
	return info, true
}

// ---------- operations ----------

func (a *GitHubAPI) Kind() string { return "github" }

// ListOwners returns the authenticated account first, then its organizations
// (pages of 100, at most apiMaxPages pages, by page number only: Link headers
// are never followed). Entries whose login does not fit the name rules are
// skipped and flagged as truncated.
func (a *GitHubAPI) ListOwners(ctx context.Context) ([]Owner, bool, error) {
	raw, err := a.do(ctx, http.MethodGet, "/user", nil, nil, http.StatusOK)
	if err != nil {
		return nil, false, err
	}
	var me struct {
		Login *string `json:"login"`
	}
	if !decodeFields(raw, &me) || me.Login == nil || !validGHLogin(*me.Login) {
		return nil, false, ghBadResponse()
	}
	out := []Owner{{Name: *me.Login, Path: *me.Login, Kind: "user", fromProvider: true}}
	truncated := false
	for page := 1; page <= apiMaxPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(apiPerPage)}, "page": {strconv.Itoa(page)}}
		raw, err := a.do(ctx, http.MethodGet, "/user/orgs", q, nil, http.StatusOK)
		if err != nil {
			return nil, false, err
		}
		var list []struct {
			Login *string `json:"login"`
		}
		if !decodeFields(raw, &list) {
			return nil, false, ghBadResponse()
		}
		for _, o := range list {
			if o.Login != nil && validGHLogin(*o.Login) {
				out = append(out, Owner{Name: *o.Login, Path: *o.Login, Kind: "org", fromProvider: true})
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

// GetOwner finds path among the token's owners (case-insensitive) and returns
// it with GitHub's own spelling; ErrNotFound when the token has no such owner.
func (a *GitHubAPI) GetOwner(ctx context.Context, path string) (Owner, error) {
	if !validGHLogin(path) {
		return Owner{}, ErrBadInput
	}
	owners, _, err := a.ListOwners(ctx)
	if err != nil {
		return Owner{}, err
	}
	for _, o := range owners {
		if strings.EqualFold(o.Path, path) {
			return o, nil
		}
	}
	return Owner{}, ErrNotFound
}

// GetRepo looks a repository up by "owner/repo" (ErrNotFound when absent). A
// case variant of an existing repository is that repository: the answer
// carries GitHub's spelling and the caller treats it as taken.
func (a *GitHubAPI) GetRepo(ctx context.Context, fullPath string) (RepoInfo, error) {
	owner, repo, ok := splitGH(fullPath)
	if !ok {
		return RepoInfo{}, ErrBadInput
	}
	raw, err := a.do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, nil, nil, http.StatusOK)
	if err != nil {
		return RepoInfo{}, err
	}
	var j ghRepoJSON
	if !decodeFields(raw, &j) {
		return RepoInfo{}, ghBadResponse()
	}
	info, ok := j.ok(fullPath)
	if !ok {
		return RepoInfo{}, ghBadResponse()
	}
	return info, nil
}

// DefaultBranch reads the default branch of the repository.
func (a *GitHubAPI) DefaultBranch(ctx context.Context, repo RepoInfo) (string, error) {
	if _, _, ok := validGHRepoInfo(repo); !ok {
		return "", ErrBadInput
	}
	info, err := a.GetRepo(ctx, repo.Path)
	if err != nil {
		return "", err
	}
	return info.DefaultBranch, nil
}

// CreateRepo creates an empty repository (auto_init false) under an owner that
// came from ListOwners/GetOwner. private is the signed visibility; the answer
// must confirm owner/name AND that visibility, or the call is an error (the
// repository then exists: the caller records it as an orphan).
func (a *GitHubAPI) CreateRepo(ctx context.Context, req CreateRepoReq) (RepoInfo, error) {
	o := req.Owner
	if !o.fromProvider || (o.Kind != "user" && o.Kind != "org") || !validGHLogin(o.Path) ||
		!validGHRepoName(req.Name) || !registrymsg.ValidDescription(req.Description) {
		return RepoInfo{}, ErrBadInput
	}
	body := map[string]any{"name": req.Name, "private": req.Private, "auto_init": false}
	if req.Description != "" {
		body["description"] = req.Description
	}
	path := "/user/repos"
	if o.Kind == "org" {
		path = "/orgs/" + o.Path + "/repos"
	}
	raw, err := a.do(ctx, http.MethodPost, path, nil, postJSON(body), http.StatusCreated)
	if err != nil {
		return RepoInfo{}, err
	}
	var j ghRepoJSON
	if !decodeFields(raw, &j) {
		return RepoInfo{}, ghBadResponse()
	}
	info, ok := j.ok(o.Path + "/" + req.Name)
	if !ok || j.Private == nil || *j.Private != req.Private {
		return RepoInfo{}, ghBadResponse()
	}
	info.justCreated = true
	return info, nil
}

// CommitFiles writes the files to branch main of a repository created in this
// flow, one contents-API request per file in the given order (the first creates
// the branch on an empty repository: UNVERIFIED against the docs, see the E2
// report). No `sha` is ever sent, so an existing file can never be replaced.
// Not atomic: an error means some files may already be committed.
func (a *GitHubAPI) CommitFiles(ctx context.Context, repo RepoInfo, files []CommitFile) error {
	owner, name, ok := validGHRepoInfo(repo)
	if !ok || !repo.justCreated || !validCommitFiles(files) {
		return ErrBadInput
	}
	for _, f := range files {
		body := map[string]any{"message": "Add " + f.Path, "content": base64.StdEncoding.EncodeToString([]byte(f.Content)), "branch": apiBranch}
		raw, err := a.do(ctx, http.MethodPut, "/repos/"+owner+"/"+name+"/contents/"+f.Path, nil, postJSON(body), http.StatusCreated)
		if err != nil {
			return err
		}
		var r struct {
			Content *struct {
				Path *string `json:"path"`
			} `json:"content"`
			Commit *struct {
				SHA *string `json:"sha"`
			} `json:"commit"`
		}
		if !decodeFields(raw, &r) || r.Content == nil || r.Content.Path == nil || *r.Content.Path != f.Path || r.Commit == nil || r.Commit.SHA == nil {
			return ghBadResponse()
		}
	}
	return nil
}

type ghFlag struct {
	Enabled *bool `json:"enabled"`
}

type ghProtectionJSON struct {
	AllowForce    *ghFlag `json:"allow_force_pushes"`
	AllowDelete   *ghFlag `json:"allow_deletions"`
	EnforceAdmins *ghFlag `json:"enforce_admins"`
}

// strong: force pushes and deletion are off and the rules apply to admins too
// (the owner is an admin; without enforce_admins the other two would not bind
// the one account that can push).
func (p ghProtectionJSON) strong() (strong, ok bool) {
	if p.AllowForce == nil || p.AllowForce.Enabled == nil || p.EnforceAdmins == nil || p.EnforceAdmins.Enabled == nil {
		return false, false
	}
	if p.AllowDelete != nil && p.AllowDelete.Enabled == nil {
		return false, false
	}
	del := p.AllowDelete != nil && *p.AllowDelete.Enabled
	return !*p.AllowForce.Enabled && !del && *p.EnforceAdmins.Enabled, true
}

// EnsureProtectedMain reads the protection of main. A strong rule is fine; a
// weaker rule is ErrProtectionWeaker and is never edited. When there is no rule
// (404) the rule is written ONLY for a repository created in this flow
// (repo.justCreated, set by CreateRepo and nowhere else): no required checks,
// no required reviews, no restrictions, admins included, force pushes and
// deletion off. For any other repository "no rule" is ErrProtectionWeaker and
// nothing is written.
func (a *GitHubAPI) EnsureProtectedMain(ctx context.Context, repo RepoInfo) error {
	owner, name, ok := validGHRepoInfo(repo)
	if !ok {
		return ErrBadInput
	}
	path := "/repos/" + owner + "/" + name + "/branches/" + apiBranch + "/protection"
	raw, err := a.do(ctx, http.MethodGet, path, nil, nil, http.StatusOK)
	if err == nil {
		var j ghProtectionJSON
		if !decodeFields(raw, &j) {
			return ghBadResponse()
		}
		strong, ok := j.strong()
		if !ok {
			return ghBadResponse()
		}
		if strong {
			return nil
		}
		return ErrProtectionWeaker
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	if !repo.justCreated {
		return ErrProtectionWeaker
	}
	body := map[string]any{
		"required_status_checks":        nil,
		"enforce_admins":                true,
		"required_pull_request_reviews": nil,
		"restrictions":                  nil,
		"allow_force_pushes":            false,
		"allow_deletions":               false,
	}
	raw, err = a.do(ctx, http.MethodPut, path, nil, postJSON(body), http.StatusOK)
	if err != nil {
		return err
	}
	var j map[string]any
	if !decodeFields(raw, &j) {
		return ghBadResponse()
	}
	return nil
}

// AddDeployKey adds one public key line to the repository as a deploy key.
// GitHub's read_only is the inverse of GitLab's can_push.
func (a *GitHubAPI) AddDeployKey(ctx context.Context, repo RepoInfo, title, key string, canPush bool) error {
	owner, name, ok := validGHRepoInfo(repo)
	if !ok || !printable(title) || len(title) > 100 || !deployKeyRE.MatchString(key) {
		return ErrBadInput
	}
	body := map[string]any{"title": title, "key": key, "read_only": !canPush}
	raw, err := a.do(ctx, http.MethodPost, "/repos/"+owner+"/"+name+"/keys", nil, postJSON(body), http.StatusCreated)
	if err != nil {
		return err
	}
	var r struct {
		ID       *int64 `json:"id"`
		ReadOnly *bool  `json:"read_only"`
	}
	if !decodeFields(raw, &r) || r.ID == nil || *r.ID <= 0 || (r.ReadOnly != nil && *r.ReadOnly == canPush) {
		return ghBadResponse()
	}
	return nil
}
