package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
)

type harnessUpgradeCoordinator struct {
	store       *daemonstore.Store
	supervisors *runtimeSupervisors
	maintenance *lifecycleCoordinator
}

func (c *lifecycleCoordinator) reserveHarnessUpgrade(id string) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.committed || c.activeID != "" || c.externalID != "" {
		return nil, fmt.Errorf("another maintenance operation is in progress")
	}
	c.externalID = id
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.externalID == id {
			c.externalID = ""
		}
	}, nil
}

func (u *harnessUpgradeCoordinator) UpgradeHarness(ctx context.Context, id, remoteID string, req *control.UpgradeHarnessCommand, phase func(string)) (json.RawMessage, error) {
	release, err := u.maintenance.reserveHarnessUpgrade(id)
	if err != nil {
		return nil, err
	}
	defer release()
	phase("inspecting")
	connections, err := u.store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		return nil, fmt.Errorf("list harness connections: %w", err)
	}
	selected, err := selectHarnessUpgradeConnection(connections, remoteID, req)
	if err != nil {
		return nil, err
	}
	client, err := newHarnessPaxlClient(req, selected)
	if err != nil {
		return nil, err
	}
	before, err := client.inspect(ctx)
	if err != nil {
		return nil, err
	}
	client.path = before.Path
	affected, err := affectedHarnessConnections(ctx, remoteID, connections, req, before)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(req.Version)
	if target == "" || target == "latest" {
		phase("resolving_version")
		var planned harnessInstallResult
		if err := client.run(ctx, "upgrade", &planned, "--version", "latest", "--dry-run"); err != nil {
			return nil, err
		}
		exact := *req
		exact.Version = planned.TargetVersion
		if planned.Phase != "planned" || exact.Version == "" || exact.Version == "latest" || exact.Validate() != nil {
			return nil, fmt.Errorf("paxl did not resolve latest to an exact target version; upgrade paxl before retrying")
		}
		target = exact.Version
	}
	registry := u.supervisors.acpPoolRegistry
	if registry == nil {
		return nil, fmt.Errorf("ACP runtime is not configured")
	}
	phase("waiting_idle")
	registry.BeginDrain(runtimes.DaemonDrainLease{CommandID: id, BootID: u.maintenance.bootID, ExpiresAt: time.Now().Add(11 * time.Minute)})
	defer registry.ClearDrain(id)
	if err := waitHarnessIdle(ctx, registry); err != nil {
		return nil, err
	}
	phase("installing")
	var installed harnessInstallResult
	if err := client.run(ctx, "upgrade", &installed, "--version", target); err != nil {
		return nil, err
	}
	if installed.Installation == nil || installed.Installation.Version != target || installed.Phase != "installed" {
		return nil, fmt.Errorf("paxl did not verify the target installation")
	}
	if len(affected) > 0 {
		phase("restarting")
		if err := u.restartAndVerifyHarness(ctx, affected, target, req.Component, phase); err != nil {
			return u.restoreHarness(client, &installed, affected, before.Version, err, phase)
		}
	}
	phase("verifying")
	observed, err := client.inspect(ctx)
	if err != nil || observed.Version != target {
		return u.restoreHarness(client, &installed, affected, before.Version, fmt.Errorf("selected installation changed during runtime verification"), phase)
	}
	for _, conn := range affected {
		if report, ok := u.supervisors.acpCapabilityReports.ACPPoolCapabilityReport(ctx, conn.ID); ok {
			installed.RuntimeReports = append(installed.RuntimeReports, report)
		}
	}
	return json.Marshal(&installed)
}

func selectHarnessUpgradeConnection(connections []control.AgentConnectionView, remoteID string, req *control.UpgradeHarnessCommand) (*control.AgentConnectionView, error) {
	if req.Component == "cli" {
		return nil, nil
	}
	for _, conn := range connections {
		if conn.ID == req.ConnectionID && conn.RemoteID == remoteID && conn.Harness == req.Harness {
			if !conn.Enabled || conn.DesiredState != control.DesiredStateRunning {
				return nil, fmt.Errorf("ACP upgrade requires an enabled running connection")
			}
			return &conn, nil
		}
	}
	return nil, fmt.Errorf("ACP connection does not belong to the selected remote and harness")
}

func sameHarnessLauncher(a, b string) bool {
	parentA, errA := filepath.EvalSymlinks(filepath.Dir(a))
	parentB, errB := filepath.EvalSymlinks(filepath.Dir(b))
	return errA == nil && errB == nil && filepath.Join(parentA, filepath.Base(a)) == filepath.Join(parentB, filepath.Base(b))
}

func affectedHarnessConnections(ctx context.Context, remoteID string, connections []control.AgentConnectionView, req *control.UpgradeHarnessCommand, before *harnessInstallation) ([]control.AgentConnectionView, error) {
	var affected []control.AgentConnectionView
	for _, conn := range connections {
		if !conn.Enabled || conn.DesiredState != control.DesiredStateRunning || conn.Harness != req.Harness {
			continue
		}
		if req.Component == "cli" {
			path, err := managedNativeConnectionPath(conn)
			if err != nil {
				return nil, err
			}
			if path != "" && sameHarnessLauncher(path, before.Path) {
				if conn.RemoteID != remoteID {
					return nil, fmt.Errorf("selected harness installation is shared with another remote; upgrade it locally")
				}
				affected = append(affected, conn)
			}
			continue
		}
		client, err := newHarnessPaxlClient(req, &conn)
		if err != nil {
			continue
		}
		installed, err := client.inspect(ctx)
		if err == nil && sameHarnessLauncher(installed.Path, before.Path) {
			if conn.RemoteID != remoteID {
				return nil, fmt.Errorf("selected ACP installation is shared with another remote; upgrade it locally")
			}
			affected = append(affected, conn)
		}
	}
	if req.Component == "acp" && len(affected) == 0 {
		return nil, fmt.Errorf("no running ACP connection uses the selected launcher")
	}
	return affected, nil
}

func waitHarnessIdle(ctx context.Context, registry *runtimes.ACPPoolRegistry) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	activity, unsubscribe := registry.SubscribeActivity()
	defer unsubscribe()
	for !registry.ActivitySnapshot().Idle() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for active tasks: %w", ctx.Err())
		case <-activity:
		}
	}
	return nil
}

func (u *harnessUpgradeCoordinator) restartAndVerifyHarness(ctx context.Context, connections []control.AgentConnectionView, version, component string, phase func(string)) error {
	previous := u.supervisors.acpCapabilityReports.harnessEpochs(connections)
	restarted := make([]control.AgentConnectionView, 0, len(connections))
	if err := u.store.WithTx(ctx, func(tx control.TxStore) error {
		for _, conn := range connections {
			next, err := tx.RestartAgentConnection(ctx, control.RestartAgentConnectionCommand{ConnectionID: conn.ID})
			if err != nil {
				return err
			}
			if next.Generation != conn.Generation || next.RestartNonce != conn.RestartNonce+1 {
				return fmt.Errorf("ACP connection %s changed during the upgrade", conn.ID)
			}
			restarted = append(restarted, next)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("restart upgraded ACP connections: %w", err)
	}
	copy(connections, restarted)
	u.supervisors.WakeAgentConnections()
	u.supervisors.WakeACPSlots()
	phase("verifying_runtime")
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		ready, err := u.supervisors.acpCapabilityReports.harnessComponentReady(connections, previous, version, component)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("new ACP processes did not report target version %s: %w", version, ctx.Err())
		case <-tick.C:
		}
	}
}

func (u *harnessUpgradeCoordinator) restoreHarness(client *harnessPaxlClient, installed *harnessInstallResult, connections []control.AgentConnectionView, previousVersion string, cause error, phase func(string)) (json.RawMessage, error) {
	if installed.RollbackID == "" {
		return nil, cause
	}
	phase("rolling_back")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var restored harnessInstallation
	if err := client.run(ctx, "rollback", &restored, "--rollback-id", installed.RollbackID); err != nil {
		return nil, fmt.Errorf("%w; rollback failed: %v", cause, err)
	}
	if len(connections) > 0 {
		if err := u.restartAndVerifyHarness(ctx, connections, previousVersion, client.component, phase); err != nil {
			return nil, fmt.Errorf("%w; previous launcher restored but runtime recovery failed: %v", cause, err)
		}
	}
	return nil, fmt.Errorf("%w; previous installation and processes restored", cause)
}

func managedNativeConnectionPath(conn control.AgentConnectionView) (string, error) {
	expected := "codex-acp"
	if conn.Harness == "claude-code" {
		expected = "claude-agent-acp"
	} else if conn.Harness == "pi" {
		expected = "pi-acp"
	}
	if len(conn.Command) != 1 || filepath.Base(conn.Command[0]) != expected {
		return "", fmt.Errorf("cannot verify harness binding for custom ACP launcher on connection %s", conn.ID)
	}
	spec := runtimes.LocalACPProcessSpec{Command: conn.Command, Env: conn.Env, WorkingDir: conn.WorkingDir}
	if conn.Harness == "pi" {
		_, launcher, err := runtimes.ResolvePiSDKBinding(spec)
		return launcher, err
	}
	return runtimes.ResolveHarnessExecutable(spec, conn.Harness)
}
