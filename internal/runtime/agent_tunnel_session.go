package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxkit/reliablemq"
)

const replayLimit = 1000

var ErrTransportQueueRotate = errors.New("manager requested queue rotation")

// AgentTunnelSessionDeps are process-wide dependencies used to run one ACP
// tunnel attempt. The desired connection data lives in AgentConnectionSpec.
type AgentTunnelSessionDeps struct {
	Headers               auth.HeaderProvider
	Dialer                WebSocketDialer
	LocalACPProcessRunner LocalACPProcessRunner
	ACPProcessPool        *PersistentACPProcessPool
	ReliableEngineFactory ReliableEngineFactory
	TransportReconciler   reliablemq.ReconcileProducerStore
	Heartbeat             HeartbeatConfig
	SessionEventSink      SessionEventSink
}

type AgentTunnelSession struct {
	spec AgentConnectionSpec
	deps AgentTunnelSessionDeps
}

type ReliableEngine interface {
	Send(ctx context.Context, msg reliablemq.OutboundMessage) (reliablemq.Frame, error)
	Receive(ctx context.Context, env reliablemq.Envelope) error
	ReplayInbound(ctx context.Context, queueID string, stream reliablemq.Stream, limit int) error
	ReplayOutbound(ctx context.Context, queueID string, stream reliablemq.Stream, limit int) error
}

type ReliableEngineFactory interface {
	NewReliableEngine(sender reliablemq.Sender, dispatcher reliablemq.Dispatcher) ReliableEngine
}

type ReliableEngineFactoryFunc func(sender reliablemq.Sender, dispatcher reliablemq.Dispatcher) ReliableEngine

func (f ReliableEngineFactoryFunc) NewReliableEngine(sender reliablemq.Sender, dispatcher reliablemq.Dispatcher) ReliableEngine {
	if f == nil {
		return nil
	}
	return f(sender, dispatcher)
}

func ReliableEngineFromStore(store reliablemq.DurableStore, opts ...reliablemq.Option) ReliableEngineFactory {
	return ReliableEngineFactoryFunc(func(sender reliablemq.Sender, dispatcher reliablemq.Dispatcher) ReliableEngine {
		return reliablemq.NewEngine(reliablemq.Config{}, store, sender, dispatcher, opts...)
	})
}

func NewAgentTunnelSession(spec AgentConnectionSpec, deps AgentTunnelSessionDeps) *AgentTunnelSession {
	if deps.LocalACPProcessRunner == nil {
		deps.LocalACPProcessRunner = ExecLocalACPProcessRunner{}
	}
	if deps.SessionEventSink == nil {
		deps.SessionEventSink = NoopSessionEventSink{}
	}
	return &AgentTunnelSession{spec: spec, deps: deps}
}

func (s *AgentTunnelSession) Run(ctx context.Context) Exit {
	if exit := s.validate(); exit.Class != "" {
		log.Printf("[paxd] agent tunnel id=%s validation failed class=%s code=%s message=%q", s.spec.ConnectionID, exit.Class, exit.Code, exit.Message)
		return exit
	}
	header, err := s.deps.Headers.Headers(ctx, s.spec.RemoteID)
	if err != nil {
		log.Printf("[paxd] agent tunnel id=%s auth headers failed remote_id=%s: %v", s.spec.ConnectionID, s.spec.RemoteID, err)
		return AuthExit("auth_headers_failed", err.Error())
	}
	log.Printf("[paxd] agent tunnel id=%s remote_id=%s auth header summary: %s", s.spec.ConnectionID, s.spec.RemoteID, requestHeaderSummary(header))
	wsURL, err := websocketURLFromHTTP(s.spec.CloudAPIURL, firstNonEmpty(s.spec.TunnelPath, DefaultAgentTunnelPath))
	if err != nil {
		log.Printf("[paxd] agent tunnel id=%s invalid cloud url %q: %v", s.spec.ConnectionID, s.spec.CloudAPIURL, err)
		return ConfigExit("invalid_cloud_url", err.Error())
	}
	q := wsURL.Query()
	q.Set("connection_id", s.spec.TransportQueueID)
	q.Set("agent_id", s.spec.CloudAgentID)
	if s.spec.InstanceID != "" {
		q.Set("instance_id", s.spec.InstanceID)
	}
	wsURL.RawQuery = q.Encode()

	s.emit(PhaseConnecting, nil)
	log.Printf("[paxd] agent tunnel id=%s dialing %s", s.spec.ConnectionID, wsURL.Redacted())
	conn, resp, err := s.deps.Dialer.Dial(ctx, wsURL.String(), header)
	respDiag := consumeResponseDiagnostics(resp)
	if err != nil {
		log.Printf("[paxd] agent tunnel id=%s dial failed %s err=%v", s.spec.ConnectionID, respDiag.String(), err)
		return classifyDialExit(err, resp)
	}
	if conn == nil {
		log.Printf("[paxd] agent tunnel id=%s dialer returned nil connection", s.spec.ConnectionID)
		return TransientExit("dial_no_connection", "websocket dialer returned nil connection")
	}

	hbConn := newHeartbeatConn(conn, s.deps.Heartbeat)
	hbConn.Start()
	defer hbConn.Close()
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go closeOnContextDone(watchCtx, hbConn)

	connectedExit := func(exit Exit) Exit {
		if exit.Class == ExitTransient {
			return exit.WithBackoffReset()
		}
		return exit
	}

	s.emit(PhaseConnected, nil)
	if s.deps.ACPProcessPool != nil {
		return s.runWithPersistentProcess(ctx, hbConn, connectedExit)
	}
	s.emit(PhaseStarting, nil)
	log.Printf(
		"[paxd] agent tunnel id=%s starting local ACP command=%q working_dir=%q transport_queue_id=%s",
		s.spec.ConnectionID,
		strings.Join(s.spec.Command, " "),
		s.spec.WorkingDir,
		s.spec.TransportQueueID,
	)
	proc, err := s.deps.LocalACPProcessRunner.Start(ctx, LocalACPProcessSpec{
		Command:    s.spec.Command,
		WorkingDir: s.spec.WorkingDir,
		Env:        s.spec.Env,
	})
	if err != nil {
		_ = hbConn.Close()
		log.Printf("[paxd] agent tunnel id=%s local ACP start failed: %v", s.spec.ConnectionID, err)
		return connectedExit(classifyProcessStartExit(err))
	}
	defer s.terminateProcess(proc)
	go io.Copy(io.Discard, proc.Stderr())

	engine := s.newReliableEngine(hbConn, proc.Stdin())
	if err := s.reconcilePaxdProducer(ctx, hbConn); err != nil {
		if errors.Is(err, ErrTransportQueueRotate) {
			log.Printf(
				"[paxd] agent tunnel id=%s reconcile requested queue rotation transport_queue_id=%s: %v",
				s.spec.ConnectionID,
				s.spec.TransportQueueID,
				err,
			)
			return connectedExit(TransientExit("reconcile_rotate", err.Error()))
		}
		log.Printf(
			"[paxd] agent tunnel id=%s reconcile failed transport_queue_id=%s: %v",
			s.spec.ConnectionID,
			s.spec.TransportQueueID,
			err,
		)
		return connectedExit(TransientExit("reconcile_failed", err.Error()))
	}
	if err := s.replayInbound(ctx, engine); err != nil {
		log.Printf("[paxd] agent tunnel id=%s replay inbound failed: %v", s.spec.ConnectionID, err)
		return connectedExit(TransientExit("replay_inbound_failed", err.Error()))
	}
	if err := s.replayOutbound(ctx, engine); err != nil {
		log.Printf("[paxd] agent tunnel id=%s replay outbound failed: %v", s.spec.ConnectionID, err)
		return connectedExit(TransientExit("replay_outbound_failed", err.Error()))
	}

	s.emit(PhaseRunning, nil)
	log.Printf("[paxd] agent tunnel id=%s running", s.spec.ConnectionID)
	errCh := make(chan error, 3)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() { errCh <- s.copyWSToStdin(runCtx, hbConn, engine) }()
	go func() { errCh <- s.copyStdoutToWS(runCtx, proc.Stdout(), engine) }()
	go func() { errCh <- proc.Wait() }()

	var result error
	select {
	case result = <-errCh:
		cancel()
	case <-ctx.Done():
		cancel()
		result = ctx.Err()
	}

	s.emit(PhaseStopping, nil)
	_ = hbConn.Close()
	_ = proc.Stdin().Close()

	if hbConn.TimedOut() {
		log.Printf("[paxd] agent tunnel id=%s heartbeat timed out", s.spec.ConnectionID)
		return connectedExit(TransientExit("heartbeat_timeout", "websocket heartbeat timed out"))
	}
	if errors.Is(result, context.Canceled) || errors.Is(result, context.DeadlineExceeded) || ctx.Err() != nil {
		return CanceledExit(result)
	}
	if result == nil {
		return connectedExit(TransientExit("session_ended", "agent tunnel session ended"))
	}
	return connectedExit(TransientExit("session_error", result.Error()))
}

func (s *AgentTunnelSession) runWithPersistentProcess(
	ctx context.Context,
	conn *heartbeatConn,
	connectedExit func(Exit) Exit,
) Exit {
	s.emit(PhaseStarting, nil)
	log.Printf(
		"[paxd] agent tunnel id=%s acquiring persistent local ACP command=%q working_dir=%q transport_queue_id=%s",
		s.spec.ConnectionID,
		strings.Join(s.spec.Command, " "),
		s.spec.WorkingDir,
		s.spec.TransportQueueID,
	)
	proc, err := s.deps.ACPProcessPool.Acquire(ctx, s.spec)
	if err != nil {
		_ = conn.Close()
		log.Printf("[paxd] agent tunnel id=%s persistent local ACP acquire failed: %v", s.spec.ConnectionID, err)
		return connectedExit(classifyProcessStartExit(err))
	}
	detachSender := proc.AttachSender(s.reliableSender(conn))
	defer detachSender()

	engine := s.newReliableEngineWithInitialize(conn, proc.Stdin(), proc.InitializeResult())
	if err := s.reconcilePaxdProducer(ctx, conn); err != nil {
		if errors.Is(err, ErrTransportQueueRotate) {
			log.Printf(
				"[paxd] agent tunnel id=%s reconcile requested queue rotation transport_queue_id=%s: %v",
				s.spec.ConnectionID,
				s.spec.TransportQueueID,
				err,
			)
			return connectedExit(TransientExit("reconcile_rotate", err.Error()))
		}
		log.Printf(
			"[paxd] agent tunnel id=%s reconcile failed transport_queue_id=%s: %v",
			s.spec.ConnectionID,
			s.spec.TransportQueueID,
			err,
		)
		return connectedExit(TransientExit("reconcile_failed", err.Error()))
	}
	if err := s.replayInbound(ctx, engine); err != nil {
		log.Printf("[paxd] agent tunnel id=%s replay inbound failed: %v", s.spec.ConnectionID, err)
		return connectedExit(TransientExit("replay_inbound_failed", err.Error()))
	}
	if err := s.replayOutbound(ctx, engine); err != nil {
		log.Printf("[paxd] agent tunnel id=%s replay outbound failed: %v", s.spec.ConnectionID, err)
		return connectedExit(TransientExit("replay_outbound_failed", err.Error()))
	}

	s.emit(PhaseRunning, nil)
	log.Printf("[paxd] agent tunnel id=%s running with persistent ACP process", s.spec.ConnectionID)
	errCh := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { errCh <- s.copyWSToStdin(runCtx, conn, engine) }()

	var result error
	processExited := false
	select {
	case result = <-errCh:
		cancel()
	case <-proc.Done():
		cancel()
		processExited = true
		result = proc.Err()
	case <-ctx.Done():
		cancel()
		result = ctx.Err()
	}

	s.emit(PhaseStopping, nil)
	_ = conn.Close()

	if conn.TimedOut() {
		log.Printf("[paxd] agent tunnel id=%s heartbeat timed out", s.spec.ConnectionID)
		return connectedExit(TransientExit("heartbeat_timeout", "websocket heartbeat timed out"))
	}
	if errors.Is(result, context.Canceled) || errors.Is(result, context.DeadlineExceeded) || ctx.Err() != nil {
		s.deps.ACPProcessPool.Stop(s.spec)
		return CanceledExit(result)
	}
	if processExited {
		msg := "persistent ACP process exited"
		if result != nil {
			msg = result.Error()
		}
		s.deps.ACPProcessPool.Forget(s.spec, proc)
		return connectedExit(TransientExit("process_ended", msg))
	}
	if result == nil {
		return connectedExit(TransientExit("session_ended", "agent tunnel session ended"))
	}
	return connectedExit(TransientExit("session_error", result.Error()))
}

func (s *AgentTunnelSession) validate() Exit {
	switch {
	case s.spec.ConnectionID == "":
		return ConfigExit("missing_connection_id", "connection id is required")
	case s.spec.RemoteID == "":
		return ConfigExit("missing_remote_id", "remote id is required")
	case s.spec.CloudAgentID == "":
		return ConfigExit("missing_cloud_agent_id", "cloud agent id is required")
	case s.spec.TransportQueueID == "":
		return ConfigExit("missing_transport_queue_id", "transport queue id is required")
	case s.spec.CloudAPIURL == "":
		return ConfigExit("missing_cloud_url", "cloud api url is required")
	case len(s.spec.Command) == 0:
		return ConfigExit("missing_command", "acp command is required")
	case s.deps.Headers == nil:
		return ConfigExit("missing_auth_provider", "auth header provider is required")
	case s.deps.Dialer == nil:
		return ConfigExit("missing_dialer", "websocket dialer is required")
	case s.deps.LocalACPProcessRunner == nil:
		return ConfigExit("missing_process_runner", "process runner is required")
	case s.deps.ReliableEngineFactory == nil:
		return ConfigExit("missing_reliable_engine", "reliablemq engine factory is required")
	}
	if s.spec.WorkingDir != "" {
		info, err := os.Stat(s.spec.WorkingDir)
		if err != nil {
			return ConfigExit("missing_working_dir", err.Error())
		}
		if !info.IsDir() {
			return ConfigExit("invalid_working_dir", "working directory is not a directory")
		}
	}
	return Exit{}
}

func (s *AgentTunnelSession) replayInbound(ctx context.Context, engine ReliableEngine) error {
	return engine.ReplayInbound(ctx, s.spec.TransportQueueID, reliablemq.StreamACP, replayLimit)
}

func (s *AgentTunnelSession) replayOutbound(ctx context.Context, engine ReliableEngine) error {
	return engine.ReplayOutbound(ctx, s.spec.TransportQueueID, reliablemq.StreamACP, replayLimit)
}

func (s *AgentTunnelSession) reconcilePaxdProducer(ctx context.Context, conn WebSocketConn) error {
	if s.deps.TransportReconciler == nil {
		return nil
	}
	checkpoint, err := s.deps.TransportReconciler.LoadProducerReconcileCheckpoint(
		ctx,
		s.spec.TransportQueueID,
		reliablemq.StreamACP,
	)
	if err != nil {
		return fmt.Errorf("load producer checkpoint: %w", err)
	}
	request, err := reliablemq.MarshalEnvelope(reliablemq.ReconcileRequestEnvelope(checkpoint))
	if err != nil {
		return err
	}
	if err := conn.WriteMessage(websocketTextMessage, request); err != nil {
		return fmt.Errorf("write reconcile request: %w", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		if isReconcileResponseClose(err) {
			return ErrTransportQueueRotate
		}
		return fmt.Errorf("read reconcile response: %w", err)
	}
	if messageType != websocketTextMessage && messageType != websocketBinaryMessage {
		return fmt.Errorf("unexpected reconcile message type %d", messageType)
	}
	response, err := reliablemq.UnmarshalEnvelope(payload)
	if err != nil {
		return fmt.Errorf("decode reconcile response: %w", err)
	}
	if response.Type != reliablemq.EnvelopeTypeReconcileResponse {
		return fmt.Errorf("expected reconcile_response, got %q", response.Type)
	}
	if response.QueueID != s.spec.TransportQueueID {
		return fmt.Errorf("unexpected reconcile queue_id %q", response.QueueID)
	}
	if response.Stream != reliablemq.StreamACP {
		return fmt.Errorf("unexpected reconcile stream %q", response.Stream)
	}
	log.Printf(
		"[paxd] agent tunnel id=%s reconciled queue_id=%s action=%s producer_next_seq=%d producer_replay_from=%d producer_replay_through=%d consumer_acked_through=%d replay_from=%d replay_through=%d advance_producer_next_seq=%d",
		s.spec.ConnectionID,
		s.spec.TransportQueueID,
		response.Action,
		checkpoint.ProducerNextSeq,
		checkpoint.ReplayFrom,
		checkpoint.ReplayThrough,
		response.ConsumerAckedThrough,
		response.From,
		response.Through,
		response.AdvanceProducerNextSeq,
	)
	switch response.Action {
	case reliablemq.ReconcileActionAligned, reliablemq.ReconcileActionReplay:
		return nil
	case reliablemq.ReconcileActionAdvanceProducer:
		return s.deps.TransportReconciler.AdvanceProducerNextSeq(
			ctx,
			s.spec.TransportQueueID,
			reliablemq.StreamACP,
			response.AdvanceProducerNextSeq,
		)
	case reliablemq.ReconcileActionRotate:
		return ErrTransportQueueRotate
	default:
		return fmt.Errorf("unsupported reconcile action %q", response.Action)
	}
}

func isReconcileResponseClose(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "unexpected EOF") ||
		strings.Contains(message, "websocket: close") ||
		message == "closed"
}

func (s *AgentTunnelSession) copyWSToStdin(ctx context.Context, conn WebSocketConn, engine ReliableEngine) error {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read tunnel: %w", err)
		}
		if messageType != websocketTextMessage && messageType != websocketBinaryMessage {
			continue
		}
		env, err := reliablemq.UnmarshalEnvelope(payload)
		if err != nil {
			return err
		}
		if env.QueueID != s.spec.TransportQueueID {
			return fmt.Errorf("unexpected reliablemq queue_id %q", env.QueueID)
		}
		if err := engine.Receive(ctx, env); err != nil {
			return err
		}
	}
}

func (s *AgentTunnelSession) copyStdoutToWS(ctx context.Context, stdout io.Reader, engine ReliableEngine) error {
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineDelimiter(line)
			if len(line) > 0 {
				if !json.Valid(line) {
					return fmt.Errorf("acp stdout payload must be JSON")
				}
				if err := s.sendOutbound(ctx, line, engine); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read acp stdout: %w", err)
		}
	}
}

func (s *AgentTunnelSession) sendOutbound(ctx context.Context, payload []byte, engine ReliableEngine) error {
	_, err := engine.Send(ctx, reliablemq.OutboundMessage{
		QueueID: s.spec.TransportQueueID,
		Stream:  reliablemq.StreamACP,
		Payload: append([]byte(nil), payload...),
		Metadata: reliablemq.Metadata{
			"agent_id": s.spec.CloudAgentID,
		},
	})
	return err
}

func (s *AgentTunnelSession) newReliableEngine(conn WebSocketConn, stdin io.Writer) ReliableEngine {
	return s.newReliableEngineWithInitialize(conn, stdin, nil)
}

func (s *AgentTunnelSession) newReliableEngineWithInitialize(conn WebSocketConn, stdin io.Writer, initializeResult json.RawMessage) ReliableEngine {
	var engine ReliableEngine
	engine = s.deps.ReliableEngineFactory.NewReliableEngine(s.reliableSender(conn), reliablemq.DispatcherFunc(func(ctx context.Context, frame reliablemq.Frame) error {
		if len(initializeResult) > 0 {
			if handled, err := s.handleManagerInitialize(ctx, frame.Payload, initializeResult, engine); handled || err != nil {
				return err
			}
		}
		return writeACPStdin(stdin, frame.Payload)
	}))
	return engine
}

func (s *AgentTunnelSession) handleManagerInitialize(
	ctx context.Context,
	payload []byte,
	initializeResult json.RawMessage,
	engine ReliableEngine,
) (bool, error) {
	response, handled, err := acpInitializeResponsePayload(payload, initializeResult)
	if err != nil || !handled || len(response) == 0 {
		return handled, err
	}
	return true, s.sendOutbound(ctx, response, engine)
}

func (s *AgentTunnelSession) reliableSender(conn WebSocketConn) reliablemq.Sender {
	return reliablemq.SenderFunc(func(ctx context.Context, env reliablemq.Envelope) error {
		_ = ctx
		data, err := reliablemq.MarshalEnvelope(env)
		if err != nil {
			return err
		}
		return conn.WriteMessage(websocketTextMessage, data)
	})
}

func (s *AgentTunnelSession) terminateProcess(proc LocalACPProcess) {
	if proc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = proc.Terminate(ctx)
}

func (s *AgentTunnelSession) emit(phase Phase, pid *int) {
	s.deps.SessionEventSink.OnSessionEvent(SessionEvent{
		Kind:         SessionAgentTunnel,
		Phase:        phase,
		RemoteID:     s.spec.RemoteID,
		ConnectionID: s.spec.ConnectionID,
		Generation:   s.spec.Generation,
		RestartNonce: s.spec.RestartNonce,
		PID:          pid,
		At:           time.Now(),
	})
}

func classifyProcessStartExit(err error) Exit {
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found") ||
		strings.Contains(err.Error(), "no such file or directory") {
		return ConfigExit("command_not_found", err.Error())
	}
	return TransientExit("process_start_failed", err.Error())
}

func writeACPStdin(stdin io.Writer, payload []byte) error {
	if _, err := stdin.Write(payload); err != nil {
		return fmt.Errorf("write acp stdin: %w", err)
	}
	if !bytes.HasSuffix(payload, []byte("\n")) {
		if _, err := stdin.Write([]byte("\n")); err != nil {
			return fmt.Errorf("write acp stdin delimiter: %w", err)
		}
	}
	return nil
}

func trimLineDelimiter(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
