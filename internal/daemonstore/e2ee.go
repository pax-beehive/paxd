package daemonstore

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrStaleE2EEConnection = errors.New("stale E2EE connection epoch")

func (s *Store) BeginE2EECommand(
	ctx context.Context,
	agentID string,
	commandID string,
	connectionEpoch int64,
) (bool, error) {
	accepted := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var fence E2EEAgentFence
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&fence, "agent_id = ?", agentID).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			fence = E2EEAgentFence{AgentID: agentID, ConnectionEpoch: connectionEpoch, UpdatedAt: s.now().UTC()}
			if err := tx.Create(&fence).Error; err != nil {
				return err
			}
		case err != nil:
			return err
		case connectionEpoch < fence.ConnectionEpoch:
			return ErrStaleE2EEConnection
		case connectionEpoch > fence.ConnectionEpoch:
			if err := tx.Model(&fence).Updates(map[string]any{
				"connection_epoch": connectionEpoch,
				"updated_at":       s.now().UTC(),
			}).Error; err != nil {
				return err
			}
		}

		var existing E2EECommandReceipt
		err = tx.First(&existing, "agent_id = ? AND command_id = ?", agentID, commandID).Error
		switch {
		case err == nil:
			accepted = existing.CompletedAt == nil
			return nil
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}

		receipt := E2EECommandReceipt{
			AgentID: agentID, CommandID: commandID,
			ConnectionEpoch: connectionEpoch, CreatedAt: s.now().UTC(),
		}
		if err := tx.Create(&receipt).Error; err != nil {
			return err
		}
		accepted = true
		return nil
	})
	return accepted, err
}

func (s *Store) CompleteE2EECommand(ctx context.Context, agentID string, commandID string) error {
	now := s.now().UTC()
	result := s.db.WithContext(ctx).Model(&E2EECommandReceipt{}).
		Where("agent_id = ? AND command_id = ? AND completed_at IS NULL", agentID, commandID).
		Update("completed_at", &now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) PruneE2EECommandReceipts(ctx context.Context, before time.Time) (int64, error) {
	result := s.db.WithContext(ctx).
		Where("completed_at IS NOT NULL AND created_at < ?", before.UTC()).
		Delete(&E2EECommandReceipt{})
	return result.RowsAffected, result.Error
}
