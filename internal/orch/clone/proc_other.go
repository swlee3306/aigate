//go:build !unix

package clone

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
