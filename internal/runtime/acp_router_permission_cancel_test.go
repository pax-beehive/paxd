package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPRouterCancelResolvesPendingPermissionsAndAllowsNextTurn(t *testing.T) {
	ctx := t.Context()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	router := newTestACPRouter("conn_1", store)
	router.UpsertSlot(slot)
	for _, sessionID := range []string{"session_1", "session_2"} {
		store.seedRoute(ACPRoute{
			ConnectionID: "conn_1", NativeSessionID: sessionID,
			BoundSlotID: "slot_a", BoundProcessEpoch: "epoch_a",
			ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`), Version: 1,
		})
	}
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)))
	for _, payload := range []string{
		`{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session_1"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/request_permission","params":{"sessionId":"session_1"}}`,
		`{"jsonrpc":"2.0","id":"other","method":"session/request_permission","params":{"sessionId":"session_2"}}`,
	} {
		require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(payload)))
	}
	require.Equal(t, SessionRuntimeWaitingApproval, router.RuntimeSnapshot().ActiveTurns[0].RuntimeStatus)
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"session_1"}}`)))
	require.Len(t, slot.writes, 4)
	var cancel acpRPCMessage
	require.NoError(t, json.Unmarshal(slot.writes[1], &cancel))
	assert.Equal(t, "session/cancel", cancel.Method)
	var ids []string
	for _, payload := range slot.writes[2:] {
		var response acpRPCMessage
		require.NoError(t, json.Unmarshal(payload, &response))
		ids = append(ids, string(response.ID))
		assert.JSONEq(t, `{"outcome":{"outcome":"cancelled"}}`, string(response.Result))
	}
	assert.ElementsMatch(t, []string{`"perm-1"`, `2`}, ids)
	assert.Len(t, router.pendingWorkerReqs, 1, "another session's request must remain pending")
	assert.Len(t, router.RuntimeSnapshot().ActiveTurns, 1, "wait for the actual prompt completion")
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":10,"result":{"stopReason":"cancelled"}}`)))
	assert.Empty(t, router.RuntimeSnapshot().ActiveTurns)
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)))
	assert.Equal(t, SessionRuntimeRunning, router.RuntimeSnapshot().ActiveTurns[0].RuntimeStatus)
}
