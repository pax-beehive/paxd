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

func TestHeartbeatReportFrameHasNoClientTTL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	service := &reportingService{}
	conn := newBlockingFakeWebSocketConn()
	runner := Runner{
		Service: service,
		Reports: ReportOptions{
			HeartbeatInterval: time.Millisecond,
			Now:               func() time.Time { return now },
			NewID:             fixedReportID("rpt_heartbeat"),
		},
	}
	done := make(chan runtimes.Exit, 1)
	go func() {
		done <- runner.RunNodeControl(ctx, conn, runtimes.RemoteSpec{RemoteID: "remote_prod", NodeID: "node_123"})
	}()

	require.Eventually(t, func() bool {
		return len(conn.writes()) >= 1
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	got := decodeReportFrame(t, conn.writes()[0])
	assert.Equal(t, KindReport, got.Kind)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, "rpt_heartbeat", got.ReportID)
	assert.Equal(t, control.ReportHeartbeat, got.Report.Type)
	assert.Equal(t, "remote_prod", got.Report.RemoteID)
	assert.Equal(t, "node_123", got.Report.NodeID)
	assert.Equal(t, "2026-06-24T12:00:00Z", got.Report.SentAt)
	require.NotNil(t, got.Report.Heartbeat)
	assert.Nil(t, got.Report.RuntimeSnapshot)
	raw := string(conn.writes()[0].payload)
	assert.NotContains(t, raw, "lease_ttl")
	assert.NotContains(t, raw, "ttl")
}

func TestInitialRuntimeSnapshotReportUsesControlLayer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 6, 24, 12, 0, 3, 0, time.UTC)
	service := &reportingService{
		snapshot: control.RuntimeSnapshotReport{
			SnapshotID: "snap_1",
			Host: &control.HostMetricsReport{
				MachineName:   "MacBook Pro",
				OS:            "darwin",
				Arch:          "amd64",
				CPUPercent:    21.4,
				MemoryPercent: 63.2,
				UptimeSeconds: 80422,
				CollectedAt:   "2026-06-24T12:00:02Z",
			},
			Agents: []control.AgentRuntimeReport{{
				ConnectionID: "conn_codex",
				CloudAgentID: "agent_123",
				RemoteID:     "remote_prod",
				NodeID:       "node_123",
				RuntimePhase: "running",
			}},
		},
	}
	conn := newBlockingFakeWebSocketConn()
	runner := Runner{
		Service: service,
		Reports: ReportOptions{
			SendInitialSnapshot: true,
			Now:                 func() time.Time { return now },
			NewID:               fixedReportID("rpt_snapshot"),
		},
	}
	done := make(chan runtimes.Exit, 1)
	go func() {
		done <- runner.RunNodeControl(ctx, conn, runtimes.RemoteSpec{RemoteID: "remote_prod", NodeID: "node_123"})
	}()

	require.Eventually(t, func() bool {
		return len(conn.writes()) >= 1
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, []string{"remote_prod"}, service.snapshotRemoteIDs)
	assert.Equal(t, []string{"node_123"}, service.snapshotNodeIDs)
	got := decodeReportFrame(t, conn.writes()[0])
	assert.Equal(t, control.ReportRuntimeSnapshot, got.Report.Type)
	require.NotNil(t, got.Report.RuntimeSnapshot)
	assert.Equal(t, "snap_1", got.Report.RuntimeSnapshot.SnapshotID)
	require.NotNil(t, got.Report.RuntimeSnapshot.Host)
	assert.Equal(t, "MacBook Pro", got.Report.RuntimeSnapshot.Host.MachineName)
	assert.Equal(t, "darwin", got.Report.RuntimeSnapshot.Host.OS)
	assert.Equal(t, "amd64", got.Report.RuntimeSnapshot.Host.Arch)
	require.NotNil(t, got.Report.RuntimeSnapshot.Host)
	assert.Equal(t, 21.4, got.Report.RuntimeSnapshot.Host.CPUPercent)
	require.Len(t, got.Report.RuntimeSnapshot.Agents, 1)
	assert.Equal(t, "conn_codex", got.Report.RuntimeSnapshot.Agents[0].ConnectionID)
}

func TestInitialSnapshotDoesNotBlockReadLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	block := make(chan struct{})
	service := &reportingService{blockSnapshot: block}
	conn := newBlockingFakeWebSocketConn()
	runner := Runner{
		Service: service,
		Reports: ReportOptions{
			SendInitialSnapshot: true,
			NewID:               fixedReportID("rpt_blocked_snapshot"),
		},
	}
	done := make(chan runtimes.Exit, 1)
	go func() {
		done <- runner.RunNodeControl(ctx, conn, runtimes.RemoteSpec{RemoteID: "remote_prod", NodeID: "node_123"})
	}()

	require.Eventually(t, conn.readStarted, 2*time.Second, 10*time.Millisecond)
	cancel()
	close(block)
	<-done
	assert.Empty(t, conn.writes())
}

func TestStatusPokeSendsDebouncedRuntimeSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pokes := make(chan struct{}, 2)
	service := &reportingService{
		snapshot: control.RuntimeSnapshotReport{SnapshotID: "snap_poke"},
	}
	conn := newBlockingFakeWebSocketConn()
	runner := Runner{
		Service: service,
		Reports: ReportOptions{
			StatusSubscribe: func(remoteID string) (<-chan struct{}, func()) {
				assert.Equal(t, "remote_prod", remoteID)
				return pokes, func() {}
			},
			PokeDebounce: time.Millisecond,
			NewID:        fixedReportID("rpt_poke"),
		},
	}
	done := make(chan runtimes.Exit, 1)
	go func() {
		done <- runner.RunNodeControl(ctx, conn, runtimes.RemoteSpec{RemoteID: "remote_prod", NodeID: "node_123"})
	}()
	require.Eventually(t, conn.readStarted, 2*time.Second, 10*time.Millisecond)

	pokes <- struct{}{}
	pokes <- struct{}{}

	require.Eventually(t, func() bool {
		return len(conn.writes()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	got := decodeReportFrame(t, conn.writes()[0])
	assert.Equal(t, control.ReportRuntimeSnapshot, got.Report.Type)
	require.NotNil(t, got.Report.RuntimeSnapshot)
	assert.Equal(t, "snap_poke", got.Report.RuntimeSnapshot.SnapshotID)
}

func TestAttachmentStatePushSendsReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	states := make(chan control.AttachmentLocalState, 1)
	service := &reportingService{}
	conn := newBlockingFakeWebSocketConn()
	runner := Runner{
		Service: service,
		Reports: ReportOptions{
			AttachmentSubscribe: func(remoteID string) (<-chan control.AttachmentLocalState, func()) {
				assert.Equal(t, "remote_prod", remoteID)
				return states, func() {}
			},
			NewID: fixedReportID("rpt_attachment"),
		},
	}
	done := make(chan runtimes.Exit, 1)
	go func() {
		done <- runner.RunNodeControl(ctx, conn, runtimes.RemoteSpec{RemoteID: "remote_prod", NodeID: "node_123"})
	}()
	require.Eventually(t, conn.readStarted, 2*time.Second, 10*time.Millisecond)

	states <- control.AttachmentLocalState{
		AttachmentID:    "att_1",
		State:           control.AttachmentLocalDownloading,
		BytesDownloaded: 1024,
		TotalBytes:      4096,
	}

	require.Eventually(t, func() bool {
		return len(conn.writes()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	got := decodeReportFrame(t, conn.writes()[0])
	assert.Equal(t, control.ReportAttachmentLocalState, got.Report.Type)
	require.NotNil(t, got.Report.AttachmentLocalState)
	assert.Equal(t, "att_1", got.Report.AttachmentLocalState.AttachmentID)
	assert.Equal(t, int64(1024), got.Report.AttachmentLocalState.BytesDownloaded)
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

func decodeReportFrame(t *testing.T, write writtenMessage) ReportFrame {
	t.Helper()
	assert.Equal(t, websocket.TextMessage, write.messageType)
	var frame ReportFrame
	require.NoError(t, json.Unmarshal(write.payload, &frame))
	return frame
}

func fixedReportID(id string) func(string) string {
	return func(string) string { return id }
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

type reportingService struct {
	snapshot          control.RuntimeSnapshotReport
	snapshotErr       error
	blockSnapshot     <-chan struct{}
	snapshotRemoteIDs []string
	snapshotNodeIDs   []string
}

func (s *reportingService) HandleCommand(context.Context, control.Source, control.Command) (control.CommandAck, error) {
	return control.CommandAck{}, nil
}

func (s *reportingService) HandleQuery(context.Context, control.Source, control.Query) (control.QueryResult, error) {
	return control.QueryResult{}, nil
}

func (s *reportingService) BuildRuntimeSnapshot(ctx context.Context, remoteID, nodeID string) (control.RuntimeSnapshotReport, error) {
	s.snapshotRemoteIDs = append(s.snapshotRemoteIDs, remoteID)
	s.snapshotNodeIDs = append(s.snapshotNodeIDs, nodeID)
	if s.blockSnapshot != nil {
		select {
		case <-ctx.Done():
			return control.RuntimeSnapshotReport{}, ctx.Err()
		case <-s.blockSnapshot:
		}
	}
	if s.snapshotErr != nil {
		return control.RuntimeSnapshotReport{}, s.snapshotErr
	}
	return s.snapshot, nil
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
