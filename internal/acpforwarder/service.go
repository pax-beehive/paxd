// Package acpforwarder tunnels ACP JSON-RPC between pax-manager and a local
// agent CLI.
//
// The forwarder treats ACP JSON-RPC as an opaque payload, but the
// pax-manager<->paxd WebSocket uses a small reliable transport envelope around
// that payload. The responsibilities are intentionally split as follows:
//   - WS reader: receives manager->paxd data frames, records them as inbound
//     received frames in the local transport journal, then ACKs manager.
//   - Inbound dispatcher: writes received inbound payloads to ACP stdin and
//     marks them applied. The current implementation performs this inline in
//     the WS reader; a future dispatcher can replay received-but-unapplied rows
//     after restart.
//   - ACP stdout reader / outbound sender: records stdout payloads as outbound
//     pending frames, writes them to the WebSocket, and marks them sent.
//   - ACK handler: consumes manager ACK frames and marks outbound frames acked.
//   - Cleanup worker: eventually removes acked/applied frames after retention.
//
// ACK semantics are deliberately narrow: an ACK means the receiver durably
// recorded the frame, not that the ACP business operation completed.
package acpforwarder

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pax-beehive/paxd/internal/acphistory"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/store"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
)

// Config controls the stateless ACP forwarder service.
type Config struct {
	CloudURL          string
	APIKey            string
	ConnectionID      string
	AgentID           string
	InstanceID        string
	Command           []string
	WorkingDir        string
	TunnelPath        string
	ReconnectInterval time.Duration
	Journal           *store.Store
	History           *daemonstore.Store
}

// Service maintains one ACP tunnel session at a time.
type Service struct {
	cfg    Config
	dialer *websocket.Dialer
}

const maxReconnectBackoff = 30 * time.Second

// New creates a forwarder service.
func New(cfg Config) *Service {
	if cfg.TunnelPath == "" {
		cfg.TunnelPath = "/api/v1/agent/tunnel"
	}
	if cfg.ReconnectInterval <= 0 {
		cfg.ReconnectInterval = 2 * time.Second
	}
	return &Service{
		cfg: cfg,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

// Run reconnects forever until ctx is canceled.
func (s *Service) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}

	backoff := s.cfg.ReconnectInterval

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		connected, err := s.runOnce(ctx)
		if connected {
			backoff = s.cfg.ReconnectInterval
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("[acp-forwarder] tunnel ended: %v; reconnecting in %s", err, backoff)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = nextReconnectBackoff(backoff, s.cfg.ReconnectInterval, connected)
	}
}

func nextReconnectBackoff(current time.Duration, initial time.Duration, connected bool) time.Duration {
	if connected {
		return initial
	}
	next := current * 2
	if next > maxReconnectBackoff {
		return maxReconnectBackoff
	}
	return next
}

func (s *Service) validate() error {
	switch {
	case s.cfg.CloudURL == "":
		return fmt.Errorf("cloud url is required")
	case s.cfg.APIKey == "":
		return fmt.Errorf("api key is required")
	case s.cfg.ConnectionID == "":
		return fmt.Errorf("connection id is required")
	case len(s.cfg.Command) == 0:
		return fmt.Errorf("acp command is required")
	case s.cfg.Journal == nil:
		return fmt.Errorf("transport journal is required")
	case s.cfg.History == nil:
		return fmt.Errorf("history store is required")
	}
	if !strings.ContainsAny(s.cfg.Command[0], `/\`) {
		if _, err := exec.LookPath(s.cfg.Command[0]); err != nil {
			return fmt.Errorf(
				"acp command executable %q not found in PATH; install it or set agents[].acp_forwarder.command",
				s.cfg.Command[0],
			)
		}
	}
	return nil
}

func (s *Service) runOnce(ctx context.Context) (bool, error) {
	tunnelURL, err := tunnelURLFromHTTP(s.cfg.CloudURL, s.cfg.TunnelPath)
	if err != nil {
		return false, err
	}

	q := tunnelURL.Query()
	q.Set("connection_id", s.cfg.ConnectionID)
	if s.cfg.AgentID != "" {
		q.Set("agent_id", s.cfg.AgentID)
	}
	if s.cfg.InstanceID != "" {
		q.Set("instance_id", s.cfg.InstanceID)
	}
	tunnelURL.RawQuery = q.Encode()

	headers := http.Header{}
	headers.Set("X-Pax-Key", s.cfg.APIKey)

	conn, resp, err := s.dialer.DialContext(ctx, tunnelURL.String(), headers)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("dial tunnel: %w", err)
	}
	defer conn.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Dir = s.cfg.WorkingDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return true, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return true, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return true, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return true, fmt.Errorf("start acp command: %w", err)
	}

	log.Printf("[acp-forwarder] connected %s -> %s", tunnelURL.Redacted(), s.cfg.Command[0])

	var wsWriteMu sync.Mutex
	engine, producer, closeEngine, err := s.newReliableEngine(stdin)
	if err != nil {
		return true, err
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClose()
		if err := closeEngine(closeCtx); err != nil {
			log.Printf("[acp-forwarder] producer write-behind close failed: %v", err)
		}
	}()
	response, err := s.reconcilePaxdProducer(runCtx, conn, producer, &wsWriteMu)
	if err != nil {
		return true, fmt.Errorf("reconcile producer: %w", err)
	}
	if err := engine.ReplayInbound(runCtx, s.cfg.ConnectionID, reliablemq.StreamACP, 1000); err != nil {
		return true, fmt.Errorf("replay inbound frames: %w", err)
	}
	binding, err := producer.Bind(runCtx, s.reliableSender(conn, &wsWriteMu), response.ConsumerAckedThrough)
	if err != nil {
		return true, fmt.Errorf("bind producer: %w", err)
	}
	defer binding.Close()
	if err := binding.WaitCaughtUp(runCtx); err != nil {
		return true, fmt.Errorf("producer recovery barrier: %w", err)
	}

	errCh := make(chan error, 3)
	go func() { errCh <- s.copyWSToStdinWithEngine(conn, engine) }()
	go func() { errCh <- s.copyStdoutToWSWithEngine(stdout, engine) }()
	go logStderr(stderr)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var result error
	select {
	case err := <-errCh:
		result = err
	case err := <-waitCh:
		result = err
	case <-ctx.Done():
		result = ctx.Err()
	}

	cancel()
	_ = conn.Close()
	_ = stdin.Close()

	select {
	case err := <-waitCh:
		if result == nil {
			result = err
		}
	case <-time.After(5 * time.Second):
		if result == nil {
			result = fmt.Errorf("acp command did not exit after cancellation")
		}
	}

	if result != nil && strings.Contains(result.Error(), "use of closed network connection") {
		return true, nil
	}
	return true, result
}

func (s *Service) copyWSToStdin(conn *websocket.Conn, stdin io.WriteCloser, wsWriteMu *sync.Mutex) error {
	engine, producer, closeEngine, err := s.newReliableEngine(stdin)
	if err != nil {
		return err
	}
	defer func() {
		_ = closeEngine(context.Background())
	}()
	binding, err := producer.Bind(context.Background(), s.reliableSender(conn, wsWriteMu), 0)
	if err != nil {
		return err
	}
	defer binding.Close()
	return s.copyWSToStdinWithEngine(conn, engine)
}

func (s *Service) copyWSToStdinWithEngine(conn *websocket.Conn, engine *reliablemq.Engine) error {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read tunnel: %w", err)
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		env, err := reliablemq.UnmarshalEnvelope(payload)
		if err != nil {
			return err
		}
		if env.QueueID != s.cfg.ConnectionID {
			return fmt.Errorf("unexpected reliablemq queue_id %q", env.QueueID)
		}
		if err := engine.Receive(context.Background(), env); err != nil {
			return fmt.Errorf("receive reliablemq envelope: %w", err)
		}
	}
}

func (s *Service) copyStdoutToWS(stdout io.Reader, conn *websocket.Conn, wsWriteMu *sync.Mutex) error {
	engine, producer, closeEngine, err := s.newReliableEngine(io.Discard)
	if err != nil {
		return err
	}
	defer func() {
		_ = closeEngine(context.Background())
	}()
	binding, err := producer.Bind(context.Background(), s.reliableSender(conn, wsWriteMu), 0)
	if err != nil {
		return err
	}
	defer binding.Close()
	return s.copyStdoutToWSWithEngine(stdout, engine)
}

func (s *Service) copyStdoutToWSWithEngine(stdout io.Reader, engine *reliablemq.Engine) error {
	reader := bufio.NewReader(stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineDelimiter(line)
			if len(line) > 0 {
				if err := engine.Send(context.Background(), reliablemq.OutboundMessage{
					QueueID: s.cfg.ConnectionID,
					Stream:  reliablemq.StreamACP,
					Payload: append([]byte(nil), line...),
					Metadata: reliablemq.Metadata{
						"agent_id": s.cfg.AgentID,
					},
				}); err != nil {
					return fmt.Errorf("send reliablemq frame: %w", err)
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

func trimLineDelimiter(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line
}

func (s *Service) newReliableEngine(
	stdin io.Writer,
) (*reliablemq.Engine, *reliablemq.Producer, func(context.Context) error, error) {
	mqStore, err := sqlstore.NewSQLite(s.cfg.Journal.DB(), sqlstore.WithTableName("transport_journal"))
	if err != nil {
		return nil, nil, nil, err
	}
	transportStore := reliablemq.NewProducerWriteBehindStore(
		mqStore,
		reliablemq.WithProducerWriteBehindRequireBatchStore(),
		reliablemq.WithProducerWriteBehindFlushFailureHandler(func(err error, stats reliablemq.ProducerWriteBehindStats) {
			log.Printf(
				"[acp-forwarder] producer write-behind flush failed: %v dirty_frames=%d dirty_patches=%d dirty_bytes=%d consecutive_failures=%d last_error=%q",
				err,
				stats.DirtyFrames,
				stats.DirtyPatches,
				stats.DirtyBytes,
				stats.ConsecutiveFlushFailures,
				stats.LastFlushError,
			)
		}),
	)
	producer, err := reliablemq.NewProducer(context.Background(), reliablemq.ProducerConfig{
		QueueID: s.cfg.ConnectionID,
		Stream:  reliablemq.StreamACP,
		OnError: func(err error) {
			log.Printf("[acp-forwarder] FATAL transport producer stopped accepting output: %v", err)
		},
	}, transportStore)
	if err != nil {
		_ = transportStore.Close(context.Background())
		return nil, nil, nil, err
	}
	dispatcher := reliablemq.DispatcherFunc(func(ctx context.Context, frame reliablemq.Frame) error {
		if err := writeACPStdin(stdin, frame.Payload); err != nil {
			return err
		}
		return nil
	})
	closeEngine := func(ctx context.Context) error {
		return errors.Join(producer.Close(ctx), transportStore.Close(ctx))
	}
	return reliablemq.NewEngine(
		reliablemq.Config{},
		transportStore,
		producer,
		dispatcher,
	), producer, closeEngine, nil
}

func (s *Service) reliableSender(conn *websocket.Conn, wsWriteMu *sync.Mutex) reliablemq.Sender {
	return reliablemq.SenderFunc(func(ctx context.Context, env reliablemq.Envelope) error {
		if env.Type == reliablemq.EnvelopeTypeData {
			if err := acphistory.ProjectOutbound(
				ctx,
				s.cfg.History,
				s.cfg.AgentID,
				env.Seq,
				env.Payload,
			); err != nil {
				log.Printf("[acp-forwarder] project outbound history failed: %v", err)
			}
		}
		data, err := reliablemq.MarshalEnvelope(env)
		if err != nil {
			return err
		}
		wsWriteMu.Lock()
		defer wsWriteMu.Unlock()
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return fmt.Errorf("write reliablemq envelope: %w", err)
		}
		return nil
	})
}

func (s *Service) reconcilePaxdProducer(
	ctx context.Context,
	conn *websocket.Conn,
	producer *reliablemq.Producer,
	wsWriteMu *sync.Mutex,
) (reliablemq.Envelope, error) {
	checkpoint, err := producer.Checkpoint(ctx)
	if err != nil {
		return reliablemq.Envelope{}, fmt.Errorf("load producer checkpoint: %w", err)
	}
	request, err := reliablemq.MarshalEnvelope(reliablemq.ReconcileRequestEnvelope(checkpoint))
	if err != nil {
		return reliablemq.Envelope{}, err
	}
	wsWriteMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, request)
	wsWriteMu.Unlock()
	if err != nil {
		return reliablemq.Envelope{}, fmt.Errorf("write reconcile request: %w", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		if isReconcileResponseClose(err) {
			return reliablemq.Envelope{}, fmt.Errorf("manager requested queue rotation")
		}
		return reliablemq.Envelope{}, fmt.Errorf("read reconcile response: %w", err)
	}
	if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
		return reliablemq.Envelope{}, fmt.Errorf("unexpected reconcile message type %d", messageType)
	}
	response, err := reliablemq.UnmarshalEnvelope(payload)
	if err != nil {
		return reliablemq.Envelope{}, fmt.Errorf("decode reconcile response: %w", err)
	}
	if response.Type != reliablemq.EnvelopeTypeReconcileResponse {
		return reliablemq.Envelope{}, fmt.Errorf("expected reconcile_response, got %q", response.Type)
	}
	if response.QueueID != s.cfg.ConnectionID {
		return reliablemq.Envelope{}, fmt.Errorf("unexpected reconcile queue_id %q", response.QueueID)
	}
	if response.Stream != reliablemq.StreamACP {
		return reliablemq.Envelope{}, fmt.Errorf("unexpected reconcile stream %q", response.Stream)
	}
	log.Printf(
		"[acp-forwarder] reconciled action=%s producer_next_seq=%d consumer_acked_through=%d replay_from=%d replay_through=%d advance_producer_next_seq=%d",
		response.Action,
		checkpoint.ProducerNextSeq,
		response.ConsumerAckedThrough,
		response.From,
		response.Through,
		response.AdvanceProducerNextSeq,
	)
	switch response.Action {
	case reliablemq.ReconcileActionAligned, reliablemq.ReconcileActionReplay:
		return response, nil
	case reliablemq.ReconcileActionAdvanceProducer:
		if err := producer.AdvanceProducerNextSeq(ctx, response.AdvanceProducerNextSeq); err != nil {
			return reliablemq.Envelope{}, err
		}
		return response, nil
	case reliablemq.ReconcileActionRotate:
		return reliablemq.Envelope{}, fmt.Errorf("manager requested queue rotation")
	default:
		return reliablemq.Envelope{}, fmt.Errorf("unsupported reconcile action %q", response.Action)
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

func logStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		log.Printf("[acp-forwarder stderr] %s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[acp-forwarder] stderr read error: %v", err)
	}
}

func tunnelURLFromHTTP(rawBase, tunnelPath string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(rawBase, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse cloud url: %w", err)
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("unsupported cloud url scheme %q", base.Scheme)
	}
	if tunnelPath == "" {
		tunnelPath = "/api/v1/agent/tunnel"
	}
	if !strings.HasPrefix(tunnelPath, "/") {
		tunnelPath = "/" + tunnelPath
	}
	base.Path = strings.TrimRight(base.Path, "/") + tunnelPath
	return base, nil
}
