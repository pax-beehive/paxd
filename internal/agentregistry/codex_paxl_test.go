package agentregistry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexDelegatesMetadataAndHistoryToPaxl(t *testing.T) {
	command := filepath.Join(t.TempDir(), "paxl")
	script := `#!/bin/sh
if [ "$2" = list ]; then
 printf '{"id":"codex:main","agent":"codex","nativeId":"main","title":"Correct main title"}\n'
else
 test "$7" = 'codex:main' || exit 2
 printf '{"schemaVersion":"paxl.session.snapshot.v1","sessionId":"codex:main"}\n'
 printf '{"sessionId":"codex:main","seq":1,"type":"tool_call","durationMs":12,"normalized":{"callId":"c1"},"raw":"original","usage":{"tokens":4}}\n'
fi
`
	require.NoError(t, os.WriteFile(command, []byte(script), 0o755))
	t.Setenv("PAXD_PAXL_COMMAND", command)
	status := DetectWithProbe(Agent{Name: "codex", Kind: "local"}, false)
	require.True(t, status.Available)
	sessions, err := ListSessions(t.Context(), status, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "Correct main title", sessions[0].Name)
	elements, err := CodexElements(t.Context(), "main")
	require.NoError(t, err)
	require.Len(t, elements, 1)
	assert.Equal(t, "tool_call", elements[0].Type)
	assert.Equal(t, int64(12), elements[0].DurationMS)
	assert.Equal(t, "c1", elements[0].NormalizedRaw["callId"])
	assert.JSONEq(t, `{"tokens":4}`, elements[0].UsageJSON)
	assert.Equal(t, "original", elements[0].RawJSON)
}

func TestCodexCannotFallBackWhenPaxlIsMissingOrFails(t *testing.T) {
	for _, command := range []string{"missing-paxl-test-binary", "/usr/bin/false"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("PAXD_PAXL_COMMAND", command)
			sessions, err := ListSessions(t.Context(), Status{Agent: Agent{Name: "codex", Kind: "local"}, Available: true}, 0)
			require.Error(t, err)
			assert.Empty(t, sessions)
			elements, err := CodexElements(context.Background(), "main")
			require.Error(t, err)
			assert.Empty(t, elements)
			if command == "missing-paxl-test-binary" {
				assert.False(t, codexLocalAvailable())
			}
		})
	}
}

// This opt-in test uses a built paxl executable to check the CLI boundary as
// well as the independent source-reader tests in the paxl repository.
func TestCodexPaxlIntegration(t *testing.T) {
	binary := os.Getenv("PAXD_TEST_PAXL")
	if binary == "" {
		t.Skip("Set PAXD_TEST_PAXL to a built paxl executable.")
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("PAXD_PAXL_COMMAND", binary+" --db "+filepath.Join(t.TempDir(), "sessions.sqlite"))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "sessions"), 0o700))
	index := `{"id":"main","thread_name":"Original","updated_at":"2026-10-16T01:00:00Z"}` + "\n" + `{"id":"fork","thread_name":"Correct compacted title","updated_at":"2026-10-16T02:00:00Z"}` + "\n" + `{"id":"review","thread_name":"Guardian review","updated_at":"2026-10-16T03:00:00Z"}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(index), 0o600))
	for id, meta := range map[string]string{
		"main":   `{"id":"main","cwd":"/work"}`,
		"fork":   `{"id":"fork","forked_from_id":"main","cwd":"/work"}`,
		"review": `{"id":"review","parent_thread_id":"main","thread_source":"guardian_review"}`,
	} {
		content := `{"type":"session_meta","payload":` + meta + "}\n" + `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"` + id + ` history"}]}}` + "\n" + `{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{}"}}` + "\n"
		path := filepath.Join(home, "sessions", "rollout-"+id+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		stamp := time.Unix(1, 0)
		if id == "fork" {
			stamp = time.Unix(2, 0)
		}
		require.NoError(t, os.Chtimes(path, stamp, stamp))
	}
	status := Status{Agent: Agent{Name: "codex", Kind: "local"}, Available: true}
	sessions, err := ListSessions(t.Context(), status, 10*time.Second)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "main", sessions[0].NativeID)
	assert.Equal(t, "Correct compacted title", sessions[0].Name)
	elements, err := CodexElements(t.Context(), "main")
	require.NoError(t, err)
	require.Len(t, elements, 2)
	assert.Equal(t, "fork history", elements[0].ContentText)
	assert.Equal(t, "tool_call", elements[1].Type)
	assert.Equal(t, "c1", elements[1].NormalizedRaw["callId"])
}
