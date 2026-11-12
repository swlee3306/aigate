package broker

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func goodProvider(id string) Provider {
	return Provider{ID: id, Kind: "gitlab", BaseURL: "http://100.64.0.10:18082", SSHURL: "ssh://git@100.64.0.10", TokenRef: "gl-admin"}
}

func TestProviderStoreRoundTrip(t *testing.T) {
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{})
	if err := st.AddProvider(goodProvider("corp")); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProvider(goodProvider("corp")); !errors.Is(err, ErrProviderExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := st.ReplaceProvider(goodProvider("nope")); !errors.Is(err, ErrProviderUnknown) {
		t.Fatalf("replace unknown: %v", err)
	}
	p2 := goodProvider("corp")
	p2.BaseURL = "https://gitlab.example.com"
	p2.TokenRef = "other-token"
	if err := st.ReplaceProvider(p2); err != nil {
		t.Fatal(err)
	}
	// refused shapes never reach the file
	before, _ := os.ReadFile(dir + "/" + RegistryFileName)
	for _, mod := range []func(*Provider){
		func(p *Provider) { p.BaseURL = "http://127.0.0.1:8080" },
		func(p *Provider) { p.BaseURL = "http://169.254.169.254" },
		func(p *Provider) { p.BaseURL = "http://localhost" },
		func(p *Provider) { p.BaseURL = "http://[::ffff:127.0.0.1]" },
		func(p *Provider) { p.BaseURL = "http://0x7f.1" },
		func(p *Provider) { p.BaseURL = "https://x.example.com/api/v4" },
		func(p *Provider) { p.BaseURL = "ftp://x.example.com" },
		func(p *Provider) { p.SSHURL = "ssh://git@127.0.0.1" },
		func(p *Provider) { p.SSHURL = "ssh://git@x.example.com/grp" },
		func(p *Provider) { p.TokenRef = "Bad Ref" },
		func(p *Provider) { p.Kind = "bitbucket" },
		func(p *Provider) { p.ID = "Bad" },
	} {
		p := goodProvider("other")
		mod(&p)
		if err := st.AddProvider(p); err == nil {
			t.Errorf("accepted %+v", p)
		}
		if p.ID != "other" {
			continue
		}
		if err := st.ReplaceProvider(Provider{ID: "corp", Kind: p.Kind, BaseURL: p.BaseURL, SSHURL: p.SSHURL, TokenRef: p.TokenRef}); err == nil {
			t.Errorf("replace accepted %+v", p)
		}
	}
	if after, _ := os.ReadFile(dir + "/" + RegistryFileName); string(after) != string(before) {
		t.Fatal("a refused provider changed the registry file")
	}
	// persisted
	st2 := mustOpen(t, dir, LegacySource{})
	got, ok := st2.ProviderByID("corp")
	if !ok || got != p2 {
		t.Fatalf("reload: %+v %v", got, ok)
	}
	// delete refused while a project references it
	if err := st.Update(func(r *Registry) error {
		r.Projects = append(r.Projects, Project{Origin: OriginUnknown, ID: "demo", Path: "/home/orch/repos/demo", Kind: "registered", Provider: strp("corp"),
			Remote: &RemoteLink{Path: "team/demo"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProvider("corp"); !errors.Is(err, ErrProviderInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	if _, ok := st.ProviderByID("corp"); !ok {
		t.Fatal("provider gone after refused delete")
	}
	if err := st.DeleteProvider("none"); !errors.Is(err, ErrProviderUnknown) {
		t.Fatalf("delete unknown: %v", err)
	}
	st.Update(func(r *Registry) error { r.Projects = nil; return nil })
	if err := st.DeleteProvider("corp"); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustOpen(t, dir, LegacySource{}).ProviderByID("corp"); ok {
		t.Fatal("delete not persisted")
	}
	// the cap of 20 comes from registry validation
	for i := 0; i < maxProviders; i++ {
		if err := st.AddProvider(goodProvider("p" + strings.Repeat("a", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)))); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := st.AddProvider(goodProvider("overflow")); err == nil {
		t.Fatal("21st provider accepted")
	}
}

// All provider writes go through RegistryStore.Update: provider.go opens no
// file and has no write helper of its own.
func TestProviderStoreHasNoOwnWritePath(t *testing.T) {
	b, _ := os.ReadFile("provider.go")
	for _, bad := range []string{"os.WriteFile", "os.Create", "os.OpenFile", "writeAtomic", "os.Rename", "ioutil"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("provider.go uses %s", bad)
		}
	}
	if strings.Count(string(b), "s.Update(") != 2 {
		t.Errorf("expected exactly the two Update calls (put, delete)")
	}
}

func TestProviderStatusReadOnly(t *testing.T) {
	dir := stateDirFor(t)
	st := mustOpen(t, dir, LegacySource{})
	st.AddProvider(goodProvider("corp"))
	ts, err := OpenTokenStore(dir, regOpts())
	if err != nil {
		t.Fatal(err)
	}
	refuse, warns := ProviderStatus(dir, regOpts())
	if refuse != nil || len(warns) != 1 {
		t.Fatalf("missing token: %v %v", refuse, warns)
	}
	if err := ts.Set("gl-admin", "gitlab", "", apiCanary, tokNow); err != nil {
		t.Fatal(err)
	}
	refuse, warns = ProviderStatus(dir, regOpts())
	if refuse != nil || len(warns) != 0 {
		t.Fatalf("healthy: %v %v", refuse, warns)
	}
	ts.Delete("gl-admin")
	ts.Set("gl-admin", "github", "", apiCanary, tokNow)
	if _, warns = ProviderStatus(dir, regOpts()); len(warns) != 1 {
		t.Fatalf("wrong kind should warn: %v", warns)
	}
	for _, w := range warns {
		if strings.Contains(w, apiCanary) {
			t.Fatal("warning leaks token")
		}
	}
}
