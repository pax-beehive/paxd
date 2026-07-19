package supervisor

import (
	"context"
	"log"
	"sort"
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

type StatusPoke interface {
	Poke(remoteID string)
}

type Snapshot struct {
	Slots []SlotSnapshot
}

type ObservedAgentRuntime struct {
	Spec  runtimes.AgentConnectionSpec
	Phase string
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
	RotateAgentTransportQueueID(ctx context.Context, connectionID string, expectedQueueID string) (string, error)
}

type ACPSlotStore interface {
	ListDesiredACPSlots(ctx context.Context) ([]runtimes.ACPSlotSpec, error)
	UpsertACPSlotStatus(ctx context.Context, update daemonstore.ACPSlotStatusUpdate) error
}

type RemoteSupervisorOptions struct {
	Store             RemoteStore
	Factory           runtimes.RemoteControlSessionFactory
	StatusPoke        StatusPoke
	Clock             Clock
	ReconcileInterval time.Duration
	Backoff           BackoffPolicy
}

type AgentConnectionSupervisorOptions struct {
	Store             AgentConnectionStore
	Factory           runtimes.AgentTunnelSessionFactory
	StatusPoke        StatusPoke
	Clock             Clock
	ReconcileInterval time.Duration
	Backoff           BackoffPolicy
	ExitHandler       RuntimeExitHandler[runtimes.AgentConnectionSpec]
	StopHandler       RuntimeStopHandler[runtimes.AgentConnectionSpec]
}

type ACPSlotSupervisorOptions struct {
	Store             ACPSlotStore
	Factory           ACPSlotSessionFactory
	Clock             Clock
	ReconcileInterval time.Duration
	Backoff           BackoffPolicy
	DrainHandler      RuntimeDrainHandler[runtimes.ACPSlotSpec]
	StopHandler       RuntimeStopHandler[runtimes.ACPSlotSpec]
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
		writeStatus:   remoteStatusWriter(opts.Store, opts.StatusPoke),
	}
	return &RemoteSupervisor{base: newBaseSupervisor(opts.Store.ListDesiredRemotes, ops, baseOptions{
		Name:              "remote",
		Clock:             opts.Clock,
		ReconcileInterval: opts.ReconcileInterval,
		Backoff:           opts.Backoff,
	})}
}

func (s *RemoteSupervisor) Start(ctx context.Context) error { return s.base.Start(ctx) }
func (s *RemoteSupervisor) Wake()                           { s.base.Wake() }
func (s *RemoteSupervisor) Snapshot() Snapshot              { return s.base.Snapshot() }
func (s *RemoteSupervisor) OnSessionEvent(event runtimes.SessionEvent) {
	if s == nil || s.base == nil || event.Kind != runtimes.SessionRemoteControl || event.RemoteID == "" {
		return
	}
	s.base.handleSessionEvent(event.RemoteID, event)
}
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
		writeStatus:   agentConnectionStatusWriter(opts.Store, opts.StatusPoke),
		handleExit:    firstAgentConnectionExitHandler(opts.ExitHandler, rotateAgentConnectionQueueOnExit(opts.Store)),
		handleStop:    opts.StopHandler,
	}
	return &AgentConnectionSupervisor{base: newBaseSupervisor(opts.Store.ListDesiredAgentConnections, ops, baseOptions{
		Name:              "agent_connection",
		Clock:             opts.Clock,
		ReconcileInterval: opts.ReconcileInterval,
		Backoff:           opts.Backoff,
	})}
}

func (s *AgentConnectionSupervisor) Start(ctx context.Context) error { return s.base.Start(ctx) }
func (s *AgentConnectionSupervisor) Wake()                           { s.base.Wake() }
func (s *AgentConnectionSupervisor) Snapshot() Snapshot              { return s.base.Snapshot() }
func (s *AgentConnectionSupervisor) OnSessionEvent(event runtimes.SessionEvent) {
	if s == nil || s.base == nil || event.Kind != runtimes.SessionAgentTunnel || event.ConnectionID == "" {
		return
	}
	s.base.handleSessionEvent(event.ConnectionID, event)
}
func (s *AgentConnectionSupervisor) Reconcile(ctx context.Context) error {
	return s.base.Reconcile(ctx)
}
func (s *AgentConnectionSupervisor) ObservedAgentRuntimes() []ObservedAgentRuntime {
	if s == nil || s.base == nil {
		return nil
	}
	s.base.mu.Lock()
	slots := make([]*runtimeSlot[runtimes.AgentConnectionSpec], 0, len(s.base.slots))
	for _, slot := range s.base.slots {
		slots = append(slots, slot)
	}
	s.base.mu.Unlock()

	out := make([]ObservedAgentRuntime, 0, len(slots))
	for _, slot := range slots {
		spec, phase, ok := slot.observedDesired()
		if !ok {
			continue
		}
		out = append(out, ObservedAgentRuntime{
			Spec:  cloneAgentConnectionSpec(spec),
			Phase: phase,
		})
	}
	return out
}

type ACPSlotSessionFactory interface {
	NewACPSlotSession(spec runtimes.ACPSlotSpec) runtimes.Session
}

type ACPSlotSupervisor struct {
	base *baseSupervisor[runtimes.ACPSlotSpec]
}

func NewACPSlotSupervisor(opts ACPSlotSupervisorOptions) *ACPSlotSupervisor {
	ops := slotOps[runtimes.ACPSlotSpec]{
		id:           func(spec runtimes.ACPSlotSpec) string { return spec.SlotID },
		generation:   func(spec runtimes.ACPSlotSpec) int64 { return spec.Generation },
		restartNonce: func(spec runtimes.ACPSlotSpec) int64 { return spec.RestartNonce },
		desiredChanged: func(current runtimes.ACPSlotSpec, next runtimes.ACPSlotSpec) bool {
			return current.CommandFingerprint != next.CommandFingerprint || current.RestartNonce != next.RestartNonce
		},
		stopBefore: func(current runtimes.ACPSlotSpec, next runtimes.ACPSlotSpec) bool {
			return current.Ordinal > next.Ordinal
		},
		startingPhase: string(runtimes.PhaseStarting),
		drainingPhase: string(runtimes.ACPSlotPhaseDraining),
		prepareStart: func(spec runtimes.ACPSlotSpec) runtimes.ACPSlotSpec {
			epoch, err := runtimes.NewACPProcessEpoch()
			if err != nil {
				log.Printf("[paxd] acp_slot slot id=%s process epoch generation failed: %v", spec.SlotID, err)
				return spec
			}
			spec.ProcessEpoch = epoch
			return spec
		},
		newSession:  opts.Factory.NewACPSlotSession,
		writeStatus: acpSlotStatusWriter(opts.Store),
		beginDrain:  opts.DrainHandler,
		handleStop:  opts.StopHandler,
	}
	return &ACPSlotSupervisor{base: newBaseSupervisor(opts.Store.ListDesiredACPSlots, ops, baseOptions{
		Name:              "acp_slot",
		Clock:             opts.Clock,
		ReconcileInterval: opts.ReconcileInterval,
		Backoff:           opts.Backoff,
	})}
}

func (s *ACPSlotSupervisor) Start(ctx context.Context) error { return s.base.Start(ctx) }
func (s *ACPSlotSupervisor) Wake()                           { s.base.Wake() }
func (s *ACPSlotSupervisor) Snapshot() Snapshot              { return s.base.Snapshot() }
func (s *ACPSlotSupervisor) Reconcile(ctx context.Context) error {
	return s.base.Reconcile(ctx)
}

func firstAgentConnectionExitHandler(
	handlers ...RuntimeExitHandler[runtimes.AgentConnectionSpec],
) RuntimeExitHandler[runtimes.AgentConnectionSpec] {
	return func(ctx context.Context, spec runtimes.AgentConnectionSpec, exit runtimes.Exit) (runtimes.AgentConnectionSpec, bool, error) {
		for _, handler := range handlers {
			if handler == nil {
				continue
			}
			next, handled, err := handler(ctx, spec, exit)
			if err != nil || handled {
				return next, handled, err
			}
		}
		return spec, false, nil
	}
}

func rotateAgentConnectionQueueOnExit(store AgentConnectionStore) RuntimeExitHandler[runtimes.AgentConnectionSpec] {
	return func(ctx context.Context, spec runtimes.AgentConnectionSpec, exit runtimes.Exit) (runtimes.AgentConnectionSpec, bool, error) {
		if exit.Code != "reconcile_rotate" {
			return spec, false, nil
		}
		queueID, err := store.RotateAgentTransportQueueID(ctx, spec.ConnectionID, spec.TransportQueueID)
		if err != nil {
			return spec, false, err
		}
		next := cloneAgentConnectionSpec(spec)
		next.TransportQueueID = queueID
		log.Printf(
			"[paxd] agent_connection slot id=%s rotated transport queue old_queue_id=%s new_queue_id=%s",
			spec.ConnectionID,
			spec.TransportQueueID,
			queueID,
		)
		return next, true, nil
	}
}

type baseOptions struct {
	Name              string
	Clock             Clock
	ReconcileInterval time.Duration
	Backoff           BackoffPolicy
}

type RuntimeExitHandler[S any] func(ctx context.Context, spec S, exit runtimes.Exit) (S, bool, error)
type RuntimeDrainHandler[S any] func(ctx context.Context, spec S) <-chan struct{}
type RuntimeStopHandler[S any] func(ctx context.Context, spec S)

type baseSupervisor[S any] struct {
	mu          sync.Mutex
	reconcileMu sync.Mutex
	name        string
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
	name := opts.Name
	if name == "" {
		name = "runtime"
	}
	return &baseSupervisor[S]{
		name:        name,
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
	log.Printf("[paxd] %s supervisor starting interval=%s", s.name, s.interval)
	if err := s.Reconcile(ctx); err != nil {
		log.Printf("[paxd] %s supervisor initial reconcile failed: %v", s.name, err)
		return err
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[paxd] %s supervisor stopping: %v", s.name, ctx.Err())
			s.stopAll(ctx)
			return ctx.Err()
		case <-s.wake:
			log.Printf("[paxd] %s supervisor wake received", s.name)
			if err := s.Reconcile(ctx); err != nil {
				log.Printf("[paxd] %s supervisor reconcile after wake failed: %v", s.name, err)
				return err
			}
		case <-ticker.C:
			log.Printf("[paxd] %s supervisor periodic reconcile", s.name)
			if err := s.Reconcile(ctx); err != nil {
				log.Printf("[paxd] %s supervisor periodic reconcile failed: %v", s.name, err)
				return err
			}
		}
	}
}

func (s *baseSupervisor[S]) Wake() {
	select {
	case s.wake <- struct{}{}:
		log.Printf("[paxd] %s supervisor wake queued", s.name)
	default:
		log.Printf("[paxd] %s supervisor wake already pending", s.name)
	}
}

func (s *baseSupervisor[S]) Reconcile(ctx context.Context) error {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	desired, err := s.listDesired(ctx)
	if err != nil {
		log.Printf("[paxd] %s supervisor list desired failed: %v", s.name, err)
		return err
	}
	seen := make(map[string]struct{}, len(desired))
	valid := 0
	created := 0
	for _, spec := range desired {
		id := s.ops.id(spec)
		if id == "" {
			log.Printf("[paxd] %s supervisor skipped desired item with empty id", s.name)
			continue
		}
		valid++
		seen[id] = struct{}{}
		slot, isNew := s.slot(id)
		if isNew {
			created++
		}
		slot.ApplyDesired(spec)
	}
	s.mu.Lock()
	toStop := make([]*runtimeSlot[S], 0)
	for id, slot := range s.slots {
		if _, ok := seen[id]; !ok {
			_, _, hasDesired := slot.observedDesired()
			if hasDesired {
				toStop = append(toStop, slot)
			}
		}
	}
	slotCount := len(s.slots)
	s.mu.Unlock()
	sort.Slice(toStop, func(i, j int) bool {
		left, _, _ := toStop[i].observedDesired()
		right, _, _ := toStop[j].observedDesired()
		if s.ops.stopBefore != nil {
			return s.ops.stopBefore(left, right)
		}
		return s.ops.id(left) < s.ops.id(right)
	})
	for _, slot := range toStop {
		slot.Stop("not_desired")
	}
	log.Printf(
		"[paxd] %s supervisor reconcile done desired=%d valid=%d slots=%d created=%d stopped=%d",
		s.name,
		len(desired),
		valid,
		slotCount,
		created,
		len(toStop),
	)
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

func (s *baseSupervisor[S]) slot(id string) (*runtimeSlot[S], bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot, ok := s.slots[id]; ok {
		return slot, false
	}
	log.Printf("[paxd] %s supervisor creating slot id=%s", s.name, id)
	slot := newRuntimeSlot(s.name, id, s.ops, s.clock, s.backoff)
	s.slots[id] = slot
	return slot, true
}

func (s *baseSupervisor[S]) handleSessionEvent(id string, event runtimes.SessionEvent) {
	s.mu.Lock()
	slot := s.slots[id]
	s.mu.Unlock()
	if slot != nil {
		slot.handleSessionEvent(event)
	}
}

func (s *baseSupervisor[S]) stopAll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, slot := range s.slots {
		slot.Stop(ctx.Err().Error())
	}
}

type slotOps[S any] struct {
	id             func(S) string
	generation     func(S) int64
	restartNonce   func(S) int64
	desiredChanged func(S, S) bool
	stopBefore     func(S, S) bool
	startingPhase  string
	drainingPhase  string
	prepareStart   func(S) S
	newSession     func(S) runtimes.Session
	writeStatus    func(context.Context, S, statusWrite) error
	handleExit     RuntimeExitHandler[S]
	beginDrain     RuntimeDrainHandler[S]
	handleStop     RuntimeStopHandler[S]
}

type statusWrite struct {
	Phase            string
	PID              *int
	Exit             runtimes.Exit
	ReconnectAttempt int
	NextRetryAt      *time.Time
	At               time.Time
}

type runtimeSlot[S any] struct {
	mu             sync.Mutex
	supervisorName string
	id             string
	ops            slotOps[S]
	clock          Clock
	backoff        BackoffPolicy
	desired        S
	hasDesired     bool
	currentSpec    S
	hasCurrentSpec bool
	currentCancel  context.CancelFunc
	currentAttempt int64
	reconnects     int
	timer          Timer
	timerToken     int64
	backoffUntil   *time.Time
	phase          string
	pending        bool
	draining       bool
	drainToken     int64
}

func newRuntimeSlot[S any](supervisorName string, id string, ops slotOps[S], clock Clock, backoff BackoffPolicy) *runtimeSlot[S] {
	return &runtimeSlot[S]{
		supervisorName: supervisorName,
		id:             id,
		ops:            ops,
		clock:          clock,
		backoff:        backoff.withDefaults(),
		phase:          PhaseStopped,
	}
}

func (s *runtimeSlot[S]) ApplyDesired(spec S) {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := !s.hasDesired || s.desiredChanged(s.desired, spec)
	oldGeneration := int64(0)
	oldRestartNonce := int64(0)
	if s.hasDesired {
		oldGeneration = s.ops.generation(s.desired)
		oldRestartNonce = s.ops.restartNonce(s.desired)
	}
	s.desired = spec
	s.hasDesired = true
	if !changed {
		if s.currentCancel != nil || s.timer != nil || s.phase == PhaseFailed || s.phase == PhaseStopped {
			return
		}
	}
	if s.timer != nil {
		log.Printf("[paxd] %s slot id=%s canceling backoff timer for updated desired", s.supervisorName, s.id)
		s.stopTimerLocked()
	}
	if s.currentCancel != nil {
		currentSpec := s.currentOrDesiredLocked()
		log.Printf(
			"[paxd] %s slot id=%s interrupting running session old_generation=%d new_generation=%d old_restart_nonce=%d new_restart_nonce=%d",
			s.supervisorName,
			s.id,
			oldGeneration,
			s.ops.generation(spec),
			oldRestartNonce,
			s.ops.restartNonce(spec),
		)
		s.pending = true
		if s.draining {
			s.draining = false
			s.drainToken++
			if s.ops.handleStop != nil {
				s.ops.handleStop(context.Background(), currentSpec)
			}
		}
		s.phase = string(runtimes.PhaseStopping)
		s.writeStatusLoggedLocked(currentSpec, statusWrite{Phase: s.phase, At: s.clock.Now()})
		s.currentCancel()
		return
	}
	s.startLocked()
}

func (s *runtimeSlot[S]) Stop(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("[paxd] %s slot id=%s stop requested reason=%s", s.supervisorName, s.id, reason)
	if !s.hasDesired && (!s.draining || reason == "not_desired") {
		return
	}
	stopSpec := s.currentOrDesiredLocked()
	s.hasDesired = false
	s.pending = false
	if s.draining && reason != "not_desired" {
		s.draining = false
		s.drainToken++
	}
	if s.timer != nil {
		s.stopTimerLocked()
	}
	if s.currentCancel != nil {
		if reason == "not_desired" && s.ops.beginDrain != nil {
			s.draining = true
			s.drainToken++
			token := s.drainToken
			attemptID := s.currentAttempt
			s.phase = s.ops.drainingPhase
			if s.phase == "" {
				s.phase = string(runtimes.PhaseStopping)
			}
			s.writeStatusLoggedLocked(stopSpec, statusWrite{Phase: s.phase, At: s.clock.Now()})
			drained := s.ops.beginDrain(context.Background(), stopSpec)
			if drained != nil {
				select {
				case <-drained:
					s.finishStopLocked(reason, stopSpec)
				default:
					go s.waitForDrain(token, attemptID, reason, stopSpec, drained)
				}
				return
			}
		}
		s.finishStopLocked(reason, stopSpec)
		return
	}
	if s.ops.handleStop != nil {
		s.ops.handleStop(context.Background(), stopSpec)
	}
	if s.phase != PhaseStopped {
		s.phase = PhaseStopped
		s.writeStatusLoggedLocked(stopSpec, statusWrite{
			Phase: PhaseStopped,
			Exit:  runtimes.TerminalExit("stopped", reason),
			At:    s.clock.Now(),
		})
	}
}

func (s *runtimeSlot[S]) waitForDrain(token int64, attemptID int64, reason string, spec S, drained <-chan struct{}) {
	<-drained
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining || token != s.drainToken || attemptID != s.currentAttempt {
		return
	}
	log.Printf("[paxd] %s slot id=%s drain complete", s.supervisorName, s.id)
	s.finishStopLocked(reason, spec)
}

func (s *runtimeSlot[S]) finishStopLocked(reason string, spec S) {
	s.draining = false
	s.drainToken++
	if s.ops.handleStop != nil {
		s.ops.handleStop(context.Background(), spec)
	}
	if s.currentCancel != nil {
		s.phase = string(runtimes.PhaseStopping)
		s.currentCancel()
		return
	}
	if s.phase != PhaseStopped {
		s.phase = PhaseStopped
		s.writeStatusLoggedLocked(spec, statusWrite{
			Phase: PhaseStopped,
			Exit:  runtimes.TerminalExit("stopped", reason),
			At:    s.clock.Now(),
		})
	}
}

func (s *runtimeSlot[S]) currentOrDesiredLocked() S {
	if s.hasCurrentSpec {
		return s.currentSpec
	}
	return s.desired
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

func (s *runtimeSlot[S]) observedDesired() (S, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desired, s.phase, s.hasDesired
}

func (s *runtimeSlot[S]) startLocked() {
	if !s.hasDesired || s.currentCancel != nil {
		return
	}
	spec := s.desired
	if s.ops.prepareStart != nil {
		spec = s.ops.prepareStart(spec)
	}
	s.currentSpec = spec
	s.hasCurrentSpec = true
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
	s.draining = false
	s.backoffUntil = nil
	s.phase = s.ops.startingPhase
	log.Printf(
		"[paxd] %s slot id=%s starting session generation=%d restart_nonce=%d reconnect_attempt=%d phase=%s",
		s.supervisorName,
		s.id,
		s.ops.generation(spec),
		s.ops.restartNonce(spec),
		s.reconnects,
		s.phase,
	)
	s.writeStatusLoggedLocked(spec, statusWrite{
		Phase:            s.phase,
		ReconnectAttempt: s.reconnects,
		At:               s.clock.Now(),
	})
	go func() {
		exit := session.Run(ctx)
		s.handleExit(attemptID, spec, exit)
	}()
}

func (s *runtimeSlot[S]) handleSessionEvent(event runtimes.SessionEvent) {
	if !event.Phase.Valid() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasDesired || !s.hasCurrentSpec || s.currentCancel == nil || s.pending || s.draining {
		return
	}
	spec := s.currentSpec
	if event.Generation != s.ops.generation(spec) || event.RestartNonce != s.ops.restartNonce(spec) {
		return
	}
	at := event.At
	if at.IsZero() {
		at = s.clock.Now()
	}
	s.phase = string(event.Phase)
	s.writeStatusLoggedLocked(spec, statusWrite{
		Phase:            s.phase,
		PID:              event.PID,
		ReconnectAttempt: s.reconnects,
		At:               at,
	})
}

func (s *runtimeSlot[S]) handleExit(attemptID int64, spec S, exit runtimes.Exit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf(
		"[paxd] %s slot id=%s session exited attempt=%d class=%s code=%s message=%q",
		s.supervisorName,
		s.id,
		attemptID,
		exit.Class,
		exit.Code,
		exit.Message,
	)
	if attemptID != s.currentAttempt {
		log.Printf("[paxd] %s slot id=%s ignoring stale session exit attempt=%d current_attempt=%d", s.supervisorName, s.id, attemptID, s.currentAttempt)
		return
	}
	s.currentCancel = nil
	s.hasCurrentSpec = false
	if !s.hasDesired {
		if s.draining {
			s.draining = false
			s.drainToken++
			if s.ops.handleStop != nil {
				s.ops.handleStop(context.Background(), spec)
			}
		}
		s.phase = PhaseStopped
		s.writeStatusLoggedLocked(spec, statusWrite{Phase: PhaseStopped, Exit: exit, At: s.clock.Now()})
		return
	}
	if s.pending || s.desiredChanged(spec, s.desired) {
		log.Printf("[paxd] %s slot id=%s restarting for newer desired", s.supervisorName, s.id)
		s.reconnects = 0
		s.startLocked()
		return
	}
	if s.ops.handleExit != nil {
		next, handled, err := s.ops.handleExit(context.Background(), spec, exit)
		if err != nil {
			log.Printf("[paxd] %s slot id=%s exit handler failed code=%s err=%v", s.supervisorName, s.id, exit.Code, err)
			exit = runtimes.TransientExit("exit_handler_failed", err.Error())
		} else if handled {
			if s.ops.id(next) != s.id {
				log.Printf("[paxd] %s slot id=%s exit handler returned different id", s.supervisorName, s.id)
				exit = runtimes.TransientExit("exit_handler_failed", "exit handler returned different id")
			} else {
				s.desired = next
				s.reconnects = 0
				s.backoffUntil = nil
				log.Printf("[paxd] %s slot id=%s restarting after handled exit code=%s", s.supervisorName, s.id, exit.Code)
				s.startLocked()
				return
			}
		}
	}

	switch exit.Class {
	case runtimes.ExitTransient, "":
		if exit.ResetBackoff {
			log.Printf("[paxd] %s slot id=%s resetting backoff after successful connection", s.supervisorName, s.id)
			s.reconnects = 0
		}
		s.reconnects++
		delay := s.backoff.duration(s.reconnects)
		next := s.clock.Now().Add(delay)
		s.backoffUntil = &next
		s.phase = PhaseBackoff
		log.Printf("[paxd] %s slot id=%s entering backoff attempt=%d delay=%s next_retry=%s", s.supervisorName, s.id, s.reconnects, delay, next.Format(time.RFC3339))
		s.writeStatusLoggedLocked(spec, statusWrite{
			Phase:            PhaseBackoff,
			Exit:             ensureExit(exit, runtimes.TransientExit("session_ended", "runtime session ended")),
			ReconnectAttempt: s.reconnects,
			NextRetryAt:      &next,
			At:               s.clock.Now(),
		})
		s.startTimerLocked(delay)
	case runtimes.ExitAuth, runtimes.ExitConfig:
		s.phase = PhaseFailed
		log.Printf("[paxd] %s slot id=%s failed terminally class=%s code=%s message=%q", s.supervisorName, s.id, exit.Class, exit.Code, exit.Message)
		s.writeStatusLoggedLocked(spec, statusWrite{
			Phase:            PhaseFailed,
			Exit:             exit,
			ReconnectAttempt: s.reconnects,
			At:               s.clock.Now(),
		})
	case runtimes.ExitTerminal:
		s.phase = PhaseStopped
		log.Printf("[paxd] %s slot id=%s stopped class=%s code=%s message=%q", s.supervisorName, s.id, exit.Class, exit.Code, exit.Message)
		s.writeStatusLoggedLocked(spec, statusWrite{
			Phase:            PhaseStopped,
			Exit:             exit,
			ReconnectAttempt: s.reconnects,
			At:               s.clock.Now(),
		})
	default:
		s.phase = PhaseFailed
		log.Printf("[paxd] %s slot id=%s returned invalid exit class=%s", s.supervisorName, s.id, exit.Class)
		s.writeStatusLoggedLocked(spec, statusWrite{
			Phase: PhaseFailed,
			Exit:  runtimes.ConfigExit("invalid_exit_class", "runtime session returned invalid exit class"),
			At:    s.clock.Now(),
		})
	}
}

func (s *runtimeSlot[S]) desiredChanged(current S, next S) bool {
	if s.ops.desiredChanged != nil {
		return s.ops.desiredChanged(current, next)
	}
	return s.ops.generation(current) != s.ops.generation(next) ||
		s.ops.restartNonce(current) != s.ops.restartNonce(next)
}

func (s *runtimeSlot[S]) startTimerLocked(delay time.Duration) {
	s.stopTimerLocked()
	timer := s.clock.NewTimer(delay)
	s.timer = timer
	s.timerToken++
	token := s.timerToken
	go func() {
		<-timer.C()
		log.Printf("[paxd] %s slot id=%s backoff timer fired token=%d", s.supervisorName, s.id, token)
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
	log.Printf("[paxd] %s slot id=%s restarting after backoff", s.supervisorName, s.id)
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

func (s *runtimeSlot[S]) writeStatusLoggedLocked(spec S, write statusWrite) {
	if err := s.writeStatusLocked(spec, write); err != nil {
		log.Printf("[paxd] %s slot id=%s write status failed phase=%s: %v", s.supervisorName, s.id, write.Phase, err)
	}
}

func remoteStatusWriter(store RemoteStore, poke StatusPoke) func(context.Context, runtimes.RemoteSpec, statusWrite) error {
	return func(ctx context.Context, spec runtimes.RemoteSpec, write statusWrite) error {
		if store == nil {
			return nil
		}
		ok, err := store.ConditionalRemoteStatusUpdate(ctx, daemonstore.RemoteStatusUpdate{
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
		if err == nil && ok && poke != nil && spec.RemoteID != "" {
			poke.Poke(spec.RemoteID)
		}
		return err
	}
}

func agentConnectionStatusWriter(store AgentConnectionStore, poke StatusPoke) func(context.Context, runtimes.AgentConnectionSpec, statusWrite) error {
	return func(ctx context.Context, spec runtimes.AgentConnectionSpec, write statusWrite) error {
		if store == nil {
			return nil
		}
		ok, err := store.ConditionalAgentConnectionStatusUpdate(ctx, daemonstore.AgentConnectionStatusUpdate{
			ConnectionID:         spec.ConnectionID,
			ObservedGeneration:   spec.Generation,
			ObservedRestartNonce: spec.RestartNonce,
			Phase:                write.Phase,
			PID:                  write.PID,
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
		if err == nil && ok && poke != nil && spec.RemoteID != "" {
			poke.Poke(spec.RemoteID)
		}
		return err
	}
}

func acpSlotStatusWriter(store ACPSlotStore) func(context.Context, runtimes.ACPSlotSpec, statusWrite) error {
	return func(ctx context.Context, spec runtimes.ACPSlotSpec, write statusWrite) error {
		if store == nil {
			return nil
		}
		return store.UpsertACPSlotStatus(ctx, daemonstore.ACPSlotStatusUpdate{
			SlotID:           spec.SlotID,
			ConnectionID:     spec.ConnectionID,
			Ordinal:          spec.Ordinal,
			ProcessEpoch:     spec.ProcessEpoch,
			Phase:            write.Phase,
			LastErrorCode:    write.Exit.Code,
			LastErrorMessage: safeExitMessage(write.Exit),
			FailureClass:     string(write.Exit.Class),
			StartedAt:        startedAt(write.Phase, write.At),
			ReadyAt:          connectedAt(write.Phase, write.At),
			StoppedAt:        stoppedAt(write.Phase, write.At),
		})
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

func cloneAgentConnectionSpec(spec runtimes.AgentConnectionSpec) runtimes.AgentConnectionSpec {
	spec.Command = append([]string(nil), spec.Command...)
	if spec.Env != nil {
		env := make(map[string]string, len(spec.Env))
		for key, value := range spec.Env {
			env[key] = value
		}
		spec.Env = env
	}
	return spec
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
