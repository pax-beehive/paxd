package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunStatusUsesLocalAPI(t *testing.T) {
	fake := &fakeClient{status: control.QueryResult{Type: control.QueryStatusGet, Status: &control.DaemonStatus{Phase: "running"}}}
	restore := replaceClientFactory(fake)
	defer restore()
	var stdout bytes.Buffer

	err := run(context.Background(), []string{"--debug-http", "http://127.0.0.1:1", "status"}, &stdout, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, 1, fake.statusCalls)
	assert.Contains(t, stdout.String(), `"phase": "running"`)
}

func TestRunHarnessDiscoverPassesProbeAndNames(t *testing.T) {
	fake := &fakeClient{}
	restore := replaceClientFactory(fake)
	defer restore()

	err := run(context.Background(), []string{"harnesses", "discover", "--probe", "--names", "codex,gemini"}, &bytes.Buffer{}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, control.DiscoverHarnessesQuery{Probe: true, Names: []string{"codex", "gemini"}}, fake.discoverQuery)
}

func TestRunLocalSessionsListPassesFilters(t *testing.T) {
	fake := &fakeClient{}
	restore := replaceClientFactory(fake)
	defer restore()

	err := run(context.Background(), []string{"local-sessions", "list", "--agent", "claude code", "--limit", "5"}, &bytes.Buffer{}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, control.ListLocalSessionsQuery{Agent: "claude code", Limit: 5}, fake.listSessionsQuery)
}

func TestRunListCommandsUseLocalAPI(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "remotes", args: []string{"remotes"}, want: "remotes"},
		{name: "agent connections", args: []string{"agent-connections"}, want: "agent-connections"},
		{name: "harnesses list", args: []string{"harnesses", "list"}, want: "harnesses"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeClient{}
			restore := replaceClientFactory(fake)
			defer restore()

			err := run(context.Background(), tt.args, &bytes.Buffer{}, &bytes.Buffer{})

			require.NoError(t, err)
			assert.Equal(t, tt.want, fake.lastCall)
		})
	}
}

func TestRunLocalSessionsSyncPassesFilters(t *testing.T) {
	fake := &fakeClient{}
	restore := replaceClientFactory(fake)
	defer restore()

	err := run(context.Background(), []string{"local-sessions", "sync", "--agent", "codex", "--limit", "2", "--timeout-ms", "500"}, &bytes.Buffer{}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, control.SyncLocalSessionsQuery{Agent: "codex", Limit: 2, TimeoutMillis: 500}, fake.syncSessionsQuery)
}

func TestRunAgentConnectionCommandsUseLocalAPI(t *testing.T) {
	fake := &fakeClient{}
	restore := replaceClientFactory(fake)
	defer restore()

	err := run(context.Background(), []string{"agent-connections", "restart", "conn_1"}, &bytes.Buffer{}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "conn_1", fake.restartID)

	err = run(context.Background(), []string{
		"agent-connections", "create",
		"--id", "conn_2",
		"--name", "Codex",
		"--harness", "codex",
		"--command", "codex --acp",
	}, &bytes.Buffer{}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "conn_2", fake.createCommand.ID)
	assert.Equal(t, []string{"codex", "--acp"}, fake.createCommand.Command)
}

func TestRunRemoteCommandsUseLocalAPI(t *testing.T) {
	fake := &fakeClient{}
	restore := replaceClientFactory(fake)
	defer restore()

	err := run(context.Background(), []string{
		"remotes", "create",
		"--id", "remote_prod",
		"--name", "Prod",
		"--api-url", "https://api.example.test",
		"--node-id", "node_1",
		"--api-key-ref", "env:PAX_NODE_KEY",
		"--default",
	}, &bytes.Buffer{}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "remote_prod", fake.createRemote.Remote.ID)
	assert.Equal(t, "Prod", fake.createRemote.Remote.Name)
	assert.Equal(t, "https://api.example.test", fake.createRemote.Remote.CloudAPIURL)
	assert.Equal(t, "node_1", fake.createRemote.Remote.NodeID)
	assert.Equal(t, "env:PAX_NODE_KEY", fake.createRemote.CloudAPIKeyRef)
	require.NotNil(t, fake.createRemote.Remote.IsDefault)
	assert.True(t, *fake.createRemote.Remote.IsDefault)

	err = run(context.Background(), []string{
		"remotes", "update", "remote_prod",
		"--name", "Prod Next",
		"--clear-api-key",
		"--enabled=false",
		"--set-enabled",
	}, &bytes.Buffer{}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "remote_prod", fake.updateRemoteID)
	require.NotNil(t, fake.updateRemote.Remote.Name)
	assert.Equal(t, "Prod Next", *fake.updateRemote.Remote.Name)
	require.NotNil(t, fake.updateRemote.Remote.Enabled)
	assert.False(t, *fake.updateRemote.Remote.Enabled)
	require.NotNil(t, fake.updateRemote.CloudAPIKeyRef)
	assert.Equal(t, "", *fake.updateRemote.CloudAPIKeyRef)

	err = run(context.Background(), []string{"remotes", "restart", "remote_prod"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "remote_prod", fake.restartRemoteID)

	err = run(context.Background(), []string{"remotes", "delete", "--cascade-agent-connections", "remote_prod"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "remote_prod", fake.deleteRemoteID)
	assert.True(t, fake.deleteRemoteCascade)
}

func TestRunRequiresCommand(t *testing.T) {
	err := run(context.Background(), []string{}, &bytes.Buffer{}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "command is required")
}

func replaceClientFactory(client *fakeClient) func() {
	original := newControlClient
	newControlClient = func(string, string) controlClient { return client }
	return func() { newControlClient = original }
}

type fakeClient struct {
	status              control.QueryResult
	statusCalls         int
	lastCall            string
	discoverQuery       control.DiscoverHarnessesQuery
	listSessionsQuery   control.ListLocalSessionsQuery
	syncSessionsQuery   control.SyncLocalSessionsQuery
	createRemote        control.CreateRemoteCommand
	updateRemote        control.UpdateRemoteCommand
	updateRemoteID      string
	restartRemoteID     string
	deleteRemoteID      string
	deleteRemoteCascade bool
	createCommand       control.CreateAgentConnectionCommand
	restartID           string
	deleteID            string
}

func (c *fakeClient) GetStatus(context.Context) (control.QueryResult, error) {
	c.statusCalls++
	return c.status, nil
}

func (c *fakeClient) ListRemotes(context.Context, bool) (control.QueryResult, error) {
	c.lastCall = "remotes"
	return control.QueryResult{Type: control.QueryRemotesList}, nil
}

func (c *fakeClient) CreateRemote(_ context.Context, commandID string, cmd control.CreateRemoteCommand) (control.CommandAck, error) {
	c.createRemote = cmd
	return control.CommandAck{CommandID: commandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (c *fakeClient) UpdateRemote(_ context.Context, commandID string, remoteID string, cmd control.UpdateRemoteCommand) (control.CommandAck, error) {
	c.updateRemoteID = remoteID
	c.updateRemote = cmd
	return control.CommandAck{CommandID: commandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (c *fakeClient) RestartRemote(_ context.Context, commandID string, remoteID string) (control.CommandAck, error) {
	c.restartRemoteID = remoteID
	return control.CommandAck{CommandID: commandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (c *fakeClient) DeleteRemote(_ context.Context, commandID string, remoteID string, cascadeAgentConnections bool) (control.CommandAck, error) {
	c.deleteRemoteID = remoteID
	c.deleteRemoteCascade = cascadeAgentConnections
	return control.CommandAck{CommandID: commandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (c *fakeClient) ListAgentConnections(context.Context, bool) (control.QueryResult, error) {
	c.lastCall = "agent-connections"
	return control.QueryResult{Type: control.QueryAgentConnectionsList}, nil
}

func (c *fakeClient) ListHarnesses(context.Context, bool) (control.QueryResult, error) {
	c.lastCall = "harnesses"
	return control.QueryResult{Type: control.QueryHarnessesList}, nil
}

func (c *fakeClient) DiscoverHarnesses(_ context.Context, query control.DiscoverHarnessesQuery) (control.QueryResult, error) {
	c.discoverQuery = query
	return control.QueryResult{Type: control.QueryHarnessesDiscover}, nil
}

func (c *fakeClient) ListLocalSessions(_ context.Context, query control.ListLocalSessionsQuery) (control.QueryResult, error) {
	c.listSessionsQuery = query
	return control.QueryResult{Type: control.QueryLocalSessionsList}, nil
}

func (c *fakeClient) SyncLocalSessions(_ context.Context, query control.SyncLocalSessionsQuery) (control.QueryResult, error) {
	c.syncSessionsQuery = query
	return control.QueryResult{Type: control.QueryLocalSessionsSync}, nil
}

func (c *fakeClient) CreateAgentConnection(_ context.Context, commandID string, cmd control.CreateAgentConnectionCommand) (control.CommandAck, error) {
	c.createCommand = cmd
	return control.CommandAck{CommandID: commandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (c *fakeClient) RestartAgentConnection(_ context.Context, commandID string, connectionID string) (control.CommandAck, error) {
	c.restartID = connectionID
	return control.CommandAck{CommandID: commandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (c *fakeClient) DeleteAgentConnection(_ context.Context, commandID string, connectionID string) (control.CommandAck, error) {
	c.deleteID = connectionID
	return control.CommandAck{CommandID: commandID, OK: true, Status: control.CommandStatusReceived}, nil
}
