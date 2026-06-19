package store

import (
	"database/sql"
	"fmt"
	"strings"
)

const (
	TransportStreamManagerToPaxd = "manager_to_paxd"
	TransportStreamPaxdToManager = "paxd_to_manager"

	// Inbound frames were received by this paxd process from pax-manager.
	// Outbound frames were produced locally by the ACP process and must be
	// delivered to pax-manager.
	TransportDirectionInbound  = "inbound"
	TransportDirectionOutbound = "outbound"

	// Outbound lifecycle:
	//   pending: paxd read a payload from ACP stdout and durably recorded it,
	//     but has not successfully written it to the manager WebSocket.
	//     Owned by the outbound sender.
	//   sent: paxd successfully wrote the frame to the WebSocket. This does not
	//     mean manager received or persisted it. Owned by the ACK handler, which
	//     will either mark it acked or leave it for reconnect replay.
	//   acked: manager acknowledged the frame after durable receive/apply on its
	//     side. Owned by cleanup after the retention window.
	//
	// Inbound lifecycle:
	//   received: paxd received a manager->paxd data frame and committed it to
	//     SQLite. Paxd may ACK manager at this point, but the payload may not
	//     have reached ACP stdin yet. Owned by the inbound dispatcher.
	//   applied: paxd successfully wrote the payload to ACP stdin. This does
	//     not mean the agent finished the business operation, only that the
	//     frame was handed to the local ACP process. Owned by cleanup after the
	//     retention window.
	//
	// Shared terminal/problem state:
	//   failed: paxd could not send, dispatch, or parse the frame. Failed frames
	//     are owned by retry/dead-letter handling and are not removed by normal
	//     cleanup until explicitly resolved.
	TransportStatusPending  = "pending"
	TransportStatusSent     = "sent"
	TransportStatusAcked    = "acked"
	TransportStatusReceived = "received"
	TransportStatusApplied  = "applied"
	TransportStatusFailed   = "failed"
)

// TransportFrame is one durable frame in the local reliable transport journal.
// PayloadJSON is the raw ACP JSON-RPC payload, not the tunnel envelope.
type TransportFrame struct {
	ID             int64
	AgentID        string
	Stream         string
	Seq            int64
	LocalDirection string
	PayloadJSON    string
	Status         string
	Error          string
	RetryCount     int
	CreatedAt      string
	UpdatedAt      string
	SentAt         string
	ReceivedAt     string
	AckedAt        string
	AppliedAt      string
}

// SaveTransportFrame inserts a durable transport frame.
func (s *Store) SaveTransportFrame(frame *TransportFrame) error {
	if frame.AgentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	if frame.Stream == "" {
		return fmt.Errorf("stream is required")
	}
	if frame.Seq <= 0 {
		return fmt.Errorf("seq must be positive")
	}
	if frame.LocalDirection == "" {
		return fmt.Errorf("local_direction is required")
	}
	if frame.PayloadJSON == "" {
		return fmt.Errorf("payload_json is required")
	}
	if frame.Status == "" {
		frame.Status = defaultTransportStatus(frame.LocalDirection)
	}
	now := nowUTC()
	if frame.CreatedAt == "" {
		frame.CreatedAt = now
	}
	if frame.UpdatedAt == "" {
		frame.UpdatedAt = now
	}
	if frame.Status == TransportStatusReceived && frame.ReceivedAt == "" {
		frame.ReceivedAt = now
	}

	res, err := s.db.Exec(`
		INSERT INTO transport_journal (
			agent_id, stream, seq, local_direction, payload_json, status, error,
			retry_count, created_at, updated_at, sent_at, received_at, acked_at, applied_at
		)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))
	`, frame.AgentID, frame.Stream, frame.Seq, frame.LocalDirection, frame.PayloadJSON,
		frame.Status, frame.Error, frame.RetryCount, frame.CreatedAt, frame.UpdatedAt,
		frame.SentAt, frame.ReceivedAt, frame.AckedAt, frame.AppliedAt)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err == nil {
		frame.ID = id
	}
	return nil
}

// SaveTransportFrameIfAbsent inserts frame unless the same agent/stream/seq/direction
// already exists. It returns true when a new row was inserted.
func (s *Store) SaveTransportFrameIfAbsent(frame *TransportFrame) (bool, error) {
	if err := validateTransportFrame(frame); err != nil {
		return false, err
	}
	if frame.Status == "" {
		frame.Status = defaultTransportStatus(frame.LocalDirection)
	}
	now := nowUTC()
	if frame.CreatedAt == "" {
		frame.CreatedAt = now
	}
	if frame.UpdatedAt == "" {
		frame.UpdatedAt = now
	}
	if frame.Status == TransportStatusReceived && frame.ReceivedAt == "" {
		frame.ReceivedAt = now
	}

	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO transport_journal (
			agent_id, stream, seq, local_direction, payload_json, status, error,
			retry_count, created_at, updated_at, sent_at, received_at, acked_at, applied_at
		)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))
	`, frame.AgentID, frame.Stream, frame.Seq, frame.LocalDirection, frame.PayloadJSON,
		frame.Status, frame.Error, frame.RetryCount, frame.CreatedAt, frame.UpdatedAt,
		frame.SentAt, frame.ReceivedAt, frame.AckedAt, frame.AppliedAt)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// NextTransportSeq returns the next sequence number for an agent stream and local direction.
func (s *Store) NextTransportSeq(agentID string, stream string, direction string) (int64, error) {
	row := s.db.QueryRow(`
		SELECT COALESCE(MAX(seq), 0) + 1
		FROM transport_journal
		WHERE agent_id = ? AND stream = ? AND local_direction = ?
	`, agentID, stream, direction)
	var next int64
	if err := row.Scan(&next); err != nil {
		return 0, err
	}
	return next, nil
}

// GetTransportFrame returns a single durable frame by its identity.
func (s *Store) GetTransportFrame(
	agentID string,
	stream string,
	seq int64,
	direction string,
) (*TransportFrame, error) {
	row := s.db.QueryRow(`
		SELECT `+transportFrameColumns+`
		FROM transport_journal
		WHERE agent_id = ? AND stream = ? AND seq = ? AND local_direction = ?
	`, agentID, stream, seq, direction)
	frame, err := scanTransportFrame(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return frame, err
}

// ListTransportFrames returns frames for an agent stream/direction filtered by status.
func (s *Store) ListTransportFrames(
	agentID string,
	stream string,
	direction string,
	statuses []string,
	limit int,
) ([]TransportFrame, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args := []any{agentID, stream, direction}
	statusSQL := ""
	if len(statuses) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(statuses)), ",")
		statusSQL = " AND status IN (" + placeholders + ")"
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT `+transportFrameColumns+`
		FROM transport_journal
		WHERE agent_id = ? AND stream = ? AND local_direction = ?`+statusSQL+`
		ORDER BY seq
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var frames []TransportFrame
	for rows.Next() {
		frame, err := scanTransportFrame(rows)
		if err != nil {
			return nil, err
		}
		frames = append(frames, *frame)
	}
	return frames, rows.Err()
}

// UpdateTransportFrameStatus advances one frame and stamps the matching status time.
func (s *Store) UpdateTransportFrameStatus(
	agentID string,
	stream string,
	seq int64,
	direction string,
	status string,
	errMsg string,
) error {
	now := nowUTC()
	column := timestampColumnForTransportStatus(status)
	query := `
		UPDATE transport_journal
		SET status = ?, error = NULLIF(?, ''), updated_at = ?`
	args := []any{status, errMsg, now}
	if column != "" {
		query += `, ` + column + ` = ?`
		args = append(args, now)
	}
	query += `
		WHERE agent_id = ? AND stream = ? AND seq = ? AND local_direction = ?`
	args = append(args, agentID, stream, seq, direction)

	_, err := s.db.Exec(query, args...)
	return err
}

// AckOutboundTransportFrames marks all outbound frames through seq as acked.
func (s *Store) AckOutboundTransportFrames(agentID string, stream string, throughSeq int64) error {
	now := nowUTC()
	_, err := s.db.Exec(`
		UPDATE transport_journal
		SET status = ?, acked_at = ?, updated_at = ?
		WHERE agent_id = ? AND stream = ? AND local_direction = ?
			AND seq <= ? AND status != ?
	`, TransportStatusAcked, now, now, agentID, stream, TransportDirectionOutbound, throughSeq, TransportStatusAcked)
	return err
}

// DeleteCompletedTransportFrames deletes completed frames older than cutoff.
func (s *Store) DeleteCompletedTransportFrames(cutoff string, limit int) (int64, error) {
	// TODO: Wire this into a low-priority maintenance worker. Only acked
	// outbound frames and applied inbound frames are eligible; pending/sent/
	// received/failed rows must remain replayable/debuggable.
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.Query(`
		SELECT id
		FROM transport_journal
		WHERE status IN (?, ?) AND updated_at < ?
		ORDER BY id
		LIMIT ?
	`, TransportStatusAcked, TransportStatusApplied, cutoff, limit)
	if err != nil {
		return 0, err
	}
	var ids []any
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	res, err := s.db.Exec(`DELETE FROM transport_journal WHERE id IN (`+placeholders+`)`, ids...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const transportFrameColumns = `
	id, agent_id, stream, seq, local_direction, payload_json, status,
	COALESCE(error, ''), retry_count, created_at, updated_at,
	COALESCE(sent_at, ''), COALESCE(received_at, ''), COALESCE(acked_at, ''),
	COALESCE(applied_at, '')`

type transportFrameScanner interface {
	Scan(dest ...any) error
}

func scanTransportFrame(scanner transportFrameScanner) (*TransportFrame, error) {
	var frame TransportFrame
	err := scanner.Scan(
		&frame.ID,
		&frame.AgentID,
		&frame.Stream,
		&frame.Seq,
		&frame.LocalDirection,
		&frame.PayloadJSON,
		&frame.Status,
		&frame.Error,
		&frame.RetryCount,
		&frame.CreatedAt,
		&frame.UpdatedAt,
		&frame.SentAt,
		&frame.ReceivedAt,
		&frame.AckedAt,
		&frame.AppliedAt,
	)
	if err != nil {
		return nil, err
	}
	return &frame, nil
}

func validateTransportFrame(frame *TransportFrame) error {
	if frame.AgentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	if frame.Stream == "" {
		return fmt.Errorf("stream is required")
	}
	if frame.Seq <= 0 {
		return fmt.Errorf("seq must be positive")
	}
	if frame.LocalDirection == "" {
		return fmt.Errorf("local_direction is required")
	}
	if frame.PayloadJSON == "" {
		return fmt.Errorf("payload_json is required")
	}
	return nil
}

func defaultTransportStatus(direction string) string {
	if direction == TransportDirectionInbound {
		return TransportStatusReceived
	}
	return TransportStatusPending
}

func timestampColumnForTransportStatus(status string) string {
	switch status {
	case TransportStatusSent:
		return "sent_at"
	case TransportStatusAcked:
		return "acked_at"
	case TransportStatusReceived:
		return "received_at"
	case TransportStatusApplied:
		return "applied_at"
	default:
		return ""
	}
}
