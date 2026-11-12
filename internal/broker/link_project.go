package broker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// link_project and attach_remote (W3e, E-D5). Both bring a repository that
// ALREADY exists at the provider under the registry:
//
//   - link_project: a NEW local project (id, path repo_roots[0]/<id>, kind
//     registered) for the remote; orchd clones it afterwards.
//   - attach_remote: an ALREADY registered project WITHOUT a provider link gets
//     provider + remote. Its path, kind and ai_context are never touched.
//
// Order (verifySignedShape order first; everything the request and the registry
// alone decide is refused in the shape stage, before the request ID is burnt):
//
//  1. signature and shape (ids, remote path for the provider's kind, deploy key
//     line and its signed hash, registry rules: already_linked, project_exists,
//     project_unknown); the request ID is consumed
//  2. per-name lock, registry rules again
//  3. GET the remote repository: only a 404 means absent (repo_not_found); any
//     other failure is a provider error. Nothing is created, changed or deleted
//     at the provider by these ops except the one deploy key below.
//  4. quota (spent right before the first write), then the deploy key POST. The
//     provider must ACCEPT the key: a duplicate or "already in use" answer is a
//     refusal, never success (GitLab: no "enable key" call exists in the allowlist)
//  5. LAST: the registry write
//
// Failure semantics (same as create_project, the deploy key is the only write):
// the provider's own refusal of the key request (400/401/403/404/422/429) means
// nothing was attached: the plain provider code, no orphan. Any other error at
// the key step (5xx, 3xx, transport, a bad 2xx answer, a panic) is of UNKNOWN
// outcome: orphan record (origin "linked") with step deploy_key + link_partial. A registry
// failure after the key was accepted: orphan with step register + link_partial.
// Nothing is deleted or retried. The orphan's project is the remote path and its
// id the repository's; link_partial has its own sentence: the repository existed before, only the key may be on it.
//
// The static --git-remote-for entries of orchd are unknown to the broker:
// static_remote_wins is orchd's refusal, never raised here.

const maxLinkRemotePerHour = 10 // broker-wide for both ops (E-D6); per-actor limits are orchd's

var (
	linkProjectKeys  = []string{"id", "provider", "remote_path", "ai_context", "publish", "deploy_key_sha256", "deploy_key", "change", "request", "issued", "signature"}
	attachRemoteKeys = []string{"id", "provider", "remote_path", "publish", "deploy_key_sha256", "deploy_key", "change", "request", "issued", "signature"}
)

// remoteReq is what both ops carry. aiContext is set for link_project only:
// attach_remote has no such key, so a request cannot widen the AI context.
type remoteReq struct {
	attach     bool
	id         string
	provider   string
	remotePath string
	aiContext  *bool
	publish    string
	keySHA256  string
	key        string
	change     string
	request    string
	issued     string
	sig        *signing.Signature
}

type linkProjectArgs struct {
	ID              string             `json:"id"` // the NEW project's name (subject of the signed change)
	Provider        string             `json:"provider"`
	RemotePath      string             `json:"remote_path"`
	AIContext       *bool              `json:"ai_context"`
	Publish         string             `json:"publish"`
	DeployKeySHA256 string             `json:"deploy_key_sha256"`
	DeployKey       string             `json:"deploy_key"` // the public key LINE; only its hash is signed
	Change          string             `json:"change"`
	Request         string             `json:"request"`
	Issued          string             `json:"issued"`
	Signature       *signing.Signature `json:"signature"`
}

type attachRemoteArgs struct {
	ID              string             `json:"id"` // the EXISTING project's id
	Provider        string             `json:"provider"`
	RemotePath      string             `json:"remote_path"`
	Publish         string             `json:"publish"`
	DeployKeySHA256 string             `json:"deploy_key_sha256"`
	DeployKey       string             `json:"deploy_key"`
	Change          string             `json:"change"`
	Request         string             `json:"request"`
	Issued          string             `json:"issued"`
	Signature       *signing.Signature `json:"signature"`
}

func (s *Server) linkProjectHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a linkProjectArgs
	if !decodeStrict(raw, &a, linkProjectKeys) || a.AIContext == nil {
		return fail("bad_request")
	}
	return s.linkOrAttach(ctx, remoteReq{id: a.ID, provider: a.Provider, remotePath: a.RemotePath, aiContext: a.AIContext,
		publish: a.Publish, keySHA256: a.DeployKeySHA256, key: a.DeployKey, change: a.Change, request: a.Request, issued: a.Issued, sig: a.Signature})
}

func (s *Server) attachRemoteHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a attachRemoteArgs
	if !decodeStrict(raw, &a, attachRemoteKeys) {
		return fail("bad_request")
	}
	return s.linkOrAttach(ctx, remoteReq{attach: true, id: a.ID, provider: a.Provider, remotePath: a.RemotePath,
		publish: a.Publish, keySHA256: a.DeployKeySHA256, key: a.DeployKey, change: a.Change, request: a.Request, issued: a.Issued, sig: a.Signature})
}

func (r remoteReq) action() string {
	if r.attach {
		return registrymsg.ActionAttachRemote
	}
	return registrymsg.ActionLinkProject
}

// signedFields is the change the signature covers (attach has no ai_context).
func (r remoteReq) signedFields() map[string]any {
	f := map[string]any{"provider": r.provider, "remote_path": r.remotePath, "publish": r.publish, "deploy_key_sha256": r.keySHA256}
	if !r.attach {
		f["ai_context"] = *r.aiContext
	}
	return f
}

// remotePathShape: the path must be one the provider's client will accept, so
// that no later step can answer a bad_request after the ID was burnt. GitHub:
// owner/repo (exactly two segments). GitLab: group path (lowercase names, at most
// 4 segments) plus a lowercase project name, as GitLabAPI.Project requires.
func remotePathShape(kind, p string) bool {
	if !registrymsg.ValidRemotePath(p) {
		return false
	}
	switch kind {
	case "github":
		_, _, ok := splitGH(p)
		return ok && strings.Count(p, "/") == 1
	case "gitlab":
		i := strings.LastIndexByte(p, '/')
		return i > 0 && registrymsg.ValidGroupPath(p[:i]) && registrymsg.ValidName(p[i+1:])
	}
	return false
}

// remoteRegistryCode applies the registry rules of link/attach to r and returns
// "" or the fixed code to refuse with. Pure: no I/O.
//
//   - link: the id must be new; an id that is already provider-linked is
//     already_linked, any other id or path clash is project_exists (use
//     attach_remote for a registered project)
//   - attach: the project must exist (project_unknown) and have neither a
//     provider link nor any remote (already_linked; a legacy broker.json mapping
//     is not adopted)
//   - both: the same remote (same provider id; GitHub case-insensitively) must
//     not be linked by another project (already_linked)
func remoteRegistryCode(reg *Registry, r remoteReq, prov Provider, remote, projPath string) string {
	if r.attach {
		var found *Project
		for i := range reg.Projects {
			if reg.Projects[i].ID == r.id {
				found = &reg.Projects[i]
			}
		}
		switch {
		case found == nil:
			return "project_unknown"
		case found.Provider != nil:
			return "already_linked"
		case found.Remote != nil:
			// A project with a legacy remote mapping (broker.json: create_mr uses it)
			// is never adopted: attaching would redirect create_mr and could re-point
			// it at another kind or host.
			return "already_linked"
		}
	} else {
		for _, p := range reg.Projects {
			switch {
			case p.ID == r.id && p.Provider != nil:
				return "already_linked"
			case p.ID == r.id || p.Path == projPath:
				return "project_exists"
			}
		}
	}
	for _, p := range reg.Projects {
		if p.ID != r.id && p.Provider != nil && *p.Provider == prov.ID && p.Remote != nil && SameRepoPath(prov.Kind, p.Remote.Path, remote) {
			return "already_linked"
		}
	}
	return ""
}

func (s *Server) linkOrAttach(ctx context.Context, r remoteReq) (res any, oe *OpError) {
	// The store is checked BEFORE the signature: a refusal here burns no ID.
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	var projPath string
	shape := func() *OpError { // everything decidable from the request and the registry alone: no ID burnt
		if r.attach {
			if !registrymsg.ValidSubject(r.id) {
				return fail1("bad_request")
			}
		} else {
			if !registrymsg.ValidName(r.id) {
				return fail1("bad_request")
			}
			p, ok := s.projectPath(r.id)
			if !ok {
				return fail1("path_not_allowed")
			}
			projPath = p
		}
		prov, ok := s.Registry.ProviderByID(r.provider)
		if !ok {
			return fail1("provider_unknown")
		}
		if !remotePathShape(prov.Kind, r.remotePath) {
			return fail1("bad_request")
		}
		if !deployKeyRE.MatchString(r.key) {
			return fail1("bad_request")
		}
		sum := sha256.Sum256([]byte(r.key))
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(r.keySHA256)) != 1 {
			return fail1("deploy_key_mismatch")
		}
		reg := s.Registry.Snapshot()
		if code := remoteRegistryCode(&reg, r, prov, r.remotePath, projPath); code != "" {
			return fail1(code)
		}
		return nil
	}
	if oe := s.verifySignedShape(r.action(), r.id, r.signedFields(), r.change, r.request, r.issued, r.sig, shape); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed.
	if !s.lockName(r.id) {
		if r.attach {
			return fail("already_linked") // another link/attach/create of this project is running
		}
		return fail("project_exists")
	}
	defer s.unlockName(r.id)
	// the registry after the lock: an earlier request may just have finished
	prov, ok := s.Registry.ProviderByID(r.provider)
	if !ok {
		return fail("provider_unknown")
	}
	// One link/attach per remote at a time, whatever the project id: taken in
	// ADDITION to the id lock, always in that order (id, then remote), and both
	// are try-locks, so neither order can deadlock. Without it two ids could
	// each pass the registry rule and attach their own key to the same repository.
	rkey := "remote|" + prov.Kind + "|" + r.provider + "|" + strings.ToLower(r.remotePath)
	if !s.lockName(rkey) {
		return fail("already_linked")
	}
	defer s.unlockName(rkey)
	snap := s.Registry.Snapshot()
	if code := remoteRegistryCode(&snap, r, prov, r.remotePath, projPath); code != "" {
		return fail(code)
	}
	var before Project // attach: the project as it is now; the final write must still see it
	for _, p := range snap.Projects {
		if p.ID == r.id {
			before = p
		}
	}
	rp, oe := s.providerForEntry(prov) // ONE instance, built from the entry read above
	if oe != nil {
		return nil, oe
	}
	// (3) the remote repository must exist: only a 404 means absent
	info, err := rp.GetRepo(ctx, r.remotePath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fail("repo_not_found")
		}
		return nil, apiFailKind(prov.Kind, err)
	}
	// the provider's answer must name the repository we asked for (GitHub: same
	// spelling up to case); the provider's own spelling is what is stored
	if !SameRepoPath(prov.Kind, info.Path, r.remotePath) || info.ID <= 0 {
		return nil, fail1(refusedCode(prov.Kind))
	}
	remote := info.Path
	if code := remoteRegistryCode(&snap, r, prov, remote, projPath); code != "" {
		return fail(code)
	}
	// The quota is spent only now, right before the first provider write.
	if s.State.LinkRemote != nil && !s.State.LinkRemote.Allow() {
		return fail("rate_limited")
	}

	step := stepDeployKey
	id := info.ID
	partial := func() (any, *OpError) {
		d := PartialDetail{Provider: r.provider, Project: remote, ProjectID: &id, Step: step, Origin: OriginLinked}
		dropped, oerr := s.Registry.AddOrphan(Orphan{Provider: r.provider, Project: remote, ProjectID: &id, Step: step,
			At: s.now().UTC().Format(orphanTimeLayout), Origin: OriginLinked})
		d.OrphanRecorded = oerr == nil
		d.OrphanDropped = dropped
		if oerr != nil {
			s.logf("%s: the orphan record could not be saved (step=%s)", r.action(), step)
		}
		s.logf("%s: partial result at step=%s", r.action(), step)
		return nil, &OpError{Code: "link_partial", Data: d}
	}
	defer func() {
		if rec := recover(); rec != nil {
			s.logf("%s: panic at step=%s type=%T", r.action(), step, rec) // never the value
			res, oe = partial()
		}
	}()
	// (4) the key must be ACCEPTED (201 with the permission we asked for)
	if err := rp.AddDeployKey(ctx, info, "orchd "+r.id, r.key, r.publish == "write"); err != nil {
		if refusedBeforeWrite(err) {
			s.logf("%s: the provider refused the deploy key", r.action())
			return nil, apiFailKind(prov.Kind, err)
		}
		return partial()
	}
	// (5) the registry entry comes LAST; a failure here leaves an orphan record
	step = stepRegister
	provID := r.provider
	var entry Project
	err = s.Registry.Update(func(reg *Registry) error {
		// the provider the calls used must still be the stored one
		if i := findProvider(reg, r.provider); i < 0 || reg.Providers[i] != prov {
			return opCodeErr("provider_unknown")
		}
		if code := remoteRegistryCode(reg, r, prov, remote, projPath); code != "" {
			return opCodeErr(code)
		}
		if !r.attach {
			entry = Project{ID: r.id, Path: projPath, Kind: "registered", AIContext: *r.aiContext, Provider: &provID,
				Remote: &RemoteLink{Path: remote, ID: &id}}
			reg.Projects = append(reg.Projects, entry)
			return nil
		}
		for i := range reg.Projects {
			p := &reg.Projects[i]
			if p.ID != r.id {
				continue
			}
			// only provider and remote are set; the project must still be the one we saw
			if p.Path != before.Path || p.Kind != before.Kind || p.AIContext != before.AIContext {
				return opCodeErr("project_unknown")
			}
			p.Provider = &provID
			p.Remote = &RemoteLink{Path: remote, ID: &id}
			entry = *p
			return nil
		}
		return opCodeErr("project_unknown")
	})
	if err != nil {
		// A write that failed only on the directory fsync keeps the state: if the
		// entry is there, the project IS linked.
		now := s.Registry.Snapshot()
		for _, p := range now.Projects {
			if p.ID == r.id && p.Provider != nil && *p.Provider == provID && p.Remote != nil && p.Remote.ID != nil && *p.Remote.ID == id &&
				SameRepoPath(prov.Kind, p.Remote.Path, remote) && (r.attach || p.Path == projPath) {
				s.logf("%s: the registry file was written but its directory fsync failed", r.action())
				entry, err = p, nil
			}
		}
		if err != nil {
			return partial()
		}
	}
	return map[string]any{
		"project":     entry,
		"ssh_url":     prov.SSHURL,
		"clone_url":   prov.SSHURL + "/" + remote + ".git",
		"cloned_hint": false, // the broker never clones; orchd does, from the registry entry
	}, nil
}
