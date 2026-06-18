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
	"encoding/json"
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
	"github.com/pax-beehive/paxd/internal/store"
)

// Config controls the stateless ACP forwarder service.
type Config struct {
	CloudURL          string
	APIKey            string
	CFClientID        string
	CFClientSecret    string
	AgentID           string
	InstanceID        string
	Command           []string
	WorkingDir        string
	TunnelPath        string
	ReconnectInterval time.Duration
	Journal           *store.Store
}

// Service maintains one ACP tunnel session at a time.
type Service struct {
	cfg    Config
	dialer *websocket.Dialer
}

const maxReconnectBackoff = 30 * time.Second

type tunnelEnvelope struct {
	Type    string          `json:"type"`
	Stream  string          `json:"stream"`
	Seq     int64           `json:"seq"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

const (
	tunnelTypeAck             = "ack"
	tunnelTypeData            = "data"
	tunnelStreamManagerToPaxd = "manager_to_paxd"
	tunnelStreamPaxdToManager = "paxd_to_manager"
)

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
	case len(s.cfg.Command) == 0:
		return fmt.Errorf("acp command is required")
	case s.cfg.Journal == nil:
		return fmt.Errorf("transport journal is required")
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
	errCh := make(chan error, 3)
	go func() { errCh <- s.copyWSToStdin(conn, stdin, &wsWriteMu) }()
	go func() { errCh <- s.copyStdoutToWS(stdout, conn, &wsWriteMu) }()
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
	defer stdin.Close()
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read tunnel: %w", err)
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		env, err := decodeTunnelEnvelope(payload)
		if err != nil {
			return err
		}
		if env.Type == tunnelTypeAck {
			if env.Stream == tunnelStreamPaxdToManager {
				if err := s.cfg.Journal.AckOutboundTransportFrames(
					s.cfg.AgentID,
					store.TransportStreamPaxdToManager,
					env.Seq,
				); err != nil {
					return fmt.Errorf("ack outbound frame: %w", err)
				}
			}
			continue
		}
		rawPayload, ok, err := unwrapManagerToPaxdEnvelope(env)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		inserted, err := s.cfg.Journal.SaveTransportFrameIfAbsent(&store.TransportFrame{
			AgentID:        s.cfg.AgentID,
			Stream:         store.TransportStreamManagerToPaxd,
			Seq:            env.Seq,
			LocalDirection: store.TransportDirectionInbound,
			PayloadJSON:    string(rawPayload),
			Status:         store.TransportStatusReceived,
		})
		if err != nil {
			return fmt.Errorf("save inbound frame: %w", err)
		}
		if err := projectTransportMessage(
			s.cfg.Journal,
			s.cfg.AgentID,
			store.TransportStreamManagerToPaxd,
			env.Seq,
			rawPayload,
		); err != nil {
			return fmt.Errorf("project inbound message history: %w", err)
		}
		if err := writeTunnelAck(conn, wsWriteMu, tunnelStreamManagerToPaxd, env.Seq); err != nil {
			return err
		}
		if !inserted {
			continue
		}
		if _, err := stdin.Write(rawPayload); err != nil {
			return fmt.Errorf("write acp stdin: %w", err)
		}
		if !bytes.HasSuffix(rawPayload, []byte("\n")) {
			if _, err := stdin.Write([]byte("\n")); err != nil {
				return fmt.Errorf("write acp stdin delimiter: %w", err)
			}
		}
		if err := s.cfg.Journal.UpdateTransportFrameStatus(
			s.cfg.AgentID,
			store.TransportStreamManagerToPaxd,
			env.Seq,
			store.TransportDirectionInbound,
			store.TransportStatusApplied,
			"",
		); err != nil {
			return fmt.Errorf("mark inbound frame applied: %w", err)
		}
	}
}

func (s *Service) copyStdoutToWS(stdout io.Reader, conn *websocket.Conn, wsWriteMu *sync.Mutex) error {
	reader := bufio.NewReader(stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineDelimiter(line)
			if len(line) > 0 {
				seq, err := s.cfg.Journal.NextTransportSeq(
					s.cfg.AgentID,
					store.TransportStreamPaxdToManager,
					store.TransportDirectionOutbound,
				)
				if err != nil {
					return fmt.Errorf("next outbound seq: %w", err)
				}
				if err := s.cfg.Journal.SaveTransportFrame(&store.TransportFrame{
					AgentID:        s.cfg.AgentID,
					Stream:         store.TransportStreamPaxdToManager,
					Seq:            seq,
					LocalDirection: store.TransportDirectionOutbound,
					PayloadJSON:    string(line),
					Status:         store.TransportStatusPending,
				}); err != nil {
					return fmt.Errorf("save outbound frame: %w", err)
				}
				if err := projectTransportMessage(
					s.cfg.Journal,
					s.cfg.AgentID,
					store.TransportStreamPaxdToManager,
					seq,
					line,
				); err != nil {
					return fmt.Errorf("project outbound message history: %w", err)
				}
				enveloped, err := wrapPaxdToManager(seq, line)
				if err != nil {
					return err
				}
				wsWriteMu.Lock()
				writeErr := conn.WriteMessage(websocket.TextMessage, enveloped)
				wsWriteMu.Unlock()
				if writeErr != nil {
					return fmt.Errorf("write tunnel: %w", writeErr)
				}
				if err := s.cfg.Journal.UpdateTransportFrameStatus(
					s.cfg.AgentID,
					store.TransportStreamPaxdToManager,
					seq,
					store.TransportDirectionOutbound,
					store.TransportStatusSent,
					"",
				); err != nil {
					return fmt.Errorf("mark outbound frame sent: %w", err)
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

func decodeTunnelEnvelope(payload []byte) (tunnelEnvelope, error) {
	var env tunnelEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return tunnelEnvelope{}, fmt.Errorf("decode tunnel envelope: %w", err)
	}
	return env, nil
}

func unwrapManagerToPaxd(payload []byte) ([]byte, bool, error) {
	env, err := decodeTunnelEnvelope(payload)
	if err != nil {
		return nil, false, err
	}
	return unwrapManagerToPaxdEnvelope(env)
}

func unwrapManagerToPaxdEnvelope(env tunnelEnvelope) ([]byte, bool, error) {
	if env.Type != tunnelTypeData {
		return nil, false, nil
	}
	if env.Stream != tunnelStreamManagerToPaxd {
		return nil, false, fmt.Errorf("unexpected tunnel stream %q", env.Stream)
	}
	if env.Seq <= 0 {
		return nil, false, fmt.Errorf("invalid tunnel seq %d", env.Seq)
	}
	if len(env.Payload) == 0 {
		return nil, false, fmt.Errorf("missing tunnel payload")
	}
	return env.Payload, true, nil
}

func writeTunnelAck(conn *websocket.Conn, wsWriteMu *sync.Mutex, stream string, seq int64) error {
	data, err := json.Marshal(tunnelEnvelope{
		Type:   tunnelTypeAck,
		Stream: stream,
		Seq:    seq,
	})
	if err != nil {
		return err
	}
	wsWriteMu.Lock()
	defer wsWriteMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("write tunnel ack: %w", err)
	}
	return nil
}

func wrapPaxdToManager(seq int64, payload []byte) ([]byte, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("wrap acp frame: payload must be JSON: %w", err)
	}
	return json.Marshal(tunnelEnvelope{
		Type:    tunnelTypeData,
		Stream:  tunnelStreamPaxdToManager,
		Seq:     seq,
		Payload: raw,
	})
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
