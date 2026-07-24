//go:build windows

package runtime

import (
	"os"
	"os/exec"
)

func prepareExecCommand(_ *exec.Cmd) {}

func interruptExecCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}

func killExecCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
