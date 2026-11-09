package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Outbound GitLab access. The broker talks to exactly one GitLab, the
// configured base URL, and can do exactly two things there: list the open
// merge requests of one project and create one. This client has no method
// that merges, approves, updates or deletes anything.
//
// API facts (official docs, see the task report): POST
// /api/v4/projects/:id/merge_requests with :id the URL-encoded project path;
// GET the same path with state=opened and source_branch=... lists. A draft is
// marked by a "Draft:" title prefix (the create endpoint documents no draft
// field; a "draft" field is sent as well and is ignored if unknown).

const (
	gitlabConnTimeout = 10 * time.Second
	gitlabTotalTime   = 15 * time.Second
	maxGitLabResponse = 1 << 20
)

var (
	// errBadURL: GitLab answered, but with a web_url outside the configured base.
	errBadURL = errors.New("bad web_url")
	// errConflict: create answered 409 (an open MR for the branch exists).
	errConflict = errors.New("conflict")
)

// GitLabClient talks to the configured GitLab. Token is sent only in the
// PRIVATE-TOKEN header. HTTP nil means the hardened default client. Every
// failure is errUpstream or errBadURL: no upstream text, no token.
type GitLabClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// newGitLabHTTP never follows redirects, ignores proxy environment variables
// and has fixed timeouts.
func newGitLabHTTP() *http.Client {
	return &http.Client{
		Timeout: gitlabTotalTime,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: gitlabConnTimeout}).DialContext,
			TLSHandshakeTimeout: gitlabConnTimeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// MR is the part of a GitLab merge request the broker reads.
type MR struct {
	IID    int64
	WebURL string
}

type mrJSON struct {
	IID          *int64  `json:"iid"`
	WebURL       *string `json:"web_url"`
	State        *string `json:"state"`
	SourceBranch *string `json:"source_branch"`
}

// mrCollectionURL builds <base>/api/v4/projects/<escaped project>/merge_requests.
// The project comes from the broker config, never from a request.
func (c *GitLabClient) mrCollectionURL(project string, q url.Values) (string, error) {
	b, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || b.Host == "" {
		return "", errUpstream
	}
	u := url.URL{
		Scheme:   b.Scheme,
		Host:     b.Host,
		Path:     "/api/v4/projects/" + project + "/merge_requests",
		RawPath:  "/api/v4/projects/" + url.PathEscape(project) + "/merge_requests",
		RawQuery: q.Encode(),
	}
	return u.String(), nil
}

func (c *GitLabClient) do(ctx context.Context, method, u string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, errUpstream
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", openAIUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = newGitLabHTTP()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, errUpstream // url.Error would quote the URL
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGitLabResponse+1))
	if err != nil || len(raw) > maxGitLabResponse {
		return 0, nil, errUpstream
	}
	return resp.StatusCode, raw, nil
}

// webURLOK reports whether u lies under the configured base URL: same
// scheme and host, prefix "<base>/", no userinfo.
func (c *GitLabClient) webURLOK(u string) bool {
	base := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasPrefix(u, base+"/") || badText(u, false) || strings.ContainsAny(u, " \\") {
		return false
	}
	pu, err := url.Parse(u)
	if err != nil || pu.User != nil {
		return false
	}
	bu, err := url.Parse(base)
	return err == nil && pu.Scheme == bu.Scheme && pu.Host == bu.Host
}

func decodeOne(raw []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(v) != nil {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

// FindOpenMR returns the open MR of project whose source branch is branch.
// ok is false when there is none.
func (c *GitLabClient) FindOpenMR(ctx context.Context, project, branch string) (mr MR, ok bool, err error) {
	u, err := c.mrCollectionURL(project, url.Values{"state": {"opened"}, "source_branch": {branch}})
	if err != nil {
		return MR{}, false, err
	}
	code, raw, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil || code != http.StatusOK {
		return MR{}, false, errUpstream
	}
	var list []mrJSON
	if !decodeOne(raw, &list) {
		return MR{}, false, errUpstream
	}
	for _, e := range list {
		// The filter is GitLab's; verify instead of trusting it.
		if e.State == nil || *e.State != "opened" || e.SourceBranch == nil || *e.SourceBranch != branch {
			continue
		}
		return c.checkMR(e)
	}
	return MR{}, false, nil
}

func (c *GitLabClient) checkMR(e mrJSON) (MR, bool, error) {
	if e.IID == nil || *e.IID <= 0 || e.WebURL == nil {
		return MR{}, false, errUpstream
	}
	if !c.webURLOK(*e.WebURL) {
		return MR{}, false, errBadURL
	}
	return MR{IID: *e.IID, WebURL: *e.WebURL}, true, nil
}

// MRCreate is what the broker sends: all fields are built by the broker.
type MRCreate struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Draft        bool   `json:"draft"`
	// Removing the source branch and squashing are explicit false, never on.
	RemoveSourceBranch bool `json:"remove_source_branch"`
	Squash             bool `json:"squash"`
}

// CreateMR creates one merge request. errConflict means GitLab said an open
// MR for the branch already exists.
func (c *GitLabClient) CreateMR(ctx context.Context, project string, in MRCreate) (MR, error) {
	u, err := c.mrCollectionURL(project, nil)
	if err != nil {
		return MR{}, err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return MR{}, errUpstream
	}
	code, raw, err := c.do(ctx, http.MethodPost, u, body)
	if err != nil {
		return MR{}, err
	}
	if code == http.StatusConflict {
		return MR{}, errConflict
	}
	if code != http.StatusCreated {
		return MR{}, errUpstream
	}
	var e mrJSON
	if !decodeOne(raw, &e) || e.State == nil || *e.State != "opened" {
		return MR{}, errUpstream
	}
	mr, _, err := c.checkMR(e)
	return mr, err
}
