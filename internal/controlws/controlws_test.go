package controlws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pax-beehive/paxd/internal/control"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/testkit/controltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandFrameMapsToControlAndWritesAck(t *testing.T) {
	request := controltest.LoadControlWSFrameRequest(t, "command_agent_connection_create_frame")
	response := controltest.LoadControlWSFrameResponse(t, "command_agent_connection_create_frame")
	cmd := controltest.LoadCommandRequest(t, "agent_connection_create")
	ack := controltest.LoadCommandResponse(t, "agent_connection_create")
	service := controltest.NewMockService(t)
	service.ExpectCommandFrom(remoteSource(), cmd).ReturnCommandAck(ack)
	conn := newFakeWebSocketConn(framePayload(t, request))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	assert.Equal(t, "node_control_read_failed", exit.Code)
	require.Len(t, conn.writes(), 1)
	assert.Equal(t, response, decodeWrittenFrame(t, conn.writes()[0]))
}

func TestQueryFrameMapsToControlAndWritesResponse(t *testing.T) {
	request := controltest.LoadControlWSFrameRequest(t, "query_remotes_list_frame")
	response := controltest.LoadControlWSFrameResponse(t, "query_remotes_list_frame")
	query := controltest.LoadQueryRequest(t, "remotes_list")
	result := controltest.LoadQueryResponse(t, "remotes_list")
	service := controltest.NewMockService(t)
	service.ExpectQueryFrom(remoteSource(), query).ReturnQueryResult(result)
	conn := newFakeWebSocketConn(framePayload(t, request))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	assert.Equal(t, response, decodeWrittenFrame(t, conn.writes()[0]))
}

func TestMalformedJSONWritesProtocolErrorAndSkipsService(t *testing.T) {
	service := controltest.NewMockService(t)
	conn := newFakeWebSocketConn([]byte(`{"kind":"command"`))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindError, got.Kind)
	require.NotNil(t, got.Error)
	assert.Equal(t, control.ErrCodeInvalidArgument, got.Error.Code)
	assert.Contains(t, got.Error.Message, "malformed JSON frame")
}

func TestCommandFrameMissingCommandIDIsRejectedBeforeService(t *testing.T) {
	service := controltest.NewMockService(t)
	conn := newFakeWebSocketConn([]byte(`{
		"kind": "command",
		"command": {
			"type": "remote.restart",
			"restart_remote": {"remote_id": "remote_prod"}
		}
	}`))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindError, got.Kind)
	require.NotNil(t, got.Error)
	assert.Equal(t, control.ErrCodeInvalidArgument, got.Error.Code)
	assert.Equal(t, "command_id", got.Error.Target)
}

func TestRejectsACPTunnelDataPlaneFrame(t *testing.T) {
	service := controltest.NewMockService(t)
	conn := newFakeWebSocketConn([]byte(`{"kind":"acp_data","payload":{"jsonrpc":"2.0"}}`))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindError, got.Kind)
	require.NotNil(t, got.Error)
	assert.Equal(t, "unsupported_frame", got.Error.Code)
}

func TestQueryServiceErrorWritesSafeErrorFrame(t *testing.T) {
	query := controltest.LoadQueryRequest(t, "remotes_list")
	service := controltest.NewMockService(t)
	service.ExpectQueryFrom(remoteSource(), query).ReturnError(errors.New("sql stack trace: secret-value"))
	conn := newFakeWebSocketConn(framePayload(t, IncomingFrame{
		Kind:      KindQuery,
		RequestID: "req_error_1",
		Query:     &query,
	}))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindError, got.Kind)
	assert.Equal(t, "req_error_1", got.RequestID)
	require.NotNil(t, got.Error)
	assert.Equal(t, control.ErrCodeInternal, got.Error.Code)
	assert.Equal(t, "control query failed", got.Error.Message)
	assert.NotContains(t, got.Error.Message, "secret-value")
}

func TestCommandServiceErrorWritesSafeErrorFrame(t *testing.T) {
	cmd := controltest.LoadCommandRequest(t, "agent_connection_create")
	service := controltest.NewMockService(t)
	service.ExpectCommandFrom(remoteSource(), cmd).ReturnError(errors.New("commit failed: secret-value"))
	conn := newFakeWebSocketConn(framePayload(t, IncomingFrame{Kind: KindCommand, CommandID: cmd.CommandID, Command: &cmd}))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindError, got.Kind)
	assert.Equal(t, cmd.CommandID, got.CommandID)
	require.NotNil(t, got.Error)
	assert.Equal(t, control.ErrCodeInternal, got.Error.Code)
	assert.Equal(t, "control command failed", got.Error.Message)
	assert.NotContains(t, got.Error.Message, "secret-value")
}

func TestCommandFrameStructuralErrorsAreRejectedBeforeService(t *testing.T) {
	cmd := controltest.LoadCommandRequest(t, "agent_connection_create")
	tests := []struct {
		name   string
		frame  IncomingFrame
		target string
	}{
		{
			name:   "missing command payload",
			frame:  IncomingFrame{Kind: KindCommand, CommandID: cmd.CommandID},
			target: "command",
		},
		{
			name:   "mismatched command id",
			frame:  IncomingFrame{Kind: KindCommand, CommandID: "cmd_other", Command: &cmd},
			target: "command_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := controltest.NewMockService(t)
			conn := newFakeWebSocketConn(framePayload(t, tt.frame))

			exit := Run(context.Background(), conn, remoteSource(), service)

			assert.Equal(t, runtimes.ExitTransient, exit.Class)
			require.Len(t, conn.writes(), 1)
			got := decodeWrittenFrame(t, conn.writes()[0])
			assert.Equal(t, KindError, got.Kind)
			require.NotNil(t, got.Error)
			assert.Equal(t, control.ErrCodeInvalidArgument, got.Error.Code)
			assert.Equal(t, tt.target, got.Error.Target)
		})
	}
}

func TestQueryFrameRequiresPayloadBeforeService(t *testing.T) {
	service := controltest.NewMockService(t)
	conn := newFakeWebSocketConn(framePayload(t, IncomingFrame{Kind: KindQuery, RequestID: "req_missing_query"}))

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindError, got.Kind)
	assert.Equal(t, "req_missing_query", got.RequestID)
	require.NotNil(t, got.Error)
	assert.Equal(t, "query", got.Error.Target)
}

func TestRunRejectsNonTextMessagesAndWriteFailuresEndSession(t *testing.T) {
	service := controltest.NewMockService(t)
	conn := newFakeWebSocketConnWithReads(fakeRead{messageType: websocket.BinaryMessage, payload: []byte(`[]`)})

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindError, got.Kind)
	require.NotNil(t, got.Error)
	assert.Equal(t, "message_type", got.Error.Target)

	writeErrConn := newFakeWebSocketConn([]byte(`{"kind":"not_supported"}`))
	writeErrConn.writeErr = errors.New("broken pipe")
	exit = Run(context.Background(), writeErrConn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	assert.Equal(t, "node_control_write_failed", exit.Code)
}

func TestRunRequiresConnectionAndService(t *testing.T) {
	exit := Run(context.Background(), nil, remoteSource(), controltest.NewMockService(t))
	assert.Equal(t, runtimes.ExitConfig, exit.Class)
	assert.Equal(t, "missing_websocket", exit.Code)

	exit = Run(context.Background(), newFakeWebSocketConn(), remoteSource(), nil)
	assert.Equal(t, runtimes.ExitConfig, exit.Class)
	assert.Equal(t, "missing_control_service", exit.Code)
}

func TestBestEffortCommandResultsAreWrittenWhenAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan control.CommandAck, 1)
	service := &resultWatchingService{results: results}
	conn := newBlockingFakeWebSocketConn()
	done := make(chan runtimes.Exit, 1)
	go func() {
		done <- Run(ctx, conn, remoteSource(), service)
	}()
	require.Eventually(t, conn.readStarted, 2*time.Second, 10*time.Millisecond)

	results <- control.CommandAck{
		CommandID: "cmd_agent_connection_create_1",
		OK:        true,
		Status:    control.CommandStatusApplied,
	}

	require.Eventually(t, func() bool {
		return len(conn.writes()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	got := decodeWrittenFrame(t, conn.writes()[0])
	assert.Equal(t, KindCommandResult, got.Kind)
	assert.Equal(t, "cmd_agent_connection_create_1", got.CommandID)
	require.NotNil(t, got.CommandAck)
	assert.Equal(t, control.CommandStatusApplied, got.CommandAck.Status)

	cancel()
	exit := <-done
	assert.Equal(t, runtimes.ExitTerminal, exit.Class)
	assert.Equal(t, "canceled", exit.Code)
}

func TestCommandResultWatcherUnavailableDoesNotBlockReadLoop(t *testing.T) {
	service := &resultWatchingService{watchErr: errors.New("watch unavailable")}
	conn := newFakeWebSocketConn()

	exit := Run(context.Background(), conn, remoteSource(), service)

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	assert.Empty(t, conn.writes())
}

func TestRunNodeControlAttributesRemoteSpec(t *testing.T) {
	cmd := controltest.LoadCommandRequest(t, "agent_connection_create")
	ack := controltest.LoadCommandResponse(t, "agent_connection_create")
	service := controltest.NewMockService(t)
	service.ExpectCommandFrom(remoteSource(), cmd).ReturnCommandAck(ack)
	conn := newFakeWebSocketConn(framePayload(t, IncomingFrame{Kind: KindCommand, CommandID: cmd.CommandID, Command: &cmd}))
	runner := NewRunner(service)

	exit := runner.RunNodeControl(context.Background(), conn, runtimes.RemoteSpec{RemoteID: "remote_prod"})

	assert.Equal(t, runtimes.ExitTransient, exit.Class)
	require.Len(t, conn.writes(), 1)
	assert.Equal(t, KindAck, decodeWrittenFrame(t, conn.writes()[0]).Kind)
}

func remoteSource() control.Source {
	return control.Source{Kind: control.SourceRemote, RemoteID: "remote_prod"}
}

func framePayload(t *testing.T, frame any) []byte {
	t.Helper()
	raw, err := json.Marshal(frame)
	require.NoError(t, err)
	return raw
}

func decodeWrittenFrame(t *testing.T, write writtenMessage) controltest.ControlWSFrame {
	t.Helper()
	assert.Equal(t, websocket.TextMessage, write.messageType)
	var frame controltest.ControlWSFrame
	require.NoError(t, json.Unmarshal(write.payload, &frame))
	return frame
}

type resultWatchingService struct {
	results  <-chan control.CommandAck
	watchErr error
}

func (s *resultWatchingService) HandleCommand(context.Context, control.Source, control.Command) (control.CommandAck, error) {
	return control.CommandAck{}, nil
}

func (s *resultWatchingService) HandleQuery(context.Context, control.Source, control.Query) (control.QueryResult, error) {
	return control.QueryResult{}, nil
}

func (s *resultWatchingService) WatchCommandResults(context.Context, control.Source) (<-chan control.CommandAck, error) {
	return s.results, s.watchErr
}

type fakeWebSocketConn struct {
	mu            sync.Mutex
	reads         []fakeRead
	writesLog     []writtenMessage
	writeErr      error
	blockReads    bool
	readStartedCh chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
}

type fakeRead struct {
	messageType int
	payload     []byte
	err         error
}

type writtenMessage struct {
	messageType int
	payload     []byte
}

func newFakeWebSocketConn(reads ...[]byte) *fakeWebSocketConn {
	fakeReads := make([]fakeRead, 0, len(reads))
	for _, read := range reads {
		fakeReads = append(fakeReads, fakeRead{messageType: websocket.TextMessage, payload: read})
	}
	return newFakeWebSocketConnWithReads(fakeReads...)
}

func newFakeWebSocketConnWithReads(reads ...fakeRead) *fakeWebSocketConn {
	return &fakeWebSocketConn{
		reads:         reads,
		readStartedCh: make(chan struct{}),
		closed:        make(chan struct{}),
	}
}

func newBlockingFakeWebSocketConn() *fakeWebSocketConn {
	conn := newFakeWebSocketConn()
	conn.blockReads = true
	return conn
}

func (c *fakeWebSocketConn) ReadMessage() (int, []byte, error) {
	c.signalReadStarted()
	c.mu.Lock()
	if len(c.reads) > 0 {
		read := c.reads[0]
		c.reads = c.reads[1:]
		c.mu.Unlock()
		return read.messageType, append([]byte(nil), read.payload...), read.err
	}
	block := c.blockReads
	c.mu.Unlock()
	if block {
		<-c.closed
		return 0, nil, context.Canceled
	}
	return 0, nil, io.EOF
}

func (c *fakeWebSocketConn) WriteMessage(messageType int, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.writesLog = append(c.writesLog, writtenMessage{
		messageType: messageType,
		payload:     append([]byte(nil), payload...),
	})
	return nil
}

func (c *fakeWebSocketConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeWebSocketConn) writes() []writtenMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]writtenMessage(nil), c.writesLog...)
}

func (c *fakeWebSocketConn) signalReadStarted() {
	defer func() {
		_ = recover()
	}()
	close(c.readStartedCh)
}

func (c *fakeWebSocketConn) readStarted() bool {
	select {
	case <-c.readStartedCh:
		return true
	default:
		return false
	}
}
