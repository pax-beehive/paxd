//go:build !windows

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareExecCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptExecCommand(cmd *exec.Cmd) error {
	return signalExecCommandGroup(cmd, syscall.SIGINT)
}

func killExecCommand(cmd *exec.Cmd) error {
	return signalExecCommandGroup(cmd, syscall.SIGKILL)
}

func signalExecCommandGroup(cmd *exec.Cmd, signal os.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	unixSignal, ok := signal.(syscall.Signal)
	if !ok {
		return cmd.Process.Signal(signal)
	}
	err := syscall.Kill(-cmd.Process.Pid, unixSignal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return err
	}
	return cmd.Process.Signal(signal)
}
