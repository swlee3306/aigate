package broker

import (
	"path/filepath"

	"aigate/internal/registrymsg"
)

// Provider storage over the registry. Every write goes through
// RegistryStore.Update (validated, atomic, rolled back on failure): there is no
// other write path for the registry file. The signed-request layer (C3) calls
// these after the signature and the one-time request ID have been handled.

// providerShape checks one provider entry: id, kind, token_ref, base_url and
// ssh_url shapes plus the address rules (no loopback, link-local, ...).
func providerShape(p Provider, allow ipPolicy, strictHTTP bool) error {
	if !registrymsg.ValidProviderID(p.ID) || !registrymsg.ValidProviderKind(p.Kind) || !registrymsg.ValidTokenRef(p.TokenRef) {
		return ErrBadInput
	}
	base := validateProviderURL
	if p.Kind == "github" {
		base = validateGitHubBase // kind-aware: the web host github.com is not an API host
	}
	if err := base(p.BaseURL, allow, strictHTTP); err != nil {
		return err
	}
	return validateSSHURL(p.SSHURL, allow)
}

func findProvider(r *Registry, id string) int {
	for i, p := range r.Providers {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// AddProvider adds p; ErrProviderExists when the id is taken.
func (s *RegistryStore) AddProvider(p Provider) error {
	return s.putProvider(p, true, defaultIPPolicy, true)
}

// ReplaceProvider replaces the entry with p's id; ErrProviderUnknown when none.
func (s *RegistryStore) ReplaceProvider(p Provider) error {
	return s.putProvider(p, false, defaultIPPolicy, true)
}

func (s *RegistryStore) putProvider(p Provider, add bool, allow ipPolicy, strictHTTP bool) error {
	if err := providerShape(p, allow, strictHTTP); err != nil {
		return err
	}
	return s.Update(func(r *Registry) error {
		i := findProvider(r, p.ID)
		switch {
		case add && i >= 0:
			return ErrProviderExists
		case !add && i < 0:
			return ErrProviderUnknown
		case add:
			r.Providers = append(r.Providers, p)
		default:
			r.Providers[i] = p
		}
		return nil
	})
}

// DeleteProvider removes the provider; ErrProviderInUse while a project
// references it, ErrProviderUnknown when there is none. Orphan records do not
// block (they outlive the provider).
func (s *RegistryStore) DeleteProvider(id string) error {
	return s.Update(func(r *Registry) error {
		i := findProvider(r, id)
		if i < 0 {
			return ErrProviderUnknown
		}
		for _, p := range r.Projects {
			if p.Provider != nil && *p.Provider == id {
				return ErrProviderInUse
			}
		}
		r.Providers = append(r.Providers[:i:i], r.Providers[i+1:]...)
		return nil
	})
}

// ProviderByID returns a copy of the provider from the live registry.
func (s *RegistryStore) ProviderByID(id string) (Provider, bool) {
	snap := s.Snapshot()
	if i := findProvider(&snap, id); i >= 0 {
		return snap.Providers[i], true
	}
	return Provider{}, false
}

// GitLabAPIFor builds the client of the configured provider id. The base URL
// is re-validated here (a hand-edited registry cannot reach a forbidden
// address). ErrProviderUnknown when there is no such provider.
func (s *Server) GitLabAPIFor(providerID string) (*GitLabAPI, error) {
	p, ok := s.Registry.ProviderByID(providerID)
	if !ok {
		return nil, ErrProviderUnknown
	}
	return newGitLabAPI(p, s.Tokens, s.apiSeams)
}

// validateProviderEntry is what a set_provider handler calls before storing
// (shape and address rules; the policy seam is the server's, nil in production).
func (s *Server) validateProviderEntry(p Provider) error {
	allow := defaultIPPolicy
	if s.apiSeams != nil && s.apiSeams.policy != nil {
		allow = s.apiSeams.policy
	}
	return providerShape(p, allow, s.apiSeams.strictHTTP())
}

// AddProviderChecked and ReplaceProviderChecked store p after the server's
// address rules (identical to AddProvider/ReplaceProvider in production).
func (s *Server) AddProviderChecked(p Provider) error {
	return s.Registry.putProvider(p, true, s.policy(), s.apiSeams.strictHTTP())
}

// ReplaceProviderChecked: see AddProviderChecked.
func (s *Server) ReplaceProviderChecked(p Provider) error {
	return s.Registry.putProvider(p, false, s.policy(), s.apiSeams.strictHTTP())
}

func (s *Server) policy() ipPolicy {
	if s.apiSeams != nil && s.apiSeams.policy != nil {
		return s.apiSeams.policy
	}
	return defaultIPPolicy
}

// ProviderStatus is the read-only part of orch-broker -check for providers: it
// reads the registry and the token store without writing, creating or
// connecting anything. refuse is a fixed sentence when a stored provider
// breaks the shape or address rules; warnings are fixed sentences (a missing
// or unusable gitlab token only warns: the broker still starts and answers
// token_ref_unknown for that provider).
func ProviderStatus(stateDir string, o RegistryOptions) (refuse error, warnings []string) {
	data, exists, err := readStateFile(filepath.Join(stateDir, RegistryFileName), o.fileOwner())
	if err != nil || !exists {
		return nil, nil // RegistryStatus already judged the file
	}
	reg, _, err := parseRegistry(data)
	if err != nil {
		return nil, nil
	}
	if len(reg.Providers) == 0 {
		return nil, nil
	}
	kinds := map[string]string{}
	if data, ok, rerr := readStateFile(filepath.Join(stateDir, TokensFileName), o.fileOwner()); rerr == nil && ok {
		if entries, perr := parseTokensFile(data); perr == nil {
			for _, e := range entries {
				kinds[e.Ref] = e.Kind
			}
		}
	}
	sec := &SecretStore{Dir: filepath.Join(stateDir, "secrets"), opts: o}
	for _, p := range reg.Providers {
		if providerShape(p, defaultIPPolicy, true) != nil {
			return errProviderAddress, warnings
		}
		if kinds[p.TokenRef] != p.Kind {
			warnings = append(warnings, "경고: 제공자가 가리키는 "+p.Kind+" 종류 토큰이 저장소에 없어 그 제공자는 token_ref_unknown이 됩니다")
		} else if sec.Lookup(p.TokenRef) != SecretOK {
			warnings = append(warnings, "경고: 제공자가 가리키는 토큰의 비밀 파일을 쓸 수 없어 그 제공자는 token_ref_unknown이 됩니다")
		}
	}
	return nil, warnings
}

var errProviderAddress = errString("저장된 제공자의 주소 또는 토큰 이름 형식이 올바르지 않습니다(루프백·링크 로컬 주소 거부 포함)")

type errString string

func (e errString) Error() string { return string(e) }
