package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/e2ee"
)

func TestE2EEReplayGivenPromptWhenHistoryIsUnavailableThenStreamContainsWholeTurn(t *testing.T) {
	key := make([]byte, 32)
	type item struct {
		payload  []byte
		metadata reliablemq.Metadata
	}
	var events []item
	b := newE2EETransportBridge(key, "agent", "queue", &fakeE2EECommandStore{seen: map[string]bool{}}, func(_ context.Context, p []byte, m reliablemq.Metadata) error {
		if m["e2ee_kind"] == "history" {
			return errors.New("history unavailable")
		}
		if m["e2ee_kind"] == "event" {
			events = append(events, item{append([]byte(nil), p...), m})
		}
		return nil
	})
	defer func() { _ = b.close(context.Background()) }()
	command := []byte(`{"jsonrpc":"2.0","id":"request","method":"session/prompt","params":{"sessionId":"native","prompt":[{"type":"text","text":"private prompt"}]}}`)
	encrypted, err := e2ee.Encrypt(key, e2ee.DirectionCommand, e2ee.Metadata{RecordID: "cmd", AgentID: "agent", SessionID: "session", Kind: "acp_command", KeyEpoch: 1}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(encrypted)
	require.NoError(t, err)
	dispatch := func(context.Context, string, []byte) (string, error) {
		require.NotEmpty(t, events, "prompt must enter the reliable stream before dispatch")
		return "native", nil
	}
	_, err = b.handleCommand(t.Context(), reliablemq.Frame{Payload: payload, Metadata: reliablemq.Metadata{"command_id": "cmd", "connection_epoch": "1"}}, dispatch)
	require.NoError(t, err)
	_, err = b.sendOutput(t.Context(), "native", []byte(`{"jsonrpc":"2.0","id":"request","result":{"stopReason":"end_turn"}}`))
	require.NoError(t, err)
	require.Len(t, events, 2)
	var turn string
	for i, event := range events {
		assert.NotContains(t, string(event.payload), "private prompt")
		assert.NotContains(t, event.metadata, "native_session_id")
		var envelope e2ee.Envelope
		require.NoError(t, json.Unmarshal(event.payload, &envelope))
		plaintext, err := e2ee.Decrypt(key, e2ee.DirectionEvent, envelope)
		require.NoError(t, err)
		var batch struct {
			TurnID string            `json:"turn_id"`
			Frames []json.RawMessage `json:"frames"`
		}
		require.NoError(t, json.Unmarshal(plaintext, &batch))
		require.NotEmpty(t, event.metadata["turn_ref"])
		assert.Equal(t, batch.TurnID, event.metadata["turn_ref"])
		if i == 0 {
			turn = batch.TurnID
			require.Len(t, batch.Frames, 1)
			assert.JSONEq(t, string(command), string(batch.Frames[0]))
		} else {
			assert.Equal(t, turn, batch.TurnID)
		}
	}
}

func TestE2EEReplayGivenFailedPromptSendThenReportsFailureBeforeDispatch(t *testing.T) {
	b := newE2EETransportBridge(make([]byte, 32), "agent", "queue", &fakeE2EECommandStore{}, func(context.Context, []byte, reliablemq.Metadata) error { return errors.New("journal unavailable") })
	session := e2eeSessionContext{sessionID: "session", keyEpoch: 1}
	require.ErrorContains(t, b.sendReplayPrompt(t.Context(), session, make([]byte, 32), []byte(`{"method":"session/prompt"}`)), "journal unavailable")
	require.Error(t, b.sendReplayPrompt(t.Context(), session, []byte("invalid"), []byte(`{}`)))
	require.Error(t, b.sendReplayPrompt(t.Context(), session, make([]byte, 32), []byte(`not-json`)))
}

func TestE2EEReplayGivenTrailingUsageAfterCompletionThenKeepsTheTurnReference(t *testing.T) {
	p := newE2EEHistoryProjector("agent", time.Now)
	_, err := p.projectCommand("session", []byte(`{"id":"p","method":"session/prompt","params":{"prompt":[{"type":"text","text":"hello"}]}}`))
	require.NoError(t, err)
	turn := p.replayTurnID("session")
	require.NotEmpty(t, turn)
	_, err = p.projectFrames("session", []json.RawMessage{json.RawMessage(`{"id":"p","result":{"stopReason":"end_turn"}}`)})
	require.NoError(t, err)
	assert.Empty(t, p.activeTurnID("session"))
	assert.Equal(t, turn, p.replayTurnID("session"))
}

func TestE2EEReplayGivenAttachmentOnlyPromptThenIncludesItInReplay(t *testing.T) {
	assert.True(t, hasE2EEReplayPrompt([]byte(`{"method":"session/prompt","params":{"prompt":[],"paxEncryptedAttachments":[{"attachment_id":"file"}]}}`)))
	assert.False(t, hasE2EEReplayPrompt([]byte(`{"method":"session/prompt","params":{"prompt":[]}}`)))
}
