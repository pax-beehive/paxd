package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EEPoolGivenActiveTurnWhenTunnelReconnectsThenFinalReplyRemainsEncrypted(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		offlineOutput bool
		writeFailure  bool
	}{
		{name: "output after reconnect"},
		{name: "output during disconnect and after reconnect", offlineOutput: true},
		{name: "network write failure then reconnect", writeFailure: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := t.Context()
			spec := validAgentSpec()
			root := make([]byte, 32)
			store := newSpyStore()
			factory := ReliableEngineFromStore(store)
			t.Cleanup(func() { require.NoError(t, factory.(*reliableEngineFactory).Close(context.Background())) })
			routes := newFakeACPRouteStore(spec.ConnectionID)
			routes.seedRoute(ACPRoute{ConnectionID: spec.ConnectionID, NativeSessionID: "native_1",
				BoundSlotID: "slot_1", BoundProcessEpoch: "epoch_1", Version: 1,
				ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`)})
			registry := NewACPPoolRegistry(ACPRouteStoreFactoryFunc(func(string) ACPRouteStore { return routes }))
			pool, err := registry.Get(spec.ConnectionID)
			require.NoError(t, err)
			slot := newFakeRouterSlot("slot_1", "epoch_1", 0)
			pool.UpsertSlot(slot)
			bindings := newFakeACPSessionBindingStore()
			require.NoError(t, bindings.BindSessionIDs(ctx, spec.ConnectionID, "session_1", "native_1"))
			receipts := &fakeE2EECommandStore{seen: make(map[string]bool)}
			start := func() (*fakeWebSocketConn, <-chan Exit) {
				conn := newFakeWebSocketConn()
				// Withhold ACKs so the next connection must replay the same durable records.
				enqueueAlignedReconcile(t, conn, spec.TransportQueueID, 0)
				running := make(chan struct{}, 1)
				session := NewAgentTunnelSession(spec, AgentTunnelSessionDeps{
					Headers: fakeHeaderProvider{header: http.Header{}}, Dialer: &fakeDialer{conn: conn},
					ACPPoolRegistry: registry, ReliableEngineFactory: factory, ACPSessionBindings: bindings,
					E2EERootKey: root, E2EECommandStore: receipts,
					Heartbeat: HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour},
					SessionEventSink: SessionEventSinkFunc(func(event SessionEvent) {
						if event.Phase == PhaseRunning {
							running <- struct{}{}
						}
					}),
				})
				done := make(chan Exit, 1)
				go func() { done <- session.Run(ctx) }()
				t.Cleanup(func() { _ = conn.Close() })
				select {
				case <-running:
				case exit := <-done:
					t.Fatalf("tunnel exited before running: %+v", exit)
				case <-time.After(3 * time.Second):
					t.Fatal("tunnel did not start")
				}
				return conn, done
			}
			conn, done := start()
			firstConn := conn
			command, err := e2ee.Encrypt(root, e2ee.DirectionCommand, e2ee.Metadata{
				RecordID: "cmd_1", AgentID: spec.CloudAgentID, SessionID: "session_1", Kind: "acp_command", KeyEpoch: 1,
			}, []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"session_1","prompt":[{"type":"text","text":"question"}]}}`))
			require.NoError(t, err)
			payload, err := json.Marshal(command)
			require.NoError(t, err)
			wire, err := reliablemq.MarshalEnvelope(reliablemq.Envelope{
				Type: reliablemq.EnvelopeTypeData, QueueID: spec.TransportQueueID, Stream: reliablemq.StreamACP, Seq: 1,
				Payload: payload, Metadata: reliablemq.Metadata{"command_id": "cmd_1", "connection_epoch": "1"},
			})
			require.NoError(t, err)
			conn.readCh <- fakeWSMessage{messageType: websocketTextMessage, payload: wire}
			require.Eventually(t, func() bool {
				frames, _ := store.ListOutboundReplay(ctx, spec.TransportQueueID, reliablemq.StreamACP, 100)
				for _, frame := range frames {
					if frame.Metadata["e2ee_kind"] == "command_ack" {
						return true
					}
				}
				return false
			}, time.Second, time.Millisecond)
			emit := func(text string) {
				chunk, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "native_1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}},
				}})
				require.NoError(t, err)
				require.NoError(t, pool.HandleSlotFrame(ctx, "slot_1", "epoch_1", chunk))
				require.NoError(t, pool.HandleSlotFrame(ctx, "slot_1", "epoch_1", []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionUpdate":"usage_update"}}}`)))
			}
			emit("before ")
			require.Eventually(t, func() bool {
				for _, write := range conn.writes() {
					envelope, _ := reliablemq.UnmarshalEnvelope(write.payload)
					if envelope.Type == reliablemq.EnvelopeTypeData {
						return true
					}
				}
				return false
			}, time.Second, time.Millisecond)
			wantText := "before "
			if scenario.writeFailure {
				conn.mu.Lock()
				conn.writeErr = errors.New("simulated network write failure")
				conn.mu.Unlock()
				emit("network failed ")
				wantText += "network failed "
			} else {
				require.NoError(t, conn.Close())
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("tunnel did not stop")
			}
			if scenario.offlineOutput {
				emit("offline ")
				wantText += "offline "
			}
			conn, done = start()
			emit("final reply")
			wantText += "final reply"
			require.NoError(t, pool.HandleSlotFrame(ctx, "slot_1", "epoch_1", []byte(`{"jsonrpc":"2.0","id":"prompt_1","result":{"stopReason":"end_turn"}}`)))
			require.Eventually(t, func() bool {
				frames, _ := store.ListOutboundReplay(ctx, spec.TransportQueueID, reliablemq.StreamACP, 100)
				for _, frame := range frames {
					var envelope e2ee.Envelope
					_ = json.Unmarshal(frame.Payload, &envelope)
					if envelope.Kind == "e2ee_message" {
						plain, _ := e2ee.Decrypt(root, e2ee.DirectionEvent, envelope)
						var body map[string]any
						_ = json.Unmarshal(plain, &body)
						if body["message_type"] == "turn_done" {
							return true
						}
					}
					var raw acpRPCMessage
					_ = json.Unmarshal(frame.Payload, &raw)
					if string(raw.ID) == `"prompt_1"` && len(raw.Result) > 0 {
						return true
					}
				}
				return false
			}, time.Second, time.Millisecond)
			frames, err := store.ListOutboundReplay(ctx, spec.TransportQueueID, reliablemq.StreamACP, 100)
			require.NoError(t, err)
			var assistantIDs []string
			texts := make(map[string]string)
			completed := make(map[string]bool)
			for _, frame := range frames {
				if frame.Metadata["e2ee_kind"] == "command_ack" {
					continue
				}
				var envelope e2ee.Envelope
				require.NoError(t, json.Unmarshal(frame.Payload, &envelope))
				require.Equal(t, 1, envelope.ProtocolVersion, "no plaintext ACP may escape after reconnect")
				plain, err := e2ee.Decrypt(root, e2ee.DirectionEvent, envelope)
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, json.Unmarshal(plain, &body))
				switch envelope.Kind {
				case "e2ee_message":
					if body["role"] == "assistant" && body["message_type"] == "agent_message_chunk" {
						messageID := body["message_id"].(string)
						if _, exists := completed[messageID]; !exists {
							assistantIDs = append(assistantIDs, messageID)
						}
						assert.Equal(t, e2eeStableID("turn", spec.CloudAgentID, "session_1", `"prompt_1"`), body["turn_id"])
						completed[messageID] = body["status"] == "complete"
					}
				case "e2ee_message_part":
					messageID := body["message_id"].(string)
					if _, exists := completed[messageID]; exists {
						texts[messageID], _ = body["text"].(string)
					}
				}
			}
			var finalText string
			for _, messageID := range assistantIDs {
				assert.True(t, completed[messageID])
				finalText += texts[messageID]
			}
			assert.Equal(t, wantText, finalText)
			assert.Equal(t, 1, slot.writeCount(), "reconnect must not redispatch the prompt")
			lastSeq := frames[len(frames)-1].Key.Seq
			require.Eventually(t, func() bool {
				for _, write := range conn.writes() {
					envelope, _ := reliablemq.UnmarshalEnvelope(write.payload)
					if envelope.Type == reliablemq.EnvelopeTypeData && envelope.Seq == lastSeq {
						return true
					}
				}
				return false
			}, time.Second, time.Millisecond)
			// Lost ACKs replay the original IDs and ciphertext, which the Manager
			// deduplicates, rather than projecting a second assistant message.
			replayed := make(map[int64]reliablemq.Envelope)
			for _, write := range conn.writes() {
				envelope, _ := reliablemq.UnmarshalEnvelope(write.payload)
				if envelope.Type == reliablemq.EnvelopeTypeData {
					replayed[envelope.Seq] = envelope
				}
			}
			replayCount := 0
			for _, write := range firstConn.writes() {
				envelope, _ := reliablemq.UnmarshalEnvelope(write.payload)
				if envelope.Type != reliablemq.EnvelopeTypeData {
					continue
				}
				assert.Equal(t, envelope, replayed[envelope.Seq])
				replayCount++
			}
			assert.Positive(t, replayCount)
			require.NoError(t, conn.Close())
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("reconnected tunnel did not stop")
			}
		})
	}
}

func TestE2EEPoolGivenTransientFailureWhenRecoveredThenRetriesAndSendsNewEncryptedFrames(t *testing.T) {
	for _, scenario := range []string{"journal retry", "journal failure then reconnect", "key provider retry"} {
		t.Run(scenario, func(t *testing.T) {
			root := make([]byte, 32)
			provider := &retryE2EEKeyProvider{root: root}
			deps := AgentTunnelSessionDeps{
				E2EERootKeyProvider: provider,
				E2EECommandStore:    &fakeE2EECommandStore{seen: make(map[string]bool)},
			}
			session := NewAgentTunnelSession(validAgentSpec(), deps)
			pool := NewACPPool("conn_1", newFakeACPRouteStore("conn_1"))
			engine := &retryE2EEEngine{events: make(chan reliablemq.OutboundMessage, 8)}
			bridge := session.poolE2EEBridge(pool, engine)
			t.Cleanup(func() { require.NoError(t, bridge.close(context.Background())) })
			command, err := e2ee.Encrypt(root, e2ee.DirectionCommand, e2ee.Metadata{
				RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1", Kind: "acp_command", KeyEpoch: 1,
			}, []byte(`{"jsonrpc":"2.0","id":"resume_1","method":"session/resume","params":{"sessionId":"native_1"}}`))
			require.NoError(t, err)
			payload, err := json.Marshal(command)
			require.NoError(t, err)
			_, err = bridge.handleCommand(t.Context(), reliablemq.Frame{
				Payload: payload, Metadata: reliablemq.Metadata{"command_id": "cmd_1", "connection_epoch": "1"},
			}, func(context.Context, string, []byte) (string, error) { return "native_1", nil })
			require.NoError(t, err)

			if scenario == "key provider retry" {
				provider.fail.Store(true)
			} else {
				engine.fail.Store(true)
			}
			first := []byte(`{"jsonrpc":"2.0","id":"response_1","result":{"text":"failed output"}}`)
			err = session.sendSessionOutput(t.Context(), "session_1", "native_1", first, engine, bridge)
			require.ErrorContains(t, err, "simulated")
			if scenario == "journal failure then reconnect" {
				session = NewAgentTunnelSession(validAgentSpec(), deps)
				engine = &retryE2EEEngine{events: make(chan reliablemq.OutboundMessage, 8)}
				bridge = session.poolE2EEBridge(pool, engine)
			} else {
				provider.fail.Store(false)
				engine.fail.Store(false)
			}
			read := func(want []byte) e2ee.Envelope {
				t.Helper()
				select {
				case message := <-engine.events:
					var envelope e2ee.Envelope
					require.NoError(t, json.Unmarshal(message.Payload, &envelope))
					require.Equal(t, 1, envelope.ProtocolVersion)
					assert.Equal(t, "session_1", envelope.SessionID)
					plain, err := e2ee.Decrypt(root, e2ee.DirectionEvent, envelope)
					require.NoError(t, err)
					var batch struct {
						Frames []json.RawMessage `json:"frames"`
					}
					require.NoError(t, json.Unmarshal(plain, &batch))
					require.Len(t, batch.Frames, 1)
					assert.JSONEq(t, string(want), string(batch.Frames[0]))
					return envelope
				case <-time.After(2 * time.Second):
					t.Fatal("recovered sender did not emit an encrypted frame")
					return e2ee.Envelope{}
				}
			}
			// Let the existing batch timer retry; do not invoke flush manually.
			retried := read(first)
			second := []byte(`{"jsonrpc":"2.0","id":"response_2","result":{"text":"new output after recovery"}}`)
			require.NoError(t, session.sendSessionOutput(t.Context(), "session_1", "native_1", second, engine, bridge))
			fresh := read(second)
			assert.NotEqual(t, retried.RecordID, fresh.RecordID)
		})
	}
}

type retryE2EEEngine struct {
	ReliableEngine
	fail   atomic.Bool
	events chan reliablemq.OutboundMessage
}

func (e *retryE2EEEngine) Send(_ context.Context, message reliablemq.OutboundMessage) error {
	if kind := message.Metadata["e2ee_kind"]; kind == "history" || kind == "command_ack" {
		return nil
	}
	if e.fail.Load() {
		return errors.New("simulated journal failure")
	}
	e.events <- message
	return nil
}

type retryE2EEKeyProvider struct {
	root []byte
	fail atomic.Bool
}

func (p *retryE2EEKeyProvider) RootKey(context.Context, string, int64) ([]byte, error) {
	if p.fail.Load() {
		return nil, errors.New("simulated key provider failure")
	}
	return append([]byte(nil), p.root...), nil
}

func TestE2EEPoolGivenRotatedQueueWhenReboundThenKeepsEncryptionAndUsesNewProducer(t *testing.T) {
	pool := NewACPPool("conn_1", newFakeACPRouteStore("conn_1"))
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		E2EERootKey: make([]byte, 32), E2EECommandStore: &fakeE2EECommandStore{seen: make(map[string]bool)},
	})
	first := &turnEnvelopeRecorder{}
	bridge := session.poolE2EEBridge(pool, first)
	sessionContext := e2eeSessionContext{sessionID: "session_1", keyEpoch: 1, connectionEpoch: 1}
	bridge.trackCommand([]byte(`{"jsonrpc":"2.0","id":"resume_1","method":"session/resume"}`), sessionContext)
	bridge.bindNativeSession("native_1", sessionContext)
	second := &turnEnvelopeRecorder{}
	session.spec.TransportQueueID = "agent_1:rotated"
	rebound := session.poolE2EEBridge(pool, second)
	handled, err := rebound.sendOutput(t.Context(), "native_1", []byte(`{"jsonrpc":"2.0","id":"prompt_1","result":{"stopReason":"end_turn"}}`))
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Empty(t, first.messages)
	require.Len(t, second.messages, 1)
	assert.Equal(t, "agent_1:rotated", second.messages[0].QueueID)
	var event e2ee.Envelope
	require.NoError(t, json.Unmarshal(second.messages[0].Payload, &event))
	plain, err := e2ee.Decrypt(make([]byte, 32), e2ee.DirectionEvent, event)
	require.NoError(t, err)
	assert.Contains(t, string(plain), "end_turn")
}

func TestE2EEPoolGivenLegacyConfigurationWhenAttachedThenKeepsLegacyOutput(t *testing.T) {
	pool := NewACPPool("conn_1", newFakeACPRouteStore("conn_1"))
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{})
	engine := &turnEnvelopeRecorder{}
	bridge := session.poolE2EEBridge(pool, engine)
	require.Nil(t, bridge)
	payload := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	require.NoError(t, session.sendSessionOutput(t.Context(), "session_1", "native_1", payload, engine, bridge))
	require.Len(t, engine.messages, 1)
	assert.Equal(t, payload, []byte(engine.messages[0].Payload))
}
