package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteControlSessionDialsWithAuthHeadersAndDelegates(t *testing.T) {
	conn := newFakeWebSocketConn()
	headers := http.Header{"X-Pax-Key": []string{"pax_key"}}
	dialer := &fakeDialer{conn: conn}
	var runnerSpec RemoteSpec
	var runnerConn WebSocketConn
	runner := NodeControlRunnerFunc(func(ctx context.Context, got WebSocketConn, spec RemoteSpec) Exit {
		runnerConn = got
		runnerSpec = spec
		return TransientExit("peer_closed", "peer closed")
	})

	session := NewRemoteControlSession(RemoteControlSessionConfig{
		Spec: RemoteSpec{
			RemoteID:    "remote_prod",
			NodeID:      "node_1",
			CloudAPIURL: "https://fleet.example.com/base",
		},
		Headers:   fakeHeaderProvider{header: headers},
		Dialer:    dialer,
		Runner:    runner,
		Heartbeat: HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour},
	})

	exit := session.Run(context.Background())
	assert.Equal(t, ExitTransient, exit.Class)
	assert.Equal(t, "wss://fleet.example.com/base/api/v1/node/control?node_id=node_1", dialer.url)
	assert.Equal(t, "pax_key", dialer.header.Get("X-Pax-Key"))
	assert.Equal(t, "remote_prod", runnerSpec.RemoteID)
	assert.NotNil(t, runnerConn)
	assert.True(t, conn.closed)
}

func TestRemoteControlSessionUnauthorizedHandshakeIsAuthExit(t *testing.T) {
	session := NewRemoteControlSession(RemoteControlSessionConfig{
		Spec:    RemoteSpec{RemoteID: "remote_prod", CloudAPIURL: "https://fleet.example.com"},
		Headers: fakeHeaderProvider{header: http.Header{}},
		Dialer: &fakeDialer{
			err:  errors.New("bad handshake"),
			resp: &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(""))},
		},
		Runner: NodeControlRunnerFunc(func(ctx context.Context, conn WebSocketConn, spec RemoteSpec) Exit {
			require.Fail(t, "runner should not be called")
			return Exit{}
		}),
	})

	exit := session.Run(context.Background())
	assert.Equal(t, ExitAuth, exit.Class)
	assert.Equal(t, "unauthorized", exit.Code)
}

func TestRemoteControlSessionDialClassification(t *testing.T) {
	forbidden := classifyDialExit(errors.New("bad handshake"), &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader("")),
	})
	assert.Equal(t, ExitAuth, forbidden.Class)
	assert.Equal(t, "access_denied", forbidden.Code)

	canceled := classifyDialExit(context.Canceled, nil)
	assert.Equal(t, ExitTerminal, canceled.Class)
	assert.Equal(t, "canceled", canceled.Code)

	transient := classifyDialExit(errors.New("i/o timeout"), nil)
	assert.Equal(t, ExitTransient, transient.Class)
	assert.Equal(t, "dial_failed", transient.Code)
}

func TestRemoteControlSessionHeartbeatTimeoutReturnsTransient(t *testing.T) {
	conn := newFakeWebSocketConn()
	session := NewRemoteControlSession(RemoteControlSessionConfig{
		Spec:      RemoteSpec{RemoteID: "remote_prod", CloudAPIURL: "https://fleet.example.com"},
		Headers:   fakeHeaderProvider{header: http.Header{}},
		Dialer:    &fakeDialer{conn: conn},
		Heartbeat: HeartbeatConfig{PingInterval: 5 * time.Millisecond, ReadTimeout: 15 * time.Millisecond},
		Runner: NodeControlRunnerFunc(func(ctx context.Context, conn WebSocketConn, spec RemoteSpec) Exit {
			_, _, err := conn.ReadMessage()
			require.Error(t, err)
			return TransientExit("read_failed", err.Error())
		}),
	})

	exit := session.Run(context.Background())
	assert.Equal(t, ExitTransient, exit.Class)
	assert.Equal(t, "heartbeat_timeout", exit.Code)
}

func TestHeartbeatReadMessageMarksActivity(t *testing.T) {
	conn := newFakeWebSocketConn()
	hb := newHeartbeatConn(conn, HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour})
	conn.readCh <- fakeWSMessage{messageType: websocketTextMessage, payload: []byte(`{"ok":true}`)}

	messageType, payload, err := hb.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocketTextMessage, messageType)
	assert.JSONEq(t, `{"ok":true}`, string(payload))
}

func TestHeartbeatPongMarksActivity(t *testing.T) {
	conn := newFakeWebSocketConn()
	hb := newHeartbeatConn(conn, HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: 30 * time.Millisecond})
	hb.Start()
	defer hb.Close()

	time.Sleep(20 * time.Millisecond)
	require.NoError(t, conn.pong("heartbeat"))
	time.Sleep(20 * time.Millisecond)

	assert.False(t, hb.TimedOut())
}

func TestAgentTunnelSessionMissingCloudAgentIDIsConfigExit(t *testing.T) {
	dialer := &fakeDialer{conn: newFakeWebSocketConn()}
	session := NewAgentTunnelSession(AgentConnectionSpec{
		ConnectionID: "conn_1",
		RemoteID:     "remote_prod",
		CloudAPIURL:  "https://fleet.example.com",
		Command:      []string{"codex", "acp"},
	}, AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                dialer,
		ReliableEngineFactory: ReliableEngineFromStore(newSpyStore()),
	})

	exit := session.Run(context.Background())
	assert.Equal(t, ExitConfig, exit.Class)
	assert.Equal(t, "missing_cloud_agent_id", exit.Code)
	assert.Zero(t, dialer.calls)
}

func TestAgentTunnelSessionRunDialsStartsProcessAndCleansUp(t *testing.T) {
	conn := newFakeWebSocketConn()
	store := newSpyStore()
	proc := newFakeProcess(`{"jsonrpc":"2.0","id":1}` + "\n")
	events := make([]SessionEvent, 0, 4)
	session := NewAgentTunnelSession(AgentConnectionSpec{
		ConnectionID:     "conn_1",
		RemoteID:         "remote_prod",
		CloudAPIURL:      "https://fleet.example.com/base",
		CloudAgentID:     "agent_1",
		TransportQueueID: "agent_1:queue_1",
		InstanceID:       "inst_1",
		Command:          []string{"codex", "acp"},
		Generation:       2,
		RestartNonce:     1,
	}, AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{"X-Pax-Key": []string{"pax_key"}}},
		Dialer:                &fakeDialer{conn: conn},
		LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: proc},
		ReliableEngineFactory: ReliableEngineFromStore(store),
		Heartbeat:             HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour},
		SessionEventSink: SessionEventSinkFunc(func(event SessionEvent) {
			events = append(events, event)
		}),
	})

	exit := session.Run(context.Background())
	assert.Equal(t, ExitTransient, exit.Class)
	assert.Equal(t, "session_ended", exit.Code)
	assert.True(t, exit.ResetBackoff)
	assert.True(t, store.hasOp("AppendOutboundData"), "store ops = %+v", store.ops)
	assert.True(t, store.hasOp("MarkSent"), "store ops = %+v", store.ops)
	assert.True(t, proc.terminated)
	assert.True(t, conn.closed)
	require.GreaterOrEqual(t, len(events), 4)
	assert.Equal(t, PhaseConnecting, events[0].Phase)
	assert.Equal(t, PhaseStopping, events[len(events)-1].Phase)
}

func TestAgentTunnelSendOutboundJournalsBeforeWebSocketSend(t *testing.T) {
	conn := newFakeWebSocketConn()
	store := newSpyStore()
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                &fakeDialer{conn: conn},
		ReliableEngineFactory: ReliableEngineFromStore(store),
	})

	engine := session.newReliableEngine(conn, io.Discard)
	require.NoError(t, session.sendOutbound(context.Background(), []byte(`{"jsonrpc":"2.0","id":1}`), engine))
	require.GreaterOrEqual(t, len(store.ops), 2)
	assert.Equal(t, []string{"AppendOutboundData", "MarkSent"}, store.ops[:2])
	writes := conn.writes()
	require.Len(t, writes, 1)
	env, err := reliablemq.UnmarshalEnvelope(writes[0].payload)
	require.NoError(t, err)
	assert.Equal(t, "agent_1:queue_1", env.QueueID)
	assert.Equal(t, reliablemq.StreamACP, env.Stream)
	assert.Equal(t, int64(1), env.Seq)
	assert.Equal(t, "agent_1", env.Metadata["agent_id"])
}

func TestAgentTunnelPersistentProcessJournalsStdoutAfterTunnelDisconnect(t *testing.T) {
	conn := newFakeWebSocketConn()
	conn.readErrWhenDrained = errors.New("network down")
	store := newSpyStore()
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutWriter.Close()
	proc := &fakeProcess{
		stdin:  &bufferWriteCloser{},
		stdout: stdoutReader,
		stderr: strings.NewReader(""),
		waitCh: make(chan error),
	}
	pool := NewPersistentACPProcessPool(fakeLocalACPProcessRunner{proc: proc}, store)
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                &fakeDialer{conn: conn},
		LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: proc},
		ACPProcessPool:        pool,
		ReliableEngineFactory: ReliableEngineFromStore(store),
		Heartbeat:             HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour},
	})
	go writePersistentInitializeResponse(t, stdoutWriter, `{"protocolVersion":1,"agentCapabilities":{}}`)

	exit := session.Run(context.Background())

	assert.Equal(t, ExitTransient, exit.Class)
	assert.Equal(t, "session_error", exit.Code)
	assert.False(t, proc.terminated)

	_, err := stdoutWriter.Write([]byte(`{"jsonrpc":"2.0","id":9}` + "\n"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		frame, ok := store.Get(reliablemq.FrameKey{
			QueueID:   "agent_1:queue_1",
			Stream:    reliablemq.StreamACP,
			Seq:       1,
			Direction: reliablemq.DirectionOutbound,
		})
		return ok && assert.ObjectsAreEqual([]byte(`{"jsonrpc":"2.0","id":9}`), []byte(frame.Payload))
	}, time.Second, 10*time.Millisecond)
	assert.False(t, proc.terminated)

	pool.Stop(validAgentSpec())
	assert.True(t, proc.terminated)
}

func TestPersistentACPProcessInitializesOnceAndSuppressesInternalResponse(t *testing.T) {
	store := newSpyStore()
	proc := newFakeProcess(strings.Join([]string{
		`{"jsonrpc":"2.0","id":"paxd.initialize","result":{"protocolVersion":1,"agentCapabilities":{"prompt":true}}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`,
		"",
	}, "\n"))
	pool := NewPersistentACPProcessPool(fakeLocalACPProcessRunner{proc: proc}, store)

	acquired, err := pool.Acquire(context.Background(), validAgentSpec())

	require.NoError(t, err)
	require.NotNil(t, acquired)
	assert.Contains(t, proc.stdin.String(), `"method":"initialize"`)
	assert.Contains(t, proc.stdin.String(), `"id":"paxd.initialize"`)
	require.Eventually(t, func() bool {
		_, ok := store.Get(reliablemq.FrameKey{
			QueueID:   "agent_1:queue_1",
			Stream:    reliablemq.StreamACP,
			Seq:       1,
			Direction: reliablemq.DirectionOutbound,
		})
		return ok
	}, time.Second, 10*time.Millisecond)

	frame, ok := store.Get(reliablemq.FrameKey{
		QueueID:   "agent_1:queue_1",
		Stream:    reliablemq.StreamACP,
		Seq:       1,
		Direction: reliablemq.DirectionOutbound,
	})
	require.True(t, ok)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`, string(frame.Payload))
	assert.NotContains(t, string(frame.Payload), "paxd.initialize")
}

func TestAgentTunnelManagerInitializeAnsweredFromCachedPersistentResult(t *testing.T) {
	conn := newFakeWebSocketConn()
	conn.readErrWhenDrained = errors.New("network down")
	store := newSpyStore()
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutWriter.Close()
	proc := &fakeProcess{
		stdin:  &bufferWriteCloser{},
		stdout: stdoutReader,
		stderr: strings.NewReader(""),
		waitCh: make(chan error),
	}
	pool := NewPersistentACPProcessPool(fakeLocalACPProcessRunner{proc: proc}, store)
	initializeEnvelope, err := reliablemq.MarshalEnvelope(reliablemq.Envelope{
		Type:    reliablemq.EnvelopeTypeData,
		QueueID: "agent_1:queue_1",
		Stream:  reliablemq.StreamACP,
		Seq:     7,
		Payload: []byte(`{"jsonrpc":"2.0","id":99,"method":"initialize","params":{"clientInfo":{"name":"manager"}}}`),
	})
	require.NoError(t, err)
	conn.readCh <- fakeWSMessage{messageType: websocketTextMessage, payload: initializeEnvelope}
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                &fakeDialer{conn: conn},
		LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: proc},
		ACPProcessPool:        pool,
		ReliableEngineFactory: ReliableEngineFromStore(store),
		Heartbeat:             HeartbeatConfig{PingInterval: time.Hour, ReadTimeout: time.Hour},
	})
	go writePersistentInitializeResponse(t, stdoutWriter, `{"protocolVersion":1,"agentCapabilities":{"prompt":true}}`)

	exit := session.Run(context.Background())

	assert.Equal(t, ExitTransient, exit.Class)
	assert.Equal(t, "session_error", exit.Code)
	assert.Contains(t, proc.stdin.String(), `"id":"paxd.initialize"`)
	assert.NotContains(t, proc.stdin.String(), `"id":99`)
	var responsePayload []byte
	for _, write := range conn.writes() {
		env, err := reliablemq.UnmarshalEnvelope(write.payload)
		if err == nil && env.Type == reliablemq.EnvelopeTypeData {
			responsePayload = env.Payload
		}
	}
	require.NotEmpty(t, responsePayload)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":99,"result":{"agentCapabilities":{"prompt":true},"protocolVersion":1}}`, string(responsePayload))
}

func TestAgentTunnelReplayInboundAppliesReceivedFrames(t *testing.T) {
	store := newSpyStore()
	inboundKey := reliablemq.FrameKey{QueueID: "agent_1:queue_1", Stream: reliablemq.StreamACP, Seq: 4, Direction: reliablemq.DirectionInbound}
	_, _, err := store.SaveInboundIfAbsent(context.Background(), reliablemq.Frame{
		Key:     inboundKey,
		Kind:    reliablemq.FrameKindData,
		Payload: []byte(`{"jsonrpc":"2.0","id":4}`),
		Status:  reliablemq.StatusReceived,
	})
	require.NoError(t, err)
	session := testAgentTunnelSession(store, newFakeWebSocketConn())
	stdin := &bytes.Buffer{}
	engine := session.newReliableEngine(newFakeWebSocketConn(), stdin)

	require.NoError(t, session.replayInbound(context.Background(), engine))
	assert.Contains(t, stdin.String(), `"id":4`)
	frame, ok := store.Get(inboundKey)
	require.True(t, ok)
	assert.Equal(t, reliablemq.StatusApplied, frame.Status)
}

func TestAgentTunnelReplayOutboundSendsPendingFrames(t *testing.T) {
	conn := newFakeWebSocketConn()
	store := newSpyStore()
	frame, err := store.AppendOutboundData(context.Background(), "agent_1:queue_1", reliablemq.StreamACP, []byte(`{"jsonrpc":"2.0","id":2}`), reliablemq.Metadata{"agent_id": "agent_1"})
	require.NoError(t, err)
	session := testAgentTunnelSession(store, conn)
	engine := session.newReliableEngine(conn, io.Discard)

	require.NoError(t, session.replayOutbound(context.Background(), engine))
	assert.Len(t, conn.writes(), 1)
	got, ok := store.Get(frame.Key)
	require.True(t, ok)
	assert.Equal(t, reliablemq.StatusSent, got.Status)
}

func TestAgentTunnelCopyWSToStdinHandlesAck(t *testing.T) {
	conn := newFakeWebSocketConn()
	store := newSpyStore()
	frame, err := store.AppendOutboundData(context.Background(), "agent_1:queue_1", reliablemq.StreamACP, []byte(`{"jsonrpc":"2.0","id":3}`), nil)
	require.NoError(t, err)
	require.NoError(t, store.MarkSent(context.Background(), frame.Key))
	ack, err := reliablemq.MarshalEnvelope(reliablemq.AckEnvelope("agent_1:queue_1", reliablemq.StreamACP, 3))
	require.NoError(t, err)
	conn.readCh <- fakeWSMessage{messageType: websocketTextMessage, payload: ack}
	conn.Close()

	session := testAgentTunnelSession(store, conn)
	engine := session.newReliableEngine(conn, &bytes.Buffer{})
	err = session.copyWSToStdin(context.Background(), conn, engine)
	require.ErrorContains(t, err, "read tunnel")
	got, ok := store.Get(frame.Key)
	require.True(t, ok)
	assert.Equal(t, reliablemq.StatusAcked, got.Status)
}

func TestAgentTunnelCopyWSToStdinReceivesDataFrame(t *testing.T) {
	conn := newFakeWebSocketConn()
	store := newSpyStore()
	payload, err := reliablemq.MarshalEnvelope(reliablemq.Envelope{
		Type:    reliablemq.EnvelopeTypeData,
		QueueID: "agent_1:queue_1",
		Stream:  reliablemq.StreamACP,
		Seq:     8,
		Payload: []byte(`{"jsonrpc":"2.0","id":8}`),
	})
	require.NoError(t, err)
	conn.readCh <- fakeWSMessage{messageType: websocketTextMessage, payload: payload}
	conn.readErrWhenDrained = errors.New("read closed")

	stdin := &bytes.Buffer{}
	session := testAgentTunnelSession(store, conn)
	engine := session.newReliableEngine(conn, stdin)
	err = session.copyWSToStdin(context.Background(), conn, engine)
	require.ErrorContains(t, err, "read tunnel")
	assert.Contains(t, stdin.String(), `"id":8`)
	assert.Len(t, conn.writes(), 1)
}

func TestAgentTunnelCopyStdoutToWSSendsLinesAndRejectsInvalidJSON(t *testing.T) {
	conn := newFakeWebSocketConn()
	session := testAgentTunnelSession(newSpyStore(), conn)
	engine := session.newReliableEngine(conn, io.Discard)

	require.NoError(t, session.copyStdoutToWS(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1}`+"\n"), engine))
	assert.Len(t, conn.writes(), 1)

	err := session.copyStdoutToWS(context.Background(), strings.NewReader("not-json\n"), engine)
	require.ErrorContains(t, err, "must be JSON")
}

func TestAgentTunnelSendOutboundMarksFailedOnWriteError(t *testing.T) {
	conn := newFakeWebSocketConn()
	conn.writeErr = errors.New("write failed")
	store := newSpyStore()
	session := testAgentTunnelSession(store, conn)
	engine := session.newReliableEngine(conn, io.Discard)

	err := session.sendOutbound(context.Background(), []byte(`{"jsonrpc":"2.0","id":1}`), engine)
	require.ErrorContains(t, err, "write failed")
	frame, ok := store.Get(reliablemq.FrameKey{QueueID: "agent_1:queue_1", Stream: reliablemq.StreamACP, Seq: 1, Direction: reliablemq.DirectionOutbound})
	require.True(t, ok)
	assert.Equal(t, "write failed", frame.ErrorMessage)
}

func TestAgentTunnelDuplicateInboundIsAckedButNotDispatchedTwice(t *testing.T) {
	conn := newFakeWebSocketConn()
	store := newSpyStore()
	stdin := &bytes.Buffer{}
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                &fakeDialer{conn: conn},
		ReliableEngineFactory: ReliableEngineFromStore(store),
	})
	env := reliablemq.Envelope{
		Type:    reliablemq.EnvelopeTypeData,
		QueueID: "agent_1:queue_1",
		Stream:  reliablemq.StreamACP,
		Seq:     7,
		Payload: []byte(`{"jsonrpc":"2.0","id":1}`),
	}
	engine := session.newReliableEngine(conn, stdin)

	require.NoError(t, engine.Receive(context.Background(), env))
	require.NoError(t, engine.Receive(context.Background(), env))
	assert.Equal(t, 1, strings.Count(stdin.String(), `"jsonrpc"`))
	assert.Len(t, conn.writes(), 2)
}

func TestAgentTunnelInboundTombstoneIsAckedWithoutDispatch(t *testing.T) {
	conn := newFakeWebSocketConn()
	store := newSpyStore()
	stdin := &bytes.Buffer{}
	session := testAgentTunnelSession(store, conn)
	env := reliablemq.Envelope{
		Type:         reliablemq.EnvelopeTypeTombstone,
		QueueID:      "conn_1",
		Stream:       reliablemq.StreamACP,
		Seq:          9,
		ErrorMessage: "remote rejected frame",
	}
	engine := session.newReliableEngine(conn, stdin)

	require.NoError(t, engine.Receive(context.Background(), env))
	assert.Zero(t, stdin.Len())
	assert.Len(t, conn.writes(), 1)
}

func TestAgentTunnelValidationHelpers(t *testing.T) {
	tests := []struct {
		name string
		spec AgentConnectionSpec
		deps func(AgentTunnelSessionDeps) AgentTunnelSessionDeps
		want string
	}{
		{name: "missing connection id", spec: AgentConnectionSpec{}, want: "missing_connection_id"},
		{name: "missing remote id", spec: AgentConnectionSpec{ConnectionID: "conn_1"}, want: "missing_remote_id"},
		{name: "missing transport queue id", spec: AgentConnectionSpec{ConnectionID: "conn_1", RemoteID: "remote", CloudAgentID: "agent", CloudAPIURL: "https://fleet.example.com", Command: []string{"cmd"}}, want: "missing_transport_queue_id"},
		{name: "missing url", spec: AgentConnectionSpec{ConnectionID: "conn_1", RemoteID: "remote", CloudAgentID: "agent", TransportQueueID: "agent:queue", Command: []string{"cmd"}}, want: "missing_cloud_url"},
		{name: "missing command", spec: AgentConnectionSpec{ConnectionID: "conn_1", RemoteID: "remote", CloudAgentID: "agent", TransportQueueID: "agent:queue", CloudAPIURL: "https://fleet.example.com"}, want: "missing_command"},
		{
			name: "missing auth provider",
			spec: validAgentSpec(),
			deps: func(deps AgentTunnelSessionDeps) AgentTunnelSessionDeps {
				deps.Headers = nil
				return deps
			},
			want: "missing_auth_provider",
		},
		{
			name: "missing dialer",
			spec: validAgentSpec(),
			deps: func(deps AgentTunnelSessionDeps) AgentTunnelSessionDeps {
				deps.Dialer = nil
				return deps
			},
			want: "missing_dialer",
		},
		{
			name: "missing process runner",
			spec: validAgentSpec(),
			deps: func(deps AgentTunnelSessionDeps) AgentTunnelSessionDeps {
				deps.LocalACPProcessRunner = nil
				return deps
			},
			want: "missing_process_runner",
		},
		{
			name: "missing reliable engine",
			spec: validAgentSpec(),
			deps: func(deps AgentTunnelSessionDeps) AgentTunnelSessionDeps {
				deps.ReliableEngineFactory = nil
				return deps
			},
			want: "missing_reliable_engine",
		},
		{
			name: "missing working dir",
			spec: func() AgentConnectionSpec {
				spec := validAgentSpec()
				spec.WorkingDir = "/path/that/does/not/exist"
				return spec
			}(),
			want: "missing_working_dir",
		},
		{
			name: "working dir is file",
			spec: func() AgentConnectionSpec {
				spec := validAgentSpec()
				f, err := os.CreateTemp("", "paxd-runtime-file")
				require.NoError(t, err)
				t.Cleanup(func() { _ = os.Remove(f.Name()) })
				_ = f.Close()
				spec.WorkingDir = f.Name()
				return spec
			}(),
			want: "invalid_working_dir",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := AgentTunnelSessionDeps{
				Headers:               fakeHeaderProvider{header: http.Header{}},
				Dialer:                &fakeDialer{conn: newFakeWebSocketConn()},
				LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: newFakeProcess("")},
				ReliableEngineFactory: ReliableEngineFromStore(newSpyStore()),
			}
			if tt.deps != nil {
				deps = tt.deps(deps)
			}
			session := &AgentTunnelSession{spec: tt.spec, deps: deps}
			assert.Equal(t, tt.want, session.validate().Code)
		})
	}

	assert.Equal(t, ExitConfig, classifyProcessStartExit(errors.New("executable file not found in $PATH")).Class)
	assert.Equal(t, "hello", string(trimLineDelimiter([]byte("hello\r\n"))))
	assert.Equal(t, "x", firstNonEmpty("", "x", "y"))
}

func validAgentSpec() AgentConnectionSpec {
	return AgentConnectionSpec{
		ConnectionID:     "conn_1",
		RemoteID:         "remote_prod",
		CloudAPIURL:      "https://fleet.example.com",
		CloudAgentID:     "agent_1",
		TransportQueueID: "agent_1:queue_1",
		Command:          []string{"codex", "acp"},
	}
}

func TestRuntimeURLAndEnvelopeHelpers(t *testing.T) {
	got, err := websocketURLFromHTTP("http://localhost:8080/base/", "node/control")
	require.NoError(t, err)
	assert.Equal(t, "ws://localhost:8080/base/node/control", got.String())

	_, err = websocketURLFromHTTP("ftp://fleet.example.com", "/x")
	require.Error(t, err)

}

func TestAgentTunnelReconcilePaxdProducerSendsCheckpointAndAdvancesCursor(t *testing.T) {
	conn := newFakeWebSocketConn()
	response, err := reliablemq.MarshalEnvelope(reliablemq.Envelope{
		Type:                   reliablemq.EnvelopeTypeReconcileResponse,
		QueueID:                "agent_1:queue_1",
		Stream:                 reliablemq.StreamACP,
		Action:                 reliablemq.ReconcileActionAdvanceProducer,
		ConsumerAckedThrough:   8,
		AdvanceProducerNextSeq: 9,
	})
	require.NoError(t, err)
	conn.readCh <- fakeWSMessage{messageType: websocketTextMessage, payload: response}
	reconciler := &spyReconcileProducerStore{
		checkpoint: reliablemq.ProducerReconcileCheckpoint{
			QueueID:         "agent_1:queue_1",
			Stream:          reliablemq.StreamACP,
			ProducerNextSeq: 4,
			ReplayFrom:      2,
			ReplayThrough:   3,
		},
	}
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                &fakeDialer{conn: conn},
		ReliableEngineFactory: ReliableEngineFromStore(newSpyStore()),
		TransportReconciler:   reconciler,
	})

	err = session.reconcilePaxdProducer(context.Background(), conn)
	require.NoError(t, err)

	writes := conn.writes()
	require.Len(t, writes, 1)
	request, err := reliablemq.UnmarshalEnvelope(writes[0].payload)
	require.NoError(t, err)
	assert.Equal(t, reliablemq.EnvelopeTypeReconcileRequest, request.Type)
	assert.Equal(t, int64(4), request.ProducerNextSeq)
	assert.Equal(t, int64(2), request.ReplayFrom)
	assert.Equal(t, int64(3), request.ReplayThrough)
	assert.Equal(t, int64(9), reconciler.advancedNextSeq)
}

func TestAgentTunnelReconcilePaxdProducerTreatsEarlyCloseAsRotate(t *testing.T) {
	conn := newFakeWebSocketConn()
	conn.readErrWhenDrained = io.ErrUnexpectedEOF
	session := NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                &fakeDialer{conn: conn},
		ReliableEngineFactory: ReliableEngineFromStore(newSpyStore()),
		TransportReconciler: &spyReconcileProducerStore{
			checkpoint: reliablemq.ProducerReconcileCheckpoint{
				ProducerNextSeq: 4,
			},
		},
	})

	err := session.reconcilePaxdProducer(context.Background(), conn)

	require.ErrorIs(t, err, ErrTransportQueueRotate)
	require.Len(t, conn.writes(), 1)
}

func TestNodeControlRunnerFuncNilReturnsConfigExit(t *testing.T) {
	exit := (NodeControlRunnerFunc(nil)).RunNodeControl(context.Background(), newFakeWebSocketConn(), RemoteSpec{})
	assert.Equal(t, ExitConfig, exit.Class)
	assert.Equal(t, "missing_node_control_runner", exit.Code)
}

func testAgentTunnelSession(store reliablemq.DurableStore, conn *fakeWebSocketConn) *AgentTunnelSession {
	return NewAgentTunnelSession(validAgentSpec(), AgentTunnelSessionDeps{
		Headers:               fakeHeaderProvider{header: http.Header{}},
		Dialer:                &fakeDialer{conn: conn},
		LocalACPProcessRunner: fakeLocalACPProcessRunner{proc: newFakeProcess("")},
		ReliableEngineFactory: ReliableEngineFromStore(store),
	})
}

type fakeHeaderProvider struct {
	header http.Header
	err    error
}

type fakeLocalACPProcessRunner struct {
	proc *fakeProcess
	err  error
}

func (r fakeLocalACPProcessRunner) Start(ctx context.Context, spec LocalACPProcessSpec) (LocalACPProcess, error) {
	return r.proc, r.err
}

type fakeProcess struct {
	stdin      *bufferWriteCloser
	stdout     io.Reader
	stderr     io.Reader
	waitCh     chan error
	terminated bool
}

func newFakeProcess(stdout string) *fakeProcess {
	return &fakeProcess{
		stdin:  &bufferWriteCloser{},
		stdout: strings.NewReader(stdout),
		stderr: strings.NewReader(""),
		waitCh: make(chan error),
	}
}

func writePersistentInitializeResponse(t *testing.T, writer io.Writer, result string) {
	t.Helper()
	_, err := writer.Write([]byte(`{"jsonrpc":"2.0","id":"paxd.initialize","result":` + result + "}\n"))
	assert.NoError(t, err)
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.Reader     { return p.stdout }
func (p *fakeProcess) Stderr() io.Reader     { return p.stderr }
func (p *fakeProcess) Wait() error           { return <-p.waitCh }
func (p *fakeProcess) Terminate(ctx context.Context) error {
	p.terminated = true
	select {
	case p.waitCh <- nil:
	default:
	}
	return nil
}

type bufferWriteCloser struct {
	bytes.Buffer
	closed bool
}

func (b *bufferWriteCloser) Close() error {
	b.closed = true
	return nil
}

func (p fakeHeaderProvider) Headers(ctx context.Context, remoteID string) (http.Header, error) {
	return p.header.Clone(), p.err
}

type fakeDialer struct {
	conn   WebSocketConn
	resp   *http.Response
	err    error
	url    string
	header http.Header
	calls  int
}

func (d *fakeDialer) Dial(ctx context.Context, url string, header http.Header) (WebSocketConn, *http.Response, error) {
	d.calls++
	d.url = url
	d.header = header.Clone()
	return d.conn, d.resp, d.err
}

type fakeWSMessage struct {
	messageType int
	payload     []byte
}

type fakeWSWrite struct {
	messageType int
	payload     []byte
}

type fakeWebSocketConn struct {
	mu                 sync.Mutex
	readCh             chan fakeWSMessage
	closeCh            chan struct{}
	closed             bool
	writeErr           error
	readErrWhenDrained error
	writeLog           []fakeWSWrite
	pongHandler        func(string) error
}

func newFakeWebSocketConn() *fakeWebSocketConn {
	return &fakeWebSocketConn{readCh: make(chan fakeWSMessage, 8), closeCh: make(chan struct{})}
}

func (c *fakeWebSocketConn) ReadMessage() (int, []byte, error) {
	select {
	case msg := <-c.readCh:
		return msg.messageType, msg.payload, nil
	default:
	}
	if c.readErrWhenDrained != nil {
		return 0, nil, c.readErrWhenDrained
	}
	select {
	case msg := <-c.readCh:
		return msg.messageType, msg.payload, nil
	case <-c.closeCh:
		return 0, nil, errors.New("closed")
	}
}

func (c *fakeWebSocketConn) WriteMessage(messageType int, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	if c.closed {
		return errors.New("closed")
	}
	c.writeLog = append(c.writeLog, fakeWSWrite{messageType: messageType, payload: append([]byte(nil), payload...)})
	return nil
}

func (c *fakeWebSocketConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.closeCh)
	}
	return nil
}

func (c *fakeWebSocketConn) SetPongHandler(handler func(string) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pongHandler = handler
}

func (c *fakeWebSocketConn) pong(appData string) error {
	c.mu.Lock()
	handler := c.pongHandler
	c.mu.Unlock()
	if handler == nil {
		return nil
	}
	return handler(appData)
}

func (c *fakeWebSocketConn) writes() []fakeWSWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]fakeWSWrite, len(c.writeLog))
	copy(out, c.writeLog)
	return out
}

type spyStore struct {
	*memory.Store
	mu  sync.Mutex
	ops []string
}

func newSpyStore() *spyStore {
	return &spyStore{Store: memory.New()}
}

func (s *spyStore) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *spyStore) hasOp(op string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, got := range s.ops {
		if got == op {
			return true
		}
	}
	return false
}

func (s *spyStore) AppendOutboundData(ctx context.Context, queueID string, stream reliablemq.Stream, payload json.RawMessage, metadata reliablemq.Metadata) (reliablemq.Frame, error) {
	s.record("AppendOutboundData")
	return s.Store.AppendOutboundData(ctx, queueID, stream, payload, metadata)
}

func (s *spyStore) AppendOutboundTombstone(ctx context.Context, queueID string, stream reliablemq.Stream, errorMessage string, metadata reliablemq.Metadata) (reliablemq.Frame, error) {
	s.record("AppendOutboundTombstone")
	return s.Store.AppendOutboundTombstone(ctx, queueID, stream, errorMessage, metadata)
}

func (s *spyStore) SaveInboundIfAbsent(ctx context.Context, frame reliablemq.Frame) (bool, reliablemq.Frame, error) {
	s.record("SaveInboundIfAbsent")
	return s.Store.SaveInboundIfAbsent(ctx, frame)
}

func (s *spyStore) ListOutboundReplay(ctx context.Context, queueID string, stream reliablemq.Stream, limit int) ([]reliablemq.Frame, error) {
	s.record("ListOutboundReplay")
	return s.Store.ListOutboundReplay(ctx, queueID, stream, limit)
}

func (s *spyStore) ListInboundReplay(ctx context.Context, queueID string, stream reliablemq.Stream, limit int) ([]reliablemq.Frame, error) {
	s.record("ListInboundReplay")
	return s.Store.ListInboundReplay(ctx, queueID, stream, limit)
}

func (s *spyStore) MarkSent(ctx context.Context, key reliablemq.FrameKey) error {
	s.record("MarkSent")
	return s.Store.MarkSent(ctx, key)
}

func (s *spyStore) AckOutboundThrough(ctx context.Context, queueID string, stream reliablemq.Stream, throughSeq int64) error {
	s.record("AckOutboundThrough")
	return s.Store.AckOutboundThrough(ctx, queueID, stream, throughSeq)
}

func (s *spyStore) MarkApplied(ctx context.Context, key reliablemq.FrameKey) error {
	s.record("MarkApplied")
	return s.Store.MarkApplied(ctx, key)
}

func (s *spyStore) MarkRejected(ctx context.Context, key reliablemq.FrameKey, errorMessage string) error {
	s.record("MarkRejected")
	return s.Store.MarkRejected(ctx, key, errorMessage)
}

func (s *spyStore) RecordSendFailure(ctx context.Context, key reliablemq.FrameKey, errorMessage string) error {
	s.record("RecordSendFailure")
	return s.Store.RecordSendFailure(ctx, key, errorMessage)
}

func (s *spyStore) RecordDispatchFailure(ctx context.Context, key reliablemq.FrameKey, errorMessage string) error {
	s.record("RecordDispatchFailure")
	return s.Store.RecordDispatchFailure(ctx, key, errorMessage)
}

func (s *spyStore) UpdateMetadata(ctx context.Context, key reliablemq.FrameKey, metadata reliablemq.Metadata) error {
	s.record("UpdateMetadata")
	return s.Store.UpdateMetadata(ctx, key, metadata)
}

type spyReconcileProducerStore struct {
	checkpoint      reliablemq.ProducerReconcileCheckpoint
	advancedNextSeq int64
}

func (s *spyReconcileProducerStore) LoadProducerReconcileCheckpoint(
	ctx context.Context,
	queueID string,
	stream reliablemq.Stream,
) (reliablemq.ProducerReconcileCheckpoint, error) {
	_ = ctx
	s.checkpoint.QueueID = queueID
	s.checkpoint.Stream = stream
	return s.checkpoint, nil
}

func (s *spyReconcileProducerStore) AdvanceProducerNextSeq(
	ctx context.Context,
	queueID string,
	stream reliablemq.Stream,
	nextSeq int64,
) error {
	_ = ctx
	_ = queueID
	_ = stream
	s.advancedNextSeq = nextSeq
	return nil
}
