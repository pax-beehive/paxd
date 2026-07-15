package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentConnectionSupervisorWakeStartsTunnelSession(t *testing.T) {
	store := newFakeAgentStore()
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 1, 0)})
	factory := &fakeAgentFactory{}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	session := factory.waitSession(t, 0)
	session.waitStarted(t)
	status := store.latestStatus("conn_1")
	if status.Phase != string(runtimes.PhaseStarting) {
		t.Fatalf("status phase = %q, want starting", status.Phase)
	}
	if got := len(factory.specs()); got != 1 {
		t.Fatalf("sessions started = %d, want 1", got)
	}
}

func TestACPSlotSupervisorRestartsWithNewProcessEpochOnDesiredChange(t *testing.T) {
	store := newFakeACPSlotStore()
	store.setDesired([]runtimes.ACPSlotSpec{acpSlotSpec("slot_1", "conn_1", 1, 0)})
	factory := &fakeACPSlotFactory{}
	sup := NewACPSlotSupervisor(ACPSlotSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	require.NoError(t, sup.Reconcile(context.Background()))
	first := factory.waitSession(t, 0)
	first.waitStarted(t)
	firstEpoch := factory.specs()[0].ProcessEpoch
	require.NotEmpty(t, firstEpoch)

	store.setDesired([]runtimes.ACPSlotSpec{acpSlotSpec("slot_1", "conn_1", 2, 0)})
	require.NoError(t, sup.Reconcile(context.Background()))
	first.waitDone(t)
	second := factory.waitSession(t, 1)
	second.waitStarted(t)
	secondEpoch := factory.specs()[1].ProcessEpoch

	require.NotEmpty(t, secondEpoch)
	assert.NotEqual(t, firstEpoch, secondEpoch)
	assert.Equal(t, secondEpoch, store.latestStatus("slot_1").ProcessEpoch)
}

func TestACPSlotSupervisorStopsActiveSlotWhenDesiredRemoved(t *testing.T) {
	store := newFakeACPSlotStore()
	store.setDesired([]runtimes.ACPSlotSpec{acpSlotSpec("slot_1", "conn_1", 1, 0)})
	factory := &fakeACPSlotFactory{}
	stopped := make(chan runtimes.ACPSlotSpec, 1)
	sup := NewACPSlotSupervisor(ACPSlotSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
		StopHandler: func(ctx context.Context, spec runtimes.ACPSlotSpec) {
			_ = ctx
			stopped <- spec
		},
	})

	require.NoError(t, sup.Reconcile(context.Background()))
	session := factory.waitSession(t, 0)
	session.waitStarted(t)
	store.setDesired(nil)
	require.NoError(t, sup.Reconcile(context.Background()))

	session.waitDone(t)
	select {
	case spec := <-stopped:
		assert.Equal(t, "slot_1", spec.SlotID)
		assert.Equal(t, "conn_1", spec.ConnectionID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for active slot stop handler")
	}
	require.Eventually(t, func() bool {
		return store.latestStatus("slot_1").Phase == PhaseStopped
	}, 2*time.Second, time.Millisecond)
}

func TestAgentConnectionSupervisorObservedRuntimesReturnsCopiedSpecs(t *testing.T) {
	store := newFakeAgentStore()
	spec := agentSpec("conn_1", 1, 0)
	spec.Command = []string{"codex", "--acp"}
	spec.WorkingDir = "/workspace/paxd"
	spec.Env = map[string]string{"PAX_TOKEN": "secret-ref"}
	store.setDesired([]runtimes.AgentConnectionSpec{spec})
	factory := &fakeAgentFactory{}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	require.NoError(t, sup.Reconcile(context.Background()))

	observed := sup.ObservedAgentRuntimes()
	require.Len(t, observed, 1)
	assert.Equal(t, "conn_1", observed[0].Spec.ConnectionID)
	assert.Equal(t, string(runtimes.PhaseStarting), observed[0].Phase)
	assert.Equal(t, []string{"codex", "--acp"}, observed[0].Spec.Command)
	assert.Equal(t, map[string]string{"PAX_TOKEN": "secret-ref"}, observed[0].Spec.Env)

	observed[0].Spec.Command[0] = "mutated"
	observed[0].Spec.Env["PAX_TOKEN"] = "mutated"

	again := sup.ObservedAgentRuntimes()
	require.Len(t, again, 1)
	assert.Equal(t, []string{"codex", "--acp"}, again[0].Spec.Command)
	assert.Equal(t, "secret-ref", again[0].Spec.Env["PAX_TOKEN"])
}

func TestAgentConnectionSupervisorRotatesTransportQueueAfterReconcileRotate(t *testing.T) {
	store := newFakeAgentStore()
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 1, 0)})
	factory := &fakeAgentFactory{
		makeSession: func(_ runtimes.AgentConnectionSpec, index int) *scriptedSession {
			if index == 0 {
				return instantSession(runtimes.TransientExit("reconcile_rotate", "manager requested queue rotation"))
			}
			return blockingSession()
		},
	}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	require.NoError(t, sup.Reconcile(context.Background()))

	factory.waitSession(t, 1).waitStarted(t)
	specs := factory.specs()
	require.Len(t, specs, 2)
	assert.Equal(t, "agent_1:queue_1", specs[0].TransportQueueID)
	assert.Equal(t, "agent_1:queue_rotated", specs[1].TransportQueueID)
	assert.Equal(t, "agent_1:queue_rotated", store.desiredQueueID("conn_1"))
}

func TestRemoteSupervisorWakeLoopReconcilesDesiredState(t *testing.T) {
	store := newFakeRemoteStore()
	factory := &fakeRemoteFactory{}
	sup := NewRemoteSupervisor(RemoteSupervisorOptions{
		Store:             store,
		Factory:           factory,
		Clock:             newFakeClock(),
		ReconcileInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- sup.Start(ctx) }()

	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 1, 0)})
	sup.Wake()
	factory.waitSession(t, 0).waitStarted(t)

	cancel()
	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}
}

func TestStatusWritersPokeRemoteAfterSuccessfulWrite(t *testing.T) {
	ctx := context.Background()
	poke := &fakeStatusPoke{}
	remoteStore := newFakeRemoteStore()
	remoteWriter := remoteStatusWriter(remoteStore, poke)
	if err := remoteWriter(ctx, remoteSpec("remote_1", 2, 0), statusWrite{Phase: "connected", At: time.Now()}); err != nil {
		t.Fatalf("remote writer error = %v", err)
	}
	if got := poke.remotes(); len(got) != 1 || got[0] != "remote_1" {
		t.Fatalf("remote pokes = %+v, want remote_1", got)
	}
	if err := remoteWriter(ctx, remoteSpec("remote_1", 1, 0), statusWrite{Phase: "stale", At: time.Now()}); err != nil {
		t.Fatalf("stale remote writer error = %v", err)
	}
	if got := poke.remotes(); len(got) != 1 {
		t.Fatalf("remote pokes after stale write = %+v, want no additional poke", got)
	}

	agentStore := newFakeAgentStore()
	agentWriter := agentConnectionStatusWriter(agentStore, poke)
	if err := agentWriter(ctx, agentSpec("conn_1", 2, 0), statusWrite{Phase: "running", At: time.Now()}); err != nil {
		t.Fatalf("agent writer error = %v", err)
	}
	if got := poke.remotes(); len(got) != 2 || got[1] != "remote_1" {
		t.Fatalf("agent pokes = %+v, want remote_1 appended", got)
	}
	if err := agentWriter(ctx, agentSpec("conn_1", 1, 0), statusWrite{Phase: "stale", At: time.Now()}); err != nil {
		t.Fatalf("stale agent writer error = %v", err)
	}
	if got := poke.remotes(); len(got) != 2 {
		t.Fatalf("agent pokes after stale write = %+v, want no additional poke", got)
	}
}

func TestTransientExitEntersInterruptibleBackoffAndTimerRestarts(t *testing.T) {
	clock := newFakeClock()
	store := newFakeRemoteStore()
	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 1, 0)})
	factory := &fakeRemoteFactory{
		makeSession: func(spec runtimes.RemoteSpec, index int) *scriptedSession {
			return instantSession(runtimes.TransientExit("dial_failed", "temporary network failure"))
		},
	}
	sup := NewRemoteSupervisor(RemoteSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   clock,
		Backoff: BackoffPolicy{Initial: time.Second, Max: time.Second},
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	waitForStatus(t, func() daemonstore.RemoteStatusUpdate {
		return store.latestStatus("remote_1")
	}, PhaseBackoff)
	status := store.latestStatus("remote_1")
	if status.ReconnectAttempt != 1 || status.NextRetryAt == nil {
		t.Fatalf("backoff status = %+v, want attempt 1 with next retry", status)
	}

	clock.fireAll()
	factory.waitSession(t, 1).waitStarted(t)
	waitForStatus(t, func() daemonstore.RemoteStatusUpdate {
		return store.latestStatus("remote_1")
	}, PhaseBackoff)
	status = store.latestStatus("remote_1")
	if status.ReconnectAttempt != 2 {
		t.Fatalf("reconnect attempt after timer = %d, want 2", status.ReconnectAttempt)
	}
}

func TestAgentConnectionBackoffResetsAfterConnectedTransientExit(t *testing.T) {
	clock := newFakeClock()
	store := newFakeAgentStore()
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 1, 0)})
	exits := []runtimes.Exit{
		runtimes.TransientExit("dial_failed", "temporary network failure"),
		runtimes.TransientExit("dial_failed", "temporary network failure"),
		runtimes.TransientExit("session_ended", "agent tunnel session ended").WithBackoffReset(),
	}
	factory := &fakeAgentFactory{
		makeSession: func(spec runtimes.AgentConnectionSpec, index int) *scriptedSession {
			return instantSession(exits[index])
		},
	}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   clock,
		Backoff: BackoffPolicy{Initial: time.Second, Max: 30 * time.Second},
	})

	require.NoError(t, sup.Reconcile(context.Background()))
	waitForAgentStatusMatch(t, store, "conn_1", func(status daemonstore.AgentConnectionStatusUpdate) bool {
		return status.LastErrorCode == "dial_failed" && status.ReconnectAttempt == 1
	})
	first := store.latestStatus("conn_1")
	require.NotNil(t, first.NextRetryAt)
	assert.Equal(t, time.Second, first.NextRetryAt.Sub(clock.Now()))

	clock.fireAll()
	factory.waitSession(t, 1).waitStarted(t)
	waitForAgentStatusMatch(t, store, "conn_1", func(status daemonstore.AgentConnectionStatusUpdate) bool {
		return status.LastErrorCode == "dial_failed" && status.ReconnectAttempt == 2
	})
	second := store.latestStatus("conn_1")
	require.NotNil(t, second.NextRetryAt)
	assert.Equal(t, 2*time.Second, second.NextRetryAt.Sub(clock.Now()))

	clock.fireAll()
	factory.waitSession(t, 2).waitStarted(t)
	waitForAgentStatusMatch(t, store, "conn_1", func(status daemonstore.AgentConnectionStatusUpdate) bool {
		return status.LastErrorCode == "session_ended" && status.ReconnectAttempt == 1
	})
	reset := store.latestStatus("conn_1")
	require.NotNil(t, reset.NextRetryAt)
	assert.Equal(t, time.Second, reset.NextRetryAt.Sub(clock.Now()))
}

func TestDesiredUpdateInterruptsRunningSessionAndCoalescesLatestSpec(t *testing.T) {
	store := newFakeAgentStore()
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 1, 0)})
	factory := &fakeAgentFactory{
		makeSession: func(spec runtimes.AgentConnectionSpec, index int) *scriptedSession {
			return blockingSession()
		},
	}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	factory.waitSession(t, 0).waitStarted(t)

	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 2, 0)})
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("generation 2 Reconcile() error = %v", err)
	}
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 3, 0)})
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("generation 3 Reconcile() error = %v", err)
	}

	factory.waitSession(t, 1).waitStarted(t)
	specs := factory.specs()
	if len(specs) != 2 {
		t.Fatalf("started specs = %d, want 2", len(specs))
	}
	if specs[1].Generation != 3 {
		t.Fatalf("second started generation = %d, want 3", specs[1].Generation)
	}
	status := store.latestStatus("conn_1")
	if status.ObservedGeneration != 3 {
		t.Fatalf("latest status generation = %d, want 3", status.ObservedGeneration)
	}
}

func TestDesiredRemovalCancelsBackoffAndWritesStopped(t *testing.T) {
	clock := newFakeClock()
	store := newFakeAgentStore()
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 1, 0)})
	factory := &fakeAgentFactory{
		makeSession: func(spec runtimes.AgentConnectionSpec, index int) *scriptedSession {
			return instantSession(runtimes.TransientExit("disconnect", "peer closed"))
		},
	}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   clock,
		Backoff: BackoffPolicy{Initial: time.Hour},
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	waitForAgentStatus(t, store, "conn_1", PhaseBackoff)

	store.setDesired(nil)
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("remove Reconcile() error = %v", err)
	}
	status := store.latestStatus("conn_1")
	if status.Phase != PhaseStopped {
		t.Fatalf("status phase = %q, want stopped", status.Phase)
	}
	clock.fireAll()
	if got := len(factory.specs()); got != 1 {
		t.Fatalf("sessions started after stopped timer = %d, want 1", got)
	}
}

func TestAgentConnectionStopHandlerRunsWhenDesiredRemovedDuringBackoff(t *testing.T) {
	clock := newFakeClock()
	store := newFakeAgentStore()
	spec := agentSpec("conn_1", 1, 0)
	store.setDesired([]runtimes.AgentConnectionSpec{spec})
	factory := &fakeAgentFactory{
		makeSession: func(spec runtimes.AgentConnectionSpec, index int) *scriptedSession {
			return instantSession(runtimes.TransientExit("disconnect", "peer closed"))
		},
	}
	var stopped []runtimes.AgentConnectionSpec
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   clock,
		Backoff: BackoffPolicy{Initial: time.Hour},
		StopHandler: func(ctx context.Context, spec runtimes.AgentConnectionSpec) {
			stopped = append(stopped, spec)
		},
	})

	require.NoError(t, sup.Reconcile(context.Background()))
	waitForAgentStatus(t, store, "conn_1", PhaseBackoff)

	store.setDesired(nil)
	require.NoError(t, sup.Reconcile(context.Background()))

	require.Len(t, stopped, 1)
	assert.Equal(t, "conn_1", stopped[0].ConnectionID)
}

func TestAuthFailureEntersFailedStateAndRedactsMessage(t *testing.T) {
	store := newFakeRemoteStore()
	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 1, 0)})
	factory := &fakeRemoteFactory{
		makeSession: func(spec runtimes.RemoteSpec, index int) *scriptedSession {
			return instantSession(runtimes.AuthExit("unauthorized", "token secret-value was rejected"))
		},
	}
	sup := NewRemoteSupervisor(RemoteSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	waitForStatus(t, func() daemonstore.RemoteStatusUpdate {
		return store.latestStatus("remote_1")
	}, PhaseFailed)
	status := store.latestStatus("remote_1")
	if status.FailureClass != string(runtimes.ExitAuth) {
		t.Fatalf("failure class = %q, want auth", status.FailureClass)
	}
	if status.LastErrorMessage != "authentication failed" {
		t.Fatalf("error message = %q, want redacted auth message", status.LastErrorMessage)
	}
	if got := len(factory.specs()); got != 1 {
		t.Fatalf("sessions started = %d, want no retry", got)
	}
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("same desired Reconcile() error = %v", err)
	}
	if got := len(factory.specs()); got != 1 {
		t.Fatalf("sessions started after same failed desired = %d, want 1", got)
	}
}

func TestManualRestartInterruptsFailedState(t *testing.T) {
	store := newFakeRemoteStore()
	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 1, 0)})
	factory := &fakeRemoteFactory{
		makeSession: func(spec runtimes.RemoteSpec, index int) *scriptedSession {
			return instantSession(runtimes.ConfigExit("bad_config", "invalid url"))
		},
	}
	sup := NewRemoteSupervisor(RemoteSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	waitForStatus(t, func() daemonstore.RemoteStatusUpdate {
		return store.latestStatus("remote_1")
	}, PhaseFailed)
	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 1, 1)})
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("restart Reconcile() error = %v", err)
	}
	factory.waitSession(t, 1).waitStarted(t)
}

func TestSnapshotReportsSlotStateWithoutSecrets(t *testing.T) {
	store := newFakeAgentStore()
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 7, 2)})
	factory := &fakeAgentFactory{}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	snap := sup.Snapshot()
	if len(snap.Slots) != 1 {
		t.Fatalf("slots = %d, want 1", len(snap.Slots))
	}
	slot := snap.Slots[0]
	if slot.ID != "conn_1" || slot.Generation != 7 || slot.RestartNonce != 2 || slot.Phase != string(runtimes.PhaseStarting) {
		t.Fatalf("slot snapshot = %+v", slot)
	}
}

func TestRemoteSnapshotWrapperReportsSlots(t *testing.T) {
	store := newFakeRemoteStore()
	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 4, 1)})
	sup := NewRemoteSupervisor(RemoteSupervisorOptions{
		Store:   store,
		Factory: &fakeRemoteFactory{},
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	snap := sup.Snapshot()
	if len(snap.Slots) != 1 || snap.Slots[0].ID != "remote_1" {
		t.Fatalf("snapshot = %+v, want remote_1 slot", snap)
	}
}

func TestAgentStartWakeWrapperReconcilesDesiredState(t *testing.T) {
	store := newFakeAgentStore()
	factory := &fakeAgentFactory{}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:             store,
		Factory:           factory,
		Clock:             newFakeClock(),
		ReconcileInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- sup.Start(ctx) }()

	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 1, 0)})
	sup.Wake()
	factory.waitSession(t, 0).waitStarted(t)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}
}

func TestTerminalAndInvalidExitStatuses(t *testing.T) {
	store := newFakeRemoteStore()
	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 1, 0)})
	factory := &fakeRemoteFactory{
		makeSession: func(spec runtimes.RemoteSpec, index int) *scriptedSession {
			if index == 0 {
				return instantSession(runtimes.TerminalExit("disabled", "remote disabled"))
			}
			return instantSession(runtimes.Exit{Class: runtimes.ExitClass("mystery")})
		},
	}
	sup := NewRemoteSupervisor(RemoteSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("terminal Reconcile() error = %v", err)
	}
	waitForStatus(t, func() daemonstore.RemoteStatusUpdate {
		return store.latestStatus("remote_1")
	}, PhaseStopped)
	if status := store.latestStatus("remote_1"); status.StoppedAt == nil {
		t.Fatalf("stopped status = %+v, want stopped_at", status)
	}

	store.setDesired([]runtimes.RemoteSpec{remoteSpec("remote_1", 2, 0)})
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("invalid exit Reconcile() error = %v", err)
	}
	waitForStatus(t, func() daemonstore.RemoteStatusUpdate {
		return store.latestStatus("remote_1")
	}, PhaseFailed)
	status := store.latestStatus("remote_1")
	if status.LastErrorCode != "invalid_exit_class" {
		t.Fatalf("error code = %q, want invalid_exit_class", status.LastErrorCode)
	}
}

func TestNilSessionFallsBackToConfigFailure(t *testing.T) {
	store := newFakeAgentStore()
	store.setDesired([]runtimes.AgentConnectionSpec{agentSpec("conn_1", 1, 0)})
	factory := &fakeAgentFactory{
		makeSession: func(spec runtimes.AgentConnectionSpec, index int) *scriptedSession {
			return nil
		},
	}
	sup := NewAgentConnectionSupervisor(AgentConnectionSupervisorOptions{
		Store:   store,
		Factory: factory,
		Clock:   newFakeClock(),
	})

	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	waitForAgentStatus(t, store, "conn_1", PhaseFailed)
	status := store.latestStatus("conn_1")
	if status.LastErrorCode != "missing_session" {
		t.Fatalf("error code = %q, want missing_session", status.LastErrorCode)
	}
}

func TestReconcileIgnoresEmptyIDsAndReturnsListErrors(t *testing.T) {
	store := newFakeRemoteStore()
	store.setDesired([]runtimes.RemoteSpec{remoteSpec("", 1, 0)})
	sup := NewRemoteSupervisor(RemoteSupervisorOptions{
		Store:   store,
		Factory: &fakeRemoteFactory{},
		Clock:   newFakeClock(),
	})
	if err := sup.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := len(sup.Snapshot().Slots); got != 0 {
		t.Fatalf("slots = %d, want empty id to be ignored", got)
	}

	store.listErr = errors.New("store down")
	if err := sup.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile() error = nil, want store error")
	}
}

func remoteSpec(id string, generation, restartNonce int64) runtimes.RemoteSpec {
	return runtimes.RemoteSpec{
		RemoteID:     id,
		Name:         id,
		CloudAPIURL:  "https://manager.example.test",
		Generation:   generation,
		RestartNonce: restartNonce,
	}
}

func agentSpec(id string, generation, restartNonce int64) runtimes.AgentConnectionSpec {
	return runtimes.AgentConnectionSpec{
		ConnectionID:     id,
		RemoteID:         "remote_1",
		CloudAPIURL:      "https://manager.example.test",
		CloudAgentID:     "agent_1",
		TransportQueueID: "agent_1:queue_1",
		InstanceID:       "inst_1",
		AgentType:        "codex",
		Harness:          "codex",
		Command:          []string{"codex", "serve"},
		TunnelPath:       "/api/v1/agent/tunnel",
		Generation:       generation,
		RestartNonce:     restartNonce,
	}
}

func acpSlotSpec(slotID string, connectionID string, generation, restartNonce int64) runtimes.ACPSlotSpec {
	return runtimes.ACPSlotSpec{
		ConnectionID:     connectionID,
		CloudAgentID:     "agent_1",
		TransportQueueID: "agent_1:queue_1",
		SlotID:           slotID,
		Ordinal:          0,
		Command:          []string{"codex", "serve"},
		Generation:       generation,
		RestartNonce:     restartNonce,
	}
}

type fakeRemoteStore struct {
	mu       sync.Mutex
	desired  []runtimes.RemoteSpec
	statuses []daemonstore.RemoteStatusUpdate
	latest   map[string]daemonstore.RemoteStatusUpdate
	listErr  error
}

func newFakeRemoteStore() *fakeRemoteStore {
	return &fakeRemoteStore{latest: make(map[string]daemonstore.RemoteStatusUpdate)}
}

func (s *fakeRemoteStore) setDesired(specs []runtimes.RemoteSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desired = append([]runtimes.RemoteSpec(nil), specs...)
}

func (s *fakeRemoteStore) ListDesiredRemotes(ctx context.Context) ([]runtimes.RemoteSpec, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]runtimes.RemoteSpec(nil), s.desired...), nil
}

func (s *fakeRemoteStore) ConditionalRemoteStatusUpdate(ctx context.Context, update daemonstore.RemoteStatusUpdate) (bool, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.latest[update.RemoteID]
	if ok && stale(update.ObservedGeneration, update.ObservedRestartNonce, current.ObservedGeneration, current.ObservedRestartNonce) {
		s.statuses = append(s.statuses, update)
		return false, nil
	}
	s.latest[update.RemoteID] = update
	s.statuses = append(s.statuses, update)
	return true, nil
}

func (s *fakeRemoteStore) latestStatus(id string) daemonstore.RemoteStatusUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[id]
}

type fakeAgentStore struct {
	mu       sync.Mutex
	desired  []runtimes.AgentConnectionSpec
	statuses []daemonstore.AgentConnectionStatusUpdate
	latest   map[string]daemonstore.AgentConnectionStatusUpdate
}

func newFakeAgentStore() *fakeAgentStore {
	return &fakeAgentStore{latest: make(map[string]daemonstore.AgentConnectionStatusUpdate)}
}

func (s *fakeAgentStore) setDesired(specs []runtimes.AgentConnectionSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desired = append([]runtimes.AgentConnectionSpec(nil), specs...)
}

func (s *fakeAgentStore) ListDesiredAgentConnections(ctx context.Context) ([]runtimes.AgentConnectionSpec, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runtimes.AgentConnectionSpec(nil), s.desired...), nil
}

func (s *fakeAgentStore) ConditionalAgentConnectionStatusUpdate(ctx context.Context, update daemonstore.AgentConnectionStatusUpdate) (bool, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.latest[update.ConnectionID]
	if ok && stale(update.ObservedGeneration, update.ObservedRestartNonce, current.ObservedGeneration, current.ObservedRestartNonce) {
		s.statuses = append(s.statuses, update)
		return false, nil
	}
	s.latest[update.ConnectionID] = update
	s.statuses = append(s.statuses, update)
	return true, nil
}

func (s *fakeAgentStore) RotateAgentTransportQueueID(ctx context.Context, connectionID string, expectedQueueID string) (string, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, spec := range s.desired {
		if spec.ConnectionID != connectionID {
			continue
		}
		if expectedQueueID != "" && spec.TransportQueueID != expectedQueueID {
			return spec.TransportQueueID, nil
		}
		queueID := spec.CloudAgentID + ":queue_rotated"
		s.desired[i].TransportQueueID = queueID
		return queueID, nil
	}
	return "", daemonstore.ErrNotFound
}

func (s *fakeAgentStore) desiredQueueID(connectionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, spec := range s.desired {
		if spec.ConnectionID == connectionID {
			return spec.TransportQueueID
		}
	}
	return ""
}

func (s *fakeAgentStore) latestStatus(id string) daemonstore.AgentConnectionStatusUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[id]
}

type fakeACPSlotStore struct {
	mu       sync.Mutex
	desired  []runtimes.ACPSlotSpec
	statuses []daemonstore.ACPSlotStatusUpdate
	latest   map[string]daemonstore.ACPSlotStatusUpdate
}

func newFakeACPSlotStore() *fakeACPSlotStore {
	return &fakeACPSlotStore{latest: make(map[string]daemonstore.ACPSlotStatusUpdate)}
}

func (s *fakeACPSlotStore) setDesired(specs []runtimes.ACPSlotSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desired = append([]runtimes.ACPSlotSpec(nil), specs...)
}

func (s *fakeACPSlotStore) ListDesiredACPSlots(ctx context.Context) ([]runtimes.ACPSlotSpec, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runtimes.ACPSlotSpec(nil), s.desired...), nil
}

func (s *fakeACPSlotStore) UpsertACPSlotStatus(ctx context.Context, update daemonstore.ACPSlotStatusUpdate) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest[update.SlotID] = update
	s.statuses = append(s.statuses, update)
	return nil
}

func (s *fakeACPSlotStore) latestStatus(id string) daemonstore.ACPSlotStatusUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[id]
}

type fakeStatusPoke struct {
	mu     sync.Mutex
	remote []string
}

func (f *fakeStatusPoke) Poke(remoteID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remote = append(f.remote, remoteID)
}

func (f *fakeStatusPoke) remotes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.remote...)
}

func stale(gen, nonce, currentGen, currentNonce int64) bool {
	return gen < currentGen || (gen == currentGen && nonce < currentNonce)
}

type fakeRemoteFactory struct {
	mu          sync.Mutex
	makeSession func(runtimes.RemoteSpec, int) *scriptedSession
	gotSpecs    []runtimes.RemoteSpec
	sessions    []*scriptedSession
}

func (f *fakeRemoteFactory) NewRemoteControlSession(spec runtimes.RemoteSpec) runtimes.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.sessions)
	session := blockingSession()
	if f.makeSession != nil {
		session = f.makeSession(spec, index)
	}
	f.gotSpecs = append(f.gotSpecs, spec)
	if session != nil {
		f.sessions = append(f.sessions, session)
		return session
	}
	return nil
}

func (f *fakeRemoteFactory) waitSession(t *testing.T, index int) *scriptedSession {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		f.mu.Lock()
		if len(f.sessions) > index {
			session := f.sessions[index]
			f.mu.Unlock()
			return session
		}
		f.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for remote session %d", index)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func (f *fakeRemoteFactory) specs() []runtimes.RemoteSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtimes.RemoteSpec(nil), f.gotSpecs...)
}

type fakeAgentFactory struct {
	mu          sync.Mutex
	makeSession func(runtimes.AgentConnectionSpec, int) *scriptedSession
	gotSpecs    []runtimes.AgentConnectionSpec
	sessions    []*scriptedSession
}

func (f *fakeAgentFactory) NewAgentTunnelSession(spec runtimes.AgentConnectionSpec) runtimes.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.sessions)
	session := blockingSession()
	if f.makeSession != nil {
		session = f.makeSession(spec, index)
	}
	f.gotSpecs = append(f.gotSpecs, spec)
	if session != nil {
		f.sessions = append(f.sessions, session)
		return session
	}
	return nil
}

func (f *fakeAgentFactory) waitSession(t *testing.T, index int) *scriptedSession {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		f.mu.Lock()
		if len(f.sessions) > index {
			session := f.sessions[index]
			f.mu.Unlock()
			return session
		}
		f.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for agent session %d", index)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func (f *fakeAgentFactory) specs() []runtimes.AgentConnectionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtimes.AgentConnectionSpec(nil), f.gotSpecs...)
}

type fakeACPSlotFactory struct {
	mu          sync.Mutex
	makeSession func(runtimes.ACPSlotSpec, int) *scriptedSession
	gotSpecs    []runtimes.ACPSlotSpec
	sessions    []*scriptedSession
}

func (f *fakeACPSlotFactory) NewACPSlotSession(spec runtimes.ACPSlotSpec) runtimes.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.sessions)
	session := blockingSession()
	if f.makeSession != nil {
		session = f.makeSession(spec, index)
	}
	f.gotSpecs = append(f.gotSpecs, spec)
	if session != nil {
		f.sessions = append(f.sessions, session)
		return session
	}
	return nil
}

func (f *fakeACPSlotFactory) waitSession(t *testing.T, index int) *scriptedSession {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		f.mu.Lock()
		if len(f.sessions) > index {
			session := f.sessions[index]
			f.mu.Unlock()
			return session
		}
		f.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for acp slot session %d", index)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func (f *fakeACPSlotFactory) specs() []runtimes.ACPSlotSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtimes.ACPSlotSpec(nil), f.gotSpecs...)
}

type scriptedSession struct {
	started chan struct{}
	done    chan struct{}
	release chan runtimes.Exit
	once    sync.Once
}

func blockingSession() *scriptedSession {
	return &scriptedSession{
		started: make(chan struct{}),
		done:    make(chan struct{}),
		release: make(chan runtimes.Exit, 1),
	}
}

func instantSession(exit runtimes.Exit) *scriptedSession {
	s := blockingSession()
	s.release <- exit
	return s
}

func (s *scriptedSession) Run(ctx context.Context) runtimes.Exit {
	s.once.Do(func() { close(s.started) })
	defer close(s.done)
	select {
	case exit := <-s.release:
		return exit
	case <-ctx.Done():
		return runtimes.CanceledExit(ctx.Err())
	}
}

func (s *scriptedSession) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session start")
	}
}

func (s *scriptedSession) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session done")
	}
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{ch: make(chan time.Time, 1), deadline: c.now.Add(d)}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *fakeClock) fireAll() {
	c.mu.Lock()
	timers := append([]*fakeTimer(nil), c.timers...)
	c.mu.Unlock()
	for _, timer := range timers {
		timer.fire()
	}
}

type fakeTimer struct {
	mu       sync.Mutex
	ch       chan time.Time
	deadline time.Time
	stopped  bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deadline = time.Now().Add(d)
	t.stopped = false
	return true
}

func (t *fakeTimer) fire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.ch <- t.deadline
	t.stopped = true
}

func waitForStatus(t *testing.T, get func() daemonstore.RemoteStatusUpdate, phase string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if status := get(); status.Phase == phase {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for remote status phase %q; latest=%+v", phase, get())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func waitForAgentStatus(t *testing.T, store *fakeAgentStore, id string, phase string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if status := store.latestStatus(id); status.Phase == phase {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for agent status phase %q; latest=%+v", phase, store.latestStatus(id))
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func waitForAgentStatusMatch(t *testing.T, store *fakeAgentStore, id string, match func(daemonstore.AgentConnectionStatusUpdate) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		status := store.latestStatus(id)
		if match(status) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for matching agent status; latest=%+v", status)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
