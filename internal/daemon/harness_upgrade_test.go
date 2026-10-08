package daemon

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestPiUpgradeSelectsOnlyConnectionsFollowingTheCLILauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix launcher fixture")
	}
	t.Setenv("PI_ACP_SDK_ROOT", "")
	require.NoError(t, os.Unsetenv("PI_ACP_SDK_ROOT"))
	root := t.TempDir()
	packageRoot := filepath.Join(root, "sdk")
	require.NoError(t, os.MkdirAll(packageRoot, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","version":"1.2.2","bin":{"pi":"cli.js"}}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "cli.js"), []byte("#!/bin/sh\nexit 0\n"), 0755))
	launcher := filepath.Join(root, "pi")
	require.NoError(t, os.Symlink(filepath.Join(packageRoot, "cli.js"), launcher))
	conn := control.AgentConnectionView{ID: "default", RemoteID: "remote", Harness: "pi", Enabled: true, DesiredState: control.DesiredStateRunning, Command: []string{"pi-acp"}, Env: map[string]string{"PATH": root}}
	pinned := conn
	pinned.ID = "pinned"
	pinned.Env = map[string]string{"PI_ACP_SDK_ROOT": packageRoot}
	req := &control.UpgradeHarnessCommand{Harness: "pi", Component: "cli", Version: "1.2.3"}
	before := &harnessInstallation{Path: launcher, Version: "1.2.2"}
	affected, err := affectedHarnessConnections(t.Context(), "remote", []control.AgentConnectionView{conn, pinned}, req, before)
	require.NoError(t, err)
	require.Equal(t, []control.AgentConnectionView{conn}, affected)
	conn.RemoteID = "other"
	_, err = affectedHarnessConnections(t.Context(), "remote", []control.AgentConnectionView{conn}, req, before)
	require.ErrorContains(t, err, "shared with another remote")
	conn.Command = []string{"npx", "pi-acp"}
	_, err = affectedHarnessConnections(t.Context(), "remote", []control.AgentConnectionView{conn}, req, before)
	require.ErrorContains(t, err, "custom ACP launcher")
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
