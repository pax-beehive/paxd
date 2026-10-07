package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/pax-beehive/paxd/internal/paxlinstall"
)

type UpgradePaxlCommand struct {
	Version string `json:"version"`
	Tag     string `json:"tag,omitempty"`
}

var paxlVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

func (c UpgradePaxlCommand) Validate() error {
	if !paxlVersionPattern.MatchString(c.Version) {
		return invalid("upgrade_paxl.version", "explicit semantic target version is required")
	}
	if c.Tag != "" && c.Tag != "stable" && c.Tag != "latest" {
		return invalid("upgrade_paxl.tag", "tag must be stable or latest")
	}
	return nil
}

type PaxlInstaller interface {
	Upgrade(context.Context, string, string, string, string, func(string)) (paxlinstall.Observation, error)
}

func (s *ControlService) confirmPaxlUpgrade(rec CommandRecord) {
	if s.paxlInstaller == nil {
		return
	}
	if _, loaded := s.paxlUpgradeActive.LoadOrStore(rec.CommandID, true); loaded {
		return
	}
	go func() {
		defer s.paxlUpgradeActive.Delete(rec.CommandID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		// Re-read under the worker lock: duplicate ACKs can arrive after completion.
		latest, err := s.store.GetCommandRecord(ctx, rec.CommandID)
		if err != nil || latest.Status != CommandStatusReceived {
			return
		}
		var cmd Command
		if err = json.Unmarshal([]byte(rec.PayloadJSON), &cmd); err != nil || cmd.UpgradePaxl == nil {
			s.completePaxlUpgrade(ctx, rec.CommandID, CommandStatusFailed, "failed", paxlinstall.Observation{}, fmt.Errorf("invalid persisted paxl upgrade"))
			return
		}
		observation, err := s.paxlInstaller.Upgrade(ctx, rec.CommandID, rec.Source.RemoteID, cmd.UpgradePaxl.Version, cmd.UpgradePaxl.Tag, func(phase string) {
			s.completePaxlUpgrade(ctx, rec.CommandID, CommandStatusReceived, phase, paxlinstall.Observation{}, nil)
		})
		status, phase := CommandStatusApplied, "verified"
		if err != nil {
			status, phase = CommandStatusFailed, "failed"
		}
		// Persist even when the execution timeout has expired.
		finalCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		s.completePaxlUpgrade(finalCtx, rec.CommandID, status, phase, observation, err)
	}()
}
func (s *ControlService) completePaxlUpgrade(ctx context.Context, id string, status CommandStatus, phase string, observation paxlinstall.Observation, upgradeErr error) {
	result, _ := json.Marshal(struct {
		Phase string                  `json:"phase"`
		Paxl  paxlinstall.Observation `json:"paxl"`
	}{phase, observation})
	completion := CommandCompletion{Status: status, ResultJSON: string(result)}
	if upgradeErr != nil {
		completion.ErrorCode = "paxl_upgrade_failed"
		completion.ErrorMessage = upgradeErr.Error()
	}
	if err := s.store.CompleteCommand(ctx, id, completion); err != nil {
		log.Printf("Persist paxl upgrade command %s: %v.", id, err)
	}
}
