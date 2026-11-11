package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// Provider, group and project-creation ops (W3c, decisions D2-D4, D9).
//
// Signed writes (set_provider, delete_provider, create_group, create_project)
// follow the W3a/W3b verifySigned order exactly: strict args -> store usable ->
// not_signed -> change required -> shape (no ID burnt) -> signers reloaded ->
// message rebuilt -> VerifyRegistry -> change equality -> only then the request
// ID is consumed. After that, any refusal has burnt the ID. Reads (providers,
// list_groups) need no signature and never return a token value.
//
// A `timeout` reply to any of the writes means the outcome is UNKNOWN: the
// handler goes on and records what it did (an orphan for create_project).
//
// Nothing here touches the filesystem: the broker's account cannot see
// /home, and the properties it protects are all syntactic.

const (
	maxCreateProjectPerHour = 10 // broker-wide (D9); per-actor limits are orchd's
	maxCreateGroupPerHour   = 10
)

var (
	setProviderKeys    = []string{"id", "kind", "base_url", "ssh_url", "token_ref", "change", "request", "issued", "signature"}
	deleteProviderKeys = []string{"id", "change", "request", "issued", "signature"}
	createGroupKeys    = []string{"id", "name", "path", "parent_path", "change", "request", "issued", "signature"}
	listGroupsKeys     = []string{"provider"}
)

// apiFail maps a provider-layer error to a registered fixed code. Nothing of
// the error but its code is used (GitLab text is never read into one).
func apiFail(err error) *OpError {
	code := WireCode(err)
	if code == CodeNotFound { // callers that care map 404 themselves
		code = CodeGitLabRefused
	}
	if _, ok := fixedMessages[code]; !ok || code == "" {
		code = "internal_error"
	}
	return fail1(code)
}

// apiFor builds the client of provider id. Errors are fixed codes.
func (s *Server) apiFor(id string) (*GitLabAPI, *OpError) {
	api, err := s.GitLabAPIFor(id)
	if err != nil {
		if errors.Is(err, ErrProviderUnknown) {
			return nil, fail1("provider_unknown")
		}
		return nil, apiFail(err)
	}
	return api, nil
}

// providersHandler is the read-only provider list (D3). It shows addresses and
// the token's NAME (never a value); in_use says whether a project references
// the provider.
func (s *Server) providersHandler(context.Context, json.RawMessage) (any, *OpError) {
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	r := s.Registry.Snapshot()
	inUse := map[string]bool{}
	for _, p := range r.Projects {
		if p.Provider != nil {
			inUse[*p.Provider] = true
		}
	}
	rows := make([]map[string]any, 0, len(r.Providers))
	for _, p := range r.Providers {
		rows = append(rows, map[string]any{"id": p.ID, "kind": p.Kind, "base_url": p.BaseURL, "ssh_url": p.SSHURL,
			"token_ref": p.TokenRef, "in_use": inUse[p.ID]})
	}
	return map[string]any{"providers": rows}, nil
}

type listGroupsArgs struct {
	Provider string `json:"provider"`
}

// listGroupsHandler asks GitLab (read-only) for the groups the token is at
// least Maintainer in. It returns path and name only, plus `truncated` when
// the 500-group cap or a skipped name made the list incomplete.
func (s *Server) listGroupsHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a listGroupsArgs
	if !decodeStrict(raw, &a, listGroupsKeys) || !registrymsg.ValidProviderID(a.Provider) {
		return fail("bad_request")
	}
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	api, oe := s.apiFor(a.Provider)
	if oe != nil {
		return nil, oe
	}
	groups, truncated, err := api.GroupsTruncated(ctx)
	if err != nil {
		return nil, apiFail(err)
	}
	rows := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		rows = append(rows, map[string]any{"path": g.FullPath, "name": g.Name})
	}
	return map[string]any{"groups": rows, "truncated": truncated}, nil
}

type setProviderArgs struct {
	ID        string             `json:"id"`
	Kind      string             `json:"kind"`
	BaseURL   string             `json:"base_url"`
	SSHURL    string             `json:"ssh_url"`
	TokenRef  string             `json:"token_ref"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

// setProviderHandler adds a provider or replaces the one with the same id.
// The signed change carries base_url, ssh_url and token_ref, so an address
// change (even with the same token_ref) is something the signer saw.
func (s *Server) setProviderHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a setProviderArgs
	if !decodeStrict(raw, &a, setProviderKeys) {
		return fail("bad_request")
	}
	// Stores usable BEFORE the signature: a refusal here burns no ID.
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	if s.Tokens.Err() != nil {
		return fail("tokens_unavailable")
	}
	p := Provider{ID: a.ID, Kind: a.Kind, BaseURL: a.BaseURL, SSHURL: a.SSHURL, TokenRef: a.TokenRef}
	fields := map[string]any{"kind": a.Kind, "base_url": a.BaseURL, "ssh_url": a.SSHURL, "token_ref": a.TokenRef}
	shape := func() *OpError { // address rules before the signature: no ID burnt
		if err := s.validateProviderEntry(p); err != nil {
			if ErrCode(err) == CodeBadURL {
				return fail1("bad_url")
			}
			return fail1("bad_request")
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionSetProvider, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed.
	// (c) the token must exist, be of kind gitlab and be usable: the store's
	// own provider storage does not check that.
	if s.Tokens.Err() != nil {
		return fail("tokens_unavailable")
	}
	if !s.gitlabTokenUsable(p.TokenRef) {
		return fail("token_ref_unknown")
	}
	// A host NAME must resolve to at least one allowed address (the connection
	// re-checks every address, so DNS rebinding does not help).
	if oe := s.checkProviderHost(ctx, p.BaseURL); oe != nil {
		return nil, oe
	}
	err := s.Registry.Update(func(r *Registry) error {
		i := findProvider(r, p.ID)
		if i < 0 {
			r.Providers = append(r.Providers, p)
			return nil
		}
		// (d) a provider that projects use keeps its addresses: clone and push
		// remotes are derived from them, so moving them under existing projects
		// would redirect those. A token swap or the same values is fine.
		old := r.Providers[i]
		if (old.BaseURL != p.BaseURL || old.SSHURL != p.SSHURL) && providerReferenced(r, p.ID) {
			return ErrProviderInUse
		}
		r.Providers[i] = p
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrProviderInUse) {
			return fail("provider_in_use")
		}
		return nil, updateFailure(err)
	}
	return map[string]any{"id": p.ID, "kind": p.Kind, "base_url": p.BaseURL, "ssh_url": p.SSHURL, "token_ref": p.TokenRef}, nil
}

func providerReferenced(r *Registry, id string) bool {
	for _, p := range r.Projects {
		if p.Provider != nil && *p.Provider == id {
			return true
		}
	}
	return false
}

// gitlabTokenUsable: the store has a usable token of kind gitlab under ref.
// Only metadata is read, never the value.
func (s *Server) gitlabTokenUsable(ref string) bool {
	list, err := s.Tokens.List(s.now())
	if err != nil {
		return false
	}
	for _, t := range list {
		if t.Ref == ref {
			return t.Kind == "gitlab" && t.Usable
		}
	}
	return false
}

// checkProviderHost resolves a host name (not an IP literal) of base and
// refuses it when no resolved address is allowed.
func (s *Server) checkProviderHost(ctx context.Context, base string) *OpError {
	_, rest, _ := strings.Cut(base, "://")
	if _, err := netip.ParseAddr(authorityHost(rest)); err == nil {
		return nil // literal: the address rules already judged it
	}
	lookup := defaultLookup
	if s.apiSeams != nil && s.apiSeams.lookup != nil {
		lookup = s.apiSeams.lookup
	}
	if err := checkResolves(ctx, base, lookup, s.policy()); err != nil {
		return apiFail(err)
	}
	return nil
}

type deleteProviderArgs struct {
	ID        string             `json:"id"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

func (s *Server) deleteProviderHandler(_ context.Context, raw json.RawMessage) (any, *OpError) {
	var a deleteProviderArgs
	if !decodeStrict(raw, &a, deleteProviderKeys) {
		return fail("bad_request")
	}
	if oe := s.verifySigned(registrymsg.ActionDeleteProvider, a.ID, map[string]any{}, a.Change, a.Request, a.Issued, a.Signature); oe != nil {
		return nil, oe
	}
	if err := s.Registry.DeleteProvider(a.ID); err != nil {
		switch {
		case errors.Is(err, ErrProviderInUse):
			return fail("provider_in_use")
		case errors.Is(err, ErrProviderUnknown):
			return fail("provider_unknown")
		}
		return nil, updateFailure(err)
	}
	return map[string]any{"id": a.ID, "deleted": true}, nil
}

type createGroupArgs struct {
	ID         string             `json:"id"` // the provider id (the subject of the signed change)
	Name       string             `json:"name"`
	Path       string             `json:"path"`
	ParentPath string             `json:"parent_path"`
	Change     string             `json:"change"`
	Request    string             `json:"request"`
	Issued     string             `json:"issued"`
	Signature  *signing.Signature `json:"signature"`
}

// createGroupHandler creates ONE private group (visibility is fixed in code)
// under an existing parent or at the top level. It never deletes or changes a
// group. A failure after the POST is of unknown outcome; the web reloads the
// group list (a retry answers group_exists when it did happen).
func (s *Server) createGroupHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a createGroupArgs
	if !decodeStrict(raw, &a, createGroupKeys) {
		return fail("bad_request")
	}
	fields := map[string]any{"name": a.Name, "path": a.Path, "parent_path": a.ParentPath}
	if oe := s.verifySigned(registrymsg.ActionCreateGroup, a.ID, fields, a.Change, a.Request, a.Issued, a.Signature); oe != nil {
		return nil, oe
	}
	api, oe := s.apiFor(a.ID)
	if oe != nil {
		return nil, oe
	}
	full := a.Path
	if a.ParentPath != "" {
		full = a.ParentPath + "/" + a.Path
		if _, err := api.Group(ctx, a.ParentPath); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fail("group_unknown")
			}
			return nil, apiFail(err)
		}
	}
	if _, err := api.Group(ctx, full); err == nil {
		return fail("group_exists")
	} else if !errors.Is(err, ErrNotFound) {
		return nil, apiFail(err)
	}
	// The quota is spent only now, right before the write.
	if s.State.CreateGroup != nil && !s.State.CreateGroup.Allow() {
		return fail("rate_limited")
	}
	g, err := api.CreateGroup(ctx, a.Name, a.Path, a.ParentPath)
	if err != nil {
		return nil, apiFail(err)
	}
	return map[string]any{"provider": a.ID, "group": map[string]any{"path": g.FullPath, "name": g.Name}}, nil
}
