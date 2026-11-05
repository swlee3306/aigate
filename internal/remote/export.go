package remote

import (
	"context"
	"io"
)

// Exports for drivers that talk to other VMs with the same hardened ssh
// invocation, escaping and bundle/env rules (internal/orch/vm).

// SSHOptions is every hardened ssh option except the target host, for a
// caller (git's GIT_SSH_COMMAND) that appends the host itself.
func (e *SSH) SSHOptions() []string { return e.sshOpts() }

// Bin is the ssh binary in use.
func (e *SSH) Bin() string { return e.bin }

// Host is the configured target host.
func (e *SSH) Host() string { return e.cfg.Host }

// Call runs one ssh command line and returns its stdout, at most maxOut
// bytes; see (*SSH).call for the failure rules.
func (e *SSH) Call(ctx context.Context, remote string, stdin io.Reader, maxOut, maxErr int) ([]byte, error) {
	return e.call(ctx, remote, stdin, maxOut, maxErr)
}

// MakeBundle writes a git bundle of shas; see makeBundle.
func MakeBundle(repo string, shas []string) (path string, cleanup func(), err error) {
	return makeBundle(repo, shas)
}

// GitEnv is the allowlisted environment for local git commands.
func GitEnv() []string { return gitEnv() }

// Quote escapes one word for a POSIX shell.
func Quote(s string) string { return shq(s) }

// ValidSHA reports whether s is a full commit id.
func ValidSHA(s string) bool { return shaRE.MatchString(s) }

// Clip shortens untrusted text for error messages.
func Clip(b []byte) string { return clip(b) }
