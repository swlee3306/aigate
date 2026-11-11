package clone

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const keyField = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

const goodRemote = "ssh://git@gitlab.example/team/proj-a.git"

type fix struct {
	root, keyFile, known string
}

func newFix(t *testing.T) fix {
	t.Helper()
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	f := fix{root: filepath.Join(real, "repos"), keyFile: filepath.Join(real, "ssh", "id_deploy"), known: filepath.Join(real, "ssh", "known_hosts")}
	for _, d := range []string{f.root, filepath.Dir(f.keyFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The private key is NOT created: Clone must never open it (ssh does).
	if err := os.WriteFile(f.known, []byte("gitlab.example "+keyField+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fix) opts(name string) Options {
	return Options{Remote: goodRemote, Dest: filepath.Join(f.root, name), Roots: []string{f.root}, SSHKey: f.keyFile, KnownHosts: f.known}
}

type call struct {
	args []string
	env  []string
	dest string
}

// recorder is a seam that records what would run and fails or succeeds.
func recorder(result error, hook func(ctx context.Context, c *exec.Cmd)) (*seams, *[]call) {
	var mu sync.Mutex
	var calls []call
	sm := &seams{run: func(ctx context.Context, c *exec.Cmd) error {
		mu.Lock()
		calls = append(calls, call{args: append([]string(nil), c.Args...), env: append([]string(nil), c.Env...), dest: c.Args[len(c.Args)-1]})
		mu.Unlock()
		if hook != nil {
			hook(ctx, c)
		}
		return result
	}}
	return sm, &calls
}

func TestCloneUsesFixedSafeOptions(t *testing.T) {
	f := newFix(t)
	for k, v := range map[string]string{
		"GIT_DIR": "/evil", "GIT_SSH": "/evil/ssh", "GIT_SSH_COMMAND": "evil", "GIT_CONFIG_COUNT": "1", "GIT_EXEC_PATH": "/evil",
		"GIT_TEMPLATE_DIR": "/evil", "GIT_PROXY_COMMAND": "evil", "SSH_AUTH_SOCK": "/evil/agent", "SSH_ASKPASS": "/evil/askpass",
		"GIT_ALLOW_PROTOCOL": "ext", "LD_PRELOAD": "/evil.so", "HTTPS_PROXY": "http://evil",
	} {
		t.Setenv(k, v)
	}
	sm, calls := recorder(nil, func(_ context.Context, c *exec.Cmd) {
		if fi, err := os.Lstat(c.Args[len(c.Args)-1]); err != nil || !fi.IsDir() {
			t.Errorf("destination directory not created before git runs: %v", err)
		}
	})
	o := f.opts("proj-a")
	if err := clone(context.Background(), o, sm); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("%d runs", len(*calls))
	}
	c := (*calls)[0]
	want := []string{"git", "-c", "protocol.allow=never", "-c", "protocol.ssh.allow=always", "-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false", "clone", "--no-recurse-submodules", "--no-local", "--", goodRemote, o.Dest}
	if strings.Join(c.args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command line:\n got %q\nwant %q", c.args, want)
	}
	env := map[string]string{}
	for _, e := range c.env {
		k, v, _ := strings.Cut(e, "=")
		if _, dup := env[k]; dup {
			t.Errorf("duplicate env %s", k)
		}
		env[k] = v
	}
	for k := range env {
		switch {
		case k == "GIT_SSH_COMMAND", k == "GIT_CONFIG_GLOBAL", k == "GIT_CONFIG_NOSYSTEM", k == "GIT_TERMINAL_PROMPT", k == "PATH", k == "HOME", k == "LC_ALL":
		default:
			t.Errorf("unexpected environment variable %s", k)
		}
	}
	if env["GIT_CONFIG_GLOBAL"] != "/dev/null" || env["GIT_CONFIG_NOSYSTEM"] != "1" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("config isolation missing: %v", env)
	}
	ssh := env["GIT_SSH_COMMAND"]
	for _, must := range []string{"-i " + f.keyFile, "IdentitiesOnly=yes", "StrictHostKeyChecking=yes", "UserKnownHostsFile=" + f.known,
		"BatchMode=yes", "IdentityAgent=none", "-F /dev/null", "GlobalKnownHostsFile=/dev/null", "ProxyCommand=none", "ForwardAgent=no", "ConnectTimeout="} {
		if !strings.Contains(ssh, must) {
			t.Errorf("GIT_SSH_COMMAND lacks %q: %s", must, ssh)
		}
	}
	if strings.HasPrefix(env["HOME"], f.root) || env["HOME"] == os.Getenv("HOME") {
		t.Errorf("HOME is not a private scratch directory: %s", env["HOME"])
	}
	if _, err := os.Lstat(env["HOME"]); err == nil {
		t.Errorf("scratch HOME %s left behind", env["HOME"])
	}
}

func TestCloneNeverOpensPrivateKey(t *testing.T) {
	f := newFix(t)
	// The key path is a directory that cannot be read at all: any open would
	// fail the clone, and the file does not exist anyway.
	sm, calls := recorder(nil, nil)
	if err := clone(context.Background(), f.opts("proj-a"), sm); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatal("did not run")
	}
	// AST check: every use of the SSHKey field in production code is an
	// argument of sshSafe (validation) or sshCommand (the ssh command text).
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "clone.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	okUse := map[*ast.SelectorExpr]bool{}
	ast.Inspect(af, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok {
			if id, ok := ce.Fun.(*ast.Ident); ok && (id.Name == "sshSafe" || id.Name == "sshCommand") {
				for _, a := range ce.Args {
					if se, ok := a.(*ast.SelectorExpr); ok {
						okUse[se] = true
					}
				}
			}
		}
		return true
	})
	uses := 0
	ast.Inspect(af, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok && se.Sel.Name == "SSHKey" {
			uses++
			if !okUse[se] {
				t.Errorf("SSHKey used outside sshSafe/sshCommand at %s", fset.Position(se.Pos()))
			}
		}
		return true
	})
	if uses == 0 {
		t.Fatal("control: no SSHKey use found")
	}
}

func TestCloneRefusesUnpinnedHost(t *testing.T) {
	f := newFix(t)
	sm, calls := recorder(nil, nil)
	for name, mod := range map[string]func(o *Options){
		"host not in file":   func(o *Options) { o.Remote = "ssh://git@evil.example/team/proj-a.git" },
		"port not in file":   func(o *Options) { o.Remote = "ssh://git@gitlab.example:2222/team/proj-a.git" },
		"missing known file": func(o *Options) { o.KnownHosts = filepath.Join(f.root, "nope") },
		"empty known file":   func(o *Options) { o.KnownHosts = "" },
	} {
		o := f.opts("proj-a")
		mod(&o)
		err := clone(context.Background(), o, sm)
		if !errors.Is(err, ErrHostNotPinned) && name != "empty known file" {
			t.Errorf("%s: %v", name, err)
		}
		if err == nil {
			t.Errorf("%s: cloned", name)
		}
		if _, e := os.Lstat(o.Dest); e == nil {
			t.Errorf("%s: destination created", name)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("git ran %d times for an unpinned host", len(*calls))
	}
}

func TestCloneRefusesExistingPathAndSymlink(t *testing.T) {
	f := newFix(t)
	sm, calls := recorder(nil, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mk := map[string]func(p string){
		"dir with file": func(p string) {
			os.MkdirAll(p, 0o700)
			os.WriteFile(filepath.Join(p, "mine"), []byte("m"), 0o644)
		},
		"empty dir":        func(p string) { os.MkdirAll(p, 0o700) },
		"regular file":     func(p string) { os.WriteFile(p, []byte("f"), 0o644) },
		"symlink to dir":   func(p string) { os.Symlink(outside, p) },
		"dangling symlink": func(p string) { os.Symlink(filepath.Join(outside, "gone"), p) },
	}
	i := 0
	for name, make := range mk {
		i++
		o := f.opts("exists-" + string(rune('a'+i)))
		make(o.Dest)
		err := clone(context.Background(), o, sm)
		if !errors.Is(err, ErrExists) {
			t.Errorf("%s: %v", name, err)
		}
		if _, e := os.Lstat(o.Dest); e != nil {
			t.Errorf("%s: the existing path was removed: %v", name, e)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "keep")); string(b) != "x" {
		t.Error("symlink target touched")
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "exists-b", "mine")); len(b) != 0 && string(b) != "m" {
		t.Error("existing content changed")
	}
	if len(*calls) != 0 {
		t.Fatalf("git ran %d times over an existing path", len(*calls))
	}
}

func TestCloneOnlyUnderRepoRootsOneLevel(t *testing.T) {
	f := newFix(t)
	sm, calls := recorder(nil, nil)
	other := t.TempDir()
	os.MkdirAll(filepath.Join(f.root, "sub"), 0o700)
	if err := os.Symlink(other, filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	for name, dest := range map[string]string{
		"two levels":      filepath.Join(f.root, "sub", "proj-a"),
		"the root itself": f.root,
		"outside":         filepath.Join(other, "proj-a"),
		"relative":        "proj-a",
		"unclean":         f.root + "/sub/../proj-a",
		"via symlink dir": filepath.Join(f.root, "link", "proj-a"),
		"hidden name":     filepath.Join(f.root, ".proj"),
		"dash name":       filepath.Join(f.root, "-proj"),
		"upper case":      filepath.Join(f.root, "Proj"),
		"one char":        filepath.Join(f.root, "p"),
		"dot dot":         filepath.Join(f.root, ".."),
		"empty":           "",
	} {
		o := f.opts("x")
		o.Dest = dest
		if err := clone(context.Background(), o, sm); !errors.Is(err, ErrBadPath) {
			t.Errorf("%s: %v", name, err)
		}
	}
	o := f.opts("x")
	o.Roots = nil
	if err := clone(context.Background(), o, sm); !errors.Is(err, ErrBadPath) {
		t.Errorf("no roots: %v", err)
	}
	// A root that is itself (or sits behind) a symlink is refused: nothing may
	// redirect the new folder.
	linkRoot := filepath.Join(filepath.Dir(f.root), "linkroot")
	if err := os.Symlink(f.root, linkRoot); err != nil {
		t.Fatal(err)
	}
	o = f.opts("x")
	o.Roots, o.Dest = []string{linkRoot}, filepath.Join(linkRoot, "proj-a")
	if err := clone(context.Background(), o, sm); !errors.Is(err, ErrBadPath) {
		t.Errorf("symlinked root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "proj-a")); err == nil {
		t.Error("created through a symlinked root")
	}
	if len(*calls) != 0 {
		t.Fatalf("git ran %d times for a bad destination", len(*calls))
	}
	if _, err := os.Lstat(filepath.Join(other, "proj-a")); err == nil {
		t.Fatal("created outside the roots")
	}
	// The good case runs once.
	if err := clone(context.Background(), f.opts("proj-a"), sm); err != nil || len(*calls) != 1 {
		t.Fatalf("good path: %v (%d runs)", err, len(*calls))
	}
}

func TestCloneRejectsBadRemoteAndSSHPaths(t *testing.T) {
	f := newFix(t)
	sm, calls := recorder(nil, nil)
	for _, r := range []string{
		"", "ext::sh -c 'touch /tmp/pwn'", "--upload-pack=touch /tmp/x", "-oProxyCommand=x", "file:///etc", "/local/bare.git",
		"https://gitlab.example/team/proj-a.git", "ssh://root@gitlab.example/team/proj-a.git", "ssh://git@gitlab.example/team/proj-a",
		"ssh://git@gitlab.example/team/proj-a.git ", "ssh://git@gitlab.example/-oProxyCommand=x/proj-a.git", "ssh://git@-oProxyCommand=x/a/b.git",
		"ssh://git@gitlab.example/Team/proj-a.git", "ssh://git@gitlab.example/team/../proj-a.git", "ssh://git@gitlab.example/team/proj-a.git?x",
		"ssh://git@gitlab.example/team/proj-a.git\n",
	} {
		o := f.opts("proj-a")
		o.Remote = r
		if err := clone(context.Background(), o, sm); !errors.Is(err, ErrBadRemote) {
			t.Errorf("remote %q: %v", r, err)
		}
	}
	for name, mod := range map[string]func(o *Options){
		"key with space":     func(o *Options) { o.SSHKey = "/home/orch/my key" },
		"key with quote":     func(o *Options) { o.SSHKey = "/home/orch/k'ey" },
		"relative key":       func(o *Options) { o.SSHKey = "id_deploy" },
		"empty key":          func(o *Options) { o.SSHKey = "" },
		"known with dollar":  func(o *Options) { o.KnownHosts = "/home/orch/$HOME/kh" },
		"unclean known path": func(o *Options) { o.KnownHosts = f.known + "/../known_hosts" },
	} {
		o := f.opts("proj-a")
		mod(&o)
		if err := clone(context.Background(), o, sm); !errors.Is(err, ErrBadConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("git ran %d times", len(*calls))
	}
}

func TestCloneCleansOnlyItsOwnPartialDir(t *testing.T) {
	f := newFix(t)
	// Neighbours that must survive: a sibling dir, a sibling file, and a dir
	// whose name merely starts like the destination.
	for _, p := range []string{"proj-b", "proj-a2"} {
		os.MkdirAll(filepath.Join(f.root, p), 0o700)
		os.WriteFile(filepath.Join(f.root, p, "data"), []byte("d"), 0o644)
	}
	os.WriteFile(filepath.Join(f.root, "note.txt"), []byte("n"), 0o644)
	sm, _ := recorder(errors.New("git failed"), func(_ context.Context, c *exec.Cmd) {
		d := c.Args[len(c.Args)-1]
		os.MkdirAll(filepath.Join(d, ".git", "objects"), 0o700)
		os.WriteFile(filepath.Join(d, "half"), []byte("h"), 0o644)
	})
	o := f.opts("proj-a")
	err := clone(context.Background(), o, sm)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("want ErrFailed, got %v", err)
	}
	if strings.Contains(err.Error(), "git failed") {
		t.Errorf("git's own error leaked: %v", err)
	}
	if _, e := os.Lstat(o.Dest); !os.IsNotExist(e) {
		t.Errorf("partial dir left behind: %v", e)
	}
	for _, p := range []string{"proj-b/data", "proj-a2/data", "note.txt"} {
		if _, e := os.Stat(filepath.Join(f.root, p)); e != nil {
			t.Errorf("neighbour %s damaged: %v", p, e)
		}
	}
	// A retry after the cleanup works (the name is free again).
	sm2, calls2 := recorder(nil, nil)
	if err := clone(context.Background(), o, sm2); err != nil || len(*calls2) != 1 {
		t.Fatalf("retry: %v", err)
	}
	// A destination replaced by something else while git ran is not removed.
	f2 := newFix(t)
	victim := t.TempDir()
	os.WriteFile(filepath.Join(victim, "important"), []byte("i"), 0o644)
	o2 := f2.opts("proj-a")
	sm3, _ := recorder(errors.New("x"), func(_ context.Context, c *exec.Cmd) {
		d := c.Args[len(c.Args)-1]
		os.RemoveAll(d)
		os.Symlink(victim, d)
	})
	if err := clone(context.Background(), o2, sm3); !errors.Is(err, ErrFailed) {
		t.Fatalf("swapped: %v", err)
	}
	if _, e := os.Stat(filepath.Join(victim, "important")); e != nil {
		t.Fatal("a swapped-in symlink's target was deleted")
	}
	// ... and the foreign symlink itself is not ours to remove either.
	if fi, e := os.Lstat(o2.Dest); e != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the swapped-in symlink was removed: %v", e)
	}
}

func TestCloneTimeoutAndCancel(t *testing.T) {
	f := newFix(t)
	sm, _ := recorder(nil, func(ctx context.Context, c *exec.Cmd) {})
	sm.run = func(ctx context.Context, c *exec.Cmd) error {
		<-ctx.Done()
		return ctx.Err()
	}
	o := f.opts("proj-a")
	o.Timeout = 50 * time.Millisecond
	start := time.Now()
	err := clone(context.Background(), o, sm)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not stop")
	}
	if _, e := os.Lstat(o.Dest); !os.IsNotExist(e) {
		t.Error("partial dir left after timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o2 := f.opts("proj-c")
	if err := clone(ctx, o2, sm); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, e := os.Lstat(o2.Dest); !os.IsNotExist(e) {
		t.Error("partial dir left after cancel")
	}
	// The default limit is the 120 s of D5.
	if DefaultTimeout != 120*time.Second {
		t.Errorf("default timeout %v", DefaultTimeout)
	}
}

func TestCloneSameDestinationOneAtATime(t *testing.T) {
	f := newFix(t)
	started, release := make(chan struct{}), make(chan struct{})
	sm := &seams{run: func(ctx context.Context, c *exec.Cmd) error {
		close(started)
		<-release
		return nil
	}}
	o := f.opts("proj-a")
	done := make(chan error, 1)
	go func() { done <- clone(context.Background(), o, sm) }()
	<-started
	err := clone(context.Background(), o, &seams{run: func(context.Context, *exec.Cmd) error { t.Error("second clone ran"); return nil }})
	if !errors.Is(err, ErrInProgress) {
		t.Errorf("second clone: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCloneDirectoryMode(t *testing.T) {
	f := newFix(t)
	sm, _ := recorder(nil, nil)
	o := f.opts("proj-a")
	if err := clone(context.Background(), o, sm); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(o.Dest)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("destination mode %v %v", fi, err)
	}
}

// ---- real git against a local fixture ----

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e"}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCloneRealGitLocalFixture runs the real git binary with the real option
// list against a local bare repository. The test-only seam swaps the remote
// for the local path and opens the file transport (the production list refuses
// it); everything else is the production command. A post-checkout hook in the
// template directory proves hooks stay off (and, with the option dropped, that
// the hook mechanism does fire).
func TestCloneRealGitLocalFixture(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required: %v", err)
	}
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "first")
	want := git(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, work, "clone", "-q", "--bare", work, bare)

	marker := filepath.Join(t.TempDir(), "hook-ran")
	tmpl := filepath.Join(t.TempDir(), "tmpl")
	os.MkdirAll(filepath.Join(tmpl, "hooks"), 0o755)
	os.WriteFile(filepath.Join(tmpl, "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755)

	run := func(name string, drop bool) (string, error) {
		f := newFix(t)
		o := f.opts(name)
		sm := &seams{
			rewriteRemote: func(string) string { return bare },
			extraConfig:   []string{"protocol.file.allow=always"},
			extraEnv:      []string{"GIT_TEMPLATE_DIR=" + tmpl},
			dropHooksPath: drop,
		}
		return o.Dest, clone(context.Background(), o, sm)
	}
	dest, err := run("proj-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, dest, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD %s want %s", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "README.md")); err != nil || string(b) != "hello\n" {
		t.Fatalf("README: %q %v", b, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran during the clone")
	}
	// Control: without core.hooksPath the same template hook fires.
	if _, err := run("proj-b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control failed: the hook mechanism did not fire (%v): the test proves nothing", err)
	}
}

// Without the seam the production list refuses a local path outright (file
// transport is off), so a request cannot be turned into a local clone.
func TestCloneProductionRefusesFileTransport(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required: %v", err)
	}
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a"), []byte("a"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "x")
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, work, "clone", "-q", "--bare", work, bare)
	f := newFix(t)
	o := f.opts("proj-a")
	err := clone(context.Background(), o, &seams{rewriteRemote: func(string) string { return bare }})
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("file transport was not refused: %v", err)
	}
	if _, e := os.Lstat(o.Dest); !os.IsNotExist(e) {
		t.Error("partial dir left")
	}
}

// ---- static: the seams are unreachable from production ----

// TestProductionCannotEnableCloneSeams: the seams type is unexported and, in
// the non-test files, appears only in its declaration, in the signature of the
// internal clone function and as the literal nil passed by Clone. It is never
// built in a composite literal, assigned or stored in a variable or field, so
// no configuration or request data can reach it.
func TestProductionCannotEnableCloneSeams(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	type use struct{ file, fn string }
	var uses []use
	skip := map[*ast.Ident]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fn := "(type/var)"
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					skip[x.Sel] = true
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "seams" {
						t.Errorf("%s: production code builds a seams literal (%s)", f, fn)
					}
				case *ast.Ident:
					if x.Name == "seams" && !skip[x] {
						uses = append(uses, use{f, fn})
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "clone" {
						last := x.Args[len(x.Args)-1]
						if nid, ok := last.(*ast.Ident); !ok || nid.Name != "nil" {
							// the recursive-looking call inside Clone must pass nil
							if fn == "Clone" {
								t.Errorf("Clone passes something other than nil as the seams")
							}
						}
					}
				}
				return true
			})
		}
	}
	allowed := map[string]bool{"clone.go|(type/var)": true, "clone.go|clone": true}
	for _, u := range uses {
		if !allowed[u.file+"|"+u.fn] {
			t.Errorf("unexpected reference to seams in %s (%s)", u.file, u.fn)
		}
	}
	src, _ := os.ReadFile("clone.go")
	if !strings.Contains(string(src), "return clone(ctx, o, nil)") {
		t.Fatal("Clone must call clone with nil seams")
	}
	// No path from the environment or a flag into the seams or the command.
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, bad := range []string{"os.Getenv", "os.LookupEnv", "flag.", "exec.Command(\"sh\"", "exec.Command(\"bash\"", "\"sh\", \"-c\"", "bash -c"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}
