package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/require"
)

func TestLocalHistoryRetryPreservesCiphertextAndOnlyStreamsNewFrames(t *testing.T) {
	pool := NewACPPool("conn", newFakeACPRouteStore("conn"))
	key := make([]byte, 32)
	var sent [][]byte
	pool.e2eeBridge = newE2EETransportBridge(key, "agent", "queue", &fakeE2EECommandStore{seen: make(map[string]bool)}, func(_ context.Context, data []byte, _ reliablemq.Metadata) error {
		sent = append(sent, append([]byte(nil), data...))
		return nil
	})
	cache := map[string][]byte{}
	persist := func(_ context.Context, id string, data []byte) ([]byte, error) {
		if existing, ok := cache[id]; ok {
			return existing, nil
		}
		cache[id] = append([]byte(nil), data...)
		return data, nil
	}
	now := time.Now().UTC()
	frames := []LocalHistoryFrame{
		{ID: 1, Direction: "input", CreatedAt: now, Payload: json.RawMessage(`{"jsonrpc":"2.0","id":"local1","method":"session/prompt","params":{"sessionId":"native","prompt":[{"type":"text","text":"Review secret"}]}}`)},
		{ID: 2, Direction: "output", CreatedAt: now, Payload: json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Partial secret"}}}}`)},
	}
	keys := e2ee.StaticRootKeyProvider{Key: key}
	require.NoError(t, pool.PublishLocalHistory(t.Context(), "agent", "manager_session", "run", frames, 0, keys, persist))
	first := append([][]byte(nil), sent...)
	sent = nil
	require.NotEmpty(t, first)
	require.NoError(t, pool.PublishLocalHistory(t.Context(), "agent", "manager_session", "run", frames, 0, keys, persist))
	require.Equal(t, first, sent)
	for _, data := range sent {
		require.NotContains(t, string(data), "secret")
		var envelope e2ee.Envelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		require.Equal(t, "manager_session", envelope.SessionID)
		plain, err := e2ee.Decrypt(key, e2ee.DirectionEvent, envelope)
		require.NoError(t, err)
		if envelope.Kind == "e2ee_message" {
			require.NotContains(t, string(plain), `"role":"assistant","status":"complete"`)
		}
	}
	sent = nil
	frames = append(frames, LocalHistoryFrame{ID: 3, Direction: "output", CreatedAt: now, Payload: json.RawMessage(`{"jsonrpc":"2.0","id":"local1","result":{"stopReason":"end_turn"}}`)})
	require.NoError(t, pool.PublishLocalHistory(t.Context(), "agent", "manager_session", "run", frames, 2, keys, persist))
	count := 0
	for _, data := range sent {
		var envelope e2ee.Envelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		if envelope.Kind == "acp_event" {
			count++
			plain, err := e2ee.Decrypt(key, e2ee.DirectionEvent, envelope)
			require.NoError(t, err)
			require.Contains(t, string(plain), "end_turn")
			require.NotContains(t, string(plain), "Partial secret")
		}
	}
	require.Equal(t, 1, count)
}
