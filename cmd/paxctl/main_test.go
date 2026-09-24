package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/remotelogin"
	"github.com/pax-beehive/paxd/internal/remotesecrets"
	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/pax-beehive/paxd/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPaxctlStatusPrintsDaemonStatus(t *testing.T) {
	client := &fakeControlClient{
		status: control.QueryResult{Status: &control.DaemonStatus{
			Phase: "running",
			Remotes: []control.RemoteStatusView{{
				RemoteID: "default",
				Phase:    "connected",
			}},
			AgentConnections: []control.AgentStatusView{{
				ConnectionID: "conn_work",
				Phase:        "running",
			}},
		}},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"status"}, &stdout, &stderr)
	require.NoError(t, err)

	assert.Contains(t, stdout.String(), "DAEMON")
	assert.Contains(t, stdout.String(), "running")
	assert.Contains(t, stdout.String(), "default")
	assert.Contains(t, stdout.String(), "conn_work")
}

func TestPaxctlRemotesListRestartAndDisconnectUseLocalAPI(t *testing.T) {
	client := &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{{
			Remote: control.Remote{ID: "default", Name: "Default", CloudAPIURL: "https://manager.example.test"},
		}}}},
		ack: control.CommandAck{OK: true, Status: control.CommandStatusReceived},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()

	var stdout, stderr bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"remotes", "list"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "default")
	assert.Contains(t, stdout.String(), "https://manager.example.test")

	stdout.Reset()
	require.NoError(t, run(context.Background(), []string{"remotes", "restart", "default"}, &stdout, &stderr))
	assert.Equal(t, []string{"default"}, client.restartedRemotes)
	assert.Contains(t, stdout.String(), "Restart requested")

	stdout.Reset()
	require.NoError(t, run(context.Background(), []string{"remotes", "disconnect", "default"}, &stdout, &stderr))
	assert.Equal(t, []string{"default"}, client.deletedRemotes)
	assert.Equal(t, []bool{false}, client.deletedRemoteCascades)
	assert.Contains(t, stdout.String(), "Disconnected")
}

func TestPaxctlRemotesRemoveCascadesAndDeletesSecret(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := (remotesecrets.Store{}).StoreNodeKey(context.Background(), "staging", "node-secret")
	require.NoError(t, err)
	client := &fakeControlClient{ack: control.CommandAck{OK: true, Status: control.CommandStatusReceived}}
	restore := stubPaxctlControlClient(t, client)
	defer restore()

	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{"remotes", "remove", "staging"}, &stdout, &stderr)

	require.NoError(t, err)
	assert.Equal(t, []string{"staging"}, client.deletedRemotes)
	assert.Equal(t, []bool{true}, client.deletedRemoteCascades)
	assert.Contains(t, stdout.String(), "Removed remote staging")
	assert.NoDirExists(t, filepath.Join(home, ".paxd", "secrets", "remotes", "staging"))
}

func TestSelectRemoteDefaultsOnlyWhenExactlyOneRemote(t *testing.T) {
	remoteID, err := selectRemote(context.Background(), &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{{
			Remote: control.Remote{ID: "prod"},
		}}}},
	}, "")
	require.NoError(t, err)
	assert.Equal(t, "prod", remoteID)

	_, err = selectRemote(context.Background(), &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{}},
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "paxd login")

	_, err = selectRemote(context.Background(), &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{
			{Remote: control.Remote{ID: "prod"}},
			{Remote: control.Remote{ID: "staging"}},
		}}},
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--remote")
}

func TestPaxctlAgentsCreateUsesSingleRemoteAndHarnessCommand(t *testing.T) {
	client := &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{{
			Remote: control.Remote{ID: "prod", CloudAPIURL: "https://manager.example.test"},
			Auth: &control.RemoteAuthView{
				Kind:            control.RemoteAuthCloudflareAccess,
				ClientID:        "cf-client",
				ClientSecretRef: "file:/tmp/cf-secret",
			},
		}}}},
		harnesses: control.QueryResult{Harnesses: &control.ListHarnessesResult{Items: []control.HarnessView{{
			Harness: "codex",
			Command: []string{"codex-acp"},
		}}}},
		ack: control.CommandAck{OK: true, Status: control.CommandStatusReceived, TargetID: "conn_work"},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()
	restoreNodeKey := stubPaxctlNodeKey(t, func(ctx context.Context, remoteID string) (string, error) {
		assert.Equal(t, "prod", remoteID)
		return "node-secret", nil
	})
	defer restoreNodeKey()
	restoreRegister := stubPaxctlAgentRegistration(t, func(ctx context.Context, remote control.RemoteView, nodeKey string, name string, agentType string) (string, error) {
		assert.Equal(t, "prod", remote.Remote.ID)
		assert.Equal(t, "https://manager.example.test", remote.Remote.CloudAPIURL)
		require.NotNil(t, remote.Auth)
		assert.Equal(t, control.RemoteAuthCloudflareAccess, remote.Auth.Kind)
		assert.Equal(t, "cf-client", remote.Auth.ClientID)
		assert.Equal(t, "file:/tmp/cf-secret", remote.Auth.ClientSecretRef)
		assert.Equal(t, "node-secret", nodeKey)
		assert.Equal(t, "work", name)
		assert.Equal(t, "codex", agentType)
		return "agent_cloud_123", nil
	})
	defer restoreRegister()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"agents", "create", "--harness", "codex", "--name", "work"}, &stdout, &stderr)
	require.NoError(t, err)

	require.Len(t, client.createdAgents, 1)
	created := client.createdAgents[0]
	assert.Equal(t, "prod", created.RemoteID)
	assert.Equal(t, "work", created.Name)
	assert.Equal(t, "agent_cloud_123", created.CloudAgentID)
	assert.Equal(t, "work", created.InstanceID)
	assert.Equal(t, "codex", created.Harness)
	assert.Equal(t, "codex", created.AgentType)
	assert.Equal(t, []string{"codex-acp"}, created.Command)
	assert.Contains(t, stdout.String(), "Agent create requested")
	assert.Contains(t, stdout.String(), "agent_cloud_123")
}

func TestRegisterCloudAgentIgnoresLegacyCloudflareAccessAuth(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "cf-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/node/agents/register", r.URL.Path)
		assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
		assert.Equal(t, "", r.Header.Get("CF-Access-Client-Id"))
		assert.Equal(t, "", r.Header.Get("CF-Access-Client-Secret"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"agent_id":"agent_cloud_123"}}`))
	}))
	defer server.Close()

	agentID, err := registerCloudAgent(context.Background(), control.RemoteView{
		Remote: control.Remote{ID: "prod", CloudAPIURL: server.URL},
		Auth: &control.RemoteAuthView{
			Kind:            control.RemoteAuthCloudflareAccess,
			ClientID:        "cf-client",
			ClientSecretRef: "file:" + secretPath,
		},
	}, "node-secret", "work", "codex")

	require.NoError(t, err)
	assert.Equal(t, "agent_cloud_123", agentID)
}

func TestPaxctlAgentsCreateDiscoversHarnessCommandWhenCacheMissing(t *testing.T) {
	client := &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{{
			Remote: control.Remote{ID: "prod", CloudAPIURL: "https://manager.example.test"},
		}}}},
		harnesses: control.QueryResult{Harnesses: &control.ListHarnessesResult{}},
		discovered: control.QueryResult{Harnesses: &control.ListHarnessesResult{Items: []control.HarnessView{{
			Harness: "hermes",
			State:   "available",
			Command: []string{"hermes", "acp"},
		}}}},
		ack: control.CommandAck{OK: true, Status: control.CommandStatusReceived, TargetID: "conn_hermes"},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()
	restoreNodeKey := stubPaxctlNodeKey(t, func(ctx context.Context, remoteID string) (string, error) {
		assert.Equal(t, "prod", remoteID)
		return "node-secret", nil
	})
	defer restoreNodeKey()
	restoreRegister := stubPaxctlAgentRegistration(t, func(ctx context.Context, remote control.RemoteView, nodeKey string, name string, agentType string) (string, error) {
		return "agent_cloud_hermes", nil
	})
	defer restoreRegister()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"agents", "create", "--harness", "hermes", "--name", "hermes"}, &stdout, &stderr)
	require.NoError(t, err)

	assert.Equal(t, control.DiscoverHarnessesQuery{Names: []string{"hermes"}}, client.discoverQuery)
	require.Len(t, client.createdAgents, 1)
	assert.Equal(t, []string{"hermes", "acp"}, client.createdAgents[0].Command)
}

func TestPaxctlAgentsCreateRejectsUnknownHarnessBeforeRegistering(t *testing.T) {
	client := &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{{
			Remote: control.Remote{ID: "prod", CloudAPIURL: "https://manager.example.test"},
		}}}},
		harnesses:  control.QueryResult{Harnesses: &control.ListHarnessesResult{}},
		discovered: control.QueryResult{Harnesses: &control.ListHarnessesResult{}},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()
	restoreRegister := stubPaxctlAgentRegistration(t, func(ctx context.Context, remote control.RemoteView, nodeKey string, name string, agentType string) (string, error) {
		t.Fatalf("registerCloudAgent should not be called for an unknown harness")
		return "", nil
	})
	defer restoreRegister()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"agents", "create", "--harness", "unknown", "--name", "work"}, &stdout, &stderr)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "harness \"unknown\" is not known")
	assert.Empty(t, client.createdAgents)
}

func TestPaxctlAgentsCreateDoesNotCreateLocalConnectionWhenCloudRegistrationFails(t *testing.T) {
	client := &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{{
			Remote: control.Remote{ID: "prod", CloudAPIURL: "https://manager.example.test"},
		}}}},
		harnesses: control.QueryResult{Harnesses: &control.ListHarnessesResult{Items: []control.HarnessView{{
			Harness: "codex",
			State:   "available",
			Command: []string{"codex-acp"},
		}}}},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()
	restoreNodeKey := stubPaxctlNodeKey(t, func(ctx context.Context, remoteID string) (string, error) {
		return "node-secret", nil
	})
	defer restoreNodeKey()
	restoreRegister := stubPaxctlAgentRegistration(t, func(ctx context.Context, remote control.RemoteView, nodeKey string, name string, agentType string) (string, error) {
		return "", errors.New("cloud unavailable")
	})
	defer restoreRegister()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"agents", "create", "--harness", "codex", "--name", "work"}, &stdout, &stderr)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cloud unavailable")
	assert.Empty(t, client.createdAgents)
}

func TestPaxctlAgentsCreateRequiresRemoteWhenMultipleRemotes(t *testing.T) {
	client := &fakeControlClient{
		remotes: control.QueryResult{Remotes: &control.ListRemotesResult{Items: []control.RemoteView{
			{Remote: control.Remote{ID: "prod"}},
			{Remote: control.Remote{ID: "staging"}},
		}}},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"agents", "create", "--harness", "codex", "--name", "work"}, &stdout, &stderr)
	require.Error(t, err)
	assert.Empty(t, client.createdAgents)
	assert.Contains(t, err.Error(), "--remote")
}

func TestPaxctlAgentsListRestartStopUseLocalAPI(t *testing.T) {
	client := &fakeControlClient{
		agents: control.QueryResult{AgentConnections: &control.ListAgentConnectionsResult{Items: []control.AgentConnectionView{{
			ID:           "conn_work",
			RemoteID:     "prod",
			Name:         "work",
			Harness:      "codex",
			DesiredState: control.DesiredStateRunning,
		}}}},
		ack: control.CommandAck{OK: true, Status: control.CommandStatusReceived},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()

	var stdout, stderr bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"agents", "list"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "conn_work")

	stdout.Reset()
	require.NoError(t, run(context.Background(), []string{"agents", "restart", "work"}, &stdout, &stderr))
	assert.Equal(t, []string{"conn_work"}, client.restartedAgents)

	stdout.Reset()
	require.NoError(t, run(context.Background(), []string{"agents", "stop", "work"}, &stdout, &stderr))
	require.Len(t, client.updatedAgents, 1)
	assert.Equal(t, "conn_work", client.updatedAgents[0].ConnectionID)
	require.NotNil(t, client.updatedAgents[0].DesiredState)
	assert.Equal(t, control.DesiredStateStopped, *client.updatedAgents[0].DesiredState)

	stdout.Reset()
	require.NoError(t, run(context.Background(), []string{"agents", "remove", "work"}, &stdout, &stderr))
	assert.Equal(t, []string{"conn_work"}, client.deletedAgents)
	assert.Contains(t, stdout.String(), "Remove requested")
}

func TestPaxctlAgentsNameOrIDResolutionHandlesMissingAndAmbiguous(t *testing.T) {
	client := &fakeControlClient{agents: control.QueryResult{AgentConnections: &control.ListAgentConnectionsResult{Items: []control.AgentConnectionView{
		{ID: "conn_1", Name: "work"},
		{ID: "conn_2", Name: "work"},
	}}}}

	_, err := resolveAgentConnectionID(context.Background(), client, "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	_, err = resolveAgentConnectionID(context.Background(), client, "work")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")

	got, err := resolveAgentConnectionID(context.Background(), client, "conn_1")
	require.NoError(t, err)
	assert.Equal(t, "conn_1", got)
}

func TestPaxctlHarnessesListAndDiscoverUseLocalAPI(t *testing.T) {
	client := &fakeControlClient{
		harnesses: control.QueryResult{Harnesses: &control.ListHarnessesResult{Items: []control.HarnessView{{
			Harness:     "codex",
			DisplayName: "Codex",
			State:       "available",
			InstallHint: "install codex-acp or use fallback: npx -y @agentclientprotocol/codex-acp",
		}}}},
		discovered: control.QueryResult{Harnesses: &control.ListHarnessesResult{Items: []control.HarnessView{{
			Harness: "claude",
			State:   "missing",
		}}}},
	}
	restore := stubPaxctlControlClient(t, client)
	defer restore()

	var stdout, stderr bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"harnesses", "list"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "codex")
	assert.Contains(t, stdout.String(), "npx -y @agentclientprotocol/codex-acp")

	stdout.Reset()
	require.NoError(t, run(context.Background(), []string{"harnesses", "discover", "--probe", "claude"}, &stdout, &stderr))
	assert.Equal(t, control.DiscoverHarnessesQuery{Probe: true, Names: []string{"claude"}}, client.discoverQuery)
	assert.Contains(t, stdout.String(), "claude")
}

func TestPaxctlRemotesLoginCommitsThroughLocalAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	client := &fakeControlClient{ack: control.CommandAck{OK: true, Status: control.CommandStatusReceived}}
	restoreClient := stubPaxctlControlClient(t, client)
	defer restoreClient()
	restoreLogin := stubPaxctlRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		assert.Equal(t, "staging", spec.RemoteID)
		assert.Equal(t, "https://wsapi.paxworkspace.net", spec.CloudAPIURL)
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_staging",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restoreLogin()

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"remotes", "login", "staging"}, &stdout, &stderr)
	require.NoError(t, err)

	require.Len(t, client.createdRemotes, 1)
	created := client.createdRemotes[0]
	assert.Equal(t, "staging", created.Remote.ID)
	assert.Equal(t, "node_staging", created.Remote.NodeID)
	assert.Contains(t, created.CloudAPIKeyRef, ".paxd/secrets/remotes/staging/node_key")
	assert.NotContains(t, created.CloudAPIKeyRef, "node-secret")
	assert.Contains(t, stdout.String(), "Remote staging login committed")
}

func TestPaxctlLoginCloudClientIgnoresCloudflareAccessEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAX_CLOUD_CF_CLIENT_ID", "cf-client")
	t.Setenv("PAX_CLOUD_CF_CLIENT_SECRET", "cf-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/node/registration/start", r.URL.Path)
		assert.Equal(t, "", r.Header.Get("CF-Access-Client-Id"))
		assert.Equal(t, "", r.Header.Get("CF-Access-Client-Secret"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"registration_id":"reg_1","pair_code":"CODE","poll_token":"poll","verification_uri":"https://example.test","expires_in":300,"interval":5}}`))
	}))
	defer server.Close()

	client, err := paxctlLoginCloudClient(server.URL)
	require.NoError(t, err)
	resp, err := client.StartNodeRegistration(&cloud.StartNodeRegistrationRequest{
		Hostname: "host",
		OS:       "linux",
		Arch:     "amd64",
	})

	require.NoError(t, err)
	assert.Equal(t, "CODE", resp.PairCode)
}

func TestPaxctlRemotesLoginDoesNotCommitWhenLoginFails(t *testing.T) {
	client := &fakeControlClient{}
	restoreClient := stubPaxctlControlClient(t, client)
	defer restoreClient()
	restoreLogin := stubPaxctlRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{}, errors.New("denied")
	})
	defer restoreLogin()

	var stdout bytes.Buffer
	err := remotesLogin(context.Background(), client, &stdout, "staging", "https://manager.example.test")

	require.Error(t, err)
	assert.Empty(t, client.createdRemotes)
}

func TestPaxctlRemotesLoginSurfacesLocalAPICommitError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	client := &fakeControlClient{createRemoteErr: errors.New("daemon down")}
	restoreClient := stubPaxctlControlClient(t, client)
	defer restoreClient()
	restoreLogin := stubPaxctlRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_staging",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restoreLogin()

	var stdout bytes.Buffer
	err := remotesLogin(context.Background(), client, &stdout, "staging", "https://manager.example.test")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "paxd setup")
}

func TestPaxctlControlCommandsValidateRequiredArgs(t *testing.T) {
	client := &fakeControlClient{}
	var stdout bytes.Buffer

	require.Error(t, remotesLogin(context.Background(), client, &stdout, "", ""))
	require.Error(t, remotesRestart(context.Background(), client, &stdout, ""))
	require.Error(t, remotesDisconnect(context.Background(), client, &stdout, ""))
	require.Error(t, remotesRemove(context.Background(), client, &stdout, ""))
	require.Error(t, agentConnectionRestart(context.Background(), client, &stdout, ""))
	require.Error(t, agentConnectionStop(context.Background(), client, &stdout, ""))
	require.Error(t, agentConnectionRemove(context.Background(), client, &stdout, ""))
}

func TestPaxctlControlHelpersSurfaceErrorsAndNilResults(t *testing.T) {
	err := queryOK(control.QueryResult{Error: &control.ControlError{Code: "boom", Message: "failed"}})
	require.Error(t, err)

	err = ackOK(control.CommandAck{Status: control.CommandStatusRejected, Error: &control.ControlError{Code: "bad", Message: "rejected"}})
	require.Error(t, err)

	err = ackOK(control.CommandAck{Status: control.CommandStatusFailed})
	require.Error(t, err)

	err = localAPIGuidance(errors.New("dial unix failed"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "paxd setup")
	assert.NoError(t, localAPIGuidance(nil))

	assert.Nil(t, remoteItems(control.QueryResult{}))
	assert.Nil(t, agentItems(control.QueryResult{}))
	assert.Nil(t, harnessItems(control.QueryResult{}))
	assert.NoError(t, ackOK(control.CommandAck{Status: control.CommandStatusReceived}))
}

func TestPaxctlControlCommandsSurfaceLocalAPIErrors(t *testing.T) {
	boom := errors.New("daemon unavailable")
	client := &fakeControlClient{
		statusErr:    boom,
		remotesErr:   boom,
		agentsErr:    boom,
		harnessesErr: boom,
		discoverErr:  boom,
	}
	var stdout bytes.Buffer

	require.Error(t, statusCommand(context.Background(), client, &stdout))
	require.Error(t, remotesList(context.Background(), client, &stdout, false))
	_, err := selectRemote(context.Background(), client, "")
	require.Error(t, err)
	require.Error(t, agentConnectionsList(context.Background(), client, &stdout, false))
	require.Error(t, harnessesList(context.Background(), client, &stdout, false))
	require.Error(t, harnessesDiscover(context.Background(), client, &stdout, control.DiscoverHarnessesQuery{}))

	_, err = selectRemote(context.Background(), client, "prod")
	require.Error(t, err)
}

func TestPaxctlListCommandsHandleEmptyResults(t *testing.T) {
	client := &fakeControlClient{
		status:    control.QueryResult{Status: &control.DaemonStatus{}},
		remotes:   control.QueryResult{Remotes: &control.ListRemotesResult{}},
		agents:    control.QueryResult{AgentConnections: &control.ListAgentConnectionsResult{}},
		harnesses: control.QueryResult{Harnesses: &control.ListHarnessesResult{}},
	}
	var stdout bytes.Buffer

	require.NoError(t, statusCommand(context.Background(), client, &stdout))
	stdout.Reset()
	require.NoError(t, remotesList(context.Background(), client, &stdout, false))
	assert.Contains(t, stdout.String(), "REMOTE")
	stdout.Reset()
	require.NoError(t, agentConnectionsList(context.Background(), client, &stdout, false))
	assert.Contains(t, stdout.String(), "AGENT")
	stdout.Reset()
	require.NoError(t, harnessesList(context.Background(), client, &stdout, false))
	assert.Contains(t, stdout.String(), "HARNESS")
}

func TestSessionsGetHTMLWritesFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{{
		SessionID: "codex:sess-1",
		NativeID:  "sess-1",
		Name:      "HTML session",
		UpdatedAt: "2026-06-18T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	session, err := store.FindSession(ctx, "codex:sess-1", "")
	if err != nil {
		t.Fatalf("FindSession() error = %v", err)
	}
	version, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		t.Fatalf("BeginSync() error = %v", err)
	}
	if err := store.CompleteSync(ctx, session.ID, version, []sessionstore.Element{{
		Seq:         1,
		Type:        "thinking",
		ContentText: strings.Repeat("long content ", 120),
	}}); err != nil {
		t.Fatalf("CompleteSync() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	outputPath := filepath.Join(dir, "session.html")
	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "sessions", "get", "codex:sess-1", "--format", "html", "--output", outputPath}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Wrote "+outputPath) {
		t.Fatalf("stdout = %q, want wrote path", stdout.String())
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(content), "<details>") {
		t.Fatalf("html does not contain folded details: %s", content)
	}
}

func TestSessionsListHTMLUsesLocalMetadataOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{{
		SessionID: "codex:sess-1",
		NativeID:  "sess-1",
		Name:      "Local metadata",
		UpdatedAt: "2026-06-18T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "sessions", "list", "-agents", "codex", "-limit", "10", "-format", "html"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "<title>paxctl sessions</title>") {
		t.Fatalf("stdout does not contain html title: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "codex:sess-1") {
		t.Fatalf("stdout does not contain session id: %s", stdout.String())
	}
}

func TestCapsulesCreateListGetRedactsMatchingSessionHistory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{{
		SessionID: "codex:sess-1",
		NativeID:  "sess-1",
		Name:      "Capsule source",
		UpdatedAt: "2026-06-18T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	session, err := store.FindSession(ctx, "codex:sess-1", "")
	if err != nil {
		t.Fatalf("FindSession() error = %v", err)
	}
	version, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		t.Fatalf("BeginSync() error = %v", err)
	}
	if err := store.CompleteSync(ctx, session.ID, version, []sessionstore.Element{{
		Seq:         1,
		Type:        "message",
		Role:        "user",
		StartedAt:   "2026-06-18T12:01:00Z",
		ContentText: "Capability injection needs token=secret123 preserved only as context.",
	}}); err != nil {
		t.Fatalf("CompleteSync() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "capsules", "create", "codex:sess-1", "--keyword", "capability injection"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("create error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "kcap_") {
		t.Fatalf("create stdout missing capsule id: %s", stdout.String())
	}

	stdout.Reset()
	err = run(ctx, []string{"--db", dbPath, "capsules", "list", "--format", "jsonl"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("list error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"keyword":"capability injection"`) {
		t.Fatalf("list stdout missing keyword: %s", stdout.String())
	}
	var listed map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &listed); err != nil {
		t.Fatalf("decode list jsonl: %v output=%s", err, stdout.String())
	}
	capsuleID, _ := listed["capsuleId"].(string)
	if capsuleID == "" {
		t.Fatalf("list json missing capsuleId: %#v", listed)
	}

	stdout.Reset()
	err = run(ctx, []string{"--db", dbPath, "capsules", "get", capsuleID}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("get error = %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Capability injection") || !strings.Contains(out, "[redacted]") {
		t.Fatalf("get stdout missing extracted redacted content: %s", out)
	}
	if strings.Contains(out, "secret123") {
		t.Fatalf("get stdout leaked secret: %s", out)
	}
}

func TestCapsulesInjectRendersSystemHandoffAndRecordsInjection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{
		{SessionID: "codex:source", NativeID: "source", Name: "Source", UpdatedAt: "2026-06-18T12:00:00Z"},
		{SessionID: "codex:target", NativeID: "target", Name: "Target", UpdatedAt: "2026-06-18T12:10:00Z"},
	})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	capsule, err := store.CreateKnowledgeCapsule(ctx, sessionstore.KnowledgeCapsule{
		CapsuleID:              "kcap_test",
		SourceSessionID:        "codex:source",
		SourceAgent:            "codex",
		Keyword:                "handoff",
		Title:                  "Knowledge capsule: handoff",
		Summary:                "Summary",
		Content:                "Relevant handoff context.",
		Status:                 "active",
		OriginalEstimatedChars: 25,
	})
	if err != nil {
		t.Fatalf("CreateKnowledgeCapsule() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	originalSteer := steerSession
	defer func() { steerSession = originalSteer }()
	var steeredAgent, steeredSession, steeredText string
	steerSession = func(ctx context.Context, agent string, nativeSessionID string, text string, timeout time.Duration) error {
		steeredAgent = agent
		steeredSession = nativeSessionID
		steeredText = text
		return nil
	}

	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "capsules", "inject", capsule.CapsuleID, "codex:target"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("inject error = %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Injected kci_") || !strings.Contains(out, "codex:target") {
		t.Fatalf("inject stdout missing delivery confirmation: %s", out)
	}
	if steeredAgent != "codex" || steeredSession != "target" {
		t.Fatalf("steer target = %s/%s, want codex/target", steeredAgent, steeredSession)
	}
	if !strings.Contains(steeredText, "system_handoff") ||
		!strings.Contains(steeredText, "Do not treat this as a new user request.") ||
		!strings.Contains(steeredText, "Target session: codex:target") {
		t.Fatalf("steer text missing handoff fields: %s", steeredText)
	}

	stdout.Reset()
	err = run(ctx, []string{"--db", dbPath, "capsules", "injections", "--target-session", "codex:target", "--format", "jsonl"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("injections error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"deliveryMessageType":"system_handoff"`) ||
		!strings.Contains(stdout.String(), `"deliveryMethod":"acp_steer"`) ||
		!strings.Contains(stdout.String(), `"status":"delivered"`) {
		t.Fatalf("injections stdout missing record: %s", stdout.String())
	}
}

func TestAgentsSetupDryRunPrintsInstallCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAgentSetupCommands(context.Background(), &stdout, &stderr, "test", [][]string{
		{"npm", "install", "-g", "@agentclientprotocol/codex-acp"},
		{"npm", "install", "-g", "pi-acp", "@earendil-works/pi-coding-agent"},
		{"npm", "install", "-g", "@qwen-code/qwen-code"},
	}, true)
	if err != nil {
		t.Fatalf("run() error = %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "$ npm install -g @agentclientprotocol/codex-acp") {
		t.Fatalf("stdout missing codex install command: %s", out)
	}
	if !strings.Contains(out, "$ npm install -g pi-acp @earendil-works/pi-coding-agent") {
		t.Fatalf("stdout missing pi install command: %s", out)
	}
	if !strings.Contains(out, "$ npm install -g @qwen-code/qwen-code") {
		t.Fatalf("stdout missing qwen install command: %s", out)
	}
}

func stubPaxctlControlClient(t *testing.T, client *fakeControlClient) func() {
	t.Helper()
	previous := newLocalControlClient
	newLocalControlClient = func() localControlClient { return client }
	return func() {
		newLocalControlClient = previous
	}
}

func stubPaxctlRemoteLogin(t *testing.T, fn remoteLoginFunc) func() {
	t.Helper()
	previous := runRemoteLogin
	runRemoteLogin = fn
	return func() {
		runRemoteLogin = previous
	}
}

func stubPaxctlNodeKey(t *testing.T, fn func(context.Context, string) (string, error)) func() {
	t.Helper()
	previous := loadRemoteNodeKey
	loadRemoteNodeKey = fn
	return func() {
		loadRemoteNodeKey = previous
	}
}

func stubPaxctlAgentRegistration(t *testing.T, fn func(context.Context, control.RemoteView, string, string, string) (string, error)) func() {
	t.Helper()
	previous := registerCloudAgent
	registerCloudAgent = fn
	return func() {
		registerCloudAgent = previous
	}
}

type fakeControlClient struct {
	status     control.QueryResult
	remotes    control.QueryResult
	agents     control.QueryResult
	harnesses  control.QueryResult
	discovered control.QueryResult
	ack        control.CommandAck

	createdRemotes        []control.CreateRemoteCommand
	restartedRemotes      []string
	deletedRemotes        []string
	deletedRemoteCascades []bool

	createdAgents   []control.CreateAgentConnectionCommand
	updatedAgents   []control.UpdateAgentConnectionCommand
	restartedAgents []string
	deletedAgents   []string

	discoverQuery control.DiscoverHarnessesQuery

	statusErr    error
	remotesErr   error
	agentsErr    error
	harnessesErr error
	discoverErr  error

	createRemoteErr error
}

func (c *fakeControlClient) GetStatus(ctx context.Context) (control.QueryResult, error) {
	if c.statusErr != nil {
		return control.QueryResult{}, c.statusErr
	}
	return c.status, nil
}

func (c *fakeControlClient) ListRemotes(ctx context.Context, includeDisabled bool) (control.QueryResult, error) {
	if c.remotesErr != nil {
		return control.QueryResult{}, c.remotesErr
	}
	return c.remotes, nil
}

func (c *fakeControlClient) CreateRemote(ctx context.Context, commandID string, cmd control.CreateRemoteCommand) (control.CommandAck, error) {
	if c.createRemoteErr != nil {
		return control.CommandAck{}, c.createRemoteErr
	}
	c.createdRemotes = append(c.createdRemotes, cmd)
	return c.ackWithCommandID(commandID), nil
}

func (c *fakeControlClient) RestartRemote(ctx context.Context, commandID string, remoteID string) (control.CommandAck, error) {
	c.restartedRemotes = append(c.restartedRemotes, remoteID)
	return c.ackWithCommandID(commandID), nil
}

func (c *fakeControlClient) DeleteRemote(ctx context.Context, commandID string, remoteID string, cascadeAgentConnections bool) (control.CommandAck, error) {
	c.deletedRemotes = append(c.deletedRemotes, remoteID)
	c.deletedRemoteCascades = append(c.deletedRemoteCascades, cascadeAgentConnections)
	return c.ackWithCommandID(commandID), nil
}

func (c *fakeControlClient) ListAgentConnections(ctx context.Context, includeDisabled bool) (control.QueryResult, error) {
	if c.agentsErr != nil {
		return control.QueryResult{}, c.agentsErr
	}
	return c.agents, nil
}

func (c *fakeControlClient) CreateAgentConnection(ctx context.Context, commandID string, cmd control.CreateAgentConnectionCommand) (control.CommandAck, error) {
	c.createdAgents = append(c.createdAgents, cmd)
	return c.ackWithCommandID(commandID), nil
}

func (c *fakeControlClient) UpdateAgentConnection(ctx context.Context, commandID string, connectionID string, cmd control.UpdateAgentConnectionCommand) (control.CommandAck, error) {
	cmd.ConnectionID = connectionID
	c.updatedAgents = append(c.updatedAgents, cmd)
	return c.ackWithCommandID(commandID), nil
}

func (c *fakeControlClient) RestartAgentConnection(ctx context.Context, commandID string, connectionID string) (control.CommandAck, error) {
	c.restartedAgents = append(c.restartedAgents, connectionID)
	return c.ackWithCommandID(commandID), nil
}

func (c *fakeControlClient) DeleteAgentConnection(ctx context.Context, commandID string, connectionID string) (control.CommandAck, error) {
	c.deletedAgents = append(c.deletedAgents, connectionID)
	return c.ackWithCommandID(commandID), nil
}

func (c *fakeControlClient) ListHarnesses(ctx context.Context, includeMissing bool) (control.QueryResult, error) {
	if c.harnessesErr != nil {
		return control.QueryResult{}, c.harnessesErr
	}
	return c.harnesses, nil
}

func (c *fakeControlClient) DiscoverHarnesses(ctx context.Context, query control.DiscoverHarnessesQuery) (control.QueryResult, error) {
	if c.discoverErr != nil {
		return control.QueryResult{}, c.discoverErr
	}
	c.discoverQuery = query
	return c.discovered, nil
}

func (c *fakeControlClient) ackWithCommandID(commandID string) control.CommandAck {
	ack := c.ack
	if ack.Status == "" {
		ack = control.CommandAck{OK: true, Status: control.CommandStatusReceived}
	}
	ack.CommandID = commandID
	return ack
}
