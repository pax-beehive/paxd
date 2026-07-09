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

func TestDefaultScannerUsesPaxlBinaryWhenAvailable(t *testing.T) {
	command := fakePaxlCommand(t)

	sessions, err := DefaultScanner{
		Timeout:     5 * time.Second,
		PaxlCommand: []string{command},
	}.ListSessions(context.Background(), SessionScannerSpec{Harness: "codex"})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:sess_paxl", sessions[0].SessionID)
	assert.Equal(t, "codex", sessions[0].AgentType)
	assert.Equal(t, "sess_paxl", sessions[0].NativeID)
	assert.Equal(t, "From paxl", sessions[0].Name)
	assert.Equal(t, "paxl", sessions[0].Source)
	require.Len(t, sessions[0].Messages, 1)
	assert.Equal(t, "assistant", sessions[0].Messages[0].Role)
	assert.Equal(t, "hello from paxl", sessions[0].Messages[0].Text)
}

func TestDefaultScannerPassesLimitToPaxl(t *testing.T) {
	command := fakePaxlCommandRequiringLimit(t, "2")

	sessions, err := DefaultScanner{
		Timeout:     5 * time.Second,
		PaxlCommand: []string{command},
	}.ListSessions(context.Background(), SessionScannerSpec{Harness: "codex", Limit: 2})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:sess_limited", sessions[0].SessionID)
}

func TestDefaultScannerUsesCodexLocalIndexBeforeACP(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	require.NoError(t, os.WriteFile(
		filepath.Join(codexHome, "session_index.jsonl"),
		[]byte(`{"id":"sess-local","thread_name":"Correct Codex thread","updated_at":"2026-07-08T23:01:50Z"}`+"\n"),
		0o644,
	))
	rolloutDir := filepath.Join(codexHome, "sessions", "2026", "07", "08")
	require.NoError(t, os.MkdirAll(rolloutDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(rolloutDir, "rollout-2026-07-08T23-01-50-sess-local.jsonl"),
		[]byte(`{"type":"session_meta","payload":{"id":"sess-local","timestamp":"2026-07-08T23:01:50Z","cwd":"/tmp/project"}}`+"\n"),
		0o644,
	))
	command := fakeACPCommand(
		t,
		`[{"sessionId":"codex:sess-local","nativeId":"sess-local","title":"# AGENTS.md instructions <INSTRUCTIONS> system prompt"}]`,
	)

	sessions, err := legacyDefaultScanner(5*time.Second).ListSessions(context.Background(), SessionScannerSpec{
		Harness: "codex",
		Command: command,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:sess-local", sessions[0].SessionID)
	assert.Equal(t, "Correct Codex thread", sessions[0].Name)
}

func TestResolvePaxlCommandReadsEnvironmentOverride(t *testing.T) {
	command := fakePaxlCommand(t)
	t.Setenv("PAXD_PAXL_COMMAND", command+" --profile local")

	resolved, ok := resolvePaxlCommand(nil)

	require.True(t, ok)
	assert.Equal(t, []string{command, "--profile", "local"}, resolved)
}

func TestDefaultScannerUsesGeminiLocalSessionsBeforeACP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GEMINI_HOME", home)
	sessionDir := filepath.Join(home, "tmp", "sample-project", "chats")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "tmp", "sample-project", ".project_root"),
		[]byte("/tmp/project"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "session-2026-06-20T05-31-gemini-local.jsonl"),
		[]byte(
			`{"sessionId":"gemini-local","projectHash":"sample-project","startTime":"2026-06-20T05:31:20.160Z","lastUpdated":"2026-06-20T05:31:20.160Z","kind":"main"}`+"\n"+
				`{"$set":{"messages":[{"id":"u1","timestamp":"2026-06-20T05:31:30.160Z","type":"user","content":[{"text":"Use paxl Gemini history"}]}],"lastUpdated":"2026-06-20T05:32:20.160Z"}}`+"\n",
		),
		0o644,
	))

	sessions, err := legacyDefaultScanner(50*time.Millisecond).ListSessions(context.Background(), SessionScannerSpec{
		Harness: "gemini",
		Command: []string{"definitely-missing-gemini-acp-test-binary", "--acp"},
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "gemini:gemini-local", sessions[0].SessionID)
	assert.Equal(t, "gemini-local", sessions[0].NativeID)
	assert.Equal(t, "gemini", sessions[0].AgentType)
	assert.Equal(t, "Use paxl Gemini history", sessions[0].Name)
	assert.Equal(t, "/tmp/project", sessions[0].ProjectID)
	assert.Equal(t, "2026-06-20T05:32:20.160Z", sessions[0].UpdatedAt)
}

func TestDefaultScannerFallsBackToGeminiACPWhenLocalSessionsAreMissing(t *testing.T) {
	t.Setenv("GEMINI_HOME", filepath.Join(t.TempDir(), "missing"))
	command := fakeACPCommand(t, `[{"sessionId":"gemini-acp","nativeId":"gemini-acp","title":"ACP Gemini"}]`)

	sessions, err := legacyDefaultScanner(5*time.Second).ListSessions(context.Background(), SessionScannerSpec{
		Harness: "gemini",
		Command: command,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "gemini-acp", sessions[0].SessionID)
	assert.Equal(t, "ACP Gemini", sessions[0].Name)
}

func TestDefaultScannerUsesCodexLocalSessionsWithoutStartingACP(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	require.NoError(t, os.WriteFile(
		filepath.Join(codexHome, "session_index.jsonl"),
		[]byte(`{"id":"sess-local","thread_name":"Local Codex","updated_at":"2026-06-20T05:32:20Z"}`+"\n"),
		0o644,
	))
	markerPath := filepath.Join(t.TempDir(), "started")
	scriptPath := filepath.Join(t.TempDir(), "fake-codex-acp.sh")
	script := `#!/bin/sh
printf started > "$1"
read line
printf '{"jsonrpc":"2.0","id":1,"result":{"authMethods":[]}}\n'
read line
printf '{"jsonrpc":"2.0","id":2,"result":{"sessions":[]}}\n'
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))

	sessions, err := legacyDefaultScanner(time.Second).ListSessions(context.Background(), SessionScannerSpec{
		Harness: "codex",
		Command: []string{scriptPath, markerPath},
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:sess-local", sessions[0].SessionID)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("codex ACP command was started; marker stat error = %v", err)
	}
}

func TestDefaultScannerMergesHermesSQLiteSessionsWhenACPSucceeds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	createReporterHermesStateDB(t, filepath.Join(home, ".hermes", "state.db"))
	insertReporterHermesSession(t, filepath.Join(home, ".hermes", "state.db"), "sess-local", "SQLite only", 1_780_000_010)
	command := fakeHermesACPCommand(t, `[{"sessionId":"sess-acp","nativeId":"sess-acp","title":"ACP only","updatedAt":"2026-06-01T20:26:40Z"}]`)

	sessions, err := legacyDefaultScanner(5*time.Second).ListSessions(context.Background(), SessionScannerSpec{
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

	sessions, err := legacyDefaultScanner(50*time.Millisecond).ListSessions(context.Background(), SessionScannerSpec{
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

	sessions, err := legacyDefaultScanner(50*time.Millisecond).ListSessions(context.Background(), SessionScannerSpec{
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
	assert.True(t, isGeminiSpec(SessionScannerSpec{Harness: "gemini"}))
	assert.True(t, isGeminiSpec(SessionScannerSpec{Command: []string{"gemini", "--acp"}}))
	assert.False(t, isGeminiSpec(SessionScannerSpec{Command: []string{"hermes", "acp"}}))
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
	return fakeACPCommand(t, sessionsJSON)
}

func fakeACPCommand(t *testing.T, sessionsJSON string) []string {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "fake-acp.sh")
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

func legacyDefaultScanner(timeout time.Duration) DefaultScanner {
	return DefaultScanner{
		Timeout:     timeout,
		PaxlCommand: []string{"definitely-missing-paxl-test-binary"},
	}
}

func fakePaxlCommand(t *testing.T) string {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "paxl")
	script := `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "list" ]; then
  printf '{"schemaVersion":"paxl.session.metadata.v1","id":"codex:sess_paxl","agent":"codex","nativeId":"sess_paxl","title":"From paxl","status":"available","updatedAt":"2026-07-07T01:02:03Z"}\n'
  exit 0
fi
if [ "$1" = "session" ] && [ "$2" = "get" ]; then
  printf '{"schemaVersion":"paxl.session.element.v1","sessionId":"codex:sess_paxl","seq":1,"type":"message","role":"assistant","completedAt":"2026-07-07T01:02:04Z","contentText":"hello from paxl"}\n'
  exit 0
fi
echo unexpected "$@" >&2
exit 2
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	return scriptPath
}

func fakePaxlCommandRequiringLimit(t *testing.T, limit string) string {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "paxl")
	script := `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "list" ]; then
  if [ "$7" != "--limit" ] || [ "$8" != "` + limit + `" ]; then
    echo missing expected limit "$@" >&2
    exit 3
  fi
  printf '{"schemaVersion":"paxl.session.metadata.v1","id":"codex:sess_limited","agent":"codex","nativeId":"sess_limited","title":"Limited","status":"available","updatedAt":"2026-07-07T01:02:03Z"}\n'
  exit 0
fi
if [ "$1" = "session" ] && [ "$2" = "get" ]; then
  exit 0
fi
echo unexpected "$@" >&2
exit 2
`
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	return scriptPath
}
