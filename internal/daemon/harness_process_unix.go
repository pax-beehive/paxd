//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

func configureHarnessPaxlProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// paxl handles SIGTERM by canceling its installer process group and restoring
	// any unverified launcher before exiting. WaitDelay bounds that recovery.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
}
