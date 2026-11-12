package clone

import (
	"context"
	"sync"
	"time"

	"aigate/internal/orch/broker"
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
//	(remote, true)  a project linked to a provider, resolved and pinned: push there;
//	("",     true)  a project that is (or, while the registry cannot be read
//	                and the cached table is stale, may be) linked but cannot be
//	                resolved safely: REFUSE the push;
//	("",     false) not a provider-linked project: the caller's own default applies.
type Resolver struct {
	Client     *broker.Client
	KnownHosts string
	// TTL is the cache lifetime (0 = DefaultResolverTTL); Now is the clock
	// (nil = time.Now).
	TTL time.Duration
	Now func() time.Time

	mu     sync.Mutex
	fresh  bool // the cache is current (not invalidated, within the TTL)
	at     time.Time
	byPath map[string]string // provider-linked project path -> remote ("" = refuse)
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
func (r *Resolver) Resolve(repo string) (string, bool) {
	if r == nil || r.Client == nil || r.Client.Socket == "" || repo == "" {
		return "", false
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
		return "", true
	}
	if remote, ok := r.byPath[repo]; ok {
		return remote, true
	}
	return "", false
}

func (r *Resolver) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	view, oerr := r.Client.Projects(ctx)
	if oerr != nil {
		r.fresh = false
		return
	}
	providers := map[string]string{}
	for _, p := range view.Providers {
		providers[p.ID] = p.SSHURL
	}
	table := map[string]string{}
	for _, p := range view.Projects {
		if p.Provider == nil || p.Remote == nil {
			continue // not a provider-linked project
		}
		table[p.Path] = ""
		ssh, known := providers[*p.Provider]
		if !known || !lastSegmentIs(p.Remote.Path, p.ID) {
			continue
		}
		remote, ok := RemoteFor(ssh, p.Remote.Path)
		if !ok || !knownhosts.RemotePinned(r.KnownHosts, remote) {
			continue
		}
		table[p.Path] = remote
	}
	r.byPath, r.fresh, r.at = table, true, r.now()
}

func lastSegmentIs(project, id string) bool {
	for i := len(project) - 1; i >= 0; i-- {
		if project[i] == '/' {
			return project[i+1:] == id
		}
	}
	return false
}
