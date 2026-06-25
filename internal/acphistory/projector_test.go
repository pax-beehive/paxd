package acphistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

func TestProjectOutboundAggregatesDeltasIntoOnePart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "paxd.db")
	history := openTestHistory(t, dbPath)

	first := json.RawMessage(`{
		"jsonrpc":"2.0",
		"method":"session/update",
		"params":{
			"sessionId":"sess-1",
			"update":{
				"sessionUpdate":"agent_message_chunk",
				"content":{"type":"text","text":"h"}
			}
		}
	}`)
	second := json.RawMessage(`{
		"jsonrpc":"2.0",
		"method":"session/update",
		"params":{
			"sessionId":"sess-1",
			"update":{
				"sessionUpdate":"agent_message_chunk",
				"content":{"type":"text","text":"i"}
			}
		}
	}`)
	thought := json.RawMessage(`{
		"jsonrpc":"2.0",
		"method":"session/update",
		"params":{
			"sessionId":"sess-1",
			"update":{
				"sessionUpdate":"agent_thought_chunk",
				"content":{"type":"text","text":"thinking"}
			}
		}
	}`)
	rpcResponse := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
	if err := ProjectOutbound(context.Background(), history, "agent-1", 1, first); err != nil {
		t.Fatalf("project first delta: %v", err)
	}
	if err := ProjectOutbound(context.Background(), history, "agent-1", 2, second); err != nil {
		t.Fatalf("project second delta: %v", err)
	}
	if err := ProjectOutbound(context.Background(), history, "agent-1", 3, thought); err != nil {
		t.Fatalf("project thought delta: %v", err)
	}
	if err := ProjectOutbound(context.Background(), history, "agent-1", 4, rpcResponse); err != nil {
		t.Fatalf("project rpc response: %v", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite for assertion: %v", err)
	}
	defer db.Close()
	var text string
	var count int
	if err := db.QueryRow(`
		SELECT message_parts.text
		FROM message_parts
		JOIN messages ON messages.message_id = message_parts.message_id
		WHERE messages.message_type = 'agent_message_chunk'
	`).Scan(&text); err != nil {
		t.Fatalf("read projected message chunk: %v", err)
	}
	if text != "hi" {
		t.Fatalf("projected message text = %q, want hi", text)
	}
	if err := db.QueryRow(`
		SELECT message_parts.text
		FROM message_parts
		JOIN messages ON messages.message_id = message_parts.message_id
		WHERE messages.message_type = 'agent_thought_chunk'
	`).Scan(&text); err != nil {
		t.Fatalf("read projected thought chunk: %v", err)
	}
	if text != "thinking" {
		t.Fatalf("projected thought text = %q, want thinking", text)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM message_parts`).Scan(&count); err != nil {
		t.Fatalf("count projected parts: %v", err)
	}
	if count != 2 {
		t.Fatalf("projected parts = %d, want 2", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE message_id NOT LIKE 'msg_%'`).Scan(&count); err != nil {
		t.Fatalf("count projected message ids: %v", err)
	}
	if count != 0 {
		t.Fatalf("projected messages with non-msg ids = %d, want 0", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE logical_key NOT LIKE 'acp:%'`).Scan(&count); err != nil {
		t.Fatalf("count projected logical keys: %v", err)
	}
	if count != 0 {
		t.Fatalf("projected messages with non-acp logical keys = %d, want 0", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE COALESCE(raw_json, '') != ''`).Scan(&count); err != nil {
		t.Fatalf("count projected message raw json: %v", err)
	}
	if count != 0 {
		t.Fatalf("projected messages with raw_json = %d, want 0", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM message_parts WHERE COALESCE(payload_json, '') != ''`).Scan(&count); err != nil {
		t.Fatalf("count projected part payload json: %v", err)
	}
	if count != 0 {
		t.Fatalf("projected parts with payload_json = %d, want 0", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE message_id LIKE '%rpc:%'`).Scan(&count); err != nil {
		t.Fatalf("count rpc-derived messages: %v", err)
	}
	if count != 0 {
		t.Fatalf("rpc-derived messages = %d, want 0", count)
	}
}

func openTestHistory(t *testing.T, path string) *daemonstore.Store {
	t.Helper()
	history, err := daemonstore.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open test history: %v", err)
	}
	if err := history.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate test history: %v", err)
	}
	return history
}
