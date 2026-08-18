package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/updater"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycleCoordinatorEmitsRestartOnlyAfterAckConfirmation(t *testing.T) {
	coordinator := newLifecycleCoordinator("boot_test")
	coordinator.ScheduleRestart("cmd_restart_1", control.RestartPaxdCommand{
		Mode: control.PaxdRestartImmediate, ShutdownGraceSeconds: 12, Reason: "operator requested",
	})

	select {
	case request := <-coordinator.ExitRequests():
		t.Fatalf("exit request emitted before ACK confirmation: %+v", request)
	default:
	}
	coordinator.ConfirmAckDelivered("cmd_other")
	select {
	case request := <-coordinator.ExitRequests():
		t.Fatalf("unrelated ACK emitted exit request: %+v", request)
	default:
	}

	coordinator.ConfirmAckDelivered("cmd_restart_1")
	select {
	case request := <-coordinator.ExitRequests():
		assert.Equal(t, "cmd_restart_1", request.CommandID)
		assert.Equal(t, "operator requested", request.Reason)
		assert.Equal(t, 12*time.Second, request.ShutdownGrace)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for confirmed restart request")
	}
	coordinator.ConfirmAckDelivered("cmd_restart_1")
	select {
	case request := <-coordinator.ExitRequests():
		t.Fatalf("duplicate ACK emitted another exit request: %+v", request)
	default:
	}
}

func TestLifecycleCoordinatorUsesDefaultRestartGrace(t *testing.T) {
	coordinator := newLifecycleCoordinator("boot_test")
	coordinator.ScheduleRestart("cmd_restart_default", control.RestartPaxdCommand{})
	coordinator.ConfirmAckDelivered("cmd_restart_default")
	request := <-coordinator.ExitRequests()
	assert.Equal(t, defaultRemoteRestartGrace, request.ShutdownGrace)
}

func TestLifecycleCoordinatorStagesAndActivatesUpgradeOnlyAfterAck(t *testing.T) {
	update := &fakeMaintenanceUpdater{
		staged:    make(chan updater.Request, 1),
		activated: make(chan string, 1),
	}
	coordinator := newLifecycleCoordinator("boot_test")
	coordinator.Configure(nil, update, nil)
	require.NoError(t, coordinator.ScheduleUpgrade("cmd_upgrade_1", "remote_home", control.UpgradePaxdCommand{
		Version: "1.2.3", Mode: control.PaxdUpgradeImmediate,
	}))

	select {
	case request := <-update.staged:
		t.Fatalf("upgrade staged before ACK confirmation: %+v", request)
	default:
	}
	coordinator.ConfirmAckDelivered("cmd_upgrade_1")

	select {
	case request := <-update.staged:
		assert.Equal(t, "cmd_upgrade_1", request.CommandID)
		assert.Equal(t, "remote_home", request.RemoteID)
		assert.Equal(t, "1.2.3", request.Version)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upgrade staging")
	}
	select {
	case requestedBootID := <-update.activated:
		assert.Equal(t, "boot_test", requestedBootID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upgrade activation")
	}
	select {
	case request := <-coordinator.ExitRequests():
		assert.Equal(t, "cmd_upgrade_1", request.CommandID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upgrade exit request")
	}
}

type fakeMaintenanceUpdater struct {
	staged    chan updater.Request
	activated chan string
}

func (u *fakeMaintenanceUpdater) Stage(_ context.Context, request updater.Request) (updater.Candidate, error) {
	u.staged <- request
	return updater.Candidate{
		CommandID:      request.CommandID,
		Path:           "/tmp/staged-paxd",
		ExecutablePath: "/tmp/paxd",
		OldVersion:     "1.2.2",
		Version:        request.Version,
	}, nil
}

func (u *fakeMaintenanceUpdater) Activate(
	_ updater.Candidate,
	requestedBootID string,
) (updater.ActivationRecord, error) {
	u.activated <- requestedBootID
	return updater.ActivationRecord{}, nil
}

func (u *fakeMaintenanceUpdater) Cleanup(updater.Candidate) {}

func TestNewBootIDIsUniqueAndOpaque(t *testing.T) {
	first, err := newBootID()
	require.NoError(t, err)
	second, err := newBootID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(first, "boot_"))
	assert.Len(t, first, len("boot_")+32)
	assert.NotEqual(t, first, second)
}
