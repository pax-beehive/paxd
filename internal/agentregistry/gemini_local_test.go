package agentregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListGeminiLocalSessionsReadsPaxlCompatibleHistory(t *testing.T) {
	geminiHome := t.TempDir()
	t.Setenv("GEMINI_HOME", geminiHome)
	sessionDir := filepath.Join(geminiHome, "tmp", "sample-project", "chats")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(geminiHome, "tmp", "sample-project", ".project_root"),
		[]byte("/tmp/project"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "session-2026-06-20T05-31-gemini123.jsonl"),
		[]byte(
			`{"sessionId":"gemini-session","projectHash":"sample-project","startTime":"2026-06-20T05:31:20.160Z","lastUpdated":"2026-06-20T05:31:20.160Z","kind":"main"}`+"\n"+
				`{"$set":{"messages":[{"id":"u1","timestamp":"2026-06-20T05:31:30.160Z","type":"user","content":[{"text":"Explain paxl"}]},{"id":"a1","timestamp":"2026-06-20T05:32:20.160Z","type":"gemini","content":[{"text":"paxl moves context."}]}],"lastUpdated":"2026-06-20T05:32:20.160Z"}}`+"\n",
		),
		0o644,
	))

	sessions, err := ListGeminiLocalSessions(context.Background(), 0)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "gemini:gemini-session", sessions[0].SessionID)
	assert.Equal(t, "gemini", sessions[0].AgentType)
	assert.Equal(t, "gemini-session", sessions[0].NativeID)
	assert.Equal(t, "Explain paxl", sessions[0].Name)
	assert.Equal(t, "/tmp/project", sessions[0].ProjectID)
	assert.Equal(t, []string{"/tmp/project"}, sessions[0].WorkspaceRoots)
	assert.Equal(t, "available", sessions[0].Status)
	assert.Equal(t, "2026-06-20T05:32:20.160Z", sessions[0].UpdatedAt)
}

func TestListGeminiLocalSessionsResolvesProjectHashAndLimit(t *testing.T) {
	geminiHome := t.TempDir()
	t.Setenv("GEMINI_HOME", geminiHome)
	projectRoot := "/tmp/pax-console"
	hashBytes := sha256.Sum256([]byte(projectRoot))
	projectHash := hex.EncodeToString(hashBytes[:])
	sessionDir := filepath.Join(geminiHome, "tmp", projectHash, "chats")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(geminiHome, "projects.json"),
		[]byte(`{"projects":{"/tmp/pax-console":"pax-console"}}`),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "session-new.json"),
		[]byte(`{
			"sessionId":"new",
			"projectHash":"`+projectHash+`",
			"startTime":"2026-06-20T05:31:20.160Z",
			"lastUpdated":"2026-06-20T05:33:20.160Z",
			"messages":[{"id":"i1","timestamp":"2026-06-20T05:31:20.160Z","type":"info","content":"Loaded project."}]
		}`),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "session-old.json"),
		[]byte(`{
			"sessionId":"old",
			"projectHash":"`+projectHash+`",
			"startTime":"2026-06-20T05:30:20.160Z",
			"lastUpdated":"2026-06-20T05:30:20.160Z"
		}`),
		0o644,
	))

	sessions, err := ListGeminiLocalSessions(context.Background(), 1)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "gemini:new", sessions[0].SessionID)
	assert.Equal(t, "pax-console", sessions[0].Name)
	assert.Equal(t, projectRoot, sessions[0].ProjectID)
}

func TestListGeminiLocalSessionsReturnsEmptyWhenRootMissing(t *testing.T) {
	t.Setenv("GEMINI_HOME", filepath.Join(t.TempDir(), "missing"))

	sessions, err := ListGeminiLocalSessions(context.Background(), 0)

	require.NoError(t, err)
	assert.Empty(t, sessions)
}

func TestListGeminiLocalSessionsSkipsNoisyTitleAndExtractsCommandName(t *testing.T) {
	geminiHome := t.TempDir()
	t.Setenv("GEMINI_HOME", geminiHome)
	sessionDir := filepath.Join(geminiHome, "tmp", "sample-project", "chats")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessionDir, "session-command.jsonl"),
		[]byte(
			`{"sessionId":"gemini-command","projectHash":"sample-project","startTime":"2026-06-20T05:31:20.160Z","lastUpdated":"2026-06-20T05:31:20.160Z","kind":"main"}`+"\n"+
				`{"id":"u1","timestamp":"2026-06-20T05:31:30.160Z","type":"user","content":"<session_context>bootstrap</session_context>"}`+"\n"+
				`{"id":"u2","timestamp":"2026-06-20T05:33:20.160Z","type":"user","content":"<command-name>/usage</command-name>\n<command-message>usage</command-message>"}`+"\n",
		),
		0o644,
	))

	sessions, err := ListGeminiLocalSessions(context.Background(), 0)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "/usage", sessions[0].Name)
	assert.Equal(t, "2026-06-20T05:33:20.160Z", sessions[0].UpdatedAt)
}
