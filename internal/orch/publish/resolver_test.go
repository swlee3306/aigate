package publish

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/clone"
	"aigate/internal/orch/policy"
)

const khKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// rfix has a recording fake git (so a push to an ssh remote can be observed
// without a server), an ssh key and a known_hosts file pinning gitlab.example.
type rfix struct {
	fixture
	gitBin, key, known, log string
}

func newRFix(t *testing.T) rfix {
	t.Helper()
	d, _ := filepath.EvalSymlinks(t.TempDir())
	r := rfix{fixture: newFixture(t), gitBin: filepath.Join(d, "fakegit"), key: filepath.Join(d, "key"), known: filepath.Join(d, "known_hosts"), log: filepath.Join(d, "git.log")}
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + r.log + "'; done\nprintf -- '--end--\\n' >> '" + r.log + "'\nexit 0\n"
	if err := os.WriteFile(r.gitBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r.key, []byte("k"), 0o600)
	os.WriteFile(r.known, []byte("gitlab.example "+khKey+"\n"), 0o644)
	return r
}

func (r rfix) pushes() []string {
	b, _ := os.ReadFile(r.log)
	return strings.Split(strings.TrimSpace(string(b)), "--end--")
}

func (r rfix) pushedTo(remote string) bool {
	for _, p := range r.pushes() {
		if strings.Contains(p, "\n"+remote+"\n") || strings.HasSuffix(strings.TrimSpace(p), "\n"+remote) {
			return true
		}
		if strings.Contains(p, remote) {
			return true
		}
	}
	return false
}

func (r rfix) pub(t *testing.T, c Config) *Publisher {
	t.Helper()
	c.Allow = policy.NewAllowlist(KindPushBranch)
	c.Git = r.gitBin
	if c.Remote == "" {
		c.Remote = r.bare
	}
	c.SSHKeyFile, c.SSHKnownHosts = r.key, r.known
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const resolved = "ssh://git@gitlab.example/team/app-one.git"

func (r rfix) push(p *Publisher, repo string) error {
	return p.Do(context.Background(), Action{Kind: KindPushBranch, Task: 3, Repo: repo, Head: r.head})
}

func TestPublishResolverOnlyForUnlistedRepos(t *testing.T) {
	a, b := newRFix(t), newRFix(t)
	var mu sync.Mutex
	var asked []string
	res := func(repo string) (string, string, bool) {
		mu.Lock()
		asked = append(asked, repo)
		mu.Unlock()
		return resolved, "", true
	}
	// b.repo has its own static remote (a local bare); a.repo is unlisted.
	p := a.pub(t, Config{RepoRemotes: map[string]string{b.repo: b.bare}, RemoteResolver: res})
	if err := a.push(p, b.repo); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Fatalf("the resolver was asked about a listed repo: %v", asked)
	}
	if !a.pushedTo(b.bare) {
		t.Fatalf("static remote not used: %v", a.pushes())
	}
	if err := a.push(p, a.repo); err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 || asked[0] != a.repo {
		t.Fatalf("resolver not asked about the unlisted repo: %v", asked)
	}
	for _, x := range asked {
		if x != a.repo {
			t.Fatalf("resolver asked about %q", x)
		}
	}
	if !a.pushedTo(resolved) {
		t.Fatalf("resolved remote not used: %v", a.pushes())
	}
	if a.pushedTo(a.bare) && !a.pushedTo(b.bare) {
		t.Fatal("the default remote was used for a resolved repo")
	}
}

func TestResolverRequiresPinnedHost(t *testing.T) {
	r := newRFix(t)
	for name, remote := range map[string]string{
		"unpinned host":    "ssh://git@evil.example/team/app-one.git",
		"unpinned port":    "ssh://git@gitlab.example:2222/team/app-one.git",
		"lookalike host":   "ssh://git@gitlab.example.evil/team/app-one.git",
		"local path":       r.bare,
		"https":            "https://gitlab.example/team/app-one.git",
		"file url":         "file://" + r.bare,
		"ext transport":    "ext::sh -c touch% /tmp/pwned",
		"option-like":      "--upload-pack=x",
		"wrong ssh user":   "ssh://root@gitlab.example/team/app-one.git",
		"trailing newline": resolved + "\n",
	} {
		os.Remove(r.log)
		p := r.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return remote, "", true }})
		if err := r.push(p, r.repo); err == nil {
			t.Errorf("%s: push was allowed", name)
		}
		if b, _ := os.ReadFile(r.log); len(b) != 0 {
			t.Errorf("%s: git ran: %s", name, b)
		}
	}
	// A pinned host is accepted.
	p := r.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return resolved, "", true }})
	if err := r.push(p, r.repo); err != nil {
		t.Fatalf("pinned host refused: %v", err)
	}
	// ("", true) is a refusal, not a fall-through to the default remote.
	os.Remove(r.log)
	p = r.pub(t, Config{RemoteResolver: func(string) (string, string, bool) { return "", "", true }})
	if err := r.push(p, r.repo); err == nil {
		t.Fatal("an unresolvable linked repo was pushed to the default remote")
	}
	if b, _ := os.ReadFile(r.log); len(b) != 0 {
		t.Fatalf("git ran: %s", b)
	}
}

func TestStaticRemotesUnchanged(t *testing.T) {
	r := newRFix(t)
	other := newRFix(t)
	// A resolver that says "not mine" leaves the old behaviour untouched: the
	// listed repo uses its remote, any other repo the default.
	p := r.pub(t, Config{RepoRemotes: map[string]string{other.repo: other.bare}, RemoteResolver: func(string) (string, string, bool) { return "", "", false }})
	if err := r.push(p, other.repo); err != nil || !r.pushedTo(other.bare) {
		t.Fatalf("listed: %v %v", err, r.pushes())
	}
	os.Remove(r.log)
	if err := r.push(p, r.repo); err != nil || !r.pushedTo(r.bare) {
		t.Fatalf("default: %v %v", err, r.pushes())
	}
	// No resolver at all: exactly the W2/W3 behaviour (the resolver code is not reached).
	os.Remove(r.log)
	q, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: r.bare, RepoRemotes: map[string]string{other.repo: other.bare}, Git: r.gitBin})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.push(q, r.repo); err != nil || !r.pushedTo(r.bare) {
		t.Fatalf("no resolver: %v %v", err, r.pushes())
	}
	if p, _ := q.remoteFor(other.repo); p != other.bare {
		t.Fatalf("remoteFor %q", p)
	}
}

// Validation at startup: a resolver may hand out ssh remotes, so it needs the
// ssh key and the pinning file exactly as a static ssh remote does.
func TestNewWithResolverNeedsSSHFiles(t *testing.T) {
	r := newRFix(t)
	res := func(string) (string, string, bool) { return "", "", false }
	al := policy.NewAllowlist(KindPushBranch)
	for name, c := range map[string]Config{
		"no ssh settings":   {Allow: al, Remote: r.bare, RemoteResolver: res},
		"no known_hosts":    {Allow: al, Remote: r.bare, SSHKeyFile: r.key, RemoteResolver: res},
		"no key":            {Allow: al, Remote: r.bare, SSHKnownHosts: r.known, RemoteResolver: res},
		"open key":          {Allow: al, Remote: r.bare, SSHKeyFile: r.known, SSHKnownHosts: r.known, RemoteResolver: res},
		"resolver w/o base": {Allow: al, SSHKeyFile: r.key, SSHKnownHosts: r.known, RemoteResolver: res},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := New(Config{Allow: al, Remote: r.bare, SSHKeyFile: r.key, SSHKnownHosts: r.known, RemoteResolver: res}); err != nil {
		t.Fatalf("local default remote with a resolver and ssh files: %v", err)
	}
	// Without a resolver the old rule stays: ssh settings on a local remote are refused.
	if _, err := New(Config{Allow: al, Remote: r.bare, SSHKeyFile: r.key, SSHKnownHosts: r.known}); err == nil {
		t.Fatal("ssh settings on a local remote accepted without a resolver")
	}
}

// With a resolver on a dead broker: a linked repo is never pushed anywhere by
// guessing. Without a resolver only the static remotes exist.
func TestPublishFailsClosedWhenBrokerDown(t *testing.T) {
	r := newRFix(t)
	res := &clone.Resolver{Client: &broker.Client{Socket: filepath.Join(t.TempDir(), "none.sock")}, KnownHosts: r.known}
	p := r.pub(t, Config{RemoteResolver: res.Resolve})
	if err := r.push(p, r.repo); err == nil {
		t.Fatal("pushed while the registry could not be read")
	}
	if b, _ := os.ReadFile(r.log); len(b) != 0 {
		t.Fatalf("git ran: %s", b)
	}
	// no resolver => static only, as before
	q, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: r.bare, Git: r.gitBin})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.push(q, r.repo); err != nil || !r.pushedTo(r.bare) {
		t.Fatalf("static: %v %v", err, r.pushes())
	}
}

// TestResolverNotWiredFromRequestData: RemoteResolver is named only in
// publish.go (the field, the startup check and remoteFor, which passes the
// repository path it was asked about) and assigned only in cmd/orchd/vm.go,
// from the registry-backed Resolve method. No request or task data reaches it.
func TestResolverNotWiredFromRequestData(t *testing.T) {
	root := "../../.."
	fset := token.NewFileSet()
	found := map[string]int{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		af, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "RemoteResolver" {
					found[rel]++
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && id.Name == "RemoteResolver" {
					found[rel]++
					// the value must be the registry-backed Resolve method value
					if se, ok := x.Value.(*ast.SelectorExpr); !ok || se.Sel.Name != "Resolve" {
						t.Errorf("%s: RemoteResolver set from something other than a Resolver's Resolve", rel)
					}
				}
			case *ast.Field:
				for _, nm := range x.Names {
					if nm.Name == "RemoteResolver" {
						found[rel]++
					}
				}
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if se, ok := l.(*ast.SelectorExpr); ok && se.Sel.Name == "RemoteResolver" && len(x.Rhs) == len(x.Lhs) {
						if rs, ok := x.Rhs[i].(*ast.SelectorExpr); !ok || rs.Sel.Name != "Resolve" {
							t.Errorf("%s: RemoteResolver assigned from something other than a Resolver's Resolve", rel)
						}
					}
				}
			case *ast.CallExpr:
				if se, ok := x.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "RemoteResolver" {
					if len(x.Args) != 1 {
						t.Errorf("%s: resolver called with %d args", rel, len(x.Args))
					} else if id, ok := x.Args[0].(*ast.Ident); !ok || id.Name != "repo" {
						t.Errorf("%s: resolver called with something other than the repo parameter", rel)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for file, n := range found {
		if file != "internal/orch/publish/publish.go" && file != "cmd/orchd/vm.go" {
			t.Errorf("RemoteResolver referenced in %s (%d times)", file, n)
		}
	}
	if found["internal/orch/publish/publish.go"] == 0 || found["cmd/orchd/vm.go"] == 0 {
		t.Fatalf("control: expected references in publish.go and vm.go, found %v", found)
	}
}
