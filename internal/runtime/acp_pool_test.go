package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPPoolRemoveSlotClearsOnlyMatchingProcessEpoch(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	store.seedRoute(ACPRoute{
		ConnectionID:      "conn_1",
		NativeSessionID:   "session_1",
		BoundSlotID:       "slot_1",
		BoundProcessEpoch: "epoch_new",
		ResumeParams:      []byte(`{"sessionId":"session_1"}`),
		Version:           1,
	})
	pool := NewACPPool("conn_1", store)

	pool.RemoveSlot(ctx, "slot_1", "epoch_old")
	route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "slot_1", route.BoundSlotID)
	assert.Equal(t, "epoch_new", route.BoundProcessEpoch)

	pool.RemoveSlot(ctx, "slot_1", "epoch_new")
	route, ok, err = store.GetACPSessionRoute(ctx, "conn_1", "session_1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, route.BoundSlotID)
	assert.Empty(t, route.BoundProcessEpoch)
	assert.Equal(t, "slot_1", route.LastSlotID)
}

func TestACPPoolReturnsTypedRouterErrorsToManager(t *testing.T) {
	pool := NewACPPool("conn_1", newFakeACPRouteStore("conn_1"))
	type emittedFrame struct {
		nativeSessionID string
		payload         []byte
	}
	emitted := make(chan emittedFrame, 1)
	pool.AttachOutputSink(ACPRouterOutputSinkFunc(func(_ context.Context, nativeSessionID string, payload []byte) error {
		emitted <- emittedFrame{nativeSessionID: nativeSessionID, payload: append([]byte(nil), payload...)}
		return nil
	}))

	require.NoError(t, pool.HandleManagerFrameForSession(
		context.Background(),
		"native-missing",
		[]byte(`{"jsonrpc":"2.0","id":"request-7","method":"session/prompt","params":{"sessionId":"native-missing","prompt":[]}}`),
	))
	frame := <-emitted
	assert.Equal(t, "native-missing", frame.nativeSessionID)
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int64 `json:"code"`
			Data struct {
				Kind           string `json:"kind"`
				RequiresResume bool   `json:"requiresResume"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(frame.payload, &response))
	assert.JSONEq(t, `"request-7"`, string(response.ID))
	assert.Equal(t, int64(-32002), response.Error.Code)
	assert.Equal(t, "session_route_missing", response.Error.Data.Kind)
	assert.True(t, response.Error.Data.RequiresResume)
}

func TestACPRouterErrorResponsePreservesNumericRequestIDAndMarksBusyRetryable(t *testing.T) {
	payload, ok := acpRouterErrorResponse(
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{}}`),
		ACPRouterError{Code: "slot_busy", Message: "ACP slot already has an active prompt"},
	)
	require.True(t, ok)
	var response struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int64 `json:"code"`
			Data struct {
				Kind      string `json:"kind"`
				Retryable bool   `json:"retryable"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(payload, &response))
	assert.Equal(t, "7", string(response.ID))
	assert.Equal(t, int64(-32001), response.Error.Code)
	assert.Equal(t, "slot_busy", response.Error.Data.Kind)
	assert.True(t, response.Error.Data.Retryable)
}

func TestACPPoolOutputSinkDetachUsesAttachmentToken(t *testing.T) {
	pool := NewACPPool("conn_1", newFakeACPRouteStore("conn_1"))
	firstCalls := 0
	secondCalls := 0

	detachFirst := pool.AttachOutputSink(ACPRouterOutputSinkFunc(func(context.Context, string, []byte) error {
		firstCalls++
		return nil
	}))
	detachSecond := pool.AttachOutputSink(ACPRouterOutputSinkFunc(func(context.Context, string, []byte) error {
		secondCalls++
		return nil
	}))

	assert.NotPanics(t, detachFirst)
	require.NoError(t, pool.outputSink.EmitManagerFrame(context.Background(), "session_1", []byte(`{}`)))
	assert.Zero(t, firstCalls)
	assert.Equal(t, 1, secondCalls)

	assert.NotPanics(t, detachSecond)
	assert.Nil(t, pool.outputSink)
}

func TestACPPoolRegistryPublishesRuntimeSnapshotsWithoutMissingFirstMutation(t *testing.T) {
	store := newFakeACPRouteStore("conn_1")
	store.seedRoute(ACPRoute{
		ConnectionID: "conn_1", NativeSessionID: "session_1",
		BoundSlotID: "slot_1", BoundProcessEpoch: "epoch_1",
		ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`), Version: 1,
	})
	registry := NewACPPoolRegistry(ACPRouteStoreFactoryFunc(func(string) ACPRouteStore { return store }))
	changes, unsubscribe := registry.SubscribeSessionRuntime("remote_prod")
	defer unsubscribe()
	pool, err := registry.Get("conn_1")
	require.NoError(t, err)
	pool.UpsertSlot(newFakeRouterSlot("slot_1", "epoch_1", 0))

	require.NoError(t, pool.HandleManagerFrame(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)))
	select {
	case <-changes:
	case <-time.After(time.Second):
		require.Fail(t, "registry did not publish the runtime mutation")
	}
	snapshot, ok := registry.SessionRuntimeSnapshot("conn_1")
	require.True(t, ok)
	require.Len(t, snapshot.ActiveTurns, 1)
	assert.Equal(t, "session_1", snapshot.ActiveTurns[0].NativeSessionID)
}
