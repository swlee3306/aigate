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

// create_project (decision D4, amended by the C2/C3 rulings):
//
//  1. signature and shape (name, group path, template, deploy key line and its
//     signed hash); a registry id or path clash is project_name_taken
//  2. GET the group (group_unknown); its id is the ONLY namespace_id used
//  3. GET the project path: it exists -> project_name_taken, never taken over
//  4. POST /projects          } from here on every error is of UNKNOWN outcome
//  5. first files, README.md first (non-atomic)    (the result may have been
//  6. GET the project: default_branch must be main  written even if we got an
//  7. protect main (Maintainer, no force push)      error): an orphan record is
//  8. deploy key                                    written, nothing is ever
//  9. LAST: register the project in the registry    deleted or retried
//
// A failure at 4-9 answers create_partial with {provider, project, project_id,
// step}: the user cleans GitLab by hand (this tool has no delete), and the name
// stays taken until then. Resume is not built.

// PartialDetail is the public, value-only detail of create_partial.
type PartialDetail struct {
	Provider       string `json:"provider"`
	Project        string `json:"project"`
	ProjectID      *int64 `json:"project_id"`
	Step           string `json:"step"`
	OrphanRecorded bool   `json:"orphan_recorded"`
	OrphanDropped  bool   `json:"orphan_dropped"`
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
	"deploy_key_sha256", "deploy_key", "change", "request", "issued", "signature"}

type createProjectArgs struct {
	ID              string             `json:"id"` // the new project's name (the subject of the signed change)
	Provider        string             `json:"provider"`
	GroupPath       string             `json:"group_path"`
	Template        string             `json:"template"`
	Description     string             `json:"description"`
	AIContext       *bool              `json:"ai_context"`
	Publish         string             `json:"publish"`
	DeployKeySHA256 string             `json:"deploy_key_sha256"`
	DeployKey       string             `json:"deploy_key"` // the public key LINE; only its hash is signed
	Change          string             `json:"change"`
	Request         string             `json:"request"`
	Issued          string             `json:"issued"`
	Signature       *signing.Signature `json:"signature"`
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

func (s *Server) createProjectHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a createProjectArgs
	if !decodeStrict(raw, &a, createProjectKeys) || a.AIContext == nil {
		return fail("bad_request")
	}
	fields := map[string]any{"provider": a.Provider, "group_path": a.GroupPath, "template": a.Template,
		"description": a.Description, "ai_context": *a.AIContext, "publish": a.Publish, "deploy_key_sha256": a.DeployKeySHA256}
	shape := func() *OpError { // no ID burnt
		if a.Template != "basic" && a.Template != "go" {
			return fail1("template_unknown")
		}
		if !deployKeyRE.MatchString(a.DeployKey) {
			return fail1("bad_request")
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionCreateProject, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed.
	sum := sha256.Sum256([]byte(a.DeployKey))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(a.DeployKeySHA256)) != 1 {
		return fail("deploy_key_mismatch")
	}
	if s.State.CreateProject != nil && !s.State.CreateProject.Allow() {
		return fail("rate_limited")
	}
	projPath, ok := s.projectPath(a.ID)
	if !ok {
		return fail("path_not_allowed")
	}
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
	api, oe := s.apiFor(a.Provider)
	if oe != nil {
		return nil, oe
	}
	prov, _ := s.Registry.ProviderByID(a.Provider)

	// (2) the group; its id is the only namespace id ever used
	grp, err := api.Group(ctx, a.GroupPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fail("group_unknown")
		}
		return nil, apiFail(err)
	}
	// (3) a project of that name on GitLab is never taken over
	full := a.GroupPath + "/" + a.ID
	if _, err := api.Project(ctx, full); err == nil {
		return fail("project_name_taken")
	} else if !errors.Is(err, ErrNotFound) {
		return nil, apiFail(err)
	}

	// (4)-(8): every error from here on is of unknown outcome
	partial := func(step string, id *int64) (any, *OpError) {
		d := PartialDetail{Provider: a.Provider, Project: full, ProjectID: id, Step: step}
		dropped, oerr := s.Registry.AddOrphan(Orphan{Provider: a.Provider, Project: full, ProjectID: id, Step: step,
			At: s.now().UTC().Format(orphanTimeLayout)})
		d.OrphanRecorded = oerr == nil
		d.OrphanDropped = dropped
		if oerr != nil {
			s.logf("create_project: the orphan record could not be saved (step=%s)", step)
		}
		s.logf("create_project: partial result at step=%s", step)
		return nil, &OpError{Code: "create_partial", Data: d}
	}
	info, err := api.CreateProject(ctx, a.GroupPath, grp.ID, a.ID, a.Description)
	if err != nil {
		return partial(stepProject, nil)
	}
	id := info.ID
	if err := api.CommitFiles(ctx, id, files); err != nil {
		return partial(stepFirstCommit, &id)
	}
	// (f) the first commit made main the default branch, or we do not register
	if pi, err := api.Project(ctx, full); err != nil || pi.ID != id || pi.DefaultBranch != apiBranch {
		return partial(stepDefaultBranch, &id)
	}
	if err := api.EnsureProtectedMain(ctx, id); err != nil {
		return partial(stepProtect, &id)
	}
	if err := api.AddDeployKey(ctx, id, "orchd "+a.ID, a.DeployKey, a.Publish == "write"); err != nil {
		return partial(stepDeployKey, &id)
	}
	// (g) the registry entry comes LAST; a failure here leaves an orphan record
	provID := a.Provider
	entry := Project{ID: a.ID, Path: projPath, Kind: "registered", AIContext: *a.AIContext, Provider: &provID,
		GitLab: &GitLabLink{Project: full, ProjectID: &id}}
	if err := s.Registry.Update(func(r *Registry) error {
		if registryHasProject(r, a.ID, projPath) {
			return opCodeErr("project_name_taken")
		}
		r.Projects = append(r.Projects, entry)
		return nil
	}); err != nil {
		return partial(stepRegister, &id)
	}
	return map[string]any{
		"project":     entry,
		"ssh_url":     prov.SSHURL,
		"clone_url":   prov.SSHURL + "/" + full + ".git",
		"cloned_hint": false, // the broker never clones; orchd does, from the registry entry
	}, nil
}
