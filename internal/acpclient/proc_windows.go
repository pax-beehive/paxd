//go:build windows

package acpclient

import "os/exec"

// prepareCommandGroup is a no-op on Windows; see the unix implementation.
func prepareCommandGroup(cmd *exec.Cmd) {}

// killCommandGroup kills only the direct process on Windows.
func killCommandGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
