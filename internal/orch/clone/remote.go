package clone

import (
	"context"
	"strings"
	"sync"
	"time"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/keys"
	"aigate/internal/orch/knownhosts"
	"aigate/internal/registrymsg"
)

// RemoteFor is the only way a clone or publish remote is built: the provider's
// ssh address (ssh://git@host[:port], nothing else) plus "/" plus the GitLab
// project path plus ".git". Both parts come from the signed registry; the
// result must have the exact remote shape knownhosts.SplitSSHRemote accepts.
func RemoteFor(sshURL, project string) (string, bool) {
	if !registrymsg.ValidSSHURL(sshURL) || !broker.ValidGitLabProjectPath(project) {
		return "", false
	}
	remote := sshURL + "/" + project + ".git"
	if _, _, ok := knownhosts.SplitSSHRemote(remote); !ok {
		return "", false
	}
	return remote, true
}

// RemoteForKind is RemoteFor for a provider kind. GitLab is RemoteFor exactly;
// GitHub takes owner/repo (registrymsg's shape rule, case kept as the registry
// holds it) and must produce the GitHub remote shape.
func RemoteForKind(kind, sshURL, path string) (string, bool) {
	switch kind {
	case "gitlab":
		return RemoteFor(sshURL, path)
	case "github":
		if !registrymsg.ValidSSHURL(sshURL) || !registrymsg.ValidRemotePathForKind("github", path) {
			return "", false
		}
		remote := sshURL + "/" + path + ".git"
		if _, _, ok := knownhosts.SplitSSHRemoteOwnerRepo(remote); !ok {
			return "", false
		}
		return remote, true
	}
	return "", false
}

// NameMatches reports whether the last segment of a remote path is the project
// id (what must hold for a project with origin "created"; linked repositories may be named anything): exactly for GitLab, ignoring case for GitHub (its names are
// case-insensitive; the registry keeps the provider's spelling).
func NameMatches(kind, remotePath, id string) bool {
	i := strings.LastIndexByte(remotePath, '/')
	if i < 0 {
		return false
	}
	if kind == "github" {
		return strings.EqualFold(remotePath[i+1:], id)
	}
	return remotePath[i+1:] == id
}

// DefaultResolverTTL is how long the registry read is reused (D6).
const DefaultResolverTTL = 30 * time.Second

// Resolver answers publish.Config.RemoteResolver for repositories that the
// static --git-remote-for list does not name. It trusts nothing but the
// registry (read through the broker, which only changes on a signed request)
// and the pinned known_hosts file: the remote of a project is
// provider.ssh_url + "/" + gitlab.project + ".git", handed out only when the
// host is pinned. It never sees request or task data other than the repository
// path it is asked about.
//
// Resolve returns:
//
//	(remote, key, true)  a project linked to a provider, resolved and pinned: push
//	                     there with this private key (GitLab: the shared deploy
//	                     key, SharedKey; GitHub: keys/<project id>/id_ed25519
//	                     under KeysDir, checked right now: missing, unreadable,
//	                     not a regular file or wrong mode/owner refuses);
//	("", "", true)       a project that is (or, while the registry cannot be read
//	                     and the cached table is stale, may be) linked but cannot
//	                     be resolved safely: REFUSE the push;
//	("", "", false)      not a provider-linked project: the caller's own default applies.
type Resolver struct {
	Client     *broker.Client
	KnownHosts string
	// KeysDir is the directory of the per-project GitHub keys; SharedKey the
	// shared GitLab deploy key (both absolute; "" = none: a GitHub project is
	// then refused, GitLab projects get no explicit key and the publisher's own
	// applies).
	KeysDir, SharedKey string
	// TTL is the cache lifetime (0 = DefaultResolverTTL); Now is the clock
	// (nil = time.Now).
	TTL time.Duration
	Now func() time.Time

	mu     sync.Mutex
	fresh  bool // the cache is current (not invalidated, within the TTL)
	at     time.Time
	byPath map[string]resolved // provider-linked project path -> answer (zero = refuse)
}

// resolved is a table row: the remote ("" = refuse) and, for GitHub, the project
// id whose key directory holds the private key.
type resolved struct {
	remote string
	github bool
	id     string
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Invalidate drops the cache so the next Resolve reads the registry again. The
// API calls it right after a registry change. The old table stays as a hint of
// which repositories were linked, should the broker be unreachable.
func (r *Resolver) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.fresh = false
	r.mu.Unlock()
}

// Resolve: see the type comment.
func (r *Resolver) Resolve(repo string) (remote, key string, ok bool) {
	if r == nil || r.Client == nil || r.Client.Socket == "" || repo == "" {
		return "", "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ttl := r.TTL
	if ttl <= 0 {
		ttl = DefaultResolverTTL
	}
	if !r.fresh || r.now().Sub(r.at) >= ttl {
		r.refresh()
	}
	if !r.fresh {
		// The table is stale (invalidated or past its TTL) and the registry could
		// not be read again: which repositories are provider-linked is UNKNOWN, so
		// nothing is pushed anywhere by guessing, a repository the old table did
		// not list included (it may be the one just created).
		return "", "", true
	}
	row, found := r.byPath[repo]
	if !found {
		return "", "", false
	}
	if row.remote == "" {
		return "", "", true
	}
	if !row.github {
		return row.remote, r.SharedKey, true
	}
	// The per-project key is looked at on EVERY call (the table may be 30 s old):
	// a key that went missing or lost its mode refuses the push.
	p, err := keys.PrivatePath(r.KeysDir, row.id)
	if err != nil {
		return "", "", true
	}
	return row.remote, p, true
}

func (r *Resolver) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	view, oerr := r.Client.Projects(ctx)
	if oerr != nil {
		r.fresh = false
		return
	}
	type prov struct{ ssh, kind string }
	providers := map[string]prov{}
	for _, p := range view.Providers {
		providers[p.ID] = prov{p.SSHURL, p.Kind}
	}
	table := map[string]resolved{}
	for _, p := range view.Projects {
		if p.Provider == nil {
			continue // not a provider-linked project
		}
		// Provider-linked but unresolved is refuse-class even without a remote
		// link (an old broker, a rollback): never the default --git-remote.
		table[p.Path] = resolved{}
		if p.Remote == nil {
			continue
		}
		pv, known := providers[*p.Provider]
		if !known {
			continue
		}
		// The project id (folder, key folder) is independent of the repository name for a linked
		// repository (W3f F1b). A repository this tool created is named by its id: a row that says
		// otherwise is a damaged registry, refused.
		if p.Origin == broker.OriginCreated && !NameMatches(pv.kind, p.Remote.Path, p.ID) {
			continue
		}
		remote, ok := RemoteForKind(pv.kind, pv.ssh, p.Remote.Path)
		if !ok || !knownhosts.RemotePinned(r.KnownHosts, remote) {
			continue
		}
		if pv.kind == "github" && !registrymsg.ValidSubject(p.ID) {
			continue
		}
		table[p.Path] = resolved{remote: remote, github: pv.kind == "github", id: p.ID}
	}
	r.byPath, r.fresh, r.at = table, true, r.now()
}
