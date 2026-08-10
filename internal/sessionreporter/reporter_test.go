package sessionreporter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/pax-beehive/paxd/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunOnceScansObservedRuntimeAndReportsSessions(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
		Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "conn_1",
			RemoteID:     "remote_1",
			CloudAPIURL:  "https://manager.example.com",
			CloudAgentID: "agent_1",
			InstanceID:   "inst_1",
			AgentType:    "codex",
			Harness:      "codex",
			Command:      []string{"codex", "--acp"},
			WorkingDir:   "/workspace/paxd",
			Env:          map[string]string{"PAX_ENV": "test"},
			Generation:   7,
			RestartNonce: 2,
		},
		Phase: string(runtimes.PhaseStarting),
	}}}
	scanner := &fakeScanner{sessions: []model.SessionInfo{{
		SessionID:      "codex:sess_1",
		NativeID:       "sess_1",
		AgentType:      "codex",
		Name:           "Debug paxd",
		ProjectID:      "/workspace/paxd",
		Preview:        "working",
		WorkspaceRoots: []string{"/workspace/paxd"},
		Source:         "cli",
		Status:         "running",
		CurrentTask:    "tests",
		UpdatedAt:      "2026-06-28T12:00:00Z",
		TokenUsage:     42,
		Messages: []model.SessionMessage{{
			SessionID:   "codex:sess_1",
			Seq:         1,
			Kind:        "message",
			Role:        "user",
			Text:        "fix the session ordering",
			StartedAt:   "2026-06-28T11:59:58Z",
			CompletedAt: "2026-06-28T11:59:59Z",
		}, {
			SessionID:   "codex:sess_1",
			Seq:         2,
			Kind:        "message",
			Role:        "assistant",
			Text:        "done",
			CompletedAt: "2026-06-28T12:00:01Z",
		}},
	}}}
	reporter := &fakeCloudReporter{}
	service := New(Options{
		RuntimeSource: source,
		Scanner:       scanner,
		Reporter:      reporter,
		RouteResolver: &fakeRouteResolver{routes: map[string]routeResolution{
			"conn_1/sess_1": {managerSessionID: "sess_manager_1", found: true},
		}},
		ScanTimeout:   time.Second,
		ReportTimeout: time.Second,
	})

	require.NoError(t, service.RunOnce(context.Background()))

	require.Len(t, scanner.calls, 1)
	assert.Equal(t, "conn_1", scanner.calls[0].ConnectionID)
	assert.Equal(t, []string{"codex", "--acp"}, scanner.calls[0].Command)
	assert.Equal(t, "/workspace/paxd", scanner.calls[0].WorkingDir)
	assert.Equal(t, map[string]string{"PAX_ENV": "test"}, scanner.calls[0].Env)
	assert.Equal(t, defaultBatchSize, scanner.calls[0].Limit)
	require.Len(t, reporter.calls, 1)
	assert.Equal(t, "remote_1", reporter.calls[0].target.RemoteID)
	assert.Equal(t, "https://manager.example.com", reporter.calls[0].target.CloudAPIURL)
	assert.Equal(t, "agent_1", reporter.calls[0].agentID)
	require.Len(t, reporter.calls[0].sessions, 1)
	assert.Equal(t, "sess_manager_1", reporter.calls[0].sessions[0].SessionID)
	assert.Equal(t, "sess_1", reporter.calls[0].sessions[0].NativeID)
	assert.Equal(t, "cli", reporter.calls[0].sessions[0].Source)
	assert.Equal(t, int64(42), reporter.calls[0].sessions[0].TokenUsage.TotalTokens)
	assert.Equal(t, "2026-06-28T11:59:58Z", reporter.calls[0].sessions[0].LastUserMessageAt)
	require.Len(t, reporter.calls[0].sessions[0].Messages, 2)
	assert.Equal(t, "done", reporter.calls[0].sessions[0].Messages[1].Text)
}

func TestLatestUserMessageAtIgnoresAssistantAndUsesCompletedAtFallback(t *testing.T) {
	messages := []model.SessionMessage{
		{Role: "user", StartedAt: "2026-06-28T10:00:00Z"},
		{Role: "assistant", CompletedAt: "2026-06-28T12:00:00Z"},
		{Role: "user", CompletedAt: "2026-06-28T11:00:00Z"},
		{Role: "user", StartedAt: "not-a-time"},
	}

	assert.Equal(t, "2026-06-28T11:00:00Z", latestUserMessageAt(messages))
}

func TestRunOnceBatchesDuplicateSessionsPerCloudAgent(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{
		{Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "conn_a",
			RemoteID:     "remote_1",
			CloudAPIURL:  "https://one.example",
			CloudAgentID: "agent_1",
			AgentType:    "codex",
		}},
		{Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "conn_b",
			RemoteID:     "remote_1",
			CloudAPIURL:  "https://one.example",
			CloudAgentID: "agent_1",
			AgentType:    "codex",
		}},
	}}
	scanner := &fakeScanner{
		sessionsByConnection: map[string][]model.SessionInfo{
			"conn_a": {
				{SessionID: "codex:sess_1", NativeID: "sess_1", Name: "First"},
				{SessionID: "codex:sess_2", NativeID: "sess_2", Name: "Second"},
			},
			"conn_b": {
				{SessionID: "codex:sess_2", NativeID: "sess_2", Name: "Duplicate"},
				{SessionID: "codex:sess_3", NativeID: "sess_3", Name: "Third"},
			},
		},
	}
	reporter := &fakeCloudReporter{}
	service := New(Options{
		RuntimeSource: source,
		Scanner:       scanner,
		Reporter:      reporter,
		RouteResolver: passThroughRouteResolver{},
		BatchSize:     2,
	})

	require.NoError(t, service.RunOnce(context.Background()))

	require.Len(t, scanner.calls, 2)
	assert.Equal(t, 2, scanner.calls[0].Limit)
	assert.Equal(t, 2, scanner.calls[1].Limit)
	require.Len(t, reporter.calls, 1)
	assert.Equal(t, "agent_1", reporter.calls[0].agentID)
	require.Len(t, reporter.calls[0].sessions, 2)
	assert.Equal(t, "sess_1", reporter.calls[0].sessions[0].SessionID)
	assert.Equal(t, "First", reporter.calls[0].sessions[0].Name)
	assert.Equal(t, "sess_2", reporter.calls[0].sessions[1].SessionID)
	assert.Equal(t, "Second", reporter.calls[0].sessions[1].Name)
}

func TestRunOnceSkipsRuntimeWithoutCloudAgentID(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
		Spec: runtimes.AgentConnectionSpec{ConnectionID: "conn_1", RemoteID: "remote_1"},
	}}}
	scanner := &fakeScanner{sessions: []model.SessionInfo{{SessionID: "sess_1"}}}
	reporter := &fakeCloudReporter{}
	service := New(Options{RuntimeSource: source, Scanner: scanner, Reporter: reporter, RouteResolver: passThroughRouteResolver{}})

	require.NoError(t, service.RunOnce(context.Background()))

	assert.Empty(t, scanner.calls)
	assert.Empty(t, reporter.calls)
}

func TestRunOnceContinuesAfterScanFailure(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{
		{Spec: runtimes.AgentConnectionSpec{ConnectionID: "bad", RemoteID: "remote_1", CloudAPIURL: "https://one.example", CloudAgentID: "agent_bad"}},
		{Spec: runtimes.AgentConnectionSpec{ConnectionID: "good", RemoteID: "remote_1", CloudAPIURL: "https://one.example", CloudAgentID: "agent_good"}},
	}}
	scanner := &fakeScanner{
		errByConnection: map[string]error{"bad": errors.New("scan failed")},
		sessionsByConnection: map[string][]model.SessionInfo{
			"good": {{SessionID: "sess_good", NativeID: "sess_good"}},
		},
	}
	reporter := &fakeCloudReporter{}
	service := New(Options{RuntimeSource: source, Scanner: scanner, Reporter: reporter, RouteResolver: passThroughRouteResolver{}})

	require.NoError(t, service.RunOnce(context.Background()))

	require.Len(t, scanner.calls, 2)
	require.Len(t, reporter.calls, 1)
	assert.Equal(t, "agent_good", reporter.calls[0].agentID)
}

func TestRunOnceScanTimeoutCancelsScannerAndContinues(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{
		{Spec: runtimes.AgentConnectionSpec{ConnectionID: "slow", RemoteID: "remote_1", CloudAPIURL: "https://one.example", CloudAgentID: "agent_slow"}},
		{Spec: runtimes.AgentConnectionSpec{ConnectionID: "fast", RemoteID: "remote_1", CloudAPIURL: "https://one.example", CloudAgentID: "agent_fast"}},
	}}
	scanner := &fakeScanner{
		blockByConnection: map[string]bool{"slow": true},
		sessionsByConnection: map[string][]model.SessionInfo{
			"fast": {{SessionID: "sess_fast", NativeID: "sess_fast"}},
		},
	}
	reporter := &fakeCloudReporter{}
	service := New(Options{
		RuntimeSource: source,
		Scanner:       scanner,
		Reporter:      reporter,
		RouteResolver: passThroughRouteResolver{},
		ScanTimeout:   10 * time.Millisecond,
	})

	require.NoError(t, service.RunOnce(context.Background()))

	require.Len(t, reporter.calls, 1)
	assert.Equal(t, "agent_fast", reporter.calls[0].agentID)
	assert.True(t, scanner.sawCanceled("slow"))
}

func TestStartReturnsImmediatelyAndRunsImmediatePass(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
		Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "conn_1",
			RemoteID:     "remote_1",
			CloudAPIURL:  "https://one.example",
			CloudAgentID: "agent_1",
		},
	}}}
	scanner := &fakeScanner{sessions: []model.SessionInfo{{SessionID: "sess_1", NativeID: "sess_1"}}}
	reporter := &fakeCloudReporter{reported: make(chan struct{}, 1)}
	service := New(Options{
		RuntimeSource: source,
		Scanner:       scanner,
		Reporter:      reporter,
		RouteResolver: passThroughRouteResolver{},
		Interval:      time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	service.Start(ctx)

	assert.Less(t, time.Since(started), 50*time.Millisecond)
	select {
	case <-reporter.reported:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for immediate report")
	}
}

func TestStartSkipsOverlappingRunsPerRemote(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{
		{Spec: runtimes.AgentConnectionSpec{ConnectionID: "a", RemoteID: "remote_a", CloudAPIURL: "https://a.example", CloudAgentID: "agent_a"}},
		{Spec: runtimes.AgentConnectionSpec{ConnectionID: "b", RemoteID: "remote_b", CloudAPIURL: "https://b.example", CloudAgentID: "agent_b"}},
	}}
	scanner := &fakeScanner{sessions: []model.SessionInfo{{SessionID: "sess_1", NativeID: "sess_1"}}}
	reporter := &fakeCloudReporter{
		blockRemote: "remote_a",
		blockCh:     make(chan struct{}),
		reported:    make(chan struct{}, 10),
	}
	service := New(Options{
		RuntimeSource: source,
		Scanner:       scanner,
		Reporter:      reporter,
		RouteResolver: passThroughRouteResolver{},
		Interval:      10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service.Start(ctx)
	require.Eventually(t, func() bool {
		return reporter.countForRemote("remote_b") >= 2
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, reporter.countForRemote("remote_a"))
	close(reporter.blockCh)
}

func TestRunOnceReportsOnlySessionsWithBoundACPRoutes(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
		Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "conn_1",
			RemoteID:     "remote_1",
			CloudAgentID: "agent_1",
		},
	}}}
	scanner := &fakeScanner{sessions: []model.SessionInfo{
		{SessionID: "codex:untracked", NativeID: "native_untracked"},
		{SessionID: "codex:pending", NativeID: "native_pending"},
		{SessionID: "codex:bound", NativeID: "native_bound"},
	}}
	routes := &fakeRouteResolver{routes: map[string]routeResolution{
		"conn_1/native_pending": {found: true},
		"conn_1/native_bound":   {managerSessionID: "sess_manager", found: true},
	}}
	reporter := &fakeCloudReporter{}
	service := New(Options{
		RuntimeSource: source,
		Scanner:       scanner,
		Reporter:      reporter,
		RouteResolver: routes,
	})

	require.NoError(t, service.RunOnce(context.Background()))

	require.Len(t, reporter.calls, 1)
	require.Len(t, reporter.calls[0].sessions, 1)
	assert.Equal(t, "sess_manager", reporter.calls[0].sessions[0].SessionID)
	assert.Equal(t, "native_bound", reporter.calls[0].sessions[0].NativeID)
}

func TestRunOnceDefersPendingRouteUntilNextPass(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
		Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "conn_1",
			RemoteID:     "remote_1",
			CloudAgentID: "agent_1",
		},
	}}}
	scanner := &fakeScanner{sessions: []model.SessionInfo{{
		SessionID: "codex:native_1",
		NativeID:  "native_1",
	}}}
	routes := &fakeRouteResolver{routes: map[string]routeResolution{
		"conn_1/native_1": {found: true},
	}}
	reporter := &fakeCloudReporter{}
	service := New(Options{
		RuntimeSource: source,
		Scanner:       scanner,
		Reporter:      reporter,
		RouteResolver: routes,
	})

	require.NoError(t, service.RunOnce(context.Background()))
	assert.Empty(t, reporter.calls)

	routes.set("conn_1", "native_1", routeResolution{
		managerSessionID: "sess_manager_1",
		found:            true,
	})
	require.NoError(t, service.RunOnce(context.Background()))

	require.Len(t, reporter.calls, 1)
	require.Len(t, reporter.calls[0].sessions, 1)
	assert.Equal(t, "sess_manager_1", reporter.calls[0].sessions[0].SessionID)
}

func TestRunOnceDoesNotFallbackWhenRouteResolutionFails(t *testing.T) {
	source := fakeRuntimeSource{runtimes: []supervisor.ObservedAgentRuntime{{
		Spec: runtimes.AgentConnectionSpec{
			ConnectionID: "conn_1",
			RemoteID:     "remote_1",
			CloudAgentID: "agent_1",
		},
	}}}
	reporter := &fakeCloudReporter{}
	service := New(Options{
		RuntimeSource: source,
		Scanner: &fakeScanner{sessions: []model.SessionInfo{{
			SessionID: "codex:native_1",
			NativeID:  "native_1",
		}}},
		Reporter: reporter,
		RouteResolver: &fakeRouteResolver{routes: map[string]routeResolution{
			"conn_1/native_1": {err: errors.New("sqlite unavailable")},
		}},
	})

	require.NoError(t, service.RunOnce(context.Background()))
	assert.Empty(t, reporter.calls)
}

type fakeRuntimeSource struct {
	runtimes []supervisor.ObservedAgentRuntime
}

type passThroughRouteResolver struct{}

func (passThroughRouteResolver) ResolveManagerSessionID(
	_ context.Context,
	_ string,
	nativeSessionID string,
) (string, bool, error) {
	return nativeSessionID, true, nil
}

type routeResolution struct {
	managerSessionID string
	found            bool
	err              error
}

type fakeRouteResolver struct {
	mu     sync.Mutex
	routes map[string]routeResolution
}

func (r *fakeRouteResolver) ResolveManagerSessionID(
	_ context.Context,
	connectionID string,
	nativeSessionID string,
) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	resolution := r.routes[connectionID+"/"+nativeSessionID]
	return resolution.managerSessionID, resolution.found, resolution.err
}

func (r *fakeRouteResolver) set(connectionID string, nativeSessionID string, resolution routeResolution) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[connectionID+"/"+nativeSessionID] = resolution
}

func (s fakeRuntimeSource) ObservedAgentRuntimes() []supervisor.ObservedAgentRuntime {
	return append([]supervisor.ObservedAgentRuntime(nil), s.runtimes...)
}

type fakeScanner struct {
	sessions             []model.SessionInfo
	sessionsByConnection map[string][]model.SessionInfo
	errByConnection      map[string]error
	blockByConnection    map[string]bool

	mu       sync.Mutex
	calls    []SessionScannerSpec
	canceled map[string]bool
}

func (s *fakeScanner) ListSessions(ctx context.Context, spec SessionScannerSpec) ([]model.SessionInfo, error) {
	s.mu.Lock()
	s.calls = append(s.calls, spec)
	s.mu.Unlock()
	if s.blockByConnection[spec.ConnectionID] {
		<-ctx.Done()
		s.mu.Lock()
		if s.canceled == nil {
			s.canceled = make(map[string]bool)
		}
		s.canceled[spec.ConnectionID] = true
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	if err := s.errByConnection[spec.ConnectionID]; err != nil {
		return nil, err
	}
	if sessions, ok := s.sessionsByConnection[spec.ConnectionID]; ok {
		return sessions, nil
	}
	return s.sessions, nil
}

func (s *fakeScanner) sawCanceled(connectionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canceled[connectionID]
}

type fakeReportCall struct {
	target   ReportTarget
	agentID  string
	sessions []cloud.SessionStatus
}

type fakeCloudReporter struct {
	blockRemote string
	blockCh     chan struct{}
	reported    chan struct{}

	mu    sync.Mutex
	calls []fakeReportCall
}

func (r *fakeCloudReporter) ReportAgentSessions(
	ctx context.Context,
	target ReportTarget,
	agentID string,
	sessions []cloud.SessionStatus,
) error {
	r.mu.Lock()
	r.calls = append(r.calls, fakeReportCall{
		target:   target,
		agentID:  agentID,
		sessions: append([]cloud.SessionStatus(nil), sessions...),
	})
	r.mu.Unlock()
	if r.reported != nil {
		select {
		case r.reported <- struct{}{}:
		default:
		}
	}
	if target.RemoteID == r.blockRemote && r.blockCh != nil {
		select {
		case <-r.blockCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *fakeCloudReporter) countForRemote(remoteID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, call := range r.calls {
		if call.target.RemoteID == remoteID {
			count++
		}
	}
	return count
}
