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

	sessions, err := DefaultScanner{Timeout: 50 * time.Millisecond}.ListSessions(context.Background(), SessionScannerSpec{
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

	sessions, err := DefaultScanner{Timeout: 5 * time.Second}.ListSessions(context.Background(), SessionScannerSpec{
		Harness: "gemini",
		Command: command,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "gemini-acp", sessions[0].SessionID)
	assert.Equal(t, "ACP Gemini", sessions[0].Name)
}

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

func TestNormalizeCodexSessionsCollapsesForkLineage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "07", "19")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-2026-07-18T01-00-00-aaa.jsonl"), []byte(
		`{"type":"session_meta","payload":{"id":"aaa","session_id":"aaa","timestamp":"2026-07-18T01:00:00Z","cwd":"/tmp/project","source":"vscode"}}`+"\n",
	), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-2026-07-19T01-00-00-bbb.jsonl"), []byte(
		`{"type":"session_meta","payload":{"id":"bbb","session_id":"bbb","forked_from_id":"aaa","timestamp":"2026-07-19T01:00:00Z","cwd":"/tmp/project","source":"vscode"}}`+"\n",
	), 0o644))

	sessions := normalizeCodexSessions([]model.SessionInfo{
		{SessionID: "bbb", Name: "forked thread", UpdatedAt: "2026-07-19T03:00:00Z"},
		{SessionID: "aaa", Name: "original thread", UpdatedAt: "2026-07-18T02:00:00Z"},
	})

	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:aaa", sessions[0].SessionID)
	assert.Equal(t, "aaa", sessions[0].NativeID)
	assert.Equal(t, "codex", sessions[0].AgentType)
	// The root thread names the conversation even when its entry is older.
	assert.Equal(t, "original thread", sessions[0].Name)
	// The newest entry provides the activity timestamp.
	assert.Equal(t, "2026-07-19T03:00:00Z", sessions[0].UpdatedAt)
}

func TestNormalizeCodexSessionsWithoutLocalStore(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))

	sessions := normalizeCodexSessions([]model.SessionInfo{
		{SessionID: "xxx", Name: "standalone"},
		{SessionID: "codex:yyy", NativeID: "yyy"},
		{},
	})

	require.Len(t, sessions, 2)
	assert.Equal(t, "codex:xxx", sessions[0].SessionID)
	assert.Equal(t, "xxx", sessions[0].NativeID)
	assert.Equal(t, "codex", sessions[0].AgentType)
	assert.Equal(t, "standalone", sessions[0].Name)
	assert.Equal(t, "codex:yyy", sessions[1].SessionID)
	assert.Equal(t, "yyy", sessions[1].NativeID)
}

func TestDefaultScannerNormalizesCodexACPSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "07", "19")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-2026-07-19T01-00-00-bbb.jsonl"), []byte(
		`{"type":"session_meta","payload":{"id":"bbb","session_id":"bbb","forked_from_id":"aaa","timestamp":"2026-07-19T01:00:00Z","cwd":"/tmp/project","source":"vscode"}}`+"\n",
	), 0o644))
	command := fakeACPCommand(t, `[{"sessionId":"bbb","title":"same title","updatedAt":"2026-07-19T03:00:00Z"},{"sessionId":"aaa","title":"same title","updatedAt":"2026-07-18T02:00:00Z"}]`)

	sessions, err := DefaultScanner{Timeout: 5 * time.Second}.ListSessions(context.Background(), SessionScannerSpec{
		Harness: "codex",
		Command: command,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:aaa", sessions[0].SessionID)
	assert.Equal(t, "aaa", sessions[0].NativeID)
	assert.Equal(t, "same title", sessions[0].Name)
}

func TestCodexSpecDetection(t *testing.T) {
	assert.True(t, isCodexSpec(SessionScannerSpec{Harness: "codex"}))
	assert.True(t, isCodexSpec(SessionScannerSpec{AgentType: "codex"}))
	assert.True(t, isCodexSpec(SessionScannerSpec{Command: []string{"codex-acp"}}))
	assert.True(t, isCodexSpec(SessionScannerSpec{Command: []string{"/usr/local/bin/codex", "--acp"}}))
	assert.False(t, isCodexSpec(SessionScannerSpec{Harness: "hermes"}))
	assert.False(t, isCodexSpec(SessionScannerSpec{Command: []string{"hermes", "acp"}}))
	assert.False(t, isCodexSpec(SessionScannerSpec{Command: []string{"npx", "-y", "@zed-industries/codex-acp"}}))
}

func TestDefaultScannerCanonicalizesKimiACPSessions(t *testing.T) {
	command := fakeACPCommand(t, `[{"sessionId":"session_abc-123","title":"hello kimi","cwd":"/tmp/project","updatedAt":"2026-07-19T02:46:15.726Z"}]`)

	sessions, err := DefaultScanner{Timeout: 5 * time.Second}.ListSessions(context.Background(), SessionScannerSpec{
		Harness: "kimi",
		Command: command,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "kimi:session_abc-123", sessions[0].SessionID)
	assert.Equal(t, "session_abc-123", sessions[0].NativeID)
	assert.Equal(t, "kimi", sessions[0].AgentType)
	assert.Equal(t, "hello kimi", sessions[0].Name)
	assert.Equal(t, "/tmp/project", sessions[0].ProjectID)
	assert.Equal(t, "2026-07-19T02:46:15.726Z", sessions[0].UpdatedAt)
}

func TestNormalizeKimiSessions(t *testing.T) {
	sessions := normalizeKimiSessions([]model.SessionInfo{
		{SessionID: "session_abc", Name: "bare id"},
		{SessionID: "kimi:session_def", NativeID: "session_def"},
		{},
	})

	require.Len(t, sessions, 2)
	assert.Equal(t, "kimi:session_abc", sessions[0].SessionID)
	assert.Equal(t, "session_abc", sessions[0].NativeID)
	assert.Equal(t, "kimi", sessions[0].AgentType)
	assert.Equal(t, "bare id", sessions[0].Name)
	assert.Equal(t, "kimi:session_def", sessions[1].SessionID)
	assert.Equal(t, "session_def", sessions[1].NativeID)
}

func TestKimiSpecDetection(t *testing.T) {
	assert.True(t, isKimiSpec(SessionScannerSpec{Harness: "kimi"}))
	assert.True(t, isKimiSpec(SessionScannerSpec{AgentType: "kimi-code"}))
	assert.True(t, isKimiSpec(SessionScannerSpec{AgentType: "kimi_code"}))
	assert.True(t, isKimiSpec(SessionScannerSpec{Command: []string{"kimi", "acp"}}))
	assert.True(t, isKimiSpec(SessionScannerSpec{Command: []string{"/home/user/.kimi-code/bin/kimi", "acp"}}))
	assert.False(t, isKimiSpec(SessionScannerSpec{Harness: "codex"}))
	assert.False(t, isKimiSpec(SessionScannerSpec{Command: []string{"codex-acp"}}))
	assert.False(t, isKimiSpec(SessionScannerSpec{Command: []string{"hermes", "acp"}}))
}

func TestDefaultScannerCanonicalizesPiACPSessions(t *testing.T) {
	command := fakeACPCommand(t, `[{"sessionId":"019f5007-733c-710e-a114-16a50fdf3fe7","title":"hello pi","cwd":"/tmp/project","updatedAt":"2026-07-11T07:16:57.296Z"}]`)

	sessions, err := DefaultScanner{Timeout: 5 * time.Second}.ListSessions(context.Background(), SessionScannerSpec{
		Harness: "pi",
		Command: command,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "pi:019f5007-733c-710e-a114-16a50fdf3fe7", sessions[0].SessionID)
	assert.Equal(t, "019f5007-733c-710e-a114-16a50fdf3fe7", sessions[0].NativeID)
	assert.Equal(t, "pi", sessions[0].AgentType)
	assert.Equal(t, "hello pi", sessions[0].Name)
	assert.Equal(t, "/tmp/project", sessions[0].ProjectID)
}

func TestNormalizePiSessions(t *testing.T) {
	sessions := normalizePiSessions([]model.SessionInfo{
		{SessionID: "abc", Name: "bare id"},
		{SessionID: "pi:def", NativeID: "def"},
		{},
	})

	require.Len(t, sessions, 2)
	assert.Equal(t, "pi:abc", sessions[0].SessionID)
	assert.Equal(t, "abc", sessions[0].NativeID)
	assert.Equal(t, "pi", sessions[0].AgentType)
	assert.Equal(t, "pi:def", sessions[1].SessionID)
	assert.Equal(t, "def", sessions[1].NativeID)
}

func TestPiSpecDetection(t *testing.T) {
	assert.True(t, isPiSpec(SessionScannerSpec{Harness: "pi"}))
	assert.True(t, isPiSpec(SessionScannerSpec{AgentType: "pi-agent"}))
	assert.True(t, isPiSpec(SessionScannerSpec{Command: []string{"pi-acp"}}))
	assert.True(t, isPiSpec(SessionScannerSpec{Command: []string{"/usr/local/bin/pi-acp"}}))
	assert.False(t, isPiSpec(SessionScannerSpec{Harness: "kimi"}))
	assert.False(t, isPiSpec(SessionScannerSpec{Command: []string{"kimi", "acp"}}))
	assert.False(t, isPiSpec(SessionScannerSpec{Command: []string{"npx", "-y", "pi-acp"}}))
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
