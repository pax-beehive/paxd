package store

import (
	"database/sql"
	"fmt"
	"time"
)

const (
	MessageSourceACPTunnel = "acp_tunnel"

	MessageDirectionManagerToPaxd = "manager_to_paxd"
	MessageDirectionPaxdToManager = "paxd_to_manager"

	MessagePartText    = "text"
	MessagePartRawJSON = "raw_json"
)

func (s *Store) UpsertMessage(msg *Message) error {
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if msg.CreatedAt == "" {
		msg.CreatedAt = now
	}
	msg.UpdatedAt = now
	row := s.db.QueryRow(`
		INSERT INTO messages (
			message_id, agent_id, session_id, source, direction, role, status, message_type,
			parent_message_id, turn_id, response_id, logical_key, raw_json, created_at, updated_at
		)
		VALUES (?, ?, NULLIF(?, ''), ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
			NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			session_id = COALESCE(excluded.session_id, messages.session_id),
			role = COALESCE(excluded.role, messages.role),
			status = COALESCE(excluded.status, messages.status),
			message_type = COALESCE(excluded.message_type, messages.message_type),
			parent_message_id = COALESCE(excluded.parent_message_id, messages.parent_message_id),
			turn_id = COALESCE(excluded.turn_id, messages.turn_id),
			response_id = COALESCE(excluded.response_id, messages.response_id),
			raw_json = COALESCE(excluded.raw_json, messages.raw_json),
			updated_at = excluded.updated_at
		RETURNING id, message_id, agent_id, COALESCE(session_id, ''), source, direction,
			COALESCE(role, ''), COALESCE(status, ''), COALESCE(message_type, ''),
			COALESCE(parent_message_id, ''), COALESCE(turn_id, ''), COALESCE(response_id, ''),
			COALESCE(logical_key, ''), COALESCE(raw_json, ''), created_at, updated_at
	`, msg.MessageID, msg.AgentID, msg.SessionID, msg.Source, msg.Direction, msg.Role,
		msg.Status, msg.MessageType, msg.ParentMessageID, msg.TurnID, msg.ResponseID,
		msg.LogicalKey, msg.RawJSON, msg.CreatedAt, msg.UpdatedAt)
	return scanMessage(row, msg)
}

func (s *Store) UpsertMessagePart(part *MessagePart) error {
	if part.MessageID == "" {
		return fmt.Errorf("message_id is required")
	}
	if part.PartIndex < 0 {
		return fmt.Errorf("part_index must be non-negative")
	}
	if part.PartType == "" {
		return fmt.Errorf("part_type is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if part.CreatedAt == "" {
		part.CreatedAt = now
	}
	part.UpdatedAt = now
	row := s.db.QueryRow(`
		INSERT INTO message_parts (
			message_id, part_index, part_type, text, payload_json, artifact_uri, created_at, updated_at
		)
		VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)
		ON CONFLICT(message_id, part_index) DO UPDATE SET
			part_type = excluded.part_type,
			text = COALESCE(excluded.text, message_parts.text),
			payload_json = COALESCE(excluded.payload_json, message_parts.payload_json),
			artifact_uri = COALESCE(excluded.artifact_uri, message_parts.artifact_uri),
			updated_at = excluded.updated_at
		RETURNING id, message_id, part_index, part_type, COALESCE(text, ''),
			COALESCE(payload_json, ''), COALESCE(artifact_uri, ''), created_at, updated_at
	`, part.MessageID, part.PartIndex, part.PartType, part.Text, part.PayloadJSON,
		part.ArtifactURI, part.CreatedAt, part.UpdatedAt)
	return scanMessagePart(row, part)
}

func (s *Store) AppendMessagePartText(
	messageID string,
	partIndex int,
	delta string,
	payloadJSON string,
) error {
	if messageID == "" {
		return fmt.Errorf("message_id is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
		INSERT INTO message_parts (
			message_id, part_index, part_type, text, payload_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?)
		ON CONFLICT(message_id, part_index) DO UPDATE SET
			text = COALESCE(message_parts.text, '') || excluded.text,
			payload_json = COALESCE(excluded.payload_json, message_parts.payload_json),
			updated_at = excluded.updated_at
	`, messageID, partIndex, MessagePartText, delta, payloadJSON, now, now)
	return err
}

func scanMessage(row *sql.Row, msg *Message) error {
	return row.Scan(
		&msg.ID,
		&msg.MessageID,
		&msg.AgentID,
		&msg.SessionID,
		&msg.Source,
		&msg.Direction,
		&msg.Role,
		&msg.Status,
		&msg.MessageType,
		&msg.ParentMessageID,
		&msg.TurnID,
		&msg.ResponseID,
		&msg.LogicalKey,
		&msg.RawJSON,
		&msg.CreatedAt,
		&msg.UpdatedAt,
	)
}

func scanMessagePart(row *sql.Row, part *MessagePart) error {
	return row.Scan(
		&part.ID,
		&part.MessageID,
		&part.PartIndex,
		&part.PartType,
		&part.Text,
		&part.PayloadJSON,
		&part.ArtifactURI,
		&part.CreatedAt,
		&part.UpdatedAt,
	)
}
