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

func TestExecProcessTerminateAfterStartContextCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := (ExecLocalACPProcessRunner{
		TerminateGracePeriod: 100 * time.Millisecond,
	}).Start(ctx, LocalACPProcessSpec{
		Command: []string{"/bin/sh", "-c", "trap '' INT; printf ready; sleep 30 & wait"},
	})
	require.NoError(t, err)
	ready := make([]byte, len("ready"))
	_, err = io.ReadFull(proc.Stdout(), ready)
	require.NoError(t, err)

	execProc := proc.(*execProcess)
	processGroupID := execProc.cmd.Process.Pid
	execProc.startWait()
	cancel()
	select {
	case <-execProc.waitDone:
		t.Fatal("start context cancellation killed the process leader directly")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, proc.Terminate(context.Background()))
	require.Eventually(t, func() bool {
		err := syscall.Kill(-processGroupID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
}

func TestACPSlotSessionCancellationStopsStubbornProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	registry := NewACPPoolRegistry(ACPRouteStoreFactoryFunc(func(connectionID string) ACPRouteStore {
		return newFakeACPRouteStore(connectionID)
	}))
	session := NewACPSlotSession(ACPSlotSessionConfig{
		Spec: ACPSlotSpec{
			ConnectionID: "conn_1",
			RemoteID:     "local",
			SlotID:       "slot_stubborn",
			Ordinal:      0,
			ProcessEpoch: "epoch_stubborn",
			Command: []string{"/bin/sh", "-c",
				`trap '' INT; sleep 30 & IFS= read -r _; printf '%s\n' '{"jsonrpc":"2.0","id":"paxd.initialize","result":{"protocolVersion":1}}'; IFS= read -r _; wait`,
			},
			PaxdVersion: "test",
		},
		Runner:   ExecLocalACPProcessRunner{TerminateGracePeriod: 100 * time.Millisecond},
		Registry: registry,
		ReadyHandler: func(context.Context, ACPSlotSpec) {
			ready <- struct{}{}
		},
	})

	done := make(chan Exit, 1)
	go func() { done <- session.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("stubborn ACP slot did not become ready")
	}
	cancel()
	select {
	case exit := <-done:
		require.Equal(t, ExitTerminal, exit.Class)
		require.Equal(t, "canceled", exit.Code)
	case <-time.After(time.Second):
		t.Fatal("ACP slot session remained stuck after cancellation")
	}
}

func TestExecProcessWaitKillsDescendantsAfterLeaderExit(t *testing.T) {
	proc, err := ExecLocalACPProcessRunner{}.Start(context.Background(), LocalACPProcessSpec{
		Command: []string{"/bin/sh", "-c", "trap '' INT; sleep 30 & printf ready; exec /bin/true"},
	})
	require.NoError(t, err)
	ready := make([]byte, len("ready"))
	_, err = io.ReadFull(proc.Stdout(), ready)
	require.NoError(t, err)

	execProc := proc.(*execProcess)
	processGroupID := execProc.cmd.Process.Pid
	require.NoError(t, proc.Wait())
	require.Eventually(t, func() bool {
		err := syscall.Kill(-processGroupID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
}
