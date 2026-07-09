package paxlclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeSessionListMapsPaxlJSONLToSessionInfo(t *testing.T) {
	data := []byte(`{"schemaVersion":"paxl.session.metadata.v1","id":"codex:sess_1","agent":"codex","nativeId":"sess_1","title":"Debug paxd","status":"available","preview":"hello","projectId":"paxd","updatedAt":"2026-07-07T01:02:03Z"}` + "\n")

	sessions, err := DecodeSessionList(data)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:sess_1", sessions[0].SessionID)
	assert.Equal(t, "codex", sessions[0].AgentType)
	assert.Equal(t, "sess_1", sessions[0].NativeID)
	assert.Equal(t, "Debug paxd", sessions[0].Name)
	assert.Equal(t, "paxd", sessions[0].ProjectID)
	assert.Equal(t, "paxl", sessions[0].Source)
	assert.Equal(t, "2026-07-07T01:02:03Z", sessions[0].UpdatedAt)
}

func TestDecodeSessionMessagesMapsPaxlElements(t *testing.T) {
	data := []byte(`{"schemaVersion":"paxl.session.element.v1","sessionId":"codex:sess_1","seq":7,"type":"message","role":"assistant","startedAt":"2026-07-07T01:02:03Z","completedAt":"2026-07-07T01:02:04Z","contentText":"done"}` + "\n")

	messages, err := DecodeSessionMessages(data)

	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "codex:sess_1", messages[0].SessionID)
	assert.Equal(t, int64(7), messages[0].Seq)
	assert.Equal(t, "message", messages[0].Kind)
	assert.Equal(t, "assistant", messages[0].Role)
	assert.Equal(t, "done", messages[0].Text)
}

func TestClientRunsPaxlBinaryForSessionList(t *testing.T) {
	command := fakePaxlCommand(t, `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "list" ]; then
  printf '{"id":"codex:sess_1","agent":"codex","nativeId":"sess_1","title":"From paxl","updatedAt":"2026-07-07T01:02:03Z"}\n'
  exit 0
fi
echo unexpected "$@" >&2
exit 2
`)

	sessions, err := Client{Command: []string{command}}.ListSessions(context.Background(), "codex", 3)

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "codex:sess_1", sessions[0].SessionID)
}

func fakePaxlCommand(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paxl")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}
