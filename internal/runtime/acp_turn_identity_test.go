package runtime

import (
	"context"
	"encoding/json"
	"github.com/pax-beehive/paxkit/reliablemq"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPTurnIdentitySurvivesCompletionAndLateResponse(t *testing.T) {
	ctx := t.Context()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	var emitted []string
	router := newTestACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(
		func(ctx context.Context, _ string, _ []byte) error {
			emitted = append(emitted, acpTurnID(ctx))
			return nil
		},
	)))
	router.UpsertSlot(slot)
	store.seedRoute(ACPRoute{ConnectionID: "conn_1", NativeSessionID: "session_1",
		BoundSlotID: "slot_a", BoundProcessEpoch: "epoch_a", Version: 1,
		ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`)})
	first := []byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)
	require.NoError(t, router.HandleManagerFrame(withACPTurnID(ctx, "turn_first"), first))
	require.Equal(t, "turn_first", router.RuntimeSnapshot().ActiveTurns[0].TurnID)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session_1","update":{"sessionUpdate":"agent_message_chunk"}}}`)))
	terminal := []byte(`{"jsonrpc":"2.0","id":10,"result":{"stopReason":"end_turn"}}`)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", terminal))
	require.Empty(t, router.RuntimeSnapshot().ActiveTurns)
	require.Error(t, router.HandleManagerFrame(withACPTurnID(ctx, "turn_first"), first), "finished turn must not execute again")
	second := []byte(`{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)
	require.NoError(t, router.HandleManagerFrame(withACPTurnID(ctx, "turn_second"), second))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", terminal))
	assert.Equal(t, []string{"turn_first", "turn_first", "turn_first"}, emitted)
	require.Equal(t, "turn_second", router.RuntimeSnapshot().ActiveTurns[0].TurnID)
	assert.NotContains(t, string(slot.writes[0]), "turn_first", "envelope ID must not alter native ACP")
}

type turnEnvelopeRecorder struct {
	ReliableEngine
	messages []reliablemq.OutboundMessage
}

func (r *turnEnvelopeRecorder) Send(_ context.Context, message reliablemq.OutboundMessage) error {
	r.messages = append(r.messages, message)
	return nil
}

func TestOutputEnvelopeKeepsOriginalTurnAcrossLaterOutput(t *testing.T) {
	recorder := &turnEnvelopeRecorder{}
	session := &AgentTunnelSession{spec: AgentConnectionSpec{CloudAgentID: "agent_1", TransportQueueID: "queue_1"}}
	payload := []byte(`{"id":10,"result":{"stopReason":"end_turn"}}`)
	require.NoError(t, session.sendSessionOutput(withACPTurnID(t.Context(), "turn_first"), "sess_1", "native_1", payload, recorder, nil))
	require.NoError(t, session.sendSessionOutput(withACPTurnID(t.Context(), "turn_second"), "sess_1", "native_1", payload, recorder, nil))
	require.Len(t, recorder.messages, 2)
	assert.Equal(t, "turn_first", recorder.messages[0].Metadata["turn_id"])
	assert.Equal(t, "turn_second", recorder.messages[1].Metadata["turn_id"])
	assert.Equal(t, payload, []byte(recorder.messages[0].Payload))
}
