package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapDoesNotWriteBusinessRowsFromConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	store := openTestStore(t)
	transportDBPath := filepath.Join(home, ".paxd", "transport.db")
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
	require.FileExists(t, transportDBPath)
	assert.Equal(t, 4, rt.supervisors.transportDB.Stats().MaxOpenConnections)
	var journalMode string
	require.NoError(t, rt.supervisors.transportDB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	assert.False(t, store.DB().Migrator().HasTable("transport_journal"))
	var transportJournalTables int
	require.NoError(t, rt.supervisors.transportDB.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'transport_journal'`,
	).Scan(&transportJournalTables))
	assert.Equal(t, 1, transportJournalTables)
	assertNoBusinessRows(t, store)
}

func TestBootstrapClearsStaleACPSessionBindingsAndPreservesResumeDescriptor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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

func TestBootstrapWiresRemotePaxdRestartLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := openTestStore(t)
	cfg := config.DefaultConfig()
	runtime, err := Bootstrap(context.Background(), Options{
		Config: &cfg, Store: store, PaxdVersion: "1.2.3",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.supervisors.closeTransport() })
	require.NotEmpty(t, runtime.maintenance.BootID())
	assert.Equal(t, runtime.maintenance.BootID(), runtime.supervisors.bootID)

	cmd := control.Command{
		CommandID: "cmd_bootstrap_restart",
		Type:      control.CommandRestartPaxd,
		RestartPaxd: &control.RestartPaxdCommand{
			Mode: control.PaxdRestartImmediate, ShutdownGraceSeconds: 9,
		},
	}
	ack, err := runtime.Control.HandleCommand(context.Background(), control.Source{
		Kind: control.SourceRemote, RemoteID: "remote_prod",
	}, cmd)
	require.NoError(t, err)
	assert.Equal(t, control.CommandStatusReceived, ack.Status)
	select {
	case request := <-runtime.ExitRequests():
		t.Fatalf("restart emitted before ACK delivery: %+v", request)
	default:
	}

	deferred, ok := runtime.Control.(control.DeferredCommandAction)
	require.True(t, ok)
	deferred.ConfirmCommandAckDelivered(cmd.CommandID)
	select {
	case request := <-runtime.ExitRequests():
		assert.Equal(t, cmd.CommandID, request.CommandID)
		assert.Equal(t, 9*time.Second, request.ShutdownGrace)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bootstrap lifecycle exit request")
	}
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
	require.NoError(t, nilRuntime.Shutdown(context.Background()))
	emptyRuntime := &Runtime{}
	emptyRuntime.StartSupervisors(context.Background())
	require.NoError(t, emptyRuntime.Shutdown(context.Background()))

	empty := &runtimeSupervisors{}
	empty.WakeRemotes()
	empty.WakeAgentConnections()
	empty.WakeACPSlots()
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
	acpSlots := &fakeDaemonSupervisor{}
	rt := &Runtime{
		hostMetrics: metrics,
		harnesses:   harnesses,
		supervisors: &runtimeSupervisors{
			remote:   remote,
			agent:    agent,
			acpSlots: acpSlots,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.StartSupervisors(ctx)

	require.Eventually(t, func() bool {
		return metrics.isStarted() && harnesses.isDiscovered() && remote.isStarted() && agent.isStarted() && acpSlots.isStarted()
	}, time.Second, 10*time.Millisecond)
	rt.supervisors.WakeRemotes()
	rt.supervisors.WakeAgentConnections()
	rt.supervisors.WakeACPSlots()
	assert.Equal(t, 1, remote.wakeCount())
	assert.Equal(t, 1, agent.wakeCount())
	assert.Equal(t, 1, acpSlots.wakeCount())
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

func TestRuntimeShutdownWaitsForSupervisors(t *testing.T) {
	remote := newGatedDaemonSupervisor()
	rt := &Runtime{supervisors: &runtimeSupervisors{remote: remote}}
	rt.StartSupervisors(context.Background())
	select {
	case <-remote.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for supervisor to start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- rt.Shutdown(shutdownCtx) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown() returned before supervisor cleanup completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(remote.release)
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() did not return after supervisor cleanup completed")
	}
	require.NoError(t, rt.Shutdown(context.Background()))
}

func TestTransportJournalGCRunsBoundedRetentionAndStops(t *testing.T) {
	// Given
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	pruner := &fakeTransportJournalPruner{result: sqlstore.PruneResult{Deleted: 7}}
	sups := &runtimeSupervisors{
		transportPruner: pruner,
		transportGCConfig: transportGCConfig{
			Interval:   5 * time.Millisecond,
			KeepFor:    72 * time.Hour,
			KeepLatest: 100,
			BatchSize:  1000,
			Now:        func() time.Time { return now },
		},
	}
	ctx, cancel := context.WithCancel(context.Background())

	// When
	sups.startTransportGC(ctx)
	require.Eventually(t, func() bool { return pruner.callCount() > 0 }, time.Second, time.Millisecond)
	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-sups.transportGCDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)

	// Then
	options := pruner.lastOptions()
	require.Equal(t, now.Add(-72*time.Hour), options.OlderThan)
	require.Equal(t, int64(100), options.KeepLatestPerQueue)
	require.Equal(t, 1000, options.Limit)
	diagnostics := sups.RuntimeDiagnostics(context.Background())
	require.NotNil(t, diagnostics.TransportJournalGC)
	require.Equal(t, int64(7), diagnostics.TransportJournalGC.TotalDeleted)
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
	mu      sync.Mutex
	started bool
}

func (f *fakeMetricsStarter) Start(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
}

func (f *fakeMetricsStarter) isStarted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

type fakeDaemonHarnessRegistry struct {
	mu         sync.Mutex
	discovered bool
}

func (f *fakeDaemonHarnessRegistry) ListCached(context.Context) ([]control.HarnessView, error) {
	return nil, nil
}

func (f *fakeDaemonHarnessRegistry) Discover(context.Context, control.DiscoverHarnessesQuery) ([]control.HarnessView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discovered = true
	return nil, nil
}

func (f *fakeDaemonHarnessRegistry) isDiscovered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discovered
}

type fakeTransportJournalPruner struct {
	mu      sync.Mutex
	calls   int
	options sqlstore.AckedOutboundPruneOptions
	result  sqlstore.PruneResult
	err     error
}

func (f *fakeTransportJournalPruner) PruneAckedOutbound(
	_ context.Context,
	options sqlstore.AckedOutboundPruneOptions,
) (sqlstore.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.options = options
	return f.result, f.err
}

func (f *fakeTransportJournalPruner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTransportJournalPruner) lastOptions() sqlstore.AckedOutboundPruneOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.options
}

type gatedDaemonSupervisor struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedDaemonSupervisor() *gatedDaemonSupervisor {
	return &gatedDaemonSupervisor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (f *gatedDaemonSupervisor) Start(ctx context.Context) error {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	<-f.release
	return ctx.Err()
}

func (f *gatedDaemonSupervisor) Wake() {}

func (f *gatedDaemonSupervisor) Snapshot() supervisor.Snapshot {
	return supervisor.Snapshot{}
}

type fakeDaemonSupervisor struct {
	mu       sync.Mutex
	wakes    int
	started  bool
	starts   int
	startErr error
}

func (f *fakeDaemonSupervisor) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	f.starts++
	return f.startErr
}

func (f *fakeDaemonSupervisor) Wake() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakes++
}

func (f *fakeDaemonSupervisor) Snapshot() supervisor.Snapshot {
	return supervisor.Snapshot{}
}

func (f *fakeDaemonSupervisor) startedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeDaemonSupervisor) isStarted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

func (f *fakeDaemonSupervisor) wakeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakes
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
