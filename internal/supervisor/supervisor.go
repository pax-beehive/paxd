package supervisor

import (
	"context"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
)

const (
	PhaseStopped = "stopped"
	PhaseBackoff = "backoff"
	PhaseFailed  = "failed"
)

type Supervisor interface {
	Start(ctx context.Context) error
	Wake()
	Snapshot() Snapshot
}

type Snapshot struct {
	Slots []SlotSnapshot
}

type SlotSnapshot struct {
	ID             string
	Generation     int64
	RestartNonce   int64
	Phase          string
	BackoffUntil   *time.Time
	PendingDesired bool
}

type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type BackoffPolicy struct {
	Initial time.Duration
	Max     time.Duration
	Factor  float64
}

func (p BackoffPolicy) withDefaults() BackoffPolicy {
	if p.Initial <= 0 {
		p.Initial = time.Second
	}
	if p.Max <= 0 {
		p.Max = 30 * time.Second
	}
	if p.Factor <= 1 {
		p.Factor = 2
	}
	return p
}

func (p BackoffPolicy) duration(attempt int) time.Duration {
	p = p.withDefaults()
	if attempt <= 1 {
		return p.Initial
	}
	d := p.Initial
	for i := 1; i < attempt; i++ {
		next := time.Duration(float64(d) * p.Factor)
		if next <= d {
			next = d
		}
		d = next
		if d >= p.Max {
			return p.Max
		}
	}
	return d
}

type RemoteStore interface {
	ListDesiredRemotes(ctx context.Context) ([]runtimes.RemoteSpec, error)
	ConditionalRemoteStatusUpdate(ctx context.Context, update daemonstore.RemoteStatusUpdate) (bool, error)
}

type AgentConnectionStore interface {
	ListDesiredAgentConnections(ctx context.Context) ([]runtimes.AgentConnectionSpec, error)
	ConditionalAgentConnectionStatusUpdate(ctx context.Context, update daemonstore.AgentConnectionStatusUpdate) (bool, error)
}

type RemoteSupervisorOptions struct {
	Store             RemoteStore
	Factory           runtimes.RemoteControlSessionFactory
	Clock             Clock
	ReconcileInterval time.Duration
	Backoff           BackoffPolicy
}

type AgentConnectionSupervisorOptions struct {
	Store             AgentConnectionStore
	Factory           runtimes.AgentTunnelSessionFactory
	Clock             Clock
	ReconcileInterval time.Duration
	Backoff           BackoffPolicy
}

type RemoteSupervisor struct {
	base *baseSupervisor[runtimes.RemoteSpec]
}

func NewRemoteSupervisor(opts RemoteSupervisorOptions) *RemoteSupervisor {
	ops := slotOps[runtimes.RemoteSpec]{
		id:            func(spec runtimes.RemoteSpec) string { return spec.RemoteID },
		generation:    func(spec runtimes.RemoteSpec) int64 { return spec.Generation },
		restartNonce:  func(spec runtimes.RemoteSpec) int64 { return spec.RestartNonce },
		startingPhase: string(runtimes.PhaseConnecting),
		newSession:    opts.Factory.NewRemoteControlSession,
		writeStatus:   remoteStatusWriter(opts.Store),
	}
	return &RemoteSupervisor{base: newBaseSupervisor(opts.Store.ListDesiredRemotes, ops, baseOptions{
		Clock:             opts.Clock,
		ReconcileInterval: opts.ReconcileInterval,
		Backoff:           opts.Backoff,
	})}
}

func (s *RemoteSupervisor) Start(ctx context.Context) error { return s.base.Start(ctx) }
func (s *RemoteSupervisor) Wake()                           { s.base.Wake() }
func (s *RemoteSupervisor) Snapshot() Snapshot              { return s.base.Snapshot() }
func (s *RemoteSupervisor) Reconcile(ctx context.Context) error {
	return s.base.Reconcile(ctx)
}

type AgentConnectionSupervisor struct {
	base *baseSupervisor[runtimes.AgentConnectionSpec]
}

func NewAgentConnectionSupervisor(opts AgentConnectionSupervisorOptions) *AgentConnectionSupervisor {
	ops := slotOps[runtimes.AgentConnectionSpec]{
		id:            func(spec runtimes.AgentConnectionSpec) string { return spec.ConnectionID },
		generation:    func(spec runtimes.AgentConnectionSpec) int64 { return spec.Generation },
		restartNonce:  func(spec runtimes.AgentConnectionSpec) int64 { return spec.RestartNonce },
		startingPhase: string(runtimes.PhaseStarting),
		newSession:    opts.Factory.NewAgentTunnelSession,
		writeStatus:   agentConnectionStatusWriter(opts.Store),
	}
	return &AgentConnectionSupervisor{base: newBaseSupervisor(opts.Store.ListDesiredAgentConnections, ops, baseOptions{
		Clock:             opts.Clock,
		ReconcileInterval: opts.ReconcileInterval,
		Backoff:           opts.Backoff,
	})}
}

func (s *AgentConnectionSupervisor) Start(ctx context.Context) error { return s.base.Start(ctx) }
func (s *AgentConnectionSupervisor) Wake()                           { s.base.Wake() }
func (s *AgentConnectionSupervisor) Snapshot() Snapshot              { return s.base.Snapshot() }
func (s *AgentConnectionSupervisor) Reconcile(ctx context.Context) error {
	return s.base.Reconcile(ctx)
}

type baseOptions struct {
	Clock             Clock
	ReconcileInterval time.Duration
	Backoff           BackoffPolicy
}

type baseSupervisor[S any] struct {
	mu          sync.Mutex
	listDesired func(context.Context) ([]S, error)
	ops         slotOps[S]
	clock       Clock
	backoff     BackoffPolicy
	interval    time.Duration
	wake        chan struct{}
	slots       map[string]*runtimeSlot[S]
}

func newBaseSupervisor[S any](listDesired func(context.Context) ([]S, error), ops slotOps[S], opts baseOptions) *baseSupervisor[S] {
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	interval := opts.ReconcileInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &baseSupervisor[S]{
		listDesired: listDesired,
		ops:         ops,
		clock:       clock,
		backoff:     opts.Backoff.withDefaults(),
		interval:    interval,
		wake:        make(chan struct{}, 1),
		slots:       make(map[string]*runtimeSlot[S]),
	}
}

func (s *baseSupervisor[S]) Start(ctx context.Context) error {
	if err := s.Reconcile(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopAll(ctx)
			return ctx.Err()
		case <-s.wake:
			if err := s.Reconcile(ctx); err != nil {
				return err
			}
		case <-ticker.C:
			if err := s.Reconcile(ctx); err != nil {
				return err
			}
		}
	}
}

func (s *baseSupervisor[S]) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *baseSupervisor[S]) Reconcile(ctx context.Context) error {
	desired, err := s.listDesired(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(desired))
	for _, spec := range desired {
		id := s.ops.id(spec)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
		slot := s.slot(id)
		slot.ApplyDesired(spec)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, slot := range s.slots {
		if _, ok := seen[id]; !ok {
			slot.Stop("not_desired")
		}
	}
	return nil
}

func (s *baseSupervisor[S]) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{Slots: make([]SlotSnapshot, 0, len(s.slots))}
	for _, slot := range s.slots {
		out.Slots = append(out.Slots, slot.Snapshot())
	}
	return out
}

func (s *baseSupervisor[S]) slot(id string) *runtimeSlot[S] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot, ok := s.slots[id]; ok {
		return slot
	}
	slot := newRuntimeSlot(id, s.ops, s.clock, s.backoff)
	s.slots[id] = slot
	return slot
}

func (s *baseSupervisor[S]) stopAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, slot := range s.slots {
		slot.Stop(ctx.Err().Error())
	}
}

type slotOps[S any] struct {
	id            func(S) string
	generation    func(S) int64
	restartNonce  func(S) int64
	startingPhase string
	newSession    func(S) runtimes.Session
	writeStatus   func(context.Context, S, statusWrite) error
}

type statusWrite struct {
	Phase            string
	Exit             runtimes.Exit
	ReconnectAttempt int
	NextRetryAt      *time.Time
	At               time.Time
}

type runtimeSlot[S any] struct {
	mu             sync.Mutex
	id             string
	ops            slotOps[S]
	clock          Clock
	backoff        BackoffPolicy
	desired        S
	hasDesired     bool
	currentCancel  context.CancelFunc
	currentAttempt int64
	reconnects     int
	timer          Timer
	timerToken     int64
	backoffUntil   *time.Time
	phase          string
	pending        bool
}

func newRuntimeSlot[S any](id string, ops slotOps[S], clock Clock, backoff BackoffPolicy) *runtimeSlot[S] {
	return &runtimeSlot[S]{
		id:      id,
		ops:     ops,
		clock:   clock,
		backoff: backoff.withDefaults(),
		phase:   PhaseStopped,
	}
}

func (s *runtimeSlot[S]) ApplyDesired(spec S) {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := !s.hasDesired ||
		s.ops.generation(s.desired) != s.ops.generation(spec) ||
		s.ops.restartNonce(s.desired) != s.ops.restartNonce(spec)
	s.desired = spec
	s.hasDesired = true
	if !changed {
		if s.currentCancel != nil || s.timer != nil || s.phase == PhaseFailed || s.phase == PhaseStopped {
			return
		}
	}
	if s.timer != nil {
		s.stopTimerLocked()
	}
	if s.currentCancel != nil {
		s.pending = true
		s.phase = string(runtimes.PhaseStopping)
		_ = s.writeStatusLocked(spec, statusWrite{Phase: s.phase, At: s.clock.Now()})
		s.currentCancel()
		return
	}
	s.startLocked()
}

func (s *runtimeSlot[S]) Stop(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasDesired = false
	s.pending = false
	if s.timer != nil {
		s.stopTimerLocked()
	}
	if s.currentCancel != nil {
		s.phase = string(runtimes.PhaseStopping)
		s.currentCancel()
		return
	}
	if s.phase != PhaseStopped {
		s.phase = PhaseStopped
		_ = s.writeStatusLocked(s.desired, statusWrite{
			Phase: PhaseStopped,
			Exit:  runtimes.TerminalExit("stopped", reason),
			At:    s.clock.Now(),
		})
	}
}

func (s *runtimeSlot[S]) Snapshot() SlotSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SlotSnapshot{
		ID:             s.id,
		Generation:     s.ops.generation(s.desired),
		RestartNonce:   s.ops.restartNonce(s.desired),
		Phase:          s.phase,
		BackoffUntil:   cloneTimePtr(s.backoffUntil),
		PendingDesired: s.pending,
	}
}

func (s *runtimeSlot[S]) startLocked() {
	if !s.hasDesired || s.currentCancel != nil {
		return
	}
	spec := s.desired
	session := s.ops.newSession(spec)
	if session == nil {
		session = runtimes.Session(sessionFunc(func(context.Context) runtimes.Exit {
			return runtimes.ConfigExit("missing_session", "runtime session factory returned nil")
		}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.currentCancel = cancel
	s.currentAttempt++
	attemptID := s.currentAttempt
	s.pending = false
	s.backoffUntil = nil
	s.phase = s.ops.startingPhase
	_ = s.writeStatusLocked(spec, statusWrite{
		Phase:            s.phase,
		ReconnectAttempt: s.reconnects,
		At:               s.clock.Now(),
	})
	go func() {
		exit := session.Run(ctx)
		s.handleExit(attemptID, spec, exit)
	}()
}

func (s *runtimeSlot[S]) handleExit(attemptID int64, spec S, exit runtimes.Exit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if attemptID != s.currentAttempt {
		_ = s.writeStatusLocked(spec, statusWrite{Phase: PhaseStopped, Exit: exit, At: s.clock.Now()})
		return
	}
	s.currentCancel = nil
	if !s.hasDesired {
		s.phase = PhaseStopped
		_ = s.writeStatusLocked(spec, statusWrite{Phase: PhaseStopped, Exit: exit, At: s.clock.Now()})
		return
	}
	if s.ops.generation(spec) != s.ops.generation(s.desired) ||
		s.ops.restartNonce(spec) != s.ops.restartNonce(s.desired) {
		s.reconnects = 0
		s.startLocked()
		return
	}

	switch exit.Class {
	case runtimes.ExitTransient, "":
		s.reconnects++
		delay := s.backoff.duration(s.reconnects)
		next := s.clock.Now().Add(delay)
		s.backoffUntil = &next
		s.phase = PhaseBackoff
		_ = s.writeStatusLocked(spec, statusWrite{
			Phase:            PhaseBackoff,
			Exit:             ensureExit(exit, runtimes.TransientExit("session_ended", "runtime session ended")),
			ReconnectAttempt: s.reconnects,
			NextRetryAt:      &next,
			At:               s.clock.Now(),
		})
		s.startTimerLocked(delay)
	case runtimes.ExitAuth, runtimes.ExitConfig:
		s.phase = PhaseFailed
		_ = s.writeStatusLocked(spec, statusWrite{
			Phase:            PhaseFailed,
			Exit:             exit,
			ReconnectAttempt: s.reconnects,
			At:               s.clock.Now(),
		})
	case runtimes.ExitTerminal:
		s.phase = PhaseStopped
		_ = s.writeStatusLocked(spec, statusWrite{
			Phase:            PhaseStopped,
			Exit:             exit,
			ReconnectAttempt: s.reconnects,
			At:               s.clock.Now(),
		})
	default:
		s.phase = PhaseFailed
		_ = s.writeStatusLocked(spec, statusWrite{
			Phase: PhaseFailed,
			Exit:  runtimes.ConfigExit("invalid_exit_class", "runtime session returned invalid exit class"),
			At:    s.clock.Now(),
		})
	}
}

func (s *runtimeSlot[S]) startTimerLocked(delay time.Duration) {
	s.stopTimerLocked()
	timer := s.clock.NewTimer(delay)
	s.timer = timer
	s.timerToken++
	token := s.timerToken
	go func() {
		<-timer.C()
		s.handleTimer(token)
	}()
}

func (s *runtimeSlot[S]) handleTimer(token int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token != s.timerToken || s.timer == nil || !s.hasDesired {
		return
	}
	s.timer = nil
	s.backoffUntil = nil
	s.startLocked()
}

func (s *runtimeSlot[S]) stopTimerLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.timerToken++
	s.backoffUntil = nil
}

func (s *runtimeSlot[S]) writeStatusLocked(spec S, write statusWrite) error {
	return s.ops.writeStatus(context.Background(), spec, write)
}

func remoteStatusWriter(store RemoteStore) func(context.Context, runtimes.RemoteSpec, statusWrite) error {
	return func(ctx context.Context, spec runtimes.RemoteSpec, write statusWrite) error {
		if store == nil {
			return nil
		}
		_, err := store.ConditionalRemoteStatusUpdate(ctx, daemonstore.RemoteStatusUpdate{
			RemoteID:             spec.RemoteID,
			ObservedGeneration:   spec.Generation,
			ObservedRestartNonce: spec.RestartNonce,
			Phase:                write.Phase,
			LastErrorCode:        write.Exit.Code,
			LastErrorMessage:     safeExitMessage(write.Exit),
			FailureClass:         string(write.Exit.Class),
			ReconnectAttempt:     write.ReconnectAttempt,
			NextRetryAt:          cloneTimePtr(write.NextRetryAt),
			ConnectedAt:          connectedAt(write.Phase, write.At),
			StoppedAt:            stoppedAt(write.Phase, write.At),
		})
		return err
	}
}

func agentConnectionStatusWriter(store AgentConnectionStore) func(context.Context, runtimes.AgentConnectionSpec, statusWrite) error {
	return func(ctx context.Context, spec runtimes.AgentConnectionSpec, write statusWrite) error {
		if store == nil {
			return nil
		}
		_, err := store.ConditionalAgentConnectionStatusUpdate(ctx, daemonstore.AgentConnectionStatusUpdate{
			ConnectionID:         spec.ConnectionID,
			ObservedGeneration:   spec.Generation,
			ObservedRestartNonce: spec.RestartNonce,
			Phase:                write.Phase,
			LastErrorCode:        write.Exit.Code,
			LastErrorMessage:     safeExitMessage(write.Exit),
			FailureClass:         string(write.Exit.Class),
			ReconnectAttempt:     write.ReconnectAttempt,
			NextRetryAt:          cloneTimePtr(write.NextRetryAt),
			StartedAt:            startedAt(write.Phase, write.At),
			ConnectedAt:          connectedAt(write.Phase, write.At),
			StoppedAt:            stoppedAt(write.Phase, write.At),
			DetailsJSON:          "{}",
		})
		return err
	}
}

func ensureExit(got runtimes.Exit, fallback runtimes.Exit) runtimes.Exit {
	if got.Class == "" {
		return fallback
	}
	return got
}

func safeExitMessage(exit runtimes.Exit) string {
	if exit.Class == runtimes.ExitAuth && exit.Message != "" {
		return "authentication failed"
	}
	return exit.Message
}

func connectedAt(phase string, at time.Time) *time.Time {
	if phase != string(runtimes.PhaseConnected) && phase != string(runtimes.PhaseRunning) {
		return nil
	}
	return cloneTimePtr(&at)
}

func startedAt(phase string, at time.Time) *time.Time {
	if phase != string(runtimes.PhaseStarting) && phase != string(runtimes.PhaseRunning) {
		return nil
	}
	return cloneTimePtr(&at)
}

func stoppedAt(phase string, at time.Time) *time.Time {
	if phase != PhaseStopped {
		return nil
	}
	return cloneTimePtr(&at)
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	out := *t
	return &out
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }
func (realClock) NewTimer(d time.Duration) Timer {
	return realTimer{Timer: time.NewTimer(d)}
}

type realTimer struct {
	*time.Timer
}

func (t realTimer) C() <-chan time.Time { return t.Timer.C }

type sessionFunc func(context.Context) runtimes.Exit

func (f sessionFunc) Run(ctx context.Context) runtimes.Exit {
	return f(ctx)
}
