package agentregistry

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListHermesLocalSessionsReadsStateDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, ".hermes", "state.db")
	createHermesStateDB(t, dbPath)
	insertHermesSession(t, dbPath, hermesSessionFixture{
		id:             "sess-old",
		source:         "telegram",
		title:          "Older session",
		cwd:            "/workspace/old",
		startedAt:      1_780_000_000,
		inputTokens:    10,
		outputTokens:   20,
		reasoningToken: 3,
		preview:        "older prompt",
		lastActive:     1_780_000_010,
	})
	insertHermesSession(t, dbPath, hermesSessionFixture{
		id:              "sess-new",
		source:          "cli",
		title:           "Newer session",
		cwd:             "/workspace/new",
		startedAt:       1_780_000_020,
		cacheReadTokens: 4,
		cacheWriteToken: 5,
		preview:         "newer prompt",
		lastActive:      1_780_000_030.25,
	})
	insertHermesSession(t, dbPath, hermesSessionFixture{
		id:        "sess-archived",
		title:     "Archived session",
		startedAt: 1_780_000_040,
		archived:  1,
	})

	sessions, err := ListHermesLocalSessions(context.Background(), []string{"hermes", "acp"}, 0)

	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, "hermes:sess-new", sessions[0].SessionID)
	assert.Equal(t, "sess-new", sessions[0].NativeID)
	assert.Equal(t, "hermes", sessions[0].AgentType)
	assert.Equal(t, "Newer session", sessions[0].Name)
	assert.Equal(t, "/workspace/new", sessions[0].ProjectID)
	assert.Equal(t, []string{"/workspace/new"}, sessions[0].WorkspaceRoots)
	assert.Equal(t, "cli", sessions[0].Source)
	assert.Equal(t, "newer prompt", sessions[0].Preview)
	assert.Equal(t, int64(9), sessions[0].TokenUsage)
	assert.Equal(t, "2026-05-28T20:27:10.25Z", sessions[0].UpdatedAt)
	assert.Equal(t, "available", sessions[0].Status)
	assert.Equal(t, "hermes:sess-old", sessions[1].SessionID)
	assert.Equal(t, int64(33), sessions[1].TokenUsage)
}

func TestListHermesLocalSessionsDerivesNameFromCWDWhenUntitled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, ".hermes", "state.db")
	createHermesStateDB(t, dbPath)
	insertHermesSession(t, dbPath, hermesSessionFixture{
		id:        "sess-abcdef123456",
		source:    "cli",
		cwd:       "/workspace/paxd",
		startedAt: 1_780_000_010,
	})

	sessions, err := ListHermesLocalSessions(context.Background(), []string{"hermes", "acp"}, 0)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "paxd (sess-abc)", sessions[0].Name)
	assert.Equal(t, "cli", sessions[0].Source)
}

func TestListHermesLocalSessionsRespectsLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, ".hermes", "state.db")
	createHermesStateDB(t, dbPath)
	insertHermesSession(t, dbPath, hermesSessionFixture{id: "sess-one", startedAt: 1_780_000_001})
	insertHermesSession(t, dbPath, hermesSessionFixture{id: "sess-two", startedAt: 1_780_000_002})

	sessions, err := ListHermesLocalSessions(context.Background(), nil, 1)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "hermes:sess-two", sessions[0].SessionID)
}

func TestListHermesLocalSessionsIgnoresHermesEnvOverrides(t *testing.T) {
	home := t.TempDir()
	ignoredHome := t.TempDir()
	ignoredDB := filepath.Join(t.TempDir(), "ignored-state.db")
	t.Setenv("HOME", home)
	t.Setenv("HERMES_HOME", ignoredHome)
	t.Setenv("HERMES_STATE_DB", ignoredDB)
	defaultDB := filepath.Join(home, ".hermes", "state.db")
	createHermesStateDB(t, defaultDB)
	createHermesStateDB(t, filepath.Join(ignoredHome, "state.db"))
	createHermesStateDB(t, ignoredDB)
	insertHermesSession(t, defaultDB, hermesSessionFixture{
		id:        "sess-home",
		title:     "Home state",
		startedAt: 1_780_000_010,
	})
	insertHermesSession(t, filepath.Join(ignoredHome, "state.db"), hermesSessionFixture{
		id:        "sess-ignored-home",
		title:     "Ignored HERMES_HOME",
		startedAt: 1_780_000_020,
	})
	insertHermesSession(t, ignoredDB, hermesSessionFixture{
		id:        "sess-ignored-db",
		title:     "Ignored HERMES_STATE_DB",
		startedAt: 1_780_000_030,
	})

	sessions, err := ListHermesLocalSessions(context.Background(), []string{"hermes", "acp"}, 0)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "hermes:sess-home", sessions[0].SessionID)
}

func TestHermesLocalAvailableChecksDefaultStateDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, ".hermes", "state.db")
	createHermesStateDB(t, dbPath)

	assert.True(t, hermesLocalAvailable(nil))
	assert.False(t, hermesLocalAvailable([]string{"hermes", "-p", "missing", "acp"}))
}

func TestListHermesLocalSessionsUsesProfileFromCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".hermes")
	defaultDB := filepath.Join(root, "state.db")
	profileDB := filepath.Join(root, "ABC", "state.db")
	createHermesStateDB(t, defaultDB)
	createHermesStateDB(t, profileDB)
	insertHermesSession(t, defaultDB, hermesSessionFixture{
		id:        "sess-default",
		title:     "Default profile",
		startedAt: 1_780_000_001,
	})
	insertHermesSession(t, profileDB, hermesSessionFixture{
		id:        "sess-profile",
		title:     "ABC profile",
		startedAt: 1_780_000_002,
	})

	defaultSessions, err := ListHermesLocalSessions(context.Background(), []string{"hermes", "acp"}, 0)
	require.NoError(t, err)
	profileSessions, err := ListHermesLocalSessions(context.Background(), []string{"hermes", "-p", "ABC", "acp"}, 0)
	require.NoError(t, err)

	require.Len(t, defaultSessions, 1)
	assert.Equal(t, "hermes:sess-default", defaultSessions[0].SessionID)
	require.Len(t, profileSessions, 1)
	assert.Equal(t, "hermes:sess-profile", profileSessions[0].SessionID)
}

func TestHermesProfileFromCommand(t *testing.T) {
	assert.Empty(t, hermesProfileFromCommand([]string{"hermes", "acp"}))
	assert.Empty(t, hermesProfileFromCommand([]string{"hermes", "--profile", "default", "acp"}))
	assert.Equal(t, "ABC", hermesProfileFromCommand([]string{"hermes", "-p", "ABC", "acp"}))
	assert.Equal(t, "ABC", hermesProfileFromCommand([]string{"hermes", "--profile", "ABC", "acp"}))
	assert.Equal(t, "ABC", hermesProfileFromCommand([]string{"hermes", "--profile=ABC", "acp"}))
	assert.Equal(t, "ABC", hermesProfileFromCommand([]string{"hermes", "-pABC", "acp"}))
}

func TestDetectAndListSessionsUseHermesLocalStateDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, ".hermes", "state.db")
	createHermesStateDB(t, dbPath)
	insertHermesSession(t, dbPath, hermesSessionFixture{
		id:        "sess-local",
		title:     "Local Hermes",
		startedAt: 1_780_000_010,
	})
	status := DetectWithProbe(Agent{
		Name:        "hermes",
		Kind:        "acp",
		Command:     []string{"definitely-missing-hermes-acp-test-binary"},
		InstallHint: "install hermes",
	}, false)
	sessions, err := ListSessions(context.Background(), status, time.Second)

	require.NoError(t, err)
	assert.True(t, status.Available)
	assert.Equal(t, "local-log", status.Capability)
	require.Len(t, sessions, 1)
	assert.Equal(t, "hermes:sess-local", sessions[0].SessionID)
}

func TestHermesPathHelpers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	defaultHome, err := defaultHermesHome()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".hermes"), defaultHome)
}

func TestListHermesLocalSessionsReturnsMissingDBError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, err := ListHermesLocalSessions(context.Background(), nil, 0)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "open hermes state db")
}

func TestUnixSecondsToRFC3339RejectsInvalidValues(t *testing.T) {
	assert.Empty(t, unixSecondsToRFC3339(0))
	assert.Empty(t, unixSecondsToRFC3339(-1))
}

type hermesSessionFixture struct {
	id              string
	source          string
	title           string
	cwd             string
	startedAt       float64
	endedAt         *float64
	inputTokens     int64
	outputTokens    int64
	cacheReadTokens int64
	cacheWriteToken int64
	reasoningToken  int64
	preview         string
	lastActive      float64
	archived        int
}

func createHermesStateDB(t *testing.T, path string) {
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

func insertHermesSession(t *testing.T, path string, fixture hermesSessionFixture) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	var endedAt any
	if fixture.endedAt != nil {
		endedAt = *fixture.endedAt
	}
	source := fixture.source
	if source == "" {
		source = "cli"
	}
	_, err = db.Exec(`
		INSERT INTO sessions (
			id, source, title, cwd, started_at, ended_at, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, reasoning_tokens, archived
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, fixture.id, source, fixture.title, fixture.cwd, fixture.startedAt, endedAt, fixture.inputTokens, fixture.outputTokens,
		fixture.cacheReadTokens, fixture.cacheWriteToken, fixture.reasoningToken, fixture.archived)
	require.NoError(t, err)
	if fixture.preview != "" {
		lastActive := fixture.lastActive
		if lastActive <= 0 {
			lastActive = fixture.startedAt
		}
		_, err = db.Exec(`
			INSERT INTO messages (session_id, role, content, timestamp, active)
			VALUES (?, 'user', ?, ?, 1)
		`, fixture.id, fixture.preview, lastActive)
		require.NoError(t, err)
	}
}
