package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type codexFailureTestSlot struct {
	*fakeRouterSlot
}

func (codexFailureTestSlot) codexSessionFailures() bool { return true }

func TestACPRouterUsesOnlyCurrentCodexPromptFailure(t *testing.T) {
	ctx := t.Context()
	profile, err := buildACPClientInitProfile("1.2.3", true)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"protocolVersion":1,"clientInfo":{"name":"paxd","version":"1.2.3"},
		"clientCapabilities":{"session":{"configOptions":{"boolean":{}}},
			"_meta":{"jetbrains":{"air":{"version":1,"capabilities":["sessionFailure"]}}}}
	}`, string(profile.Params))

	store := newFakeACPRouteStore("conn_1")
	slot := codexFailureTestSlot{newFakeRouterSlot("slot_a", "epoch_a", 0)}
	var emitted [][]byte
	var attributedTurn string
	router := newTestACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(
		func(ctx context.Context, _ string, payload []byte) error {
			emitted = append(emitted, payload)
			attributedTurn = acpTurnID(ctx)
			return nil
		},
	)))
	router.UpsertSlot(slot)
	store.seedRoute(ACPRoute{
		ConnectionID: "conn_1", NativeSessionID: "session_1",
		BoundSlotID: "slot_a", BoundProcessEpoch: "epoch_a", Version: 1,
		ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`),
	})
	first := []byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)
	require.NoError(t, router.HandleManagerFrame(withACPTurnID(ctx, "turn_first"), first))

	chunk := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"HTTP 400 invalid_model_request"}}}}`)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", chunk))
	assert.JSONEq(t, string(chunk), string(emitted[0]))

	failed := []byte(`{"jsonrpc":"2.0","id":10,"result":{"stopReason":"end_turn","_meta":{"jetbrains":{"air":{"version":1,"sessionFailure":{"id":"native-turn:error","revision":1,"severity":"error","title":"private vendor detail"}}}}}}`)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "stale_epoch", failed))
	require.Len(t, router.RuntimeSnapshot().ActiveTurns, 1)
	assert.JSONEq(t, string(failed), string(emitted[1]))

	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", failed))
	require.Len(t, emitted, 3)
	assert.Equal(t, "turn_first", attributedTurn)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":10,"error":{"code":-32603,"message":"Codex could not complete this turn.","data":{"kind":"codex_turn_failed"}}}`, string(emitted[2]))
	require.Empty(t, router.RuntimeSnapshot().ActiveTurns)

	second := []byte(`{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)
	require.NoError(t, router.HandleManagerFrame(withACPTurnID(ctx, "turn_second"), second))
	recovered := []byte(`{"jsonrpc":"2.0","id":11,"result":{"stopReason":"end_turn","_meta":{"jetbrains":{"air":{"version":1,"sessionFailure":{"id":"native-turn:retry","revision":1,"severity":"warning"}}}}}}`)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", recovered))
	assert.JSONEq(t, string(recovered), string(emitted[3]))
	require.Empty(t, router.RuntimeSnapshot().ActiveTurns)
}
