//go:build windows

package builtin

import (
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {
	// Windows does not support Setpgid
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}
