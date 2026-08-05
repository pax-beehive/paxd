package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
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

func TestNewBootIDIsUniqueAndOpaque(t *testing.T) {
	first, err := newBootID()
	require.NoError(t, err)
	second, err := newBootID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(first, "boot_"))
	assert.Len(t, first, len("boot_")+32)
	assert.NotEqual(t, first, second)
}
