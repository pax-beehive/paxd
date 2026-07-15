package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

type ACPSlotPhase string

const (
	ACPSlotPhaseStarting ACPSlotPhase = "starting"
	ACPSlotPhaseReady    ACPSlotPhase = "ready"
	ACPSlotPhaseDraining ACPSlotPhase = "draining"
	ACPSlotPhaseStopped  ACPSlotPhase = "stopped"
	ACPSlotPhaseFailed   ACPSlotPhase = "failed"
)

type ACPSlotSpec struct {
	ConnectionID       string
	CloudAgentID       string
	TransportQueueID   string
	SlotID             string
	Ordinal            int
	Command            []string
	WorkingDir         string
	Env                map[string]string
	ProcessEpoch       string
	PaxdVersion        string
	CommandFingerprint string
	Generation         int64
	RestartNonce       int64
}

type ACPSlotEventType string

const (
	ACPSlotEventReady    ACPSlotEventType = "ready"
	ACPSlotEventFrame    ACPSlotEventType = "frame"
	ACPSlotEventTerminal ACPSlotEventType = "terminal"
)

type ACPSlotEvent struct {
	Type         ACPSlotEventType
	ConnectionID string
	SlotID       string
	ProcessEpoch string
	Ordinal      int
	Phase        ACPSlotPhase
	Payload      json.RawMessage
	Err          error
	At           time.Time
}

type ACPSlotEventSink interface {
	OnACPSlotEvent(event ACPSlotEvent)
}

type ACPSlotEventSinkFunc func(event ACPSlotEvent)

func (f ACPSlotEventSinkFunc) OnACPSlotEvent(event ACPSlotEvent) {
	if f != nil {
		f(event)
	}
}

type NoopACPSlotEventSink struct{}

func (NoopACPSlotEventSink) OnACPSlotEvent(ACPSlotEvent) {}

type ACPSlotOption func(*ACPSlot)

func WithACPSlotEventSink(sink ACPSlotEventSink) ACPSlotOption {
	return func(slot *ACPSlot) {
		if sink != nil {
			slot.sink = sink
		}
	}
}

// ACPSlot owns one ACP stdio process epoch. It is intentionally independent of
// the tunnel/reliable transport path; callers decide how emitted ACP frames are
// routed.
type ACPSlot struct {
	spec   ACPSlotSpec
	runner LocalACPProcessRunner
	sink   ACPSlotEventSink

	mu          sync.Mutex
	proc        LocalACPProcess
	phase       ACPSlotPhase
	initProfile acpClientInitProfile
	initResult  acpWorkerInitResult
	initWaiter  *acpRPCWaiter
	err         error
	done        chan struct{}
	stopOnce    sync.Once
	terminal    sync.Once
}

func NewACPSlot(spec ACPSlotSpec, runner LocalACPProcessRunner, opts ...ACPSlotOption) *ACPSlot {
	if runner == nil {
		runner = ExecLocalACPProcessRunner{}
	}
	slot := &ACPSlot{
		spec:       spec,
		runner:     runner,
		sink:       NoopACPSlotEventSink{},
		phase:      ACPSlotPhaseStarting,
		initWaiter: newACPRPCWaiter(),
		done:       make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(slot)
		}
	}
	return slot
}

func (s *ACPSlot) Start(ctx context.Context) error {
	if s.spec.ConnectionID == "" {
		return fmt.Errorf("connection id is required")
	}
	if s.spec.SlotID == "" {
		return fmt.Errorf("slot id is required")
	}
	if s.spec.ProcessEpoch == "" {
		return fmt.Errorf("process epoch is required")
	}
	proc, err := s.runner.Start(ctx, LocalACPProcessSpec{
		Command:    s.spec.Command,
		WorkingDir: s.spec.WorkingDir,
		Env:        s.spec.Env,
	})
	if err != nil {
		s.setTerminal(ACPSlotPhaseFailed, err)
		return err
	}
	s.mu.Lock()
	s.proc = proc
	s.mu.Unlock()

	go io.Copy(io.Discard, proc.Stderr())
	go s.copyStdout()
	go s.wait()
	if err := s.initialize(ctx); err != nil {
		s.Terminate(context.Background())
		return err
	}
	s.mu.Lock()
	s.phase = ACPSlotPhaseReady
	s.mu.Unlock()
	s.emit(ACPSlotEvent{Type: ACPSlotEventReady, Phase: ACPSlotPhaseReady})
	return nil
}

func (s *ACPSlot) SlotID() string {
	return s.spec.SlotID
}

func (s *ACPSlot) ProcessEpoch() string {
	return s.spec.ProcessEpoch
}

func (s *ACPSlot) Ordinal() int {
	return s.spec.Ordinal
}

func (s *ACPSlot) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase == ACPSlotPhaseReady && s.err == nil
}

func (s *ACPSlot) Done() <-chan struct{} {
	return s.done
}

func (s *ACPSlot) Send(ctx context.Context, payload []byte) error {
	s.mu.Lock()
	proc := s.proc
	ready := s.phase == ACPSlotPhaseReady || s.phase == ACPSlotPhaseStarting
	s.mu.Unlock()
	if proc == nil || !ready {
		return fmt.Errorf("acp slot %s is not accepting input", s.spec.SlotID)
	}
	return writeACPStdin(proc.Stdin(), payload)
}

func (s *ACPSlot) Terminate(ctx context.Context) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		proc := s.proc
		s.phase = ACPSlotPhaseDraining
		s.mu.Unlock()
		if proc != nil {
			_ = proc.Terminate(ctx)
		}
	})
}

func (s *ACPSlot) initialize(ctx context.Context) error {
	profile, err := buildACPClientInitProfile(s.spec.PaxdVersion)
	if err != nil {
		return fmt.Errorf("build acp initialize profile: %w", err)
	}
	request, err := buildInternalACPInitializeRequest(profile)
	if err != nil {
		return fmt.Errorf("build acp initialize request: %w", err)
	}
	initializedNotification, err := buildInternalACPInitializedNotification()
	if err != nil {
		return fmt.Errorf("build acp initialized notification: %w", err)
	}
	waitCh, cancelWait, err := s.initWaiter.Register(json.RawMessage(fmt.Sprintf("%q", internalACPInitializeID)))
	if err != nil {
		return fmt.Errorf("register acp initialize waiter: %w", err)
	}
	defer cancelWait()
	s.mu.Lock()
	s.initProfile = profile
	s.mu.Unlock()
	if err := s.Send(ctx, request); err != nil {
		return fmt.Errorf("send acp initialize: %w", err)
	}
	msg, err := waitACPRPCResult(ctx, waitCh, s.done)
	if err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	if msg.Error != nil {
		return fmt.Errorf("acp initialize rpc error %d: %s", msg.Error.Code, msg.Error.Message)
	}
	result := bytes.TrimSpace(msg.Result)
	if len(result) == 0 {
		result = []byte(`{}`)
	}
	canonicalResult, err := canonicalJSON(json.RawMessage(result))
	if err != nil {
		return fmt.Errorf("canonicalize acp initialize result: %w", err)
	}
	s.mu.Lock()
	s.initResult = acpWorkerInitResult{
		Result:        canonicalResult,
		ResultHash:    hashBytes(canonicalResult),
		InitializedAt: time.Now(),
	}
	s.mu.Unlock()
	if err := s.Send(ctx, initializedNotification); err != nil {
		return fmt.Errorf("send acp initialized notification: %w", err)
	}
	return nil
}

func (s *ACPSlot) copyStdout() {
	reader := bufio.NewReader(s.currentStdout())
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineDelimiter(line)
			if len(line) > 0 {
				msg, ok := parseACPRPCMessage(line)
				if !ok {
					s.setTerminal(ACPSlotPhaseFailed, fmt.Errorf("invalid acp stdout payload"))
					s.Terminate(context.Background())
					return
				}
				if s.initWaiter.Resolve(msg) {
					continue
				}
				s.emit(ACPSlotEvent{
					Type:    ACPSlotEventFrame,
					Phase:   s.phaseSnapshot(),
					Payload: append(json.RawMessage(nil), line...),
				})
			}
		}
		if err != nil {
			if err != io.EOF {
				s.setTerminal(ACPSlotPhaseFailed, err)
			}
			return
		}
	}
}

func (s *ACPSlot) currentStdout() io.Reader {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return bytes.NewReader(nil)
	}
	return s.proc.Stdout()
}

func (s *ACPSlot) wait() {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	var err error
	if proc != nil {
		err = proc.Wait()
	}
	if err != nil {
		s.setTerminal(ACPSlotPhaseFailed, err)
		return
	}
	s.setTerminal(ACPSlotPhaseStopped, nil)
}

func (s *ACPSlot) setTerminal(phase ACPSlotPhase, err error) {
	s.terminal.Do(func() {
		s.mu.Lock()
		s.phase = phase
		s.err = err
		s.proc = nil
		s.mu.Unlock()
		s.initWaiter.Close(err)
		close(s.done)
		s.emit(ACPSlotEvent{Type: ACPSlotEventTerminal, Phase: phase, Err: err})
	})
}

func (s *ACPSlot) phaseSnapshot() ACPSlotPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func (s *ACPSlot) emit(event ACPSlotEvent) {
	event.ConnectionID = s.spec.ConnectionID
	event.SlotID = s.spec.SlotID
	event.ProcessEpoch = s.spec.ProcessEpoch
	event.Ordinal = s.spec.Ordinal
	if event.At.IsZero() {
		event.At = time.Now()
	}
	s.sink.OnACPSlotEvent(event)
}

type acpRPCWaiter struct {
	mu      sync.Mutex
	waiters map[string]chan acpRPCWaitResult
	closed  bool
	err     error
}

type acpRPCWaitResult struct {
	msg acpRPCMessage
	err error
}

func newACPRPCWaiter() *acpRPCWaiter {
	return &acpRPCWaiter{waiters: make(map[string]chan acpRPCWaitResult)}
}

func (w *acpRPCWaiter) Wait(ctx context.Context, id json.RawMessage, done <-chan struct{}) (acpRPCMessage, error) {
	ch, cancel, err := w.Register(id)
	if err != nil {
		return acpRPCMessage{}, err
	}
	defer cancel()
	return waitACPRPCResult(ctx, ch, done)
}

func (w *acpRPCWaiter) Register(id json.RawMessage) (<-chan acpRPCWaitResult, func(), error) {
	key := rpcIDKey(id)
	if key == "" {
		return nil, nil, fmt.Errorf("rpc id is required")
	}
	ch := make(chan acpRPCWaitResult, 1)
	w.mu.Lock()
	if w.closed {
		err := w.err
		w.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("rpc waiter closed")
		}
		return nil, nil, err
	}
	w.waiters[key] = ch
	w.mu.Unlock()
	cancel := func() {
		w.mu.Lock()
		delete(w.waiters, key)
		w.mu.Unlock()
	}
	return ch, cancel, nil
}

func waitACPRPCResult(ctx context.Context, ch <-chan acpRPCWaitResult, done <-chan struct{}) (acpRPCMessage, error) {
	select {
	case result := <-ch:
		return result.msg, result.err
	case <-done:
		return acpRPCMessage{}, fmt.Errorf("rpc waiter closed")
	case <-ctx.Done():
		return acpRPCMessage{}, ctx.Err()
	}
}

func (w *acpRPCWaiter) Resolve(msg acpRPCMessage) bool {
	if msg.Method != "" || len(bytes.TrimSpace(msg.ID)) == 0 {
		return false
	}
	key := rpcIDKey(msg.ID)
	w.mu.Lock()
	ch := w.waiters[key]
	if ch != nil {
		delete(w.waiters, key)
	}
	w.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- acpRPCWaitResult{msg: msg}
	return true
}

func (w *acpRPCWaiter) Close(err error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.err = err
	waiters := w.waiters
	w.waiters = make(map[string]chan acpRPCWaitResult)
	w.mu.Unlock()
	for _, ch := range waiters {
		ch <- acpRPCWaitResult{err: err}
	}
}

func rpcIDKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}
