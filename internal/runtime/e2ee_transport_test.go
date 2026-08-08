package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EEBridgeDecryptsDeduplicatesAndAcknowledgesCommands(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	receipts := &fakeE2EECommandStore{seen: make(map[string]bool)}
	var sent [][]byte
	bridge := newE2EETransportBridge(rootKey, "agent_1", "queue_1", receipts,
		func(_ context.Context, payload []byte, _ reliablemq.Metadata) error {
			sent = append(sent, append([]byte(nil), payload...))
			return nil
		})
	command := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"native_1","prompt":[{"type":"text","text":"secret"}]}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1",
		Kind: "acp_command", KeyEpoch: 1,
	}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	frame := reliablemq.Frame{Payload: payload, Metadata: reliablemq.Metadata{
		"command_id": "cmd_1", "connection_epoch": "3",
	}}
	dispatched := 0
	dispatch := func(_ context.Context, managerSessionID string, got []byte) (string, error) {
		dispatched++
		assert.Equal(t, "session_1", managerSessionID)
		assert.Equal(t, command, got)
		return "native_1", nil
	}

	handled, err := bridge.handleCommand(context.Background(), frame, dispatch)
	require.NoError(t, err)
	assert.True(t, handled)
	handled, err = bridge.handleCommand(context.Background(), frame, dispatch)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, 1, dispatched)
	var acknowledgements int
	for _, ackPayload := range sent {
		var ack map[string]any
		require.NoError(t, json.Unmarshal(ackPayload, &ack))
		if ack["type"] != "e2ee_command_ack" {
			continue
		}
		acknowledgements++
		assert.Equal(t, "e2ee_command_ack", ack["type"])
		assert.Equal(t, "cmd_1", ack["command_id"])
	}
	assert.Equal(t, 2, acknowledgements)
}

func TestE2EEBridgeBatchesStreamingFramesAndFlushesBoundary(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	receipts := &fakeE2EECommandStore{seen: make(map[string]bool)}
	var mu sync.Mutex
	var sent [][]byte
	bridge := newE2EETransportBridge(rootKey, "agent_1", "queue_1", receipts,
		func(_ context.Context, payload []byte, _ reliablemq.Metadata) error {
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, append([]byte(nil), payload...))
			return nil
		})
	command := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"native_1"}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1",
		Kind: "acp_command", KeyEpoch: 9,
	}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload:  payload,
		Metadata: reliablemq.Metadata{"command_id": "cmd_1", "connection_epoch": "1"},
	}, func(context.Context, string, []byte) (string, error) { return "native_1", nil })
	require.NoError(t, err)

	for _, output := range [][]byte{
		[]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"a"}}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"b"}}}}`),
		[]byte(`{"jsonrpc":"2.0","id":"prompt_1","result":{"stopReason":"end_turn"}}`),
	} {
		handled, err := bridge.sendOutput(context.Background(), "native_1", output)
		require.NoError(t, err)
		assert.True(t, handled)
	}

	mu.Lock()
	var eventPayload []byte
	for _, candidate := range sent {
		var envelope e2ee.Envelope
		if json.Unmarshal(candidate, &envelope) == nil && envelope.Kind == "acp_event" {
			eventPayload = append([]byte(nil), candidate...)
			break
		}
	}
	mu.Unlock()
	require.NotEmpty(t, eventPayload)
	var event e2ee.Envelope
	require.NoError(t, json.Unmarshal(eventPayload, &event))
	assert.Equal(t, "acp_event", event.Kind)
	plaintext, err := e2ee.Decrypt(rootKey, e2ee.DirectionEvent, event)
	require.NoError(t, err)
	var batch struct {
		Frames []json.RawMessage `json:"frames"`
	}
	require.NoError(t, json.Unmarshal(plaintext, &batch))
	require.Len(t, batch.Frames, 3)
	assert.Contains(t, string(batch.Frames[0]), `"text":"a"`)
	assert.Contains(t, string(batch.Frames[1]), `"text":"b"`)
	assert.Contains(t, string(batch.Frames[2]), "end_turn")
}

func TestE2EEBridgeGivenPromptAndResponseWhenCanonicalHistoryIsSentThenEncryptsCleanRevisionedRecords(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	type sentRecord struct {
		payload  []byte
		metadata reliablemq.Metadata
	}
	var sent []sentRecord
	bridge := newE2EETransportBridge(
		rootKey,
		"agent_1",
		"queue_1",
		&fakeE2EECommandStore{seen: make(map[string]bool)},
		func(_ context.Context, payload []byte, metadata reliablemq.Metadata) error {
			if metadata["e2ee_kind"] == "history" {
				sent = append(sent, sentRecord{
					payload: append([]byte(nil), payload...), metadata: metadata,
				})
			}
			return nil
		},
	)
	command := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"prompt":[{"type":"text","text":"secret prompt"}]}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1",
		Kind: "acp_command", KeyEpoch: 3,
	}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload: payload, Metadata: reliablemq.Metadata{
			"command_id": "cmd_1", "connection_epoch": "2",
		},
	}, func(context.Context, string, []byte) (string, error) { return "native_1", nil })
	require.NoError(t, err)
	_, err = bridge.sendOutput(context.Background(), "native_1", []byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"secret answer"}}}}`,
	))
	require.NoError(t, err)
	_, err = bridge.sendOutput(context.Background(), "native_1", []byte(
		`{"jsonrpc":"2.0","id":"prompt_1","result":{"stopReason":"end_turn"}}`,
	))
	require.NoError(t, err)

	require.NotEmpty(t, sent)
	kinds := make(map[string]int)
	for _, record := range sent {
		var encrypted e2ee.Envelope
		require.NoError(t, json.Unmarshal(record.payload, &encrypted))
		assert.NotContains(t, string(record.payload), "secret")
		plaintext, decryptErr := e2ee.Decrypt(rootKey, e2ee.DirectionEvent, encrypted)
		require.NoError(t, decryptErr)
		var canonical struct {
			MessageID string `json:"message_id"`
			Revision  int64  `json:"revision"`
		}
		require.NoError(t, json.Unmarshal(plaintext, &canonical))
		assert.Equal(t, record.metadata["message_id"], canonical.MessageID)
		assert.Equal(t, record.metadata["revision"], fmt.Sprint(canonical.Revision))
		kinds[encrypted.Kind]++
	}
	assert.Positive(t, kinds["e2ee_message"])
	assert.Positive(t, kinds["e2ee_message_part"])
}

func TestE2EEBridgeEncryptsSessionNewResponseBeforeNativeRouteExists(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	var sent [][]byte
	bridge := newE2EETransportBridge(rootKey, "agent_1", "queue_1", &fakeE2EECommandStore{seen: make(map[string]bool)},
		func(_ context.Context, payload []byte, metadata reliablemq.Metadata) error {
			if metadata["e2ee_kind"] == "event" {
				sent = append(sent, append([]byte(nil), payload...))
			}
			return nil
		})
	command := []byte(`{"jsonrpc":"2.0","id":"new_1","method":"session/new","params":{"cwd":"/tmp/project"}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd_new", AgentID: "agent_1", SessionID: "session_1",
		Kind: "acp_command", KeyEpoch: 4,
	}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload: payload, Metadata: reliablemq.Metadata{"command_id": "cmd_new", "connection_epoch": "2"},
	}, func(context.Context, string, []byte) (string, error) { return "", nil })
	require.NoError(t, err)

	handled, err := bridge.sendOutput(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":"new_1","result":{"sessionId":"native_1"}}`))
	require.NoError(t, err)
	assert.True(t, handled)
	require.Len(t, sent, 1)
	var event e2ee.Envelope
	require.NoError(t, json.Unmarshal(sent[0], &event))
	assert.Equal(t, int64(4), event.KeyEpoch)
	assert.Equal(t, "session_1", event.SessionID)
}

func TestE2EEBridgeRestoresBatchWhenReliableSendFails(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	failed := false
	var sent []byte
	bridge := newE2EETransportBridge(rootKey, "agent_1", "queue_1", &fakeE2EECommandStore{seen: make(map[string]bool)},
		func(_ context.Context, payload []byte, metadata reliablemq.Metadata) error {
			if metadata["e2ee_kind"] != "event" {
				return nil
			}
			if !failed {
				failed = true
				return errors.New("journal unavailable")
			}
			sent = append([]byte(nil), payload...)
			return nil
		})
	command := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"native_1"}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1",
		Kind: "acp_command", KeyEpoch: 1,
	}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload: payload, Metadata: reliablemq.Metadata{"command_id": "cmd_1", "connection_epoch": "1"},
	}, func(context.Context, string, []byte) (string, error) { return "native_1", nil })
	require.NoError(t, err)

	output := []byte(`{"jsonrpc":"2.0","id":"prompt_1","result":{"stopReason":"end_turn"}}`)
	handled, err := bridge.sendOutput(context.Background(), "native_1", output)
	require.ErrorContains(t, err, "journal unavailable")
	assert.True(t, handled)
	require.NoError(t, bridge.flush(context.Background(), "session_1"))
	require.NotEmpty(t, sent)
	var event e2ee.Envelope
	require.NoError(t, json.Unmarshal(sent, &event))
	plaintext, err := e2ee.Decrypt(rootKey, e2ee.DirectionEvent, event)
	require.NoError(t, err)
	assert.Contains(t, string(plaintext), "end_turn")
}

func TestE2EEBridgeRejectsInvalidCommandsAndRetriesDispatchFailure(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	command := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"native_1"}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1",
		Kind: "acp_command", KeyEpoch: 1,
	}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)

	handled, err := (*e2eeTransportBridge)(nil).handleCommand(
		context.Background(), reliablemq.Frame{Payload: payload}, nil,
	)
	require.ErrorContains(t, err, "no root key")
	assert.True(t, handled)

	store := &fakeE2EECommandStore{seen: make(map[string]bool)}
	bridge := newE2EETransportBridge(rootKey, "agent_1", "queue_1", store,
		func(context.Context, []byte, reliablemq.Metadata) error { return nil })
	handled, err = bridge.handleCommand(
		context.Background(), reliablemq.Frame{Payload: []byte("not-json")}, nil,
	)
	require.NoError(t, err)
	assert.False(t, handled)

	wrongRoute := envelope
	wrongRoute.AgentID = "agent_2"
	wrongPayload, err := json.Marshal(wrongRoute)
	require.NoError(t, err)
	handled, err = bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload: wrongPayload, Metadata: reliablemq.Metadata{"connection_epoch": "1"},
	}, nil)
	require.ErrorContains(t, err, "route")
	assert.True(t, handled)

	_, err = bridge.handleCommand(context.Background(), reliablemq.Frame{Payload: payload}, nil)
	require.ErrorContains(t, err, "connection epoch")
	_, err = bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload: payload, Metadata: reliablemq.Metadata{"connection_epoch": "1", "command_id": "other"},
	}, nil)
	require.ErrorContains(t, err, "id metadata mismatch")

	dispatches := 0
	frame := reliablemq.Frame{Payload: payload, Metadata: reliablemq.Metadata{
		"connection_epoch": "1", "command_id": "cmd_1",
	}}
	_, err = bridge.handleCommand(context.Background(), frame, func(context.Context, string, []byte) (string, error) {
		dispatches++
		return "", errors.New("local ACP unavailable")
	})
	require.ErrorContains(t, err, "local ACP unavailable")
	_, err = bridge.handleCommand(context.Background(), frame, func(context.Context, string, []byte) (string, error) {
		dispatches++
		return "native_1", nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, dispatches, "an incomplete receipt must not suppress retry")
}

func TestE2EEBridgeCloseFlushesPendingStreamingBatch(t *testing.T) {
	t.Parallel()
	rootKey := make([]byte, 32)
	var events int
	bridge := newE2EETransportBridge(rootKey, "agent_1", "queue_1", &fakeE2EECommandStore{seen: make(map[string]bool)},
		func(_ context.Context, _ []byte, metadata reliablemq.Metadata) error {
			if metadata["e2ee_kind"] == "event" {
				events++
			}
			return nil
		})
	command := []byte(`{"jsonrpc":"2.0","id":"prompt_1","method":"session/prompt","params":{"sessionId":"native_1"}}`)
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionCommand, e2ee.Metadata{
		RecordID: "cmd_1", AgentID: "agent_1", SessionID: "session_1", Kind: "acp_command", KeyEpoch: 1,
	}, command)
	require.NoError(t, err)
	payload, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = bridge.handleCommand(context.Background(), reliablemq.Frame{
		Payload: payload, Metadata: reliablemq.Metadata{"command_id": "cmd_1", "connection_epoch": "1"},
	}, func(context.Context, string, []byte) (string, error) { return "native_1", nil })
	require.NoError(t, err)
	handled, err := bridge.sendOutput(context.Background(), "native_1", []byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native_1","update":{"sessionUpdate":"agent_message_chunk"}}}`,
	))
	require.NoError(t, err)
	assert.True(t, handled)
	require.NoError(t, bridge.close(context.Background()))
	assert.Equal(t, 1, events)
	_, err = bridge.sendOutput(context.Background(), "native_1", []byte(`{"jsonrpc":"2.0","id":"prompt_1","result":{}}`))
	require.ErrorContains(t, err, "closed")
}

func TestE2EEBoundaryFrameClassification(t *testing.T) {
	t.Parallel()
	assert.False(t, e2eeBoundaryFrame([]byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_delta"}}}`,
	)))
	assert.True(t, e2eeBoundaryFrame([]byte(
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call"}}}`,
	)))
	assert.True(t, e2eeBoundaryFrame([]byte(`{"jsonrpc":"2.0","method":"session/request_permission"}`)))
	assert.True(t, e2eeBoundaryFrame([]byte("invalid")))
}

type fakeE2EECommandStore struct {
	mu        sync.Mutex
	seen      map[string]bool
	completed map[string]bool
}

func (s *fakeE2EECommandStore) BeginE2EECommand(
	_ context.Context,
	_ string,
	commandID string,
	_ int64,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed[commandID] {
		return false, nil
	}
	s.seen[commandID] = true
	return true, nil
}

func (s *fakeE2EECommandStore) CompleteE2EECommand(_ context.Context, _ string, commandID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed == nil {
		s.completed = make(map[string]bool)
	}
	s.completed[commandID] = true
	return nil
}
