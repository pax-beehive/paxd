package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
)

func TestOpenHardMigratesOldTransportJournalToReliableMQSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "paxd.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE transport_journal (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id        TEXT NOT NULL,
			stream          TEXT NOT NULL,
			seq             INTEGER NOT NULL,
			local_direction TEXT NOT NULL,
			payload_json    TEXT NOT NULL,
			status          TEXT NOT NULL,
			UNIQUE(agent_id, stream, seq, local_direction)
		);
		INSERT INTO transport_journal (
			agent_id, stream, seq, local_direction, payload_json, status
		) VALUES (
			'agent_legacy', 'paxd_to_manager', 1, 'outbound', '{"jsonrpc":"2.0"}', 'sent'
		);
	`); err != nil {
		_ = db.Close()
		t.Fatalf("seed old journal: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() after old journal seed error = %v", err)
	}
	defer s.Close()

	for _, column := range []string{"queue_id", "direction", "kind", "metadata_json", "error_message"} {
		ok, err := tableHasColumn(s.db, "transport_journal", column)
		if err != nil {
			t.Fatalf("tableHasColumn(%s) error = %v", column, err)
		}
		if !ok {
			t.Fatalf("transport_journal missing %s after migration", column)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transport_journal`).Scan(&count); err != nil {
		t.Fatalf("count migrated journal rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("migrated journal rows = %d, want 0 for hard migration", count)
	}
	if _, err := sqlstore.NewSQLite(s.DB(), sqlstore.WithTableName("transport_journal")); err != nil {
		t.Fatalf("open reliablemq sqlstore after migration: %v", err)
	}
}
