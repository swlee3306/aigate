package publish

import (
	"context"
	"path/filepath"
	"testing"

	"aigate/internal/orch/policy"
)

// A task of a repository with its own remote pushes there; any other
// repository still pushes to the default remote.
func TestPushUsesPerRepoRemote(t *testing.T) {
	a, b := newFixture(t), newFixture(t)
	p, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: a.bare, RepoRemotes: map[string]string{b.repo: b.bare}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.Do(ctx, Action{Kind: KindPushBranch, Task: 1, Repo: b.repo, Head: b.head}); err != nil {
		t.Fatal(err)
	}
	if refs(t, a.bare) != "" || refs(t, b.bare) != "refs/heads/orch/1 "+b.head {
		t.Fatalf("override repo: default=%q override=%q", refs(t, a.bare), refs(t, b.bare))
	}
	if err := p.Do(ctx, Action{Kind: KindPushBranch, Task: 2, Repo: a.repo, Head: a.head}); err != nil {
		t.Fatal(err)
	}
	if refs(t, a.bare) != "refs/heads/orch/2 "+a.head || refs(t, b.bare) != "refs/heads/orch/1 "+b.head {
		t.Fatalf("default repo: default=%q override=%q", refs(t, a.bare), refs(t, b.bare))
	}
}

func TestPerRepoRemoteValidation(t *testing.T) {
	fx := newFixture(t)
	al := policy.NewAllowlist(KindPushBranch)
	for name, c := range map[string]Config{
		"no default":    {Allow: al, RepoRemotes: map[string]string{fx.repo: fx.bare}},
		"relative repo": {Allow: al, Remote: fx.bare, RepoRemotes: map[string]string{"repo": fx.bare}},
		"unclean repo":  {Allow: al, Remote: fx.bare, RepoRemotes: map[string]string{fx.repo + "/../x": fx.bare}},
		"bad remote":    {Allow: al, Remote: fx.bare, RepoRemotes: map[string]string{fx.repo: "ftp://x/y"}},
		"ssh no files":  {Allow: al, Remote: fx.bare, RepoRemotes: map[string]string{fx.repo: "ssh://git@h/g/p.git"}},
		"https no tok":  {Allow: al, Remote: fx.bare, RepoRemotes: map[string]string{filepath.Join(fx.repo, "x"): "https://h/g/p.git"}},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
