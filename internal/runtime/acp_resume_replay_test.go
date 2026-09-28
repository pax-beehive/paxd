package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func replayUpdate(session, kind string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":%q,"content":{"type":"text","text":"fixture"}}}}`, session, kind))
}

func TestACPRouterGivenColdResumeWhenHistoryReplaysThenOnlyStateAndLiveOutputReachManager(t *testing.T) {
	ctx := withACPTurnID(t.Context(), "turn_new")
	store := newFakeACPRouteStore("conn_1")
	store.seedRoute(ACPRoute{ConnectionID: "conn_1", NativeSessionID: "cold", ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`), Version: 1})
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	frames := make(chan []byte, 30)
	atMarker, releaseMarker := make(chan struct{}), make(chan struct{})
	router := newTestACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(ctx context.Context, session string, payload []byte) error {
		var msg acpRPCMessage
		require.NoError(t, json.Unmarshal(payload, &msg))
		if msg.Method == acpSessionResumedMethod {
			close(atMarker)
			<-releaseMarker
		}
		frames <- payload
		return nil
	})))
	router.UpsertSlot(slot)
	done := make(chan error, 1)
	go func() {
		done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":84,"method":"session/prompt","params":{"sessionId":"cold","prompt":[]}}`))
	}()
	require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
	for _, kind := range []string{"user_message_chunk", "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "plan"} {
		require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", replayUpdate("cold", kind)))
	}
	assert.Empty(t, frames, "resume transcript must not escape into reliable transport/history")
	// State updates and other sessions/processes are not replay for this scope.
	for _, kind := range []string{"available_commands_update", "usage_update", "current_mode_update", "config_option_update"} {
		frame := replayUpdate("cold", kind)
		require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", frame))
		assert.Equal(t, frame, <-frames)
	}
	other := replayUpdate("other", "agent_message_chunk")
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", other))
	assert.Equal(t, other, <-frames)
	oldEpoch := replayUpdate("cold", "agent_message_chunk")
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_other", oldEpoch))
	assert.Equal(t, oldEpoch, <-frames)
	permission := []byte(`{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"cold"}}`)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", permission))
	assert.Equal(t, permission, <-frames)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":{"models":{"currentModelId":"restored"}}}`)))
	<-atMarker
	// The resume response has been consumed, but the new prompt has not been sent.
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", replayUpdate("cold", "agent_message_chunk")))
	assert.Empty(t, frames, "late replay after the response is still suppressed before prompt dispatch")
	close(releaseMarker)
	require.NoError(t, <-done)
	assert.Contains(t, string(<-frames), acpSessionResumedMethod)
	assert.Equal(t, 2, slot.writeCount())
	live := replayUpdate("cold", "agent_message_chunk")
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", live))
	assert.Equal(t, live, <-frames, "actual next-turn output must survive")
	final := []byte(`{"jsonrpc":"2.0","id":84,"result":{"stopReason":"end_turn"}}`)
	require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", final))
	assert.Equal(t, final, <-frames)
	assert.Empty(t, frames)
}

func TestACPRouterGivenExplicitResumeWhenReplayArrivesAfterResponseThenSuppressUntilPromptOrLoad(t *testing.T) {
	for _, nextMethod := range []string{"session/prompt", "session/load"} {
		t.Run(nextMethod, func(t *testing.T) {
			ctx := t.Context()
			store := newFakeACPRouteStore("conn_1")
			slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
			frames := make(chan []byte, 10)
			router := newTestACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(_ context.Context, _ string, payload []byte) error { frames <- payload; return nil })))
			router.UpsertSlot(slot)
			done := make(chan error, 1)
			go func() {
				done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":7,"method":"session/resume","params":{"sessionId":"cold","cwd":"/work","mcpServers":[]}}`))
			}()
			require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", replayUpdate("cold", "user_message_chunk")))
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":{"modes":{"currentModeId":"normal"}}}`)))
			require.NoError(t, <-done)
			assert.JSONEq(t, `{"jsonrpc":"2.0","id":7,"result":{"modes":{"currentModeId":"normal"}}}`, string(<-frames))
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", replayUpdate("cold", "user_message_chunk")))
			assert.Empty(t, frames)
			// An explicit resume on an already-hot route also keeps replay suppressed.
			require.NoError(t, router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":8,"method":"session/resume","params":{"sessionId":"cold","cwd":"/work","mcpServers":[]}}`)))
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":8,"result":{}}`)))
			assert.JSONEq(t, `{"jsonrpc":"2.0","id":8,"result":{}}`, string(<-frames))
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", replayUpdate("cold", "tool_call")))
			assert.Empty(t, frames)
			require.NoError(t, router.HandleManagerFrame(ctx, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":9,"method":%q,"params":{"sessionId":"cold","prompt":[]}}`, nextMethod))))
			frame := replayUpdate("cold", "user_message_chunk")
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", frame))
			assert.Equal(t, frame, <-frames, "explicit load history and live prompt frames must remain available")
			assert.Empty(t, frames)
		})
	}
}

type replayFailingSlot struct {
	*fakeRouterSlot
	failMethod string
}

func (s *replayFailingSlot) Send(ctx context.Context, payload []byte) error {
	var msg acpRPCMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}
	if msg.Method == s.failMethod {
		return fmt.Errorf("fixture send failed")
	}
	return s.fakeRouterSlot.Send(ctx, payload)
}

func TestACPRouterGivenFailedResumeOrPromptWhenLateReplayArrivesThenKeepItSuppressed(t *testing.T) {
	for _, failure := range []string{"resume_send", "resume_error", "cancel", "prompt_send"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := newFakeACPRouteStore("conn_1")
			store.seedRoute(ACPRoute{ConnectionID: "conn_1", NativeSessionID: "cold", ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`), Version: 1})
			slot := &replayFailingSlot{fakeRouterSlot: newFakeRouterSlot("slot_a", "epoch_a", 0)}
			if failure == "resume_send" {
				slot.failMethod = "session/resume"
			}
			if failure == "prompt_send" {
				slot.failMethod = "session/prompt"
			}
			frames := make(chan []byte, 10)
			router := newTestACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(_ context.Context, _ string, payload []byte) error { frames <- payload; return nil })))
			router.UpsertSlot(slot)
			done := make(chan error, 1)
			go func() {
				done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"cold","prompt":[]}}`))
			}()
			if failure != "resume_send" {
				require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
				switch failure {
				case "cancel":
					cancel()
				case "resume_error":
					require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","error":{"code":-1,"message":"fixture resume error"}}`)))
				default:
					require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":{}}`)))
				}
			}
			require.Error(t, <-done)
			require.NoError(t, router.HandleSlotFrame(t.Context(), "slot_a", "epoch_a", replayUpdate("cold", "agent_message_chunk")))
			assert.Empty(t, frames)
			assert.Empty(t, router.RuntimeSnapshot().ActiveTurns)
		})
	}
}

func TestACPRouterGivenReplayGuardWhenWorkerIsReplacedThenRemoveOnlyItsScope(t *testing.T) {
	for _, teardown := range []string{"remove_epoch", "remove_all", "replace"} {
		t.Run(teardown, func(t *testing.T) {
			router := newTestACPRouter("conn_1", newFakeACPRouteStore("conn_1"))
			router.UpsertSlot(newFakeRouterSlot("slot_a", "epoch_a", 0))
			guarded := resumeTranscriptScope{"cold", "slot_a", "epoch_a"}
			unaffected := resumeTranscriptScope{"other", "slot_b", "epoch_b"}
			router.setResumeTranscriptSuppressed(guarded, true)
			router.setResumeTranscriptSuppressed(unaffected, true)
			switch teardown {
			case "remove_epoch":
				router.RemoveSlot("slot_a", "epoch_wrong")
				require.Len(t, router.resumeTranscripts, 2)
				router.RemoveSlot("slot_a", "epoch_a")
			case "remove_all":
				router.RemoveSlot("slot_a", "")
			case "replace":
				router.UpsertSlot(newFakeRouterSlot("slot_a", "epoch_new", 0))
			}
			assert.NotContains(t, router.resumeTranscripts, guarded)
			assert.Contains(t, router.resumeTranscripts, unaffected)
		})
	}
}

func TestACPRouterGivenResumeGuardWhenNonTranscriptFrameArrivesThenPreserveIt(t *testing.T) {
	router := newTestACPRouter("conn_1", newFakeACPRouteStore("conn_1"))
	scope := resumeTranscriptScope{"cold", "slot_a", "epoch_a"}
	router.setResumeTranscriptSuppressed(scope, true)
	for _, msg := range []acpRPCMessage{
		{Method: "session/update", Params: json.RawMessage(`{"update":{"sessionUpdate":"future_extension"}}`)},
		{Method: "session/update", Params: json.RawMessage(`[]`)},
		{Method: "session/update", Params: json.RawMessage(`{"update":{"sessionUpdate":"agent_message_chunk"}}`), ID: json.RawMessage(`1`)},
		{Method: "session/request_permission"},
	} {
		assert.False(t, router.suppressResumeTranscript("cold", "slot_a", "epoch_a", msg))
	}
	// An explicit resume of a hot, running session must not mute the active turn.
	router.setResumeTranscriptSuppressed(scope, false)
	slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
	require.NoError(t, router.acquirePromptLease("cold", slot, json.RawMessage(`1`), "turn_live"))
	router.setResumeTranscriptSuppressed(scope, true)
	assert.Empty(t, router.resumeTranscripts)
}
