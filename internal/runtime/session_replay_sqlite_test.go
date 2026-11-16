package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentTunnelGivenSQLiteReplayDispatchFailureThenRestartPreservesOrderedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport.db")
	failure := errors.New("ACP stdin unavailable")
	writer := &failedACPInput{err: failure}
	first := newSQLiteReplaySession(t, path, writer)
	for seq := int64(1); seq <= 1010; seq++ {
		_, _, err := first.journal.SaveInboundIfAbsent(t.Context(), reliablemq.Frame{
			Key: replayFrameKey(seq), Kind: reliablemq.FrameKindData,
			Payload: []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt"}`, seq)),
		})
		require.NoError(t, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := first.session.replayInbound(ctx, first.engine)
	require.ErrorIs(t, err, failure)
	assert.Equal(t, 1, writer.calls)
	state, err := first.journal.LoadQueueState(t.Context(), "agent_1:queue_1", reliablemq.StreamACP)
	require.NoError(t, err)
	assert.Zero(t, state.InboundAppliedThrough)
	pending, err := first.journal.ListInboundReplay(t.Context(), "agent_1:queue_1", reliablemq.StreamACP, 1000)
	require.NoError(t, err)
	require.Len(t, pending, 1000)
	assert.Contains(t, pending[0].ErrorMessage, failure.Error())
	assert.Empty(t, pending[1].ErrorMessage)
	first.close()

	var output bytes.Buffer
	restarted := newSQLiteReplaySession(t, path, &output)
	require.NoError(t, restarted.session.replayInbound(t.Context(), restarted.engine))
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	require.Len(t, lines, 1010)
	for index, line := range lines {
		var request struct {
			ID int `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &request))
		require.Equal(t, index+1, request.ID)
	}
	state, err = restarted.journal.LoadQueueState(t.Context(), "agent_1:queue_1", reliablemq.StreamACP)
	require.NoError(t, err)
	assert.Equal(t, int64(1010), state.InboundAppliedThrough)
	assert.NoError(t, restarted.session.replayInbound(t.Context(), restarted.engine))
	assert.Len(t, strings.Split(strings.TrimSpace(output.String()), "\n"), 1010)
}

func TestAgentTunnelGivenWrappedSQLiteStoreThenGapCannotReachWorkerOutOfOrder(t *testing.T) {
	var output bytes.Buffer
	fixture := newSQLiteReplaySession(t, filepath.Join(t.TempDir(), "transport.db"), &output)
	connection := newFakeWebSocketConn()
	binding, err := fixture.producer.Bind(t.Context(), fixture.session.reliableSender(connection), 0)
	require.NoError(t, err)
	t.Cleanup(binding.Close)
	frame := func(seq int64) reliablemq.Envelope {
		return reliablemq.Envelope{
			Type: reliablemq.EnvelopeTypeData, QueueID: "agent_1:queue_1", Stream: reliablemq.StreamACP,
			Seq: seq, Payload: []byte(fmt.Sprintf(`{"id":%d,"method":"session/prompt"}`, seq)),
		}
	}
	require.NoError(t, fixture.engine.Receive(t.Context(), frame(2)))
	assert.Empty(t, output.String())
	require.NoError(t, fixture.engine.Receive(t.Context(), frame(1)))
	assert.Equal(t, "{\"id\":1,\"method\":\"session/prompt\"}\n{\"id\":2,\"method\":\"session/prompt\"}\n", output.String())
}

func TestAgentTunnelGivenInconsistentInboundHistoryThenFailClosedWithoutReconnectReset(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live_receive_%t", live), func(t *testing.T) {
			fixture := newSQLiteReplaySession(t, filepath.Join(t.TempDir(), "transport.db"), io.Discard)
			seed := func() {
				for seq := int64(1); seq <= 2; seq++ {
					_, _, err := fixture.journal.SaveInboundIfAbsent(t.Context(), reliablemq.Frame{
						Key: replayFrameKey(seq), Kind: reliablemq.FrameKindData, Payload: []byte(`{"id":1,"method":"session/prompt"}`),
					})
					require.NoError(t, err)
				}
				require.NoError(t, fixture.journal.MarkApplied(t.Context(), replayFrameKey(2)))
			}
			connection := newFakeWebSocketConn()
			enqueueAlignedReconcile(t, connection, "agent_1:queue_1", 0)
			process := newFakeProcess("")
			stdout, stdoutWriter := io.Pipe()
			process.stdout = stdout
			t.Cleanup(func() { _ = stdoutWriter.Close(); _ = stdout.Close() })
			if !live {
				seed()
			}
			session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
				Headers: fakeHeaderProvider{header: http.Header{}}, Dialer: &fakeDialer{conn: connection},
				LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: process},
				ReliableEngineFactory: fixture.session.deps.ReliableEngineFactory,
				Heartbeat:             HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour},
				SessionEventSink: SessionEventSinkFunc(func(event SessionEvent) {
					if live && event.Phase == PhaseRunning {
						seed()
						payload, err := reliablemq.MarshalEnvelope(reliablemq.Envelope{
							Type: reliablemq.EnvelopeTypeData, QueueID: "agent_1:queue_1", Stream: reliablemq.StreamACP,
							Seq: 3, Payload: []byte(`{"id":3,"method":"session/prompt"}`),
						})
						require.NoError(t, err)
						connection.readCh <- fakeWSMessage{messageType: websocketTextMessage, payload: payload}
					}
				}),
			})
			exit := session.Run(t.Context())
			assert.Equal(t, ExitConfig, exit.Class)
			assert.Equal(t, "transport_history_inconsistent", exit.Code)
			assert.False(t, exit.ResetBackoff)
			assert.Empty(t, process.stdin.String())
			frames, err := fixture.journal.ListInboundReplay(t.Context(), "agent_1:queue_1", reliablemq.StreamACP, 1000)
			require.NoError(t, err)
			require.NotEmpty(t, frames)
			assert.Equal(t, int64(1), frames[0].Key.Seq)
			assert.Equal(t, reliablemq.StatusReceived, frames[0].Status)
		})
	}
}

func TestAgentTransportFailureGivenDeliveryErrorThenPreserveRetryClassification(t *testing.T) {
	deliveryErr := errors.New("worker stdin unavailable")
	exit := classifyAgentTransportFailure("session_error", deliveryErr)
	assert.Equal(t, ExitTransient, exit.Class)
	assert.Equal(t, "session_error", exit.Code)
	assert.Equal(t, deliveryErr.Error(), exit.Message)
	wrapped := fmt.Errorf("replay failed: %w", &reliablemq.InboundHistoryError{
		QueueID: "queue", Stream: reliablemq.StreamACP, Seq: 1, AppliedThrough: 2,
	})
	exit = classifyAgentTransportFailure("transport_recovery_failed", wrapped)
	assert.Equal(t, ExitConfig, exit.Class)
	assert.Equal(t, "transport_history_inconsistent", exit.Code)
	assert.False(t, exit.ResetBackoff)
}

type sqliteReplaySession struct {
	journal  *sqlstore.Store
	session  *AgentTunnelSession
	engine   ReliableEngine
	producer *reliablemq.Producer
	close    func()
}

func newSQLiteReplaySession(t *testing.T, path string, stdin io.Writer) sqliteReplaySession {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	journal, err := sqlstore.NewSQLite(db, sqlstore.WithTableName("transport_journal"))
	require.NoError(t, err)
	wrapped := reliablemq.NewProducerWriteBehindStore(journal, reliablemq.WithProducerWriteBehindRequireBatchStore())
	factory := ReliableEngineFromStore(wrapped).(*reliableEngineFactory)
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{ReliableEngineFactory: factory})
	engine, producer, _, err := session.newReliableEngine(t.Context(), stdin)
	require.NoError(t, err)
	var once sync.Once
	close := func() {
		once.Do(func() {
			require.NoError(t, factory.Close(context.Background()))
			require.NoError(t, wrapped.Close(context.Background()))
			require.NoError(t, db.Close())
		})
	}
	t.Cleanup(close)
	return sqliteReplaySession{journal: journal, session: session, engine: engine, producer: producer, close: close}
}

func replayFrameKey(seq int64) reliablemq.FrameKey {
	return reliablemq.FrameKey{QueueID: "agent_1:queue_1", Stream: reliablemq.StreamACP, Seq: seq, Direction: reliablemq.DirectionInbound}
}

type failedACPInput struct {
	err   error
	calls int
}

func (w *failedACPInput) Write([]byte) (int, error) {
	w.calls++
	return 0, w.err
}
