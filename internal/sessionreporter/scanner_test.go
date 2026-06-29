package sessionreporter

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultScannerMergesHermesSQLiteSessionsWhenACPSucceeds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	createReporterHermesStateDB(t, filepath.Join(home, ".hermes", "state.db"))
	insertReporterHermesSession(t, filepath.Join(home, ".hermes", "state.db"), "sess-local", "SQLite only", 1_780_000_010)
	command := fakeHermesACPCommand(t, `[{"sessionId":"sess-acp","nativeId":"sess-acp","title":"ACP only","updatedAt":"2026-06-01T20:26:40Z"}]`)

	sessions, err := DefaultScanner{Timeout: 5 * time.Second}.ListSessions(context.Background(), SessionScannerSpec{
		Harness: "hermes",
		Command: command,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, "hermes:sess-local", sessions[0].SessionID)
	assert.Equal(t, "SQLite only", sessions[0].Name)
	assert.Equal(t, "hermes:sess-acp", sessions[1].SessionID)
	assert.Equal(t, "ACP only", sessions[1].Name)
}

func TestDefaultScannerReadsHermesProfileStateDBFromCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".hermes")
	profileDB := filepath.Join(root, "ABC", "state.db")
	createReporterHermesStateDB(t, filepath.Join(root, "state.db"))
	createReporterHermesStateDB(t, profileDB)
	insertReporterHermesSession(t, filepath.Join(root, "state.db"), "sess-default", "Default profile", 1_780_000_010)
	insertReporterHermesSession(t, profileDB, "sess-profile", "ABC profile", 1_780_000_020)

	sessions, err := DefaultScanner{Timeout: 50 * time.Millisecond}.ListSessions(context.Background(), SessionScannerSpec{
		Command: []string{filepath.Join(t.TempDir(), "hermes"), "--profile", "ABC", "acp"},
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "hermes:sess-profile", sessions[0].SessionID)
	assert.Equal(t, "ABC profile", sessions[0].Name)
}

func TestDefaultScannerUsesHermesSQLiteWhenACPFail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	createReporterHermesStateDB(t, filepath.Join(home, ".hermes", "state.db"))
	insertReporterHermesSession(t, filepath.Join(home, ".hermes", "state.db"), "sess-local", "SQLite fallback", 1_780_000_010)

	sessions, err := DefaultScanner{Timeout: 50 * time.Millisecond}.ListSessions(context.Background(), SessionScannerSpec{
		Harness: "hermes",
		Command: []string{"definitely-missing-hermes-acp-test-binary"},
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "hermes:sess-local", sessions[0].SessionID)
	assert.Equal(t, "SQLite fallback", sessions[0].Name)
}

func TestMergeSessionInfosCombinesDuplicateHermesMetadata(t *testing.T) {
	merged := mergeSessionInfos(
		[]model.SessionInfo{{
			SessionID:      "hermes:sess-1",
			AgentType:      "hermes",
			NativeID:       "sess-1",
			Name:           "SQLite title",
			Preview:        "first prompt",
			WorkspaceRoots: []string{"/workspace"},
			Source:         "cli",
			TokenUsage:     42,
			UpdatedAt:      "2026-05-28T20:27:10Z",
		}},
		[]model.SessionInfo{{
			SessionID:   "hermes:sess-1",
			AgentType:   "hermes",
			NativeID:    "sess-1",
			Name:        "ACP title",
			Status:      "running",
			CurrentTask: "responding",
		}},
	)

	require.Len(t, merged, 1)
	assert.Equal(t, "hermes:sess-1", merged[0].SessionID)
	assert.Equal(t, "ACP title", merged[0].Name)
	assert.Equal(t, "first prompt", merged[0].Preview)
	assert.Equal(t, []string{"/workspace"}, merged[0].WorkspaceRoots)
	assert.Equal(t, "cli", merged[0].Source)
	assert.Equal(t, "running", merged[0].Status)
	assert.Equal(t, "responding", merged[0].CurrentTask)
	assert.Equal(t, int64(42), merged[0].TokenUsage)
}

func TestNormalizeHermesSessionsCanonicalizesNativeIDs(t *testing.T) {
	sessions := normalizeHermesSessions([]model.SessionInfo{
		{SessionID: "sess-1", Name: "native session id"},
		{SessionID: "hermes:sess-2", AgentType: "hermes"},
		{},
	})

	require.Len(t, sessions, 2)
	assert.Equal(t, "hermes:sess-1", sessions[0].SessionID)
	assert.Equal(t, "sess-1", sessions[0].NativeID)
	assert.Equal(t, "hermes:sess-2", sessions[1].SessionID)
	assert.Equal(t, "sess-2", sessions[1].NativeID)
}

func TestHermesSpecDetectionAndMergeKeys(t *testing.T) {
	assert.True(t, isHermesSpec(SessionScannerSpec{Command: []string{"hermes", "acp"}}))
	assert.True(t, isHermesSpec(SessionScannerSpec{Command: []string{"hermes", "-p", "ABC", "acp"}}))
	assert.True(t, isHermesSpec(SessionScannerSpec{Command: []string{"/usr/local/bin/hermes", "--profile=ABC", "acp"}}))
	assert.False(t, isHermesSpec(SessionScannerSpec{Harness: "codex"}))
	assert.Equal(t, "hermes:sess-1", sessionMergeKey(model.SessionInfo{SessionID: "hermes:sess-1"}))
	assert.Empty(t, sessionMergeKey(model.SessionInfo{}))
}

func TestMergeHermesLocalSessionsReturnsACPWhenSQLiteUnavailable(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing-home"))

	sessions, err := mergeHermesLocalSessions(context.Background(), SessionScannerSpec{
		Harness: "hermes",
	}, []model.SessionInfo{{SessionID: "sess-acp", Name: "ACP only"}})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "hermes:sess-acp", sessions[0].SessionID)
	assert.Equal(t, "ACP only", sessions[0].Name)
}

func fakeHermesACPCommand(t *testing.T, sessionsJSON string) []string {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "fake-hermes-acp.sh")
	script := `#!/bin/sh
read line
printf '{"jsonrpc":"2.0","id":1,"result":{"authMethods":[]}}\n'
read line
printf '{"jsonrpc":"2.0","id":2,"result":{"sessions":` + sessionsJSON + `}}\n'
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	return []string{scriptPath}
}

func createReporterHermesStateDB(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			title TEXT,
			cwd TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			end_reason TEXT,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			archived INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT,
			timestamp REAL NOT NULL,
			active INTEGER NOT NULL DEFAULT 1
		);
	`)
	require.NoError(t, err)
}

func insertReporterHermesSession(t *testing.T, path string, id string, title string, startedAt float64) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`
		INSERT INTO sessions (id, source, title, started_at)
		VALUES (?, 'cli', ?, ?)
	`, id, title, startedAt)
	require.NoError(t, err)
}
