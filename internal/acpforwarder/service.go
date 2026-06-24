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
	CFClientID        string
	CFClientSecret    string
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
	if s.cfg.CFClientID != "" {
		headers.Set("CF-Access-Client-Id", s.cfg.CFClientID)
	}
	if s.cfg.CFClientSecret != "" {
		headers.Set("CF-Access-Client-Secret", s.cfg.CFClientSecret)
	}

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
	engine, err := s.newReliableEngine(conn, stdin, &wsWriteMu)
	if err != nil {
		return true, err
	}
	if err := engine.ReplayInbound(runCtx, s.cfg.ConnectionID, reliablemq.StreamACP, 1000); err != nil {
		return true, fmt.Errorf("replay inbound frames: %w", err)
	}
	if err := engine.ReplayOutbound(runCtx, s.cfg.ConnectionID, reliablemq.StreamACP, 1000); err != nil {
		return true, fmt.Errorf("replay outbound frames: %w", err)
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
	engine, err := s.newReliableEngine(conn, stdin, wsWriteMu)
	if err != nil {
		return err
	}
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
	engine, err := s.newReliableEngine(conn, io.Discard, wsWriteMu)
	if err != nil {
		return err
	}
	return s.copyStdoutToWSWithEngine(stdout, engine)
}

func (s *Service) copyStdoutToWSWithEngine(stdout io.Reader, engine *reliablemq.Engine) error {
	reader := bufio.NewReader(stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineDelimiter(line)
			if len(line) > 0 {
				if _, err := engine.Send(context.Background(), reliablemq.OutboundMessage{
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

func (s *Service) newReliableEngine(conn *websocket.Conn, stdin io.Writer, wsWriteMu *sync.Mutex) (*reliablemq.Engine, error) {
	mqStore, err := sqlstore.NewSQLite(s.cfg.Journal.DB(), sqlstore.WithTableName("transport_journal"))
	if err != nil {
		return nil, err
	}
	sender := reliablemq.SenderFunc(func(ctx context.Context, env reliablemq.Envelope) error {
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
	dispatcher := reliablemq.DispatcherFunc(func(ctx context.Context, frame reliablemq.Frame) error {
		if err := writeACPStdin(stdin, frame.Payload); err != nil {
			return err
		}
		return nil
	})
	return reliablemq.NewEngine(
		reliablemq.Config{},
		mqStore,
		sender,
		dispatcher,
	), nil
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
