package runtime

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type SessionRuntimeStatus string

const (
	SessionRuntimeRunning         SessionRuntimeStatus = "running"
	SessionRuntimeWaitingApproval SessionRuntimeStatus = "waiting_approval"
)

type SessionActiveTurn struct {
	NativeSessionID   string               `json:"native_session_id"`
	TurnInstanceID    string               `json:"turn_instance_id"`
	PromptRequestID   json.RawMessage      `json:"prompt_request_id"`
	RuntimeStatus     SessionRuntimeStatus `json:"runtime_status"`
	PendingApprovalID json.RawMessage      `json:"pending_approval_id,omitempty"`
	SlotID            string               `json:"slot_id,omitempty"`
	ProcessEpoch      string               `json:"process_epoch,omitempty"`
}

type ActiveTurnsSnapshot struct {
	Revision    uint64              `json:"revision"`
	ActiveTurns []SessionActiveTurn `json:"active_turns"`
}

type SessionRuntimeResetStatus string

const (
	SessionRuntimeResetSuppressed        SessionRuntimeResetStatus = "suppressed"
	SessionRuntimeResetAlreadySuppressed SessionRuntimeResetStatus = "already_suppressed"
	SessionRuntimeResetAlreadyTerminal   SessionRuntimeResetStatus = "already_terminal"
	SessionRuntimeResetConflict          SessionRuntimeResetStatus = "conflict"
	SessionRuntimeResetNotFound          SessionRuntimeResetStatus = "not_found"
)

type SessionRuntimeResetResult struct {
	Status   SessionRuntimeResetStatus `json:"status"`
	Revision uint64                    `json:"revision"`
}

type SessionRuntimeTurnProjector struct {
	mu sync.Mutex

	active       map[string]SessionActiveTurn
	suppressed   map[string]struct{}
	terminal     map[string]struct{}
	terminalFIFO []string
	revision     uint64
	subscribers  map[uint64]chan struct{}
	nextSubID    uint64
}

const sessionRuntimeTerminalLimit = 1024

func NewSessionRuntimeProjector() *SessionRuntimeTurnProjector {
	return &SessionRuntimeTurnProjector{
		active:      make(map[string]SessionActiveTurn),
		suppressed:  make(map[string]struct{}),
		terminal:    make(map[string]struct{}),
		subscribers: make(map[uint64]chan struct{}),
	}
}

func (p *SessionRuntimeTurnProjector) StartTurn(
	nativeSessionID string,
	promptRequestID json.RawMessage,
	slotID string,
	processEpoch string,
) (SessionActiveTurn, error) {
	if p == nil {
		return SessionActiveTurn{}, fmt.Errorf("session runtime projector is required")
	}
	nativeSessionID = strings.TrimSpace(nativeSessionID)
	if nativeSessionID == "" {
		return SessionActiveTurn{}, fmt.Errorf("native session id is required")
	}
	requestID := bytes.TrimSpace(promptRequestID)
	if len(requestID) == 0 || !json.Valid(requestID) || bytes.Equal(requestID, []byte("null")) {
		return SessionActiveTurn{}, fmt.Errorf("prompt request id is required")
	}
	turnInstanceID, err := newSessionRuntimeTurnInstanceID()
	if err != nil {
		return SessionActiveTurn{}, err
	}
	turn := SessionActiveTurn{
		NativeSessionID: nativeSessionID,
		TurnInstanceID:  turnInstanceID,
		PromptRequestID: append(json.RawMessage(nil), requestID...),
		RuntimeStatus:   SessionRuntimeRunning,
		SlotID:          strings.TrimSpace(slotID),
		ProcessEpoch:    strings.TrimSpace(processEpoch),
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.active[nativeSessionID]; exists {
		return SessionActiveTurn{}, fmt.Errorf("native session already has an active turn")
	}
	p.active[nativeSessionID] = turn
	p.markChangedLocked()
	return cloneSessionActiveTurn(turn), nil
}

func (p *SessionRuntimeTurnProjector) WaitForApproval(
	nativeSessionID string,
	turnInstanceID string,
	pendingApprovalID json.RawMessage,
) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	turn, ok := p.active[nativeSessionID]
	if !ok || turn.TurnInstanceID != turnInstanceID {
		return false
	}
	turn.RuntimeStatus = SessionRuntimeWaitingApproval
	turn.PendingApprovalID = append(json.RawMessage(nil), bytes.TrimSpace(pendingApprovalID)...)
	p.active[nativeSessionID] = turn
	if !p.isSuppressedLocked(turn) {
		p.markChangedLocked()
	}
	return true
}

func (p *SessionRuntimeTurnProjector) ResolveApproval(nativeSessionID string, turnInstanceID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	turn, ok := p.active[nativeSessionID]
	if !ok || turn.TurnInstanceID != turnInstanceID {
		return false
	}
	changed := turn.RuntimeStatus != SessionRuntimeRunning || len(turn.PendingApprovalID) > 0
	turn.RuntimeStatus = SessionRuntimeRunning
	turn.PendingApprovalID = nil
	p.active[nativeSessionID] = turn
	if changed && !p.isSuppressedLocked(turn) {
		p.markChangedLocked()
	}
	return true
}

func (p *SessionRuntimeTurnProjector) CompleteTurn(nativeSessionID string, turnInstanceID string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	turn, ok := p.active[nativeSessionID]
	if !ok || turn.TurnInstanceID != turnInstanceID {
		return false
	}
	hidden := p.isSuppressedLocked(turn)
	delete(p.active, nativeSessionID)
	delete(p.suppressed, runtimeTurnKey(nativeSessionID, turnInstanceID))
	p.rememberTerminalLocked(nativeSessionID, turnInstanceID)
	if !hidden {
		p.markChangedLocked()
	}
	return true
}

func (p *SessionRuntimeTurnProjector) CompleteSlot(slotID string, processEpoch string) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	removed := 0
	visibleChanged := false
	for nativeSessionID, turn := range p.active {
		if turn.SlotID != slotID || processEpoch != "" && turn.ProcessEpoch != processEpoch {
			continue
		}
		if !p.isSuppressedLocked(turn) {
			visibleChanged = true
		}
		delete(p.active, nativeSessionID)
		delete(p.suppressed, runtimeTurnKey(nativeSessionID, turn.TurnInstanceID))
		p.rememberTerminalLocked(nativeSessionID, turn.TurnInstanceID)
		removed++
	}
	if visibleChanged {
		p.markChangedLocked()
	}
	return removed
}

func (p *SessionRuntimeTurnProjector) ActiveTurn(nativeSessionID string) (SessionActiveTurn, bool) {
	if p == nil {
		return SessionActiveTurn{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	turn, ok := p.active[nativeSessionID]
	return cloneSessionActiveTurn(turn), ok
}

func (p *SessionRuntimeTurnProjector) Snapshot() ActiveTurnsSnapshot {
	if p == nil {
		return ActiveTurnsSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

func (p *SessionRuntimeTurnProjector) SnapshotAndSubscribe() (
	ActiveTurnsSnapshot,
	<-chan struct{},
	func(),
) {
	if p == nil {
		closed := make(chan struct{})
		close(closed)
		return ActiveTurnsSnapshot{}, closed, func() {}
	}
	p.mu.Lock()
	p.nextSubID++
	subscriberID := p.nextSubID
	changes := make(chan struct{}, 1)
	p.subscribers[subscriberID] = changes
	snapshot := p.snapshotLocked()
	p.mu.Unlock()
	var once sync.Once
	return snapshot, changes, func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.subscribers, subscriberID)
			p.mu.Unlock()
		})
	}
}

func (p *SessionRuntimeTurnProjector) Reset(nativeSessionID string, expectedTurnInstanceID string) SessionRuntimeResetResult {
	if p == nil {
		return SessionRuntimeResetResult{Status: SessionRuntimeResetNotFound}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Every reset attempt requests a complete self-healing snapshot. A stale
	// manager projection can otherwise remain visible when the compared turn
	// already completed or was replaced before the command arrived.
	p.markChangedLocked()
	key := runtimeTurnKey(nativeSessionID, expectedTurnInstanceID)
	if _, terminal := p.terminal[key]; terminal {
		return SessionRuntimeResetResult{Status: SessionRuntimeResetAlreadyTerminal, Revision: p.revision}
	}
	turn, ok := p.active[nativeSessionID]
	if !ok {
		return SessionRuntimeResetResult{Status: SessionRuntimeResetNotFound, Revision: p.revision}
	}
	if turn.TurnInstanceID != expectedTurnInstanceID {
		return SessionRuntimeResetResult{Status: SessionRuntimeResetConflict, Revision: p.revision}
	}
	if _, suppressed := p.suppressed[key]; suppressed {
		return SessionRuntimeResetResult{Status: SessionRuntimeResetAlreadySuppressed, Revision: p.revision}
	}
	p.suppressed[key] = struct{}{}
	return SessionRuntimeResetResult{Status: SessionRuntimeResetSuppressed, Revision: p.revision}
}

func (p *SessionRuntimeTurnProjector) snapshotLocked() ActiveTurnsSnapshot {
	snapshot := ActiveTurnsSnapshot{Revision: p.revision}
	for _, turn := range p.active {
		if p.isSuppressedLocked(turn) {
			continue
		}
		snapshot.ActiveTurns = append(snapshot.ActiveTurns, cloneSessionActiveTurn(turn))
	}
	sort.Slice(snapshot.ActiveTurns, func(i int, j int) bool {
		return snapshot.ActiveTurns[i].NativeSessionID < snapshot.ActiveTurns[j].NativeSessionID
	})
	return snapshot
}

func (p *SessionRuntimeTurnProjector) markChangedLocked() {
	p.revision++
	for _, changes := range p.subscribers {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
}

func (p *SessionRuntimeTurnProjector) isSuppressedLocked(turn SessionActiveTurn) bool {
	_, suppressed := p.suppressed[runtimeTurnKey(turn.NativeSessionID, turn.TurnInstanceID)]
	return suppressed
}

func (p *SessionRuntimeTurnProjector) rememberTerminalLocked(nativeSessionID string, turnInstanceID string) {
	key := runtimeTurnKey(nativeSessionID, turnInstanceID)
	if _, exists := p.terminal[key]; exists {
		return
	}
	p.terminal[key] = struct{}{}
	p.terminalFIFO = append(p.terminalFIFO, key)
	if len(p.terminalFIFO) <= sessionRuntimeTerminalLimit {
		return
	}
	oldest := p.terminalFIFO[0]
	p.terminalFIFO = p.terminalFIFO[1:]
	delete(p.terminal, oldest)
}

func runtimeTurnKey(nativeSessionID string, turnInstanceID string) string {
	return nativeSessionID + "\x00" + turnInstanceID
}

func cloneSessionActiveTurn(turn SessionActiveTurn) SessionActiveTurn {
	turn.PromptRequestID = append(json.RawMessage(nil), turn.PromptRequestID...)
	turn.PendingApprovalID = append(json.RawMessage(nil), turn.PendingApprovalID...)
	return turn
}

func newSessionRuntimeTurnInstanceID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("allocate turn instance id: %w", err)
	}
	return "turn_" + hex.EncodeToString(random[:]), nil
}
