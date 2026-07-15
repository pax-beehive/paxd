package runtime

import (
	"context"
	"testing"

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

func TestACPPoolOutputSinkDetachUsesAttachmentToken(t *testing.T) {
	pool := NewACPPool("conn_1", newFakeACPRouteStore("conn_1"))
	firstCalls := 0
	secondCalls := 0

	detachFirst := pool.AttachOutputSink(ACPRouterOutputSinkFunc(func(context.Context, []byte) error {
		firstCalls++
		return nil
	}))
	detachSecond := pool.AttachOutputSink(ACPRouterOutputSinkFunc(func(context.Context, []byte) error {
		secondCalls++
		return nil
	}))

	assert.NotPanics(t, detachFirst)
	require.NoError(t, pool.outputSink.EmitManagerFrame(context.Background(), []byte(`{}`)))
	assert.Zero(t, firstCalls)
	assert.Equal(t, 1, secondCalls)

	assert.NotPanics(t, detachSecond)
	assert.Nil(t, pool.outputSink)
}
