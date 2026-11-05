package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
)

const secret = "glpat-SECRET-TOKEN-1234567890"

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type fixture struct {
	repo, bare, head, base, tokenFile string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\nb\n"), 0o644)
	git(t, repo, "commit", "-q", "-am", "work")
	head := git(t, repo, "rev-parse", "HEAD")
	bare, _ := filepath.EvalSymlinks(t.TempDir())
	git(t, bare, "init", "-q", "--bare")
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture{repo: repo, bare: bare, head: head, base: base, tokenFile: tf}
}

type fakeGitLab struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	tokens []string
	bodies []map[string]any
	status int
	echo   bool // answer with a body containing the token header
}

func newGitLab(t *testing.T) *fakeGitLab {
	f := &fakeGitLab{status: 201}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.EscapedPath())
		f.tokens = append(f.tokens, r.Header.Get("PRIVATE-TOKEN"))
		f.bodies = append(f.bodies, m)
		f.mu.Unlock()
		w.WriteHeader(f.status)
		if f.echo {
			io.WriteString(w, "bad token "+r.Header.Get("PRIVATE-TOKEN"))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []int64
}

func (n *fakeNotifier) Notify(_ context.Context, task int64, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, task)
	return nil
}

func refs(t *testing.T, bare string) string {
	return git(t, bare, "for-each-ref", "--format=%(refname) %(objectname)")
}

func TestAllowlistedActionRuns(t *testing.T) {
	fx := newFixture(t)
	gl := newGitLab(t)
	n := &fakeNotifier{}
	p, err := New(Config{
		Allow:  policy.NewAllowlist(KindPushBranch, KindDraftPR, KindNotify),
		Remote: fx.bare, TokenFile: fx.tokenFile, Notifier: n,
		GitLab: GitLab{BaseURL: gl.URL, Project: "grp/proj", TargetBranch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tk := store.Task{ID: 5, Repo: fx.repo, Base: fx.base, HeadSHA: fx.head}
	if err := p.Publish(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	// Exactly one ref arrived: refs/heads/orch/5 at the head commit.
	if got, want := refs(t, fx.bare), "refs/heads/orch/5 "+fx.head; got != want {
		t.Fatalf("remote refs %q, want %q", got, want)
	}
	// The draft MR: right project path, token only in the header, draft true.
	if len(gl.paths) != 1 || gl.paths[0] != "/api/v4/projects/grp%2Fproj/merge_requests" {
		t.Fatalf("requests %v", gl.paths)
	}
	if gl.tokens[0] != secret {
		t.Fatal("token header missing")
	}
	b := gl.bodies[0]
	if b["draft"] != true || b["source_branch"] != "orch/5" || b["target_branch"] != "main" || !strings.HasPrefix(b["title"].(string), "Draft:") {
		t.Fatalf("body %v", b)
	}
	if len(n.calls) != 1 || n.calls[0] != 5 {
		t.Fatalf("notify %v", n.calls)
	}
	// Pushing again is a no-op; an existing MR (409) is fine.
	gl.status = 409
	if err := p.Publish(context.Background(), tk); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	// The repository config never saw the token or a credentialed URL.
	cfg, _ := os.ReadFile(filepath.Join(fx.repo, ".git", "config"))
	if bytes.Contains(cfg, []byte(secret)) {
		t.Fatal("token in .git/config")
	}
}

func TestUnlistedActionGoesToApproval(t *testing.T) {
	fx := newFixture(t)
	gl := newGitLab(t)
	p, err := New(Config{
		Allow:  policy.NewAllowlist(KindPushBranch), // draft_pr is NOT listed
		Remote: fx.bare, TokenFile: fx.tokenFile,
		GitLab: GitLab{BaseURL: gl.URL, Project: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := Action{Kind: KindDraftPR, Task: 7, Repo: fx.repo, Base: fx.base, Head: fx.head}
	err = p.Do(context.Background(), a)
	var na *NeedsApprovalError
	if !errors.Is(err, ErrNeedsApproval) || !errors.As(err, &na) {
		t.Fatalf("want ErrNeedsApproval, got %v", err)
	}
	if na.Hash != approvals.PublishHash(7, fx.head, KindDraftPR) || na.Action.Kind != KindDraftPR {
		t.Fatalf("approval info %+v", na)
	}
	if len(gl.paths) != 0 {
		t.Fatalf("GitLab was called: %v", gl.paths)
	}
	// Through Publish: the plan parks at draft_pr BEFORE the listed push runs
	// (no partial publication).
	err = p.Publish(context.Background(), store.Task{ID: 7, Repo: fx.repo, Base: fx.base, HeadSHA: fx.head})
	if !errors.Is(err, ErrNeedsApproval) {
		t.Fatalf("Publish: %v", err)
	}
	if got := refs(t, fx.bare); got != "" {
		t.Fatalf("push ran although the plan cannot complete: %q", got)
	}
	if len(gl.paths) != 0 {
		t.Fatal("GitLab was called")
	}
	// An unknown kind is not in the floor but not allowed by default either.
	if err := p.Do(context.Background(), Action{Kind: "post_comment", Task: 7}); !errors.Is(err, ErrNeedsApproval) {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestFloorBeatsAllowlistInPublish(t *testing.T) {
	fx := newFixture(t)
	kinds := []string{"merge", "force_push", "delete_branch", "deploy", "modify_account"}
	p, err := New(Config{Allow: policy.NewAllowlist(append(kinds, KindPushBranch)...), Remote: fx.bare})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds {
		err := p.Do(context.Background(), Action{Kind: k, Task: 1, Repo: fx.repo, Head: fx.head})
		if !errors.Is(err, policy.ErrFloor) || errors.Is(err, ErrNeedsApproval) {
			t.Errorf("%s: %v", k, err)
		}
	}
	if refs(t, fx.bare) != "" {
		t.Fatal("something was pushed")
	}
}

func TestPushOnlyTaskBranch(t *testing.T) {
	fx := newFixture(t)
	p, err := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: fx.bare})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ref := range []string{"refs/heads/main", "refs/heads/orch/*", "refs/heads/orch/6", "+refs/heads/orch/5", "orch/5",
		"refs/heads/orch/5:refs/heads/main", "refs/heads/orch/5 refs/heads/main", "refs/tags/v1", "refs/heads/orch/../main"} {
		err := p.Do(ctx, Action{Kind: KindPushBranch, Task: 5, Repo: fx.repo, Head: fx.head, Ref: ref})
		if !errors.Is(err, ErrBadRef) {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	for _, head := range []string{"", "main", "HEAD", "--force", fx.head + " main", strings.ToUpper(fx.head), fx.head[:10]} {
		if err := p.Do(ctx, Action{Kind: KindPushBranch, Task: 5, Repo: fx.repo, Head: head}); err == nil {
			t.Errorf("head %q accepted", head)
		}
	}
	if err := p.Do(ctx, Action{Kind: KindPushBranch, Task: 0, Repo: fx.repo, Head: fx.head}); err == nil {
		t.Error("task 0 accepted")
	}
	if got := refs(t, fx.bare); got != "" {
		t.Fatalf("remote changed: %q", got)
	}
	// The one legal push, with its exact ref spelled out, works.
	if err := p.Do(ctx, Action{Kind: KindPushBranch, Task: 5, Repo: fx.repo, Head: fx.head, Ref: RefFor(5)}); err != nil {
		t.Fatal(err)
	}
	if got := refs(t, fx.bare); got != "refs/heads/orch/5 "+fx.head {
		t.Fatalf("refs %q", got)
	}
	// A diverged branch is never forced.
	git(t, fx.repo, "reset", "-q", "--hard", fx.base)
	os.WriteFile(filepath.Join(fx.repo, "a.txt"), []byte("other\n"), 0o644)
	git(t, fx.repo, "commit", "-q", "-am", "other")
	other := git(t, fx.repo, "rev-parse", "HEAD")
	if err := p.Do(ctx, Action{Kind: KindPushBranch, Task: 5, Repo: fx.repo, Head: other}); err == nil {
		t.Fatal("non-fast-forward push succeeded")
	}
	if got := refs(t, fx.bare); got != "refs/heads/orch/5 "+fx.head {
		t.Fatalf("remote was overwritten: %q", got)
	}
}

func TestPushRunsNoHooks(t *testing.T) {
	fx := newFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
	os.WriteFile(filepath.Join(fx.repo, ".git", "hooks", "pre-push"), []byte(hook), 0o755)
	// A repo-local config that would also run something must not matter.
	git(t, fx.repo, "config", "core.hooksPath", filepath.Join(fx.repo, ".git", "hooks"))
	p, _ := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: fx.bare})
	if err := p.Do(context.Background(), Action{Kind: KindPushBranch, Task: 2, Repo: fx.repo, Head: fx.head}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("pre-push hook ran")
	}
}

func TestHumanApprovedActionRunsOnceForThatHead(t *testing.T) {
	fx := newFixture(t)
	home := t.TempDir()
	p, err := New(Config{Allow: policy.NewAllowlist(), Remote: fx.bare, Grants: approvals.Grants{Home: home}})
	if err != nil {
		t.Fatal(err)
	}
	a := Action{Kind: KindPushBranch, Task: 3, Repo: fx.repo, Base: fx.base, Head: fx.head}
	if err := p.Do(context.Background(), a); !errors.Is(err, ErrNeedsApproval) {
		t.Fatalf("before grant: %v", err)
	}
	rid, _, _ := gate.RepoID(fx.repo)
	st := approval.Store{Path: gate.ApprovalsPath(home)}
	// A grant for another head does not cover it.
	if err := st.Grant(rid, fx.base, approvals.PublishHash(3, fx.base, KindPushBranch), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := p.Do(context.Background(), a); !errors.Is(err, ErrNeedsApproval) {
		t.Fatalf("wrong-head grant: %v", err)
	}
	if err := st.Grant(rid, fx.base, approvals.PublishHash(3, fx.head, KindPushBranch), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := p.Do(context.Background(), a); err != nil {
		t.Fatalf("with grant: %v", err)
	}
	// The floor still wins over a grant.
	if err := st.Grant(rid, fx.base, approvals.PublishHash(3, fx.head, "merge"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := p.Do(context.Background(), Action{Kind: "merge", Task: 3, Repo: fx.repo, Base: fx.base, Head: fx.head}); !errors.Is(err, policy.ErrFloor) {
		t.Fatalf("floor vs grant: %v", err)
	}
}

func TestTokenFileMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	os.WriteFile(f, []byte(secret), 0o600)
	if _, err := ReadToken(f); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666} {
		os.Chmod(f, mode)
		_, err := ReadToken(f)
		if err == nil {
			t.Errorf("mode %04o accepted", mode)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks token: %v", err)
		}
	}
	os.Chmod(f, 0o600)
	link := filepath.Join(dir, "link")
	os.Symlink(f, link)
	if _, err := ReadToken(link); err == nil {
		t.Error("symlink accepted")
	}
	os.WriteFile(f, []byte("two words"), 0o600)
	if _, err := ReadToken(f); err == nil {
		t.Error("token with whitespace accepted")
	}
	os.WriteFile(f, []byte("  \n"), 0o600)
	if _, err := ReadToken(f); err == nil {
		t.Error("empty token accepted")
	}
	if _, err := ReadToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
	// New refuses a loose token file up front.
	os.WriteFile(f, []byte(secret), 0o600)
	os.Chmod(f, 0o644)
	fx := newFixture(t)
	gl := newGitLab(t)
	if _, err := New(Config{Remote: fx.bare, TokenFile: f, GitLab: GitLab{BaseURL: gl.URL, Project: "1"}}); err == nil {
		t.Error("New accepted a world-readable token file")
	}
}

func TestConfigValidation(t *testing.T) {
	fx := newFixture(t)
	for name, c := range map[string]Config{
		"nothing":        {},
		"remote creds":   {Remote: "https://user:pw@example.com/r.git", TokenFile: fx.tokenFile},
		"remote http":    {Remote: "http://example.com/r.git"},
		"remote ssh":     {Remote: "ssh://git@example.com/r.git"},
		"remote ext":     {Remote: "ext::sh -c id"},
		"relative path":  {Remote: "repo.git"},
		"https no token": {Remote: "https://example.com/r.git"},
		"gitlab http":    {GitLab: GitLab{BaseURL: "http://gitlab.example.com", Project: "1"}, TokenFile: fx.tokenFile},
		"gitlab creds":   {GitLab: GitLab{BaseURL: "https://u:p@gitlab.example.com", Project: "1"}, TokenFile: fx.tokenFile},
		"gitlab no proj": {GitLab: GitLab{BaseURL: "https://gitlab.example.com"}, TokenFile: fx.tokenFile},
		"gitlab no tok":  {GitLab: GitLab{BaseURL: "https://gitlab.example.com", Project: "1"}},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAskpassHelperHoldsNoToken(t *testing.T) {
	dir := t.TempDir()
	ask, err := writeAskpass(dir, "/tmp/some token path")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ask)
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("token in askpass script")
	}
	fi, _ := os.Stat(ask)
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("askpass mode %04o", fi.Mode().Perm())
	}
	if _, err := writeAskpass(dir, "/tmp/it's"); err == nil {
		t.Fatal("quote in token path accepted")
	}
	fx := newFixture(t)
	ask, _ = writeAskpass(dir, fx.tokenFile)
	user, _ := exec.Command(ask, "Username for 'https://x': ").Output()
	pass, _ := exec.Command(ask, "Password for 'https://x': ").Output()
	if string(user) != "oauth2\n" || string(pass) != secret {
		t.Fatalf("askpass user %q pass-ok=%v", user, string(pass) == secret)
	}
}

// The token never shows up in errors, the log, or what git prints.
func TestTokenNeverInLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	fx := newFixture(t)
	gl := newGitLab(t)
	gl.status, gl.echo = 500, true // the server even echoes the token back
	p, err := New(Config{
		Allow:  policy.NewAllowlist(KindPushBranch, KindDraftPR),
		Remote: fx.bare, TokenFile: fx.tokenFile,
		GitLab: GitLab{BaseURL: gl.URL, Project: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	collect := func(err error) {
		if err != nil {
			all = append(all, err.Error())
		}
	}
	tk := store.Task{ID: 9, Repo: fx.repo, Base: fx.base, HeadSHA: fx.head}
	collect(p.Publish(context.Background(), tk)) // GitLab 500
	gl.Close()
	collect(p.Do(context.Background(), Action{Kind: KindDraftPR, Task: 9, Repo: fx.repo, Head: fx.head})) // connection refused
	// A failing git push (remote that is not a repository).
	bad, _ := New(Config{Allow: policy.NewAllowlist(KindPushBranch), Remote: filepath.Join(t.TempDir(), "nope"), TokenFile: fx.tokenFile})
	collect(bad.Do(context.Background(), Action{Kind: KindPushBranch, Task: 9, Repo: fx.repo, Head: fx.head}))
	if len(all) != 3 {
		t.Fatalf("expected 3 errors, got %q", all)
	}
	for _, s := range append(all, logs.String(), p.PublishNote()) {
		if strings.Contains(s, secret) || strings.Contains(s, "SECRET-TOKEN") {
			t.Fatalf("token leaked: %q", s)
		}
	}
}
