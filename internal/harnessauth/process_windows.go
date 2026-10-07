//go:build windows

package harnessauth

import (
	"os"
	"os/exec"
)

func prepareLoginCommand(_ *exec.Cmd) {}

func stopLoginCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
