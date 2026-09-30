package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EEHistoryProjectorGivenPromptAndStreamingOutputWhenBoundaryArrivesThenBuildsCanonicalHistory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 18, 30, 0, 0, time.UTC)
	projector := newE2EEHistoryProjector("agent_1", func() time.Time { return now })

	userRecords, err := projector.projectCommand("session_1", []byte(
		`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"session_1","prompt":[{"type":"text","text":"hello"}]}}`,
	))
	require.NoError(t, err)
	require.Len(t, userRecords, 2)
	assert.Equal(t, "e2ee_message", userRecords[0].kind)
	assert.Equal(t, "e2ee_message_part", userRecords[1].kind)
	var userMessage e2eeHistoryMessagePayload
	require.NoError(t, json.Unmarshal(userRecords[0].plaintext, &userMessage))
	assert.Equal(t, "user", userMessage.Role)
	assert.Equal(t, "user_message", userMessage.MessageType)
	var userPart e2eeHistoryPartPayload
	require.NoError(t, json.Unmarshal(userRecords[1].plaintext, &userPart))
	assert.Equal(t, "hello", userPart.Text)

	records, err := projector.projectFrames("session_1", []json.RawMessage{
		json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hel"}}}}`),
		json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"lo"}}}}`),
		json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionUpdate":"tool_call","toolCallId":"tool_1","title":"Read"}}}`),
	})
	require.NoError(t, err)
	var textPart e2eeHistoryPartPayload
	for _, record := range records {
		if record.kind != "e2ee_message_part" {
			continue
		}
		var candidate e2eeHistoryPartPayload
		require.NoError(t, json.Unmarshal(record.plaintext, &candidate))
		if candidate.PartType == "text" {
			textPart = candidate
		}
	}
	assert.Equal(t, "hello", textPart.Text)

	done, err := projector.projectFrames("session_1", []json.RawMessage{
		json.RawMessage(`{"jsonrpc":"2.0","id":"prompt_1","result":{"stopReason":"end_turn"}}`),
	})
	require.NoError(t, err)
	var messageTypes []string
	for _, record := range done {
		if record.kind != "e2ee_message" {
			continue
		}
		var message e2eeHistoryMessagePayload
		require.NoError(t, json.Unmarshal(record.plaintext, &message))
		messageTypes = append(messageTypes, message.MessageType)
		if message.MessageType == "agent_message_chunk" {
			assert.Equal(t, "complete", message.Status)
		}
	}
	assert.ElementsMatch(t, []string{"turn_done"}, messageTypes)
}

func TestE2EEHistoryProjectorGivenLongStreamWhenPartFillsThenSealsBoundedPartWithoutFullSnapshot(t *testing.T) {
	t.Parallel()
	projector := newE2EEHistoryProjector("agent_1", time.Now)
	_, err := projector.projectCommand("session_1", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"go"}]}}`,
	))
	require.NoError(t, err)
	content := strings.Repeat("x", e2eeHistoryPartBytes+17)
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": content},
		}},
	})
	require.NoError(t, err)
	records, err := projector.projectFrames("session_1", []json.RawMessage{frame})
	require.NoError(t, err)
	var sealed e2eeHistoryPartPayload
	for _, record := range records {
		if record.kind == "e2ee_message_part" {
			require.NoError(t, json.Unmarshal(record.plaintext, &sealed))
		}
	}
	assert.Equal(t, e2eeHistoryPartBytes, len(sealed.Text))
	assert.Equal(t, 0, sealed.PartIndex)
}

func TestE2EEHistoryProjectorGivenRepeatedPartialCheckpointsWhenPartIsUpsertedThenKeepsWholePart(t *testing.T) {
	t.Parallel()
	now := time.Now()
	projector := newE2EEHistoryProjector("agent_1", func() time.Time { return now })
	_, err := projector.projectCommand("session_1", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"go"}]}}`,
	))
	require.NoError(t, err)

	project := func(content string) []e2eeCanonicalRecord {
		textFrame := fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":%q}}}}`,
			content,
		)
		records, projectErr := projector.projectFrames("session_1", []json.RawMessage{json.RawMessage(textFrame)})
		require.NoError(t, projectErr)
		checkpoint, checkpointErr := projector.checkpointText(projector.sessions["session_1"], false)
		require.NoError(t, checkpointErr)
		records = append(records, checkpoint...)
		return records
	}

	assert.Equal(t, "hello", lastE2EETextPart(t, project("hello")).Text)
	assert.Equal(t, "hello world", lastE2EETextPart(t, project(" world")).Text)
}

func TestE2EEHistoryProjectorGivenContinuousLongThinkWhenCheckpointElapsesThenPersistsPartialPart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 22, 0, 0, 0, time.UTC)
	projector := newE2EEHistoryProjector("agent_1", func() time.Time { return now })
	_, err := projector.projectCommand("session_1", []byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[{"type":"text","text":"go"}]}}`,
	))
	require.NoError(t, err)
	frame := func(content string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_thought_chunk","content":{"text":%q}}}}`,
			content,
		))
	}

	records, err := projector.projectFrames("session_1", []json.RawMessage{frame("thinking")})
	require.NoError(t, err)
	assert.Empty(t, records[1:], "the initial frame only emits the message header")

	now = now.Add(e2eeHistoryCheckpointDelay)
	records, err = projector.projectFrames("session_1", []json.RawMessage{frame(" more")})
	require.NoError(t, err)
	part := lastE2EETextPart(t, records)
	assert.Equal(t, "thinking more", part.Text)
	assert.Equal(t, "agent_thought_chunk", part.PartType)
}

func lastE2EETextPart(t *testing.T, records []e2eeCanonicalRecord) e2eeHistoryPartPayload {
	t.Helper()
	var result e2eeHistoryPartPayload
	for _, record := range records {
		if record.kind != "e2ee_message_part" {
			continue
		}
		var candidate e2eeHistoryPartPayload
		require.NoError(t, json.Unmarshal(record.plaintext, &candidate))
		if candidate.Text != "" {
			result = candidate
		}
	}
	return result
}
