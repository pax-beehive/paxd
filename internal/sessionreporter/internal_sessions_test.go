package sessionreporter

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/pax-beehive/paxd/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunOnceDoesNotReportSecretaryWorkSessions(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		name := "manager identity"
		if resolved {
			name = "native identity resolved to manager"
		}
		t.Run(name, func(t *testing.T) {
			source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
				Spec: runtimes.AgentConnectionSpec{
					ConnectionID: "connection", RemoteID: "remote", CloudAgentID: "agent",
				},
			}}}
			sessions := []model.SessionInfo{
				{SessionID: "sess_secretary_work", NativeID: "internal-native"},
				{SessionID: "sess_ordinary", NativeID: "ordinary-native"},
			}
			opts := Options{RuntimeSource: source}
			if resolved {
				sessions[0].SessionID = "dscode:internal-native"
				opts.RouteResolver = &fakeRouteResolver{routes: map[string]routeResolution{
					"connection/internal-native": {managerSessionID: "sess_secretary_work", found: true},
					"connection/ordinary-native": {managerSessionID: "sess_ordinary", found: true},
				}}
			}
			opts.Scanner = &fakeScanner{sessions: sessions}
			reporter := &fakeCloudReporter{}
			opts.Reporter = reporter

			require.NoError(t, New(opts).RunOnce(t.Context()))

			require.Len(t, reporter.calls, 1)
			require.Len(t, reporter.calls[0].sessions, 1)
			assert.Equal(t, "sess_ordinary", reporter.calls[0].sessions[0].SessionID)
		})
	}
}

func TestDefaultScannerSkipsSecretaryHistoryBeforeApplyingBatchLimit(t *testing.T) {
	command := filepath.Join(t.TempDir(), "paxl")
	script := `#!/bin/sh
if [ "$2" = list ]; then
  printf '{"id":"dscode:internal","agent":"dscode","nativeId":"internal"}\n'
  if [ "$7" != --limit ]; then
    printf '{"id":"dscode:ordinary","agent":"dscode","nativeId":"ordinary"}\n'
    printf '{"id":"dscode:older","agent":"dscode","nativeId":"older"}\n'
  fi
else
  printf '%s\n' "$7" >> "$PWD/history-reads"
  printf '{"sessionId":"%s","seq":1,"type":"message","role":"user","contentText":"User content"}\n' "$7"
fi
`
	require.NoError(t, os.WriteFile(command, []byte(script), 0o755))
	root := t.TempDir()
	scanner := DefaultScanner{
		PaxlCommand: []string{command},
		RouteResolver: &fakeRouteResolver{routes: map[string]routeResolution{
			"connection/internal": {managerSessionID: "sess_secretary_work", found: true},
			"connection/ordinary": {managerSessionID: "sess_ordinary", found: true},
			"connection/older":    {managerSessionID: "sess_older", found: true},
		}},
	}

	sessions, err := scanner.ListSessions(t.Context(), SessionScannerSpec{
		ConnectionID: "connection", Harness: "dscode", WorkingDir: root, Limit: 1,
	})

	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "ordinary", sessions[0].NativeID)
	reads, err := os.ReadFile(filepath.Join(root, "history-reads"))
	require.NoError(t, err)
	assert.Equal(t, "dscode:ordinary\n", string(reads))
}

func TestDefaultScannerUsesDurableManagerIdentityForSecretaryClassification(t *testing.T) {
	for _, test := range []struct {
		name       string
		resolution routeResolution
		wantError  bool
		wantCount  int
	}{
		{name: "ordinary", resolution: routeResolution{managerSessionID: "sess_ordinary", found: true}, wantCount: 1},
		{name: "untracked", wantCount: 1},
		{name: "pending", resolution: routeResolution{found: true}, wantCount: 1},
		{name: "internal", resolution: routeResolution{managerSessionID: " sess_secretary_work ", found: true}},
		{name: "lookup unavailable", resolution: routeResolution{err: errors.New("route unavailable")}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scanner := DefaultScanner{
				PaxlCommand: []string{fakePaxlCommand(t)},
				RouteResolver: &fakeRouteResolver{routes: map[string]routeResolution{
					"connection/sess_paxl": test.resolution,
				}},
			}

			sessions, err := scanner.ListSessions(t.Context(), SessionScannerSpec{
				ConnectionID: "connection", Harness: "codex", Limit: 1,
			})

			if test.wantError {
				require.ErrorContains(t, err, "route unavailable")
			} else {
				require.NoError(t, err)
			}
			assert.Len(t, sessions, test.wantCount)
		})
	}
}

func TestRunOnceReportsUserSessionWithSecretaryLikeNativeIdentity(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
		Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "connection", RemoteID: "remote", CloudAgentID: "agent",
		},
	}}}
	reporter := &fakeCloudReporter{}
	service := New(Options{
		RuntimeSource: source,
		Scanner: &fakeScanner{sessions: []model.SessionInfo{{
			SessionID: "dscode:sess_secretary_user_chosen", NativeID: "sess_secretary_user_chosen",
		}}},
		Reporter: reporter,
		RouteResolver: &fakeRouteResolver{routes: map[string]routeResolution{
			"connection/sess_secretary_user_chosen": {managerSessionID: "sess_user", found: true},
		}},
	})

	require.NoError(t, service.RunOnce(t.Context()))

	require.Len(t, reporter.calls, 1)
	require.Len(t, reporter.calls[0].sessions, 1)
	assert.Equal(t, "sess_user", reporter.calls[0].sessions[0].SessionID)
}
