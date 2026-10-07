//go:build !windows

package harnessauth

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareLoginCommand(cmd *exec.Cmd) {
	// Native CLIs may be launched through Node or shell wrappers. Cancellation
	// must stop the credential-writing descendants as well as the wrapper.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return stopLoginCommand(cmd) }
}

func stopLoginCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
