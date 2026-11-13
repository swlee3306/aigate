package broker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// W3f (F2): signed deletion ops. This is the first code of the broker that can
// delete anything at a provider, so every rule below is deliberately narrow.
//
//	remove_project      the registry row goes; with the signed detach_key=true the
//	                    project's own deploy key is taken off the remote FIRST (W3g)
//	delete_remote_repo  the provider repository of a project THIS TOOL created
//	                    (W3g: with also_local=true the registry row goes as well)
//	remove_deploy_key   our deploy key (title "orchd <id>") off a project's remote
//	clear_orphan        one orphan record (no provider call)
//
// Order of every op (the same as create/link): strict args -> registry usable ->
// the shape stage (everything the request and the registry alone decide; a
// refusal here burns NO request ID) -> signature, signed change, request ID
// consumed -> locks (project id, then remote; try-locks, released by defer) ->
// the registry again -> live checks -> rate limit spent right before the first
// external write -> the write -> the registry write.
//
// delete_remote_repo and remove_deploy_key read the repository live first and
// delete only when BOTH the provider's numeric id equals the registry's (and the
// signed) one AND its full name equals the registry path (GitHub: case
// insensitive). A DELETE is sent at most once and NEVER retried: an error after
// the request may have left (timeout, 5xx, a status that is not the documented
// success, transport, a panic) is an UNCERTAIN outcome (delete_partial): the
// registry is not touched, nothing is cleaned up locally, nothing is retried.
// A provider refusal (400/401/403/404/409/422, a rate limit) means nothing was
// deleted (remove_refused / provider_rate_limited). No orphan records are made
// for deletes.
//
// The token is the provider's own token_ref token (the client reads it from the
// token store with ValueOfKind); the ops have no token argument and no other
// credential exists on this path.
//
// Key identity (remove_deploy_key and remove_project with detach_key). First
// (W3h, keyLineCode, in the shape stage: no ID burnt, no provider contact) the
// unsigned `deploy_key` line orchd sends is bound to the registration: a row
// that carries remote.key_sha256 (the signed deploy_key_sha256 of its
// create/link/attach) needs sha256(line) to equal it; a row without one is
// refused on GitLab (the body alone would let the unsigned line pick any key of
// the project) and keeps the W3g rule on GitHub (the title is part of it). All
// refusals are deploy_key_mismatch. Then classifyKeys, unchanged: GitHub: title
// "orchd <project id>" AND the key body of the line; GitLab: the key body (the
// title is ignored, a shared key keeps the first project's title). One match
// is deleted, two are refused, and when none matches but any key has our title
// or body the answer is a refusal, never "already gone". GitLab: the key is
// taken off this project only.

const (
	maxDeleteRepoPerDay   = 3 // broker-wide, rolling 24 hours (F-D5)
	maxDeleteRepoPerHour  = 2 // broker-wide burst cap below the daily one; per-actor limits are orchd's (the broker does not know the actor)
	maxLocalDeletePerHour = 10
)

var (
	// deploy_key is the unsigned public key line orchd reads from its own key
	// folder. It is accepted only with detach_key=true (W3g) and, like in
	// remove_deploy_key, must hash to the row's signed key_sha256 (W3h; a
	// GitLab row without one is refused, a GitHub row keeps the W3g rule).
	removeProjectKeys    = []string{"id", "scope", "legacy_ack", "detach_key", "deploy_key", "change", "request", "issued", "signature"}
	deleteRemoteRepoKeys = []string{"id", "remote_path", "remote_id", "confirm_name", "also_local", "change", "request", "issued", "signature"}
	removeDeployKeyKeys  = []string{"id", "remote_path", "deploy_key", "change", "request", "issued", "signature"}
	clearOrphanKeys      = []string{"id", "project", "change", "request", "issued", "signature"}
)

func findProject(reg *Registry, id string) (Project, bool) {
	for _, p := range reg.Projects {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

func remoteLockKey(kind, provider, path string) string {
	return "remote|" + kind + "|" + provider + "|" + strings.ToLower(path)
}

// sameRowCore compares the fields a deletion depends on. Other fields (such as
// ai_context, flipped by set_flag at any time) must not turn a completed delete
// into a failure.
func sameRowCore(a, b Project) bool {
	eqStr := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqRemote := func(x, y *RemoteLink) bool {
		if (x == nil) != (y == nil) {
			return false
		}
		if x == nil {
			return true
		}
		return x.Path == y.Path && (x.ID == nil) == (y.ID == nil) && (x.ID == nil || *x.ID == *y.ID) && eqStr(x.KeySHA256, y.KeySHA256)
	}
	return a.ID == b.ID && a.Path == b.Path && a.Kind == b.Kind && a.Origin == b.Origin &&
		eqStr(a.Provider, b.Provider) && eqRemote(a.Remote, b.Remote) && eqStr(a.RemoteDeletedAt, b.RemoteDeletedAt)
}

// keyLineCode binds orchd's unsigned key line to the row (W3h, H-D3): "" or
// deploy_key_mismatch. With remote.key_sha256 the line must hash to it
// (constant-time compare of the lowercase hex). Without it only a GitHub row
// passes (its W3g rule needs our title as well); every other kind is refused.
// Pure: the caller has already checked that p has a remote.
func keyLineCode(prov Provider, p Project, line string) string {
	if p.Remote == nil {
		return "deploy_key_mismatch"
	}
	if p.Remote.KeySHA256 != nil {
		sum := sha256.Sum256([]byte(line))
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(*p.Remote.KeySHA256)) != 1 {
			return "deploy_key_mismatch"
		}
		return ""
	}
	if prov.Kind != "github" {
		return "deploy_key_mismatch"
	}
	return ""
}

// keyVerdict classifies a provider's key list against OUR key (project id +
// the public key body orchd supplied). Identity rule (W3g, G-D2):
//
//	GitHub  a key is ours iff its title is "orchd <id>" AND its body matches.
//	GitLab  a key is ours iff its body matches; the title is ignored, because a
//	        shared deploy key keeps the title of the FIRST project it was made for.
//
// Exactly one such key: keyOne. Two or more: keyMany (never guess). None: if ANY
// listed key has our title OR our body the situation cannot be told from
// "already gone" and keyLookalike is returned (the caller must refuse and keep
// everything); only when neither appears is it keyNone ("already gone").
type keyVerdict int

const (
	keyNone keyVerdict = iota
	keyOne
	keyMany
	keyLookalike
)

func classifyKeys(kind, id, wantBody string, keys []DeployKeyInfo) (DeployKeyInfo, keyVerdict) {
	title := "orchd " + id
	var match []DeployKeyInfo
	look := false
	for _, k := range keys {
		bodyOK, titleOK := k.Body == wantBody, k.Title == title
		ours := bodyOK && titleOK
		if kind == "gitlab" {
			ours = bodyOK
		}
		switch {
		case ours:
			match = append(match, k)
		case bodyOK || titleOK:
			look = true
		}
	}
	switch {
	case len(match) == 1:
		return match[0], keyOne
	case len(match) > 1:
		return DeployKeyInfo{}, keyMany
	case look:
		return DeployKeyInfo{}, keyLookalike
	}
	return DeployKeyInfo{}, keyNone
}

// ---------- remove_project ----------

type removeProjectArgs struct {
	ID        string             `json:"id"`
	Scope     string             `json:"scope"`
	LegacyAck *bool              `json:"legacy_ack"`
	DetachKey *bool              `json:"detach_key"` // signed (W3g)
	DeployKey *string            `json:"deploy_key"` // NOT signed: orchd's key line, only with detach_key=true
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

// removeProjectCode: "" or the refusal code. legacy_ack must equal
// (origin == legacy) in BOTH directions. detach_key=true needs a row that has a
// provider and a live remote (provider entry present, remote set with a number,
// not deleted): anything else is refused (W3g, G-D3), and orchd's key line
// must be bound to the row (keyLineCode, W3h). It returns the project and, for
// detach_key=true, its provider entry.
func removeProjectCode(reg *Registry, id string, ack, detach bool, keyLine string) (Project, Provider, string) {
	p, ok := findProject(reg, id)
	if !ok {
		return Project{}, Provider{}, "project_unknown"
	}
	legacy := p.Origin == OriginLegacy
	switch {
	case legacy && !ack:
		return p, Provider{}, "legacy_ack_required"
	case !legacy && ack:
		return p, Provider{}, "bad_request" // a second confirmation only exists for legacy projects
	}
	if !detach {
		return p, Provider{}, ""
	}
	if p.Provider == nil || p.Remote == nil || p.RemoteDeletedAt != nil {
		return p, Provider{}, "bad_request" // no remote to take a key from (never linked, legacy, or deleted already)
	}
	i := findProvider(reg, *p.Provider)
	if i < 0 {
		return p, Provider{}, "provider_unknown"
	}
	prov := reg.Providers[i]
	if !registrymsg.ValidRemotePathForKind(prov.Kind, p.Remote.Path) {
		return p, prov, "bad_request"
	}
	if p.Remote.ID == nil {
		return p, prov, "remote_changed"
	}
	if code := keyLineCode(prov, p, keyLine); code != "" {
		return p, prov, code
	}
	return p, prov, ""
}

func (s *Server) removeProjectHandler(ctx context.Context, raw json.RawMessage) (res any, oe *OpError) {
	var a removeProjectArgs
	if !decodeStrict(raw, &a, removeProjectKeys) || a.LegacyAck == nil || a.DetachKey == nil {
		return fail("bad_request")
	}
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	detach := *a.DetachKey
	fields := map[string]any{"scope": a.Scope, "legacy_ack": *a.LegacyAck, "detach_key": detach}
	wantBody, keyLine := "", ""
	shape := func() *OpError {
		if !registrymsg.ValidSubject(a.ID) || a.Scope != "local" {
			return fail1("bad_request")
		}
		if detach {
			// orchd's key line is required and must be a key; without detach it
			// must be absent (nothing is ever sent that no signature covers)
			if a.DeployKey == nil || !deployKeyRE.MatchString(*a.DeployKey) {
				return fail1("bad_request")
			}
			b, ok := keyBody(*a.DeployKey)
			if !ok {
				return fail1("bad_request")
			}
			wantBody, keyLine = b, *a.DeployKey
		} else if a.DeployKey != nil {
			return fail1("bad_request")
		}
		reg := s.Registry.Snapshot()
		if _, _, code := removeProjectCode(&reg, a.ID, *a.LegacyAck, detach, keyLine); code != "" {
			return fail1(code)
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionRemoveProject, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed.
	if !s.lockName(a.ID) {
		return fail("busy")
	}
	defer s.unlockName(a.ID)
	snap := s.Registry.Snapshot()
	before, prov, code := removeProjectCode(&snap, a.ID, *a.LegacyAck, detach, keyLine)
	if code != "" {
		return fail(code)
	}
	keyState := ""
	keySent := false
	if detach {
		// W3g: the deploy key goes off the remote BEFORE the row is unregistered.
		// Nothing is unregistered unless the key is definitely gone (deleted, or
		// not there). One DELETE at most, never retried.
		rkey := remoteLockKey(prov.Kind, prov.ID, before.Remote.Path)
		if !s.lockName(rkey) {
			return fail("busy")
		}
		defer s.unlockName(rkey)
		// (distinct names: the recover below must assign the NAMED result oe)
		rp, perr := s.providerForEntry(prov) // ONE instance: its live handles belong to it
		if perr != nil {
			return nil, perr
		}
		info, lerr := s.liveMatches(ctx, rp, prov.Kind, before)
		if lerr != nil {
			return nil, lerr
		}
		keys, err := rp.ListDeployKeys(ctx, info)
		if err != nil {
			return nil, apiFailKind(prov.Kind, err)
		}
		found, verdict := classifyKeys(prov.Kind, a.ID, wantBody, keys)
		switch verdict {
		case keyMany: // not unique: never guess which one is ours; the row stays
			s.logf("remove_project: more than one key matches, nothing removed")
			return fail("remove_refused")
		case keyLookalike: // a key with our title or body that is not clearly ours: cannot tell from "already gone"
			s.logf("remove_project: a similar key exists but none is clearly ours, nothing removed")
			return fail("remove_refused")
		}
		// One slot of the shared local-delete budget per op, spent right before
		// the first external write (the key DELETE) or, when there is no key to
		// delete, right before the registry write.
		if !s.allowLocalDelete() {
			return fail("rate_limited")
		}
		if verdict == keyNone {
			keyState = "already_gone"
		} else {
			defer func() {
				if rec := recover(); rec != nil {
					s.logf("remove_project: panic type=%T", rec) // never the value
					if keySent {
						res, oe = nil, fail1("delete_partial")
					} else {
						res, oe = nil, fail1("internal_error")
					}
				}
			}()
			keySent = true
			if err := rp.RemoveDeployKey(ctx, info, found); err != nil {
				return nil, s.deleteFailure("remove_project", prov.Kind, err)
			}
			keyState = "removed"
		}
	} else if !s.allowLocalDelete() {
		return fail("rate_limited")
	}
	err := s.Registry.Update(func(reg *Registry) error {
		got, ok := findProject(reg, a.ID)
		if !ok || !sameRowCore(got, before) { // the row that was checked (flags may change meanwhile)
			return opCodeErr("project_unknown")
		}
		kept := make([]Project, 0, len(reg.Projects))
		for _, p := range reg.Projects {
			if p.ID != a.ID {
				kept = append(kept, p)
			}
		}
		reg.Projects = kept
		return nil
	})
	if err != nil {
		now := s.Registry.Snapshot()
		if _, still := findProject(&now, a.ID); !still && s.Registry.Err() == nil {
			s.logf("remove_project: the registry file was written but its directory fsync failed")
		} else if keySent {
			// the key was taken off the remote but the row could not be removed
			s.logf("remove_project: the key was removed at the provider, the registry could not be updated")
			return nil, fail1("delete_partial")
		} else {
			return nil, updateFailure(err)
		}
	}
	s.logf("remove_project: removed project %s from the registry", a.ID)
	out := map[string]any{"id": a.ID, "removed": true}
	if detach {
		out["key_state"] = keyState
	}
	return out, nil
}

// ---------- delete_remote_repo ----------

type deleteRemoteRepoArgs struct {
	ID          string             `json:"id"` // the project id (subject of the signed change)
	RemotePath  string             `json:"remote_path"`
	RemoteID    string             `json:"remote_id"`
	ConfirmName string             `json:"confirm_name"`
	AlsoLocal   *bool              `json:"also_local"` // signed; orchd acts on it, the broker only carries it
	Change      string             `json:"change"`
	Request     string             `json:"request"`
	Issued      string             `json:"issued"`
	Signature   *signing.Signature `json:"signature"`
}

// deleteRepoCode applies every registry rule of delete_remote_repo. Pure: no I/O.
// It returns the project row, its provider entry and "" or the refusal code.
func deleteRepoCode(reg *Registry, a deleteRemoteRepoArgs) (Project, Provider, string) {
	p, ok := findProject(reg, a.ID)
	if !ok {
		return Project{}, Provider{}, "project_unknown"
	}
	switch p.Origin {
	case OriginCreated:
	case OriginLinked:
		return p, Provider{}, "not_created_by_tool"
	default: // unknown, legacy (and anything unforeseen)
		return p, Provider{}, "origin_unknown"
	}
	if p.Provider == nil || p.Remote == nil || p.RemoteDeletedAt != nil {
		return p, Provider{}, "remote_changed" // no remote any more (deleted already or never linked)
	}
	i := findProvider(reg, *p.Provider)
	if i < 0 {
		return p, Provider{}, "provider_unknown"
	}
	prov := reg.Providers[i]
	if !registrymsg.ValidRemotePathForKind(prov.Kind, a.RemotePath) || !registrymsg.ValidRemotePathForKind(prov.Kind, p.Remote.Path) {
		return p, prov, "bad_request"
	}
	if !SameRepoPath(prov.Kind, a.RemotePath, p.Remote.Path) {
		return p, prov, "remote_changed"
	}
	if p.Remote.ID == nil || strconv.FormatInt(*p.Remote.ID, 10) != a.RemoteID {
		return p, prov, "remote_changed"
	}
	// the typed name is compared with the REGISTRY's repository name (what the
	// user was shown), exact case
	if name, ok := registrymsg.RemoteLastSegment(p.Remote.Path); !ok || name != a.ConfirmName {
		return p, prov, "confirm_mismatch"
	}
	return p, prov, ""
}

// uncertainDelete / refusedDelete classify the error of a DELETE call.
func refusedDelete(err error) bool {
	switch ErrCode(err) {
	case CodeGitHubRefused, CodeGitLabRefused, CodeProviderRateLimited, CodeNotFound:
	default:
		return false
	}
	switch ErrStatus(err) {
	case 400, 401, 403, 404, 409, 422, 429:
		return true
	}
	return false
}

// deleteFailure maps the error of a DELETE (or key DELETE) request that was
// attempted. Not sent / refused: nothing changed. Anything else: UNCERTAIN.
func (s *Server) deleteFailure(op, kind string, err error) *OpError {
	switch {
	case deleteNotSent(err):
		return apiFailKind(kind, err)
	case refusedDelete(err):
		if ErrCode(err) == CodeProviderRateLimited {
			return fail1("provider_rate_limited")
		}
		s.logf("%s: the provider refused the request", op)
		return fail1("remove_refused")
	}
	s.logf("%s: outcome unknown (nothing is retried)", op)
	return fail1("delete_partial")
}

// allowDeleteRepo spends one slot of the daily and the hourly cap, or none.
func (s *Server) allowDeleteRepo() bool {
	d, h := s.State.DeleteRepoDay, s.State.DeleteRepoHour
	s.delLimitMu.Lock() // check both, then consume both: never one without the other
	defer s.delLimitMu.Unlock()
	if d == nil || h == nil || d.Remaining() < 1 || h.Remaining() < 1 { // fail closed
		return false
	}
	return d.Allow() && h.Allow()
}

func (s *Server) allowLocalDelete() bool {
	return s.State.DeleteLocal != nil && s.State.DeleteLocal.Allow()
}

// liveMatches reads the repository and compares it with the registry row: id
// and full name must both match. A 404 or a redirect means the repository is
// not (any more) where the registry says: remote_changed.
func (s *Server) liveMatches(ctx context.Context, rp RepoProvider, kind string, p Project) (RepoInfo, *OpError) {
	info, err := rp.LiveRepo(ctx, p.Remote.Path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RepoInfo{}, fail1("remote_changed")
		}
		switch ErrStatus(err) {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			return RepoInfo{}, fail1("remote_changed")
		}
		return RepoInfo{}, apiFailKind(kind, err)
	}
	if p.Remote.ID == nil || info.ID != *p.Remote.ID || !SameRepoPath(kind, info.Path, p.Remote.Path) {
		return RepoInfo{}, fail1("remote_changed")
	}
	return info, nil
}

func (s *Server) deleteRemoteRepoHandler(ctx context.Context, raw json.RawMessage) (res any, oe *OpError) {
	var a deleteRemoteRepoArgs
	if !decodeStrict(raw, &a, deleteRemoteRepoKeys) || a.AlsoLocal == nil {
		return fail("bad_request")
	}
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	fields := map[string]any{"remote_path": a.RemotePath, "remote_id": a.RemoteID, "confirm_name": a.ConfirmName, "also_local": *a.AlsoLocal}
	shape := func() *OpError {
		if !registrymsg.ValidSubject(a.ID) || !registrymsg.ValidRemotePath(a.RemotePath) || !registrymsg.ValidRemoteID(a.RemoteID) ||
			!registrymsg.ValidConfirmName(a.ConfirmName) {
			return fail1("bad_request")
		}
		reg := s.Registry.Snapshot()
		if _, _, code := deleteRepoCode(&reg, a); code != "" {
			return fail1(code)
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionDeleteRemoteRepo, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed.
	if !s.lockName(a.ID) {
		return fail("busy")
	}
	defer s.unlockName(a.ID)
	snap := s.Registry.Snapshot()
	before, prov, code := deleteRepoCode(&snap, a)
	if code != "" {
		return fail(code)
	}
	rkey := remoteLockKey(prov.Kind, prov.ID, before.Remote.Path)
	if !s.lockName(rkey) {
		return fail("busy")
	}
	defer s.unlockName(rkey)
	rp, oe := s.providerForEntry(prov) // ONE instance: the live handle belongs to it
	if oe != nil {
		return nil, oe
	}
	info, oe := s.liveMatches(ctx, rp, prov.Kind, before)
	if oe != nil {
		return nil, oe
	}
	// The quota is spent only now, right before the first (and only) write.
	if !s.allowDeleteRepo() {
		return fail("rate_limited")
	}
	sent := false
	defer func() {
		if rec := recover(); rec != nil {
			s.logf("delete_remote_repo: panic type=%T", rec) // never the value
			if sent {
				res, oe = nil, fail1("delete_partial")
			} else {
				res, oe = nil, fail1("internal_error")
			}
		}
	}()
	sent = true
	if err := rp.DeleteRepo(ctx, info); err != nil {
		return nil, s.deleteFailure("delete_remote_repo", prov.Kind, err)
	}
	state := "deleted"
	if prov.Kind == "gitlab" {
		state = "requested" // GitLab may delay the deletion
	}
	stamp := s.now().UTC().Format(orphanTimeLayout)
	if *a.AlsoLocal {
		// W3g (G-D4): the repository is gone and the signature also covered the
		// registration: the row is REMOVED (no "remote null" leftover). The same
		// meaningful-field re-check as before; a failure is delete_partial.
		err := s.Registry.Update(func(reg *Registry) error {
			got, ok := findProject(reg, a.ID)
			if !ok {
				return opCodeErr("project_unknown")
			}
			if !sameRowCore(got, before) { // the row that was checked (flags may change meanwhile)
				return opCodeErr("remote_changed")
			}
			kept := make([]Project, 0, len(reg.Projects))
			for _, p := range reg.Projects {
				if p.ID != a.ID {
					kept = append(kept, p)
				}
			}
			reg.Projects = kept
			return nil
		})
		if err != nil {
			now := s.Registry.Snapshot()
			if _, still := findProject(&now, a.ID); !still && s.Registry.Err() == nil {
				s.logf("delete_remote_repo: the registry file was written but its directory fsync failed")
			} else {
				s.logf("delete_remote_repo: deleted at the provider, the registry row could not be removed")
				return nil, fail1("delete_partial")
			}
		}
		s.logf("delete_remote_repo: removed the remote repository and the registry row of project %s", a.ID)
		return map[string]any{"id": a.ID, "remote_state": state, "remote_deleted_at": stamp, "registry_removed": true}, nil
	}
	// The registry keeps Provider (attach_remote stays blocked by
	// already_linked) and records remote null + the time.
	err := s.Registry.Update(func(reg *Registry) error {
		for i := range reg.Projects {
			p := &reg.Projects[i]
			if p.ID != a.ID {
				continue
			}
			if !sameRowCore(*p, before) { // the row that was checked (flags may change meanwhile)
				return opCodeErr("remote_changed")
			}
			p.Remote = nil
			p.RemoteDeletedAt = &stamp
			return nil
		}
		return opCodeErr("project_unknown")
	})
	if err != nil {
		now := s.Registry.Snapshot()
		p, ok := findProject(&now, a.ID)
		if ok && p.Remote == nil && p.RemoteDeletedAt != nil && s.Registry.Err() == nil {
			s.logf("delete_remote_repo: the registry file was written but its directory fsync failed")
			stamp = *p.RemoteDeletedAt
		} else {
			s.logf("delete_remote_repo: deleted at the provider, the registry could not be updated")
			return nil, fail1("delete_partial")
		}
	}
	return map[string]any{"id": a.ID, "remote_state": state, "remote_deleted_at": stamp}, nil
}

// ---------- remove_deploy_key ----------

type removeDeployKeyArgs struct {
	ID         string             `json:"id"`
	RemotePath string             `json:"remote_path"`
	DeployKey  *string            `json:"deploy_key"` // REQUIRED, NOT signed: bound to the row's key_sha256 (W3h), and the key must match it
	Change     string             `json:"change"`
	Request    string             `json:"request"`
	Issued     string             `json:"issued"`
	Signature  *signing.Signature `json:"signature"`
}

func removeKeyCode(reg *Registry, a removeDeployKeyArgs) (Project, Provider, string) {
	p, ok := findProject(reg, a.ID)
	if !ok {
		return Project{}, Provider{}, "project_unknown"
	}
	if p.Provider == nil || p.Remote == nil {
		return p, Provider{}, "remote_changed"
	}
	i := findProvider(reg, *p.Provider)
	if i < 0 {
		return p, Provider{}, "provider_unknown"
	}
	prov := reg.Providers[i]
	if !registrymsg.ValidRemotePathForKind(prov.Kind, a.RemotePath) {
		return p, prov, "bad_request"
	}
	if !SameRepoPath(prov.Kind, a.RemotePath, p.Remote.Path) || p.Remote.ID == nil {
		return p, prov, "remote_changed"
	}
	if code := keyLineCode(prov, p, *a.DeployKey); code != "" { // W3h: the line must be the registered one
		return p, prov, code
	}
	return p, prov, ""
}

func (s *Server) removeDeployKeyHandler(ctx context.Context, raw json.RawMessage) (res any, oe *OpError) {
	var a removeDeployKeyArgs
	// The key line is required: without it nothing identifies the key but its
	// title, so nothing is sent (no ID burnt, no provider contact).
	if !decodeStrict(raw, &a, removeDeployKeyKeys) || a.DeployKey == nil {
		return fail("bad_request")
	}
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	fields := map[string]any{"remote_path": a.RemotePath}
	wantBody := ""
	shape := func() *OpError {
		if !registrymsg.ValidSubject(a.ID) || !registrymsg.ValidRemotePath(a.RemotePath) {
			return fail1("bad_request")
		}
		b, ok := keyBody(*a.DeployKey)
		if !deployKeyRE.MatchString(*a.DeployKey) || !ok {
			return fail1("bad_request")
		}
		wantBody = b
		reg := s.Registry.Snapshot()
		if _, _, code := removeKeyCode(&reg, a); code != "" {
			return fail1(code)
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionRemoveDeployKey, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	if !s.lockName(a.ID) {
		return fail("busy")
	}
	defer s.unlockName(a.ID)
	snap := s.Registry.Snapshot()
	before, prov, code := removeKeyCode(&snap, a)
	if code != "" {
		return fail(code)
	}
	rkey := remoteLockKey(prov.Kind, prov.ID, before.Remote.Path)
	if !s.lockName(rkey) {
		return fail("busy")
	}
	defer s.unlockName(rkey)
	rp, oe := s.providerForEntry(prov)
	if oe != nil {
		return nil, oe
	}
	info, oe := s.liveMatches(ctx, rp, prov.Kind, before)
	if oe != nil {
		return nil, oe
	}
	keys, err := rp.ListDeployKeys(ctx, info)
	if err != nil {
		return nil, apiFailKind(prov.Kind, err)
	}
	found, verdict := classifyKeys(prov.Kind, a.ID, wantBody, keys)
	switch verdict {
	case keyNone:
		return fail("key_not_found")
	case keyMany: // not unique: never guess which one is ours
		s.logf("remove_deploy_key: more than one key matches, nothing removed")
		return fail("remove_refused")
	case keyLookalike:
		s.logf("remove_deploy_key: a similar key exists but none is clearly ours, nothing removed")
		return fail("remove_refused")
	}
	if !s.allowLocalDelete() {
		return fail("rate_limited")
	}
	sent := false
	defer func() {
		if rec := recover(); rec != nil {
			s.logf("remove_deploy_key: panic type=%T", rec)
			if sent {
				res, oe = nil, fail1("delete_partial")
			} else {
				res, oe = nil, fail1("internal_error")
			}
		}
	}()
	sent = true
	if err := rp.RemoveDeployKey(ctx, info, found); err != nil {
		return nil, s.deleteFailure("remove_deploy_key", prov.Kind, err)
	}
	return map[string]any{"id": a.ID, "key_removed": true}, nil
}

// ---------- clear_orphan ----------

type clearOrphanArgs struct {
	ID        string             `json:"id"` // the provider id (subject of the signed change)
	Project   string             `json:"project"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

func orphanIndex(reg *Registry, provider, project string) int {
	for i, o := range reg.Orphans {
		if o.Provider == provider && o.Project == project { // exact: no case folding, no prefix
			return i
		}
	}
	return -1
}

// clearOrphanHandler removes ONE orphan record (the oldest with that provider
// and project: the signed change names no more than that). No provider call.
func (s *Server) clearOrphanHandler(_ context.Context, raw json.RawMessage) (any, *OpError) {
	var a clearOrphanArgs
	if !decodeStrict(raw, &a, clearOrphanKeys) {
		return fail("bad_request")
	}
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	fields := map[string]any{"project": a.Project}
	shape := func() *OpError {
		if !registrymsg.ValidProviderID(a.ID) || !registrymsg.ValidOrphanProject(a.Project) {
			return fail1("bad_request")
		}
		reg := s.Registry.Snapshot()
		if orphanIndex(&reg, a.ID, a.Project) < 0 {
			return fail1("orphan_unknown")
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionClearOrphan, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	lk := "orphan|" + a.ID + "|" + a.Project
	if !s.lockName(lk) {
		return fail("busy")
	}
	defer s.unlockName(lk)
	if !s.allowLocalDelete() {
		return fail("rate_limited")
	}
	err := s.Registry.Update(func(reg *Registry) error {
		i := orphanIndex(reg, a.ID, a.Project)
		if i < 0 {
			return opCodeErr("orphan_unknown")
		}
		reg.Orphans = append(append([]Orphan(nil), reg.Orphans[:i]...), reg.Orphans[i+1:]...)
		return nil
	})
	if err != nil {
		return nil, updateFailure(err)
	}
	return map[string]any{"provider": a.ID, "project": a.Project, "cleared": true}, nil
}
