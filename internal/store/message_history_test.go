package store

import (
	"path/filepath"
	"testing"
)

func TestMessageHistoryAppendsTextDeltas(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "paxd.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	msg := &Message{
		MessageID:   "acp:agent-1:paxd_to_manager:sess-1:turn-1:assistant",
		AgentID:     "agent-1",
		SessionID:   "sess-1",
		Source:      MessageSourceACPTunnel,
		Direction:   MessageDirectionPaxdToManager,
		Role:        "assistant",
		MessageType: "message:delta",
		TurnID:      "turn-1",
		LogicalKey:  "acp:agent-1:paxd_to_manager:sess-1:turn-1:assistant",
		RawJSON:     `{"entityType":"message","eventType":"delta"}`,
	}
	if err := db.UpsertMessage(msg); err != nil {
		t.Fatalf("upsert message: %v", err)
	}
	if err := db.AppendMessagePartText(msg.MessageID, 0, "hel", `{"content":"hel"}`); err != nil {
		t.Fatalf("append first delta: %v", err)
	}
	if err := db.AppendMessagePartText(msg.MessageID, 0, "lo", `{"content":"lo"}`); err != nil {
		t.Fatalf("append second delta: %v", err)
	}
	var text string
	var count int
	if err := db.db.QueryRow(`
		SELECT text FROM message_parts WHERE message_id = ? AND part_index = 0
	`, msg.MessageID).Scan(&text); err != nil {
		t.Fatalf("read part: %v", err)
	}
	if text != "hello" {
		t.Fatalf("part text = %q, want hello", text)
	}
	if err := db.db.QueryRow(`
		SELECT COUNT(*) FROM message_parts WHERE message_id = ?
	`, msg.MessageID).Scan(&count); err != nil {
		t.Fatalf("count parts: %v", err)
	}
	if count != 1 {
		t.Fatalf("parts = %d, want 1", count)
	}
}
