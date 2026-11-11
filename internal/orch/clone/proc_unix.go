//go:build unix

package clone

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts git in its own process group and, when the command's
// context ends, kills the whole group (git's ssh child included).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
