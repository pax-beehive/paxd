//go:build !windows

package harnessauth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/require"
)

const testURL = "https://example.invalid/native-auth"

var testOwner = control.Source{Kind: control.SourceRemote, RemoteID: "manager-a"}

func fakeCLI(t *testing.T, script string, ttl time.Duration) *Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paxl")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1 $2 $3\" = 'harness auth login' ] || exit 42\n"+script), 0700))
	m := New(context.Background(), Options{Command: path, TTL: ttl})
	t.Cleanup(m.Close)
	return m
}
func startLogin(t *testing.T, m *Manager) control.HarnessAuthView {
	t.Helper()
	view, err := m.Login(t.Context(), testOwner, "start-1", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.NoError(t, err)
	return view
}
func awaitState(t *testing.T, m *Manager, id, state string) control.HarnessAuthView {
	t.Helper()
	var view control.HarnessAuthView
	require.Eventually(t, func() bool {
		var err error
		view, err = m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude", SessionID: id})
		return err == nil && view.State == state
	}, 5*time.Second, 5*time.Millisecond)
	return view
}
func promptScript() string {
	return `printf '%s\n' '{"version":1,"type":"notice","url":"https://example.invalid/native-auth"}' '{"version":1,"type":"prompt","id":"one","kind":"secret","message":"Authorization code"}'
IFS= read -r answer
printf '%s' "$answer" > "$PAXL_INPUT_RECORD"
printf '%s\n' '{"version":1,"type":"result","result":{"harness":"claude","state":"succeeded"}}'
`
}
func TestPaxlAttemptSurvivesRequestCancellationAndChecksPromptOwnership(t *testing.T) {
	record := filepath.Join(t.TempDir(), "input")
	t.Setenv("PAXL_INPUT_RECORD", record)
	m := fakeCLI(t, promptScript(), time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	first, err := m.Login(ctx, testOwner, "start-1", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.NoError(t, err)
	cancel()
	view := awaitState(t, m, first.SessionID, "awaiting_input")
	require.Equal(t, "one", view.Prompt.ID)
	require.Equal(t, testURL, view.AuthorizationURL)
	replay := startLogin(t, m)
	require.Equal(t, first.SessionID, replay.SessionID)
	_, err = m.Status(t.Context(), control.Source{Kind: control.SourceRemote, RemoteID: "other"}, control.HarnessAuthStatusQuery{Harness: "claude", SessionID: first.SessionID})
	require.Error(t, err)
	_, err = m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude", SessionID: first.SessionID, ActorID: "other-user"})
	require.Error(t, err)
	req := control.HarnessAuthLoginCommand{Harness: "claude", Operation: "submit", SessionID: first.SessionID, PromptID: "stale", Code: "private-code"}
	_, err = m.Login(t.Context(), testOwner, "submit-1", req)
	require.Error(t, err)
	req.PromptID = "one"
	_, err = m.Login(t.Context(), testOwner, "submit-1", req)
	require.NoError(t, err)
	view = awaitState(t, m, first.SessionID, "succeeded")
	require.Empty(t, view.Events)
	require.Nil(t, view.Prompt)
	require.Empty(t, view.AuthorizationURL)
	require.Nil(t, view.LoggedIn)
	data, err := os.ReadFile(record)
	require.NoError(t, err)
	var input map[string]any
	require.NoError(t, json.Unmarshal(data, &input))
	require.Equal(t, "private-code", input["value"])
	require.Equal(t, "one", input["id"])
	_, err = m.Login(t.Context(), testOwner, "submit-1", req)
	require.NoError(t, err)
	req.Code = "different"
	_, err = m.Login(t.Context(), testOwner, "submit-1", req)
	require.Error(t, err)
}
func TestPaxlExitWithoutSuccessfulFinalResultIsNotAuthenticationSuccess(t *testing.T) {
	for _, script := range []string{"exit 0\n", `printf '%s\n' '{"version":1,"type":"result","result":{"harness":"claude","state":"succeeded"}}'; exit 1`, "echo private-native-output\n"} {
		t.Run(script, func(t *testing.T) {
			m := fakeCLI(t, script, time.Minute)
			view := startLogin(t, m)
			view = awaitState(t, m, view.SessionID, "failed")
			require.NotEmpty(t, view.ErrorCode)
			data, err := json.Marshal(view)
			require.NoError(t, err)
			require.NotContains(t, string(data), "private-native-output")
		})
	}
}
func TestPaxlAttemptDeadlineExpiresAndCloseReapsOwnedProcess(t *testing.T) {
	m := fakeCLI(t, "exec sleep 60\n", 60*time.Millisecond)
	view := startLogin(t, m)
	awaitState(t, m, view.SessionID, "expired")
	m.Close()
	_, err := m.Login(t.Context(), testOwner, "after-close", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.Error(t, err)
}

func TestPaxlCapabilitiesProbeRequiresVersionedProtocolBeforeAuthentication(t *testing.T) {
	for _, reply := range []string{`{"protocol_version":1}`, `{"protocol_version":0}`, `private-native-output`} {
		t.Run(reply, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "paxl")
			script := "#!/bin/sh\n[ \"$*\" = 'harness auth capabilities --format json' ] || exit 23\ncat <<'CAP'\n" + reply + "\nCAP\n"
			require.NoError(t, os.WriteFile(path, []byte(script), 0700))
			m := New(t.Context(), Options{Command: path})
			t.Cleanup(m.Close)
			view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "codex", Operation: "capabilities"})
			if reply == `{"protocol_version":1}` {
				require.NoError(t, err)
				require.Equal(t, 1, view.ProtocolVersion)
			} else {
				require.ErrorContains(t, err, "update paxl")
				require.NotContains(t, err.Error(), "private-native-output")
			}
		})
	}
}
