package daemon

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapDoesNotWriteBusinessRowsFromConfig(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cfg := config.DefaultConfig()
	cfg.Cloud.APIURL = "https://app.paxtech.net"
	cfg.Cloud.APIKey = "pax_key"
	cfg.Agents = []config.RuntimeAgentConfig{{
		AgentID:    "agent_codex",
		InstanceID: "codex-main",
		Name:       "Codex Main",
		AgentType:  "codex",
		ACPForwarder: config.AgentACPForwarderConfig{
			Harness: "codex",
			Command: []string{"codex-acp"},
		},
	}}

	rt, err := Bootstrap(ctx, Options{Config: &cfg, Store: store})

	require.NoError(t, err)
	require.NotNil(t, rt.LocalHandler)
	require.NotNil(t, rt.supervisors)
	assert.NotNil(t, rt.supervisors.remote)
	assert.NotNil(t, rt.supervisors.agent)
	assertNoBusinessRows(t, store)
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

	server, err = StartDebugHTTP(context.Background(), "0.0.0.0:0", http.NewServeMux())
	require.Error(t, err)
	assert.Nil(t, server)
	assert.Contains(t, err.Error(), "loopback")

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

func openTestStore(t *testing.T) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(filepath.Join(t.TempDir(), "paxd.db"))
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	return store
}

func assertNoBusinessRows(t *testing.T, store *daemonstore.Store) {
	t.Helper()
	models := []any{
		&daemonstore.Remote{},
		&daemonstore.RemoteAuth{},
		&daemonstore.RemoteStatus{},
		&daemonstore.AgentConnection{},
		&daemonstore.AgentConnectionStatus{},
		&daemonstore.ControlCommand{},
		&daemonstore.HarnessInventory{},
		&daemonstore.LocalSession{},
		&daemonstore.LocalSessionElement{},
		&daemonstore.Message{},
		&daemonstore.MessagePart{},
		&daemonstore.Setting{},
	}
	for _, model := range models {
		var count int64
		require.NoError(t, store.DB().Model(model).Count(&count).Error)
		assert.Zero(t, count, "%T has rows", model)
	}
}
