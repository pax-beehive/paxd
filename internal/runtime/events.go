package runtime

import "time"

type Phase string

const (
	PhaseConnecting Phase = "connecting"
	PhaseConnected  Phase = "connected"
	PhaseStarting   Phase = "starting"
	PhaseRunning    Phase = "running"
	PhaseStopping   Phase = "stopping"
)

func (p Phase) Valid() bool {
	switch p {
	case PhaseConnecting, PhaseConnected, PhaseStarting, PhaseRunning, PhaseStopping:
		return true
	default:
		return false
	}
}

type SessionKind string

const (
	SessionRemoteControl SessionKind = "remote_control"
	SessionAgentTunnel   SessionKind = "agent_tunnel"
)

func (k SessionKind) Valid() bool {
	switch k {
	case SessionRemoteControl, SessionAgentTunnel:
		return true
	default:
		return false
	}
}

type SessionEvent struct {
	Kind         SessionKind
	Phase        Phase
	RemoteID     string
	ConnectionID string
	Generation   int64
	RestartNonce int64
	PID          *int
	At           time.Time
	Details      map[string]string
}

func (e SessionEvent) WithDetail(key string, value string) SessionEvent {
	if e.Details == nil {
		e.Details = make(map[string]string, 1)
	}
	e.Details[key] = value
	return e
}

type SessionEventSink interface {
	OnSessionEvent(event SessionEvent)
}

type SessionEventSinkFunc func(event SessionEvent)

func (f SessionEventSinkFunc) OnSessionEvent(event SessionEvent) {
	if f != nil {
		f(event)
	}
}

type NoopSessionEventSink struct{}

func (NoopSessionEventSink) OnSessionEvent(event SessionEvent) {
	_ = event
}
