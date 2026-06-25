package daemonstore

import (
	"context"
	"testing"
)

func TestMessageHistoryAppendsTextDeltas(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	msg := &Message{
		MessageID:   "acp:agent-1:paxd_to_manager:sess-1:turn-1:assistant",
		AgentID:     "agent-1",
		SessionID:   "sess-1",
		Source:      "acp_tunnel",
		Direction:   "paxd_to_manager",
		Role:        "assistant",
		MessageType: "message:delta",
		TurnID:      "turn-1",
		LogicalKey:  testStringPtr("acp:agent-1:paxd_to_manager:sess-1:turn-1:assistant"),
		RawJSON:     `{"entityType":"message","eventType":"delta"}`,
	}
	if err := store.UpsertMessage(ctx, msg); err != nil {
		t.Fatalf("upsert message: %v", err)
	}
	if msg.ID == 0 || msg.CreatedAt.IsZero() || msg.UpdatedAt.IsZero() {
		t.Fatalf("message timestamps/id not populated: %+v", msg)
	}
	if err := store.AppendMessagePartText(ctx, msg.MessageID, 0, "hel", `{"content":"hel"}`); err != nil {
		t.Fatalf("append first delta: %v", err)
	}
	if err := store.AppendMessagePartText(ctx, msg.MessageID, 0, "lo", `{"content":"lo"}`); err != nil {
		t.Fatalf("append second delta: %v", err)
	}
	var part MessagePart
	if err := store.DB().
		Where("message_id = ? AND part_index = ?", msg.MessageID, 0).
		First(&part).Error; err != nil {
		t.Fatalf("read part: %v", err)
	}
	if part.Text != "hello" {
		t.Fatalf("part text = %q, want hello", part.Text)
	}
	var count int64
	if err := store.DB().Model(&MessagePart{}).Where("message_id = ?", msg.MessageID).Count(&count).Error; err != nil {
		t.Fatalf("count parts: %v", err)
	}
	if count != 1 {
		t.Fatalf("parts = %d, want 1", count)
	}
}

func TestUpsertMessageMergesNonEmptyFields(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	msg := &Message{
		MessageID: "msg_1",
		AgentID:   "agent-1",
		Source:    "acp_tunnel",
		Direction: "paxd_to_manager",
		SessionID: "sess-1",
		Role:      "assistant",
	}
	if err := store.UpsertMessage(ctx, msg); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	createdAt := msg.CreatedAt

	update := &Message{
		MessageID:   "msg_1",
		AgentID:     "agent-1",
		Source:      "acp_tunnel",
		Direction:   "paxd_to_manager",
		Status:      "received",
		MessageType: "agent_message_chunk",
	}
	if err := store.UpsertMessage(ctx, update); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if update.SessionID != "sess-1" || update.Role != "assistant" {
		t.Fatalf("empty update overwrote existing fields: %+v", update)
	}
	if update.Status != "received" || update.MessageType != "agent_message_chunk" {
		t.Fatalf("non-empty update fields were not applied: %+v", update)
	}
	if !update.CreatedAt.Equal(createdAt) {
		t.Fatalf("created_at changed: got %s want %s", update.CreatedAt, createdAt)
	}
}

func testStringPtr(value string) *string {
	return &value
}
