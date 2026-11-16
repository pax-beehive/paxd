package acpforwarder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/store"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwarderGivenSequenceGapThenWaitForMissingInput(t *testing.T) {
	journal := openTestJournal(t)
	t.Cleanup(func() { _ = journal.Close() })
	svc := New(Config{ConnectionID: "conn_1", AgentID: "agent_1", Journal: journal})
	var stdin bytes.Buffer
	engine, _, closeEngine, err := svc.newReliableEngine(&stdin)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeEngine(context.Background())) })

	// A later request must not reach the worker before its missing predecessor.
	require.NoError(t, engine.Receive(t.Context(), forwarderInput(2)))
	assert.Empty(t, stdin.String())
	second := mustGetReliableFrame(t, journal, forwarderInputKey(2))
	assert.Equal(t, reliablemq.StatusReceived, second.Status)

	require.NoError(t, engine.Receive(t.Context(), forwarderInput(1)))
	assert.Equal(t, "{\"id\":1}\n{\"id\":2}\n", stdin.String())
	state, err := openReliableStore(t, journal).LoadQueueState(t.Context(), "conn_1", reliablemq.StreamACP)
	require.NoError(t, err)
	assert.Equal(t, int64(2), state.InboundAppliedThrough)
}

func TestForwarderGivenFailedReplayBatchThenStopAndRecoverAfterSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paxd.db")
	journal, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = journal.Close() })
	mq := openReliableStore(t, journal)
	batch := reliablemq.StoreBatch{}
	const count = 1010
	for seq := int64(1); seq <= count; seq++ {
		batch.Frames = append(batch.Frames, reliablemq.Frame{
			Key: forwarderInputKey(seq), Kind: reliablemq.FrameKindData,
			Payload: forwarderInput(seq).Payload, Status: reliablemq.StatusReceived,
		})
	}
	require.NoError(t, mq.ApplyBatch(t.Context(), batch))
	svc := New(Config{ConnectionID: "conn_1", AgentID: "agent_1", Journal: journal})
	stdin := &countingFailedInput{}
	engine, _, closeEngine, err := svc.newReliableEngine(stdin)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	// A full failed batch must return the worker error, rather than spin or advance.
	err = engine.ReplayInbound(ctx, "conn_1", reliablemq.StreamACP, 1000)
	assert.ErrorIs(t, err, errTestWriteFailure)
	assert.Equal(t, 1, stdin.calls)
	state, err := mq.LoadQueueState(t.Context(), "conn_1", reliablemq.StreamACP)
	require.NoError(t, err)
	assert.Zero(t, state.InboundAppliedThrough)
	first := mustGetReliableFrame(t, journal, forwarderInputKey(1))
	assert.Equal(t, reliablemq.StatusReceived, first.Status)
	assert.Contains(t, first.ErrorMessage, "write acp stdin")
	last := mustGetReliableFrame(t, journal, forwarderInputKey(count))
	assert.Equal(t, reliablemq.StatusReceived, last.Status)
	require.NoError(t, closeEngine(t.Context()))
	require.NoError(t, journal.Close())

	// Reopening the actual database retries every retained request in order.
	journal, err = store.Open(path)
	require.NoError(t, err)
	svc = New(Config{ConnectionID: "conn_1", AgentID: "agent_1", Journal: journal})
	var recovered bytes.Buffer
	engine, _, closeEngine, err = svc.newReliableEngine(&recovered)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeEngine(context.Background())) })
	require.NoError(t, engine.ReplayInbound(t.Context(), "conn_1", reliablemq.StreamACP, 1000))
	lines := strings.Split(strings.TrimSpace(recovered.String()), "\n")
	require.Len(t, lines, count)
	for offset, line := range lines {
		require.Equal(t, fmt.Sprintf(`{"id":%d}`, offset+1), line)
	}
	state, err = openReliableStore(t, journal).LoadQueueState(t.Context(), "conn_1", reliablemq.StreamACP)
	require.NoError(t, err)
	assert.Equal(t, int64(count), state.InboundAppliedThrough)
}

type countingFailedInput struct{ calls int }

func (w *countingFailedInput) Write([]byte) (int, error) {
	w.calls++
	return 0, errTestWriteFailure
}

func forwarderInput(seq int64) reliablemq.Envelope {
	return reliablemq.Envelope{
		Type: reliablemq.EnvelopeTypeData, QueueID: "conn_1", Stream: reliablemq.StreamACP,
		Seq: seq, Payload: json.RawMessage(fmt.Sprintf(`{"id":%d}`, seq)),
	}
}

func forwarderInputKey(seq int64) reliablemq.FrameKey {
	return reliablemq.FrameKey{QueueID: "conn_1", Stream: reliablemq.StreamACP, Seq: seq, Direction: reliablemq.DirectionInbound}
}
