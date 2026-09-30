package runtime

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EEHistorySegmentsPreserveTextToolOrder(t *testing.T) {
	for _, batched := range []bool{false, true} {
		t.Run(fmt.Sprintf("batched=%t", batched), func(t *testing.T) {
			projector := newE2EEHistoryProjector("agent_1", time.Now)
			_, err := projector.projectCommand("session_1", []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"go"}]}}`))
			require.NoError(t, err)
			text := func(kind, content string) json.RawMessage {
				return json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":%q,"content":{"text":%q}}}}`, kind, content))
			}
			frames := []json.RawMessage{
				text("agent_message_chunk", "A1"), text("agent_message_chunk", "A2"),
				json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"tool_1"}}}`),
				text("agent_message_chunk", "B1"), text("agent_message_chunk", "B2"),
				text("agent_thought_chunk", "thinking"),
				text("agent_message_chunk", "C"),
				json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`),
			}
			var records []e2eeCanonicalRecord
			if batched {
				records, err = projector.projectFrames("session_1", frames)
				require.NoError(t, err)
			} else {
				for _, frame := range frames {
					next, projectErr := projector.projectFrames("session_1", []json.RawMessage{frame})
					require.NoError(t, projectErr)
					records = append(records, next...)
					// Persistence checkpoints must not create a new segment.
					checkpoint, checkpointErr := projector.checkpointText(projector.sessions["session_1"], false)
					require.NoError(t, checkpointErr)
					records = append(records, checkpoint...)
				}
			}
			// Model durable upserts: header revisions keep the original row position,
			// and partial part revisions replace their previous value.
			var order []string
			messages := map[string]e2eeHistoryMessagePayload{}
			parts := map[string]e2eeHistoryPartPayload{}
			for _, record := range records {
				if record.kind == "e2ee_message" {
					var message e2eeHistoryMessagePayload
					require.NoError(t, json.Unmarshal(record.plaintext, &message))
					if _, exists := messages[message.MessageID]; !exists {
						order = append(order, message.MessageID)
					}
					messages[message.MessageID] = message
				} else {
					var part e2eeHistoryPartPayload
					require.NoError(t, json.Unmarshal(record.plaintext, &part))
					parts[part.MessageID] = part
				}
			}
			var kinds, contents []string
			for _, id := range order {
				message := messages[id]
				kinds = append(kinds, message.MessageType)
				if message.MessageType == "agent_message_chunk" || message.MessageType == "agent_thought_chunk" {
					assert.JSONEq(t, `{"text_layout":"segment"}`, string(message.RawJSON))
					assert.Equal(t, "complete", message.Status)
					contents = append(contents, parts[id].Text)
				}
			}
			assert.Equal(t, []string{"agent_message_chunk", "tool_call", "agent_message_chunk", "agent_thought_chunk", "agent_message_chunk", "turn_done"}, kinds)
			assert.Equal(t, []string{"A1A2", "B1B2", "thinking", "C"}, contents)
			assert.Empty(t, projector.sessions["session_1"].activeTextKey)
			assert.Len(t, projector.sessions["session_1"].text, 1, "retain only user request deduplication state")
		})
	}
}
