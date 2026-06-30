//go:build windows

package acpbridge

import "os/exec"

func prepareChildProcess(cmd *exec.Cmd) {}

func terminateChildProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
