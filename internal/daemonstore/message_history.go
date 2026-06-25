package daemonstore

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

const messagePartText = "text"

func (s *Store) UpsertMessage(ctx context.Context, msg *Message) error {
	if err := validateMessageForUpsert(msg); err != nil {
		return err
	}
	now := s.currentTime()
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
	}
	msg.UpdatedAt = now

	err := s.db.WithContext(ctx).Create(msg).Error
	if err == nil {
		return nil
	}
	if mapCreateErr(err) != ErrDuplicate {
		return err
	}

	var existing Message
	if err := s.db.WithContext(ctx).First(&existing, "message_id = ?", msg.MessageID).Error; err != nil {
		return mapGormErr(err)
	}
	mergeMessage(&existing, *msg)
	existing.UpdatedAt = now
	if err := s.db.WithContext(ctx).Save(&existing).Error; err != nil {
		return mapGormErr(err)
	}
	*msg = existing
	return nil
}

func (s *Store) UpsertMessagePart(ctx context.Context, part *MessagePart) error {
	if err := validateMessagePartForUpsert(part); err != nil {
		return err
	}
	now := s.currentTime()
	if part.CreatedAt.IsZero() {
		part.CreatedAt = now
	}
	part.UpdatedAt = now

	err := s.db.WithContext(ctx).Create(part).Error
	if err == nil {
		return nil
	}
	if mapCreateErr(err) != ErrDuplicate {
		return err
	}

	var existing MessagePart
	if err := s.db.WithContext(ctx).
		Where("message_id = ? AND part_index = ?", part.MessageID, part.PartIndex).
		First(&existing).Error; err != nil {
		return mapGormErr(err)
	}
	mergeMessagePart(&existing, *part)
	existing.UpdatedAt = now
	if err := s.db.WithContext(ctx).Save(&existing).Error; err != nil {
		return mapGormErr(err)
	}
	*part = existing
	return nil
}

func (s *Store) AppendMessagePartText(
	ctx context.Context,
	messageID string,
	partIndex int,
	delta string,
	payloadJSON string,
) error {
	if messageID == "" {
		return fmt.Errorf("message_id is required")
	}
	if partIndex < 0 {
		return fmt.Errorf("part_index must be non-negative")
	}
	now := s.currentTime()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var part MessagePart
		err := tx.Where("message_id = ? AND part_index = ?", messageID, partIndex).First(&part).Error
		if err != nil {
			if !isMissing(err) {
				return err
			}
			part = MessagePart{
				MessageID:   messageID,
				PartIndex:   partIndex,
				PartType:    messagePartText,
				Text:        delta,
				PayloadJSON: payloadJSON,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			return tx.Create(&part).Error
		}
		part.PartType = messagePartText
		part.Text += delta
		if payloadJSON != "" {
			part.PayloadJSON = payloadJSON
		}
		part.UpdatedAt = now
		return tx.Save(&part).Error
	})
}

func validateMessageForUpsert(msg *Message) error {
	if msg.MessageID == "" {
		return fmt.Errorf("message_id is required")
	}
	if msg.AgentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	if msg.Source == "" {
		return fmt.Errorf("source is required")
	}
	if msg.Direction == "" {
		return fmt.Errorf("direction is required")
	}
	return nil
}

func validateMessagePartForUpsert(part *MessagePart) error {
	if part.MessageID == "" {
		return fmt.Errorf("message_id is required")
	}
	if part.PartIndex < 0 {
		return fmt.Errorf("part_index must be non-negative")
	}
	if part.PartType == "" {
		return fmt.Errorf("part_type is required")
	}
	return nil
}

func mergeMessage(dst *Message, src Message) {
	if src.SessionID != "" {
		dst.SessionID = src.SessionID
	}
	if src.Role != "" {
		dst.Role = src.Role
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.MessageType != "" {
		dst.MessageType = src.MessageType
	}
	if src.ParentMessageID != "" {
		dst.ParentMessageID = src.ParentMessageID
	}
	if src.TurnID != "" {
		dst.TurnID = src.TurnID
	}
	if src.ResponseID != "" {
		dst.ResponseID = src.ResponseID
	}
	if src.RawJSON != "" {
		dst.RawJSON = src.RawJSON
	}
}

func mergeMessagePart(dst *MessagePart, src MessagePart) {
	dst.PartType = src.PartType
	if src.Text != "" {
		dst.Text = src.Text
	}
	if src.PayloadJSON != "" {
		dst.PayloadJSON = src.PayloadJSON
	}
	if src.ArtifactURI != "" {
		dst.ArtifactURI = src.ArtifactURI
	}
}
