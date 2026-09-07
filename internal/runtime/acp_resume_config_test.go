package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPRouterColdResumePublishesConfigurationBeforePrompt(t *testing.T) {
	for _, result := range []string{
		`{"configOptions":[{"id":"model","name":"Model","currentValue":"restored","options":[]}],"_meta":{"extension":true}}`,
		`{"models":{"currentModelId":"legacy","availableModels":[]}}`,
	} {
		t.Run(result, func(t *testing.T) {
			ctx := t.Context()
			store := newFakeACPRouteStore("conn_1")
			store.seedRoute(ACPRoute{ConnectionID: "conn_1", NativeSessionID: "session_cold", ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`), Version: 1})
			slot := newFakeRouterSlot("slot_a", "epoch_a", 0)
			frames := make(chan []byte, 2)
			router := newTestACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(ctx context.Context, sessionID string, payload []byte) error {
				require.Equal(t, "session_cold", sessionID)
				var msg acpRPCMessage
				require.NoError(t, json.Unmarshal(payload, &msg))
				if msg.Method == acpSessionResumedMethod {
					route, ok, err := store.GetACPSessionRoute(ctx, "conn_1", sessionID)
					require.NoError(t, err)
					require.True(t, ok)
					assert.Equal(t, "epoch_a", route.BoundProcessEpoch)
					assert.Equal(t, 1, slot.writeCount(), "publish restored state before forwarding the prompt")
				}
				frames <- append([]byte(nil), payload...)
				return nil
			})))
			router.UpsertSlot(slot)
			done := make(chan error, 1)
			go func() {
				done <- router.HandleManagerFrame(ctx, []byte(`{"jsonrpc":"2.0","id":20,"method":"session/prompt","params":{"sessionId":"session_cold","prompt":[]}}`))
			}()
			require.Eventually(t, func() bool { return slot.writeCount() == 1 }, eventuallyWait, eventuallyTick)
			commands := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session_cold","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"status","description":"Status"}]}}}`)
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", commands))
			require.NoError(t, router.HandleSlotFrame(ctx, "slot_a", "epoch_a", []byte(`{"jsonrpc":"2.0","id":"paxd.resume.1","result":`+result+`}`)))
			require.NoError(t, <-done)
			assert.JSONEq(t, string(commands), string(<-frames))
			expected := `{"jsonrpc":"2.0","method":"_pax/session_resumed","params":{"sessionId":"session_cold","result":` + result + `}}`
			assert.JSONEq(t, expected, string(<-frames))
			assert.Equal(t, 2, slot.writeCount())
			assert.Empty(t, frames, "the internal resume response must not leak its request ID")
		})
	}
}

func TestACPRouterEmptyResumeResultDoesNotInventConfiguration(t *testing.T) {
	router := newTestACPRouter("conn_1", newFakeACPRouteStore("conn_1"), WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(context.Context, string, []byte) error {
		t.Error("unexpected configuration notification")
		return nil
	})))
	for _, result := range []string{`{}`, `null`, `{"ok":true}`} {
		require.NoError(t, router.emitResumedSessionConfig(t.Context(), "session_cold", json.RawMessage(result)))
	}
}
