package broker

import (
	"context"
	"strings"
)

// W3e (E2): the provider interface. A broker flow talks to RepoProvider; the
// GitLab client of W3c (gitlabapi.go, unchanged) and the GitHub client
// (githubapi.go) are its two implementations. Each implementation keeps its own
// (method, path shape) allowlist and its own single `do`.
//
// Handles that prove provenance. Two values may only come from the provider
// itself: Owner (ListOwners / GetOwner: the target was looked up, never taken
// from a caller) and RepoInfo.justCreated (only CreateRepo of THIS flow sets it).
// CreateRepo refuses an Owner that did not come from the provider; the GitHub
// implementation writes protection or files only for a handle with justCreated.

// Owner is where a repository is created: a GitLab group or a GitHub user or
// organization. Only the login/path and a kind are kept.
type Owner struct {
	ID   int64  // GitLab namespace id; 0 for GitHub
	Name string // display name (GitLab); the login for GitHub
	Path string // GitLab full group path, or GitHub login
	Kind string // "group" (GitLab), "user" or "org" (GitHub)

	fromProvider bool // set by ListOwners / GetOwner only
}

// RepoInfo is a repository the provider confirmed. Path is the provider's own
// spelling (GitHub: as GitHub writes it, which may differ in case from what was
// asked).
type RepoInfo struct {
	ID            int64
	Path          string
	DefaultBranch string // "" when the provider names none that fits the branch rule

	// justCreated is true only for the value CreateRepo returns of THIS flow.
	justCreated bool
}

// CreateRepoReq asks for a new, empty repository. Private must be true for
// GitLab (it only creates private projects); for GitHub it is the signed
// visibility choice.
type CreateRepoReq struct {
	Owner       Owner
	Name        string
	Description string
	Private     bool
}

// RepoProvider is what the create/link flows need from a provider. Every
// error is a *CodeError with a fixed code (see WireCode).
type RepoProvider interface {
	Kind() string
	// ListOwners lists the owners the token can create repositories in
	// (login/path only). truncated: the list is not complete (page cap or an
	// entry that does not fit our name rules was skipped).
	ListOwners(ctx context.Context) (owners []Owner, truncated bool, err error)
	// GetOwner looks one owner up (ErrNotFound when the provider has none).
	GetOwner(ctx context.Context, path string) (Owner, error)
	// GetRepo is the existence check (ErrNotFound when absent). On GitHub a
	// case variant of an existing repository IS that repository.
	GetRepo(ctx context.Context, fullPath string) (RepoInfo, error)
	CreateRepo(ctx context.Context, req CreateRepoReq) (RepoInfo, error)
	// CommitFiles writes the first files one by one (not atomic).
	CommitFiles(ctx context.Context, repo RepoInfo, files []CommitFile) error
	// DefaultBranch reads the repository's default branch ("" when none fits).
	DefaultBranch(ctx context.Context, repo RepoInfo) (string, error)
	// EnsureProtectedMain makes sure main cannot be force-pushed or deleted
	// (GitLab: Maintainer push/merge). It never edits or removes an existing rule;
	// a weaker or (GitHub, repo not created in this flow) absent rule is
	// ErrProtectionWeaker.
	EnsureProtectedMain(ctx context.Context, repo RepoInfo) error
	// AddDeployKey registers one public key. canPush false means read only
	// (GitHub read_only is the inverse of GitLab can_push).
	AddDeployKey(ctx context.Context, repo RepoInfo, title, key string, canPush bool) error

	// W3f: the delete side. Every call below works only on a handle this very
	// client instance produced (see delete_clients.go): LiveRepo first, then
	// DeleteRepo / ListDeployKeys, and RemoveDeployKey only for a key that
	// ListDeployKeys of this instance returned. A DELETE is sent at most once per
	// handle and is never retried.
	//
	// LiveRepo reads the repository (GET) and reports what the provider says,
	// whatever name it carries: the caller compares id and name.
	LiveRepo(ctx context.Context, fullPath string) (RepoInfo, error)
	// DeleteRepo deletes the repository LiveRepo just returned.
	DeleteRepo(ctx context.Context, repo RepoInfo) error
	// ListDeployKeys lists the repository's deploy keys (complete list or an error).
	ListDeployKeys(ctx context.Context, repo RepoInfo) ([]DeployKeyInfo, error)
	// RemoveDeployKey takes one listed key off THIS repository. GitLab: the key is
	// disabled for this project only; no call deletes a key everywhere.
	RemoveDeployKey(ctx context.Context, repo RepoInfo, key DeployKeyInfo) error
}

// DeployKeyInfo is one deploy key of a repository: the provider's id, its title
// and the public key body ("type base64", no comment).
type DeployKeyInfo struct {
	ID    int64
	Title string
	Body  string
}

// SameRepoPath reports whether two repository paths name the same repository
// for the provider kind: GitHub names are case-insensitive, GitLab's are
// compared as written (W3c).
func SameRepoPath(kind, a, b string) bool {
	if kind == "github" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// IsProviderRefused: the provider answered but refused (either implementation).
func IsProviderRefused(err error) bool {
	c := ErrCode(err)
	return c == CodeGitLabRefused || c == CodeGitHubRefused
}

// ---------- GitLab implementation (a thin adapter) ----------

type gitlabRepoProvider struct{ api *GitLabAPI }

func newGitLabRepoProvider(a *GitLabAPI) RepoProvider { return gitlabRepoProvider{a} }

func (g gitlabRepoProvider) Kind() string { return "gitlab" }

func groupOwner(gr Group) Owner {
	return Owner{ID: gr.ID, Name: gr.Name, Path: gr.FullPath, Kind: "group", fromProvider: true}
}

func (g gitlabRepoProvider) ListOwners(ctx context.Context) ([]Owner, bool, error) {
	gs, trunc, err := g.api.GroupsTruncated(ctx)
	if err != nil {
		return nil, false, err
	}
	out := make([]Owner, 0, len(gs))
	for _, gr := range gs {
		out = append(out, groupOwner(gr))
	}
	return out, trunc, nil
}

func (g gitlabRepoProvider) GetOwner(ctx context.Context, path string) (Owner, error) {
	gr, err := g.api.Group(ctx, path)
	if err != nil {
		return Owner{}, err
	}
	return groupOwner(gr), nil
}

func (g gitlabRepoProvider) GetRepo(ctx context.Context, fullPath string) (RepoInfo, error) {
	p, err := g.api.Project(ctx, fullPath)
	if err != nil {
		return RepoInfo{}, err
	}
	return RepoInfo{ID: p.ID, Path: p.PathWithNamespace, DefaultBranch: p.DefaultBranch}, nil
}

func (g gitlabRepoProvider) CreateRepo(ctx context.Context, req CreateRepoReq) (RepoInfo, error) {
	// GitLab projects are always private; the namespace id must come from a lookup.
	if !req.Private || !req.Owner.fromProvider || req.Owner.Kind != "group" {
		return RepoInfo{}, ErrBadInput
	}
	p, err := g.api.CreateProject(ctx, req.Owner.Path, req.Owner.ID, req.Name, req.Description)
	if err != nil {
		return RepoInfo{}, err
	}
	return RepoInfo{ID: p.ID, Path: p.PathWithNamespace, DefaultBranch: p.DefaultBranch}, nil
}

func (g gitlabRepoProvider) CommitFiles(ctx context.Context, repo RepoInfo, files []CommitFile) error {
	return g.api.CommitFiles(ctx, repo.ID, files)
}

func (g gitlabRepoProvider) DefaultBranch(ctx context.Context, repo RepoInfo) (string, error) {
	p, err := g.api.Project(ctx, repo.Path)
	if err != nil {
		return "", err
	}
	return p.DefaultBranch, nil
}

func (g gitlabRepoProvider) EnsureProtectedMain(ctx context.Context, repo RepoInfo) error {
	return g.api.EnsureProtectedMain(ctx, repo.ID)
}

func (g gitlabRepoProvider) AddDeployKey(ctx context.Context, repo RepoInfo, title, key string, canPush bool) error {
	return g.api.AddDeployKey(ctx, repo.ID, title, key, canPush)
}

func (g gitlabRepoProvider) LiveRepo(ctx context.Context, fullPath string) (RepoInfo, error) {
	return g.api.liveRepo(ctx, fullPath)
}

func (g gitlabRepoProvider) DeleteRepo(ctx context.Context, repo RepoInfo) error {
	return g.api.deleteRepo(ctx, repo)
}

func (g gitlabRepoProvider) ListDeployKeys(ctx context.Context, repo RepoInfo) ([]DeployKeyInfo, error) {
	return g.api.listDeployKeys(ctx, repo)
}

func (g gitlabRepoProvider) RemoveDeployKey(ctx context.Context, repo RepoInfo, key DeployKeyInfo) error {
	return g.api.removeDeployKey(ctx, repo, key)
}

// RepoProviderFor builds a NEW client of the configured provider id for its kind
// (the kind-aware apiFor). A flow must call it once and reuse that one instance
// for every step: the GitHub client remembers which repositories it created, and
// only for those honours the justCreated handle (protection PUT, first files).
// A handle used on another instance is refused. The base URL is re-validated for
// that kind. A provider whose kind has no client answers ErrBadInput.
func (s *Server) RepoProviderFor(providerID string) (RepoProvider, error) {
	p, ok := s.Registry.ProviderByID(providerID)
	if !ok {
		return nil, ErrProviderUnknown
	}
	return s.repoProviderForEntry(p)
}

// repoProviderForEntry builds the client of an entry the caller already read
// (a flow reads the provider ONCE and builds its client from that very value,
// so no registry change can slip in between).
func (s *Server) repoProviderForEntry(p Provider) (RepoProvider, error) {
	switch p.Kind {
	case "gitlab":
		a, err := newGitLabAPI(p, s.Tokens, s.apiSeams)
		if err != nil {
			return nil, err
		}
		return newGitLabRepoProvider(a), nil
	case "github":
		a, err := newGitHubAPI(p, s.Tokens, s.apiSeams)
		if err != nil {
			return nil, err
		}
		return a, nil
	}
	return nil, ErrBadInput
}
