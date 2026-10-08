package daemon

import (
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/stretchr/testify/require"
)

func TestHarnessUpgradeRequiresEveryNewSlotToReportTargetVersion(t *testing.T) {
	reports := newACPCapabilityReports()
	connections := []control.AgentConnectionView{{ID: "conn", DesiredACPSlots: 2}}
	report := runtimes.ACPPoolCapabilityReport{
		ConnectionID: "conn", InitPhase: runtimes.ACPPoolInitPhaseReady,
		Implementation: &runtimes.ACPImplementationIdentity{ACPAgent: &runtimes.ACPAgentImplementation{Version: "1.2.3"}},
	}
	slot := runtimes.ACPSlotSpec{ConnectionID: "conn", SlotID: "one", ProcessEpoch: "old"}
	require.NoError(t, reports.ReportACPSlotCapability(t.Context(), slot, &report))
	previous := reports.harnessEpochs(connections)
	var ready bool
	ready, _ = reports.harnessReady(connections, previous, "1.2.3")
	require.False(t, ready)
	slot.ProcessEpoch = "new-one"
	require.NoError(t, reports.ReportACPSlotCapability(t.Context(), slot, &report))
	ready, _ = reports.harnessReady(connections, previous, "1.2.3")
	require.False(t, ready)
	slot.SlotID, slot.ProcessEpoch = "two", "new-two"
	report.Implementation.ACPAgent.Version = "1.2.2"
	require.NoError(t, reports.ReportACPSlotCapability(t.Context(), slot, &report))
	ready, _ = reports.harnessReady(connections, previous, "1.2.3")
	require.False(t, ready)
	report.Implementation.ACPAgent.Version = "1.2.3"
	require.NoError(t, reports.ReportACPSlotCapability(t.Context(), slot, &report))
	ready, err := reports.harnessReady(connections, previous, "1.2.3")
	require.NoError(t, err)
	require.True(t, ready)
}

func TestHarnessUpgradeAndDaemonMaintenanceAreMutuallyExclusive(t *testing.T) {
	c := newLifecycleCoordinator("boot")
	release, err := c.reserveHarnessUpgrade("harness")
	require.NoError(t, err)
	require.Error(t, c.ScheduleRestart("restart", control.RestartPaxdCommand{}))
	_, err = c.reserveHarnessUpgrade("second")
	require.Error(t, err)
	release()
	require.NoError(t, c.ScheduleRestart("restart", control.RestartPaxdCommand{}))
	_, err = c.reserveHarnessUpgrade("harness")
	require.Error(t, err)
}

func TestCLIUpgradeVerifiesRuntimeRatherThanAdapterVersion(t *testing.T) {
	reports := newACPCapabilityReports()
	connections := []control.AgentConnectionView{{ID: "conn", Harness: "codex", DesiredACPSlots: 1}}
	report := runtimes.ACPPoolCapabilityReport{ConnectionID: "conn", InitPhase: runtimes.ACPPoolInitPhaseReady, Implementation: &runtimes.ACPImplementationIdentity{ACPAgent: &runtimes.ACPAgentImplementation{Version: "1.2.3"}, Runtime: &runtimes.ACPRuntimeImplementation{Name: "codex", Version: "1.2.2"}}}
	slot := runtimes.ACPSlotSpec{ConnectionID: "conn", SlotID: "one", ProcessEpoch: "new"}
	require.NoError(t, reports.ReportACPSlotCapability(t.Context(), slot, &report))
	ready, err := reports.harnessComponentReady(connections, nil, "1.2.3", "cli")
	require.Error(t, err)
	require.False(t, ready)
	report.Implementation.Runtime.Version = "1.2.3"
	report.Implementation.ACPAgent.Version = "9.0.0"
	require.NoError(t, reports.ReportACPSlotCapability(t.Context(), slot, &report))
	ready, err = reports.harnessComponentReady(connections, nil, "1.2.3", "cli")
	require.NoError(t, err)
	require.True(t, ready)
}
