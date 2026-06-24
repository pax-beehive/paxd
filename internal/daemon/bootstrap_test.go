package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapImportsYAMLConfigIntoDaemonStoreAndLocalAPI(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cfg := testConfig(t)

	rt, err := Bootstrap(ctx, Options{Config: cfg, Store: store, ImportYAML: true})

	require.NoError(t, err)
	require.NotNil(t, rt.LocalHandler)
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, remotes, 1)
	assert.Equal(t, "default", remotes[0].Remote.ID)
	assert.Equal(t, "https://api.example.test", remotes[0].Remote.CloudAPIURL)
	assert.Equal(t, "node_1", remotes[0].Remote.NodeID)
	assert.Equal(t, int64(1), remotes[0].Generation)

	conns, err := store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, conns, 2)
	assert.Equal(t, "codex-main", conns[0].ID)
	assert.Equal(t, "agent_codex", conns[0].CloudAgentID)
	assert.Equal(t, []string{"codex", "--acp"}, conns[0].Command)
	assert.Equal(t, "review", conns[1].ID)
	assert.Equal(t, "claude-code", conns[1].Harness)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/agent-connections?include_disabled=true", nil)
	rt.LocalHandler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var result control.QueryResult
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&result))
	require.NotNil(t, result.AgentConnections)
	assert.Len(t, result.AgentConnections.Items, 2)
}

func TestImportConfigIsIdempotentAndUpdatesChangedAgents(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cfg := testConfig(t)

	require.NoError(t, ImportConfig(ctx, store, cfg))
	material, err := store.GetRemoteAuthMaterial(ctx, "default")
	require.NoError(t, err)
	assert.Equal(t, "inline:node-key", material.CloudAPIKeyRef)
	assert.Equal(t, control.RemoteAuthCloudflareAccess, material.AuthKind)
	require.NotNil(t, material.CloudflareAccess)
	assert.Equal(t, "cf-client", material.CloudflareAccess.ClientID)
	assert.Equal(t, "inline:cf-secret", material.CloudflareAccess.ClientSecretRef)

	require.NoError(t, ImportConfig(ctx, store, cfg))
	conns, err := store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, conns, 2)
	assert.Equal(t, int64(1), conns[0].Generation)

	cfg.Agents[0].ACPForwarder.WorkingDir = "/workspace/next"
	require.NoError(t, ImportConfig(ctx, store, cfg))
	conns, err = store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, conns, 2)
	assert.Equal(t, int64(2), conns[0].Generation)
	assert.Equal(t, "/workspace/next", conns[0].WorkingDir)

	cfg.Cloud.APIKey = ""
	cfg.Cloud.CFClientID = ""
	cfg.Cloud.CFClientSecret = ""
	require.NoError(t, ImportConfig(ctx, store, cfg))
	material, err = store.GetRemoteAuthMaterial(ctx, "default")
	require.NoError(t, err)
	assert.Equal(t, "", material.CloudAPIKeyRef)
	assert.Equal(t, control.RemoteAuthNone, material.AuthKind)
	assert.Nil(t, material.CloudflareAccess)
}

func TestImportConfigRejectsPartialCloudflareAccessConfig(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cfg := testConfig(t)
	cfg.Cloud.CFClientSecret = ""

	err := ImportConfig(ctx, store, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires both")
}

func TestStartDebugHTTPRejectsNonLoopbackAddress(t *testing.T) {
	server, err := StartDebugHTTP(context.Background(), "0.0.0.0:0", http.NewServeMux())

	require.Error(t, err)
	assert.Nil(t, server)
	assert.Contains(t, err.Error(), "loopback")
}

func openTestStore(t *testing.T) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(filepath.Join(t.TempDir(), "paxd.db"))
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	return store
}

func TestBootstrapRequiresConfig(t *testing.T) {
	rt, err := Bootstrap(context.Background(), Options{})

	require.Error(t, err)
	assert.Nil(t, rt)
	assert.Contains(t, err.Error(), "config")
}

func TestLocalAPIListenerValidationDoesNotBindWhenDisabledOrInvalid(t *testing.T) {
	server, err := StartDebugHTTP(context.Background(), "", http.NewServeMux())
	require.NoError(t, err)
	assert.Nil(t, server)

	server, err = StartDebugHTTP(context.Background(), "bad-address", http.NewServeMux())
	require.Error(t, err)
	assert.Nil(t, server)

	server, err = StartUnixLocalAPI(context.Background(), "", nil)
	require.Error(t, err)
	assert.Nil(t, server)
	assert.Contains(t, err.Error(), "handler")

	assert.Nil(t, (*LocalAPIServer)(nil).Addr())
	assert.NoError(t, (*LocalAPIServer)(nil).Close())
}

func TestConfigImportHelpersCoverHarnessAndSecretVariants(t *testing.T) {
	assert.Equal(t, []string{"hermes", "acp"}, acpCommandForHarness("hermes"))
	assert.Equal(t, []string{"codex", "--acp"}, acpCommandForHarness("codex"))
	assert.Equal(t, []string{"claude", "--acp"}, acpCommandForHarness("claude_code"))
	assert.Equal(t, []string{"gemini", "--acp"}, acpCommandForHarness("gemini"))
	assert.Nil(t, acpCommandForHarness("custom"))

	assert.Equal(t, "", secretRef(""))
	assert.Equal(t, "env:PAX_KEY", secretRef("env:PAX_KEY"))
	assert.Equal(t, "inline:literal-key", secretRef("literal-key"))
	assert.False(t, stringSlicesEqual([]string{"a"}, []string{"b"}))
	assert.False(t, stringMapsEqual(map[string]string{"a": "1"}, map[string]string{"a": "2"}))
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	enabled := true
	return &config.Config{
		Agent: config.AgentConfig{
			Name:     "dev-node",
			Hostname: "host-1",
		},
		Cloud: config.CloudConfig{
			APIURL:         "https://api.example.test",
			NodeID:         "node_1",
			APIKey:         "node-key",
			CFClientID:     "cf-client",
			CFClientSecret: "cf-secret",
		},
		Daemon: config.DaemonConfig{
			DBPath: filepath.Join(t.TempDir(), "daemon.db"),
		},
		ACPForwarder: config.ACPForwarderConfig{
			TunnelPath: "/api/v1/agent/tunnel",
		},
		Agents: []config.RuntimeAgentConfig{
			{
				AgentID:    "agent_codex",
				InstanceID: "codex-main",
				Name:       "Codex Main",
				AgentType:  "codex",
				Enabled:    &enabled,
				ACPForwarder: config.AgentACPForwarderConfig{
					Harness: "codex",
					Command: []string{"codex", "--acp"},
				},
			},
			{
				AgentID:    "agent_review",
				InstanceID: "review",
				Name:       "Review",
				AgentType:  "claude-code",
				Enabled:    &enabled,
			},
		},
	}
}
