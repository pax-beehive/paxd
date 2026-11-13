package harnessauth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func accountCLI(t *testing.T, script string) *Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700))
	m := New(t.Context(), Options{Command: path})
	t.Cleanup(m.Close)
	return m
}

func TestClaudeStatusReturnsOnlyReportedPublicAccountFields(t *testing.T) {
	m := accountCLI(t, `echo '{"loggedIn":true,"authMethod":"claude.ai","email":"owner@example.test","orgName":"Example Team","subscriptionType":"max","accessToken":"must-not-return"}'`)
	view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude"})
	require.NoError(t, err)
	require.NotNil(t, view.Account)
	assert.Equal(t, "owner@example.test", view.Account.Email)
	assert.Equal(t, "Example Team", view.Account.Organization)
	assert.Equal(t, "max", view.Account.Plan)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "must-not-return")
}

func TestClaudeStatusOmitsIdentityWhenLoggedOutOrFieldsAreInvalid(t *testing.T) {
	for _, output := range []string{
		`{"loggedIn":false,"email":"old@example.test"}`,
		`{"loggedIn":true}`,
		`{"loggedIn":true,"email":{"value":"not-a-string"},"subscriptionType":42}`,
		`{"loggedIn":true,"email":"bad\nvalue","orgName":"bad\u202ename","subscriptionType":"` + strings.Repeat("x", 129) + `"}`,
	} {
		m := accountCLI(t, "printf '%s' '"+output+"'\n")
		view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude"})
		require.NoError(t, err)
		assert.Nil(t, view.Account)
	}
}

func TestClaudeCompletedLoginReportsNativeAccount(t *testing.T) {
	m := accountCLI(t, `if [ "$2" = status ]; then echo '{"loggedIn":true,"authMethod":"claude.ai","email":"signed-in@example.test"}'; fi`)
	first, err := m.Login(t.Context(), testOwner, "account-login", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "start"})
	require.NoError(t, err)
	view := awaitHarnessState(t, m, "claude", first.SessionID, "succeeded")
	require.NotNil(t, view.Account)
	assert.Equal(t, "signed-in@example.test", view.Account.Email)
}

func TestCodexIdentityTimeoutPreservesNativeStatusAndStopsTheProbe(t *testing.T) {
	m := accountCLI(t, `if [ "$1 $2" = 'login status' ]; then echo 'Logged in using ChatGPT'; exit 0; fi
: > "$0.started"
sleep 10 &
wait`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		view control.HarnessAuthView
		err  error
	}
	done := make(chan result, 1)
	go func() {
		view, err := m.Status(ctx, testOwner, control.HarnessAuthStatusQuery{Harness: "codex"})
		done <- result{view, err}
	}()
	require.Eventually(t, func() bool { _, err := os.Stat(m.command + ".started"); return err == nil }, 2*time.Second, 5*time.Millisecond)
	started := time.Now()
	cancel()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, "logged_in", got.view.State)
		assert.Nil(t, got.view.Account)
		assert.Less(t, time.Since(started), 2*time.Second)
	case <-time.After(2 * time.Second):
		t.Fatal("Account probe did not stop after cancellation")
	}
}

func TestCodexStatusReadsChatGPTAccountWithoutRefreshingTokens(t *testing.T) {
	m := accountCLI(t, `
if [ "$1 $2" = 'login status' ]; then echo 'Logged in using ChatGPT'; exit 0; fi
[ "$1" = 'app-server' ] || exit 2
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r initialized
IFS= read -r request
case "$request" in *'"refreshToken":false'*) ;; *) exit 3 ;; esac
printf '%s\n' '{"method":"account/updated","params":{}}'
printf '%s\n' '{"id":"server-request","method":"unrelated/request"}'
printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","email":"codex@example.test","planType":"pro","accessToken":"must-not-return"}}}'
cat >/dev/null
`)
	view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "codex"})
	require.NoError(t, err)
	require.NotNil(t, view.Account)
	assert.Equal(t, "codex@example.test", view.Account.Email)
	assert.Equal(t, "pro", view.Account.Plan)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "must-not-return")
}

func TestCodexIdentityFailurePreservesNativeCredentialStatus(t *testing.T) {
	for name, reply := range map[string]string{
		"unavailable":     "exit 1",
		"malformed":       "echo 'not json'",
		"api key account": `echo '{"id":2,"result":{"account":{"type":"apiKey","email":"do-not-use@example.test"}}}'`,
		"null account":    `echo '{"id":2,"result":{"account":null}}'`,
		"rpc failure":     `echo '{"id":2,"error":{"message":"must-not-return"}}'`,
	} {
		t.Run(name, func(t *testing.T) {
			m := accountCLI(t, "if [ \"$1 $2\" = 'login status' ]; then echo 'Logged in using ChatGPT'; exit 0; fi\n"+
				"IFS= read -r line\necho '{\"id\":1,\"result\":{}}'\nIFS= read -r line\nIFS= read -r line\n"+reply+"\n")
			view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "codex"})
			require.NoError(t, err)
			assert.Equal(t, "logged_in", view.State)
			assert.Nil(t, view.Account)
		})
	}
}

func TestStatusAcceptsAgentTypesAndReportsUnsupportedOnes(t *testing.T) {
	m := accountCLI(t, `printf x >> "$0.calls"
echo '{"loggedIn":true,"authMethod":"claude.ai","email":"owner@example.test"}'`)
	view, err := m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "claude-code"})
	require.NoError(t, err)
	assert.Equal(t, "claude-code", view.Harness)
	assert.Equal(t, "logged_in", view.State)
	require.NotNil(t, view.Account)

	view, err = m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "kimi"})
	require.NoError(t, err)
	assert.Equal(t, control.HarnessAuthView{Harness: "kimi", State: "unsupported"}, view)
	_, err = m.Status(t.Context(), testOwner, control.HarnessAuthStatusQuery{Harness: "Not A Harness"})
	require.Error(t, err)

	calls, err := os.ReadFile(m.command + ".calls")
	require.NoError(t, err)
	assert.Equal(t, "x", string(calls), "only the claude-code status ran a native command")
	require.NoError(t, control.HarnessAuthLoginCommand{Harness: "claude-code", Operation: "start"}.Validate())
	require.Error(t, control.HarnessAuthLoginCommand{Harness: "kimi", Operation: "start"}.Validate())
}
