package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/harnessregistry"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/pax-beehive/paxd/internal/localsessions"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalAPIControlStoreAndSupervisorIntegration(t *testing.T) {
	store := openIntegrationStore(t)
	remoteFactory := &recordingRemoteFactory{}
	agentFactory := &recordingAgentFactory{}
	remoteSupervisor, agentSupervisor := startIntegrationSupervisors(t, store, remoteFactory, agentFactory)
	service := control.NewService(control.ServiceOptions{
		Store:       store,
		Supervisors: supervisorWaker{remotes: remoteSupervisor, agents: agentSupervisor},
	})
	handler := localapi.NewHandler(service)

	remoteCommand := createRemoteCommand("cmd_localapi_remote_create_int")
	var remoteAck control.CommandAck
	postJSON(t, handler, "/v1/remotes", remoteCommand.CommandID, remoteCommand.CreateRemote, http.StatusAccepted, &remoteAck)
	assert.True(t, remoteAck.OK)
	assert.Equal(t, "remote_prod", remoteAck.TargetID)
	requireStarted(t, requireRemoteSession(t, remoteFactory, 0))

	agentCommand := createAgentConnectionCommand("cmd_localapi_agent_create_int")
	var agentAck control.CommandAck
	postJSON(t, handler, "/v1/agent-connections", agentCommand.CommandID, agentCommand.CreateAgentConnection, http.StatusAccepted, &agentAck)
	assert.True(t, agentAck.OK)
	assert.Equal(t, "conn_codex", agentAck.TargetID)
	requireStarted(t, requireAgentSession(t, agentFactory, 0))
	requireAgentPhase(t, service, "conn_codex", "starting", 1)

	var result control.QueryResult
	getJSON(t, handler, "/v1/agent-connections?include_disabled=true", http.StatusOK, &result)
	require.NotNil(t, result.AgentConnections)
	require.Len(t, result.AgentConnections.Items, 1)
	assert.Equal(t, "conn_codex", result.AgentConnections.Items[0].ID)
	assert.NotNil(t, result.AgentConnections.Items[0].Status)
}

func TestLocalObservationWorksThroughLocalAPIWithoutRemotes(t *testing.T) {
	store := openIntegrationStore(t)
	harnesses := harnessregistry.New(store, staticDetector{
		name: "codex",
		view: control.HarnessView{
			Harness:     "codex",
			DisplayName: "Codex",
			State:       harnessregistry.StateAvailable,
			Capability:  harnessregistry.CapabilityACP,
			Command:     []string{"/usr/local/bin/codex", "--acp"},
		},
	})
	localSessions := localsessions.New(store, staticScannerRegistry{
		available: []string{"codex"},
		scanners: map[string]localsessions.Scanner{
			"codex": staticScanner{sessions: []control.LocalSessionView{
				{NativeID: "sess_1", Title: "Local debugging", Preview: "working on paxd"},
			}},
		},
	}, localsessions.WithClock(func() time.Time {
		return time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	}))
	service := control.NewService(control.ServiceOptions{
		Store:         store,
		Harnesses:     harnesses,
		LocalSessions: localSessions,
	})
	handler := localapi.NewHandler(service)

	var discover control.QueryResult
	postJSON(t, handler, "/v1/harnesses/discover", "", control.DiscoverHarnessesQuery{Probe: true}, http.StatusOK, &discover)
	require.NotNil(t, discover.Harnesses)
	require.Len(t, discover.Harnesses.Items, 1)
	assert.Equal(t, "codex", discover.Harnesses.Items[0].Harness)

	var syncResult control.QueryResult
	postJSON(t, handler, "/v1/local/sessions/sync", "", control.SyncLocalSessionsQuery{Agent: "codex"}, http.StatusOK, &syncResult)
	require.NotNil(t, syncResult.LocalSessionSync)
	assert.Equal(t, 1, syncResult.LocalSessionSync.Synced)

	var overview control.QueryResult
	getJSON(t, handler, "/v1/local/overview", http.StatusOK, &overview)
	require.NotNil(t, overview.LocalOverview)
	assert.Equal(t, []control.HarnessView{{
		Harness:     "codex",
		DisplayName: "Codex",
		State:       harnessregistry.StateAvailable,
		Capability:  harnessregistry.CapabilityACP,
		Command:     []string{"/usr/local/bin/codex", "--acp"},
	}}, overview.LocalOverview.Harnesses)
	require.Len(t, overview.LocalOverview.Sessions, 1)
	assert.Equal(t, "codex:sess_1", overview.LocalOverview.Sessions[0].ID)

	connections, err := store.ListAgentConnections(context.Background(), control.ListAgentConnectionsQuery{IncludeDisabled: true})
	require.NoError(t, err)
	assert.Empty(t, connections)
}

func TestControlCommandsPersistDesiredStateAndWakeSupervisors(t *testing.T) {
	ctx := context.Background()
	store := openIntegrationStore(t)
	remoteFactory := &recordingRemoteFactory{}
	agentFactory := &recordingAgentFactory{}
	remoteSupervisor, agentSupervisor := startIntegrationSupervisors(t, store, remoteFactory, agentFactory)
	service := control.NewService(control.ServiceOptions{
		Store:       store,
		Supervisors: supervisorWaker{remotes: remoteSupervisor, agents: agentSupervisor},
	})

	remoteAck, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, createRemoteCommand("cmd_remote_create_int"))
	require.NoError(t, err)
	assert.Equal(t, control.CommandAck{
		CommandID:         "cmd_remote_create_int",
		OK:                true,
		Status:            control.CommandStatusReceived,
		TargetType:        "remote",
		TargetID:          "remote_prod",
		DesiredGeneration: 1,
		Result:            remoteAck.Result,
	}, remoteAck)

	remoteSession := requireRemoteSession(t, remoteFactory, 0)
	requireStarted(t, remoteSession)
	assert.Equal(t, []runtimes.RemoteSpec{{
		RemoteID:     "remote_prod",
		Name:         "Production",
		CloudAPIURL:  "https://api.example.test",
		NodeID:       "node_1",
		Generation:   1,
		RestartNonce: 0,
	}}, remoteFactory.specs())
	requireRemotePhase(t, service, "remote_prod", "connecting", 1)

	agentAck, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, createAgentConnectionCommand("cmd_agent_create_int"))
	require.NoError(t, err)
	assert.True(t, agentAck.OK)
	assert.Equal(t, "conn_codex", agentAck.TargetID)
	assert.Equal(t, int64(1), agentAck.DesiredGeneration)

	agentSession := requireAgentSession(t, agentFactory, 0)
	requireStarted(t, agentSession)
	assert.Equal(t, []runtimes.AgentConnectionSpec{{
		ConnectionID: "conn_codex",
		RemoteID:     "remote_prod",
		CloudAPIURL:  "https://api.example.test",
		InstanceID:   "inst_1",
		AgentType:    "codex",
		Harness:      "codex",
		Command:      []string{"codex", "--acp"},
		WorkingDir:   "/workspace/project",
		TunnelPath:   "/api/v1/agent/tunnel",
		Env:          map[string]string{"PAX_PROFILE": "prod"},
		Generation:   1,
		RestartNonce: 0,
	}}, agentFactory.specs())
	requireAgentPhase(t, service, "conn_codex", "starting", 1)

	replayedAck, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, createAgentConnectionCommand("cmd_agent_create_int"))
	require.NoError(t, err)
	assert.Equal(t, agentAck.CommandID, replayedAck.CommandID)
	assert.Equal(t, agentAck.Status, replayedAck.Status)
	assert.Len(t, agentFactory.specs(), 1)
}

func TestControlUpdatesInterruptRunningAgentAndPersistStoppedState(t *testing.T) {
	ctx := context.Background()
	store := openIntegrationStore(t)
	remoteFactory := &recordingRemoteFactory{}
	agentFactory := &recordingAgentFactory{}
	remoteSupervisor, agentSupervisor := startIntegrationSupervisors(t, store, remoteFactory, agentFactory)
	service := control.NewService(control.ServiceOptions{
		Store:       store,
		Supervisors: supervisorWaker{remotes: remoteSupervisor, agents: agentSupervisor},
	})

	_, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, createRemoteCommand("cmd_remote_create_for_update"))
	require.NoError(t, err)
	_, err = service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, createAgentConnectionCommand("cmd_agent_create_for_update"))
	require.NoError(t, err)
	firstSession := requireAgentSession(t, agentFactory, 0)
	requireStarted(t, firstSession)

	workingDir := "/workspace/next"
	updateAck, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, control.Command{
		CommandID: "cmd_agent_update_int",
		Type:      control.CommandAgentConnectionUpdate,
		UpdateAgentConnection: &control.UpdateAgentConnectionCommand{
			ConnectionID: "conn_codex",
			WorkingDir:   &workingDir,
		},
	})
	require.NoError(t, err)
	assert.True(t, updateAck.OK)
	assert.Equal(t, int64(2), updateAck.DesiredGeneration)
	requireCanceled(t, firstSession)

	secondSession := requireAgentSession(t, agentFactory, 1)
	requireStarted(t, secondSession)
	secondSpec := agentFactory.specs()[1]
	assert.Equal(t, int64(2), secondSpec.Generation)
	assert.Equal(t, "/workspace/next", secondSpec.WorkingDir)
	requireAgentPhase(t, service, "conn_codex", "starting", 2)

	deleteAck, err := service.HandleCommand(ctx, control.Source{Kind: control.SourceLocal}, control.Command{
		CommandID:             "cmd_agent_delete_int",
		Type:                  control.CommandAgentConnectionDelete,
		DeleteAgentConnection: &control.DeleteAgentConnectionCommand{ConnectionID: "conn_codex"},
	})
	require.NoError(t, err)
	assert.True(t, deleteAck.OK)
	assert.Equal(t, int64(3), deleteAck.DesiredGeneration)
	requireCanceled(t, secondSession)
	requireAgentPhase(t, service, "conn_codex", "stopped", 2)

	queryResult, err := service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, control.Query{
		Type:                 control.QueryAgentConnectionsList,
		ListAgentConnections: &control.ListAgentConnectionsQuery{},
	})
	require.NoError(t, err)
	require.NotNil(t, queryResult.AgentConnections)
	assert.Empty(t, queryResult.AgentConnections.Items)

	queryResult, err = service.HandleQuery(ctx, control.Source{Kind: control.SourceLocal}, control.Query{
		Type:                 control.QueryAgentConnectionsList,
		ListAgentConnections: &control.ListAgentConnectionsQuery{IncludeDisabled: true},
	})
	require.NoError(t, err)
	require.NotNil(t, queryResult.AgentConnections)
	require.Len(t, queryResult.AgentConnections.Items, 1)
	assert.False(t, queryResult.AgentConnections.Items[0].Enabled)
	assert.Equal(t, control.DesiredStateDeleted, queryResult.AgentConnections.Items[0].DesiredState)
}

func openIntegrationStore(t *testing.T) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(t.TempDir() + "/paxd-integration.db")
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	db, err := store.DB().DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	return store
}

func startIntegrationSupervisors(
	t *testing.T,
	store *daemonstore.Store,
	remoteFactory *recordingRemoteFactory,
	agentFactory *recordingAgentFactory,
) (*supervisor.RemoteSupervisor, *supervisor.AgentConnectionSupervisor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	remoteErr := make(chan error, 1)
	agentErr := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		var remoteDone bool
		var agentDone bool
		var remoteExit error
		var agentExit error
		require.Eventually(t, func() bool {
			select {
			case remoteExit = <-remoteErr:
				remoteDone = true
			default:
			}
			select {
			case agentExit = <-agentErr:
				agentDone = true
			default:
			}
			return remoteDone && agentDone
		}, 2*time.Second, 10*time.Millisecond)
		assert.ErrorIs(t, remoteExit, context.Canceled)
		assert.ErrorIs(t, agentExit, context.Canceled)
	})

	remoteSupervisor := supervisor.NewRemoteSupervisor(supervisor.RemoteSupervisorOptions{
		Store:             store,
		Factory:           remoteFactory,
		ReconcileInterval: time.Hour,
	})
	agentSupervisor := supervisor.NewAgentConnectionSupervisor(supervisor.AgentConnectionSupervisorOptions{
		Store:             store,
		Factory:           agentFactory,
		ReconcileInterval: time.Hour,
	})
	go func() {
		remoteErr <- remoteSupervisor.Start(ctx)
	}()
	go func() {
		agentErr <- agentSupervisor.Start(ctx)
	}()
	return remoteSupervisor, agentSupervisor
}

func createRemoteCommand(commandID string) control.Command {
	enabled := true
	isDefault := true
	return control.Command{
		CommandID: commandID,
		Type:      control.CommandRemoteCreate,
		CreateRemote: &control.CreateRemoteCommand{
			Remote: control.Remote{
				ID:          "remote_prod",
				Name:        "Production",
				CloudAPIURL: "https://api.example.test",
				NodeID:      "node_1",
				Enabled:     &enabled,
				IsDefault:   &isDefault,
			},
			CloudAPIKeyRef: "env:PAX_NODE_KEY",
		},
	}
}

func createAgentConnectionCommand(commandID string) control.Command {
	enabled := true
	return control.Command{
		CommandID: commandID,
		Type:      control.CommandAgentConnectionCreate,
		CreateAgentConnection: &control.CreateAgentConnectionCommand{
			ID:           "conn_codex",
			RemoteID:     "remote_prod",
			Name:         "codex-main",
			InstanceID:   "inst_1",
			AgentType:    "codex",
			Harness:      "codex",
			Command:      []string{"codex", "--acp"},
			WorkingDir:   "/workspace/project",
			TunnelPath:   "/api/v1/agent/tunnel",
			Env:          map[string]string{"PAX_PROFILE": "prod"},
			Enabled:      &enabled,
			DesiredState: control.DesiredStateRunning,
		},
	}
}

func requireRemotePhase(t *testing.T, service control.Service, remoteID string, phase string, generation int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		result, err := service.HandleQuery(context.Background(), control.Source{Kind: control.SourceLocal}, control.Query{
			Type:        control.QueryRemotesList,
			ListRemotes: &control.ListRemotesQuery{IncludeDisabled: true},
		})
		return err == nil &&
			result.Remotes != nil &&
			len(result.Remotes.Items) == 1 &&
			result.Remotes.Items[0].Remote.ID == remoteID &&
			result.Remotes.Items[0].Status != nil &&
			result.Remotes.Items[0].Status.Phase == phase &&
			result.Remotes.Items[0].Status.ObservedGeneration == generation
	}, 2*time.Second, 10*time.Millisecond)
}

func requireAgentPhase(t *testing.T, service control.Service, connectionID string, phase string, generation int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		result, err := service.HandleQuery(context.Background(), control.Source{Kind: control.SourceLocal}, control.Query{
			Type:                 control.QueryAgentConnectionsList,
			ListAgentConnections: &control.ListAgentConnectionsQuery{IncludeDisabled: true},
		})
		return err == nil &&
			result.AgentConnections != nil &&
			len(result.AgentConnections.Items) == 1 &&
			result.AgentConnections.Items[0].ID == connectionID &&
			result.AgentConnections.Items[0].Status != nil &&
			result.AgentConnections.Items[0].Status.Phase == phase &&
			result.AgentConnections.Items[0].Status.ObservedGeneration >= generation
	}, 2*time.Second, 10*time.Millisecond)
}

func requireRemoteSession(t *testing.T, factory *recordingRemoteFactory, index int) *blockingSession {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(factory.sessions()) > index
	}, 2*time.Second, 10*time.Millisecond)
	return factory.sessions()[index]
}

func requireAgentSession(t *testing.T, factory *recordingAgentFactory, index int) *blockingSession {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(factory.sessions()) > index
	}, 2*time.Second, 10*time.Millisecond)
	return factory.sessions()[index]
}

func requireStarted(t *testing.T, session *blockingSession) {
	t.Helper()
	require.Eventually(t, session.started, 2*time.Second, 10*time.Millisecond)
}

func requireCanceled(t *testing.T, session *blockingSession) {
	t.Helper()
	require.Eventually(t, session.canceled, 2*time.Second, 10*time.Millisecond)
}

func postJSON(t *testing.T, handler http.Handler, path string, commandID string, body any, wantStatus int, dest any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("X-Pax-Command-Id", commandID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
	require.NoError(t, json.NewDecoder(rec.Body).Decode(dest))
}

func getJSON(t *testing.T, handler http.Handler, path string, wantStatus int, dest any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, wantStatus, rec.Code, rec.Body.String())
	require.NoError(t, json.NewDecoder(rec.Body).Decode(dest))
}

type supervisorWaker struct {
	remotes *supervisor.RemoteSupervisor
	agents  *supervisor.AgentConnectionSupervisor
}

func (w supervisorWaker) WakeRemotes() {
	w.remotes.Wake()
}

func (w supervisorWaker) WakeAgentConnections() {
	w.agents.Wake()
}

type staticDetector struct {
	name string
	view control.HarnessView
}

func (d staticDetector) Name() string {
	return d.name
}

func (d staticDetector) Detect(context.Context, control.DiscoverHarnessesQuery) (control.HarnessView, error) {
	return d.view, nil
}

type staticScannerRegistry struct {
	available []string
	scanners  map[string]localsessions.Scanner
}

func (r staticScannerRegistry) ScannerFor(agent string) (localsessions.Scanner, bool) {
	scanner, ok := r.scanners[agent]
	return scanner, ok
}

func (r staticScannerRegistry) AvailableAgents(context.Context) ([]string, error) {
	return append([]string(nil), r.available...), nil
}

type staticScanner struct {
	sessions []control.LocalSessionView
}

func (s staticScanner) ListSessions(context.Context, string, int) ([]control.LocalSessionView, error) {
	return append([]control.LocalSessionView(nil), s.sessions...), nil
}

type recordingRemoteFactory struct {
	mu       sync.Mutex
	specsLog []runtimes.RemoteSpec
	runs     []*blockingSession
}

func (f *recordingRemoteFactory) NewRemoteControlSession(spec runtimes.RemoteSpec) runtimes.Session {
	session := newBlockingSession()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specsLog = append(f.specsLog, spec)
	f.runs = append(f.runs, session)
	return session
}

func (f *recordingRemoteFactory) specs() []runtimes.RemoteSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtimes.RemoteSpec(nil), f.specsLog...)
}

func (f *recordingRemoteFactory) sessions() []*blockingSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*blockingSession(nil), f.runs...)
}

type recordingAgentFactory struct {
	mu       sync.Mutex
	specsLog []runtimes.AgentConnectionSpec
	runs     []*blockingSession
}

func (f *recordingAgentFactory) NewAgentTunnelSession(spec runtimes.AgentConnectionSpec) runtimes.Session {
	session := newBlockingSession()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specsLog = append(f.specsLog, spec)
	f.runs = append(f.runs, session)
	return session
}

func (f *recordingAgentFactory) specs() []runtimes.AgentConnectionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtimes.AgentConnectionSpec(nil), f.specsLog...)
}

func (f *recordingAgentFactory) sessions() []*blockingSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*blockingSession(nil), f.runs...)
}

type blockingSession struct {
	startOnce  sync.Once
	cancelOnce sync.Once
	startedCh  chan struct{}
	canceledCh chan struct{}
}

func newBlockingSession() *blockingSession {
	return &blockingSession{
		startedCh:  make(chan struct{}),
		canceledCh: make(chan struct{}),
	}
}

func (s *blockingSession) Run(ctx context.Context) runtimes.Exit {
	s.startOnce.Do(func() { close(s.startedCh) })
	<-ctx.Done()
	s.cancelOnce.Do(func() { close(s.canceledCh) })
	return runtimes.CanceledExit(ctx.Err())
}

func (s *blockingSession) started() bool {
	select {
	case <-s.startedCh:
		return true
	default:
		return false
	}
}

func (s *blockingSession) canceled() bool {
	select {
	case <-s.canceledCh:
		return true
	default:
		return false
	}
}
