package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/updater"
)

const (
	defaultMaintenanceIdleGrace    = 5 * time.Second
	defaultMaintenanceDrainTimeout = 2 * time.Minute
	maintenanceStageTimeout        = 5 * time.Minute
	maintenancePollInterval        = 100 * time.Millisecond
)

type maintenanceActivity interface {
	BeginDrain(runtimes.DaemonDrainLease)
	ClearDrain(commandID string)
	CloseForCutover(commandID string, force bool) bool
	ActivitySnapshot() runtimes.DaemonActivitySnapshot
	SubscribeActivity() (<-chan struct{}, func())
}

type maintenanceUpdater interface {
	Stage(context.Context, updater.Request) (updater.Candidate, error)
	Activate(updater.Candidate, string) (updater.ActivationRecord, error)
	Cleanup(updater.Candidate)
}

type commandCompleter interface {
	CompleteCommand(context.Context, string, control.CommandCompletion) error
}

type maintenanceIntent struct {
	commandID string
	kind      string
	phase     string
	restart   control.RestartPaxdCommand
	upgrade   control.UpgradePaxdCommand
	candidate updater.Candidate
	cancel    context.CancelFunc
	committed bool
}

type lifecycleCoordinator struct {
	mu        sync.Mutex
	bootID    string
	intents   map[string]*maintenanceIntent
	activeID  string
	committed bool
	exit      chan ExitRequest
	activity  maintenanceActivity
	updater   maintenanceUpdater
	commands  commandCompleter
}

func newLifecycleCoordinator(bootID string) *lifecycleCoordinator {
	return &lifecycleCoordinator{
		bootID: bootID, intents: make(map[string]*maintenanceIntent), exit: make(chan ExitRequest, 1),
	}
}

func (c *lifecycleCoordinator) Configure(
	activity maintenanceActivity,
	update maintenanceUpdater,
	commands commandCompleter,
) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.activity = activity
	c.updater = update
	c.commands = commands
	c.mu.Unlock()
}

func (c *lifecycleCoordinator) BootID() string {
	if c == nil {
		return ""
	}
	return c.bootID
}

func (c *lifecycleCoordinator) DaemonPhase() string {
	if c == nil {
		return "running"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	intent := c.intents[c.activeID]
	if intent == nil {
		return "running"
	}
	switch intent.phase {
	case "draining":
		return "draining"
	case "activating", "shutdown_committed":
		return "stopping"
	default:
		return "running"
	}
}
func (c *lifecycleCoordinator) ScheduleRestart(
	commandID string,
	command control.RestartPaxdCommand,
) error {
	if strings.TrimSpace(commandID) == "" {
		return errors.New("maintenance command id is required")
	}
	if command.Mode == "" {
		command.Mode = control.PaxdRestartImmediate
	}
	return c.schedule(&maintenanceIntent{
		commandID: commandID, kind: "restart", phase: "awaiting_ack", restart: command,
	})
}

func (c *lifecycleCoordinator) ScheduleUpgrade(
	commandID string,
	command control.UpgradePaxdCommand,
) error {
	if strings.TrimSpace(commandID) == "" {
		return errors.New("maintenance command id is required")
	}
	if command.Mode == "" {
		command.Mode = control.PaxdUpgradeWhenIdle
	}
	return c.schedule(&maintenanceIntent{
		commandID: commandID, kind: "upgrade", phase: "awaiting_ack", upgrade: command,
	})
}

func (c *lifecycleCoordinator) schedule(intent *maintenanceIntent) error {
	if c == nil {
		return errors.New("paxd lifecycle is not configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.intents[intent.commandID]; existing != nil {
		return nil
	}
	if c.committed || c.activeID != "" {
		return control.ControlError{
			Code: control.ErrCodeConflict, Message: "another paxd maintenance operation is in progress",
			Target: "command_id",
		}
	}
	c.intents[intent.commandID] = intent
	c.activeID = intent.commandID
	return nil
}

func (c *lifecycleCoordinator) ConfirmAckDelivered(commandID string) {
	if c == nil || strings.TrimSpace(commandID) == "" {
		return
	}
	c.mu.Lock()
	intent := c.intents[commandID]
	if intent == nil || intent.phase != "awaiting_ack" || intent.committed {
		c.mu.Unlock()
		return
	}
	intent.phase = "acknowledged"
	c.mu.Unlock()
	c.persist(intent, control.CommandStatusReceived, "", "")
	go c.run(intent)
}

func (c *lifecycleCoordinator) Cancel(commandID string) error {
	if c == nil {
		return errors.New("paxd lifecycle is not configured")
	}
	c.mu.Lock()
	intent := c.intents[commandID]
	if intent == nil {
		c.mu.Unlock()
		return control.ControlError{Code: control.ErrCodeNotFound, Message: "maintenance command not found"}
	}
	if intent.committed || intent.phase == "activating" || intent.phase == "shutdown_committed" {
		c.mu.Unlock()
		return control.ControlError{Code: control.ErrCodeConflict, Message: "maintenance can no longer be canceled"}
	}
	intent.phase = "canceled"
	cancel := intent.cancel
	candidate := intent.candidate
	if c.activeID == commandID {
		c.activeID = ""
	}
	activity := c.activity
	update := c.updater
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if activity != nil {
		activity.ClearDrain(commandID)
	}
	if update != nil {
		update.Cleanup(candidate)
	}
	c.persist(intent, control.CommandStatusFailed, "maintenance_canceled", "maintenance was canceled")
	return nil
}

func (c *lifecycleCoordinator) run(intent *maintenanceIntent) {
	if intent.kind == "restart" {
		if intent.restart.Mode == control.PaxdRestartImmediate {
			c.commitRestart(intent, true)
			return
		}
		c.waitForCutover(intent, false)
		return
	}
	c.stageUpgrade(intent)
}

func (c *lifecycleCoordinator) stageUpgrade(intent *maintenanceIntent) {
	c.mu.Lock()
	update := c.updater
	if intent.phase == "canceled" {
		c.mu.Unlock()
		return
	}
	intent.phase = "staging"
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceStageTimeout)
	intent.cancel = cancel
	c.mu.Unlock()
	c.persist(intent, control.CommandStatusReceived, "", "")
	if update == nil {
		cancel()
		c.fail(intent, "updater_unavailable", "paxd updater is not configured")
		return
	}
	candidate, err := update.Stage(ctx, updater.Request{
		CommandID: intent.commandID, Version: intent.upgrade.Version, Tag: intent.upgrade.Tag,
	})
	cancel()
	if err != nil {
		c.fail(intent, "upgrade_stage_failed", err.Error())
		return
	}
	c.mu.Lock()
	if intent.phase == "canceled" {
		c.mu.Unlock()
		update.Cleanup(candidate)
		return
	}
	intent.candidate = candidate
	intent.phase = "staged"
	intent.cancel = nil
	c.mu.Unlock()
	c.persist(intent, control.CommandStatusReceived, "", "")
	switch intent.upgrade.Mode {
	case control.PaxdUpgradeImmediate:
		c.commitUpgrade(intent, true)
	case control.PaxdUpgradeOpportunistic:
		c.waitForCutover(intent, true)
	default:
		c.waitForCutover(intent, false)
	}
}

func (c *lifecycleCoordinator) waitForCutover(intent *maintenanceIntent, opportunistic bool) {
	idleGrace := defaultMaintenanceIdleGrace
	drainTimeout := defaultMaintenanceDrainTimeout
	force := false
	if intent.kind == "restart" {
		idleGrace = durationOrDefault(intent.restart.IdleGraceSeconds, idleGrace)
		drainTimeout = durationOrDefault(intent.restart.DrainTimeoutSeconds, drainTimeout)
		force = intent.restart.ForceAtDeadline
	} else {
		idleGrace = durationOrDefault(intent.upgrade.IdleGraceSeconds, idleGrace)
		drainTimeout = durationOrDefault(intent.upgrade.DrainTimeoutSeconds, drainTimeout)
		force = intent.upgrade.ForceAtDeadline
	}
	c.mu.Lock()
	activity := c.activity
	if intent.phase == "canceled" {
		c.mu.Unlock()
		return
	}
	if opportunistic {
		intent.phase = "pending_idle"
	} else {
		intent.phase = "draining"
	}
	c.mu.Unlock()
	deadline := time.Now().Add(drainTimeout)
	if !opportunistic && activity != nil {
		activity.BeginDrain(runtimes.DaemonDrainLease{
			CommandID: intent.commandID, BootID: c.bootID, ExpiresAt: deadline,
		})
	}
	c.persist(intent, control.CommandStatusReceived, "", "")
	var changes <-chan struct{}
	unsubscribe := func() {}
	if activity != nil {
		changes, unsubscribe = activity.SubscribeActivity()
	}
	defer unsubscribe()
	ticker := time.NewTicker(maintenancePollInterval)
	defer ticker.Stop()
	deadlineTimer := time.NewTimer(drainTimeout)
	defer deadlineTimer.Stop()
	var idleSince time.Time
	for {
		if c.intentCanceled(intent) {
			return
		}
		idle := activity == nil || activity.ActivitySnapshot().Idle()
		if idle {
			if idleSince.IsZero() {
				idleSince = time.Now()
			}
			if time.Since(idleSince) >= idleGrace && c.closeForCutover(intent, false) {
				if intent.kind == "upgrade" {
					c.commitUpgrade(intent, false)
				} else {
					c.commitRestart(intent, false)
				}
				return
			}
		} else {
			idleSince = time.Time{}
		}
		select {
		case <-deadlineTimer.C:
			if force && c.closeForCutover(intent, true) {
				if intent.kind == "upgrade" {
					c.commitUpgrade(intent, true)
				} else {
					c.commitRestart(intent, true)
				}
				return
			}
			c.fail(intent, "drain_expired", "maintenance drain deadline expired")
			return
		case <-ticker.C:
		case <-changes:
		}
	}
}

func (c *lifecycleCoordinator) closeForCutover(intent *maintenanceIntent, force bool) bool {
	c.mu.Lock()
	activity := c.activity
	c.mu.Unlock()
	return activity == nil || activity.CloseForCutover(intent.commandID, force)
}

func (c *lifecycleCoordinator) commitRestart(intent *maintenanceIntent, force bool) {
	if !c.closeForCutover(intent, force) {
		return
	}
	c.commitExit(intent)
}

func (c *lifecycleCoordinator) commitUpgrade(intent *maintenanceIntent, force bool) {
	if !c.closeForCutover(intent, force) {
		return
	}
	c.mu.Lock()
	update := c.updater
	intent.phase = "activating"
	candidate := intent.candidate
	c.mu.Unlock()
	c.persist(intent, control.CommandStatusReceived, "", "")
	_, err := update.Activate(candidate, c.bootID)
	if err != nil {
		var committed updater.ActivationCommittedError
		if !errors.As(err, &committed) {
			c.fail(intent, "upgrade_activation_failed", err.Error())
			return
		}
		log.Printf("[paxd] upgrade activation committed with durability warning command_id=%s: %v", intent.commandID, err)
	}
	c.commitExit(intent)
}

func (c *lifecycleCoordinator) commitExit(intent *maintenanceIntent) {
	c.mu.Lock()
	if intent.phase == "canceled" || intent.committed || c.committed {
		c.mu.Unlock()
		return
	}
	intent.phase = "shutdown_committed"
	intent.committed = true
	c.committed = true
	grace := defaultRemoteRestartGrace
	reason := intent.restart.Reason
	if intent.kind == "upgrade" {
		reason = intent.upgrade.Reason
		grace = durationOrDefault(intent.upgrade.ShutdownGraceSeconds, grace)
	} else {
		grace = durationOrDefault(intent.restart.ShutdownGraceSeconds, grace)
	}
	c.mu.Unlock()
	c.persist(intent, control.CommandStatusApplied, "", "")
	c.exit <- ExitRequest{CommandID: intent.commandID, Reason: reason, ShutdownGrace: grace}
}

func (c *lifecycleCoordinator) fail(intent *maintenanceIntent, code, message string) {
	c.mu.Lock()
	if intent.committed || intent.phase == "canceled" {
		c.mu.Unlock()
		return
	}
	intent.phase = "failed"
	activity := c.activity
	update := c.updater
	candidate := intent.candidate
	if c.activeID == intent.commandID {
		c.activeID = ""
	}
	c.mu.Unlock()
	if activity != nil {
		activity.ClearDrain(intent.commandID)
	}
	if update != nil {
		update.Cleanup(candidate)
	}
	c.persist(intent, control.CommandStatusFailed, code, message)
}

func (c *lifecycleCoordinator) intentCanceled(intent *maintenanceIntent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return intent.phase == "canceled"
}

func (c *lifecycleCoordinator) persist(
	intent *maintenanceIntent,
	status control.CommandStatus,
	errorCode string,
	errorMessage string,
) {
	c.mu.Lock()
	commands := c.commands
	resultJSON := c.resultJSONLocked(intent)
	phase := intent.phase
	c.mu.Unlock()
	if commands == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := commands.CompleteCommand(ctx, intent.commandID, control.CommandCompletion{
		Status: status, ResultJSON: resultJSON, ErrorCode: errorCode, ErrorMessage: errorMessage,
	}); err != nil {
		log.Printf("[paxd] persist maintenance command_id=%s phase=%s failed: %v", intent.commandID, phase, err)
	}
}

func (c *lifecycleCoordinator) resultJSONLocked(intent *maintenanceIntent) string {
	var value any
	if intent.kind == "upgrade" {
		value = control.PaxdUpgradeResult{
			RequestedBootID: c.bootID, TargetVersion: intent.upgrade.Version, Phase: intent.phase,
			StagedPath: intent.candidate.Path, SHA256: intent.candidate.SHA256,
		}
	} else {
		value = control.PaxdRestartResult{RequestedBootID: c.bootID, Phase: intent.phase}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func durationOrDefault(seconds int, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func (c *lifecycleCoordinator) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprintf("boot=%s active=%s committed=%t", c.bootID, c.activeID, c.committed)
}
