package control_test

import (
	"context"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/paxlinstall"
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
	"time"
)

type fakePaxlInstaller struct{ calls atomic.Int32 }

func (f *fakePaxlInstaller) Upgrade(_ context.Context, _, _, version, _ string, phase func(string)) (paxlinstall.Observation, error) {
	f.calls.Add(1)
	phase("verifying")
	return paxlinstall.Observation{Status: "installed", Version: version}, nil
}
func TestPaxlUpgradeWaitsForAckAndDeduplicates(t *testing.T) {
	store := openControlTestStore(t)
	installer := &fakePaxlInstaller{}
	service := control.NewService(control.ServiceOptions{Store: store, PaxlInstaller: installer})
	cmd := control.Command{CommandID: "paxl-test", Type: control.CommandUpgradePaxl, UpgradePaxl: &control.UpgradePaxlCommand{Version: "1.2.3"}}
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

func TestPaxlUpgradeRejectsLocalAndCrossRemoteReplay(t *testing.T) {
	store := openControlTestStore(t)
	service := control.NewService(control.ServiceOptions{Store: store, PaxlInstaller: &fakePaxlInstaller{}})
	cmd := control.Command{CommandID: "scoped", Type: control.CommandUpgradePaxl, UpgradePaxl: &control.UpgradePaxlCommand{Version: "1.2.3"}}
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

func TestPaxlUpgradeResumesReceivedCommandAfterServiceRestart(t *testing.T) {
	store := openControlTestStore(t)
	installer := &fakePaxlInstaller{}
	service := control.NewService(control.ServiceOptions{Store: store, PaxlInstaller: installer})
	cmd := control.Command{CommandID: "resume", Type: control.CommandUpgradePaxl, UpgradePaxl: &control.UpgradePaxlCommand{Version: "1.2.3"}}
	src := control.Source{Kind: control.SourceRemote, RemoteID: "a"}
	_, err := service.HandleCommand(t.Context(), src, cmd)
	require.NoError(t, err)
	restarted := control.NewService(control.ServiceOptions{Store: store, PaxlInstaller: installer})
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
