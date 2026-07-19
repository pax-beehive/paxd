//go:build !windows

package acpclient

import (
	"os/exec"
	"syscall"
)

// prepareCommandGroup puts the command in its own process group so cleanup
// can kill the whole process tree. ACP adapters may be wrappers (for example
// a Node launcher around a native binary) whose children inherit the stdio
// pipes; killing only the direct child leaves those pipes open and blocks
// cmd.Wait forever.
func prepareCommandGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killCommandGroup kills the command's whole process group, falling back to
// the direct process when the group signal fails.
func killCommandGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
