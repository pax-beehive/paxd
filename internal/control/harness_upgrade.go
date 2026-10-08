package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"
)

type UpgradeHarnessCommand struct {
	Harness      string `json:"harness"`
	Component    string `json:"component"`
	Version      string `json:"version"`
	ConnectionID string `json:"connection_id,omitempty"`
}

var harnessTargetVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)

func (c UpgradeHarnessCommand) Validate() error {
	if c.Harness != "claude-code" && c.Harness != "codex" && c.Harness != "pi" {
		return invalid("upgrade_harness.harness", "harness must be claude-code, codex or pi")
	}
	if c.Component != "acp" && c.Component != "cli" {
		return invalid("upgrade_harness.component", "component must be cli or acp")
	}
	if !harnessTargetVersion.MatchString(c.Version) {
		return invalid("upgrade_harness.version", "an exact semantic target version is required")
	}
	if (c.Component == "acp") != (c.ConnectionID != "") {
		return invalid("upgrade_harness.connection_id", "connection_id is required only for ACP adapter upgrades")
	}
	return nil
}

type HarnessInstaller interface {
	UpgradeHarness(context.Context, string, string, *UpgradeHarnessCommand, func(string)) (json.RawMessage, error)
}

func (s *ControlService) confirmHarnessUpgrade(rec CommandRecord) {
	if s.harnessInstaller == nil {
		return
	}
	if _, loaded := s.harnessUpgradeActive.LoadOrStore(rec.CommandID, true); loaded {
		return
	}
	go func() {
		defer s.harnessUpgradeActive.Delete(rec.CommandID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		latest, err := s.store.GetCommandRecord(ctx, rec.CommandID)
		if err != nil || latest.Status != CommandStatusReceived {
			return
		}
		var cmd Command
		var result json.RawMessage
		if err = json.Unmarshal([]byte(rec.PayloadJSON), &cmd); err == nil && cmd.UpgradeHarness != nil {
			result, err = s.harnessInstaller.UpgradeHarness(ctx, rec.CommandID, rec.Source.RemoteID, cmd.UpgradeHarness, func(phase string) {
				s.completeHarnessUpgrade(ctx, rec.CommandID, CommandStatusReceived, phase, nil, nil)
			})
		} else {
			err = fmt.Errorf("invalid persisted harness upgrade")
		}
		status, phase := CommandStatusApplied, "verified"
		if err != nil {
			status, phase = CommandStatusFailed, "failed"
		}
		finalCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		s.completeHarnessUpgrade(finalCtx, rec.CommandID, status, phase, result, err)
	}()
}

func (s *ControlService) completeHarnessUpgrade(ctx context.Context, id string, status CommandStatus, phase string, result json.RawMessage, upgradeErr error) {
	raw, _ := json.Marshal(struct {
		Phase   string          `json:"phase"`
		Harness json.RawMessage `json:"harness,omitempty"`
	}{phase, result})
	completion := CommandCompletion{Status: status, ResultJSON: string(raw)}
	if upgradeErr != nil {
		completion.ErrorCode = "harness_upgrade_failed"
		completion.ErrorMessage = upgradeErr.Error()
	}
	if err := s.store.CompleteCommand(ctx, id, completion); err != nil {
		log.Printf("Persist harness upgrade command %s: %v.", id, err)
	}
}
