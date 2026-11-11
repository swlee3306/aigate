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

// DefaultTimeout is the clone time limit of D5.
const DefaultTimeout = 120 * time.Second

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
func sshCommand(key, knownHosts string) string {
	return "ssh -F /dev/null -i " + key + " -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes" +
		" -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + knownHosts + " -o GlobalKnownHostsFile=/dev/null" +
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
	if !sshSafe(o.SSHKey) || !sshSafe(o.KnownHosts) {
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

	// The destination is claimed by mkdir: it fails if anything is there, a
	// symlink included, and what it creates is the only thing cleaned later.
	if err := os.Mkdir(o.Dest, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return ErrBadPath
	}
	made, err := os.Lstat(o.Dest)
	if err != nil {
		return ErrFailed
	}
	cleanup := func() {
		if fi, err := os.Lstat(o.Dest); err == nil && fi.IsDir() && os.SameFile(fi, made) {
			_ = os.RemoveAll(o.Dest)
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
	args = append(args, "clone", "--no-recurse-submodules", "--no-local", "--", remote, o.Dest)

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
		"GIT_SSH_COMMAND=" + sshCommand(o.SSHKey, o.KnownHosts),
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
	return nil
}
