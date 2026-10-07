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

func TestCodexDeviceLoginSurvivesClientExitAndCompletesWithoutCodeSubmission(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "codex")
	gate := filepath.Join(root, "approved")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = 'login status' ]; then echo 'Logged in using ChatGPT' >&2; exit 0; fi\n" +
		"[ \"$1 $2\" = 'login --device-auth' ] || exit 2\n" +
		"printf '\\033[1mhttps://auth.openai.com/codex/device\\033[0m\\n'\n" +
		"printf 'Enter this one-time code: ABCD-EFGHJ\\n'\n" +
		"while [ ! -f '" + gate + "' ]; do sleep 0.01; done\n"
	require.NoError(t, os.WriteFile(command, []byte(script), 0700))
	m := New(t.Context(), Options{Command: command})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithCancel(t.Context())
	first, err := m.Login(ctx, testOwner, "start-codex", control.HarnessAuthLoginCommand{Harness: "codex", Operation: "start"})
	require.NoError(t, err)
	cancel()
	view := awaitHarnessState(t, m, "codex", first.SessionID, "awaiting_browser")
	require.Equal(t, "https://auth.openai.com/codex/device", view.AuthorizationURL)
	require.Equal(t, "ABCD-EFGHJ", view.UserCode)
	// Browser completion does not submit an authorization code through Pax.
	_, err = m.Login(t.Context(), testOwner, "unexpected-submit", control.HarnessAuthLoginCommand{Harness: "codex", Operation: "submit", SessionID: first.SessionID, Code: "not-a-device-code"})
	require.Error(t, err)
	require.NoError(t, os.WriteFile(gate, nil, 0600))
	view = awaitHarnessState(t, m, "codex", first.SessionID, "succeeded")
	require.NotNil(t, view.LoggedIn)
	require.True(t, *view.LoggedIn)
	require.Empty(t, view.AuthorizationURL)
	require.Empty(t, view.UserCode)
}

func TestCodexAPIKeyLoginUsesStdinAndDoesNotExposeNativeOutput(t *testing.T) {
	command := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
if [ "$1 $2" = 'login status' ]; then echo 'Logged in using an API key - secret-value' >&2; exit 0; fi
[ "$1 $2" = 'login --with-api-key' ] && [ "$#" = 2 ] || exit 2
value=$(cat)
[ "$value" = 'secret-value' ] || exit 1
`
	require.NoError(t, os.WriteFile(command, []byte(script), 0700))
	m := New(t.Context(), Options{Command: command})
	t.Cleanup(m.Close)
	first, err := m.Login(t.Context(), testOwner, "key-login", control.HarnessAuthLoginCommand{Harness: "codex", Operation: "start", Method: "api-key", Code: "secret-value"})
	require.NoError(t, err)
	view := awaitHarnessState(t, m, "codex", first.SessionID, "succeeded")
	require.Equal(t, "api_key", view.AuthMethod)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret-value")
}

func awaitHarnessState(t *testing.T, m *Manager, harness, id, state string) control.HarnessAuthView {
	t.Helper()
	var view control.HarnessAuthView
	require.Eventually(t, func() bool {
		var err error
		view, err = m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: harness, SessionID: id})
		return err == nil && view.State == state
	}, 5*time.Second, 5*time.Millisecond)
	return view
}

func TestFailedDeviceLoginRemainsVisibleWithoutAnInternalHandle(t *testing.T) {
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\necho secret-value >&2\nexit 1\n"), 0700))
	m := New(t.Context(), Options{Command: command})
	t.Cleanup(m.Close)
	first, err := m.Login(t.Context(), testOwner, "start", control.HarnessAuthLoginCommand{Harness: "codex", Operation: "start"})
	require.NoError(t, err)
	awaitHarnessState(t, m, "codex", first.SessionID, "failed")
	view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "codex"})
	require.NoError(t, err)
	require.Equal(t, "failed", view.State)
	require.Equal(t, "login_failed", view.ErrorCode)
}

func TestStatusFailureIsNotReportedAsLoggedOut(t *testing.T) {
	for _, output := range []string{"network unavailable", "unexpected format", "Logged in using an API key - secret-value"} {
		t.Run(output, func(t *testing.T) {
			command := filepath.Join(t.TempDir(), "codex")
			require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\necho '"+output+"' >&2\nexit 1\n"), 0700))
			m := New(t.Context(), Options{Command: command})
			t.Cleanup(m.Close)
			view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "codex"})
			require.Error(t, err)
			require.Nil(t, view.LoggedIn)
			require.NotContains(t, err.Error(), output)
		})
	}
}

func TestDeviceChallengeWaitsForCompleteCodeAndRejectsLookalikeURL(t *testing.T) {
	m := New(t.Context(), Options{})
	t.Cleanup(m.Close)
	s := &session{view: control.HarnessAuthView{Harness: "codex", State: "starting"}}
	w := &loginOutput{manager: m, session: s}
	_, err := w.Write([]byte("https://auth.openai.com/codex/device.evil\nABCD-EFGHJ\n"))
	require.NoError(t, err)
	require.Equal(t, "starting", s.view.State)
	w.tail = ""
	_, err = w.Write([]byte("https://auth.openai.com/codex/device\nABCD-EFGH"))
	require.NoError(t, err)
	require.Equal(t, "starting", s.view.State)
	_, err = w.Write([]byte("J\n"))
	require.NoError(t, err)
	require.Equal(t, "ABCD-EFGHJ", s.view.UserCode)
}

func TestCodexAccessTokenStatusMatchesInstalledNativeCLI(t *testing.T) {
	for _, line := range []string{"Logged in using access token", "Logged in using personal access token"} {
		t.Run(line, func(t *testing.T) {
			command := filepath.Join(t.TempDir(), "codex")
			script := "#!/bin/sh\nif [ \"$1 $2\" = 'login status' ]; then echo '" + line + "' >&2; exit 0; fi\n[ \"$1 $2\" = 'login --with-access-token' ] || exit 2\nvalue=$(cat)\n[ \"$value\" = 'private-token' ]\n"
			require.NoError(t, os.WriteFile(command, []byte(script), 0700))
			m := New(t.Context(), Options{Command: command})
			t.Cleanup(m.Close)
			first, err := m.Login(t.Context(), testOwner, "token-login", control.HarnessAuthLoginCommand{Harness: "codex", Operation: "start", Method: "access-token", Code: "private-token"})
			require.NoError(t, err)
			view := awaitHarnessState(t, m, "codex", first.SessionID, "succeeded")
			require.Equal(t, "access_token", view.AuthMethod)
		})
	}
}
