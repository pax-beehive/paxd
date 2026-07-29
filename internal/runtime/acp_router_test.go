package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	eventuallyWait = time.Second
	eventuallyTick = 10 * time.Millisecond
)

func TestACPRouterCommitsNewSessionRouteBeforeEmittingResponse(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	var emitted [][]byte
	var emittedSessionID string
	router := NewACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(ctx context.Context, nativeSessionID string, payload []byte) error {
		assert.True(t, store.boundBeforeEmit("session_1"), "route must be bound before session/new response is emitted")
		emittedSessionID = nativeSessionID
		emitted = append(emitted, append([]byte(nil), payload...))
		return nil
	})))
	router.UpsertSlot(slot)

	err := router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work","mcpServers":[{"name":"fs","command":"fs-mcp","args":[],"env":[]}],"additionalDirectories":["/shared"]}}`))
	require.NoError(t, err)
	require.Len(t, slot.writes, 1)

	err = router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_1"}}`))
	require.NoError(t, err)
	require.Len(t, emitted, 1)
	assert.Equal(t, "session_1", emittedSessionID)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_1"}}`, string(emitted[0]))

	route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "slot_a", route.BoundSlotID)
	assert.Equal(t, "epoch_a", route.BoundProcessEpoch)
	assert.JSONEq(t, `{"cwd":"/work","mcpServers":[{"name":"fs","command":"fs-mcp","args":[],"env":[]}],"additionalDirectories":["/shared"]}`, string(route.ResumeParams))
}

func TestACPRouterLocalizesPaxConversationMCPServerOnNewSession(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	router := NewACPRouter(
		"conn_1",
		store,
		WithACPRouterExecutablePath(func() (string, error) {
			return "/home/kk/.local/bin/paxd", nil
		}),
	)
	router.UpsertSlot(slot)

	err := router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work","mcpServers":[{"name":"pax-conversation-k7m2x5qa","command":"paxd","args":["mcp","conversation","serve"],"env":[{"name":"PAX_AGENT_ID","value":"agent_1"}]},{"name":"fs","command":"fs-mcp","args":[],"env":[]}]}}`))
	require.NoError(t, err)
	require.Len(t, slot.writes, 1)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work","mcpServers":[{"name":"pax-conversation-k7m2x5qa","command":"/home/kk/.local/bin/paxd","args":["mcp","conversation","serve"],"env":[{"name":"PAX_AGENT_ID","value":"agent_1"}]},{"name":"fs","command":"fs-mcp","args":[],"env":[]}]}}`, string(slot.writes[0]))

	err = router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_1"}}`))
	require.NoError(t, err)
	route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{"cwd":"/work","mcpServers":[{"name":"pax-conversation-k7m2x5qa","command":"/home/kk/.local/bin/paxd","args":["mcp","conversation","serve"],"env":[{"name":"PAX_AGENT_ID","value":"agent_1"}]},{"name":"fs","command":"fs-mcp","args":[],"env":[]}]}`, string(route.ResumeParams))
}

func TestIsPaxConversationMCPName(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{name: "pax-conversation", expected: true},
		{name: "pax-conversation-k7m2x5qa", expected: true},
		{name: "pax-conversation-short", expected: false},
		{name: "pax-conversation-12345678", expected: false},
		{name: "pax-conversation-k7m2x9q!", expected: false},
		{name: "other-k7m2x9qa", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isPaxConversationMCPName(tt.name))
		})
	}
}

func TestACPRouterDistributesNewSessionsAcrossReadySlotsAndKeepsRoutesSticky(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slotA := newFakeRouterSlot("slot_a", "epoch_a", 0)
	slotB := newFakeRouterSlot("slot_b", "epoch_b", 1)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slotA)
	router.UpsertSlot(slotB)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work-a","mcpServers":[]}}`)))
	require.Len(t, slotA.writes, 1)
	require.Empty(t, slotB.writes)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_a"}}`)))

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work-b","mcpServers":[]}}`)))
	require.Len(t, slotB.writes, 1)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session_b"}}`)))

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session_a","prompt":[]}}`)))
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session_b","prompt":[]}}`)))
	require.Len(t, slotA.writes, 2)
	require.Len(t, slotB.writes, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session_a","prompt":[]}}`, string(slotA.writes[1]))
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session_b","prompt":[]}}`, string(slotB.writes[1]))
}

func TestACPRouterAssignsNewSessionToSlotWithLeastRecentPrompt(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slotA := newFakeRouterSlot("slot_a", "epoch_a", 0)
	slotB := newFakeRouterSlot("slot_b", "epoch_b", 1)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slotA)
	router.UpsertSlot(slotB)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work-a","mcpServers":[]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_a"}}`)))
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work-b","mcpServers":[]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session_b"}}`)))

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session_a","prompt":[]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`)))

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":4,"method":"session/new","params":{"cwd":"/work-c","mcpServers":[]}}`)))
	require.Equal(t, 2, slotB.writeCount(), "slot without a recent prompt should receive the new session")
	require.Equal(t, 2, slotA.writeCount())
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":4,"method":"session/new","params":{"cwd":"/work-c","mcpServers":[]}}`, string(slotB.writes[1]))
}

func TestACPRouterDoesNotAssignNewSessionToSlotWithActivePrompt(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slotA := newFakeRouterSlot("slot_a", "epoch_a", 0)
	slotB := newFakeRouterSlot("slot_b", "epoch_b", 1)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slotA)
	router.UpsertSlot(slotB)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work-a","mcpServers":[]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_a"}}`)))
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work-b","mcpServers":[]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session_b"}}`)))

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session_a","prompt":[]}}`)))
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":4,"method":"session/new","params":{"cwd":"/work-c","mcpServers":[]}}`)))

	require.Equal(t, 2, slotA.writeCount())
	require.Equal(t, 2, slotB.writeCount(), "slot without an active prompt should receive the new session")
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":4,"method":"session/new","params":{"cwd":"/work-c","mcpServers":[]}}`, string(slotB.writes[1]))
}

func TestACPRouterResumesOnlySessionsBoundToRestartedSlot(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	oldSlotA := newFakeRouterSlot("slot_a", "epoch_a_old", 0)
	slotB := newFakeRouterSlot("slot_b", "epoch_b", 1)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(oldSlotA)
	router.UpsertSlot(slotB)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work-a","mcpServers":[]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a_old", []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_a"}}`)))
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work-b","mcpServers":[]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session_b"}}`)))

	router.RemoveSlot("slot_a", "epoch_a_old")
	cleared, err := store.ClearACPSessionRoutesForProcess(ctx, "conn_1", "slot_a", "epoch_a_old")
	require.NoError(t, err)
	require.Equal(t, int64(1), cleared)
	newSlotA := newFakeRouterSlot("slot_a", "epoch_a_new", 0)
	router.UpsertSlot(newSlotA)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session_b","prompt":[]}}`)))
	require.Len(t, slotB.writes, 2)
	assert.NotContains(t, string(slotB.writes[1]), "session/resume")

	done := make(chan error, 1)
	go func() {
		done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session_a","prompt":[]}}`))
	}()
	require.Eventually(t, func() bool { return newSlotA.writeCount() == 1 }, eventuallyWait, eventuallyTick)
	assert.Contains(t, string(newSlotA.writes[0]), "session/resume")
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a_new", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":{}}`)))
	require.NoError(t, <-done)
	require.Len(t, newSlotA.writes, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session_a","prompt":[]}}`, string(newSlotA.writes[1]))

	routeB, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_b")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "slot_b", routeB.BoundSlotID)
	assert.Equal(t, "epoch_b", routeB.BoundProcessEpoch)
}

func TestACPRouterDrainStopsAdmissionAndWaitsForActivePrompt(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	store.seedRoute(ACPRoute{
		ConnectionID:      "conn_1",
		NativeSessionID:   "session_a",
		BoundSlotID:       "slot_a",
		BoundProcessEpoch: "epoch_a",
		ResumeParams:      json.RawMessage(`{"cwd":"/work-a","mcpServers":[]}`),
		Version:           1,
	})
	slotA := newFakeRouterSlot("slot_a", "epoch_a", 0)
	slotB := newFakeRouterSlot("slot_b", "epoch_b", 1)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slotA)
	router.UpsertSlot(slotB)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{"sessionId":"session_a","prompt":[]}}`)))
	drained := router.BeginSlotDrain("slot_a", "epoch_a")
	select {
	case <-drained:
		t.Fatal("active slot reported drained before its prompt completed")
	default:
	}

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":11,"method":"session/new","params":{"cwd":"/work-b","mcpServers":[]}}`)))
	require.Len(t, slotB.writes, 1)
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":12,"method":"session/cancel","params":{"sessionId":"session_a"}}`)))
	require.Len(t, slotA.writes, 2)
	assert.Contains(t, string(slotA.writes[1]), "session/cancel")

	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":10,"result":{}}`)))
	select {
	case <-drained:
	case <-time.After(eventuallyWait):
		t.Fatal("slot did not finish draining after its active prompt completed")
	}
}

func TestACPRouterResumesCreatedSessionAfterSlotProcessEpochChanges(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	oldSlot := newFakeRouterSlot("slot_a", "epoch_old", 0)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(oldSlot)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/work","mcpServers":[{"name":"fs","command":"fs-mcp","args":[],"env":[]}],"additionalDirectories":["/shared"]}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_old", []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"session_1"}}`)))

	router.RemoveSlot("slot_a", "epoch_old")
	cleared, err := store.ClearACPSessionRoutesForProcess(ctx, "conn_1", "slot_a", "epoch_old")
	require.NoError(t, err)
	assert.Equal(t, int64(1), cleared)
	newSlot := newFakeRouterSlot("slot_a", "epoch_new", 0)
	router.UpsertSlot(newSlot)

	done := make(chan error, 1)
	go func() {
		done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`))
	}()

	require.Eventually(t, func() bool { return newSlot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":"paxd.resume.1","method":"session/resume","params":{"sessionId":"session_1","cwd":"/work","mcpServers":[{"name":"fs","command":"fs-mcp","args":[],"env":[]}],"additionalDirectories":["/shared"]}}`, string(newSlot.writes[0]))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_new", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":{}}`)))
	require.NoError(t, <-done)
	require.Len(t, newSlot.writes, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`, string(newSlot.writes[1]))

	route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "slot_a", route.BoundSlotID)
	assert.Equal(t, "epoch_new", route.BoundProcessEpoch)
}

func TestACPRouterEnforcesOneActivePromptPerSessionAndSlot(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	store.seedRoute(ACPRoute{
		ConnectionID:      "conn_1",
		NativeSessionID:   "session_1",
		BoundSlotID:       "slot_a",
		BoundProcessEpoch: "epoch_a",
		ResumeParams:      json.RawMessage(`{"sessionId":"session_1","cwd":"/work","mcpServers":{}}`),
		Version:           1,
	})
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slot)

	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)))
	err := router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`))
	require.Error(t, err)
	assert.Equal(t, "session_busy", routerErrorCode(err))

	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":10,"result":{"ok":true}}`)))
	require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":12,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`)))
	assert.Len(t, slot.writes, 2)
}

func TestACPRouterResumesColdRouteBeforePrompt(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_b", "epoch_b", 1)
	store.seedRoute(ACPRoute{
		ConnectionID:    "conn_1",
		NativeSessionID: "session_cold",
		LastSlotID:      "slot_a",
		ResumeParams:    json.RawMessage(`{"cwd":"/work","mcpServers":{"fs":{}}}`),
		Version:         1,
	})
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slot)

	done := make(chan error, 1)
	go func() {
		done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":20,"method":"session/prompt","params":{"sessionId":"session_cold","prompt":[]}}`))
	}()

	require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":"paxd.resume.1","method":"session/resume","params":{"sessionId":"session_cold","cwd":"/work","mcpServers":{"fs":{}}}}`, string(slot.writes[0]))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":{"ok":true}}`)))
	require.NoError(t, <-done)
	require.Len(t, slot.writes, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":20,"method":"session/prompt","params":{"sessionId":"session_cold","prompt":[]}}`, string(slot.writes[1]))

	route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_cold")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "slot_b", route.BoundSlotID)
	assert.Equal(t, "epoch_b", route.BoundProcessEpoch)
}

func TestACPRouterLocalizesPaxConversationMCPServerOnColdResume(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_b", "epoch_b", 1)
	store.seedRoute(ACPRoute{
		ConnectionID:    "conn_1",
		NativeSessionID: "session_cold",
		ResumeParams:    json.RawMessage(`{"cwd":"/work","mcpServers":[{"name":"pax-conversation","command":"/usr/local/bin/paxd","args":["mcp","conversation","serve"],"env":[{"name":"PAX_SESSION_ID","value":"session_cold"}]}]}`),
		Version:         1,
	})
	router := NewACPRouter(
		"conn_1",
		store,
		WithACPRouterExecutablePath(func() (string, error) {
			return "/home/kk/.local/bin/paxd", nil
		}),
	)
	router.UpsertSlot(slot)

	done := make(chan error, 1)
	go func() {
		done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":20,"method":"session/prompt","params":{"sessionId":"session_cold","prompt":[]}}`))
	}()

	require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":"paxd.resume.1","method":"session/resume","params":{"sessionId":"session_cold","cwd":"/work","mcpServers":[{"name":"pax-conversation","command":"/home/kk/.local/bin/paxd","args":["mcp","conversation","serve"],"env":[{"name":"PAX_SESSION_ID","value":"session_cold"}]}]}}`, string(slot.writes[0]))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":{"ok":true}}`)))
	require.NoError(t, <-done)
	require.Len(t, slot.writes, 2)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":20,"method":"session/prompt","params":{"sessionId":"session_cold","prompt":[]}}`, string(slot.writes[1]))

	route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_cold")
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{"cwd":"/work","mcpServers":[{"name":"pax-conversation","command":"/home/kk/.local/bin/paxd","args":["mcp","conversation","serve"],"env":[{"name":"PAX_SESSION_ID","value":"session_cold"}]}]}`, string(route.ResumeParams))
}

func TestACPRouterResumeFailureDoesNotCreateReplacementSession(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_b", "epoch_b", 1)
	store.seedRoute(ACPRoute{
		ConnectionID:    "conn_1",
		NativeSessionID: "session_cold",
		ResumeParams:    json.RawMessage(`{"cwd":"/work","mcpServers":{}}`),
		Version:         1,
	})
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slot)

	done := make(chan error, 1)
	go func() {
		done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":20,"method":"session/prompt","params":{"sessionId":"session_cold","prompt":[]}}`))
	}()
	require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","error":{"code":-32000,"message":"gone"}}`)))
	err := <-done
	require.Error(t, err)
	assert.Equal(t, "session_resume_failed", routerErrorCode(err))
	require.Len(t, slot.writes, 1)
	assert.NotContains(t, string(slot.writes[0]), "session/new")
}

func TestACPRouterIncompleteColdResumeDescriptorDoesNotReachSlot(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_b", "epoch_b", 1)
	store.seedRoute(ACPRoute{
		ConnectionID:    "conn_1",
		NativeSessionID: "session_cold",
		ResumeParams:    json.RawMessage(`{"sessionId":"session_cold"}`),
		Version:         1,
	})
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slot)

	err := router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":20,"method":"session/prompt","params":{"sessionId":"session_cold","prompt":[]}}`))

	require.Error(t, err)
	assert.Equal(t, "session_route_missing", routerErrorCode(err))
	assert.Empty(t, slot.writes)
}

func TestResumeDescriptorFromSessionNewParams(t *testing.T) {
	t.Run("defaults omitted MCP servers to an empty list", func(t *testing.T) {
		descriptor, err := resumeDescriptorFromSessionNewParams(json.RawMessage(`{"cwd":"/work"}`))

		require.NoError(t, err)
		assert.JSONEq(t, `{"cwd":"/work","mcpServers":[]}`, string(descriptor))
	})

	t.Run("normalizes supported snake case fields", func(t *testing.T) {
		descriptor, err := resumeDescriptorFromSessionNewParams(json.RawMessage(`{"cwd":"/work","mcp_servers":[{"name":"fs"}],"additional_directories":["/shared"]}`))

		require.NoError(t, err)
		assert.JSONEq(t, `{"cwd":"/work","mcpServers":[{"name":"fs"}],"additionalDirectories":["/shared"]}`, string(descriptor))
	})

	t.Run("rejects malformed params", func(t *testing.T) {
		_, err := resumeDescriptorFromSessionNewParams(json.RawMessage(`{"cwd":`))

		require.Error(t, err)
		assert.Equal(t, "invalid_session_lifecycle", routerErrorCode(err))
	})

	t.Run("rejects params without cwd", func(t *testing.T) {
		_, err := resumeDescriptorFromSessionNewParams(json.RawMessage(`{"mcpServers":[]}`))

		require.Error(t, err)
		assert.Equal(t, "invalid_session_lifecycle", routerErrorCode(err))
	})
}

func TestResumeParamsForSessionRejectsIncompleteDescriptor(t *testing.T) {
	tests := []struct {
		name       string
		sessionID  string
		descriptor json.RawMessage
	}{
		{name: "empty session ID", descriptor: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`)},
		{name: "empty descriptor", sessionID: "session_1"},
		{name: "malformed descriptor", sessionID: "session_1", descriptor: json.RawMessage(`{"cwd":`)},
		{name: "missing cwd", sessionID: "session_1", descriptor: json.RawMessage(`{"mcpServers":[]}`)},
		{name: "missing MCP servers", sessionID: "session_1", descriptor: json.RawMessage(`{"cwd":"/work"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resumeParamsForSession(tt.sessionID, tt.descriptor)

			require.Error(t, err)
			assert.Equal(t, "session_route_missing", routerErrorCode(err))
		})
	}
}

func TestACPRouterValidatesWorkerRequestSourceBySessionAndRawID(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slotA := newFakeRouterSlot("slot_a", "epoch_a", 0)
	slotB := newFakeRouterSlot("slot_b", "epoch_b", 1)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slotA)
	router.UpsertSlot(slotB)

	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":1,"method":"permission/request","params":{"sessionId":"session_a"}}`)))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_b", "epoch_b", []byte(`{"jsonrpc":"2.0","id":1,"method":"permission/request","params":{"sessionId":"session_b"}}`)))

	require.NoError(t, router.HandleManagerFrameForSession(ctx, "session_b", []byte(`{"jsonrpc":"2.0","id":1,"result":{"approved":true}}`)))
	require.NoError(t, router.HandleManagerFrameForSession(ctx, "session_a", []byte(`{"jsonrpc":"2.0","id":1,"result":{"approved":false}}`)))

	require.Len(t, slotA.writes, 1)
	require.Len(t, slotB.writes, 1)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{"approved":false}}`, string(slotA.writes[0]))
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{"approved":true}}`, string(slotB.writes[0]))
}

func TestACPRouterUnknownRouteDoesNotReachSlot(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	router := NewACPRouter("conn_1", store)
	router.UpsertSlot(slot)

	err := router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"missing","prompt":[]}}`))
	require.Error(t, err)
	assert.Equal(t, "session_route_missing", routerErrorCode(err))
	assert.Empty(t, slot.writes)
}

func TestACPRouterExplicitResumeCreatesMissingRouteBeforeReply(t *testing.T) {
	ctx := context.Background()
	store := newFakeACPRouteStore("conn_1")
	managerFrames := make(chan []byte, 1)
	router := NewACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(_ context.Context, nativeSessionID string, payload []byte) error {
		require.Equal(t, "session_legacy", nativeSessionID)
		managerFrames <- append([]byte(nil), payload...)
		return nil
	})))
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	router.UpsertSlot(slot)
	done := make(chan error, 1)
	go func() {
		done <- router.HandleManagerFrameForSession(ctx, "session_legacy", []byte(`{"jsonrpc":"2.0","id":"resume-7","method":"session/resume","params":{"sessionId":"session_legacy","cwd":"/work","mcpServers":[],"additionalDirectories":["/shared"]}}`))
	}()

	require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
	slot.mu.Lock()
	request := append([]byte(nil), slot.writes[0]...)
	slot.mu.Unlock()
	var internalResume acpRPCMessage
	require.NoError(t, json.Unmarshal(request, &internalResume))
	assert.Equal(t, "session/resume", internalResume.Method)
	assert.NotEqual(t, `"resume-7"`, string(internalResume.ID))
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":`+string(internalResume.ID)+`,"result":{}}`)))
	require.NoError(t, <-done)

	response := <-managerFrames
	var responseMessage acpRPCMessage
	require.NoError(t, json.Unmarshal(response, &responseMessage))
	assert.Equal(t, `"resume-7"`, string(responseMessage.ID))
	assert.JSONEq(t, `{}`, string(responseMessage.Result))
	route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", "session_legacy")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "slot_a", route.BoundSlotID)
	assert.Equal(t, "epoch_a", route.BoundProcessEpoch)
	assert.JSONEq(t, `{"cwd":"/work","mcpServers":[],"additionalDirectories":["/shared"]}`, string(route.ResumeParams))
}

func routerErrorCode(err error) string {
	var routerErr ACPRouterError
	if errors.As(err, &routerErr) {
		return routerErr.Code
	}
	return ""
}

type fakeRouterSlot struct {
	slotID       string
	processEpoch string
	ordinal      int
	ready        bool
	mu           sync.Mutex
	writes       [][]byte
}

func newFakeRouterSlot(slotID string, processEpoch string, ordinal int) *fakeRouterSlot {
	return &fakeRouterSlot{slotID: slotID, processEpoch: processEpoch, ordinal: ordinal, ready: true}
}

func (s *fakeRouterSlot) SlotID() string {
	return s.slotID
}

func (s *fakeRouterSlot) ProcessEpoch() string {
	return s.processEpoch
}

func (s *fakeRouterSlot) Ordinal() int {
	return s.ordinal
}

func (s *fakeRouterSlot) Ready() bool {
	return s.ready
}

func (s *fakeRouterSlot) Send(ctx context.Context, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, append([]byte(nil), payload...))
	return nil
}

func (s *fakeRouterSlot) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

type fakeACPRouteStore struct {
	connectionID string
	mu           sync.Mutex
	routes       map[string]ACPRoute
	bound        map[string]bool
}

func newFakeACPRouteStore(connectionID string) *fakeACPRouteStore {
	return &fakeACPRouteStore{
		connectionID: connectionID,
		routes:       make(map[string]ACPRoute),
		bound:        make(map[string]bool),
	}
}

func (s *fakeACPRouteStore) seedRoute(route ACPRoute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[route.NativeSessionID] = route
}

func (s *fakeACPRouteStore) boundBeforeEmit(nativeSessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bound[nativeSessionID]
}

func (s *fakeACPRouteStore) GetACPSessionRoute(ctx context.Context, connectionID string, nativeSessionID string) (ACPRoute, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route, ok := s.routes[nativeSessionID]
	return route, ok, nil
}

func (s *fakeACPRouteStore) UpsertACPSessionRoute(ctx context.Context, connectionID string, nativeSessionID string, resumeParams json.RawMessage) (ACPRoute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route := s.routes[nativeSessionID]
	if route.Version == 0 {
		route.Version = 1
	} else {
		route.Version++
	}
	route.ConnectionID = connectionID
	route.NativeSessionID = nativeSessionID
	route.ResumeParams = append(json.RawMessage(nil), resumeParams...)
	s.routes[nativeSessionID] = route
	return route, nil
}

func (s *fakeACPRouteStore) BindACPSessionRoute(ctx context.Context, update ACPRouteBindingUpdate) (ACPRoute, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route, ok := s.routes[update.NativeSessionID]
	if !ok || route.Version != update.ExpectedVersion {
		return ACPRoute{}, false, nil
	}
	route.BoundSlotID = update.SlotID
	route.BoundProcessEpoch = update.ProcessEpoch
	if len(update.ResumeParams) > 0 {
		route.ResumeParams = append(json.RawMessage(nil), update.ResumeParams...)
	}
	route.Version++
	s.routes[update.NativeSessionID] = route
	s.bound[update.NativeSessionID] = true
	return route, true, nil
}

func (s *fakeACPRouteStore) CountBoundACPSessionRoutesBySlot(ctx context.Context, connectionID string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int)
	for _, route := range s.routes {
		if route.ConnectionID == connectionID && route.BoundSlotID != "" && route.BoundProcessEpoch != "" {
			counts[route.BoundSlotID]++
		}
	}
	return counts, nil
}

func (s *fakeACPRouteStore) ClearACPSessionRoutesForProcess(ctx context.Context, connectionID string, slotID string, processEpoch string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cleared int64
	for nativeSessionID, route := range s.routes {
		if route.ConnectionID != connectionID || route.BoundSlotID != slotID || route.BoundProcessEpoch != processEpoch {
			continue
		}
		route.LastSlotID = route.BoundSlotID
		route.BoundSlotID = ""
		route.BoundProcessEpoch = ""
		route.Version++
		s.routes[nativeSessionID] = route
		cleared++
	}
	return cleared, nil
}
