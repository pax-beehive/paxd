package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPhaseValid(t *testing.T) {
	for _, phase := range []Phase{PhaseConnecting, PhaseConnected, PhaseStarting, PhaseRunning, PhaseStopping} {
		assert.True(t, phase.Valid(), "%q Valid()", phase)
	}
	assert.False(t, Phase("backoff").Valid(), "backoff is supervisor status, not a runtime session event phase")
}

func TestSessionKindValid(t *testing.T) {
	for _, kind := range []SessionKind{SessionRemoteControl, SessionAgentTunnel} {
		assert.True(t, kind.Valid(), "%q Valid()", kind)
	}
	assert.False(t, SessionKind("supervisor").Valid())
}

func TestSessionEventSinkFuncReceivesSessionEvent(t *testing.T) {
	var got SessionEvent
	sink := SessionEventSinkFunc(func(event SessionEvent) {
		got = event
	})

	at := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	sink.OnSessionEvent(SessionEvent{
		Kind:         SessionAgentTunnel,
		Phase:        PhaseRunning,
		RemoteID:     "remote_prod",
		ConnectionID: "conn_codex",
		Generation:   3,
		RestartNonce: 1,
		At:           at,
	})

	assert.Equal(t, SessionAgentTunnel, got.Kind)
	assert.Equal(t, PhaseRunning, got.Phase)
	assert.Equal(t, "remote_prod", got.RemoteID)
	assert.Equal(t, "conn_codex", got.ConnectionID)
	assert.Equal(t, int64(3), got.Generation)
	assert.Equal(t, int64(1), got.RestartNonce)
	assert.Equal(t, at, got.At)
}

func TestSessionEventWithDetail(t *testing.T) {
	event := SessionEvent{Kind: SessionRemoteControl}.WithDetail("url", "wss://example.test/node-control")
	assert.Equal(t, "wss://example.test/node-control", event.Details["url"])
}

func TestNoopSessionEventSink(t *testing.T) {
	NoopSessionEventSink{}.OnSessionEvent(SessionEvent{Kind: SessionRemoteControl, Phase: PhaseConnecting})
}
