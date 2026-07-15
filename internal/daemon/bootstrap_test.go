package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/pax-beehive/paxkit/reliablemq"
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
	assert.IsType(t, &reliablemq.ProducerWriteBehindStore{}, rt.supervisors.transportFlusher)
	assertNoBusinessRows(t, store)
}

func TestBootstrapClearsStaleACPSessionBindingsAndPreservesResumeDescriptor(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	route, err := store.UpsertACPSessionRoute(ctx, daemonstore.ACPSessionRouteUpsert{
		ConnectionID:     "conn_codex",
		NativeSessionID:  "session_1",
		ResumeParamsJSON: `{"cwd":"/work","mcpServers":[]}`,
	})
	require.NoError(t, err)
	_, bound, err := store.BindACPSessionRoute(ctx, daemonstore.ACPSessionRouteBindingUpdate{
		ConnectionID:    "conn_codex",
		NativeSessionID: "session_1",
		SlotID:          "slot_a",
		ProcessEpoch:    "epoch_stale",
		ExpectedVersion: route.Version,
	})
	require.NoError(t, err)
	require.True(t, bound)

	cfg := config.DefaultConfig()
	runtime, err := Bootstrap(ctx, Options{Config: &cfg, Store: store})

	require.NoError(t, err)
	require.NotNil(t, runtime)
	route, err = store.GetACPSessionRoute(ctx, "conn_codex", "session_1")
	require.NoError(t, err)
	assert.Empty(t, route.BoundSlotID)
	assert.Empty(t, route.BoundProcessEpoch)
	assert.Equal(t, "slot_a", route.LastSlotID)
	assert.JSONEq(t, `{"cwd":"/work","mcpServers":[]}`, route.ResumeParamsJSON)
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

func TestDefaultControlSocketPathExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Equal(t, filepath.Join(home, ".paxd", "paxd.sock"), DefaultControlSocketPath())
}

func TestStartUnixLocalAPIServesAndCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := filepath.Join("/private/tmp", fmt.Sprintf("paxd-test-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server, err := StartUnixLocalAPI(ctx, socketPath, http.NewServeMux())
	if err != nil && errors.Is(err, os.ErrPermission) {
		t.Skipf("unix socket bind not permitted in this sandbox: %v", err)
	}
	require.NoError(t, err)
	require.NotNil(t, server)
	assert.NotNil(t, server.Addr())

	require.NoError(t, server.Close())
}

func TestStartDebugHTTPServesAndCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := StartDebugHTTP(ctx, "127.0.0.1:0", http.NewServeMux())
	if err != nil && errors.Is(err, os.ErrPermission) {
		t.Skipf("tcp bind not permitted in this sandbox: %v", err)
	}
	require.NoError(t, err)
	require.NotNil(t, server)
	assert.NotNil(t, server.Addr())

	require.NoError(t, server.Close())
}

func TestRuntimeSupervisorValidationAndNilBranches(t *testing.T) {
	var nilRuntime *Runtime
	nilRuntime.StartSupervisors(context.Background())
	(&Runtime{}).StartSupervisors(context.Background())

	empty := &runtimeSupervisors{}
	empty.WakeRemotes()
	empty.WakeAgentConnections()
	empty.Start(context.Background())

	if err := empty.Configure(nil, nil); err == nil {
		t.Fatal("Configure(nil store) error = nil")
	}
	if err := empty.Configure(openTestStore(t), nil); err == nil {
		t.Fatal("Configure(nil service) error = nil")
	}

	assert.Equal(t, "", firstNonEmpty("", ""))
	assert.Equal(t, "/tmp/paxd", expandHome("/tmp/paxd"))
}

func TestRuntimeSupervisorsHelpers(t *testing.T) {
	metrics := &fakeMetricsStarter{}
	harnesses := &fakeDaemonHarnessRegistry{}
	remote := &fakeDaemonSupervisor{}
	agent := &fakeDaemonSupervisor{}
	rt := &Runtime{
		hostMetrics: metrics,
		harnesses:   harnesses,
		supervisors: &runtimeSupervisors{
			remote: remote,
			agent:  agent,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.StartSupervisors(ctx)

	require.Eventually(t, func() bool {
		return metrics.started && harnesses.discovered && remote.started && agent.started
	}, time.Second, 10*time.Millisecond)
	rt.supervisors.WakeRemotes()
	rt.supervisors.WakeAgentConnections()
	assert.Equal(t, 1, remote.wakes)
	assert.Equal(t, 1, agent.wakes)
	cancel()
}

func TestRuntimeSupervisorsStartIgnoresCanceledAndDeadlineErrors(t *testing.T) {
	remote := &fakeDaemonSupervisor{startErr: context.Canceled}
	agent := &fakeDaemonSupervisor{startErr: context.DeadlineExceeded}
	sups := &runtimeSupervisors{
		remote: remote,
		agent:  agent,
	}
	sups.Start(context.Background())
	require.Eventually(t, func() bool {
		return remote.startedCount() == 1 && agent.startedCount() == 1
	}, time.Second, 10*time.Millisecond)
}

func TestRuntimeSessionFactoriesReturnSessions(t *testing.T) {
	remoteFactory := remoteControlSessionFactory{
		headers: fakeHeaderProvider{},
		dialer:  fakeWebSocketDialer{},
		runner: runtimes.NodeControlRunnerFunc(func(context.Context, runtimes.WebSocketConn, runtimes.RemoteSpec) runtimes.Exit {
			return runtimes.CanceledExit(context.Canceled)
		}),
	}
	remoteSession := remoteFactory.NewRemoteControlSession(runtimes.RemoteSpec{
		RemoteID:        "remote_prod",
		CloudAPIURL:     "https://api.example.test",
		NodeControlPath: "/api/v2/node/control",
	})
	require.NotNil(t, remoteSession)

	agentFactory := agentTunnelSessionFactory{}
	agentSession := agentFactory.NewAgentTunnelSession(runtimes.AgentConnectionSpec{
		ConnectionID: "conn_codex",
		RemoteID:     "remote_prod",
		CloudAgentID: "agent_123",
		InstanceID:   "inst_1",
		CloudAPIURL:  "https://api.example.test",
		Command:      []string{"codex"},
	})
	require.NotNil(t, agentSession)
}

func openTestStore(t *testing.T) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(filepath.Join(t.TempDir(), "paxd.db"))
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	return store
}

type fakeMetricsStarter struct {
	started bool
}

func (f *fakeMetricsStarter) Start(context.Context) {
	f.started = true
}

type fakeDaemonHarnessRegistry struct {
	discovered bool
}

func (f *fakeDaemonHarnessRegistry) ListCached(context.Context) ([]control.HarnessView, error) {
	return nil, nil
}

func (f *fakeDaemonHarnessRegistry) Discover(context.Context, control.DiscoverHarnessesQuery) ([]control.HarnessView, error) {
	f.discovered = true
	return nil, nil
}

type fakeDaemonSupervisor struct {
	wakes    int
	started  bool
	starts   int
	startErr error
}

func (f *fakeDaemonSupervisor) Start(context.Context) error {
	f.started = true
	f.starts++
	return f.startErr
}

func (f *fakeDaemonSupervisor) Wake() {
	f.wakes++
}

func (f *fakeDaemonSupervisor) Snapshot() supervisor.Snapshot {
	return supervisor.Snapshot{}
}

func (f *fakeDaemonSupervisor) startedCount() int {
	return f.starts
}

type fakeHeaderProvider struct{}

func (fakeHeaderProvider) Headers(context.Context, string) (http.Header, error) {
	return http.Header{}, nil
}

type fakeWebSocketDialer struct{}

func (fakeWebSocketDialer) Dial(context.Context, string, http.Header) (runtimes.WebSocketConn, *http.Response, error) {
	return nil, nil, errors.New("not dialed in factory test")
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
