package harnessauth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/require"
)

const testURL = "https://claude.com/cai/oauth/authorize?response_type=code&state=test-state&code_challenge=test-challenge"

var testOwner = control.Source{Kind: control.SourceRemote, RemoteID: "remote-a"}

func fakeCLI(t *testing.T, login string, ttl time.Duration) *Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nif [ \"$2\" = status ]; then\n echo '{\"loggedIn\":true,\"authMethod\":\"oauth\"}'\n exit 0\nfi\n" + login
	require.NoError(t, os.WriteFile(path, []byte(script), 0700))
	m := New(context.Background(), Options{Command: path, TTL: ttl})
	t.Cleanup(m.Close)
	return m
}
func startLogin(t *testing.T, m *Manager) control.HarnessAuthView {
	t.Helper()
	v, err := m.Login(context.Background(), testOwner, "start-1", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.NoError(t, err)
	return v
}
func awaitState(t *testing.T, m *Manager, id, state string) control.HarnessAuthView {
	t.Helper()
	var view control.HarnessAuthView
	require.Eventually(t, func() bool {
		var err error
		view, err = m.Status(context.Background(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude", SessionID: id})
		return err == nil && view.State == state
	}, 5*time.Second, 5*time.Millisecond)
	return view
}
func promptScript() string {
	return "printf '%s\\n' '" + testURL + "'\nprintf 'Paste code here if prompted > '\nIFS= read -r code\n[ \"$code\" = 'test-code#test-state' ]\n"
}

func TestLoginSurvivesRequestCancellationAndVerifiesResult(t *testing.T) {
	m := fakeCLI(t, promptScript(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	first, err := m.Login(ctx, testOwner, "start-1", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.NoError(t, err)
	cancel()
	view := awaitState(t, m, first.SessionID, "awaiting_code")
	require.Equal(t, testURL, view.AuthorizationURL)
	replay := startLogin(t, m)
	require.Equal(t, first.SessionID, replay.SessionID)
	_, err = m.Login(context.Background(), testOwner, "start-2", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.NoError(t, err)
	submit := control.HarnessAuthLoginCommand{Harness: "claude", Operation: "submit", SessionID: first.SessionID, Code: "test-code#test-state"}
	_, err = m.Login(context.Background(), testOwner, "submit-1", submit)
	require.NoError(t, err)
	_, err = m.Login(context.Background(), testOwner, "submit-1", submit)
	require.NoError(t, err)
	submit.Code = "different"
	_, err = m.Login(context.Background(), testOwner, "submit-1", submit)
	require.ErrorContains(t, err, "already used")
	view = awaitState(t, m, first.SessionID, "succeeded")
	require.Empty(t, view.AuthorizationURL)
	require.NotNil(t, view.LoggedIn)
	require.True(t, *view.LoggedIn)
}

func TestOwnerIsolationCancellationAndTimeout(t *testing.T) {
	m := fakeCLI(t, promptScript(), time.Minute)
	v := startLogin(t, m)
	awaitState(t, m, v.SessionID, "awaiting_code")
	other := control.Source{Kind: control.SourceRemote, RemoteID: "remote-b"}
	_, err := m.Status(context.Background(), other, control.HarnessAuthStatusQuery{Harness: "claude", SessionID: v.SessionID})
	require.ErrorContains(t, err, "not found")
	_, err = m.Login(context.Background(), other, "cancel", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "cancel", SessionID: v.SessionID})
	require.ErrorContains(t, err, "not found")
	_, err = m.Login(context.Background(), testOwner, "cancel", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "cancel", SessionID: v.SessionID})
	require.NoError(t, err)
	view := awaitState(t, m, v.SessionID, "cancelled")
	require.Empty(t, view.AuthorizationURL)
	m.Close()
	_, err = m.Login(context.Background(), testOwner, "start-next", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.Error(t, err)
	expiring := fakeCLI(t, promptScript(), 100*time.Millisecond)
	exp := startLogin(t, expiring)
	awaitState(t, expiring, exp.SessionID, "expired")
}

func TestFailureDoesNotExposeRawOutput(t *testing.T) {
	m := fakeCLI(t, "echo 'secret-code secret-token' >&2\nexit 1\n", time.Minute)
	first := startLogin(t, m)
	view := awaitState(t, m, first.SessionID, "failed")
	require.Equal(t, "login_failed", view.ErrorCode)
	require.Empty(t, view.AuthorizationURL)
}

func TestRecentOutcomeSurvivesAnotherHarnessOrOwnerLogin(t *testing.T) {
	for _, next := range []struct {
		name    string
		harness string
		owner   control.Source
	}{
		{"another harness", "codex", testOwner},
		{"another owner", "claude", control.Source{Kind: control.SourceRemote, RemoteID: "remote-b"}},
	} {
		t.Run(next.name, func(t *testing.T) {
			m := fakeCLI(t, "exit 1\n", time.Minute)
			first := startLogin(t, m)
			awaitState(t, m, first.SessionID, "failed")
			_, err := m.Login(t.Context(), next.owner, "next-login", control.HarnessAuthLoginCommand{Harness: next.harness, Operation: "start"})
			require.NoError(t, err)
			view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude"})
			require.NoError(t, err)
			require.Equal(t, first.SessionID, view.SessionID)
			require.Equal(t, "failed", view.State)
			require.Equal(t, "login_failed", view.ErrorCode)
		})
	}
}

func TestURLParsingAcrossChunksAndTerminalEscapes(t *testing.T) {
	m := New(context.Background(), Options{})
	defer m.Close()
	s := &session{view: control.HarnessAuthView{State: "starting"}}
	w := &loginOutput{manager: m, session: s}
	text := "\x1b]8;;" + testURL + "\x07" + testURL + "\x1b]8;;\x07\nPaste code here if prompted > "
	for _, b := range []byte(text) {
		_, err := w.Write([]byte{b})
		require.NoError(t, err)
	}
	require.Equal(t, testURL, s.view.AuthorizationURL)
	require.Equal(t, "awaiting_code", s.view.State)
}
func TestURLParsingRejectsUntrustedHost(t *testing.T) {
	m := New(context.Background(), Options{})
	defer m.Close()
	s := &session{view: control.HarnessAuthView{State: "starting"}}
	w := &loginOutput{manager: m, session: s}
	_, err := w.Write([]byte("https://evil.test/cai/oauth/authorize?response_type=code&state=x&code_challenge=y\nPaste code here"))
	require.NoError(t, err)
	require.Empty(t, s.view.AuthorizationURL)
}

func TestLoggedOutStatusExitOneIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho '{\"loggedIn\":false,\"authMethod\":\"none\"}'\nexit 1\n"), 0700))
	m := New(context.Background(), Options{Command: path})
	defer m.Close()
	view, err := m.Status(context.Background(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude"})
	require.NoError(t, err)
	require.Equal(t, "logged_out", view.State)
}

// Opt-in smoke check: starts and cancels a real OAuth flow without submitting
// a code or changing credentials. The CLI may open the default browser.
func TestRealClaudeLoginPipes(t *testing.T) {
	path := os.Getenv("PAXD_TEST_CLAUDE_BINARY")
	if path == "" {
		t.Skip("set PAXD_TEST_CLAUDE_BINARY to exercise the installed Claude CLI")
	}
	m := New(context.Background(), Options{Command: path, TTL: 30 * time.Second})
	defer m.Close()
	v := startLogin(t, m)
	var view control.HarnessAuthView
	require.Eventually(t, func() bool {
		var err error
		view, err = m.Status(context.Background(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude", SessionID: v.SessionID})
		return err == nil && (view.State == "awaiting_code" || view.State == "failed")
	}, 20*time.Second, 50*time.Millisecond)
	require.Equal(t, "awaiting_code", view.State)
	require.NotEmpty(t, view.AuthorizationURL)
	_, err := m.Login(context.Background(), testOwner, "cancel", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "cancel", SessionID: v.SessionID})
	require.NoError(t, err)
}

func TestLoginCanBeResumedByHarnessWithFiveMinuteDeadline(t *testing.T) {
	m := fakeCLI(t, promptScript(), 0)
	first := startLogin(t, m)
	expires, err := time.Parse(time.RFC3339, first.ExpiresAt)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), expires, 2*time.Second)
	awaitState(t, m, first.SessionID, "awaiting_code")
	resumed, err := m.Login(t.Context(), testOwner, "another-start", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.NoError(t, err)
	require.Equal(t, first.SessionID, resumed.SessionID)
	require.Equal(t, first.ExpiresAt, resumed.ExpiresAt)
	_, err = m.Login(t.Context(), control.Source{Kind: control.SourceRemote, RemoteID: "other"}, "foreign-start", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.ErrorContains(t, err, "already running")
	state, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude"})
	require.NoError(t, err)
	require.Equal(t, first.SessionID, state.SessionID)
	_, err = m.Login(t.Context(), testOwner, "submit-without-session", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "submit", Code: "test-code#test-state"})
	require.NoError(t, err)
	awaitState(t, m, first.SessionID, "succeeded")
}

func TestClaudeConsoleLoginUsesNativeMethodAndCannotBeRetargeted(t *testing.T) {
	command := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
if [ "$1 $2" = 'auth status' ]; then echo '{"loggedIn":true,"authMethod":"api_key"}'; exit 0; fi
[ "$1 $2 $3" = 'auth login --console' ] || exit 2
printf 'https://platform.claude.com/oauth/authorize?response_type=code&state=state&code_challenge=challenge\nPaste code here > '
IFS= read -r code
[ "$code" = 'console-code#state' ]
`
	require.NoError(t, os.WriteFile(command, []byte(script), 0700))
	m := New(t.Context(), Options{Command: command})
	t.Cleanup(m.Close)
	first, err := m.Login(t.Context(), testOwner, "console", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start", Method: "console"})
	require.NoError(t, err)
	awaitState(t, m, first.SessionID, "awaiting_code")
	_, err = m.Login(t.Context(), testOwner, "switch", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start", Method: "subscription"})
	require.Error(t, err)
	_, err = m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "codex", SessionID: first.SessionID})
	require.Error(t, err)
	_, err = m.Login(t.Context(), testOwner, "submit", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "submit", SessionID: first.SessionID, Code: "console-code#state"})
	require.NoError(t, err)
	require.Equal(t, "api_key", awaitState(t, m, first.SessionID, "succeeded").AuthMethod)
}
