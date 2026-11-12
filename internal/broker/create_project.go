package broker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// create_project (decision D4, amended by the C2/C3 rulings; W3e E-D4 makes it
// provider-generic: the flow talks to ONE RepoProvider instance, GitLab or GitHub):
//
//  1. signature and shape (name, group path / owner, template, deploy key line and
//     its signed hash, visibility: required for github, forbidden for gitlab); a
//     registry id or path clash is project_name_taken
//  2. look the group / owner up at the provider (group_unknown); what the
//     provider returns (GitLab namespace id, GitHub's spelling of the login) is
//     the ONLY target ever used, never the signed string
//  3. GET the repository path: it exists -> project_name_taken, never taken over
//  4. create the repository   } from here on every error is of UNKNOWN outcome
//  5. first files, README.md first (non-atomic)   (the result may have been
//  6. default branch must be main (GitLab: GET;    written even if we got an
//     GitHub: CommitFiles reads it after file 1)   error): an orphan record is
//  7. protect main (no force push, no deletion)    written, nothing is ever
//  8. deploy key                                   deleted or retried
//  9. LAST: register the project in the registry
//
// A failure at 4-9 answers create_partial with {provider, project, project_id,
// step}: the user cleans the provider by hand (this tool has no delete), and
// the name stays taken until then. Resume is not built.
//
// One exception (GitHub only): when the create-repository call itself is
// refused with 400/401/403/404/422/429 (or never sent), GitHub made nothing, so
// the plain provider code is answered and NO orphan is recorded.

// PartialDetail is the public, value-only detail of create_partial.
type PartialDetail struct {
	Provider       string `json:"provider"`
	Project        string `json:"project"`
	ProjectID      *int64 `json:"project_id"`
	Step           string `json:"step"`
	OrphanRecorded bool   `json:"orphan_recorded"`
	OrphanDropped  bool   `json:"orphan_dropped"`
	Origin         string `json:"origin"` // OriginCreated (create_partial) or OriginLinked (link_partial)
}

// Step names of the orphan record and of create_partial.
const (
	stepProject       = "project"
	stepFirstCommit   = "first_commit"
	stepDefaultBranch = "default_branch"
	stepProtect       = "protect"
	stepDeployKey     = "deploy_key"
	stepRegister      = "register"
)

var createProjectKeys = []string{"id", "provider", "group_path", "template", "description", "ai_context", "publish",
	"deploy_key_sha256", "visibility", "deploy_key", "change", "request", "issued", "signature"}

type createProjectArgs struct {
	ID              string `json:"id"` // the new project's name (the subject of the signed change)
	Provider        string `json:"provider"`
	GroupPath       string `json:"group_path"`
	Template        string `json:"template"`
	Description     string `json:"description"`
	AIContext       *bool  `json:"ai_context"`
	Publish         string `json:"publish"`
	DeployKeySHA256 string `json:"deploy_key_sha256"`
	// Visibility is optional in the signed schema; whether it is required
	// (github) or forbidden (gitlab) depends on the provider kind, which only
	// this op knows. No default: absent stays absent.
	Visibility *string            `json:"visibility"`
	DeployKey  string             `json:"deploy_key"` // the public key LINE; only its hash is signed
	Change     string             `json:"change"`
	Request    string             `json:"request"`
	Issued     string             `json:"issued"`
	Signature  *signing.Signature `json:"signature"`
}

// signedFields is the change the signature covers. visibility is part of it
// only when the request carries it (no default is ever filled in).
func (a createProjectArgs) signedFields() map[string]any {
	f := map[string]any{"provider": a.Provider, "group_path": a.GroupPath, "template": a.Template,
		"description": a.Description, "ai_context": *a.AIContext, "publish": a.Publish, "deploy_key_sha256": a.DeployKeySHA256}
	if a.Visibility != nil {
		f["visibility"] = *a.Visibility
	}
	return f
}

// lockName reserves name for one create at a time (same-name creates are
// serialised: exactly one proceeds, the others see project_name_taken).
func (s *Server) lockName(name string) bool {
	s.creatingMu.Lock()
	defer s.creatingMu.Unlock()
	if s.creating == nil {
		s.creating = map[string]bool{}
	}
	if s.creating[name] {
		return false
	}
	s.creating[name] = true
	return true
}

func (s *Server) unlockName(name string) {
	s.creatingMu.Lock()
	delete(s.creating, name)
	s.creatingMu.Unlock()
}

// projectPath is where orchd will clone the project: one level below the
// configured repo root (the broker never touches that path).
func (s *Server) projectPath(name string) (string, bool) {
	if len(s.Config.RepoRoots) == 0 {
		return "", false
	}
	p := path.Join(s.Config.RepoRoots[0], name)
	return p, s.pathAllowed(p)
}

func registryHasProject(r *Registry, id, p string) bool {
	for _, x := range r.Projects {
		if x.ID == id || x.Path == p {
			return true
		}
	}
	return false
}

// createProviderRules are the kind/visibility/owner rules of create_project for
// ONE provider entry, as a refusal code ("" = fine). They run in the shape stage
// and again on the provider re-read after the name lock (a signed request must
// never be carried out under a provider whose kind changed in between).
func createProviderRules(a createProjectArgs, prov Provider) string {
	switch prov.Kind {
	case "gitlab":
		if a.Visibility != nil {
			return "visibility_not_allowed"
		}
		if !registrymsg.ValidGroupPath(a.GroupPath) { // the signed field is a superset: GitLab keeps the W3c rule
			return "bad_request"
		}
	case "github":
		switch {
		case a.Visibility == nil:
			return "visibility_required"
		case *a.Visibility != "public" && *a.Visibility != "private":
			return "bad_request"
		case !validGHOwnerLogin(a.GroupPath): // an owner is ONE login (case kept), not a path
			return "bad_request"
		}
	default:
		return "not_supported_for_provider"
	}
	return ""
}

func (s *Server) createProjectHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a createProjectArgs
	if !decodeStrict(raw, &a, createProjectKeys) || a.AIContext == nil {
		return fail("bad_request")
	}
	// The store is checked BEFORE the signature: a refusal here burns no ID.
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	fields := a.signedFields()
	shape := func() *OpError { // everything decidable from the request alone: no ID burnt
		if !registrymsg.ValidName(a.ID) {
			return fail1("bad_request")
		}
		if a.Template != "basic" && a.Template != "go" {
			return fail1("template_unknown")
		}
		if !deployKeyRE.MatchString(a.DeployKey) {
			return fail1("bad_request")
		}
		sum := sha256.Sum256([]byte(a.DeployKey))
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(a.DeployKeySHA256)) != 1 {
			return fail1("deploy_key_mismatch")
		}
		if _, ok := s.projectPath(a.ID); !ok {
			return fail1("path_not_allowed")
		}
		// Visibility rules need the provider's kind (an unknown provider is
		// answered later, as before). All of this is decidable from the request
		// and the registry alone, so no ID is burnt.
		if prov, ok := s.Registry.ProviderByID(a.Provider); ok {
			if code := createProviderRules(a, prov); code != "" {
				return fail1(code)
			}
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionCreateProject, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed.
	projPath, _ := s.projectPath(a.ID)
	if !s.lockName(a.ID) {
		return fail("project_name_taken")
	}
	defer s.unlockName(a.ID)
	// (1) the registry, after the lock: an earlier create may just have finished
	snap := s.Registry.Snapshot()
	if registryHasProject(&snap, a.ID, projPath) {
		return fail("project_name_taken")
	}
	files := TemplateFiles(a.Template, a.ID)
	if files == nil {
		return fail("template_unknown")
	}
	// The provider as it is NOW; the provider calls and the final registry write
	// must both see exactly this entry.
	prov, ok := s.Registry.ProviderByID(a.Provider)
	if !ok {
		return fail("provider_unknown")
	}
	// the kind may have changed since the shape stage: the rules apply to THIS entry
	if code := createProviderRules(a, prov); code != "" {
		return fail(code)
	}
	// ONE provider instance for the whole flow (the GitHub client honours the
	// justCreated handle only for a repository it created itself).
	rp, oe := s.providerForEntry(prov)
	if oe != nil {
		return nil, oe
	}

	// (2) the owner; what the provider returns is the only target ever used
	owner, err := rp.GetOwner(ctx, a.GroupPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fail("group_unknown")
		}
		return nil, apiFailKind(prov.Kind, err)
	}
	// (3) a repository of that name at the provider is never taken over
	full := owner.Path + "/" + a.ID
	if _, err := rp.GetRepo(ctx, full); err == nil {
		return fail("project_name_taken")
	} else if !errors.Is(err, ErrNotFound) {
		return nil, apiFailKind(prov.Kind, err)
	}
	// The quota is spent only now, right before the first provider write.
	if s.State.CreateProject != nil && !s.State.CreateProject.Allow() {
		return fail("rate_limited")
	}
	return s.createWritePhase(ctx, a, rp, prov, owner, full, projPath, files)
}

// createWritePhase is steps 4-9. Every error, and any panic, from here on is
// of unknown outcome: an orphan record with the CURRENT step is written and
// create_partial is answered. Nothing is retried or deleted. rp is the SAME
// provider instance that looked the owner up.
func (s *Server) createWritePhase(ctx context.Context, a createProjectArgs, rp RepoProvider, prov Provider, owner Owner,
	full, projPath string, files []File) (res any, oe *OpError) {
	step := stepProject
	var pid *int64
	partial := func() (any, *OpError) {
		d := PartialDetail{Provider: a.Provider, Project: full, ProjectID: pid, Step: step, Origin: OriginCreated}
		dropped, oerr := s.Registry.AddOrphan(Orphan{Provider: a.Provider, Project: full, ProjectID: pid, Step: step,
			At: s.now().UTC().Format(orphanTimeLayout), Origin: OriginCreated})
		d.OrphanRecorded = oerr == nil
		d.OrphanDropped = dropped
		if oerr != nil {
			s.logf("create_project: the orphan record could not be saved (step=%s)", step)
		}
		s.logf("create_project: partial result at step=%s", step)
		return nil, &OpError{Code: "create_partial", Data: d}
	}
	defer func() {
		if r := recover(); r != nil {
			s.logf("create_project: panic at step=%s type=%T", step, r) // never the value
			res, oe = partial()
		}
	}()

	req := CreateRepoReq{Owner: owner, Name: a.ID, Description: a.Description, Private: true} // GitLab: always private
	if prov.Kind == "github" {
		req.Private = *a.Visibility == "private" // the signed choice; shape guaranteed it is present
	}
	info, err := rp.CreateRepo(ctx, req)
	if err != nil {
		if prov.Kind == "github" && refusedBeforeWrite(err) {
			// GitHub refused the create call itself: nothing exists, so there is
			// no orphan (the W3c rule below stays for GitLab and for any other error).
			s.logf("create_project: the provider refused the create call")
			return nil, apiFailKind(prov.Kind, err)
		}
		return partial()
	}
	id := info.ID
	pid = &id
	step = stepFirstCommit
	if err := rp.CommitFiles(ctx, info, files); err != nil {
		if errors.Is(err, ErrDefaultBranchMismatch) { // GitHub: the first file made another branch the default
			step = stepDefaultBranch
		}
		return partial()
	}
	if prov.Kind == "gitlab" {
		// (f) the first commit made main the default branch, or we do not register
		// (GitHub reads it inside CommitFiles, right after the first file)
		step = stepDefaultBranch
		if pi, err := rp.GetRepo(ctx, full); err != nil || pi.ID != id || pi.DefaultBranch != apiBranch {
			return partial()
		}
	}
	step = stepProtect
	if err := rp.EnsureProtectedMain(ctx, info); err != nil {
		return partial()
	}
	step = stepDeployKey
	if err := rp.AddDeployKey(ctx, info, "orchd "+a.ID, a.DeployKey, a.Publish == "write"); err != nil {
		return partial()
	}
	// (g) the registry entry comes LAST; a failure here leaves an orphan record
	step = stepRegister
	provID := a.Provider
	entry := Project{ID: a.ID, Path: projPath, Kind: "registered", AIContext: *a.AIContext, Provider: &provID,
		Remote: &RemoteLink{Path: full, ID: &id}}
	err = s.Registry.Update(func(r *Registry) error {
		if registryHasProject(r, a.ID, projPath) {
			return opCodeErr("project_name_taken")
		}
		// the provider the calls used must still be the stored one
		if i := findProvider(r, a.Provider); i < 0 || r.Providers[i] != prov {
			return opCodeErr("provider_unknown")
		}
		r.Projects = append(r.Projects, entry)
		return nil
	})
	if err != nil {
		// A write that failed only on the directory fsync keeps the state: if
		// the entry is there, the project IS registered.
		snap := s.Registry.Snapshot()
		for _, p := range snap.Projects {
			if p.ID == a.ID && p.Path == projPath && p.Remote != nil && p.Remote.ID != nil && *p.Remote.ID == id {
				s.logf("create_project: the registry file was written but its directory fsync failed")
				err = nil
			}
		}
		if err != nil {
			return partial()
		}
	}
	return map[string]any{
		"project":     entry,
		"ssh_url":     prov.SSHURL,
		"clone_url":   prov.SSHURL + "/" + full + ".git",
		"cloned_hint": false, // the broker never clones; orchd does, from the registry entry
	}, nil
}
