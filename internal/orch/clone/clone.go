// Package clone clones a registered project from its GitLab provider into a
// folder directly under one of the allowed repository roots. It is the only
// place orchd runs `git clone`, and it is built to be hard to misuse:
//
//   - git is started with os/exec directly, never through a shell, and the
//     argument list is assembled here from validated parts;
//   - the remote must have the exact shape ssh://git@host[:port]/group/.../name.git
//     and its host must be pinned in the known_hosts file (nothing is tried
//     otherwise); where the remote comes from is the caller's business, and
//     the API builds it only from the registry entry (never from request data);
//   - the protocol list is closed (ssh only), hooks and the file system monitor
//     are off, submodules are not followed, `--no-local` is always given, the
//     environment is rebuilt from scratch (no inherited GIT_* or SSH_* values)
//     and ssh may use only the deploy key and the pinned host keys;
//   - the destination is created here with mkdir (it must not exist, a symlink
//     included) and, on failure, only that directory is removed;
//   - git's own output is discarded: errors are fixed sentinels.
//
// The private key is never opened by this code: only its path goes into the ssh
// command line that git runs.
package clone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aigate/internal/orch/knownhosts"
	"aigate/internal/registrymsg"
)

// DefaultTimeout is the clone time limit (D5 said 120 s). The retry endpoint's
// response is written by an http.Server with a 120 s WriteTimeout (daemon.go),
// so 100 s plus the local work around the clone has to fit inside that; the
// browser itself waits 130 s.
const DefaultTimeout = 100 * time.Second

// Fixed errors. None carries git output, a path or a host.
var (
	ErrBadConfig     = errors.New("clone: the ssh key or known_hosts setting is unusable")
	ErrBadRemote     = errors.New("clone: the remote is not an allowed ssh remote")
	ErrBadPath       = errors.New("clone: the destination is not a new folder directly under an allowed root")
	ErrHostNotPinned = errors.New("clone: the host is not pinned in the known_hosts file")
	ErrExists        = errors.New("clone: the destination already exists")
	ErrInProgress    = errors.New("clone: this destination is being cloned already")
	ErrFailed        = errors.New("clone: git clone failed")
	ErrTimeout       = errors.New("clone: git clone timed out")
)

// Options describes one clone. Roots are the allowed repository roots
// (symlink-free absolute paths); Dest must be a direct child of one of them.
type Options struct {
	// Git is the git binary ("" = "git").
	Git string
	// Remote is ssh://git@host[:port]/group/.../name.git.
	Remote string
	// Dest is the absolute, clean path of the new folder.
	Dest  string
	Roots []string
	// SSH is the ssh binary's absolute path ("" = "ssh", found by git).
	SSH string
	// SSHKey is the deploy key's PRIVATE path (only named in the ssh command)
	// and KnownHosts the pinning file; both absolute without special characters.
	SSHKey, KnownHosts string
	// Timeout bounds the clone (0 = DefaultTimeout).
	Timeout time.Duration
}

// seams are the test-only replacements of the command runner and of the
// remote and option list (a local fixture cannot use the ssh transport). The
// type is unexported and production code never builds one: Clone passes nil.
// TestProductionCannotEnableCloneSeams pins that.
type seams struct {
	run           func(ctx context.Context, cmd *exec.Cmd) error
	rewriteRemote func(remote string) string
	extraConfig   []string
	extraEnv      []string
	dropHooksPath bool
	// dropTemplate leaves out --template= (a control that shows the option matters).
	dropTemplate bool
}

var (
	inflightMu sync.Mutex
	inflight   = map[string]bool{}
)

// Clone runs the clone described by o. See the package comment.
func Clone(ctx context.Context, o Options) error {
	return clone(ctx, o, nil)
}

func sshSafe(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && !strings.ContainsAny(s, " \t'\"\\$`;&|<>(){}*?[]!#~\n\r\x00")
}

// sshCommand is the only ssh git may run: this key, the pinned host keys, no
// agent, config file, proxy, forwarding or prompt.
func sshCommand(sshBin, key, knownHosts string) string {
	if sshBin == "" {
		sshBin = "ssh"
	}
	// UpdateHostKeys=no: ssh may never add or rewrite entries in the pinning file.
	return sshBin + " -F /dev/null -i " + key + " -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes" +
		" -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o UserKnownHostsFile=" + knownHosts + " -o GlobalKnownHostsFile=/dev/null" +
		" -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ProxyCommand=none -o ControlMaster=no -o ConnectTimeout=20"
}

// destOK checks the destination: absolute, clean, a direct child of an allowed
// root (the parent must be that root exactly, with no symlink anywhere in its
// path, so nothing can redirect the new folder), with a plain project name.
func destOK(dest string, roots []string) bool {
	if dest == "" || !filepath.IsAbs(dest) || filepath.Clean(dest) != dest {
		return false
	}
	if !registrymsg.ValidName(filepath.Base(dest)) {
		return false
	}
	parent := filepath.Dir(dest)
	if real, err := filepath.EvalSymlinks(parent); err != nil || real != parent {
		return false
	}
	for _, r := range roots {
		if r == parent && filepath.Clean(r) == r {
			return true
		}
	}
	return false
}

func clone(ctx context.Context, o Options, sm *seams) error {
	if !sshSafe(o.SSHKey) || !sshSafe(o.KnownHosts) || (o.SSH != "" && !sshSafe(o.SSH)) {
		return ErrBadConfig
	}
	host, port, ok := knownhosts.SplitSSHRemote(o.Remote)
	if !ok {
		return ErrBadRemote
	}
	if !destOK(o.Dest, o.Roots) {
		return ErrBadPath
	}
	if !knownhosts.Pinned(o.KnownHosts, host, port) {
		return ErrHostNotPinned
	}
	inflightMu.Lock()
	if inflight[o.Dest] {
		inflightMu.Unlock()
		return ErrInProgress
	}
	inflight[o.Dest] = true
	inflightMu.Unlock()
	defer func() {
		inflightMu.Lock()
		delete(inflight, o.Dest)
		inflightMu.Unlock()
	}()

	// The destination must not exist (a symlink included). git clones into a
	// staging directory next to it, .clone-<random> (never a valid project
	// name), claimed with mkdir; a finished clone is renamed to the destination
	// without replacing anything. A crash leaves only a .clone-* directory,
	// which SweepStale removes at startup. Only the staging directory this call
	// created is ever cleaned.
	if _, err := os.Lstat(o.Dest); err == nil {
		return ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrBadPath
	}
	staging, made, err := makeStaging(filepath.Dir(o.Dest))
	if err != nil {
		return ErrFailed
	}
	cleanup := func() {
		if fi, err := os.Lstat(staging); err == nil && fi.IsDir() && os.SameFile(fi, made) {
			_ = os.RemoveAll(staging)
		}
	}
	scratch, err := os.MkdirTemp("", "orch-clone-")
	if err != nil {
		cleanup()
		return ErrFailed
	}
	defer os.RemoveAll(scratch)

	remote := o.Remote
	if sm != nil && sm.rewriteRemote != nil {
		remote = sm.rewriteRemote(remote)
	}
	gitBin := o.Git
	if gitBin == "" {
		gitBin = "git"
	}
	args := []string{"-c", "protocol.allow=never", "-c", "protocol.ssh.allow=always"}
	if sm == nil || !sm.dropHooksPath {
		args = append(args, "-c", "core.hooksPath=/dev/null")
	}
	args = append(args, "-c", "core.fsmonitor=false")
	if sm != nil {
		for _, kv := range sm.extraConfig {
			args = append(args, "-c", kv)
		}
	}
	args = append(args, "clone", "--no-recurse-submodules", "--no-local")
	if sm == nil || !sm.dropTemplate {
		args = append(args, "--template=")
	}
	args = append(args, "--", remote, staging)

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, gitBin, args...)
	cmd.Dir = scratch
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin", "HOME=" + scratch, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=" + sshCommand(o.SSH, o.SSHKey, o.KnownHosts),
	}
	if sm != nil {
		cmd.Env = append(cmd.Env, sm.extraEnv...)
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = io.Discard, io.Discard, nil
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	var runErr error
	if sm != nil && sm.run != nil {
		runErr = sm.run(cctx, cmd)
	} else {
		runErr = cmd.Run()
	}
	if runErr != nil {
		timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
		cleanup()
		if timedOut {
			return ErrTimeout
		}
		return ErrFailed
	}
	if err := renameNoReplace(staging, o.Dest); err != nil {
		cleanup()
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return ErrFailed
	}
	return nil
}

// tempPrefix starts every staging directory name; it is not a valid project name.
const tempPrefix = ".clone-"

func makeStaging(parent string) (string, os.FileInfo, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	p := filepath.Join(parent, tempPrefix+hex.EncodeToString(b[:]))
	if err := os.Mkdir(p, 0o700); err != nil {
		return "", nil, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		os.Remove(p)
		return "", nil, err
	}
	return p, fi, nil
}

// SweepStale removes the .clone-* directories a crashed clone left directly
// under each root. Only real directories are removed (a symlink or a file of
// that name is left alone, nothing is followed, nothing deeper is looked at).
// It returns how many were removed. orchd calls it once at startup.
func SweepStale(roots []string) int {
	n := 0
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !strings.HasPrefix(e.Name(), tempPrefix) {
				continue
			}
			p := filepath.Join(root, e.Name())
			if fi, err := os.Lstat(p); err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
				if os.RemoveAll(p) == nil {
					n++
				}
			}
		}
	}
	return n
}

// IsStaging reports whether a directory name is a clone staging name.
func IsStaging(name string) bool { return strings.HasPrefix(name, tempPrefix) }

// toolDirs are the only directories git and ssh are looked for in.
func toolDirs() []string {
	return []string{"/usr/local/bin", "/usr/bin", "/bin", "/opt/homebrew/bin"}
}

// FindTool returns the absolute path of the executable name in the fixed tool
// directories ("" if absent). It never reads the environment.
func FindTool(name string) string { return findIn(toolDirs(), name) }

func findIn(dirs []string, name string) string {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return ""
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}
