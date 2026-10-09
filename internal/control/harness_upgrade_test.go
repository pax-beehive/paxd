package control_test

import (
	"context"
	"encoding/json"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
	"time"
)

type fakeHarnessInstaller struct{ calls atomic.Int32 }

func (f *fakeHarnessInstaller) UpgradeHarness(_ context.Context, _, _ string, req *control.UpgradeHarnessCommand, phase func(string)) (json.RawMessage, error) {
	f.calls.Add(1)
	phase("verifying_runtime")
	return json.Marshal(map[string]string{"version": req.Version})
}
func TestHarnessUpgradeWaitsForAckAndDeduplicates(t *testing.T) {
	store := openControlTestStore(t)
	installer := &fakeHarnessInstaller{}
	service := control.NewService(control.ServiceOptions{Store: store, HarnessInstaller: installer})
	cmd := control.Command{CommandID: "paxl-test", Type: control.CommandUpgradeHarness, UpgradeHarness: &control.UpgradeHarnessCommand{Harness: "codex", Component: "acp", ConnectionID: "adapter", Version: "1.2.3"}}
	src := control.Source{Kind: control.SourceRemote, RemoteID: "remote"}
	ack, err := service.HandleCommand(t.Context(), src, cmd)
	require.NoError(t, err)
	require.Equal(t, control.CommandStatusReceived, ack.Status)
	require.Zero(t, installer.calls.Load())
	service.ConfirmCommandAckDelivered(cmd.CommandID)
	require.Eventually(t, func() bool {
		rec, e := store.GetCommandRecord(t.Context(), cmd.CommandID)
		return e == nil && rec.Status == control.CommandStatusApplied
	}, time.Second, time.Millisecond*10)
	ack, err = service.HandleCommand(t.Context(), src, cmd)
	require.NoError(t, err)
	require.Equal(t, control.CommandStatusApplied, ack.Status)
	service.ConfirmCommandAckDelivered(cmd.CommandID)
	require.EqualValues(t, 1, installer.calls.Load())
}

func TestHarnessUpgradeRejectsLocalAndCrossRemoteReplay(t *testing.T) {
	store := openControlTestStore(t)
	service := control.NewService(control.ServiceOptions{Store: store, HarnessInstaller: &fakeHarnessInstaller{}})
	cmd := control.Command{CommandID: "scoped", Type: control.CommandUpgradeHarness, UpgradeHarness: &control.UpgradeHarnessCommand{Harness: "codex", Component: "acp", ConnectionID: "adapter", Version: "1.2.3"}}
	ack, err := service.HandleCommand(t.Context(), control.Source{Kind: control.SourceLocal}, cmd)
	require.NoError(t, err)
	require.Equal(t, control.CommandStatusRejected, ack.Status)
	ack, err = service.HandleCommand(t.Context(), control.Source{Kind: control.SourceRemote, RemoteID: "a"}, cmd)
	require.NoError(t, err)
	require.Equal(t, control.CommandStatusReceived, ack.Status)
	ack, err = service.HandleCommand(t.Context(), control.Source{Kind: control.SourceRemote, RemoteID: "b"}, cmd)
	require.NoError(t, err)
	require.Equal(t, control.CommandStatusRejected, ack.Status)
	require.Equal(t, control.ErrCodeConflict, ack.Error.Code)
}

func TestHarnessUpgradeResumesReceivedCommandAfterServiceRestart(t *testing.T) {
	store := openControlTestStore(t)
	installer := &fakeHarnessInstaller{}
	service := control.NewService(control.ServiceOptions{Store: store, HarnessInstaller: installer})
	cmd := control.Command{CommandID: "resume", Type: control.CommandUpgradeHarness, UpgradeHarness: &control.UpgradeHarnessCommand{Harness: "codex", Component: "acp", ConnectionID: "adapter", Version: "1.2.3"}}
	src := control.Source{Kind: control.SourceRemote, RemoteID: "a"}
	_, err := service.HandleCommand(t.Context(), src, cmd)
	require.NoError(t, err)
	restarted := control.NewService(control.ServiceOptions{Store: store, HarnessInstaller: installer})
	ack, err := restarted.HandleCommand(t.Context(), src, cmd)
	require.NoError(t, err)
	require.Equal(t, control.CommandStatusReceived, ack.Status)
	restarted.ConfirmCommandAckDelivered(cmd.CommandID)
	require.Eventually(t, func() bool {
		rec, e := store.GetCommandRecord(t.Context(), cmd.CommandID)
		return e == nil && rec.Status == control.CommandStatusApplied
	}, time.Second, time.Millisecond*10)
	require.EqualValues(t, 1, installer.calls.Load())
}

func TestHarnessUpgradeAcceptsBothComponents(t *testing.T) {
	for _, harness := range []string{"claude-code", "codex", "pi"} {
		cmd := control.UpgradeHarnessCommand{Harness: harness, Component: "cli", Version: "1.2.3"}
		require.NoError(t, cmd.Validate())
		cmd.Component = "acp"
		require.Error(t, cmd.Validate())
		cmd.ConnectionID = "adapter"
		require.NoError(t, cmd.Validate())
	}
}

func TestHarnessUpgradeVersionDefaults(t *testing.T) {
	for _, version := range []string{"", " ", "latest", "1.2.3", "1.2.3-beta.1"} {
		require.NoError(t, (control.UpgradeHarnessCommand{Harness: "codex", Component: "cli", Version: version}).Validate())
	}
	for _, version := range []string{"next", "^1.2.3", "1", "https://example.com/package"} {
		require.Error(t, (control.UpgradeHarnessCommand{Harness: "codex", Component: "cli", Version: version}).Validate())
	}
}
