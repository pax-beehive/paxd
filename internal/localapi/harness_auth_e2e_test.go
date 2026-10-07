package localapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/harnessauth"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/stretchr/testify/require"
)

// Opt in with a freshly built paxl binary. The daemon control service is real;
// the Claude executable is fake, so this test never touches user credentials.
func TestPaxlLoginContinuesAcrossIndependentCLIProcesses(t *testing.T) {
	binary := os.Getenv("PAXL_TEST_BINARY")
	if binary == "" {
		t.Skip("set PAXL_TEST_BINARY to test the paxl/paxd Unix socket contract")
	}
	root, err := os.MkdirTemp("/tmp", "pax-auth-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	marker := filepath.Join(root, "authenticated")
	count := filepath.Join(root, "starts")
	command := filepath.Join(root, "claude")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$2" = status ]; then
 if [ -f %q ]; then
  echo '{"loggedIn":true,"authMethod":"oauth"}'
  exit 0
 fi
 echo '{"loggedIn":false,"authMethod":"none"}'
 exit 1
fi
echo start >> %q
printf 'https://claude.com/cai/oauth/authorize?response_type=code&state=test&code_challenge=test\n'
printf 'Paste code here if prompted > '
IFS= read -r code
[ "$code" = 'smoke-code#test' ] || exit 1
touch %q
`, marker, count, marker)
	require.NoError(t, os.WriteFile(command, []byte(script), 0700))
	manager := harnessauth.New(t.Context(), harnessauth.Options{Command: command})
	t.Cleanup(manager.Close)
	socket := filepath.Join(root, "paxd.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: localapi.NewHandler(control.NewService(control.ServiceOptions{HarnessAuth: manager})), ReadHeaderTimeout: time.Second}
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	go func() { _ = server.Serve(listener) }()
	run := func(action, input string, extra ...string) map[string]any {
		t.Helper()
		args := append([]string{"auth", action, "--harness", "claude", "--socket", socket, "--format", "json"}, extra...)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Stdin = strings.NewReader(input)
		raw, err := cmd.CombinedOutput()
		require.NoError(t, err, string(raw))
		var view map[string]any
		require.NoError(t, json.Unmarshal(raw, &view))
		require.NotContains(t, view, "session_id")
		require.NotContains(t, string(raw), "smoke-code")
		return view
	}
	require.Equal(t, "logged_out", run("status", "")["state"])
	first := run("login", "")
	require.Equal(t, "awaiting_code", first["state"])
	repeated := run("login", "")
	require.Equal(t, first["authorization_url"], repeated["authorization_url"])
	require.Equal(t, first["expires_at"], repeated["expires_at"])
	require.Equal(t, "awaiting_code", run("status", "")["state"])
	require.Equal(t, "succeeded", run("login", "smoke-code#test\n", "--code-stdin")["state"])
	require.Equal(t, true, run("status", "")["logged_in"])
	starts, err := os.ReadFile(count)
	require.NoError(t, err)
	require.Equal(t, "start\n", string(starts))
}

func TestPaxlCodexLoginAndCancellationAcrossProcesses(t *testing.T) {
	binary := os.Getenv("PAXL_TEST_BINARY")
	if binary == "" {
		t.Skip("set PAXL_TEST_BINARY to test the paxl/paxd Unix socket contract")
	}
	root, err := os.MkdirTemp("/tmp", "pax-codex-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	command := filepath.Join(root, "codex")
	gate := filepath.Join(root, "approved")
	method := filepath.Join(root, "method")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = 'login status' ]; then
 if [ -f %q ]; then cat %q >&2; exit 0; fi
 echo 'Not logged in' >&2; exit 1
fi
case "$2" in
 --device-auth)
  printf 'https://auth.openai.com/codex/device\nABCD-EFGHJ\n'
  while [ ! -f %q ]; do sleep 0.02; done
  echo 'Logged in using ChatGPT' > %q;;
 --with-api-key)
  value=$(cat)
  [ "$value" = 'private-api-key' ] || exit 2
  echo 'Logged in using an API key - private-api-key' > %q;;
 *) exit 2;;
esac
`, method, method, gate, method, method)
	require.NoError(t, os.WriteFile(command, []byte(script), 0700))
	manager := harnessauth.New(t.Context(), harnessauth.Options{Command: command})
	t.Cleanup(manager.Close)
	socket := filepath.Join(root, "paxd.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: localapi.NewHandler(control.NewService(control.ServiceOptions{HarnessAuth: manager})), ReadHeaderTimeout: time.Second}
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	go func() { _ = server.Serve(listener) }()
	run := func(action, input string, extra ...string) map[string]any {
		t.Helper()
		args := append([]string{"auth", action, "--harness", "codex", "--socket", socket, "--format", "json"}, extra...)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Stdin = strings.NewReader(input)
		raw, err := cmd.CombinedOutput()
		require.NoError(t, err, string(raw))
		require.NotContains(t, string(raw), "private-api-key")
		var view map[string]any
		require.NoError(t, json.Unmarshal(raw, &view))
		require.NotContains(t, view, "session_id")
		return view
	}
	require.Equal(t, "logged_out", run("status", "")["state"])
	first := run("login", "")
	require.Equal(t, "awaiting_browser", first["state"])
	require.Equal(t, "ABCD-EFGHJ", first["user_code"])
	require.Equal(t, first["expires_at"], run("login", "")["expires_at"])
	require.Equal(t, "cancelled", run("cancel", "")["state"])
	// Cancellation reaps asynchronously; wait for the child before starting again.
	require.Eventually(t, func() bool {
		view, err := manager.Login(t.Context(), control.Source{Kind: control.SourceLocal}, "restart", control.HarnessAuthLoginCommand{Harness: "codex", Operation: "start"})
		return err == nil && view.State == "starting"
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, os.WriteFile(gate, nil, 0600))
	require.Eventually(t, func() bool { return run("status", "")["state"] == "succeeded" }, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "api_key", run("login", "private-api-key\n", "--method", "api-key", "--secret-stdin")["auth_method"])
}
