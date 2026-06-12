// Package state implements the daemon state machine.
//
// States: STARTING → REGISTERING → RUNNING → STOPPING → STOPPED
package state

import (
	"context"
	"sync"
)

// State represents a daemon lifecycle state.
type State int

const (
	STARTING State = iota
	REGISTERING
	RUNNING
	STOPPING
	STOPPED
)

// String returns a human-readable state name.
func (s State) String() string {
	switch s {
	case STARTING:
		return "STARTING"
	case REGISTERING:
		return "REGISTERING"
	case RUNNING:
		return "RUNNING"
	case STOPPING:
		return "STOPPING"
	case STOPPED:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// Machine manages daemon lifecycle state with thread-safe transitions.
type Machine struct {
	mu    sync.RWMutex
	state State
	ctx   context.Context
	cancel context.CancelFunc
}

// NewMachine creates a state machine in STARTING state.
func NewMachine() *Machine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Machine{
		state:  STARTING,
		ctx:    ctx,
		cancel: cancel,
	}
}

// State returns the current state.
func (m *Machine) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Context returns a context that is cancelled when the daemon enters STOPPING.
func (m *Machine) Context() context.Context {
	return m.ctx
}

// Transition attempts to move to a new state. Returns an error if the
// transition is not allowed.
func (m *Machine) Transition(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.allowed(to) {
		return &InvalidTransition{From: m.state, To: to}
	}
	m.state = to

	if to == STOPPING || to == STOPPED {
		m.cancel()
	}
	return nil
}

// allowed validates state transitions.
func (m *Machine) allowed(to State) bool {
	switch m.state {
	case STARTING:
		return to == REGISTERING || to == STOPPING
	case REGISTERING:
		return to == RUNNING || to == STOPPING
	case RUNNING:
		return to == STOPPING
	case STOPPING:
		return to == STOPPED
	case STOPPED:
		return false // terminal state
	default:
		return false
	}
}

// InvalidTransition is returned when an invalid state transition is attempted.
type InvalidTransition struct {
	From State
	To   State
}

func (e *InvalidTransition) Error() string {
	return "invalid state transition: " + e.From.String() + " → " + e.To.String()
}
