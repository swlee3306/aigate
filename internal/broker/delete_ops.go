package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// W3f (F2): signed deletion ops. This is the first code of the broker that can
// delete anything at a provider, so every rule below is deliberately narrow.
//
//	remove_project      local only: the registry row goes, nothing at the provider
//	delete_remote_repo  the provider repository of a project THIS TOOL created
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
// remove_deploy_key's key identity. The signed change has only remote_path (and
// the subject, the project id), so the key cannot be bound by a signed hash. The
// rule: exactly one key of the repository has the title "orchd <project id>"
// (more than one: refuse, delete nothing; none: key_not_found). The optional
// unsigned `deploy_key` argument (orchd's public key line) can only NARROW this:
// when sent, the matching key must also carry that key body. It is never
// trusted to widen anything. GitLab: the key is taken off this project only.

const (
	maxDeleteRepoPerDay   = 3 // broker-wide, rolling 24 hours (F-D5)
	maxDeleteRepoPerHour  = 3 // broker-wide; per-actor limits are orchd's (the broker does not know the actor)
	maxLocalDeletePerHour = 10
)

var (
	removeProjectKeys    = []string{"id", "scope", "legacy_ack", "change", "request", "issued", "signature"}
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

// ---------- remove_project ----------

type removeProjectArgs struct {
	ID        string             `json:"id"`
	Scope     string             `json:"scope"`
	LegacyAck *bool              `json:"legacy_ack"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

// removeProjectCode: "" or the refusal code. legacy_ack must equal
// (origin == legacy) in BOTH directions.
func removeProjectCode(reg *Registry, id string, ack bool) (Project, string) {
	p, ok := findProject(reg, id)
	if !ok {
		return Project{}, "project_unknown"
	}
	legacy := p.Origin == OriginLegacy
	switch {
	case legacy && !ack:
		return p, "legacy_ack_required"
	case !legacy && ack:
		return p, "bad_request" // a second confirmation only exists for legacy projects
	}
	return p, ""
}

func (s *Server) removeProjectHandler(_ context.Context, raw json.RawMessage) (any, *OpError) {
	var a removeProjectArgs
	if !decodeStrict(raw, &a, removeProjectKeys) || a.LegacyAck == nil {
		return fail("bad_request")
	}
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	fields := map[string]any{"scope": a.Scope, "legacy_ack": *a.LegacyAck}
	shape := func() *OpError {
		if !registrymsg.ValidSubject(a.ID) || a.Scope != "local" {
			return fail1("bad_request")
		}
		reg := s.Registry.Snapshot()
		if _, code := removeProjectCode(&reg, a.ID, *a.LegacyAck); code != "" {
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
	before, code := removeProjectCode(&snap, a.ID, *a.LegacyAck)
	if code != "" {
		return fail(code)
	}
	if !s.allowLocalDelete() {
		return fail("rate_limited")
	}
	err := s.Registry.Update(func(reg *Registry) error {
		got, ok := findProject(reg, a.ID)
		if !ok || !reflect.DeepEqual(got, before) { // exactly the row that was checked
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
		} else {
			return nil, updateFailure(err)
		}
	}
	return map[string]any{"id": a.ID, "removed": true}, nil
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
	if name, ok := registrymsg.RemoteLastSegment(a.RemotePath); !ok || name != a.ConfirmName {
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
	// The repository is gone. The registry keeps Provider (attach_remote stays
	// blocked by already_linked) and records remote null + the time.
	stamp := s.now().UTC().Format(orphanTimeLayout)
	err := s.Registry.Update(func(reg *Registry) error {
		for i := range reg.Projects {
			p := &reg.Projects[i]
			if p.ID != a.ID {
				continue
			}
			if !reflect.DeepEqual(*p, before) { // exactly the row that was checked
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
	state := "deleted"
	if prov.Kind == "gitlab" {
		state = "requested" // GitLab may delay the deletion
	}
	return map[string]any{"id": a.ID, "remote_state": state, "remote_deleted_at": stamp}, nil
}

// ---------- remove_deploy_key ----------

type removeDeployKeyArgs struct {
	ID         string             `json:"id"`
	RemotePath string             `json:"remote_path"`
	DeployKey  *string            `json:"deploy_key"` // optional, NOT signed: can only narrow the match
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
	return p, prov, ""
}

func (s *Server) removeDeployKeyHandler(ctx context.Context, raw json.RawMessage) (res any, oe *OpError) {
	var a removeDeployKeyArgs
	if !decodeStrict(raw, &a, removeDeployKeyKeys) {
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
		if a.DeployKey != nil {
			b, ok := keyBody(*a.DeployKey)
			if !deployKeyRE.MatchString(*a.DeployKey) || !ok {
				return fail1("bad_request")
			}
			wantBody = b
		}
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
	title := "orchd " + a.ID
	var match []DeployKeyInfo
	for _, k := range keys {
		if k.Title == title && (wantBody == "" || k.Body == wantBody) {
			match = append(match, k)
		}
	}
	switch {
	case len(match) == 0:
		return fail("key_not_found")
	case len(match) > 1: // not unique: never guess which one is ours
		s.logf("remove_deploy_key: more than one key has the title, nothing removed")
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
	if err := rp.RemoveDeployKey(ctx, info, match[0]); err != nil {
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
