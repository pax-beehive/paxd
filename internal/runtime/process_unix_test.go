//go:build !windows

package runtime

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecProcessTerminateKillsEntireProcessGroupAfterGracePeriod(t *testing.T) {
	proc, err := (ExecLocalACPProcessRunner{
		TerminateGracePeriod: 100 * time.Millisecond,
	}).Start(context.Background(), LocalACPProcessSpec{
		Command: []string{"/bin/sh", "-c", "trap '' INT; printf ready; sleep 30 & wait"},
	})
	require.NoError(t, err)
	ready := make([]byte, len("ready"))
	_, err = io.ReadFull(proc.Stdout(), ready)
	require.NoError(t, err)
	require.Equal(t, "ready", string(ready))

	execProc := proc.(*execProcess)
	processGroupID := execProc.cmd.Process.Pid
	require.NoError(t, proc.Terminate(context.Background()))

	require.Eventually(t, func() bool {
		err := syscall.Kill(-processGroupID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
}
