package broker

import "testing"

// Final review I4: until W3d moves the merge-request path to providers, a
// project that is linked to a provider must never take the W2b create_mr path
// (which would use the legacy gitlab section and token file of broker.json).
// Legacy projects (no provider) are unchanged.
func TestCreateMRRefusesProviderLinkedProject(t *testing.T) {
	m := newMREnv(t, nil)
	if _, oerr := m.call(t, m.args()); oerr != nil {
		t.Fatalf("control: legacy project: %s", oerr.Code)
	}
	n := len(m.gl.Reqs())
	id := "gl"
	if err := m.srv.Registry.Update(func(g *Registry) error {
		g.Providers = append(g.Providers, Provider{ID: id, Kind: "gitlab", BaseURL: "https://10.0.0.7", SSHURL: "ssh://git@10.0.0.7", TokenRef: "gl-admin"})
		g.Projects[0].Provider = &id
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, oerr := m.call(t, m.args())
	wantCode(t, oerr, "repo_not_mapped")
	if len(m.gl.Reqs()) != n {
		t.Fatal("GitLab was contacted for a provider-linked project")
	}
	// back to a legacy project: works again
	if err := m.srv.Registry.Update(func(g *Registry) error { g.Projects[0].Provider = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, oerr := m.call(t, m.args()); oerr != nil {
		t.Fatalf("legacy again: %s", oerr.Code)
	}
}

// Re-review 5: the status op lists create_mr-reachable projects (mr_projects):
// a provider-linked project is not one of them (same rule as GitLabProject).
func TestStatusMRProjectsSkipsProviderLinked(t *testing.T) {
	m := newMREnv(t, nil)
	list := func() []string {
		res, e := m.srv.Handlers["status"](nil, nil)
		if e != nil {
			t.Fatal(e.Code)
		}
		return res.(map[string]any)["mr_projects"].([]string)
	}
	if len(list()) != 1 {
		t.Fatalf("control: %v", list())
	}
	id := "gl"
	if err := m.srv.Registry.Update(func(g *Registry) error {
		g.Providers = append(g.Providers, Provider{ID: id, Kind: "gitlab", BaseURL: "https://10.0.0.7", SSHURL: "ssh://git@10.0.0.7", TokenRef: "gl-admin"})
		g.Projects[0].Provider = &id
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("a provider-linked project is listed as create_mr-reachable: %v", got)
	}
}
